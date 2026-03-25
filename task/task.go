package task

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/go-yaml/yaml"
)

func ParseYAML(data []byte) (TasksConfig, error) {
	var cfg TasksConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return TasksConfig{}, err
	}
	return cfg, nil
}

type Messager interface {
	Send(format string, args ...any)
}

type prefixMessager struct {
	prefix string
	next   Messager
}

func (p prefixMessager) Send(format string, args ...any) {
	p.next.Send("%s: %s", p.prefix, fmt.Sprintf(format, args...))
}

type syncOp struct {
	op     string
	result chan string
}

type Task struct {
	Name       string   `yaml:"name"`
	Dir        string   `yaml:"dir"`
	Entrypoint []string `yaml:"entrypoint"`

	once        sync.Once
	msg         Messager
	cmd         *exec.Cmd
	op          chan syncOp
	stopped     chan bool
	restartNext bool
	// TODO: auto restart

	logDir      string
	logPath     string // path to the current log file (set after start or adopt)
	stateDir    string
	adoptedPID  int
	adoptedPGID int
}

func (t *Task) isRunning() bool {
	return t.cmd != nil || t.adoptedPID != 0
}

// pgid returns the process group ID for the running task, whether spawned or adopted.
func (t *Task) pgid() int {
	if t.cmd != nil {
		return t.cmd.Process.Pid
	}
	return t.adoptedPGID
}

func (t *Task) Update(t2 *Task) {
	if t.Name == t2.Name && t.Dir == t2.Dir && reflect.DeepEqual(t.Entrypoint, t2.Entrypoint) {
		return
	}
	t.Name = t2.Name
	t.Dir = t2.Dir
	t.Entrypoint = t2.Entrypoint
	t.Do("restart")
}

func (t *Task) Loop(msg Messager) {
	t.once.Do(func() {
		t.msg = msg
		t.op = make(chan syncOp, 10)
		t.stopped = make(chan bool, 1)
		go t.loop()
	})
}

func (t *Task) Do(op string) string {
	so := syncOp{
		op:     op,
		result: make(chan string),
	}
	t.op <- so
	return <-so.result
}

func (t *Task) loop() {
	// Check if there's a running process to adopt from a previous session
	if s, err := LoadState(t.stateDir, t.Name); err == nil {
		result := t.adopt(s)
		t.msg.Send(result)
	} else {
		t.msg.Send(t.start())
	}

	for {
		select {
		case op := <-t.op:
			result := t.do(op.op)
			if op.result != nil {
				op.result <- result
			}

		case <-t.stopped:
			t.msg.Send("stopped")
			t.cmd = nil
			t.adoptedPID = 0
			t.adoptedPGID = 0
			if t.logPath != "" {
				if f, err := os.OpenFile(t.logPath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					fmt.Fprintf(f, "=== Stopped at %s ===\n", time.Now().Format(time.RFC3339))
					f.Close()
				}
			}
			if t.restartNext {
				t.msg.Send("restarting...")
				t.restartNext = false
				t.start()
			}
		}
	}
}

func (t *Task) do(op string) string {
	switch op {
	case "start":
		return t.start()
	case "stop":
		return t.stop()
	case "restart":
		return t.restart()
	case "logs":
		return t.logs()
	case "status":
		return t.status()
	case "pull":
		return t.pull()
	default:
		return fmt.Sprintf("unknown command %s", op)
	}
}

func (t *Task) start() string {
	if t.isRunning() {
		return "already running"
	}
	if len(t.Entrypoint) == 0 {
		return "no entrypoint"
	}

	// Create log file with temp name (we don't know PID yet)
	tmpPath := filepath.Join(t.logDir, t.Name+".starting.log")
	logFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Sprintf("couldn't open log: %v", err)
	}

	t.cmd = exec.Command(t.Entrypoint[0], t.Entrypoint[1:]...)
	t.cmd.Dir = t.Dir
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	t.cmd.Stdout = logFile
	t.cmd.Stderr = logFile

	if err := t.cmd.Start(); err != nil {
		logFile.Close()
		os.Remove(tmpPath)
		t.cmd = nil
		return fmt.Sprintf("couldn't start: %v", err)
	}

	// Rename log file to include PID (child keeps writing — same inode)
	pid := t.cmd.Process.Pid
	t.logPath = LogPath(t.logDir, t.Name, pid)
	os.Rename(tmpPath, t.logPath)

	// Write start marker, then close our handle — child owns the fd now
	fmt.Fprintf(logFile, "=== Started at %s (pid %d) ===\n", time.Now().Format(time.RFC3339), pid)
	logFile.Close()

	// Write running state with process identity
	SaveState(t.stateDir, t.Name, RunningState(pid, t.logPath))

	// Clean up old log files for this task (keep last 5)
	CleanupOldLogs(t.logDir, t.Name, 5)

	go func() {
		if err := t.cmd.Wait(); err != nil {
			t.msg.Send("Wait(): %v", err)
		}
		t.stopped <- true
	}()

	return fmt.Sprintf("started (pid %d)", pid)
}

func (t *Task) adopt(s State) string {
	if t.cmd != nil {
		return "already running"
	}

	// Desired state is stopped — respect it
	if s.Status == "stopped" {
		return "stopped (preserved from previous session)"
	}

	// PID is dead — process crashed while mezzaops was down, restart it
	if !IsAlive(s.PID) {
		return "stale pid (process dead), " + t.start()
	}

	// PID is alive but belongs to a different process (reboot or PID reuse)
	if !VerifyProcess(s) {
		return "pid reused by different process, " + t.start()
	}

	// PID is alive and verified — adopt it
	t.adoptedPID = s.PID
	t.adoptedPGID = s.PGID
	t.logPath = s.LogPath

	go t.pollAlive(s.PID)

	return fmt.Sprintf("adopted (pid %d)", s.PID)
}

func (t *Task) pollAlive(pid int) {
	for {
		time.Sleep(2 * time.Second)
		if !IsAlive(pid) {
			t.stopped <- true
			return
		}
	}
}

func (t *Task) stop() string {
	if !t.isRunning() {
		return "already stopped"
	}
	SaveState(t.stateDir, t.Name, State{Status: "stopped"})
	syscall.Kill(-t.pgid(), syscall.SIGKILL)
	return "stopping"
}

func (t *Task) logs() string {
	if t.logPath == "" {
		return "no logs"
	}
	log := TailLogFile(t.logPath, 1500)
	if log == "" {
		return "empty logs"
	}
	return fmt.Sprintf("```%s```", log)
}

func (t *Task) status() string {
	if !t.isRunning() {
		return "stopped"
	}
	if t.adoptedPID != 0 {
		return fmt.Sprintf("running (adopted, pid %d)", t.adoptedPID)
	}
	return "running"
}

func (t *Task) restart() string {
	if !t.isRunning() {
		return t.start()
	}
	t.restartNext = true
	t.stop()
	return "restarting..."
}

func (t *Task) pull() string {
	pullCmd := exec.Command("git", "pull")
	pullCmd.Dir = t.Dir

	stdout, err := pullCmd.StdoutPipe()
	if err != nil {
		return err.Error()
	}
	stderr, err := pullCmd.StderrPipe()
	if err != nil {
		return err.Error()
	}

	if err := pullCmd.Start(); err != nil {
		return err.Error()
	}

	stdoutData, _ := io.ReadAll(stdout)
	stderrData, _ := io.ReadAll(stderr)
	out := string(stdoutData) + string(stderrData)
	if out != "" {
		out = "```" + out + "```"
	}
	if err := pullCmd.Wait(); err != nil {
		out += err.Error()
	}
	return out
}
