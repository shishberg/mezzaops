package task

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
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

	muLog  sync.Mutex
	logbuf bytes.Buffer
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
		t.stopped = make(chan bool)
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
	t.msg.Send(t.start())
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

			t.muLog.Lock()
			t.logbuf.WriteString(fmt.Sprintf("=== Stopped at %s ===\n", time.Now().Format(time.RFC3339)))
			t.muLog.Unlock()

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
	if t.cmd != nil {
		return "already running"
	}

	if len(t.Entrypoint) == 0 {
		return "no entrypoint"
	}
	t.cmd = exec.Command(t.Entrypoint[0], t.Entrypoint[1:]...)
	t.cmd.Dir = t.Dir
	// Set process group so that any child processes get terminated at the same time
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return err.Error()
	}
	stderr, err := t.cmd.StderrPipe()
	if err != nil {
		return err.Error()
	}

	if err := t.cmd.Start(); err != nil {
		t.cmd = nil
		return fmt.Sprintf("couldn't start: %v", err)
	}

	t.muLog.Lock()
	t.logbuf.WriteString(fmt.Sprintf("=== Started at %s ===\n", time.Now().Format(time.RFC3339)))
	t.muLog.Unlock()

	go t.readToLog(stdout)
	go t.readToLog(stderr)

	go func() {
		if err := t.cmd.Wait(); err != nil {
			t.msg.Send("Wait(): %v", err)
		}
		t.stopped <- true
	}()

	return "started"
}

func (t *Task) readToLog(in io.ReadCloser) {
	var buf [1024]byte
	for {
		n, err := in.Read(buf[:])
		if n > 0 {
			t.muLog.Lock()
			_, _ = t.logbuf.Write(buf[:n])
			t.muLog.Unlock()
		}
		if err != nil {
			// TODO: check it's EOF
			break
		}
	}
}

func (t *Task) stop() string {
	if t.cmd == nil {
		return "already stopped"
	}
	if t.cmd.Process != nil {
		// Kill the process group to include children
		syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
	return "stopping"
}

func (t *Task) logs() string {
	t.muLog.Lock()
	defer t.muLog.Unlock()
	log := t.logbuf.String()
	t.logbuf = bytes.Buffer{}
	if log == "" {
		return "empty logs"
	}
	// Discord max message length is 2000.
	const maxLen = 1500
	if len(log) > maxLen {
		log = "...\n" + log[len(log)-1500:]
	}
	return fmt.Sprintf("```%s```", log)
}

func (t *Task) status() string {
	if t.cmd == nil {
		return "stopped"
	}
	return "running"
}

func (t *Task) restart() string {
	if t.cmd == nil {
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
