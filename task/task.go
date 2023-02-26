package task

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
)

type Messager interface {
	Send(fmt string, args ...any)
}

type Task struct {
	Name       string   `yaml:"name"`
	Dir        string   `yaml:"dir"`
	Entrypoint []string `yaml:"entrypoint"`

	Messager Messager

	once        sync.Once
	cmd         *exec.Cmd
	op          chan string
	stopped     chan bool
	restartNext bool
	// TODO: auto restart

	muLog  sync.Mutex
	logbuf bytes.Buffer
}

func (t *Task) Loop() {
	t.once.Do(func() {
		t.op = make(chan string, 10)
		t.stopped = make(chan bool)
		go t.loop()
	})
}

func (t *Task) Do(op string) {
	t.op <- op
}

func (t *Task) loop() {
	t.start()
	for {
		select {
		case op := <-t.op:
			t.do(op)

		case <-t.stopped:
			t.Messager.Send("stopped")
			t.cmd = nil
			if t.restartNext {
				t.Messager.Send("restarting...")
				t.restartNext = false
				t.start()
			}
		}
	}
}

func (t *Task) do(op string) {
	switch op {
	case "start":
		t.start()
	case "stop":
		t.stop()
	case "restart":
		t.restart()
	case "log":
		t.log()
	default:
		t.Messager.Send("unknown command %s", op)
	}
}

func (t *Task) start() {
	if t.cmd != nil {
		t.Messager.Send("already running")
		return
	}

	if len(t.Entrypoint) == 0 {
		t.Messager.Send("no entrypoint")
		return
	}
	t.cmd = exec.Command(t.Entrypoint[0], t.Entrypoint[1:]...)
	if err := t.cmd.Start(); err != nil {
		t.Messager.Send("couldn't start: %v", err)
		t.cmd = nil
	}
	t.Messager.Send("started")

	go t.readToLog(t.cmd.StderrPipe())
	go t.readToLog(t.cmd.StdoutPipe())

	go func() {
		if err := t.cmd.Wait(); err != nil {
			t.Messager.Send("Wait(): %v", err)
		}
		t.Messager.Send("stopped")
		t.stopped <- true
	}()
}

func (t *Task) readToLog(in io.ReadCloser, err error) {
	if err != nil {
		// TODO: what?
		return
	}
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

func (t *Task) stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
}

func (t *Task) log() {
	t.muLog.Lock()
	defer t.muLog.Unlock()
	log := t.logbuf.String()
	t.Messager.Send("```%s```", log)
	t.logbuf = bytes.Buffer{}
}

func (t *Task) restart() {
	t.restartNext = true
	t.stop()
}
