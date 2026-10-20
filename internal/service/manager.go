package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shishberg/mezzaops/internal/config"
	"github.com/shishberg/mezzaops/internal/deploy"
)

// reloadSnapshot is the state captured by an internal stop for Reload.
type reloadSnapshot struct {
	wasRunning bool   // the backend was running just before the stop
	desired    string // DesiredStatus at the moment of the stop
}

// reloadedMsg is the result of an op that reached a service after Reload
// replaced it.
const reloadedMsg = "service was reloaded; retry"

// syncOp is a synchronous operation request sent to a service's goroutine.
type syncOp struct {
	op     string
	result chan string
	// internalStop makes the op Reload's stop-for-replacement instead of
	// op: it stops the service without recording operator intent. Only
	// Reload sets it; Do cannot.
	internalStop bool
	// snap, if non-nil, receives the snapshot taken by an internalStop
	// before result is sent.
	snap *reloadSnapshot
}

// ServiceState holds the current state of a managed service.
type ServiceState struct {
	Status        string    `json:"status"`
	DesiredStatus string    `json:"desired_status,omitempty"`
	LastDeploy    time.Time `json:"last_deploy,omitzero"`
	LastRestart   time.Time `json:"last_restart,omitzero"`
	LastResult    string    `json:"last_result,omitempty"`
	LastOutput    string    `json:"last_output,omitempty"`
	FailedStep    string    `json:"failed_step,omitempty"`
}

// managedService wraps a backend with its config, event loop, and deploy queue.
type managedService struct {
	config           config.ServiceConfig
	backend          Backend
	opCh             chan syncOp   // synchronous ops: start/stop/restart/status/logs/pull
	deployCh         chan struct{} // async deploy trigger (capacity 1, latest-wins)
	restartOnStartup bool          // adopt: false triggers a restart when the loop starts
	autostartOnReady bool          // C1: cold-boot autostart issues a "start" (not "restart") after SignalReady

	// ctx is cancelled when Reload replaces this service (or the manager
	// stops). done is closed when the service loop has exited.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

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

	// For reload
	servicesDir string
	reloadMu    sync.Mutex // serializes Reload

	// newBackend builds the backend for a service. Defaults to
	// backendForConfig; tests substitute a factory to inject failures.
	newBackend func(config.ServiceConfig) Backend

	// startOnBoot is the resolved autostart default (Config.StartOnBoot).
	// Per-service ServiceConfig.Autostart overrides this. Read by
	// newManagedService and Reload.
	startOnBoot bool

	// stateLock is the exclusive flock file on the state dir, held for
	// the lifetime of the manager. Released by Stop.
	stateLock *os.File

	// readyCh is closed when frontends are connected and it's safe to send notifications.
	readyCh chan struct{}

	// shutdownCh is closed when a self-deploy succeeds, signalling the app to exit.
	shutdownCh chan struct{}
}

// NewManager creates a Manager for the given service configs.
func NewManager(cfg *config.Config, services []config.ServiceConfig, notifier Notifier) (*Manager, error) {
	return newManager(cfg, services, notifier, nil)
}

// newManager is NewManager with an optional backend factory (nil selects
// backendForConfig).
func newManager(cfg *config.Config, services []config.ServiceConfig, notifier Notifier, factory func(config.ServiceConfig) Backend) (*Manager, error) {
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// C5: take an exclusive flock on cfg.StateDir to prevent a second
	// mezzaops instance from adopting/killing the same processes. We
	// hit this in production: a stale binary kept running alongside the
	// live one and killed adopted services out from under it.
	stateLock, err := acquireStateLock(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0755); err != nil {
		releaseStateLock(stateLock)
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		services:    make(map[string]*managedService, len(services)),
		notifier:    notifier,
		ctx:         ctx,
		cancel:      cancel,
		logDir:      cfg.LogDir,
		stateDir:    cfg.StateDir,
		servicesDir: cfg.ServicesDir,
		startOnBoot: cfg.StartOnBoot,
		stateLock:   stateLock,
		readyCh:     make(chan struct{}),
		shutdownCh:  make(chan struct{}),
	}
	m.newBackend = factory
	if m.newBackend == nil {
		m.newBackend = m.backendForConfig
	}

	for _, svc := range services {
		ms := m.newManagedService(svc, cfg.StartOnBoot, false)
		m.services[svc.Name] = ms
		m.startServiceLoop(ms)
	}

	// Clean up orphan state files
	m.cleanOrphans()

	return m, nil
}

// asProcessBackend returns the *ProcessBackend behind b, looking through
// wrappers that embed one.
func asProcessBackend(b Backend) (*ProcessBackend, bool) {
	if pb, ok := b.(interface{ processBackend() *ProcessBackend }); ok {
		return pb.processBackend(), true
	}
	return nil, false
}

// newManagedService creates a managedService with the appropriate backend.
// replacing is true when Reload is swapping out a running instance of the
// same service: cold-boot startup scheduling does not apply, and Reload
// decides whether to start the replacement.
func (m *Manager) newManagedService(svc config.ServiceConfig, startOnBoot, replacing bool) *managedService {
	backend := m.newBackend(svc)

	msCtx, msCancel := context.WithCancel(m.ctx)
	ms := &managedService{
		config:   svc,
		backend:  backend,
		opCh:     make(chan syncOp, 10),
		deployCh: make(chan struct{}, 1),
		ctx:      msCtx,
		cancel:   msCancel,
		done:     make(chan struct{}),
	}

	// Always try to load persisted state (deploy info, backend state).
	// We need the loaded DesiredStatus to decide whether to autostart a
	// process whose pid is dead: if the operator explicitly stopped
	// the service on the previous run, DesiredStatus is "stopped" and
	// autostart must not override that (unless AutostartForce is set).
	// The live Status field is not used here because an unexpected
	// process exit also writes "stopped" into it, which would
	// incorrectly suppress autostart.
	//
	// Legacy state files (written before C1) lack the desired_status
	// field entirely. The two pre-C1 paths that produce status:stopped
	// (operator stop and crashed-process exit) are indistinguishable
	// on disk. We choose to autostart in that ambiguous case: a
	// service that the operator stopped before upgrading will start
	// once after the upgrade, at which point the next state save
	// records the running intent and the operator can re-stop if
	// needed. The alternative (treating absent desired_status as
	// stopped) would silently break cold-boot recovery for services
	// that crashed just before the upgrade.
	var persistedDesired string
	var persistedBoot int64
	s, raw, err := LoadState(m.stateDir, svc.Name)
	if err == nil {
		persistedBoot = s.BootTime
		ms.state.LastDeploy = s.LastDeploy
		ms.state.LastRestart = s.LastRestart
		ms.state.LastResult = s.LastResult
		ms.state.LastOutput = s.LastOutput
		ms.state.FailedStep = s.FailedStep
		ms.state.DesiredStatus = s.DesiredStatus
		persistedDesired = s.DesiredStatus
		backend.RestoreBackendState(raw)
	}

	// On startup: adopt existing state, mark for restart, or
	// autostart on cold boot.
	adopt := svc.ShouldAdopt()
	if pb, ok := asProcessBackend(backend); ok {
		pb.adopt = adopt
		adopted, msg := pb.TryAdopt()
		log.Printf("**%s**: %s", svc.Name, msg)

		// C1: if adoption did not result in a running process and
		// autostart is enabled (and the operator did not explicitly
		// stop the service on the previous run), queue a Start.
		// The actual Start is gated on SignalReady in serviceLoop
		// via the existing restartOnStartup machinery. We set
		// autostartOnReady (not just restartOnStartup) so the loop
		// issues a "start" op rather than a "restart" — a cold-boot
		// autostart should not look like a hot reload to the
		// notifier (no "restarted" event, no LastRestart bump).

		//
		// With adopt: false a surviving child from the previous mezzaops
		// would be duplicated, so autostart only after a host reboot
		// (no child can have survived one); see rebootedSince.
		if !adopted && !replacing && svc.ShouldAutostart(startOnBoot) {
			if (persistedDesired != "stopped" || svc.AutostartForce) &&
				(adopt || m.rebootedSince(svc.Name, persistedBoot)) {
				ms.autostartOnReady = true
				ms.restartOnStartup = true
				reason := "cold boot (adoption failed)"
				if persistedDesired == "stopped" && svc.AutostartForce {
					reason = "cold boot (autostart_force; ignoring stopped state)"
				}
				log.Printf("**%s**: autostart scheduled (%s)", svc.Name, reason)
			}
		}
	} else if !adopt && !replacing {
		ms.restartOnStartup = true
	}

	return ms
}

// bootTimeFn returns the host boot time (unix seconds), or 0 if unknown.
// A variable so tests can substitute it.
var bootTimeFn = func() int64 {
	bt, err := host.BootTime()
	if err != nil {
		return 0
	}
	return int64(bt)
}

// rebootedSince reports whether the host rebooted since the state recording
// savedBoot was written, logging why an adopt: false service is not
// autostarted when it did not (or when that cannot be determined).
func (m *Manager) rebootedSince(name string, savedBoot int64) bool {
	now := bootTimeFn()
	switch {
	case savedBoot == 0:
		log.Printf("**%s**: adopt: false, not autostarting: no host boot time recorded in the saved state", name)
	case now == 0:
		log.Printf("**%s**: adopt: false, not autostarting: cannot determine the current host boot time", name)
	case now == savedBoot:
		log.Printf("**%s**: adopt: false, not autostarting: mezzaops restarted without a host reboot, so the previous process may still be running", name)
	default:
		return true
	}
	return false
}

// backendForConfig selects the appropriate backend based on the service config.
func (m *Manager) backendForConfig(svc config.ServiceConfig) Backend {
	if len(svc.Entrypoint) > 0 || svc.Process.Cmd != "" {
		return NewProcessBackend(
			svc.Name, svc.Dir,
			svc.Entrypoint, svc.Process.Cmd,
			m.logDir,
		)
	}
	if svc.ServiceName != "" {
		if runtime.GOOS == "darwin" {
			return NewLaunchctlBackend(svc.ServiceName)
		}
		return NewSystemctlBackend(svc.ServiceName, svc.UserService, svc.Sudo)
	}
	// Fallback: no-op process backend
	return NewProcessBackend(
		svc.Name, svc.Dir,
		nil, "echo 'no backend configured'",
		m.logDir,
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
	defer close(ms.done)

	// Cold-boot op on startup. Two flavours:
	//   - autostartOnReady (C1): a process service whose pid was
	//     dead on cold boot, and autostart is enabled. We issue
	//     "start" so the notification reads "started" (not
	//     "restarted") and LastRestart is not bumped.
	//   - restartOnStartup (pre-existing): non-process backends with
	//     adopt: false, or process services that legitimately need
	//     a "restart" semantics. We issue "restart".
	// Both paths wait on readyCh so the notification has somewhere
	// to land (the frontends reconnect in the background via the
	// C2 retry loop; if SignalReady has not fired by the time
	// frontends are working, the notification is still dropped
	// silently — we do not block the cold boot on chat health).
	if ms.restartOnStartup {
		op := "restart"
		if ms.autostartOnReady {
			op = "start"
		}
		select {
		case <-m.readyCh:
		case <-ms.ctx.Done():
			m.drainOps(ms)
			return
		}
		log.Printf("**%s**: cold-boot op %q (adopt: false / autostart)", ms.config.Name, op)
		result := m.handleOp(ms, op)
		log.Printf("**%s**: %s", ms.config.Name, result)
		m.saveServiceState(ms)
	}

	// Get the process exit channel if this is a ProcessBackend
	var exitCh <-chan struct{}
	if pb, ok := asProcessBackend(ms.backend); ok {
		exitCh = pb.WaitForExit()
	}

	// retired is set (loop-local) once an internal stop succeeds: Reload is
	// replacing this service, so nothing further may run on its backend.
	retired := false
	forwardDeploy := false

	for {
		select {
		case op := <-ms.opCh:
			if retired {
				op.result <- reloadedMsg
				continue
			}
			var result string
			if op.internalStop {
				var snap reloadSnapshot
				snap, result = m.internalStop(ms)
				if result == "stopped" {
					retired = true
					if op.snap != nil {
						*op.snap = snap
					}
				}
			} else {
				result = m.handleOp(ms, op.op)
			}

			// Persist state after start/stop/restart, before replying, so
			// a caller (notably Reload) that acts on the reply sees it.
			if op.op == "start" || op.op == "stop" || op.op == "restart" || op.internalStop {
				m.saveServiceState(ms)
			}
			op.result <- result

			// Refresh exit channel after start/restart (new process, new done chan)
			if pb, ok := asProcessBackend(ms.backend); ok {
				exitCh = pb.WaitForExit()
			}

		case <-ms.deployCh:
			if retired || ms.ctx.Err() != nil {
				// Reload is replacing or removing this service; never
				// run the deploy here. It is handed on when the loop exits.
				forwardDeploy = true
				continue
			}
			m.executeDeploy(ms)

			// Refresh exit channel after deploy (may have restarted)
			if pb, ok := asProcessBackend(ms.backend); ok {
				exitCh = pb.WaitForExit()
			}

		case <-exitCh:
			exitCh = nil
			if retired {
				continue
			}
			// Process exited unexpectedly
			m.notifyEvent(ms.config.Name, "exited")
			m.saveServiceState(ms)
			// exitCh is now nil: the closed channel would otherwise
			// busy-loop.

		case <-ms.ctx.Done():
			m.drainOps(ms)
			if len(ms.deployCh) > 0 {
				forwardDeploy = true
			}
			if forwardDeploy && m.ctx.Err() == nil {
				// This is the only place a deploy is forwarded. The
				// replacement is installed before this context is
				// cancelled, so the request reaches it; for a removed
				// service there is none, and the failure is reported.
				name := ms.config.Name
				if err := m.RequestDeploy(name); err != nil {
					m.notifier.DeployFailed(name, "reload", fmt.Sprintf("deploy was requested while the service was being reloaded and could not be run: %v", err))
				}
			}
			return
		}
	}
}

// goneMsg is the reply for an op that cannot run because its service was
// replaced by Reload, or the manager is stopping.
func (m *Manager) goneMsg() string {
	if m.ctx.Err() != nil {
		return "manager shutting down"
	}
	return reloadedMsg
}

// drainOps answers every op still queued on a service whose loop is
// exiting, so callers do not wait forever.
func (m *Manager) drainOps(ms *managedService) {
	msg := m.goneMsg()
	for {
		select {
		case op := <-ms.opCh:
			op.result <- msg
		default:
			return
		}
	}
}

// internalStop stops the service for a reload and returns the state Reload
// needs. It runs inside the service loop, so the snapshot cannot be made
// stale by queued ops. Unlike "stop" it does not record operator intent.
func (m *Manager) internalStop(ms *managedService) (reloadSnapshot, string) {
	status, statusErr := ms.backend.Status(m.ctx)
	wasRunning := statusErr == nil && status == "running"
	if err := ms.backend.Stop(m.ctx); err != nil {
		return reloadSnapshot{}, fmt.Sprintf("stop failed: %v", err)
	}
	ms.stateMu.Lock()
	snap := reloadSnapshot{wasRunning: wasRunning, desired: ms.state.DesiredStatus}
	ms.stateMu.Unlock()
	m.notifyEvent(ms.config.Name, "stopped")
	return snap, "stopped"
}

// handleOp executes a synchronous operation within the service loop.
func (m *Manager) handleOp(ms *managedService, op string) string {
	ctx := m.ctx
	switch op {
	case "start":
		// Record operator intent when the command is accepted, before
		// the backend call, so a failed start still clears a prior
		// "stopped" and a later cold boot is not suppressed.
		ms.stateMu.Lock()
		ms.state.DesiredStatus = "running"
		ms.stateMu.Unlock()
		if err := ms.backend.Start(ctx); err != nil {
			return fmt.Sprintf("start failed: %v", err)
		}
		m.notifyEvent(ms.config.Name, "started")
		return "started"

	case "stop":
		if err := ms.backend.Stop(ctx); err != nil {
			return fmt.Sprintf("stop failed: %v", err)
		}
		// Record operator intent. Live status is also "stopped" but
		// will be overwritten on the next saveServiceState when the
		// backend reports its status; DesiredStatus is preserved
		// across that rewrite.
		ms.stateMu.Lock()
		ms.state.DesiredStatus = "stopped"
		ms.stateMu.Unlock()
		m.notifyEvent(ms.config.Name, "stopped")
		return "stopped"

	case "restart":
		ms.stateMu.Lock()
		ms.state.DesiredStatus = "running"
		ms.stateMu.Unlock()
		if err := ms.backend.Restart(ctx); err != nil {
			return fmt.Sprintf("restart failed: %v", err)
		}
		ms.stateMu.Lock()
		ms.state.LastRestart = time.Now()
		ms.stateMu.Unlock()
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

		m.saveServiceState(ms)
		m.notifier.DeployFailed(name, failedStep, output)
		return
	}

	// Self-deploy: skip restart, save state, notify, then signal shutdown
	if ms.config.SelfDeploy {
		ms.stateMu.Lock()
		ms.state.Status = "running"
		ms.state.LastResult = "success"
		ms.state.LastOutput = result.Output
		ms.state.FailedStep = ""
		ms.stateMu.Unlock()

		m.saveServiceState(ms)
		m.notifier.DeploySucceeded(name, result.Output)
		close(m.shutdownCh)
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

		m.saveServiceState(ms)
		m.notifier.DeployFailed(name, "restart", result.Output)
		return
	}

	ms.stateMu.Lock()
	ms.state.Status = "running"
	ms.state.LastResult = "success"
	ms.state.LastOutput = result.Output
	ms.state.FailedStep = ""
	ms.state.LastRestart = time.Now()
	// The deploy left the process running, so the operator's intent is
	// "running" regardless of an earlier stop.
	ms.state.DesiredStatus = "running"
	ms.stateMu.Unlock()

	m.saveServiceState(ms)
	m.notifier.DeploySucceeded(name, result.Output)
	m.notifyEvent(name, "restarted")
}

// saveServiceState persists the current state of a managed service to disk.
func (m *Manager) saveServiceState(ms *managedService) {
	if m.stateDir == "" {
		return
	}
	// SaveBackendState may inspect processes; keep that outside stateMu.
	backendState := ms.backend.SaveBackendState()
	ms.stateMu.Lock()
	state := State{
		Status:        ms.state.Status,
		DesiredStatus: ms.state.DesiredStatus,
		LastDeploy:    ms.state.LastDeploy,
		LastResult:    ms.state.LastResult,
		LastOutput:    ms.state.LastOutput,
		FailedStep:    ms.state.FailedStep,
		Backend:       backendState,
		BootTime:      bootTimeFn(),
	}
	ms.stateMu.Unlock()

	// If status is not a special deploy state, query the backend for live status.
	// We only overwrite Status, never DesiredStatus — the operator's last
	// expressed intent must survive an unexpected process exit so C1
	// autostart can distinguish "operator stopped" from "crashed".
	if state.Status != "deploying" && state.Status != "failed" {
		if status, err := ms.backend.Status(m.ctx); err == nil {
			state.Status = status
		}
	}

	_ = SaveState(m.stateDir, ms.config.Name, state)
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
	return m.doOn(ms, syncOp{op: op, result: make(chan string, 1)})
}

// doOn sends so to ms's loop and waits for the result. It returns instead of
// blocking if the service is replaced or the manager stops.
func (m *Manager) doOn(ms *managedService, so syncOp) string {
	select {
	case ms.opCh <- so:
	case <-ms.ctx.Done():
		return m.goneMsg()
	}

	select {
	case result := <-so.result:
		return result
	case <-ms.ctx.Done():
		// The loop may have answered just before exiting.
		select {
		case result := <-so.result:
			return result
		default:
			return m.goneMsg()
		}
	}
}

// RequestDeploy queues a deploy request for the named service (latest-wins).
func (m *Manager) RequestDeploy(name string) error {
	// Look up and enqueue under m.mu. Reload swaps the service in the map
	// under the same lock before retiring the old one, so a request either
	// reaches the replacement or is queued on the old service before its
	// context is cancelled, where the retiring loop forwards it.
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.services[name]
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
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	if m.servicesDir == "" {
		return fmt.Errorf("no services_dir configured")
	}

	newConfigs, err := config.LoadServices(m.servicesDir)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	var errs []error
	seen := make(map[string]bool)
	for _, svc := range newConfigs {
		seen[svc.Name] = true

		m.mu.Lock()
		existing, exists := m.services[svc.Name]
		m.mu.Unlock()

		if exists {
			// Check if config changed
			if !serviceConfigEqual(existing.config, svc) {
				var snap reloadSnapshot
				so := syncOp{internalStop: true, result: make(chan string, 1), snap: &snap}
				if result := m.doOn(existing, so); result != "stopped" {
					// Keep the old service in place; the swap would
					// otherwise run two copies of the process.
					errs = append(errs, fmt.Errorf("reload %s: internal stop: %s", svc.Name, result))
					continue
				}

				// The replacement keeps the old desired state and is
				// started if and only if the old service was running.
				// (The desired-state copy matters when no state dir is
				// configured.)
				ms := m.newManagedService(svc, m.startOnBoot, true)
				ms.restartOnStartup = snap.wasRunning
				ms.autostartOnReady = snap.wasRunning
				ms.state.DesiredStatus = snap.desired
				if snap.wasRunning {
					log.Printf("**%s**: config changed; restarting after reload", svc.Name)
				}
				m.mu.Lock()
				m.services[svc.Name] = ms
				m.mu.Unlock()
				// Retire the old loop; queued and late ops get an error.
				existing.cancel()
				m.startServiceLoop(ms)
			}
			continue
		}

		// New service
		ms := m.newManagedService(svc, m.startOnBoot, false)
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
		m.mu.Lock()
		removed := m.services[name]
		m.mu.Unlock()
		if removed == nil {
			continue
		}
		// The internal stop retires the loop, so a deploy queued behind it
		// is reported as failed instead of running on a removed service.
		m.doOn(removed, syncOp{internalStop: true, result: make(chan string, 1)})
		m.mu.Lock()
		delete(m.services, name)
		m.mu.Unlock()
		removed.cancel() // end the loop of a service that no longer exists
	}

	m.cleanOrphans()

	return errors.Join(errs...)
}

// serviceConfigEqual compares two ServiceConfigs for equality.
func serviceConfigEqual(a, b config.ServiceConfig) bool {
	if a.Name != b.Name || a.Dir != b.Dir || a.Branch != b.Branch || a.Repo != b.Repo {
		return false
	}
	if a.ServiceName != b.ServiceName || a.UserService != b.UserService || a.Sudo != b.Sudo {
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
	if a.RequireConfirmation != b.RequireConfirmation {
		return false
	}
	if a.SelfDeploy != b.SelfDeploy {
		return false
	}
	// Tri-state booleans: compare the pointers first (nil vs set),
	// then the values when both are set. A change to Adopt / Autostart
	// must trigger a config-equal mismatch so Reload re-creates the
	// managed service with the new startup behaviour.
	if !sameBoolPtr(a.Adopt, b.Adopt) {
		return false
	}
	if !sameBoolPtr(a.Autostart, b.Autostart) {
		return false
	}
	if a.AutostartForce != b.AutostartForce {
		return false
	}
	return true
}

// sameBoolPtr returns true if the two *bool values are both nil, or
// both non-nil and dereference to the same value. Tri-state fields
// like Adopt / Autostart use this so a transition from "unset" to
// "true" or "true" to "false" is correctly detected as a config change.
func sameBoolPtr(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
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
	maps.Copy(services, m.services)
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

// GetServiceState returns the current state for a single named service.
func (m *Manager) GetServiceState(name string) (ServiceState, bool) {
	m.mu.Lock()
	ms, ok := m.services[name]
	m.mu.Unlock()
	if !ok {
		return ServiceState{}, false
	}
	return m.liveState(ms), true
}

// GetServiceLogs returns the recent logs for a named service.
func (m *Manager) GetServiceLogs(name string) string {
	return m.Do(name, "logs")
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

// ShutdownCh returns a channel that is closed when a self-deploy succeeds,
// signalling the application to shut down gracefully.
func (m *Manager) ShutdownCh() <-chan struct{} {
	return m.shutdownCh
}

// Stop cancels the manager context and waits for all service loops to exit.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
	// Release the state lock so a follow-up instance can start
	// immediately. flock(2) would also release this on process exit,
	// but releasing explicitly makes the lifecycle clear and lets a
	// subsequent NewManager in the same process (e.g. a test) succeed.
	releaseStateLock(m.stateLock)
	m.stateLock = nil
}

// SetNotifier sets or replaces the notifier. Must be called before deploys
// start to avoid races (the notifier is used from service loop goroutines).
func (m *Manager) SetNotifier(n Notifier) {
	m.notifier = n
}

// SignalReady indicates that frontends are connected and notifications can be sent.
// Service loops waiting on restartOnStartup will proceed after this is called.
func (m *Manager) SignalReady() {
	close(m.readyCh)
}

// NotifyWebhook forwards webhook details to the notifier, for a service that
// matched an incoming webhook.
func (m *Manager) NotifyWebhook(name string, info WebhookInfo) {
	m.notifier.WebhookReceived(name, info)
}

// killFn sends a signal; a seam so tests can observe cleanOrphans.
var killFn = syscall.Kill

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
		// Orphan state file -- kill the process if it's still alive
		_, raw, err := LoadState(m.stateDir, name)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		// Extract process info from backend sub-object or top-level (old format)
		var ps processBackendState
		var wrapper struct {
			Backend json.RawMessage `json:"backend"`
		}
		if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Backend) > 0 {
			_ = json.Unmarshal(wrapper.Backend, &ps)
		} else {
			_ = json.Unmarshal(raw, &ps)
		}
		if ps.PID != 0 && ps.PGID > 0 && IsAlive(ps.PID) && VerifyProcess(ps) {
			_ = killFn(-ps.PGID, syscall.SIGKILL)
		}
		RemoveState(m.stateDir, name)
	}
}
