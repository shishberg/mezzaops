// task/state_test.go
package task

import (
	"os"
	"testing"
)

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	s := State{
		Status:     "running",
		PID:        1234,
		PGID:       1234,
		LogPath:    "/tmp/logs/web.1234.log",
		BootTime:   1711000000,
		CreateTime: 1711000123000,
	}

	if err := SaveState(dir, "mytask", s); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(dir, "mytask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.PID != 1234 || got.PGID != 1234 {
		t.Fatalf("basic fields: got %+v", got)
	}
	if got.LogPath != "/tmp/logs/web.1234.log" {
		t.Fatalf("LogPath: got %q", got.LogPath)
	}
	if got.BootTime != 1711000000 || got.CreateTime != 1711000123000 {
		t.Fatalf("identity fields: got boot=%d create=%d", got.BootTime, got.CreateTime)
	}
}

func TestSaveAndLoadStoppedState(t *testing.T) {
	dir := t.TempDir()
	s := State{Status: "stopped"}

	if err := SaveState(dir, "mytask", s); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(dir, "mytask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" || got.PID != 0 {
		t.Fatalf("got %+v, want stopped with no PID", got)
	}
}

func TestLoadStateMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadState(dir, "nonexistent")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func TestRemoveState(t *testing.T) {
	dir := t.TempDir()
	_ = SaveState(dir, "mytask", State{PID: 1})
	RemoveState(dir, "mytask")

	_, err := LoadState(dir, "mytask")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist after remove, got %v", err)
	}
}
