package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogPath(t *testing.T) {
	got := LogPath("/tmp/logs", "web", 1234)
	want := "/tmp/logs/web.1234.log"
	if got != want {
		t.Fatalf("LogPath: got %q, want %q", got, want)
	}
}

func TestTailLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte("hello world\nsecond line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := TailLogFile(path, 1000)
	if !strings.Contains(got, "hello world") || !strings.Contains(got, "second line") {
		t.Fatalf("unexpected tail: %q", got)
	}
}

func TestTailLogFileTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 200)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := TailLogFile(path, 100)
	if len(got) > 100 {
		t.Fatalf("tail too long: %d bytes", len(got))
	}
}

func TestTailLogFileSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte("small"), 0644); err != nil {
		t.Fatal(err)
	}

	got := TailLogFile(path, 1000)
	if got != "small" {
		t.Fatalf("expected %q, got %q", "small", got)
	}
}

func TestTailLogFileMissing(t *testing.T) {
	got := TailLogFile("/nonexistent/file.log", 1000)
	if got != "" {
		t.Fatalf("expected empty for missing file, got %q", got)
	}
}

func TestCleanupOldLogs(t *testing.T) {
	dir := t.TempDir()
	// Create 5 log files for "web"
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, fmt.Sprintf("web.%d.log", 1000+i))
		if err := os.WriteFile(path, []byte("log"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Keep 2
	CleanupOldLogs(dir, "web", 2)

	matches, _ := filepath.Glob(filepath.Join(dir, "web.*.log"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 log files after cleanup, got %d", len(matches))
	}
}
