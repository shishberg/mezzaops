package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type Tasks struct {
	mu    sync.Mutex
	Tasks map[string]*Task

	filename string
	logDir   string
	stateDir string
	msgr     Messager

	onChange func() // guarded by mu
}

type TasksConfig struct {
	Tasks []*Task `yaml:"task"`
}

func StartFromConfig(filename, logDir, stateDir string, msgr Messager) (*Tasks, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	tasks := &Tasks{
		Tasks:    make(map[string]*Task),
		filename: filename,
		logDir:   logDir,
		stateDir: stateDir,
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
		t.logDir = ts.logDir
		t.stateDir = ts.stateDir
		t.onChange = func() {
			ts.mu.Lock()
			fn := ts.onChange
			ts.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
		t.Loop(prefixMessager{t.Name, ts.msgr})
	}
	for _, t := range ts.Tasks {
		if !seen[t.Name] {
			t.Do("stop")
			delete(ts.Tasks, t.Name)
		}
	}

	// Clean up orphaned processes: state files for tasks no longer in config.
	// This handles the case where a task is removed from config while mezzaops is down.
	ts.cleanOrphans(seen)

	return nil
}

func (ts *Tasks) cleanOrphans(configuredTasks map[string]bool) {
	entries, err := filepath.Glob(filepath.Join(ts.stateDir, "*.json"))
	if err != nil {
		return
	}
	for _, path := range entries {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if configuredTasks[name] {
			continue
		}
		s, err := LoadState(ts.stateDir, name)
		if err != nil {
			os.Remove(path)
			continue
		}
		if s.PID != 0 && IsAlive(s.PID) && VerifyProcess(s) {
			ts.msgr.Send("killing orphaned process %s (pid %d)", name, s.PID)
			syscall.Kill(-s.PGID, syscall.SIGKILL)
		}
		os.Remove(path)
	}
}

func (ts *Tasks) StartAll() {
	for _, t := range ts.Tasks {
		t.Do("start")
	}
}

func (ts *Tasks) StopAll() {
	for _, t := range ts.Tasks {
		t.Do("stop")
	}
}

func (ts *Tasks) Get(name string) *Task {
	return ts.Tasks[name]
}

// SetOnChange registers a callback invoked after any task state change.
// Safe to call while tasks are running.
func (ts *Tasks) SetOnChange(fn func()) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.onChange = fn
}

// CountRunning returns (running, total) task counts.
// Safe to call from any goroutine (e.g. OnChange callbacks from task loops).
func (ts *Tasks) CountRunning() (int, int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	running := 0
	for _, t := range ts.Tasks {
		if t.isRunning() {
			running++
		}
	}
	return running, len(ts.Tasks)
}
