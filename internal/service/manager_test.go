package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shishberg/mezzaops/internal/config"
	"github.com/stretchr/testify/assert"
)

// testConfig builds a Config suitable for testing with temporary directories.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
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
	m, err := newManager(cfg, services, notifier, factory)
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
	if !snap.wasRunning {
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

// --- adopt: false autostart is gated on a host reboot ---

func currentBootTime(t *testing.T) int64 {
	t.Helper()
	bt, err := host.BootTime()
	if err != nil {
		t.Skipf("boot time unavailable: %v", err)
	}
	return int64(bt)
}

// adoptFalseAutostartManager starts a manager for an adopt:false, autostart
// service whose saved state (dead pid, desired running) has the given boot time.
func adoptFalseAutostartManager(t *testing.T, savedBoot int64) *Manager {
	t.Helper()
	cfg := testConfig(t)
	if err := SaveState(cfg.StateDir, "testsvc", State{
		Status:        "running",
		DesiredStatus: "running",
		Backend:       json.RawMessage(`{"pid":99999999,"pgid":99999999}`),
		BootTime:      savedBoot,
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
	m := adoptFalseAutostartManager(t, currentBootTime(t)-100)
	waitForStatus(t, m, "testsvc", "running")
}

func TestNewManager_AdoptFalseDoesNotAutostartWithoutReboot(t *testing.T) {
	m := adoptFalseAutostartManager(t, currentBootTime(t))
	time.Sleep(300 * time.Millisecond)
	if result := m.Do("testsvc", "status"); !strings.Contains(result, "stopped") {
		t.Fatalf("adopt:false service autostarted without a host reboot: %q", result)
	}
}

func TestNewManager_AdoptFalseDoesNotAutostartWithoutRecordedBootTime(t *testing.T) {
	m := adoptFalseAutostartManager(t, 0)
	time.Sleep(300 * time.Millisecond)
	if result := m.Do("testsvc", "status"); !strings.Contains(result, "stopped") {
		t.Fatalf("adopt:false service autostarted with no recorded boot time: %q", result)
	}
}

func TestManager_SaveRecordsBootTime(t *testing.T) {
	cfg := testConfig(t)
	m, err := NewManager(cfg, []config.ServiceConfig{sleepService("svc", t.TempDir())}, NopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	m.Do("svc", "start")
	s, _, err := LoadState(cfg.StateDir, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if s.BootTime != currentBootTime(t) {
		t.Fatalf("saved boot time: got %d, want %d", s.BootTime, currentBootTime(t))
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
