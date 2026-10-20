package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
	"github.com/shishberg/mezzaops/internal/config"
	"github.com/stretchr/testify/assert"
)

// testConfig builds a Config suitable for testing with temporary directories.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cleanupChildren(t) // stop service processes the test starts
	return &config.Config{
		LogDir:   t.TempDir(),
		StateDir: t.TempDir(),
	}
}

// sleepService returns a ServiceConfig that runs sleep in the given dir.
func sleepService(name, dir string) config.ServiceConfig {
	return config.ServiceConfig{
		Name:       name,
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
	}
}

func TestNewManager_CreatesServices(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	names := m.ServiceNames()
	if len(names) != 1 || names[0] != "testsvc" {
		t.Fatalf("expected [testsvc], got %v", names)
	}
}

func TestNewManager_NoServices(t *testing.T) {
	cfg := testConfig(t)

	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	names := m.ServiceNames()
	if len(names) != 0 {
		t.Fatalf("expected no services, got %v", names)
	}
}

func TestManager_DoStartAndStatus(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	result := m.Do("testsvc", "start")
	if !strings.Contains(result, "started") && !strings.Contains(result, "running") {
		t.Fatalf("start result: %q", result)
	}

	result = m.Do("testsvc", "status")
	if !strings.Contains(result, "running") {
		t.Fatalf("status after start: %q", result)
	}
}

func TestManager_DoStop(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("testsvc", "start")
	result := m.Do("testsvc", "stop")
	if !strings.Contains(result, "stop") {
		t.Fatalf("stop result: %q", result)
	}

	// Give process time to die
	time.Sleep(200 * time.Millisecond)

	result = m.Do("testsvc", "status")
	if !strings.Contains(result, "stopped") {
		t.Fatalf("status after stop: %q", result)
	}
}

func TestManager_DoRestart(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("testsvc", "start")
	result := m.Do("testsvc", "restart")
	if !strings.Contains(result, "restart") {
		t.Fatalf("restart result: %q", result)
	}

	result = m.Do("testsvc", "status")
	if !strings.Contains(result, "running") {
		t.Fatalf("status after restart: %q", result)
	}
}

func TestManager_DoLogs(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sh", "-c", "echo hello && sleep 3600"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("testsvc", "start")
	time.Sleep(200 * time.Millisecond)

	result := m.Do("testsvc", "logs")
	if !strings.Contains(result, "hello") {
		t.Fatalf("logs should contain 'hello', got %q", result)
	}
}

func TestManager_DoPull(t *testing.T) {
	// Create a temp git repo
	dir := t.TempDir()
	cmds := []string{
		"git init",
		"git config user.email test@test.com",
		"git config user.name Test",
		"git commit --allow-empty -m init",
	}
	for _, c := range cmds {
		parts := strings.Fields(c)
		cmd := exec.Command(parts[0], parts[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %q: %s: %v", c, out, err)
		}
	}

	cfg := testConfig(t)
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	result := m.Do("testsvc", "pull")
	// git pull in a repo with no remote will produce an error message
	// but it should not hang
	if result == "" {
		t.Fatal("pull should return non-empty output")
	}
}

func TestManager_DoUnknownService(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	result := m.Do("nonexistent", "status")
	if !strings.Contains(result, "not found") {
		t.Fatalf("expected 'not found', got %q", result)
	}
}

func TestManager_CountRunning(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	running, total := m.CountRunning()
	if total != 1 {
		t.Fatalf("total: got %d, want 1", total)
	}
	if running != 0 {
		t.Fatalf("running before start: got %d, want 0", running)
	}

	m.Do("testsvc", "start")
	running, _ = m.CountRunning()
	if running != 1 {
		t.Fatalf("running after start: got %d, want 1", running)
	}
}

func TestManager_GetAllStates(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	states := m.GetAllStates()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if _, ok := states["testsvc"]; !ok {
		t.Fatal("missing state for testsvc")
	}
}

func TestManager_FindServiceByRepo(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Repo:       "github.com/org/repo",
		Branch:     "main",
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Match with full prefix
	name, found := m.FindServiceByRepo("github.com/org/repo", "main")
	if !found || name != "testsvc" {
		t.Fatalf("FindServiceByRepo full prefix: got %q, %v", name, found)
	}

	// Match without github prefix (webhook style)
	name, found = m.FindServiceByRepo("org/repo", "main")
	if !found || name != "testsvc" {
		t.Fatalf("FindServiceByRepo short: got %q, %v", name, found)
	}

	// No match
	_, found = m.FindServiceByRepo("org/other", "main")
	if found {
		t.Fatal("expected no match for wrong repo")
	}

	// Wrong branch
	_, found = m.FindServiceByRepo("org/repo", "dev")
	if found {
		t.Fatal("expected no match for wrong branch")
	}
}

func TestManager_GetServiceConfig(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Branch:     "main",
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	sc, ok := m.GetServiceConfig("testsvc")
	if !ok {
		t.Fatal("expected to find config")
	}
	if sc.Branch != "main" {
		t.Fatalf("branch: got %q, want main", sc.Branch)
	}

	_, ok = m.GetServiceConfig("nonexistent")
	if ok {
		t.Fatal("expected not found for nonexistent")
	}
}

func TestManager_RequestDeploy_Success(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"echo deploying"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	// Start the service first so restart works
	m.Do("testsvc", "start")

	if err := m.RequestDeploy("testsvc"); err != nil {
		t.Fatal(err)
	}

	// Wait for deploy to complete
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["testsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(rec.getDeployStarted()) == 0 {
		t.Fatal("expected DeployStarted notification")
	}
	if len(rec.getDeploySucceeded()) == 0 {
		t.Fatal("expected DeploySucceeded notification")
	}
}

func TestManager_RequestDeploy_Failure(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"false"}, // will fail
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	if err := m.RequestDeploy("testsvc"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["testsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(rec.getDeployStarted()) == 0 {
		t.Fatal("expected DeployStarted notification")
	}
	if len(rec.getDeployFailed()) == 0 {
		t.Fatal("expected DeployFailed notification")
	}
}

func TestManager_RequestDeploy_NotFound(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	if err := m.RequestDeploy("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent service")
	}
}

func TestManager_ServiceNames_Sorted(t *testing.T) {
	cfg := testConfig(t)
	svcs := []config.ServiceConfig{
		sleepService("charlie", t.TempDir()),
		sleepService("alpha", t.TempDir()),
		sleepService("bravo", t.TempDir()),
	}

	m, err := NewManager(cfg, svcs, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	names := m.ServiceNames()
	expected := []string{"alpha", "bravo", "charlie"}
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %v", names)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Fatalf("names[%d]: got %q, want %q", i, name, expected[i])
		}
	}
}

func TestManager_StartAllStopAll(t *testing.T) {
	cfg := testConfig(t)
	svcs := []config.ServiceConfig{
		sleepService("svc1", t.TempDir()),
		sleepService("svc2", t.TempDir()),
	}

	m, err := NewManager(cfg, svcs, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.StartAll()
	running, _ := m.CountRunning()
	if running != 2 {
		t.Fatalf("after StartAll: running=%d, want 2", running)
	}

	m.StopAll()
	time.Sleep(200 * time.Millisecond)

	running, _ = m.CountRunning()
	if running != 0 {
		t.Fatalf("after StopAll: running=%d, want 0", running)
	}
}

func TestManager_StopShutdown(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}

	m.Do("testsvc", "start")

	// Stop should complete without hanging
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop timed out")
	}
}

func TestManager_SetOnChange(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	var mu sync.Mutex
	var events []string
	m.SetOnChange(func(name, event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, name+":"+event)
	})

	m.Do("testsvc", "start")
	// onChange is called within the service loop (same goroutine as Do),
	// but give it a moment to be safe.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n == 0 {
		t.Fatal("expected onChange to be called")
	}
}

func TestManager_SetNotifier(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	rec := &recordingNotifier{}
	m.SetNotifier(rec)

	m.Do("testsvc", "start")
	time.Sleep(100 * time.Millisecond)

	if len(rec.getServiceEvents()) == 0 {
		t.Fatal("expected ServiceEvent after SetNotifier")
	}
}

func TestManager_Reload(t *testing.T) {
	// Create services dir with a service config file
	servicesDir := t.TempDir()
	svcData := []byte("dir: /tmp\nentrypoint:\n  - sleep\n  - \"3600\"\n")
	if err := os.WriteFile(filepath.Join(servicesDir, "svc1.yaml"), svcData, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir

	services, err := config.LoadServices(servicesDir)
	if err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(cfg, services, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	if len(m.ServiceNames()) != 1 {
		t.Fatalf("expected 1 service, got %v", m.ServiceNames())
	}

	// Add a second service
	svc2Data := []byte("dir: /tmp\nentrypoint:\n  - sleep\n  - \"3600\"\n")
	if err := os.WriteFile(filepath.Join(servicesDir, "svc2.yaml"), svc2Data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}

	names := m.ServiceNames()
	sort.Strings(names)
	if len(names) != 2 || names[0] != "svc1" || names[1] != "svc2" {
		t.Fatalf("after reload: got %v", names)
	}

	// Remove svc1
	_ = os.Remove(filepath.Join(servicesDir, "svc1.yaml"))
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}

	names = m.ServiceNames()
	if len(names) != 1 || names[0] != "svc2" {
		t.Fatalf("after removing svc1: got %v", names)
	}
}

func TestManager_ProcessExitNotification(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	// Process that exits quickly
	svc := config.ServiceConfig{
		Name:       "shortsvc",
		Dir:        dir,
		Entrypoint: []string{"sh", "-c", "exit 0"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("shortsvc", "start")

	// Wait for process to exit and notification
	time.Sleep(1 * time.Second)

	// Should have got "started" and "exited" events
	events := rec.getServiceEvents()
	foundStarted := false
	foundExited := false
	for _, e := range events {
		if e.name == "shortsvc" && e.a == "started" {
			foundStarted = true
		}
		if e.name == "shortsvc" && e.a == "exited" {
			foundExited = true
		}
	}
	if !foundStarted {
		t.Fatalf("expected 'started' event, got %+v", events)
	}
	if !foundExited {
		t.Fatalf("expected 'exited' event, got %+v", events)
	}
}

func TestManager_DeployNoSteps(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		// No deploy steps — should vacuously succeed and restart
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	// Start the service first so restart works
	m.Do("testsvc", "start")

	if err := m.RequestDeploy("testsvc"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["testsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(rec.getDeployStarted()) == 0 {
		t.Fatal("expected DeployStarted notification")
	}
	if len(rec.getDeploySucceeded()) == 0 {
		t.Fatal("expected DeploySucceeded notification")
	}
}

func TestManager_DeployRestartFailure(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	// Service with deploy steps but a backend that will fail restart
	// We use a service_name that doesn't exist (launchctl/systemctl) - but that
	// won't work in tests. Instead, use a process that can deploy but the
	// restart step after deploy will fail because the entrypoint is bad.
	svc := config.ServiceConfig{
		Name:       "badsvc",
		Dir:        dir,
		Entrypoint: []string{"/nonexistent/binary"},
		Deploy:     []string{"echo ok"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	if err := m.RequestDeploy("badsvc"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["badsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	failed := rec.getDeployFailed()
	if len(failed) == 0 {
		t.Fatal("expected DeployFailed notification for restart failure")
	}
	// The failed step should be "restart"
	found := false
	for _, f := range failed {
		if f.a == "restart" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected failed step 'restart', got %+v", failed)
	}
}

func TestManager_ConcurrentOps(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Run multiple ops concurrently - should not deadlock or race
	done := make(chan struct{}, 10)
	for i := 0; i < 5; i++ {
		go func() {
			m.Do("testsvc", "status")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent ops timed out")
		}
	}
}

func TestManager_StateSavedAfterStart(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("testsvc", "start")

	// Give the service loop time to persist state after the op completes
	time.Sleep(200 * time.Millisecond)

	// State file should exist with backend data
	s, _, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatalf("state file should exist after start: %v", err)
	}
	if s.Status != "running" {
		t.Fatalf("state status: got %q, want running", s.Status)
	}
	if s.Backend == nil {
		t.Fatal("state backend should be non-nil after start")
	}
}

func TestManager_DeployStateSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	logDir := t.TempDir()
	dir := t.TempDir()
	rec := &recordingNotifier{}

	cfg := &config.Config{
		LogDir:   logDir,
		StateDir: stateDir,
	}
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"echo deploying"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}

	m.SignalReady() // deploys wait for the manager to be ready

	// Start and deploy
	m.Do("testsvc", "start")
	if err := m.RequestDeploy("testsvc"); err != nil {
		t.Fatal(err)
	}

	// Wait for deploy to complete
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["testsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify deploy succeeded
	states := m.GetAllStates()
	if states["testsvc"].LastResult != "success" {
		t.Fatalf("deploy result: got %q, want success", states["testsvc"].LastResult)
	}

	m.Stop()

	// Create a NEW manager with the same stateDir (simulating restart)
	cfg2 := &config.Config{
		LogDir:   logDir,
		StateDir: stateDir,
	}
	m2, err := NewManager(cfg2, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Stop()

	// Verify the new manager loaded the persisted deploy info
	states2 := m2.GetAllStates()
	if states2["testsvc"].LastResult != "success" {
		t.Fatalf("after restart, LastResult: got %q, want success", states2["testsvc"].LastResult)
	}
	if states2["testsvc"].LastDeploy.IsZero() {
		t.Fatal("after restart, LastDeploy should be non-zero")
	}
}

func TestManager_OldFormatMigration(t *testing.T) {
	stateDir := t.TempDir()
	logDir := t.TempDir()
	dir := t.TempDir()

	// Write an old-format state file (pid/pgid at top level, no backend sub-object)
	oldState := `{"status":"running","pid":99999999,"pgid":99999999,"log_path":"/tmp/old.log","boot_time":1711000000,"create_time":1711000123000}`
	if err := os.WriteFile(filepath.Join(stateDir, "testsvc.json"), []byte(oldState), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		LogDir:   logDir,
		StateDir: stateDir,
	}
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	// The manager should have loaded the old-format state and the ProcessBackend
	// should have migrated it via RestoreBackendState. The PID 99999999 is dead,
	// so adoption should have reported "stale pid". The service should be functional.
	result := m.Do("testsvc", "status")
	if !strings.Contains(result, "stopped") {
		t.Fatalf("expected stopped (stale pid), got %q", result)
	}
}

func TestManager_GetServiceState(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Should find existing service
	state, ok := m.GetServiceState("testsvc")
	if !ok {
		t.Fatal("expected to find state for testsvc")
	}
	if state.Status == "" {
		t.Fatal("expected non-empty status")
	}

	// Should not find nonexistent service
	_, ok = m.GetServiceState("nonexistent")
	if ok {
		t.Fatal("expected not found for nonexistent")
	}
}

func TestManager_GetServiceLogs(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        dir,
		Entrypoint: []string{"sh", "-c", "echo hello && sleep 3600"},
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("testsvc", "start")
	time.Sleep(200 * time.Millisecond)

	logs := m.GetServiceLogs("testsvc")
	if !strings.Contains(logs, "hello") {
		t.Fatalf("logs should contain 'hello', got %q", logs)
	}
}

func TestManager_SelfDeploy(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	svc := config.ServiceConfig{
		Name:       "selfsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"echo deploying"},
		SelfDeploy: true,
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	// Start the service first
	m.Do("selfsvc", "start")

	// Record the status before deploy — should be running
	states := m.GetAllStates()
	if states["selfsvc"].Status != "running" {
		t.Fatalf("status before deploy: got %q, want running", states["selfsvc"].Status)
	}

	if err := m.RequestDeploy("selfsvc"); err != nil {
		t.Fatal(err)
	}

	// Wait for shutdown signal (self-deploy success should close ShutdownCh)
	select {
	case <-m.ShutdownCh():
		// good — shutdown was signalled
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ShutdownCh to be closed")
	}

	// DeploySucceeded should have been called
	if len(rec.getDeploySucceeded()) == 0 {
		t.Fatal("expected DeploySucceeded notification")
	}

	// State should be persisted with LastResult: "success"
	s, _, err := LoadState(cfg.StateDir, "selfsvc")
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if s.LastResult != "success" {
		t.Fatalf("state LastResult: got %q, want success", s.LastResult)
	}

	// The backend should NOT have been restarted — the service should still
	// be running with the same process from the initial start.
	states = m.GetAllStates()
	if states["selfsvc"].Status != "running" {
		t.Fatalf("status after self-deploy: got %q, want running", states["selfsvc"].Status)
	}
}

func TestManager_SelfDeploy_Failure(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	rec := &recordingNotifier{}

	svc := config.ServiceConfig{
		Name:       "selfsvc",
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"false"}, // will fail
		SelfDeploy: true,
	}

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	if err := m.RequestDeploy("selfsvc"); err != nil {
		t.Fatal(err)
	}

	// Wait for deploy to complete by polling state
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for deploy")
		default:
		}
		states := m.GetAllStates()
		if states["selfsvc"].Status != "deploying" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// ShutdownCh should NOT be closed
	select {
	case <-m.ShutdownCh():
		t.Fatal("ShutdownCh should not be closed on deploy failure")
	default:
		// good — not closed
	}

	// DeployFailed should have been called
	if len(rec.getDeployFailed()) == 0 {
		t.Fatal("expected DeployFailed notification")
	}

	// State should show LastResult: "failed"
	s, _, err := LoadState(cfg.StateDir, "selfsvc")
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if s.LastResult != "failed" {
		t.Fatalf("state LastResult: got %q, want failed", s.LastResult)
	}
}

func TestManager_NotifyWebhook(t *testing.T) {
	cfg := testConfig(t)
	rec := &recordingNotifier{}

	m, err := NewManager(cfg, nil, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	info := WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		Compare:   "https://example/compare",
		Pusher:    "alice",
		CommitID:  "deadbeef",
		CommitMsg: "fix things",
		CommitURL: "https://example/commit",
		Author:    "Alice",
		Timestamp: "2026-04-10T00:00:00Z",
	}
	m.NotifyWebhook("svc", info)

	got := rec.getWebhookReceived()
	if len(got) != 1 {
		t.Fatalf("expected 1 webhook call, got %d", len(got))
	}
	if got[0].name != "svc" {
		t.Fatalf("name: got %q", got[0].name)
	}
	if got[0].info != info {
		t.Fatalf("info: got %+v", got[0].info)
	}
}

func TestManager_ServiceLoopWithContext(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}

	// Cancel immediately should cause loop to exit
	m.Stop()

	// Do should return an error-like response since ctx is cancelled
	result := m.Do("testsvc", "status")
	_ = result // may return error or timeout, just shouldn't hang
}

// --- C1: autostart on cold boot ---

// autostartService builds a ServiceConfig that runs `sleep 3600`. autostart
// is a tri-state; pass nil to use the global default.
func autostartService(name, dir string, autostart *bool) config.ServiceConfig {
	svc := config.ServiceConfig{
		Name:       name,
		Dir:        dir,
		Entrypoint: []string{"sleep", "3600"},
	}
	if autostart != nil {
		svc.Autostart = autostart
	}
	return svc
}

// writeStoppedState writes a state file with DesiredStatus:"stopped" for
// the named service, simulating "operator stopped this on the previous
// run". C1 consults DesiredStatus (not Status) so an unexpected
// process exit does not suppress autostart.
func writeStoppedState(t *testing.T, stateDir, name string) {
	t.Helper()
	s := State{Status: "stopped", DesiredStatus: "stopped"}
	if err := SaveState(stateDir, name, s); err != nil {
		t.Fatal(err)
	}
}

func TestNewManager_AutostartDisabledByDefault(t *testing.T) {
	// With no autostart set anywhere, a process service with a dead pid
	// stays down (preserves pre-C1 behaviour). The operator can still
	// start it manually via the frontends.
	cfg := testConfig(t)
	cfg.StartOnBoot = false
	dir := t.TempDir()

	// Pre-seed a stale state file so the process backend tries and
	// fails to adopt (stale pid).
	stateDir := cfg.StateDir
	staleState := State{
		Status:  "running",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}
	if err := SaveState(stateDir, "testsvc", staleState); err != nil {
		t.Fatal(err)
	}

	svc := autostartService("testsvc", dir, nil)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Give the service loop a moment in case it would try to start.
	time.Sleep(100 * time.Millisecond)
	result := m.Do("testsvc", "status")
	if !strings.Contains(result, "stopped") {
		t.Fatalf("autostart-disabled service should be stopped, got: %q", result)
	}
}

func TestNewManager_AutostartPerServiceStartsProcess(t *testing.T) {
	// Per-service autostart=true + dead pid + status:"running" →
	// process should be started on cold boot after SignalReady.
	cfg := testConfig(t)
	dir := t.TempDir()
	stateDir := cfg.StateDir

	staleState := State{
		Status:  "running",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}
	if err := SaveState(stateDir, "testsvc", staleState); err != nil {
		t.Fatal(err)
	}

	yes := true
	svc := autostartService("testsvc", dir, &yes)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Unblock the autostart gate (the service loop waits on
	// restartOnStartup / readyCh before issuing the Start).
	m.SignalReady()

	// Poll until the process is running, with a generous timeout.
	deadline := time.Now().Add(5 * time.Second)
	for {
		result := m.Do("testsvc", "status")
		if strings.Contains(result, "running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("autostart service did not start in time, status: %q", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNewManager_AutostartGlobalDefaultStartsProcess(t *testing.T) {
	// Global Config.StartOnBoot=true with no per-service override
	// should autostart the process.
	cfg := testConfig(t)
	cfg.StartOnBoot = true
	dir := t.TempDir()
	stateDir := cfg.StateDir

	staleState := State{
		Status:  "running",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}
	if err := SaveState(stateDir, "testsvc", staleState); err != nil {
		t.Fatal(err)
	}

	svc := autostartService("testsvc", dir, nil)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.SignalReady()

	deadline := time.Now().Add(5 * time.Second)
	for {
		result := m.Do("testsvc", "status")
		if strings.Contains(result, "running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("global-autostart service did not start in time, status: %q", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNewManager_AutostartRespectsPersistedStopped(t *testing.T) {
	// A service with persisted status:"stopped" must NOT be autostarted
	// even when autostart=true, unless AutostartForce is also set.
	cfg := testConfig(t)
	dir := t.TempDir()
	stateDir := cfg.StateDir

	writeStoppedState(t, stateDir, "testsvc")

	yes := true
	svc := autostartService("testsvc", dir, &yes)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.SignalReady()

	// Give the service loop time to (not) start.
	time.Sleep(200 * time.Millisecond)
	result := m.Do("testsvc", "status")
	if !strings.Contains(result, "stopped") {
		t.Fatalf("stopped service should stay stopped, got: %q", result)
	}
}

func TestNewManager_AutostartForceOverridesPersistedStopped(t *testing.T) {
	// With AutostartForce, a service with status:"stopped" in its
	// state file is still started on cold boot.
	cfg := testConfig(t)
	dir := t.TempDir()
	stateDir := cfg.StateDir

	writeStoppedState(t, stateDir, "testsvc")

	yes := true
	svc := autostartService("testsvc", dir, &yes)
	svc.AutostartForce = true

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.SignalReady()

	deadline := time.Now().Add(5 * time.Second)
	for {
		result := m.Do("testsvc", "status")
		if strings.Contains(result, "running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("autostart_force service did not start, status: %q", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNewManager_AutostartPerServiceFalseOverridesGlobal(t *testing.T) {
	// Per-service autostart=false must override global true.
	cfg := testConfig(t)
	cfg.StartOnBoot = true
	dir := t.TempDir()
	stateDir := cfg.StateDir

	staleState := State{
		Status:  "running",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}
	if err := SaveState(stateDir, "testsvc", staleState); err != nil {
		t.Fatal(err)
	}

	no := false
	svc := autostartService("testsvc", dir, &no)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.SignalReady()
	time.Sleep(200 * time.Millisecond)
	result := m.Do("testsvc", "status")
	if !strings.Contains(result, "stopped") {
		t.Fatalf("per-service autostart=false should win, got: %q", result)
	}
}

func TestServiceConfig_ShouldAutostart(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		svc      config.ServiceConfig
		global   bool
		expected bool
	}{
		{"unset+global-false", config.ServiceConfig{}, false, false},
		{"unset+global-true", config.ServiceConfig{}, true, true},
		{"per-yes-overrides-global-false", config.ServiceConfig{Autostart: &yes}, false, true},
		{"per-yes-overrides-global-true", config.ServiceConfig{Autostart: &yes}, true, true},
		{"per-no-overrides-global-true", config.ServiceConfig{Autostart: &no}, true, false},
		{"per-no-overrides-global-false", config.ServiceConfig{Autostart: &no}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.expected, c.svc.ShouldAutostart(c.global))
		})
	}
}

// --- C5: single-instance lock on state dir ---

func TestNewManager_TakesStateLock(t *testing.T) {
	// A second manager pointed at the same state dir must fail fast
	// with a descriptive error rather than co-managing the same
	// processes. (C5 acceptance criterion.)
	cfg := testConfig(t)
	cfg.LogDir = t.TempDir()
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Second instance: same state dir, must fail.
	_, err = NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err == nil {
		t.Fatal("second NewManager on the same state dir should fail")
	}
	assert.Contains(t, err.Error(), "another mezzaops instance")
}

func TestNewManager_LockReleasedOnStop(t *testing.T) {
	// After the first manager stops, a new manager pointed at the
	// same state dir must succeed. (C5 acceptance criterion.)
	cfg := testConfig(t)
	cfg.LogDir = t.TempDir()
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m1, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	m1.Stop()

	m2, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatalf("second NewManager after first Stop should succeed, got: %v", err)
	}
	defer m2.Stop()
}

func TestNewManager_LockFileContainsPID(t *testing.T) {
	// Sanity: the lock file is created in the state dir and contains
	// the holding PID. Operators can `cat` it to see who holds the
	// lock when troubleshooting.
	cfg := testConfig(t)
	cfg.LogDir = t.TempDir()
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	lockPath := filepath.Join(cfg.StateDir, "mezzaops.lock")
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatalf("expected lock file at %q, got: %v", lockPath, readErr)
	}
	assert.Contains(t, string(data), "pid=")
}

// --- C1 follow-up: a crashed process is NOT an operator stop ---

func TestNewManager_AutostartIgnoresCrashedProcess(t *testing.T) {
	// Regression: an unexpected process exit writes status="stopped"
	// to the state file (because the live status is "stopped"). C1
	// must not interpret that as an operator stop; it must consult
	// DesiredStatus, which is only set on explicit start/stop ops.
	cfg := testConfig(t)
	dir := t.TempDir()
	stateDir := cfg.StateDir

	// Simulate a crashed process: Status="stopped" (the live status
	// was overwritten when the previous run noticed the dead pid and
	// re-saved state), DesiredStatus="" (no operator intent).
	crashedState := State{
		Status:  "stopped", // written by the previous run's exit handler
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}
	if err := SaveState(stateDir, "testsvc", crashedState); err != nil {
		t.Fatal(err)
	}

	yes := true
	svc := autostartService("testsvc", dir, &yes)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.SignalReady()

	// The service should be started (not treated as a stopped-by-operator).
	deadline := time.Now().Add(5 * time.Second)
	for {
		result := m.Do("testsvc", "status")
		if strings.Contains(result, "running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("crashed-then-autostart should have started, got: %q", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServiceState_DesiredStatusRoundTrip(t *testing.T) {
	// DesiredStatus must survive save/load with the same value.
	dir := t.TempDir()
	want := State{
		Status:        "running",
		DesiredStatus: "stopped",
	}
	if err := SaveState(dir, "testsvc", want); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadState(dir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "stopped", got.DesiredStatus, "DesiredStatus must round-trip")
	assert.Equal(t, "running", got.Status, "Status must round-trip")
}

func TestServiceState_DesiredStatusNotClobberedBySave(t *testing.T) {
	// saveServiceState re-queries the backend for live status, but
	// must not overwrite DesiredStatus (operator intent). Set up a
	// service whose DesiredStatus="stopped" and live status="running"
	// and verify a save preserves DesiredStatus.
	cfg := testConfig(t)
	dir := t.TempDir()
	svc := sleepService("testsvc", dir)

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Inject a DesiredStatus.
	m.mu.Lock()
	ms := m.services["testsvc"]
	ms.stateMu.Lock()
	ms.state.DesiredStatus = "stopped"
	ms.stateMu.Unlock()
	m.mu.Unlock()

	// Start the service (live status becomes "running", DesiredStatus
	// is cleared by handleOp "start" — that is the right behaviour for
	// an explicit start). Save and verify DesiredStatus is "running".
	m.Do("testsvc", "start")
	m.saveServiceState(ms)

	s, _, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "running", s.DesiredStatus, "operator's start should clear persisted stop")
}

// --- C5 follow-up: reload respects autostart field changes ---

func TestServiceConfigEqual_AutostartChangesTriggerReload(t *testing.T) {
	// A change to Autostart (per-service) must be detected by
	// serviceConfigEqual so Reload re-creates the managed service.
	yes, no := true, false
	cases := []struct {
		name string
		a, b config.ServiceConfig
		want bool
	}{
		{"identical", config.ServiceConfig{}, config.ServiceConfig{}, true},
		{"autostart-nil-vs-true", config.ServiceConfig{}, config.ServiceConfig{Autostart: &yes}, false},
		{"autostart-true-vs-false", config.ServiceConfig{Autostart: &yes}, config.ServiceConfig{Autostart: &no}, false},
		{"autostart-true-vs-true", config.ServiceConfig{Autostart: &yes}, config.ServiceConfig{Autostart: &yes}, true},
		{"adopt-nil-vs-false", config.ServiceConfig{}, config.ServiceConfig{Adopt: &no}, false},
		{"autostart-force-changed", config.ServiceConfig{AutostartForce: false}, config.ServiceConfig{AutostartForce: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, serviceConfigEqual(c.a, c.b))
		})
	}
}

// --- Boot-resilience follow-ups ---

// waitForPersistedDesired polls the state file until desired_status matches.
// The service loop persists state just after replying to an op, so the file
// can lag the reply briefly.
func waitForPersistedDesired(t *testing.T, stateDir, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _, err := LoadState(stateDir, name)
		if err == nil && s.DesiredStatus == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted desired_status for %s: got %q (err %v), want %q", name, s.DesiredStatus, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForStatus polls Do(name, "status") until it contains want.
func waitForStatus(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result := m.Do(name, "status")
		if strings.Contains(result, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service %s did not reach %q, status: %q", name, want, result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManager_ReloadPreservesDesiredStateWhenAutostartToggled(t *testing.T) {
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	base := "dir: /tmp\nentrypoint:\n  - sleep\n  - \"3600\"\n"
	if err := os.WriteFile(svcPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	services, err := config.LoadServices(servicesDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(cfg, services, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // as in production, frontends are up before any reload

	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")

	// Turn autostart on for the running service and reload.
	if err := os.WriteFile(svcPath, []byte(base+"autostart: true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}

	waitForStatus(t, m, "svc1", "running")
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "running" {
		t.Fatalf("DesiredStatus after reload: got %q, want running", got)
	}
	waitForPersistedDesired(t, cfg.StateDir, "svc1", "running")
}

func TestManager_ReloadKeepsStoppedServiceStopped(t *testing.T) {
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	base := "dir: /tmp\nentrypoint:\n  - sleep\n  - \"3600\"\n"
	if err := os.WriteFile(svcPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	services, _ := config.LoadServices(servicesDir)
	m, err := NewManager(cfg, services, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady()

	m.Do("svc1", "start")
	m.Do("svc1", "stop")

	if err := os.WriteFile(svcPath, []byte(base+"autostart: true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	waitForStatus(t, m, "svc1", "stopped")
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "stopped" {
		t.Fatalf("DesiredStatus after reload: got %q, want stopped", got)
	}
}

func TestManager_FailedStartRecordsIntent(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			cfg := testConfig(t)
			writeStoppedState(t, cfg.StateDir, "badsvc")
			svc := config.ServiceConfig{
				Name:       "badsvc",
				Dir:        t.TempDir(),
				Entrypoint: []string{"/nonexistent/mezzaops-test-binary"},
			}
			m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Stop()

			result := m.Do("badsvc", op)
			if !strings.Contains(result, "failed") {
				t.Fatalf("expected %s to fail, got %q", op, result)
			}
			if got := m.GetAllStates()["badsvc"].DesiredStatus; got != "running" {
				t.Fatalf("DesiredStatus after failed %s: got %q, want running", op, got)
			}
			waitForPersistedDesired(t, cfg.StateDir, "badsvc", "running")
		})
	}
}

func TestManager_DeployRecordsRunningIntent(t *testing.T) {
	cfg := testConfig(t)
	svc := config.ServiceConfig{
		Name:       "testsvc",
		Dir:        t.TempDir(),
		Entrypoint: []string{"sleep", "3600"},
		Deploy:     []string{"echo deploying"},
	}
	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady() // deploys wait for the manager to be ready

	m.Do("testsvc", "start")
	m.Do("testsvc", "stop")
	if got := m.GetAllStates()["testsvc"].DesiredStatus; got != "stopped" {
		t.Fatalf("precondition: DesiredStatus %q, want stopped", got)
	}

	if err := m.RequestDeploy("testsvc"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.GetAllStates()["testsvc"]
		if st.Status == "running" && st.LastResult == "success" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deploy did not finish: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := m.GetAllStates()["testsvc"].DesiredStatus; got != "running" {
		t.Fatalf("DesiredStatus after deploy: got %q, want running", got)
	}
	waitForPersistedDesired(t, cfg.StateDir, "testsvc", "running")
}

func TestManager_InternalStopPreservesDesiredStatus(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Do("svc", "start")
	if result := m.doOn(m.services["svc"], syncOp{internalStop: true, result: make(chan string, 1)}); result != "stopped" {
		t.Fatalf("internal stop: got %q", result)
	}
	if got := m.GetAllStates()["svc"].DesiredStatus; got != "running" {
		t.Fatalf("DesiredStatus after internal stop: got %q, want running", got)
	}
	if pb, _ := asProcessBackend(m.services["svc"].backend); pb.IsRunning() {
		t.Fatal("internal stop should have stopped the process")
	}
}

// reloadFixture creates a manager with one service svc1 loaded from a
// services dir, returning a helper that rewrites the service file.
func reloadFixture(t *testing.T, extra string) (*Manager, *config.Config, func(extra string)) {
	t.Helper()
	return reloadFixtureWith(t, extra, nil, NopNotifier{})
}

// reloadFixtureWith is reloadFixture with an optional backend factory.
func reloadFixtureWith(t *testing.T, extra string, factory func(config.ServiceConfig) Backend, notifier Notifier) (*Manager, *config.Config, func(extra string)) {
	t.Helper()
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	write := func(extra string) {
		data := "dir: /tmp\nentrypoint:\n  - sleep\n  - \"3600\"\n" + extra
		if err := os.WriteFile(svcPath, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(extra)
	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	services, err := config.LoadServices(servicesDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newManager(cfg, services, notifier, factory, hostRules())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	m.SignalReady()
	return m, cfg, write
}

func TestManager_ReloadDoesNotStartCrashedAutostartService(t *testing.T) {
	// A crashed autostart service (desired running, process dead) must
	// not be started by a reload: only a service that was running
	// before the reload is running after it.
	m, _, write := reloadFixture(t, "autostart: true\n")
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")

	pb := m.services["svc1"].backend.(*ProcessBackend)
	pb.mu.Lock()
	pgid := pb.pgid
	pb.mu.Unlock()
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	waitForStatus(t, m, "svc1", "stopped")

	write("autostart: true\nbranch: other\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	waitForStatus(t, m, "svc1", "stopped")
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "running" {
		t.Fatalf("DesiredStatus: got %q, want running (carried over)", got)
	}
}

func TestManager_ReloadStoppedAdoptFalseServiceStaysStopped(t *testing.T) {
	m, _, write := reloadFixture(t, "adopt: false\n")
	m.Do("svc1", "start")
	m.Do("svc1", "stop")

	write("adopt: false\nbranch: other\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	waitForStatus(t, m, "svc1", "stopped")
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "stopped" {
		t.Fatalf("DesiredStatus: got %q, want stopped", got)
	}
}

// failingStopBackend wraps a ProcessBackend and fails Stop while fail is set.
// Embedding *ProcessBackend keeps it recognised as process-backed, so the
// production adoption path runs against it.
type failingStopBackend struct {
	*ProcessBackend
	fail *atomic.Bool
}

func (f failingStopBackend) Stop(ctx context.Context) error {
	if f.fail.Load() {
		return errors.New("boom")
	}
	return f.ProcessBackend.Stop(ctx)
}

func TestManager_ReloadAbortsWhenInternalStopFails(t *testing.T) {
	var fail atomic.Bool
	logDir := t.TempDir()
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		return failingStopBackend{ProcessBackend: pb, fail: &fail}
	}
	m, _, write := reloadFixtureWith(t, "", factory, NopNotifier{})
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")

	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()

	fail.Store(true)
	write("branch: other\n")
	err := m.Reload()
	if err == nil || !strings.Contains(err.Error(), "svc1") {
		t.Fatalf("expected Reload to report the failed stop for svc1, got %v", err)
	}
	m.mu.Lock()
	current := m.services["svc1"]
	m.mu.Unlock()
	if current != old {
		t.Fatal("failed stop must keep the old service in place")
	}
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "running" {
		t.Fatalf("DesiredStatus after failed stop: got %q, want running", got)
	}
	fail.Store(false) // let cleanup stop the process
}

func TestManager_ReloadRespectsStopQueuedBeforeReload(t *testing.T) {
	m, _, write := reloadFixture(t, "")
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")

	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()
	// Queue an operator stop without waiting for it; the loop handles it
	// before the reload's internal stop, which must see its effect.
	old.opCh <- syncOp{op: "stop", result: make(chan string, 1)}

	write("autostart: true\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	waitForStatus(t, m, "svc1", "stopped")
	if got := m.GetAllStates()["svc1"].DesiredStatus; got != "stopped" {
		t.Fatalf("DesiredStatus: got %q, want stopped", got)
	}
}

// blockingStopBackend blocks Stop until release is closed (when non-nil).
type blockingStopBackend struct {
	*ProcessBackend
	entered chan struct{}
	release chan struct{}
}

func (b blockingStopBackend) Stop(ctx context.Context) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.ProcessBackend.Stop(ctx)
}

func TestManager_DeployRacingWithReloadRunsExactlyOnce(t *testing.T) {
	logDir := t.TempDir()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		if svc.Branch == "" {
			return blockingStopBackend{ProcessBackend: pb, entered: entered, release: release}
		}
		return pb
	}
	runsFile := filepath.Join(t.TempDir(), "runs")
	deployYAML := "deploy:\n  - echo run >> " + runsFile + "\n"
	m, _, write := reloadFixtureWith(t, deployYAML, factory, NopNotifier{})

	reloadDone := make(chan error, 1)
	write(deployYAML + "branch: other\n")
	go func() { reloadDone <- m.Reload() }()

	// Reload's internal stop is now running inside the old loop.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("internal stop did not start")
	}
	if err := m.RequestDeploy("svc1"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if st := m.GetAllStates()["svc1"]; st.LastResult == "success" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deploy requested during reload was dropped: %+v", m.GetAllStates()["svc1"])
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a duplicate forward would run by now
	data, err := os.ReadFile(runsFile)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "run"); n != 1 {
		t.Fatalf("deploy ran %d times, want exactly 1", n)
	}
}

func TestManager_DeployForRemovedServiceIsReported(t *testing.T) {
	logDir := t.TempDir()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		return blockingStopBackend{ProcessBackend: pb, entered: entered, release: release}
	}
	rec := &recordingNotifier{}
	m, cfg, _ := reloadFixtureWith(t, "deploy:\n  - echo deploying\n", factory, rec)

	// Removing the service makes Reload stop it, which blocks in the loop.
	if err := os.Remove(filepath.Join(cfg.ServicesDir, "svc1.yaml")); err != nil {
		t.Fatal(err)
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- m.Reload() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not start")
	}
	if err := m.RequestDeploy("svc1"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(rec.getDeployFailed()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a deploy for a removed service was dropped without a failure report")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rec.getDeployStarted()) != 0 {
		t.Fatal("a removed service must not run its deploy")
	}
}

func TestManager_ConcurrentReloadsCreateOneReplacement(t *testing.T) {
	var created atomic.Int32
	logDir := t.TempDir()
	factory := func(svc config.ServiceConfig) Backend {
		created.Add(1)
		return NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
	}
	m, _, write := reloadFixtureWith(t, "", factory, NopNotifier{})
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")
	before := created.Load()

	write("branch: other\n")
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Reload()
		}()
	}
	wg.Wait()
	if got := created.Load() - before; got != 1 {
		t.Fatalf("concurrent reloads created %d replacement services, want 1", got)
	}
}

func TestManager_DoRejectsInternalStopString(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if result := m.Do("svc", "stop-internal"); !strings.Contains(result, "unknown command") {
		t.Fatalf("Do must not accept the internal stop: got %q", result)
	}
}

func TestManager_OpsQueuedBehindInternalStopAreRejected(t *testing.T) {
	m, _, _ := reloadFixture(t, "")
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")
	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()

	// Queue a stop-internal followed by an op behind it, directly on the
	// old loop: the second must be rejected, not run on the old backend.
	var snap reloadSnapshot
	first := syncOp{internalStop: true, result: make(chan string, 1), snap: &snap}
	second := syncOp{op: "start", result: make(chan string, 1)}
	old.opCh <- first
	old.opCh <- second
	if got := <-first.result; got != "stopped" {
		t.Fatalf("internal stop: %q", got)
	}
	if !snap.backendRunning {
		t.Fatal("snapshot should record the service was running")
	}
	if got := <-second.result; got != reloadedMsg {
		t.Fatalf("op queued behind the internal stop: got %q, want %q", got, reloadedMsg)
	}
	if pb, _ := asProcessBackend(old.backend); pb.IsRunning() {
		t.Fatal("op behind the internal stop ran on the old backend")
	}

}

func TestManager_ReloadRetiresOldServiceLoop(t *testing.T) {
	m, _, write := reloadFixture(t, "")
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")
	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()

	// A real reload retires the loop, and later ops do not block.
	write("branch: other\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("old service loop did not exit after reload")
	}
	finished := make(chan string, 1)
	go func() { finished <- m.doOn(old, syncOp{op: "status", result: make(chan string, 1)}) }()
	select {
	case got := <-finished:
		if got != reloadedMsg {
			t.Fatalf("late op on retired service: got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("op on a retired service blocked")
	}
}

func TestManager_StateSavedBeforeOpReply(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.Do("svc", "start")
	m.Do("svc", "stop")
	// No polling: the reply must follow the save.
	s, _, err := LoadState(cfg.StateDir, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if s.DesiredStatus != "stopped" {
		t.Fatalf("state not saved before reply: desired_status %q", s.DesiredStatus)
	}
}

// --- cleanOrphans never signals a non-positive process group ---

func TestCleanOrphans_NeverKillsNonPositivePgid(t *testing.T) {
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	// State for a service that is no longer configured, with a live pid
	// but pgid 0: kill(-0) would signal our own process group.
	raw, _ := json.Marshal(processBackendState{PID: child.Process.Pid, PGID: 0})
	if err := SaveState(cfg.StateDir, "gone", State{Status: "running", Backend: raw}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []int
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		calls = append(calls, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { killFn = orig })

	m, err := NewManager(cfg, nil, NopNotifier{}) // NewManager runs cleanOrphans
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	mu.Lock()
	defer mu.Unlock()
	for _, pid := range calls {
		if pid >= 0 {
			t.Fatalf("cleanOrphans called kill(%d): would signal our own process group", pid)
		}
	}
}

func TestCleanOrphans_KillsVerifiedGroup(t *testing.T) {
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	raw, _ := json.Marshal(processBackendState{PID: child.Process.Pid, PGID: 424242})
	if err := SaveState(cfg.StateDir, "gone", State{Status: "running", Backend: raw}); err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error { got.Store(int64(pid)); return nil }
	t.Cleanup(func() { killFn = orig })

	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got.Load() != -424242 {
		t.Fatalf("cleanOrphans kill target: got %d, want -424242", got.Load())
	}
}

func TestNewManager_AdoptFalseAutostartsWhenNoStateFileExists(t *testing.T) {
	// A configured state dir with no state for the service means this
	// service never ran here, so no previous child can exist.
	cfg := testConfig(t)
	yes, no := true, false
	svc := autostartService("testsvc", t.TempDir(), &yes)
	svc.Adopt = &no
	// The rules this test is about, whatever platform runs it.
	m, err := newManager(cfg, []config.ServiceConfig{svc}, NopNotifier{}, nil, rulesLinux())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady()
	waitForStatus(t, m, "testsvc", "running")
}

// --- boot-time tolerance ---

func TestProcessBackend_TryAdoptRejectsNonPositivePid(t *testing.T) {
	for _, pid := range []int{0, -1} {
		b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", t.TempDir())
		b.adopt = true
		b.restoredState = &processBackendState{PID: pid}
		b.restoredStatus = "running"
		if outcome, msg := b.TryAdopt(""); outcome == adoptYes {
			t.Errorf("pid %d adopted: %q", pid, msg)
		}
	}
}

func TestManager_ReloadAbortsRemovalWhenStopFails(t *testing.T) {
	var fail atomic.Bool
	logDir := t.TempDir()
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		return failingStopBackend{ProcessBackend: pb, fail: &fail}
	}
	m, cfg, _ := reloadFixtureWith(t, "", factory, NopNotifier{})
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")
	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()

	fail.Store(true)
	if err := os.Remove(filepath.Join(cfg.ServicesDir, "svc1.yaml")); err != nil {
		t.Fatal(err)
	}
	err := m.Reload()
	if err == nil || !strings.Contains(err.Error(), "svc1") {
		t.Fatalf("expected Reload to report the failed stop, got %v", err)
	}
	m.mu.Lock()
	current := m.services["svc1"]
	m.mu.Unlock()
	if current != old || old.ctx.Err() != nil {
		t.Fatal("a service whose stop failed must stay in place and keep running its loop")
	}
	fail.Store(false)
}

func TestManager_ReloadDuringReadinessWaitReturnsPromptly(t *testing.T) {
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	base := "dir: /tmp\nautostart: true\nentrypoint:\n  - sleep\n  - \"3600\"\n"
	if err := os.WriteFile(svcPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	services, _ := config.LoadServices(servicesDir)
	m, err := NewManager(cfg, services, NopNotifier{}) // no state: autostart is pending on readyCh
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop() // also unblocks anything left waiting

	// An ordinary op waits behind the pending startup, as on main.
	statusDone := make(chan string, 1)
	go func() { statusDone <- m.Do("svc1", "status") }()

	if err := os.WriteFile(svcPath, []byte(base+"branch: other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- m.Reload() }()
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Reload blocked behind the readiness wait")
	}
	// The ordinary op was queued on the old service and must be answered,
	// not left hanging.
	select {
	case <-statusDone:
	case <-time.After(3 * time.Second):
		t.Fatal("op queued during the readiness wait was never answered")
	}
}

// pendingStartupManager builds a manager with one autostart service whose
// cold-boot start is pending on readiness (no SignalReady yet). With a
// factory the backend can be wrapped.
func pendingStartupManager(t *testing.T, factory func(config.ServiceConfig) Backend) (*Manager, *config.Config, string, func(extra string)) {
	t.Helper()
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	write := func(extra string) {
		data := "dir: /tmp\nautostart: true\nentrypoint:\n  - sleep\n  - \"3600\"\n" + extra
		if err := os.WriteFile(svcPath, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	services, _ := config.LoadServices(servicesDir)
	m, err := newManager(cfg, services, NopNotifier{}, factory, hostRules())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m, cfg, svcPath, write
}

func TestManager_OpsServedInOrderBeforeReadiness(t *testing.T) {
	m, _, _, _ := pendingStartupManager(t, nil)

	done := make(chan string, 1)
	go func() { done <- m.Do("svc1", "status") }()
	select {
	case got := <-done:
		if got != "stopped" {
			t.Fatalf("status before readiness: got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("op was not served while the startup op waits for readiness")
	}
}

func TestManager_OperatorOpBeforeReadinessCancelsStartup(t *testing.T) {
	m, _, _, _ := pendingStartupManager(t, nil)

	done := make(chan string, 1)
	go func() { done <- m.Do("svc1", "stop") }()
	select {
	case got := <-done:
		if got != "stopped" {
			t.Fatalf("stop before readiness: got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operator stop was not served before readiness")
	}
	m.SignalReady()
	time.Sleep(300 * time.Millisecond)
	if got := m.Do("svc1", "status"); got != "stopped" {
		t.Fatalf("operator stop must cancel the pending startup op, status %q", got)
	}
}

func TestManager_ReloadBeforeReadinessKeepsColdBootStart(t *testing.T) {
	m, _, _, write := pendingStartupManager(t, nil)

	write("branch: other\n")
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- m.Reload() }()
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Reload blocked before readiness")
	}
	m.SignalReady()
	waitForStatus(t, m, "svc1", "running")
}

func TestManager_FailedReloadStopBeforeReadinessKeepsStartup(t *testing.T) {
	var fail atomic.Bool
	logDir := t.TempDir()
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		return failingStopBackend{ProcessBackend: pb, fail: &fail}
	}
	m, _, _, write := pendingStartupManager(t, factory)

	fail.Store(true)
	write("branch: other\n")
	if err := m.Reload(); err == nil {
		t.Fatal("expected Reload to report the failed stop")
	}
	fail.Store(false)
	m.SignalReady()
	waitForStatus(t, m, "svc1", "running") // the startup op is still pending and runs
}

func TestManager_OperatorStopQueuedBeforeReloadIsAppliedInOrder(t *testing.T) {
	m, _, _, write := pendingStartupManager(t, nil)
	m.mu.Lock()
	old := m.services["svc1"]
	m.mu.Unlock()

	stop := syncOp{op: "stop", result: make(chan string, 1)}
	old.opCh <- stop
	write("branch: other\n")
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- m.Reload() }()
	if got := <-stop.result; got != "stopped" {
		t.Fatalf("operator stop queued before the reload: got %q, want it applied", got)
	}
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
	m.SignalReady()
	time.Sleep(300 * time.Millisecond)
	if got := m.Do("svc1", "status"); got != "stopped" {
		t.Fatalf("replacement started despite the operator stop: %q", got)
	}
	if d := m.GetAllStates()["svc1"].DesiredStatus; d != "stopped" {
		t.Fatalf("DesiredStatus: got %q, want stopped", d)
	}
}

// --- deploy forwarding targets the specific successor ---

func TestForwardDeploy_GoesToSuccessorNotSameNameService(t *testing.T) {
	rec := &recordingNotifier{}
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc1", t.TempDir())}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	newMS := func() *managedService {
		ctx, cancel := context.WithCancel(m.ctx)
		return &managedService{config: config.ServiceConfig{Name: "svc1"}, deployCh: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	}
	m.mu.Lock()
	current := m.services["svc1"] // a later service with the same name
	m.mu.Unlock()

	// Retired service with a recorded successor: deploy goes there.
	successor := newMS()
	retired := newMS()
	retired.successor = successor
	m.forwardDeploy(retired)
	if len(successor.deployCh) != 1 {
		t.Fatal("deploy was not forwarded to the recorded successor")
	}
	if len(current.deployCh) != 0 {
		t.Fatal("deploy reached an unrelated service with the same name")
	}

	// Removed service (no successor): reported, and not given to the
	// same-named service.
	removed := newMS()
	m.forwardDeploy(removed)
	if len(current.deployCh) != 0 {
		t.Fatal("deploy for a removed service reached a same-named service")
	}
	if len(rec.getDeployFailed()) != 1 {
		t.Fatalf("removed service deploy should be reported once, got %d", len(rec.getDeployFailed()))
	}

	// A successor that was itself retired passes the deploy along.
	second := newMS()
	middle := newMS()
	middle.successor = second
	middle.cancel()
	first := newMS()
	first.successor = middle
	m.forwardDeploy(first)
	if len(second.deployCh) != 1 {
		t.Fatal("deploy did not follow the successor chain")
	}
}

func TestIsAlive_RejectsNonPositivePids(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if IsAlive(pid) {
			t.Errorf("IsAlive(%d) must be false (kill would signal a group or everything)", pid)
		}
	}
}

func TestCleanOrphans_NeverKillsWithNonPositivePid(t *testing.T) {
	cfg := testConfig(t)
	raw, _ := json.Marshal(processBackendState{PID: -1, PGID: 4242})
	if err := SaveState(cfg.StateDir, "gone", State{Status: "running", Backend: raw}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	orig := killFn
	killFn = func(int, syscall.Signal) error { calls.Add(1); return nil }
	t.Cleanup(func() { killFn = orig })

	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if calls.Load() != 0 {
		t.Fatal("cleanOrphans killed on a record with pid -1")
	}
}

func TestManager_SaveStateErrorIsLogged(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	// Make the state dir unusable: replace it with a regular file.
	if err := os.RemoveAll(cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.StateDir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	m.Do("svc", "start")
	if out := buf.String(); !strings.Contains(out, "**svc**") || !strings.Contains(out, "saving state") {
		t.Fatalf("a failed state save must be logged with the service name, got: %q", out)
	}
}

// --- adopt: false autostart is gated on a host reboot (boot ID) ---

// withBootID substitutes the current boot ID for the test.
func withBootID(t *testing.T, id string) {
	t.Helper()
	orig := bootIDFn
	bootIDFn = func() string { return id }
	t.Cleanup(func() { bootIDFn = orig })
}

// adoptFalseAutostartManager starts a manager for an adopt:false, autostart
// service whose saved state (dead pid, desired running) has the given boot ID.
func adoptFalseAutostartManager(t *testing.T, savedBootID string) *Manager {
	t.Helper()
	cfg := testConfig(t)
	if err := SaveState(cfg.StateDir, "testsvc", State{
		Status:        "running",
		DesiredStatus: "running",
		Backend:       json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
		BootID:        savedBootID,
	}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	svc := autostartService("testsvc", t.TempDir(), &yes)
	svc.Adopt = &no
	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	m.SignalReady()
	return m
}

func TestNewManager_AdoptFalseAutostartsAfterHostReboot(t *testing.T) {
	withBootID(t, "boot-B")
	m := adoptFalseAutostartManager(t, "boot-A")
	waitForStatus(t, m, "testsvc", "running")
}

func TestNewManager_AdoptFalseDoesNotAutostartOnSameBoot(t *testing.T) {
	withBootID(t, "boot-A")
	m := adoptFalseAutostartManager(t, "boot-A")
	time.Sleep(300 * time.Millisecond)
	if result := m.Do("testsvc", "status"); !strings.Contains(result, "stopped") {
		t.Fatalf("adopt:false service autostarted without a host reboot: %q", result)
	}
}

func TestNewManager_AdoptFalseDoesNotAutostartWhenBootIDUnknown(t *testing.T) {
	for name, c := range map[string]struct{ now, saved string }{
		"saved state has none": {"boot-B", ""},
		"current unavailable":  {"", "boot-A"},
	} {
		t.Run(name, func(t *testing.T) {
			withBootID(t, c.now)
			m := adoptFalseAutostartManager(t, c.saved)
			time.Sleep(300 * time.Millisecond)
			if result := m.Do("testsvc", "status"); !strings.Contains(result, "stopped") {
				t.Fatalf("autostarted with an unknown boot ID: %q", result)
			}
		})
	}
}

func TestManager_SaveRecordsBootID(t *testing.T) {
	withBootID(t, "boot-X")
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.Do("svc", "start")
	s, raw, err := LoadState(cfg.StateDir, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID != "boot-X" {
		t.Fatalf("saved boot id: got %q", s.BootID)
	}
	if strings.Count(string(raw), `"boot_id":"boot-X"`) != 2 { // state and process record
		t.Fatalf("process record should carry the boot id: %s", raw)
	}
}

func TestBootID_RealValueIsNonEmpty(t *testing.T) {
	if id := bootID(); id == "" {
		t.Fatal("bootID() returned empty on a supported platform")
	}
}

func TestRebootedSince(t *testing.T) {
	for _, c := range []struct {
		name       string
		now, saved string
		want       bool
	}{
		{"same", "a", "a", false},
		{"different", "b", "a", true},
		{"saved empty", "b", "", false},
		{"current empty", "", "a", false},
	} {
		m := &Manager{bootID: c.now}
		if got := m.rebootedSince("svc", c.saved); got != c.want {
			t.Errorf("%s: rebootedSince = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- review follow-ups: start offset, deploy gating, replacement decisions ---

func TestManager_DeployWaitsForReadinessAfterOperatorOp(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	m, _, _, write := pendingStartupManager(t, nil)
	write("deploy:\n  - echo run >> " + runs + "\n")
	if err := m.Reload(); err != nil { // pick up the deploy steps (still pre-readiness)
		t.Fatal(err)
	}

	if got := m.Do("svc1", "stop"); got != "stopped" {
		t.Fatalf("stop before readiness: %q", got)
	}
	if err := m.RequestDeploy("svc1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(runs); err == nil {
		t.Fatal("a deploy ran before readiness (its notifications would be lost)")
	}
	m.SignalReady()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(runs); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("deploy never ran after readiness")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManager_ReloadDisablingAutostartBeforeReadinessDoesNotStart(t *testing.T) {
	m, _, svcPath, _ := pendingStartupManager(t, nil)
	// Same service, autostart now off.
	data := "dir: /tmp\nautostart: false\nentrypoint:\n  - sleep\n  - \"3600\"\n"
	if err := os.WriteFile(svcPath, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	m.SignalReady()
	time.Sleep(400 * time.Millisecond)
	if got := m.Do("svc1", "status"); got != "stopped" {
		t.Fatalf("the replacement started although the new config disables autostart: %q", got)
	}
}

func TestManager_ReplacementDeployWaitsForManagerReadiness(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	m, _, svcPath, _ := pendingStartupManager(t, nil)
	// Replace the service with one that has no startup op of its own.
	data := "dir: /tmp\nautostart: false\nentrypoint:\n  - sleep\n  - \"3600\"\ndeploy:\n  - echo run >> " + runs + "\n"
	if err := os.WriteFile(svcPath, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestDeploy("svc1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(runs); err == nil {
		t.Fatal("a replacement ran a deploy before the manager was ready")
	}
	m.SignalReady()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(runs); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("deploy never ran after readiness")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNewManager_LegacyStateGetsBootIDStamped(t *testing.T) {
	withBootID(t, "boot-A")
	cfg := testConfig(t)
	// A legacy state file: no boot_id.
	if err := SaveState(cfg.StateDir, "testsvc", State{
		Status: "running", DesiredStatus: "running",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	svc := autostartService("testsvc", t.TempDir(), &yes)
	svc.Adopt = &no

	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	m.SignalReady()
	time.Sleep(300 * time.Millisecond)
	if got := m.Do("testsvc", "status"); !strings.Contains(got, "stopped") {
		t.Fatalf("legacy state without a boot ID must not autostart now: %q", got)
	}
	m.Stop()
	s, _, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID != "boot-A" {
		t.Fatalf("startup should record the current boot ID, got %q", s.BootID)
	}

	// Next time the host has rebooted: the stamped state detects it.
	withBootID(t, "boot-B")
	m2, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Stop()
	m2.SignalReady()
	waitForStatus(t, m2, "testsvc", "running")
}

func TestManager_SaveStatePersistsLastRestart(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.Do("svc", "start")
	m.Do("svc", "restart")
	s, _, err := LoadState(cfg.StateDir, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if s.LastRestart.IsZero() {
		t.Fatal("LastRestart was not persisted")
	}
}

// --- boot ID bookkeeping ---

func TestNewManager_BootIDStampTouchesOnlyBootID(t *testing.T) {
	withBootID(t, "boot-A")
	cfg := testConfig(t)
	legacyBackend := json.RawMessage(`{"pid":99999999,"pgid":99999999,"log_path":"/x.log"}`)
	if err := SaveState(cfg.StateDir, "testsvc", State{
		Status: "failed", DesiredStatus: "running", FailedStep: "build", Backend: legacyBackend,
	}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := LoadState(cfg.StateDir, "testsvc")

	yes, no := true, false
	svc := autostartService("testsvc", t.TempDir(), &yes)
	svc.Adopt = &no
	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	after, _, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	if after.BootID != "boot-A" {
		t.Fatalf("boot_id: got %q", after.BootID)
	}
	if after.Status != "failed" || after.FailedStep != "build" || after.DesiredStatus != "running" {
		t.Fatalf("stamp changed other fields: %+v", after)
	}
	if string(after.Backend) != string(before.Backend) {
		t.Fatalf("stamp changed the backend record: %s -> %s", before.Backend, after.Backend)
	}
}

func TestManager_ReloadWhileStartupPendingKeepsRebootSignal(t *testing.T) {
	// The host rebooted (file says boot-A, now boot-B), so an adopt:false
	// autostart is pending. A reload and then a restart of mezzaops before
	// readiness must still see the reboot.
	withBootID(t, "boot-B")
	servicesDir := t.TempDir()
	svcPath := filepath.Join(servicesDir, "svc1.yaml")
	write := func(extra string) {
		data := "dir: /tmp\nadopt: false\nautostart: true\nentrypoint:\n  - sleep\n  - \"3600\"\n" + extra
		if err := os.WriteFile(svcPath, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	cfg := testConfig(t)
	cfg.ServicesDir = servicesDir
	if err := SaveState(cfg.StateDir, "svc1", State{
		Status: "running", DesiredStatus: "running", BootID: "boot-A",
		Backend: json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
	}); err != nil {
		t.Fatal(err)
	}
	services, _ := config.LoadServices(servicesDir)
	m, err := NewManager(cfg, services, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	write("branch: other\n")
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	m.Stop() // mezzaops restarts before readiness

	s, _, err := LoadState(cfg.StateDir, "svc1")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID != "boot-A" {
		t.Fatalf("an unrun startup op must not consume the reboot signal; file has %q", s.BootID)
	}
	services, _ = config.LoadServices(servicesDir)
	m2, err := NewManager(cfg, services, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Stop()
	m2.SignalReady()
	waitForStatus(t, m2, "svc1", "running")
}

func TestNewManager_ReplacingModeDoesNoStamping(t *testing.T) {
	withBootID(t, "boot-B")
	cfg := testConfig(t)
	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := SaveState(cfg.StateDir, "svc1", State{Status: "stopped", BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}

	ms := m.newManagedService(sleepService("svc1", t.TempDir()), false, modeReplacing)
	defer ms.cancel()
	s, _, err := LoadState(cfg.StateDir, "svc1")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID != "boot-A" {
		t.Fatalf("a replacement must not stamp the state, file has %q", s.BootID)
	}
	if ms.recordedBootID != "boot-A" {
		t.Fatalf("recordedBootID: got %q, want the loaded value", ms.recordedBootID)
	}
}

// --- exact, clock-independent process identity ---

// withStartID substitutes the process start ID lookup for the test.
func withStartID(t *testing.T, fn func(pid int) (int64, error)) {
	t.Helper()
	orig := processStartIDFn
	processStartIDFn = fn
	t.Cleanup(func() { processStartIDFn = orig })
}

func TestVerifyProcess_SameBootIdenticalStartIDIsVerifiedDespiteClockStep(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 123456, nil })
	// CreateTime is derived from the wall clock; here it is hours off, as
	// after a clock step on the same boot. The start ID is what counts.
	ps := processBackendState{PID: os.Getpid(), BootID: "boot-A", StartID: 123456, CreateTime: 1}
	if !verifiedWith(ps, hostRules(), "") {
		t.Fatal("same boot and identical start ID must verify regardless of CreateTime")
	}
}

func TestVerifyProcess_SameBootDifferentStartIDIsNotVerified(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 123457, nil })
	ct := int64(0)
	if p, err := process.NewProcess(int32(os.Getpid())); err == nil {
		ct, _ = p.CreateTime()
	}
	ps := processBackendState{PID: os.Getpid(), BootID: "boot-A", StartID: 123456, CreateTime: ct}
	if verifiedWith(ps, hostRules(), "") {
		t.Fatal("a different start ID is a different process, even if CreateTime matches")
	}
}

func TestVerifyProcess_LookupErrorsAreNotVerified(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 0, errors.New("no such process") })
	if verifiedWith(processBackendState{PID: os.Getpid(), BootID: "boot-A", StartID: 1}, hostRules(), "") {
		t.Error("a start ID lookup error must not verify")
	}
	// Legacy path: CreateTime recorded but the process cannot be looked up.
	if verifiedWith(processBackendState{PID: 99999999, CreateTime: 1234}, hostRules(), "") {
		t.Error("a CreateTime lookup error must not verify")
	}
}

func TestSaveBackendState_RecordsStartIDAndBootID(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 424242, nil })
	child := exec.Command("sleep", "3600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	var ps processBackendState
	if err := json.Unmarshal(saveBackendStateFromFields(child.Process.Pid, child.Process.Pid, "", hostRules()), &ps); err != nil {
		t.Fatal(err)
	}
	wantStart := int64(424242)
	if hostRules().startIDIsCreateTime {
		wantStart = ps.CreateTime // no second lookup where the start ID is CreateTime
	}
	if ps.StartID != wantStart || ps.BootID != "boot-A" {
		t.Fatalf("record: %+v", ps)
	}
}

func TestParseStartTicks(t *testing.T) {
	// Field 22 counts from after the last ')' because comm may contain
	// spaces and parentheses.
	const tail = "S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 12345 67 18446744073709551615"
	for name, comm := range map[string]string{
		"plain":       "sleep",
		"spaces":      "my svc",
		"parentheses": "a) (b",
		"nested":      "x (y) z)",
	} {
		stat := "4242 (" + comm + ") " + tail
		got, err := parseStartTicks(stat)
		if err != nil || got != 987654 {
			t.Errorf("%s: parseStartTicks = %d, %v; want 987654", name, got, err)
		}
	}
	for _, bad := range []string{"", "4242 (sleep", "4242 (sleep) S 1 2"} {
		if _, err := parseStartTicks(bad); err == nil {
			t.Errorf("parseStartTicks(%q) should fail", bad)
		}
	}
}

func TestStampBootID_NullFileDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svc.json"), []byte("null"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := StampBootID(dir, "svc", "boot-A"); err != nil {
		t.Fatal(err)
	}
	s, _, err := LoadState(dir, "svc")
	if err != nil || s.BootID != "boot-A" {
		t.Fatalf("after stamping a null file: %+v, %v", s, err)
	}
}

func TestSaveState_RecordsCurrentBootIDWhenBackendRunning(t *testing.T) {
	// A replacement that inherited an old boot ID, then a deploy that
	// starts the process: the saved boot_id must be the current one.
	withBootID(t, "boot-B")
	cfg := testConfig(t)
	m, err := NewManager(cfg, nil, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := SaveState(cfg.StateDir, "svc1", State{Status: "stopped", BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}
	svc := sleepService("svc1", t.TempDir())
	svc.Deploy = []string{"echo deploying"}
	ms := m.newManagedService(svc, false, modeReplacing)
	m.mu.Lock()
	m.services["svc1"] = ms
	m.mu.Unlock()
	m.startServiceLoop(ms)
	m.SignalReady()

	if err := m.RequestDeploy("svc1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st := m.GetAllStates()["svc1"]; st.LastResult == "success" && st.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deploy did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitForPersistedBootID(t, cfg.StateDir, "svc1", "boot-B")
}

func waitForPersistedBootID(t *testing.T, dir, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _, err := LoadState(dir, name)
		if err == nil && s.BootID == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted boot_id for %s: got %q, want %q", name, s.BootID, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- review follow-ups: legacy tolerance, three-state verification ---

func TestLegacyCreateTimeTolerance_Values(t *testing.T) {
	if runtime.GOOS == "linux" && hostRules().createTimeTolerance != 1000 {
		t.Errorf("Linux tolerance: got %d, want 1000", hostRules().createTimeTolerance)
	}
	if runtime.GOOS == "darwin" && hostRules().createTimeTolerance != 0 {
		t.Errorf("darwin must compare CreateTime exactly, got tolerance %d", hostRules().createTimeTolerance)
	}
}

func TestSameBoot(t *testing.T) {
	const boot = int64(1_700_000_000)
	for _, d := range []int64{-1, 0, 1, bootTimeTolerance, -bootTimeTolerance} {
		if !sameBoot(boot, boot+d) {
			t.Errorf("boot times %d apart are the same boot", d)
		}
	}
	for _, d := range []int64{bootTimeTolerance + 1, -bootTimeTolerance - 1, 3600, -86400} {
		if sameBoot(boot, boot+d) {
			t.Errorf("boot times %d apart are different boots", d)
		}
	}
}

func TestVerifyProcess_ThreeStates(t *testing.T) {
	withBootID(t, "boot-A")
	pid := os.Getpid()

	withStartID(t, func(int) (int64, error) { return 7, nil })
	if got, _ := verifyProcess(processBackendState{PID: pid, BootID: "boot-A", StartID: 7}, hostRules(), ""); got != verifyVerified {
		t.Errorf("identical start ID: got %v, want verified", got)
	}
	if got, _ := verifyProcess(processBackendState{PID: pid, BootID: "boot-A", StartID: 8}, hostRules(), ""); got != verifyMismatch {
		t.Errorf("different start ID: got %v, want mismatch", got)
	}
	if got, _ := verifyProcess(processBackendState{PID: pid, BootID: "boot-OLD", StartID: 7}, hostRules(), ""); got != verifyMismatch {
		t.Errorf("different boot: got %v, want mismatch", got)
	}
	// The pid is alive but the lookup failed: unknown, not a mismatch.
	withStartID(t, func(int) (int64, error) { return 0, errors.New("permission denied") })
	if got, _ := verifyProcess(processBackendState{PID: pid, BootID: "boot-A", StartID: 7}, hostRules(), ""); got != verifyUnknown {
		t.Errorf("lookup failure: got %v, want unknown", got)
	}
	// A gone-looking error on a live pid (hidepid, pid namespaces) is unknown.
	withStartID(t, func(int) (int64, error) { return 0, fs.ErrNotExist })
	if got, _ := verifyProcess(processBackendState{PID: pid, BootID: "boot-A", StartID: 7}, hostRules(), ""); got != verifyUnknown {
		t.Errorf("ENOENT on a live pid: got %v, want unknown", got)
	}
	// No identity data at all: accepted, as on main.
	if got, _ := verifyProcess(processBackendState{PID: pid}, hostRules(), ""); got != verifyVerified {
		t.Errorf("no identity data: got %v, want verified", got)
	}
}

func TestSaveBackendState_StartIDReusesCreateTimeWhereItIsTheStartID(t *testing.T) {
	var calls atomic.Int32
	withStartID(t, func(int) (int64, error) { calls.Add(1); return 424242, nil })
	child := exec.Command("sleep", "3600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	var ps processBackendState
	if err := json.Unmarshal(saveBackendStateFromFields(child.Process.Pid, child.Process.Pid, "", hostRules()), &ps); err != nil {
		t.Fatal(err)
	}
	if hostRules().startIDIsCreateTime {
		if calls.Load() != 0 || ps.StartID != ps.CreateTime {
			t.Fatalf("start ID should reuse CreateTime without a second lookup: calls=%d %+v", calls.Load(), ps)
		}
	} else if ps.StartID != 424242 {
		t.Fatalf("start ID: %+v", ps)
	}
}

func TestSaveState_SkipsStatusLookupForNonProcessBackendWhileDeploying(t *testing.T) {
	var calls atomic.Int32
	fake := &countingStatusBackend{calls: &calls}
	m := &Manager{stateDir: t.TempDir(), ctx: context.Background(), bootID: "b"}
	ms := &managedService{config: config.ServiceConfig{Name: "svc"}, backend: fake}
	ms.state.Status = "deploying"
	m.saveServiceState(ms)
	if calls.Load() != 0 {
		t.Fatalf("Status queried %d times for a non-process backend while deploying", calls.Load())
	}
	ms.state.Status = "stopped"
	m.saveServiceState(ms)
	if calls.Load() != 1 {
		t.Fatalf("Status should be queried once for a normal save, got %d", calls.Load())
	}
}

type countingStatusBackend struct{ calls *atomic.Int32 }

func (c *countingStatusBackend) Start(context.Context) error   { return nil }
func (c *countingStatusBackend) Stop(context.Context) error    { return nil }
func (c *countingStatusBackend) Restart(context.Context) error { return nil }
func (c *countingStatusBackend) Status(context.Context) (string, error) {
	c.calls.Add(1)
	return "stopped", nil
}
func (c *countingStatusBackend) Logs(context.Context, int) (string, error) { return "", nil }
func (c *countingStatusBackend) SaveBackendState() json.RawMessage         { return nil }
func (c *countingStatusBackend) RestoreBackendState(json.RawMessage)       {}

// --- deterministic verification tests (all lookups driven through seams) ---

func withCreateTime(t *testing.T, fn func(pid int) (int64, error)) {
	t.Helper()
	orig := createTimeFn
	createTimeFn = fn
	t.Cleanup(func() { createTimeFn = orig })
}

func withBootTime(t *testing.T, fn func() (int64, error)) {
	t.Helper()
	orig := bootTimeFn
	bootTimeFn = fn
	t.Cleanup(func() { bootTimeFn = orig })
}

func withAlive(t *testing.T, alive bool) {
	t.Helper()
	fn := func(int) bool { return alive }
	isAliveOverride.Store(&fn)
	t.Cleanup(func() { isAliveOverride.Store(nil) })
}

func TestVerifyProcess_BootIDAndBootTimeViaSeams(t *testing.T) {
	withAlive(t, true)
	const bt = int64(1_700_000_000)
	withBootTime(t, func() (int64, error) { return bt, nil })
	withStartID(t, func(int) (int64, error) { return 9, nil })
	withBootID(t, "boot-A")
	pid := 1234
	// Matching IDs: same boot, even if the (jittery) boot time disagrees.
	if !verifiedWith(processBackendState{PID: pid, BootID: "boot-A", StartID: 9, BootTime: bt - 500}, hostRules(), "") {
		t.Error("matching boot ID and start ID must verify")
	}
	if verifiedWith(processBackendState{PID: pid, BootID: "boot-OLD", StartID: 9}, hostRules(), "") {
		t.Error("a different boot ID must not verify")
	}
	// An ID on only one side falls back to the boot-time check.
	withCreateTime(t, func(int) (int64, error) { return 5000, nil })
	if !verifiedWith(processBackendState{PID: pid, BootTime: bt + 1, CreateTime: 5000}, hostRules(), "") {
		t.Error("no recorded ID: boot time within tolerance must verify")
	}
	if verifiedWith(processBackendState{PID: pid, BootTime: bt - 500, CreateTime: 5000}, hostRules(), "") {
		t.Error("no recorded ID: a different boot time must not verify")
	}
	withBootID(t, "")
	if verifiedWith(processBackendState{PID: pid, BootID: "boot-A", StartID: 9, BootTime: bt - 500, CreateTime: 5000}, hostRules(), "") {
		t.Error("no current ID: fall back to the boot time, which differs")
	}
}

func TestVerifyProcess_BootTimeErrorFallsThroughToCreateTime(t *testing.T) {
	withAlive(t, true)
	withBootID(t, "")
	withBootTime(t, func() (int64, error) { return 0, errors.New("no boot time") })
	withCreateTime(t, func(int) (int64, error) { return 5000, nil })
	rec := processBackendState{PID: 1234, BootTime: 1_700_000_000, CreateTime: 5000}
	if got, _ := verifyProcess(rec, hostRules(), ""); got != verifyVerified {
		t.Errorf("boot time unreadable, CreateTime matches: got %v, want verified", got)
	}
	rec.CreateTime = 9000
	if got, _ := verifyProcess(rec, rulesDarwin(), ""); got != verifyMismatch { // macOS rules: CreateTime is absolute
		t.Errorf("boot time unreadable, CreateTime differs: got %v, want mismatch", got)
	}
	if got, _ := verifyProcess(rec, rulesLinux(), ""); got != verifyUnknown { // Linux rules
		t.Errorf("boot time unreadable, CreateTime differs (Linux): got %v, want unknown", got)
	}
}

// --- unverified (unknown identity) records, driven through seams ---

const unknownPID = 424242

// unknownIdentityManager returns a manager for an autostart service whose
// saved record names a live pid whose identity cannot be looked up.
func unknownIdentityManager(t *testing.T, cfg *config.Config, svc config.ServiceConfig) *Manager {
	t.Helper()
	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	m.SignalReady()
	return m
}

func seedUnknownIdentity(t *testing.T) (*config.Config, config.ServiceConfig) {
	t.Helper()
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 0, errors.New("permission denied") })
	withAlive(t, true)
	cfg := testConfig(t)
	raw, _ := json.Marshal(processBackendState{PID: unknownPID, PGID: unknownPID, BootID: "boot-A", StartID: 5})
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: raw, BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	return cfg, autostartService("testsvc", t.TempDir(), &yes) // adopt defaults to true
}

func TestNewManager_UnknownIdentityIsNeitherAdoptedNorAutostarted(t *testing.T) {
	cfg, svc := seedUnknownIdentity(t)
	m := unknownIdentityManager(t, cfg, svc)
	defer m.Stop()

	m.mu.Lock()
	ms := m.services["testsvc"]
	m.mu.Unlock()
	// Nothing was scheduled at construction (before the loop could act on it).
	if ms.restartOnStartup || ms.autostartOnReady {
		t.Fatal("autostart was scheduled over a live process whose identity is unknown")
	}
	pb, _ := asProcessBackend(ms.backend)
	if pb.IsRunning() {
		t.Fatal("a process with unknown identity must not be adopted")
	}
}

func TestProcessBackend_TryAdoptReportsUnknownIdentity(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 0, errors.New("permission denied") })
	withAlive(t, true)
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", t.TempDir())
	b.adopt = true
	b.restoredState = &processBackendState{PID: unknownPID, PGID: unknownPID, BootID: "boot-A", StartID: 5}
	b.restoredStatus = "running"
	if outcome, msg := b.TryAdopt(""); outcome != adoptUnknown {
		t.Fatalf("outcome %v (%s), want adoptUnknown", outcome, msg)
	}
}

func TestVerifyProcess_LegacyBootTimeErrorNeedsCreateTime(t *testing.T) {
	withAlive(t, true)
	withBootID(t, "")
	withBootTime(t, func() (int64, error) { return 0, errors.New("no boot time") })
	withCreateTime(t, func(int) (int64, error) { return 5000, nil })
	// A record with only a boot time and an unreadable boot time has no
	// identity check at all: unknown, never verified.
	if got, _ := verifyProcess(processBackendState{PID: 1234, BootTime: 1_700_000_000}, hostRules(), ""); got != verifyUnknown {
		t.Errorf("boot time only, unreadable: got %v, want unknown", got)
	}
}

func TestCreateTimeMatches(t *testing.T) {
	for _, c := range []struct {
		live, rec, tol int64
		want           bool
	}{
		{5000, 5000, 0, true},
		{5000, 5001, 0, false},
		{5000, 4999, 0, false},
		{5000, 6000, 1000, true},
		{5000, 4000, 1000, true},
		{5000, 6001, 1000, false},
		{5000, 3999, 1000, false},
	} {
		if got := createTimeMatches(c.live, c.rec, c.tol); got != c.want {
			t.Errorf("createTimeMatches(%d, %d, tol %d) = %v, want %v", c.live, c.rec, c.tol, got, c.want)
		}
	}
	if got := int64(hostRules().createTimeTolerance); (runtime.GOOS == "linux" && got != 1000) || (runtime.GOOS == "darwin" && got != 0) {
		t.Errorf("platform tolerance: got %d on %s", got, runtime.GOOS)
	}
}

func TestVerifyProcess_BootIDMatchWithoutStartIDUsesCreateTime(t *testing.T) {
	withAlive(t, true)
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { t.Fatal("no start ID recorded: must not look one up"); return 0, nil })
	withCreateTime(t, func(int) (int64, error) { return 1_700_000_000_000, nil })
	// Far enough off to be a mismatch on every platform.
	if got, _ := verifyProcess(processBackendState{PID: 1234, BootID: "boot-A", CreateTime: 1_700_000_005_000}, rulesDarwin(), ""); got != verifyMismatch {
		t.Errorf("CreateTime mismatch (macOS rules): got %v, want mismatch", got)
	}
	if got, _ := verifyProcess(processBackendState{PID: 1234, BootID: "boot-A", CreateTime: 1_700_000_005_000}, rulesLinux(), ""); got != verifyUnknown {
		t.Errorf("CreateTime difference (Linux rules): got %v, want unknown", got)
	}
	if got, _ := verifyProcess(processBackendState{PID: 1234, BootID: "boot-A", CreateTime: 1_700_000_000_000}, hostRules(), ""); got != verifyVerified {
		t.Errorf("CreateTime match: got %v, want verified", got)
	}
}

func TestVerifyProcess_NoComparisonPossibleIsUnknown(t *testing.T) {
	withAlive(t, true)
	withStartID(t, func(int) (int64, error) {
		t.Fatal("must not look up a start ID without a current boot ID")
		return 0, nil
	})
	withBootID(t, "") // the current boot ID is unreadable
	// The record has identity fields (boot ID and start ID) but none of them
	// can be compared: unknown, not verified.
	if got, _ := verifyProcess(processBackendState{PID: 1234, BootID: "boot-A", StartID: 7}, hostRules(), ""); got != verifyUnknown {
		t.Errorf("boot ID + start ID, no current boot ID, nothing else: got %v, want unknown", got)
	}
	// A record with no identity fields at all is accepted, as on main.
	if got, _ := verifyProcess(processBackendState{PID: 1234}, hostRules(), ""); got != verifyVerified {
		t.Errorf("no identity fields: got %v, want verified", got)
	}
}

func TestManager_FailedStopStillRecordsIntent(t *testing.T) {
	var fail atomic.Bool
	logDir := t.TempDir()
	factory := func(svc config.ServiceConfig) Backend {
		pb := NewProcessBackend(svc.Name, svc.Dir, svc.Entrypoint, svc.Process.Cmd, logDir)
		return failingStopBackend{ProcessBackend: pb, fail: &fail}
	}
	m, _, _ := reloadFixtureWith(t, "", factory, NopNotifier{})
	m.Do("svc1", "start")
	waitForStatus(t, m, "svc1", "running")
	t.Cleanup(func() { fail.Store(false) }) // let the fixture's cleanup stop the process
	fail.Store(true)
	if got := m.Do("svc1", "stop"); !strings.Contains(got, "stop failed") {
		t.Fatalf("expected a failed stop, got %q", got)
	}
	if d := m.GetAllStates()["svc1"].DesiredStatus; d != "stopped" {
		t.Fatalf("a failed stop still records the operator's intent: DesiredStatus %q", d)
	}
}

func TestVerifyProcess_Algorithm(t *testing.T) {
	const live = int64(1_700_000_000_000)
	const bt = int64(1_700_000_000)
	errLookup := errors.New("lookup failed")
	cases := []struct {
		name       string
		rec        processBackendState
		curBootID  string
		alive      bool
		startID    func(int) (int64, error)
		createTime func(int) (int64, error)
		rules      platformRules
		want       verifyResult
	}{
		{"not alive", processBackendState{PID: 10, CreateTime: live}, "", false, nil, nil, rulesLinux(), verifyMismatch},
		{"non-positive pid", processBackendState{PID: 0, CreateTime: live}, "", true, nil, nil, rulesLinux(), verifyMismatch},
		{"no identity fields", processBackendState{PID: 10}, "boot-A", true, nil, nil, rulesLinux(), verifyVerified},
		{"boot ID differs", processBackendState{PID: 10, BootID: "boot-OLD", StartID: 7}, "boot-A", true, nil, nil, rulesLinux(), verifyMismatch},
		{"boot ID matches only", processBackendState{PID: 10, BootID: "boot-A"}, "boot-A", true, nil, nil, rulesLinux(), verifyUnknown},
		{"start ID equal", processBackendState{PID: 10, BootID: "boot-A", StartID: 7}, "boot-A", true,
			func(int) (int64, error) { return 7, nil }, nil, rulesLinux(), verifyVerified},
		{"start ID differs", processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 8, nil }, func(int) (int64, error) { return live, nil }, rulesLinux(), verifyMismatch},
		{"start ID lookup fails, nothing else", processBackendState{PID: 10, BootID: "boot-A", StartID: 7}, "boot-A", true,
			func(int) (int64, error) { return 0, errLookup }, nil, rulesLinux(), verifyUnknown},
		{"start ID lookup says ENOENT but the pid is alive (hidepid, pid namespace): CreateTime decides",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 0, fs.ErrNotExist }, func(int) (int64, error) { return live, nil }, rulesLinux(), verifyVerified},
		{"CreateTime differs (macOS: absolute, so a mismatch)", processBackendState{PID: 10, CreateTime: live + 5000}, "", true, nil,
			func(int) (int64, error) { return live, nil }, rulesDarwin(), verifyMismatch},
		{"CreateTime differs (Linux: moves with the clock, so unknown)", processBackendState{PID: 10, CreateTime: live + 5000}, "", true, nil,
			func(int) (int64, error) { return live, nil }, rulesLinux(), verifyUnknown},
		{"legacy record, boot IDs equal, no start ID, CreateTime differs: Linux unknown",
			processBackendState{PID: 10, BootID: "boot-A", CreateTime: live + 5000}, "boot-A", true, nil,
			func(int) (int64, error) { return live, nil }, rulesLinux(), verifyUnknown},
		{"legacy record, boot IDs equal, no start ID, CreateTime differs: macOS mismatch",
			processBackendState{PID: 10, BootID: "boot-A", CreateTime: live + 5000}, "boot-A", true, nil,
			func(int) (int64, error) { return live, nil }, rulesDarwin(), verifyMismatch},
		{"darwin: start ID lookup says ENOENT but the pid is alive: unknown",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 0, fs.ErrNotExist }, nil, rulesDarwin(), verifyUnknown},
		{"boot time differs, no boot ID: Linux unknown (BootTime moves with the clock)",
			processBackendState{PID: 10, BootTime: bt - 500, CreateTime: live}, "", true, nil, nil, rulesLinux(), verifyUnknown},
		{"boot time differs, no boot ID: darwin mismatch",
			processBackendState{PID: 10, BootTime: bt - 500, CreateTime: live}, "", true, nil, nil, rulesDarwin(), verifyMismatch},
		{"boot ID unreadable, start ID + CreateTime match: verified",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "", true, nil,
			func(int) (int64, error) { return live, nil }, rulesLinux(), verifyVerified},
		{"boot ID unreadable, start ID + CreateTime differs: Linux unknown",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "", true, nil,
			func(int) (int64, error) { return live + 5000, nil }, rulesLinux(), verifyUnknown},
		{"boot ID unreadable, start ID + CreateTime differs: macOS mismatch",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "", true, nil,
			func(int) (int64, error) { return live + 5000, nil }, rulesDarwin(), verifyMismatch},
		{"CreateTime lookup fails, alive", processBackendState{PID: 10, CreateTime: live}, "", true, nil,
			func(int) (int64, error) { return 0, errLookup }, rulesLinux(), verifyUnknown},
		{"start ID lookup fails, CreateTime differs: unknown, not mismatch (Linux CreateTime moves with the clock)",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 0, errLookup }, func(int) (int64, error) { return live + 3_600_000, nil }, rulesLinux(), verifyUnknown},
		{"start ID lookup fails, CreateTime matches (Linux fallback verifies)",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 0, errLookup }, func(int) (int64, error) { return live, nil }, rulesLinux(), verifyVerified},
		{"darwin: start ID lookup fails is unknown without a CreateTime call",
			processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: live}, "boot-A", true,
			func(int) (int64, error) { return 0, errLookup }, nil, rulesDarwin(), verifyUnknown},
		{"legacy: boot time matches, no CreateTime",
			processBackendState{PID: 10, BootTime: bt}, "", true, nil, nil, rulesLinux(), verifyUnknown},
		{"boot ID unreadable, start ID only", processBackendState{PID: 10, BootID: "boot-A", StartID: 7}, "", true, nil, nil, rulesLinux(), verifyUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBootID(t, c.curBootID)
			withBootTime(t, func() (int64, error) { return bt, nil })
			withAlive(t, c.alive)
			if c.startID != nil {
				withStartID(t, c.startID)
			} else {
				withStartID(t, func(int) (int64, error) { t.Error("unexpected start ID lookup"); return 0, nil })
			}
			if c.createTime != nil {
				withCreateTime(t, c.createTime)
			} else {
				withCreateTime(t, func(int) (int64, error) { t.Error("unexpected CreateTime lookup"); return 0, nil })
			}
			if got, _ := verifyProcess(c.rec, c.rules, ""); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// aliveOnce makes isAliveFn report true for the first call and false after,
// as when a process exits in the middle of a check.
// Only calls for pid are counted, so other goroutines (pollers for other
// tests' processes) cannot consume the single "true".
func aliveOnce(t *testing.T, pid int) {
	t.Helper()
	var calls atomic.Int32
	fn := func(p int) bool {
		if p != pid {
			return IsAlive(p)
		}
		return calls.Add(1) == 1
	}
	isAliveOverride.Store(&fn)
	t.Cleanup(func() { isAliveOverride.Store(nil) })
}

func TestVerifyProcess_ProcessExitingMidCheckIsMismatch(t *testing.T) {
	withBootID(t, "boot-A")
	lookupErr := errors.New("lookup failed")

	t.Run("start ID path", func(t *testing.T) {
		aliveOnce(t, 10)
		withStartID(t, func(int) (int64, error) { return 0, lookupErr })
		withCreateTime(t, func(int) (int64, error) { t.Error("must not fall through once the process is gone"); return 0, nil })
		if got, _ := verifyProcess(processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: 1}, rulesLinux(), ""); got != verifyMismatch {
			t.Errorf("got %v, want mismatch", got)
		}
	})
	t.Run("CreateTime path", func(t *testing.T) {
		aliveOnce(t, 10)
		withCreateTime(t, func(int) (int64, error) { return 0, lookupErr })
		if got, _ := verifyProcess(processBackendState{PID: 10, CreateTime: 1}, rulesLinux(), ""); got != verifyMismatch {
			t.Errorf("got %v, want mismatch", got)
		}
	})
	t.Run("still alive means unknown", func(t *testing.T) {
		withAlive(t, true)
		withCreateTime(t, func(int) (int64, error) { return 0, lookupErr })
		if got, _ := verifyProcess(processBackendState{PID: 10, CreateTime: 1}, rulesLinux(), ""); got != verifyUnknown {
			t.Errorf("got %v, want unknown", got)
		}
	})
}

func TestVerifyProcess_MismatchReasons(t *testing.T) {
	const bt = int64(1_700_000_000)
	withBootID(t, "boot-A")
	withBootTime(t, func() (int64, error) { return bt, nil })
	lookupErr := errors.New("lookup failed")

	reason := func(name string, ps processBackendState, rules platformRules, want mismatchReason) {
		t.Helper()
		got, r := verifyProcess(ps, rules, "")
		if got != verifyMismatch || r != want {
			t.Errorf("%s: got %v/%v, want mismatch/%v", name, got, r, want)
		}
	}

	withAlive(t, true)
	reason("non-positive pid", processBackendState{PID: 0, CreateTime: 1}, rulesLinux(), reasonBadPID)
	reason("different boot (boot ID)", processBackendState{PID: 10, BootID: "boot-OLD", StartID: 7}, rulesLinux(), reasonDifferentBoot)
	reason("different boot (boot time, macOS)", processBackendState{PID: 10, BootTime: bt - 500, CreateTime: 1}, rulesDarwin(), reasonDifferentBoot)
	withStartID(t, func(int) (int64, error) { return 8, nil })
	reason("different process (start ID)", processBackendState{PID: 10, BootID: "boot-A", StartID: 7}, rulesLinux(), reasonDifferentProcess)
	withCreateTime(t, func(int) (int64, error) { return 9_000, nil })
	reason("different process (CreateTime, macOS)", processBackendState{PID: 10, CreateTime: 1_000}, rulesDarwin(), reasonDifferentProcess)

	withAlive(t, false)
	reason("dead pid", processBackendState{PID: 10, CreateTime: 1}, rulesLinux(), reasonDead)

	// Gone after a failed lookup: alive for the first check, then not.
	aliveOnce(t, 10)
	withStartID(t, func(int) (int64, error) { return 0, lookupErr })
	reason("gone after a failed start ID lookup", processBackendState{PID: 10, BootID: "boot-A", StartID: 7, CreateTime: 1}, rulesLinux(), reasonDead)
	aliveOnce(t, 10)
	withCreateTime(t, func(int) (int64, error) { return 0, lookupErr })
	reason("gone after a failed CreateTime lookup", processBackendState{PID: 10, CreateTime: 1}, rulesLinux(), reasonDead)
}

func TestVerifyProcess_GoneLookingErrorOnLivePidIsUnknown(t *testing.T) {
	withBootID(t, "boot-A")
	withAlive(t, true)
	withStartID(t, func(int) (int64, error) { return 0, fs.ErrNotExist })
	withCreateTime(t, func(int) (int64, error) { return 0, fs.ErrNotExist })
	for _, rules := range []platformRules{rulesLinux(), rulesDarwin()} {
		if got, _ := verifyProcess(processBackendState{PID: 10, BootID: "boot-A", StartID: 7}, rules, ""); got != verifyUnknown {
			t.Errorf("rules %v: ENOENT on a live pid (start ID): got %v, want unknown", rules, got)
		}
		if got, _ := verifyProcess(processBackendState{PID: 10, CreateTime: 1}, rules, ""); got != verifyUnknown {
			t.Errorf("rules %v: ENOENT on a live pid (CreateTime): got %v, want unknown", rules, got)
		}
	}
	// Not alive means dead.
	withAlive(t, false)
	if got, r := verifyProcess(processBackendState{PID: 10, CreateTime: 1}, rulesLinux(), ""); got != verifyMismatch || r != reasonDead {
		t.Errorf("dead pid: %v/%v", got, r)
	}
}

func TestTryAdopt_MessagePerMismatchReason(t *testing.T) {
	withBootID(t, "boot-A")
	withAlive(t, true)
	b := NewProcessBackend("test", t.TempDir(), []string{"sleep", "3600"}, "", t.TempDir())
	b.adopt = true
	b.restoredStatus = "running"
	b.restoredState = &processBackendState{PID: 10, BootID: "boot-OLD", StartID: 7}
	if outcome, msg := b.TryAdopt(""); outcome != adoptNo || !strings.Contains(msg, "previous boot") {
		t.Errorf("different boot: %v %q", outcome, msg)
	}
	withStartID(t, func(int) (int64, error) { return 8, nil })
	b.restoredState = &processBackendState{PID: 10, BootID: "boot-A", StartID: 7}
	if outcome, msg := b.TryAdopt(""); outcome != adoptNo || !strings.Contains(msg, "reused by different process") {
		t.Errorf("different process: %v %q", outcome, msg)
	}
}

func TestNewManager_AdoptedProcessIsRepersistedWithCurrentIdentity(t *testing.T) {
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	// A legacy record written by hand: no BootID, no StartID.
	ct, err := createTimeFn(child.Process.Pid)
	if err != nil {
		t.Skip(err)
	}
	bt, err := bootTimeFn()
	if err != nil {
		t.Skip(err)
	}
	legacy, _ := json.Marshal(processBackendState{PID: child.Process.Pid, PGID: child.Process.Pid, BootTime: bt, CreateTime: ct})
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: legacy}); err != nil {
		t.Fatal(err)
	}

	// The current build: a boot ID is available and start IDs resolve.
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 99, nil })
	svc := autostartService("testsvc", t.TempDir(), nil)
	m, err := NewManager(cfg, []config.ServiceConfig{svc}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Do("testsvc", "status"); got != "running" {
		t.Fatalf("the legacy record should be adopted: %q", got)
	}
	_, raw, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Backend processBackendState `json:"backend"`
		BootID  string              `json:"boot_id"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Backend.BootID != "boot-A" || saved.Backend.StartID == 0 || saved.BootID != "boot-A" {
		t.Fatalf("adoption should rewrite the record with boot_id and start_id: %+v / %q", saved.Backend, saved.BootID)
	}
}

func TestNewManager_AdoptedCurrentRecordIsNotRewritten(t *testing.T) {
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 99, nil })
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ct, err := createTimeFn(child.Process.Pid)
	if err != nil {
		t.Skip(err)
	}
	rec, _ := json.Marshal(processBackendState{PID: child.Process.Pid, PGID: child.Process.Pid, BootID: "boot-A", StartID: 99, CreateTime: ct})
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: rec, BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "testsvc.json")
	before, _ := os.ReadFile(path)
	// A distinctive mtime lets us detect any rewrite.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(cfg, []config.ServiceConfig{autostartService("testsvc", t.TempDir(), nil)}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Do("testsvc", "status"); got != "running" {
		t.Fatalf("the record should be adopted: %q", got)
	}
	after, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(after) != string(before) || !info.ModTime().Before(time.Now().Add(-30*time.Minute)) {
		t.Fatalf("a current record must not be rewritten on adoption\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestVerifyProcess_ServiceBootIDCanOnlyReject(t *testing.T) {
	const bt = int64(1_700_000_000)
	withBootID(t, "boot-B")
	withAlive(t, true)
	withBootTime(t, func() (int64, error) { return bt, nil })
	withCreateTime(t, func(int) (int64, error) { return 5000, nil })
	legacy := processBackendState{PID: 10, BootTime: bt, CreateTime: 5000} // no BootID

	for _, rules := range []platformRules{rulesLinux(), rulesDarwin()} {
		// Written under another boot: no process in it can be alive.
		if got, r := verifyProcess(legacy, rules, "boot-A"); got != verifyMismatch || r != reasonDifferentBoot {
			t.Errorf("service written under another boot: %v/%v, want mismatch/different boot", got, r)
		}
		// A matching file boot ID proves nothing about the record: the usual
		// checks still apply and decide.
		if got, _ := verifyProcess(legacy, rules, "boot-B"); got != verifyVerified {
			t.Errorf("matching file boot ID, matching BootTime and CreateTime: %v, want verified", got)
		}
		// ... including the BootTime rule, which it must not skip.
		moved := legacy
		moved.BootTime = bt - 500
		want := verifyUnknown // Linux: BootTime moves with the clock
		if rules == rulesDarwin() {
			want = verifyMismatch
		}
		if got, _ := verifyProcess(moved, rules, "boot-B"); got != want {
			t.Errorf("rules %+v: matching file boot ID but differing BootTime: %v, want %v", rules, got, want)
		}
	}
	// The file's boot ID is not used for the start ID check.
	withStartID(t, func(int) (int64, error) {
		t.Error("start ID must not be looked up without the record's own boot ID")
		return 0, nil
	})
	_, _ = verifyProcess(processBackendState{PID: 10, StartID: 7, CreateTime: 5000}, rulesLinux(), "boot-B")
}

func TestNewManager_LegacyRecordAfterRebootAutostartsOnLinux(t *testing.T) {
	// Real reboot: the state file says boot-A, now boot-B, and the recorded
	// pid happens to be alive (reused by something unrelated).
	withBootID(t, "boot-B")
	withAlive(t, true)
	cfg := testConfig(t)
	legacy, _ := json.Marshal(processBackendState{PID: unknownPID, PGID: unknownPID, BootTime: 1, CreateTime: 5000})
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: legacy, BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	svc := autostartService("testsvc", t.TempDir(), &yes)
	// The rules this test is about, whatever platform runs it.
	m, err := newManager(cfg, []config.ServiceConfig{svc}, NopNotifier{}, nil, rulesLinux())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.SignalReady()
	waitForStatus(t, m, "testsvc", "running")
}

// adoptWith adopts a real child under the current build, with the current host
// boot ID bootID and a record that mutate customises, and returns the state
// file's contents before and after startup.
func adoptWith(t *testing.T, bootID string, mutate func(rec *processBackendState)) (before, after []byte) {
	t.Helper()
	withBootID(t, bootID)
	withStartID(t, func(int) (int64, error) { return 99, nil })
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ct, err := createTimeFn(child.Process.Pid)
	if err != nil {
		t.Skip(err)
	}
	rec := processBackendState{PID: child.Process.Pid, PGID: child.Process.Pid, CreateTime: ct}
	mutate(&rec)
	raw, _ := json.Marshal(rec)
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: raw, BootID: "boot-A"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "testsvc.json")
	before, _ = os.ReadFile(path)
	m, err := NewManager(cfg, []config.ServiceConfig{autostartService("testsvc", t.TempDir(), nil)}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Do("testsvc", "status"); got != "running" {
		t.Fatalf("the record should be adopted: %q", got)
	}
	after, _ = os.ReadFile(path)
	return before, after
}

func TestAdoption_MissingStartIDAloneIsNotLegacy(t *testing.T) {
	// BootID present, StartID absent (the lookup may simply fail here): the
	// record is not rewritten.
	before, after := adoptWith(t, "boot-A", func(rec *processBackendState) { rec.BootID = "boot-A" })
	if string(before) != string(after) {
		t.Fatalf("a record with a boot ID must not be re-persisted\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestAdoption_LegacyRecordNotRewrittenWithoutAHostBootID(t *testing.T) {
	// Re-persisting cannot add a boot ID that is unavailable.
	before, after := adoptWith(t, "", func(rec *processBackendState) {})
	if string(before) != string(after) {
		t.Fatalf("nothing to add: must not be re-persisted\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestAdoption_StampRunsWhenTheRepersistDoesNotRecordTheBootID(t *testing.T) {
	// A legacy record is adopted, but by the time the re-persist saves, the
	// process no longer looks running (it exits right after adoption), so the
	// save does not record the boot ID. The stamp must still run.
	withBootID(t, "boot-A")
	withStartID(t, func(int) (int64, error) { return 99, nil })
	cfg := testConfig(t)
	child := exec.Command("sleep", "3600")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ct, err := createTimeFn(child.Process.Pid)
	if err != nil {
		t.Skip(err)
	}
	bt, err := bootTimeFn()
	if err != nil {
		t.Skip(err)
	}
	legacy, _ := json.Marshal(processBackendState{PID: child.Process.Pid, PGID: child.Process.Pid, BootTime: bt, CreateTime: ct})
	if err := SaveState(cfg.StateDir, "testsvc", State{Status: "running", DesiredStatus: "running", Backend: legacy}); err != nil {
		t.Fatal(err)
	}

	// Alive for the adoption check, then not: Status reports stopped at save.
	aliveOnce(t, child.Process.Pid)
	m, err := NewManager(cfg, []config.ServiceConfig{autostartService("testsvc", t.TempDir(), nil)}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	s, _, err := LoadState(cfg.StateDir, "testsvc")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID != "boot-A" {
		t.Fatalf("the boot ID stamp must run when the re-persist did not record it: boot_id %q", s.BootID)
	}
}
