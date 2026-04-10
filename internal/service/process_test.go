package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newTestBackend(t *testing.T, entrypoint []string, cmd string) *ProcessBackend {
	t.Helper()
	logDir := t.TempDir()
	return NewProcessBackend("test", t.TempDir(), entrypoint, cmd, logDir, false)
}

func TestProcessBackend_StartAndStatus(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != "stopped" {
		t.Fatalf("initial status: got %q, want stopped", status)
	}

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}

	status, err = b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("after start: got %q, want running", status)
	}

	t.Cleanup(func() { _ = b.Stop(context.Background()) })
}

func TestProcessBackend_Stop(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// Wait a moment for the done channel to close
	time.Sleep(100 * time.Millisecond)

	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != "stopped" {
		t.Fatalf("after stop: got %q, want stopped", status)
	}
}

func TestProcessBackend_Restart(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := b.Restart(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("after restart: got %q, want running", status)
	}

	t.Cleanup(func() { _ = b.Stop(context.Background()) })
}

func TestProcessBackend_Logs(t *testing.T) {
	b := newTestBackend(t, []string{"sh", "-c", "echo hello && sleep 3600"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	// Give the process a moment to write output
	time.Sleep(200 * time.Millisecond)

	logs, err := b.Logs(ctx, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "hello") {
		t.Fatalf("logs should contain 'hello', got %q", logs)
	}
}

func TestProcessBackend_StartWritesMarker(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	// Read the log file directly
	time.Sleep(100 * time.Millisecond)
	logs, err := b.Logs(ctx, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "=== Started at") {
		t.Fatalf("log should contain start marker, got %q", logs)
	}
}

func TestProcessBackend_DoubleStartIsNoop(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	// Second start should be a no-op (no error)
	if err := b.Start(ctx); err != nil {
		t.Fatalf("double start should not error, got %v", err)
	}
}

func TestProcessBackend_StopWhenNotRunning(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	// Stop when not running should be a no-op
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("stop when not running should not error, got %v", err)
	}
}

func TestProcessBackend_ShCmdFallback(t *testing.T) {
	b := newTestBackend(t, nil, "sleep 3600")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("after start with cmd: got %q, want running", status)
	}

	t.Cleanup(func() { _ = b.Stop(context.Background()) })
}

func TestProcessBackend_TryAdoptDisabled(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, false)

	msg := b.TryAdopt()
	if !strings.Contains(msg, "adoption disabled") {
		t.Fatalf("TryAdopt with adopt=false: got %q", msg)
	}
}

func TestProcessBackend_TryAdoptNoRestoredState(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, true)

	// No RestoreBackendState called, so restoredState is nil
	msg := b.TryAdopt()
	if !strings.Contains(msg, "no state to adopt") {
		t.Fatalf("TryAdopt with no restored state: got %q", msg)
	}
}

func TestProcessBackend_WaitForExit(t *testing.T) {
	b := newTestBackend(t, []string{"sh", "-c", "exit 0"}, "")
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}

	ch := b.WaitForExit()
	select {
	case <-ch:
		// Process exited
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process exit")
	}
}

func TestProcessBackend_SaveBackendStateRoundTrip(t *testing.T) {
	b := newTestBackend(t, []string{"sleep", "3600"}, "")
	ctx := context.Background()

	// Before start, SaveBackendState should return nil
	raw := b.SaveBackendState()
	if raw != nil {
		t.Fatalf("SaveBackendState before start should return nil, got %s", string(raw))
	}

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	// After start, SaveBackendState should return valid JSON with PID
	raw = b.SaveBackendState()
	if raw == nil {
		t.Fatal("SaveBackendState after start should return non-nil")
	}

	var bs processBackendState
	if err := json.Unmarshal(raw, &bs); err != nil {
		t.Fatalf("SaveBackendState returned invalid JSON: %v", err)
	}
	if bs.PID == 0 {
		t.Fatal("PID should be nonzero")
	}
	if bs.PGID == 0 {
		t.Fatal("PGID should be nonzero")
	}
}

func TestProcessBackend_RestoreBackendStateNewFormat(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, true)

	// Simulate a new-format state file with backend sub-object
	backendJSON := json.RawMessage(`{"pid":12345,"pgid":12345,"log_path":"/tmp/test.log","boot_time":1711000000,"create_time":1711000123000}`)
	fullState := State{
		Status:  "running",
		Backend: backendJSON,
	}
	fullJSON, _ := json.Marshal(fullState)

	b.RestoreBackendState(json.RawMessage(fullJSON))

	// Verify restoredState was set
	if b.restoredState == nil {
		t.Fatal("restoredState should be non-nil after RestoreBackendState")
	}
	if b.restoredState.PID != 12345 {
		t.Fatalf("restoredState.PID: got %d, want 12345", b.restoredState.PID)
	}
	if b.restoredState.PGID != 12345 {
		t.Fatalf("restoredState.PGID: got %d, want 12345", b.restoredState.PGID)
	}
	if b.restoredState.LogPath != "/tmp/test.log" {
		t.Fatalf("restoredState.LogPath: got %q", b.restoredState.LogPath)
	}
}

func TestProcessBackend_RestoreBackendStateOldFormat(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, true)

	// Simulate an old-format state file with pid/pgid at top level (no backend sub-object)
	oldFormatJSON := json.RawMessage(`{"status":"running","pid":54321,"pgid":54321,"log_path":"/tmp/old.log","boot_time":1711000000,"create_time":1711000123000}`)

	b.RestoreBackendState(oldFormatJSON)

	// Verify restoredState was set via migration path
	if b.restoredState == nil {
		t.Fatal("restoredState should be non-nil after RestoreBackendState (old format)")
	}
	if b.restoredState.PID != 54321 {
		t.Fatalf("restoredState.PID: got %d, want 54321", b.restoredState.PID)
	}
	if b.restoredState.LogPath != "/tmp/old.log" {
		t.Fatalf("restoredState.LogPath: got %q", b.restoredState.LogPath)
	}
}

func TestProcessBackend_TryAdoptStopped(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, true)

	// Restore a "stopped" state -- TryAdopt should respect that
	b.restoredState = &processBackendState{}
	b.restoredStatus = "stopped"

	msg := b.TryAdopt()
	if !strings.Contains(msg, "stopped") {
		t.Fatalf("TryAdopt with stopped state: got %q", msg)
	}
}

func TestProcessBackend_TryAdoptStalePID(t *testing.T) {
	logDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, true)

	// Restore state with a PID that doesn't exist
	b.restoredState = &processBackendState{
		PID:  99999999,
		PGID: 99999999,
	}
	b.restoredStatus = "running"

	msg := b.TryAdopt()
	if !strings.Contains(msg, "stale pid") {
		t.Fatalf("TryAdopt with dead PID: got %q", msg)
	}
}
