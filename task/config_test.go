package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

	var changes atomic.Int32
	tasks, err := StartFromConfig(configFile, logDir, stateDir, noopMessager{})
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.StopAll()

	tasks.SetOnChange(func() { changes.Add(1) })

	// Give the auto-start time to fire (but OnChange wasn't set yet, so no count)
	time.Sleep(500 * time.Millisecond)

	// Explicit stop should trigger OnChange
	tasks.Get("sleeper").Do("stop")
	time.Sleep(500 * time.Millisecond)

	if changes.Load() < 1 {
		t.Fatalf("expected OnChange to be called at least once after stop, got %d", changes.Load())
	}

	// Start again should trigger OnChange
	before := changes.Load()
	tasks.Get("sleeper").Do("start")
	time.Sleep(500 * time.Millisecond)

	if changes.Load() <= before {
		t.Fatalf("expected OnChange to be called after start, got %d (was %d before)", changes.Load(), before)
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

func TestRestartOnChangeNotCalledWithStaleCount(t *testing.T) {
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

	tasks, err := StartFromConfig(configFile, logDir, stateDir, noopMessager{})
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.StopAll()

	// Wait for auto-start
	time.Sleep(500 * time.Millisecond)

	// Track every running count reported via OnChange
	var mu sync.Mutex
	var counts []int
	tasks.SetOnChange(func() {
		running, _ := tasks.CountRunning()
		mu.Lock()
		counts = append(counts, running)
		mu.Unlock()
	})

	// Restart
	tasks.Get("sleeper").Do("restart")
	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	// During restart, OnChange should never report 0 running.
	// It should only fire once (from start()), showing 1 running.
	for i, c := range counts {
		if c == 0 {
			t.Errorf("OnChange reported 0 running at call %d — stale intermediate state leaked; all counts: %v", i, counts)
		}
	}
	if len(counts) == 0 {
		t.Error("expected OnChange to be called at least once during restart")
	}
}
