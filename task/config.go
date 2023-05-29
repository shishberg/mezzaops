package task

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type Tasks struct {
	mu    sync.Mutex
	Tasks map[string]*Task

	filename string
	watcher  *fsnotify.Watcher

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
	var err error
	tasks.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	go tasks.watch()
	if err := tasks.watcher.Add(filename); err != nil {
		tasks.Close()
		return nil, err
	}

	if err := tasks.Reload(); err != nil {
		tasks.Close()
		return nil, err
	}
	return tasks, nil
}

func (t *Tasks) watch() {
	for {
		select {
		case event, ok := <-t.watcher.Events:
			if !ok {
				return
			}
			log.Println("config event:", event)
			if event.Has(fsnotify.Write) {
				t.msgr.Send("config changed, reloading")
				if err := t.Reload(); err != nil {
					t.msgr.Send("config load error: %s", err)
				}
			}

		case err, ok := <-t.watcher.Errors:
			if !ok {
				return
			}
			t.msgr.Send("config file error: %s", err)
		}
	}
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

func (t *Tasks) Close() {
	t.watcher.Close()
}
