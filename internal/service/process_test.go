package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestBackend(t *testing.T, entrypoint []string, cmd string) *ProcessBackend {
	t.Helper()
	logDir := t.TempDir()
	stateDir := t.TempDir()
	return NewProcessBackend("test", t.TempDir(), entrypoint, cmd, logDir, stateDir, false)
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
	stateDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, stateDir, false)

	msg := b.TryAdopt()
	if !strings.Contains(msg, "adoption disabled") {
		t.Fatalf("TryAdopt with adopt=false: got %q", msg)
	}
}

func TestProcessBackend_TryAdoptNoStateFile(t *testing.T) {
	logDir := t.TempDir()
	stateDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, stateDir, true)

	msg := b.TryAdopt()
	if !strings.Contains(msg, "no state file") {
		t.Fatalf("TryAdopt with no state: got %q", msg)
	}
}

func TestProcessBackend_StateFileCreatedOnStart(t *testing.T) {
	logDir := t.TempDir()
	stateDir := t.TempDir()
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", logDir, stateDir, false)
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	// State file should exist
	statePath := filepath.Join(stateDir, "test.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatal("state file should exist after start")
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
