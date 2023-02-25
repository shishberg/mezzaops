package task

import (
	"fmt"
	"os/exec"
	"sync"
)

type State int

// const (
// 	StateStopped State = iota
// 	StateRunning       = iota
// 	StateError         = iota
// )

type Task struct {
	Dir        string   `yaml:"dir"`
	Entrypoint []string `yaml:"entrypoint"`

	once        sync.Once
	cmd         *exec.Cmd
	op          chan string
	stopped     chan bool
	restartNext bool
	logbuf      string
}

func (t *Task) Loop() {
	t.once.Do(func() {
		t.op = make(chan string, 10)
		t.stopped = make(chan bool)
		t.loop()
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
			// TODO: print status
			t.cmd = nil
			if t.restartNext {
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
		// TODO: return error
	}
}

func (t *Task) start() {
	if t.cmd != nil {
		// TODO: say "already running"
		return
	}

	// TODO: error if len(t.Entrypoint) == 0
	t.cmd = exec.Command(t.Entrypoint[0], t.Entrypoint[1:]...)
	if err := t.cmd.Start(); err != nil {
		// TODO: return this immediately
		t.logbuf += fmt.Sprintf("%v\n", err)
		t.cmd = nil
	}
	go func() {
		if err := t.cmd.Wait(); err != nil {
			// TODO: log error
		}
		t.stopped <- true
	}()
}

func (t *Task) stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
}

func (t *Task) log() {
}

func (t *Task) restart() {

}
