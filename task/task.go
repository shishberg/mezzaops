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
	op          chan syncOp
	stopped     chan bool
	restartNext bool

	pid      int // nonzero when running (spawned or adopted)
	pgid     int
	logDir   string
	logPath  string
	stateDir string
	onChange func() // called after state transitions; may be nil
}

func (t *Task) isRunning() bool {
	return t.pid != 0
}

func (t *Task) notifyChange() {
	if t.onChange != nil {
		t.onChange()
	}
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
			t.pid = 0
			t.pgid = 0
			if t.logPath != "" {
				if f, err := os.OpenFile(t.logPath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					fmt.Fprintf(f, "=== Stopped at %s ===\n", time.Now().Format(time.RFC3339))
					f.Close()
				}
			}
			t.msg.Send("stopped")
			if t.restartNext {
				t.restartNext = false
				t.msg.Send(t.start()) // start() calls notifyChange() with correct count
			} else {
				t.notifyChange()
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

	cmd := exec.Command(t.Entrypoint[0], t.Entrypoint[1:]...)
	cmd.Dir = t.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.Remove(tmpPath)
		return fmt.Sprintf("couldn't start: %v", err)
	}

	// From here on, the task is running. Store pid/pgid — same fields
	// used by adopt(), so stop/restart/status work identically.
	t.pid = cmd.Process.Pid
	t.pgid = cmd.Process.Pid

	// Rename log file to include PID (child keeps writing — same inode)
	t.logPath = LogPath(t.logDir, t.Name, t.pid)
	os.Rename(tmpPath, t.logPath)

	// Write start marker, then close our handle — child owns the fd now
	fmt.Fprintf(logFile, "=== Started at %s (pid %d) ===\n", time.Now().Format(time.RFC3339), t.pid)
	logFile.Close()

	// Write running state with process identity
	SaveState(t.stateDir, t.Name, RunningState(t.pid, t.logPath))

	// Clean up old log files for this task (keep last 5)
	CleanupOldLogs(t.logDir, t.Name, 5)

	// cmd.Wait() reaps the child and notifies the loop
	go func() {
		if err := cmd.Wait(); err != nil {
			t.msg.Send("Wait(): %v", err)
		}
		t.stopped <- true
	}()

	t.notifyChange()
	return fmt.Sprintf("started (pid %d)", t.pid)
}

func (t *Task) adopt(s State) string {
	if t.isRunning() {
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

	// PID is alive and verified — adopt it.
	// Same fields as start(), so stop/restart/status work identically.
	t.pid = s.PID
	t.pgid = s.PGID
	t.logPath = s.LogPath

	// pollAlive monitors the process since we can't cmd.Wait() on
	// a process we didn't spawn.
	go t.pollAlive(s.PID)

	t.notifyChange()
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
	syscall.Kill(-t.pgid, syscall.SIGKILL)
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
	return fmt.Sprintf("running (pid %d)", t.pid)
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
