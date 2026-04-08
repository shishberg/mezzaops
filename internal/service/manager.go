package service

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/shishberg/mezzaops/internal/deploy"
)

// syncOp is a synchronous operation request sent to a service's goroutine.
type syncOp struct {
	op     string
	result chan string
}

// ServiceState holds the current state of a managed service.
type ServiceState struct {
	Status     string    `json:"status"`
	LastDeploy time.Time `json:"last_deploy,omitempty"`
	LastResult string    `json:"last_result,omitempty"`
	LastOutput string    `json:"last_output,omitempty"`
	FailedStep string    `json:"failed_step,omitempty"`
}

// managedService wraps a backend with its config, event loop, and deploy queue.
type managedService struct {
	config   config.ServiceConfig
	backend  Backend
	opCh     chan syncOp   // synchronous ops: start/stop/restart/status/logs/pull
	deployCh chan struct{} // async deploy trigger (capacity 1, latest-wins)

	// state is only accessed from the service loop goroutine (no lock needed).
	state ServiceState

	// stateMu protects reads of state from outside the loop (GetAllStates, etc.)
	stateMu sync.Mutex
}

// Manager manages a set of services.
type Manager struct {
	services map[string]*managedService
	notifier Notifier
	onChange func(name, event string) // for Discord presence updates
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Config for creating new services
	logDir   string
	stateDir string
	adopt    bool

	// For reload
	servicesDir string
}

// NewManager creates a Manager for the given service configs.
func NewManager(cfg *config.Config, services []config.ServiceConfig, notifier Notifier) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		services:    make(map[string]*managedService, len(services)),
		notifier:    notifier,
		ctx:         ctx,
		cancel:      cancel,
		logDir:      cfg.LogDir,
		stateDir:    cfg.StateDir,
		adopt:       cfg.Process.Adopt,
		servicesDir: cfg.ServicesDir,
	}

	for _, svc := range services {
		ms := m.newManagedService(svc)
		m.services[svc.Name] = ms
		m.startServiceLoop(ms)
	}

	// Clean up orphan state files
	m.cleanOrphans()

	return m, nil
}

// newManagedService creates a managedService with the appropriate backend.
func (m *Manager) newManagedService(svc config.ServiceConfig) *managedService {
	backend := m.backendForConfig(svc)

	ms := &managedService{
		config:   svc,
		backend:  backend,
		opCh:     make(chan syncOp, 10),
		deployCh: make(chan struct{}, 1),
	}

	// Try to adopt existing processes for ProcessBackend
	if pb, ok := backend.(*ProcessBackend); ok && m.adopt {
		pb.TryAdopt()
	}

	return ms
}

// backendForConfig selects the appropriate backend based on the service config.
func (m *Manager) backendForConfig(svc config.ServiceConfig) Backend {
	if len(svc.Entrypoint) > 0 || svc.Process.Cmd != "" {
		return NewProcessBackend(
			svc.Name, svc.Dir,
			svc.Entrypoint, svc.Process.Cmd,
			m.logDir, m.stateDir, m.adopt,
		)
	}
	if svc.ServiceName != "" {
		if runtime.GOOS == "darwin" {
			return NewLaunchctlBackend(svc.ServiceName)
		}
		return NewSystemctlBackend(svc.ServiceName, svc.UserService)
	}
	// Fallback: no-op process backend
	return NewProcessBackend(
		svc.Name, svc.Dir,
		nil, "echo 'no backend configured'",
		m.logDir, m.stateDir, false,
	)
}

// startServiceLoop launches the goroutine for a managed service.
func (m *Manager) startServiceLoop(ms *managedService) {
	m.wg.Add(1)
	go m.serviceLoop(ms)
}

// serviceLoop is the per-service event loop. All operations on a service are
// serialized through this goroutine.
func (m *Manager) serviceLoop(ms *managedService) {
	defer m.wg.Done()

	// Get the process exit channel if this is a ProcessBackend
	var exitCh <-chan struct{}
	if pb, ok := ms.backend.(*ProcessBackend); ok {
		exitCh = pb.WaitForExit()
	}

	for {
		select {
		case op := <-ms.opCh:
			result := m.handleOp(ms, op.op)
			op.result <- result

			// Refresh exit channel after start/restart (new process, new done chan)
			if pb, ok := ms.backend.(*ProcessBackend); ok {
				exitCh = pb.WaitForExit()
			}

		case <-ms.deployCh:
			m.executeDeploy(ms)

			// Refresh exit channel after deploy (may have restarted)
			if pb, ok := ms.backend.(*ProcessBackend); ok {
				exitCh = pb.WaitForExit()
			}

		case <-exitCh:
			// Process exited unexpectedly
			m.notifyEvent(ms.config.Name, "exited")
			// Reset the exit channel: get a fresh one (will be closed immediately
			// since process is dead, so set to nil to avoid busy-looping)
			exitCh = nil

		case <-m.ctx.Done():
			return
		}
	}
}

// handleOp executes a synchronous operation within the service loop.
func (m *Manager) handleOp(ms *managedService, op string) string {
	ctx := m.ctx
	switch op {
	case "start":
		if err := ms.backend.Start(ctx); err != nil {
			return fmt.Sprintf("start failed: %v", err)
		}
		m.notifyEvent(ms.config.Name, "started")
		return "started"

	case "stop":
		if err := ms.backend.Stop(ctx); err != nil {
			return fmt.Sprintf("stop failed: %v", err)
		}
		m.notifyEvent(ms.config.Name, "stopped")
		return "stopped"

	case "restart":
		if err := ms.backend.Restart(ctx); err != nil {
			return fmt.Sprintf("restart failed: %v", err)
		}
		m.notifyEvent(ms.config.Name, "restarted")
		return "restarted"

	case "status":
		status, err := ms.backend.Status(ctx)
		if err != nil {
			return fmt.Sprintf("status error: %v", err)
		}
		return status

	case "logs":
		logs, err := ms.backend.Logs(ctx, 1500)
		if err != nil {
			return fmt.Sprintf("logs error: %v", err)
		}
		if logs == "" {
			return "no logs"
		}
		return logs

	case "pull":
		return m.gitPull(ms)

	default:
		return fmt.Sprintf("unknown command: %s", op)
	}
}

// gitPull runs `git pull` in the service's working directory.
func (m *Manager) gitPull(ms *managedService) string {
	cmd := exec.Command("git", "pull")
	cmd.Dir = ms.config.Dir

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		buf.WriteString(err.Error())
	}

	out := buf.String()
	if out == "" {
		return "git pull: no output"
	}
	return out
}

// executeDeploy runs the deploy pipeline for a service.
func (m *Manager) executeDeploy(ms *managedService) {
	name := ms.config.Name
	steps := ms.config.Deploy

	if len(steps) == 0 {
		return
	}

	ms.stateMu.Lock()
	ms.state.Status = "deploying"
	ms.state.LastDeploy = time.Now()
	ms.stateMu.Unlock()

	m.notifier.DeployStarted(name)

	result, err := deploy.RunSteps(m.ctx, steps, ms.config.Dir)
	if err != nil || result.Status != "success" {
		failedStep := ""
		output := ""
		if result != nil {
			failedStep = result.FailedStep
			output = result.Output
		}

		ms.stateMu.Lock()
		ms.state.Status = "failed"
		ms.state.LastResult = "failed"
		ms.state.LastOutput = output
		ms.state.FailedStep = failedStep
		ms.stateMu.Unlock()

		m.notifier.DeployFailed(name, failedStep, output)
		return
	}

	// Deploy succeeded, restart the service
	if restartErr := ms.backend.Restart(m.ctx); restartErr != nil {
		ms.stateMu.Lock()
		ms.state.Status = "failed"
		ms.state.LastResult = "failed"
		ms.state.LastOutput = result.Output
		ms.state.FailedStep = "restart"
		ms.stateMu.Unlock()

		m.notifier.DeployFailed(name, "restart", result.Output)
		return
	}

	ms.stateMu.Lock()
	ms.state.Status = "running"
	ms.state.LastResult = "success"
	ms.state.LastOutput = result.Output
	ms.state.FailedStep = ""
	ms.stateMu.Unlock()

	m.notifier.DeploySucceeded(name, result.Output)
	m.notifyEvent(name, "restarted")
}

// notifyEvent calls the notifier and onChange callback.
func (m *Manager) notifyEvent(name, event string) {
	m.notifier.ServiceEvent(name, event)

	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()

	if fn != nil {
		fn(name, event)
	}
}

// Do sends a synchronous operation to the named service and blocks for the result.
func (m *Manager) Do(name, op string) string {
	m.mu.Lock()
	ms, ok := m.services[name]
	m.mu.Unlock()

	if !ok {
		return fmt.Sprintf("service %q not found", name)
	}

	so := syncOp{
		op:     op,
		result: make(chan string, 1),
	}

	select {
	case ms.opCh <- so:
	case <-m.ctx.Done():
		return "manager shutting down"
	}

	select {
	case result := <-so.result:
		return result
	case <-m.ctx.Done():
		return "manager shutting down"
	}
}

// RequestDeploy queues a deploy request for the named service (latest-wins).
func (m *Manager) RequestDeploy(name string) error {
	m.mu.Lock()
	ms, ok := m.services[name]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("service %q not found", name)
	}

	// Mark as deploying synchronously so callers see a non-idle state immediately.
	ms.stateMu.Lock()
	ms.state.Status = "deploying"
	ms.stateMu.Unlock()

	// Drain any pending deploy request (latest-wins).
	select {
	case <-ms.deployCh:
	default:
	}

	// Non-blocking send.
	select {
	case ms.deployCh <- struct{}{}:
	default:
	}

	return nil
}

// StartAll starts all services.
func (m *Manager) StartAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		m.Do(name, "start")
	}
}

// StopAll stops all services.
func (m *Manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		m.Do(name, "stop")
	}
}

// Reload re-reads services from the services directory and adds/removes/updates
// services as needed.
func (m *Manager) Reload() error {
	if m.servicesDir == "" {
		return fmt.Errorf("no services_dir configured")
	}

	newConfigs, err := config.LoadServices(m.servicesDir)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	seen := make(map[string]bool)
	for _, svc := range newConfigs {
		seen[svc.Name] = true

		m.mu.Lock()
		existing, exists := m.services[svc.Name]
		m.mu.Unlock()

		if exists {
			// Check if config changed
			if !serviceConfigEqual(existing.config, svc) {
				m.Do(svc.Name, "stop")
				// Cancel the old loop by removing from map; it will exit on ctx.Done
				// when manager stops, but we need to replace it now.
				ms := m.newManagedService(svc)
				m.mu.Lock()
				m.services[svc.Name] = ms
				m.mu.Unlock()
				m.startServiceLoop(ms)
			}
			continue
		}

		// New service
		ms := m.newManagedService(svc)
		m.mu.Lock()
		m.services[svc.Name] = ms
		m.mu.Unlock()
		m.startServiceLoop(ms)
	}

	// Remove services no longer in config
	m.mu.Lock()
	toRemove := make([]string, 0)
	for name := range m.services {
		if !seen[name] {
			toRemove = append(toRemove, name)
		}
	}
	m.mu.Unlock()

	for _, name := range toRemove {
		m.Do(name, "stop")
		m.mu.Lock()
		delete(m.services, name)
		m.mu.Unlock()
	}

	m.cleanOrphans()

	return nil
}

// serviceConfigEqual compares two ServiceConfigs for equality.
func serviceConfigEqual(a, b config.ServiceConfig) bool {
	if a.Name != b.Name || a.Dir != b.Dir || a.Branch != b.Branch || a.Repo != b.Repo {
		return false
	}
	if a.ServiceName != b.ServiceName || a.UserService != b.UserService {
		return false
	}
	if a.Process.Cmd != b.Process.Cmd {
		return false
	}
	if len(a.Entrypoint) != len(b.Entrypoint) {
		return false
	}
	for i := range a.Entrypoint {
		if a.Entrypoint[i] != b.Entrypoint[i] {
			return false
		}
	}
	if len(a.Deploy) != len(b.Deploy) {
		return false
	}
	for i := range a.Deploy {
		if a.Deploy[i] != b.Deploy[i] {
			return false
		}
	}
	return true
}

// ServiceNames returns a sorted list of all service names.
func (m *Manager) ServiceNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CountRunning returns (running, total) service counts.
func (m *Manager) CountRunning() (int, int) {
	m.mu.Lock()
	services := make([]*managedService, 0, len(m.services))
	for _, ms := range m.services {
		services = append(services, ms)
	}
	m.mu.Unlock()

	running := 0
	for _, ms := range services {
		status, err := ms.backend.Status(m.ctx)
		if err == nil && status == "running" {
			running++
		}
	}
	return running, len(services)
}

// SetOnChange registers a callback invoked after service state transitions.
func (m *Manager) SetOnChange(fn func(name, event string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

// GetAllStates returns a snapshot of all service states.
func (m *Manager) GetAllStates() map[string]ServiceState {
	m.mu.Lock()
	services := make(map[string]*managedService, len(m.services))
	for name, ms := range m.services {
		services[name] = ms
	}
	m.mu.Unlock()

	result := make(map[string]ServiceState, len(services))
	for name, ms := range services {
		result[name] = m.liveState(ms)
	}
	return result
}

// liveState returns the service state with a live status probe.
// During "deploying", the cached status is preserved.
func (m *Manager) liveState(ms *managedService) ServiceState {
	ms.stateMu.Lock()
	s := ms.state
	ms.stateMu.Unlock()

	if s.Status == "deploying" {
		return s
	}

	if status, err := ms.backend.Status(m.ctx); err == nil {
		s.Status = status
	}
	return s
}

// FindServiceByRepo returns the service name matching the given repo and branch.
// repo may be "org/name" or "github.com/org/name"; both are matched.
func (m *Manager) FindServiceByRepo(repo, branch string) (string, bool) {
	normalizedRepo := strings.TrimPrefix(repo, "github.com/")

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ms := range m.services {
		cfgRepo := strings.TrimPrefix(ms.config.Repo, "github.com/")
		if cfgRepo == normalizedRepo && ms.config.Branch == branch {
			return ms.config.Name, true
		}
	}
	return "", false
}

// GetServiceConfig returns the config for a named service.
func (m *Manager) GetServiceConfig(name string) (config.ServiceConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ms, ok := m.services[name]
	if !ok {
		return config.ServiceConfig{}, false
	}
	return ms.config, true
}

// Stop cancels the manager context and waits for all service loops to exit.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// SetNotifier sets or replaces the notifier. Must be called before deploys
// start to avoid races (the notifier is used from service loop goroutines).
func (m *Manager) SetNotifier(n Notifier) {
	m.notifier = n
}

// cleanOrphans removes state files for services not in the current config and
// kills their processes if still alive.
func (m *Manager) cleanOrphans() {
	if m.stateDir == "" {
		return
	}

	entries, err := filepath.Glob(filepath.Join(m.stateDir, "*.json"))
	if err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, path := range entries {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if _, ok := m.services[name]; ok {
			continue
		}
		// Orphan state file
		RemoveState(m.stateDir, name)
	}
}
