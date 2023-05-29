package task

import (
	"fmt"
	"os"
	"sync"
)

type Tasks struct {
	mu    sync.Mutex
	Tasks map[string]*Task

	filename string

	msgr Messager
}

type TasksConfig struct {
	Tasks []*Task `yaml:"task"`
}

func StartFromConfig(filename string, msgr Messager) (*Tasks, error) {
	tasks := &Tasks{
		Tasks:    make(map[string]*Task),
		filename: filename,
		msgr:     msgr,
	}
	if err := tasks.Reload(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (ts *Tasks) Reload() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	yaml, err := os.ReadFile(ts.filename)
	if err != nil {
		return err
	}
	cfg, err := ParseYAML(yaml)
	if err != nil {
		return err
	}

	seen := make(map[string]bool)
	for _, t := range cfg.Tasks {
		if seen[t.Name] {
			return fmt.Errorf("duplicate config: %s", t.Name)
		}
		seen[t.Name] = true

		if oldTask := ts.Tasks[t.Name]; oldTask != nil {
			oldTask.Update(t)
			continue
		}
		ts.Tasks[t.Name] = t
		t.Loop(prefixMessager{t.Name, ts.msgr})
	}
	for _, t := range ts.Tasks {
		if !seen[t.Name] {
			t.Do("stop")
			delete(ts.Tasks, t.Name)
		}
	}
	return nil
}

func (ts *Tasks) StopAll() {
	for _, t := range ts.Tasks {
		t.Do("stop")
	}
}

func (ts *Tasks) Get(name string) *Task {
	return ts.Tasks[name]
}
