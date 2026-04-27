package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	backend := json.RawMessage(`{"pid":1234,"pgid":1234,"log_path":"/tmp/logs/web.1234.log","boot_time":1711000000,"create_time":1711000123000}`)
	s := State{
		Status:  "running",
		Backend: backend,
	}

	if err := SaveState(dir, "mytask", s); err != nil {
		t.Fatal(err)
	}

	got, raw, err := LoadState(dir, "mytask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("Status: got %q, want running", got.Status)
	}
	if got.Backend == nil {
		t.Fatal("Backend should be non-nil")
	}
	// Verify raw bytes are returned
	if raw == nil {
		t.Fatal("raw bytes should be non-nil")
	}

	// Verify we can unmarshal backend data
	var bs struct {
		PID  int    `json:"pid"`
		PGID int    `json:"pgid"`
		Log  string `json:"log_path"`
	}
	if err := json.Unmarshal(got.Backend, &bs); err != nil {
		t.Fatal(err)
	}
	if bs.PID != 1234 || bs.PGID != 1234 {
		t.Fatalf("backend fields: got %+v", bs)
	}
	if bs.Log != "/tmp/logs/web.1234.log" {
		t.Fatalf("backend LogPath: got %q", bs.Log)
	}
}

func TestSaveAndLoadStoppedState(t *testing.T) {
	dir := t.TempDir()
	s := State{Status: "stopped"}

	if err := SaveState(dir, "mytask", s); err != nil {
		t.Fatal(err)
	}

	got, _, err := LoadState(dir, "mytask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("got %+v, want stopped", got)
	}
	if got.Backend != nil {
		t.Fatalf("Backend should be nil for stopped state, got %s", string(got.Backend))
	}
}

func TestLoadStateMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := LoadState(dir, "nonexistent")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func TestRemoveState(t *testing.T) {
	dir := t.TempDir()
	_ = SaveState(dir, "mytask", State{Status: "running"})
	RemoveState(dir, "mytask")

	_, _, err := LoadState(dir, "mytask")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist after remove, got %v", err)
	}
}

func TestLoadStateReturnsRawBytes(t *testing.T) {
	dir := t.TempDir()
	s := State{
		Status:  "running",
		Backend: json.RawMessage(`{"pid":42}`),
	}

	if err := SaveState(dir, "test", s); err != nil {
		t.Fatal(err)
	}

	_, raw, err := LoadState(dir, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Verify raw contains the full JSON
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "running" {
		t.Fatalf("raw should contain status, got %v", m)
	}
	backend, ok := m["backend"].(map[string]interface{})
	if !ok {
		t.Fatal("raw should contain backend object")
	}
	if backend["pid"] != float64(42) {
		t.Fatalf("raw backend pid: got %v", backend["pid"])
	}
}

func TestStateWithDeployFields(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	s := State{
		Status:     "failed",
		LastDeploy: now,
		LastResult: "failed",
		LastOutput: "build error on line 42",
		FailedStep: "build",
	}

	if err := SaveState(dir, "test", s); err != nil {
		t.Fatal(err)
	}

	got, _, err := LoadState(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("Status: got %q", got.Status)
	}
	if !got.LastDeploy.Equal(now) {
		t.Fatalf("LastDeploy: got %v, want %v", got.LastDeploy, now)
	}
	if got.LastResult != "failed" {
		t.Fatalf("LastResult: got %q", got.LastResult)
	}
	if got.LastOutput != "build error on line 42" {
		t.Fatalf("LastOutput: got %q", got.LastOutput)
	}
	if got.FailedStep != "build" {
		t.Fatalf("FailedStep: got %q", got.FailedStep)
	}
}

func TestState_ZeroTimesAreOmittedFromJSON(t *testing.T) {
	s := State{Status: "running"} // zero LastDeploy, zero LastRestart
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "last_deploy") {
		t.Errorf("zero LastDeploy should be omitted, got: %s", got)
	}
	if strings.Contains(got, "last_restart") {
		t.Errorf("zero LastRestart should be omitted, got: %s", got)
	}
}

func TestServiceState_ZeroTimesAreOmittedFromJSON(t *testing.T) {
	s := ServiceState{Status: "running"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "last_deploy") {
		t.Errorf("zero LastDeploy should be omitted, got: %s", got)
	}
	if strings.Contains(got, "last_restart") {
		t.Errorf("zero LastRestart should be omitted, got: %s", got)
	}
}
