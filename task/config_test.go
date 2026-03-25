package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type noopMessager struct{}

func (n noopMessager) Send(format string, args ...any) {}

type recordingMessager struct {
	mu       sync.Mutex
	messages []string
}

func (r *recordingMessager) Send(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *recordingMessager) Messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.messages))
	copy(cp, r.messages)
	return cp
}

func (r *recordingMessager) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = nil
}

func TestCountRunning(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "tasks.yaml")
	logDir := filepath.Join(dir, "logs")
	stateDir := filepath.Join(dir, "state")

	os.WriteFile(configFile, []byte(`task:
- name: a
  dir: /tmp
  entrypoint:
    - sleep
    - "60"
- name: b
  dir: /tmp
  entrypoint:
    - sleep
    - "60"
`), 0644)

	tasks, err := StartFromConfig(configFile, logDir, stateDir, noopMessager{})
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.StopAll()

	// Give tasks time to start
	time.Sleep(500 * time.Millisecond)

	running, total := tasks.CountRunning()
	if total != 2 {
		t.Fatalf("expected 2 total tasks, got %d", total)
	}
	if running != 2 {
		t.Fatalf("expected 2 running tasks, got %d", running)
	}

	// Stop one
	tasks.Get("a").Do("stop")
	time.Sleep(500 * time.Millisecond)

	running, total = tasks.CountRunning()
	if total != 2 {
		t.Fatalf("expected 2 total tasks, got %d", total)
	}
	if running != 1 {
		t.Fatalf("expected 1 running task after stop, got %d", running)
	}
}

func TestOnChangeCalledOnStartAndStop(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "tasks.yaml")
	logDir := filepath.Join(dir, "logs")
	stateDir := filepath.Join(dir, "state")

	os.WriteFile(configFile, []byte(`task:
- name: sleeper
  dir: /tmp
  entrypoint:
    - sleep
    - "60"
`), 0644)

	changes := 0
	tasks, err := StartFromConfig(configFile, logDir, stateDir, noopMessager{})
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.StopAll()

	tasks.OnChange = func() { changes++ }

	// Give the auto-start time to fire (but OnChange wasn't set yet, so no count)
	time.Sleep(500 * time.Millisecond)

	// Explicit stop should trigger OnChange
	tasks.Get("sleeper").Do("stop")
	time.Sleep(500 * time.Millisecond)

	if changes < 1 {
		t.Fatalf("expected OnChange to be called at least once after stop, got %d", changes)
	}

	// Start again should trigger OnChange
	before := changes
	tasks.Get("sleeper").Do("start")
	time.Sleep(500 * time.Millisecond)

	if changes <= before {
		t.Fatalf("expected OnChange to be called after start, got %d (was %d before)", changes, before)
	}
}

func TestRestartMessages(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "tasks.yaml")
	logDir := filepath.Join(dir, "logs")
	stateDir := filepath.Join(dir, "state")

	os.WriteFile(configFile, []byte(`task:
- name: sleeper
  dir: /tmp
  entrypoint:
    - sleep
    - "60"
`), 0644)

	msgr := &recordingMessager{}
	tasks, err := StartFromConfig(configFile, logDir, stateDir, msgr)
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.StopAll()

	// Wait for auto-start
	time.Sleep(500 * time.Millisecond)
	msgr.Clear()

	// Restart and wait for it to complete
	result := tasks.Get("sleeper").Do("restart")
	if result != "restarting..." {
		t.Fatalf("expected 'restarting...', got %q", result)
	}
	time.Sleep(1 * time.Second)

	msgs := msgr.Messages()

	// Should see: "stopped" then "started (pid ...)"
	// Should NOT see a second "restarting..."
	var hasStarted, hasStopped bool
	restartingCount := 0
	for _, m := range msgs {
		if strings.Contains(m, "stopped") {
			hasStopped = true
		}
		if strings.Contains(m, "started (pid") {
			hasStarted = true
		}
		if strings.Contains(m, "restarting") {
			restartingCount++
		}
	}

	if !hasStopped {
		t.Errorf("expected 'stopped' message, got: %v", msgs)
	}
	if !hasStarted {
		t.Errorf("expected 'started (pid ...)' message, got: %v", msgs)
	}
	if restartingCount > 0 {
		t.Errorf("expected no 'restarting...' in async messages (it's the sync return value), got %d in: %v", restartingCount, msgs)
	}
}
