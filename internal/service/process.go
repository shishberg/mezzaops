package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

// processBackendState holds the backend-specific state for a ProcessBackend.
type processBackendState struct {
	PID        int    `json:"pid,omitempty"`
	PGID       int    `json:"pgid,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
	BootTime   int64  `json:"boot_time,omitempty"`
	CreateTime int64  `json:"create_time,omitempty"`
	// Orphan marks a record persisted for a live process this backend does
	// not manage (adopt: false), as opposed to the record of a child that
	// has exited.
	Orphan bool `json:"orphan,omitempty"`
}

// ProcessBackend manages a service as a child process with process adoption
// and graceful shutdown (SIGTERM then SIGKILL after timeout).
type ProcessBackend struct {
	name       string
	dir        string
	entrypoint []string // explicit argv
	cmd        string   // sh -c wrapper -- used if entrypoint is empty
	logDir     string
	adopt      bool // whether to attempt process adoption

	mu      sync.Mutex
	pid     int
	pgid    int
	logPath string
	process *exec.Cmd
	done    chan struct{} // closed when process exits

	unverifiedLogged atomic.Bool          // the "cannot be verified" warning was logged for this record
	orphan           *processBackendState // live untracked process from a previous run (adopt: false)
	restoredState    *processBackendState // set by RestoreBackendState
	restoredStatus   string               // status from the state file
}

// NewProcessBackend creates a new ProcessBackend.
func NewProcessBackend(name, dir string, entrypoint []string, cmd string, logDir string) *ProcessBackend {
	return &ProcessBackend{
		name:       name,
		dir:        dir,
		entrypoint: entrypoint,
		cmd:        cmd,
		logDir:     logDir,
	}
}

// Start launches the process. If already running, returns nil. It refuses
// while a live process from a previous instance (adopt: false) exists, so
// the orphan is not duplicated and its record not erased.
func (p *ProcessBackend) Start(ctx context.Context) error {
	p.mu.Lock()
	running := p.isRunning()
	orphan := p.orphan
	p.mu.Unlock()
	if running {
		return nil
	}
	if orphan != nil {
		// The process lookup happens outside p.mu.
		if p.recordAlive(*orphan, false) {
			return fmt.Errorf("previous instance pid %d is still running; stop it or enable adopt", orphan.PID)
		}
		p.mu.Lock()
		if p.orphan == orphan {
			p.orphan = nil
		}
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isRunning() {
		return nil
	}

	// Build the command
	var cmd *exec.Cmd
	if len(p.entrypoint) > 0 {
		cmd = exec.Command(p.entrypoint[0], p.entrypoint[1:]...)
	} else if p.cmd != "" {
		cmd = exec.Command("sh", "-c", p.cmd)
	} else {
		return fmt.Errorf("no entrypoint or cmd configured for %s", p.name)
	}

	cmd.Dir = p.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Create log file with temp name (we don't know PID yet)
	tmpPath := filepath.Join(p.logDir, p.name+".starting.log")
	if err := os.MkdirAll(p.logDir, 0755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	logFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("starting process: %w", err)
	}

	pid := cmd.Process.Pid
	p.pid = pid
	p.pgid = pid
	p.process = cmd

	// Rename log file to include PID (child keeps writing -- same inode)
	p.logPath = LogPath(p.logDir, p.name, pid)
	_ = os.Rename(tmpPath, p.logPath)

	// Write start marker, then close our handle -- child owns the fd now
	_, _ = fmt.Fprintf(logFile, "=== Started at %s (pid %d) ===\n", time.Now().Format(time.RFC3339), pid)
	_ = logFile.Close()

	// Clean up old log files (keep last 5)
	CleanupOldLogs(p.logDir, p.name, 5)

	// Wait goroutine: reaps the child and signals done
	done := make(chan struct{})
	p.done = done
	go func() {
		_ = cmd.Wait()
		// Forget the pid once the child is reaped, so a later state save
		// does not persist a pid the OS may reuse for an unrelated process.
		p.mu.Lock()
		if p.done == done {
			p.pid = 0
			p.pgid = 0
			p.process = nil
			p.done = nil
		}
		p.mu.Unlock()
		close(done)
	}()

	return nil
}

type keepOrphanKey struct{}

// withKeepOrphan returns a context telling Stop to leave a leftover
// (orphan) process alone. Reload's internal stop uses it.
func withKeepOrphan(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepOrphanKey{}, true)
}

// Stop sends SIGTERM to the process group, waits up to 5 seconds,
// then sends SIGKILL. Returns nil if the process is not running. If a
// verified leftover process from a previous instance exists, it is stopped
// the same way; one whose identity cannot be verified is not touched and an
// error is returned.
func (p *ProcessBackend) Stop(ctx context.Context) error {
	p.mu.Lock()

	if !p.isRunning() {
		orphan := p.orphan
		p.mu.Unlock()
		if orphan == nil || ctx.Value(keepOrphanKey{}) != nil {
			return nil
		}
		return p.stopOrphan(orphan)
	}

	pgid := p.pgid
	done := p.done
	logPath := p.logPath

	// Clear state under the lock
	p.pid = 0
	p.pgid = 0
	p.process = nil
	p.done = nil

	p.mu.Unlock()

	// Send SIGTERM to the process group
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	// Wait for exit with a 5-second timeout, then force-kill
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}

	// Write stop marker to log
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			_, _ = fmt.Fprintf(f, "=== Stopped at %s ===\n", time.Now().Format(time.RFC3339))
			_ = f.Close()
		}
	}

	return nil
}

// stopOrphan terminates a leftover process from a previous instance.
func (p *ProcessBackend) stopOrphan(orphan *processBackendState) error {
	forget := func() {
		p.mu.Lock()
		if p.orphan == orphan {
			p.orphan = nil
		}
		p.mu.Unlock()
	}
	if orphan.PID == 0 || !IsAlive(orphan.PID) {
		forget()
		return nil
	}
	id, why := checkIdentity(*orphan)
	switch id {
	case identityMismatch:
		forget() // the pid now belongs to something else
		return nil
	case identityUnverified:
		return fmt.Errorf("previous instance pid %d cannot be verified as our process (%s); stop it manually", orphan.PID, why)
	}

	pgid := orphan.PGID
	if pgid <= 0 {
		pgid = orphan.PID
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if !waitDead(orphan.PID, 5*time.Second) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !waitDead(orphan.PID, 5*time.Second) {
			return fmt.Errorf("previous instance pid %d did not exit after SIGKILL", orphan.PID)
		}
	}
	forget()
	return nil
}

// waitDead polls until pid is gone or timeout elapses.
func waitDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for IsAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}

// Restart stops then starts the process.
func (p *ProcessBackend) Restart(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil {
		return err
	}
	return p.Start(ctx)
}

// Status returns "running" or "stopped".
func (p *ProcessBackend) Status(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isRunning() {
		return "running", nil
	}
	return "stopped", nil
}

// Logs returns the tail of the current log file.
func (p *ProcessBackend) Logs(ctx context.Context, tail int) (string, error) {
	p.mu.Lock()
	logPath := p.logPath
	p.mu.Unlock()

	if logPath == "" {
		return "", nil
	}
	return TailLogFile(logPath, tail), nil
}

// SaveBackendState returns the backend-specific state as JSON.
// Returns nil if the process is not running.
func (p *ProcessBackend) SaveBackendState() json.RawMessage {
	p.mu.Lock()
	pid := p.pid
	pgid := p.pgid
	logPath := p.logPath
	orphan := p.orphan
	p.mu.Unlock()

	if pid == 0 && orphan != nil && p.recordAlive(*orphan, false) {
		raw, err := json.Marshal(orphan)
		if err == nil {
			return raw
		}
	}
	return saveBackendStateFromFields(pid, pgid, logPath)
}

func saveBackendStateFromFields(pid, pgid int, logPath string) json.RawMessage {
	if pid == 0 {
		return nil
	}

	ps := processBackendState{
		PID:     pid,
		PGID:    pgid,
		LogPath: logPath,
	}
	if bootTime, err := host.BootTime(); err == nil {
		ps.BootTime = int64(bootTime)
	}
	if proc, err := process.NewProcess(int32(pid)); err == nil {
		if ct, err := proc.CreateTime(); err == nil {
			ps.CreateTime = ct
		}
	}

	data, err := json.Marshal(ps)
	if err != nil {
		return nil
	}
	return data
}

// RestoreBackendState restores the backend-specific state from the full state
// file JSON. It handles both the new format (with a "backend" sub-object) and
// the old format (with pid/pgid/etc at the top level) for migration.
func (p *ProcessBackend) RestoreBackendState(fullStateJSON json.RawMessage) {
	if fullStateJSON == nil {
		return
	}

	// Try new format: extract the "backend" sub-object
	var wrapper struct {
		Status  string          `json:"status"`
		Backend json.RawMessage `json:"backend"`
	}
	if err := json.Unmarshal(fullStateJSON, &wrapper); err != nil {
		return
	}

	p.restoredStatus = wrapper.Status

	var ps processBackendState
	if len(wrapper.Backend) > 0 {
		// New format: unmarshal from backend sub-object
		if err := json.Unmarshal(wrapper.Backend, &ps); err != nil {
			return
		}
	} else {
		// Old format: pid/pgid/etc at top level (migration path)
		if err := json.Unmarshal(fullStateJSON, &ps); err != nil {
			return
		}
	}

	p.restoredState = &ps
}

// TryAdopt attempts to re-adopt a process from a previous session using
// the restored state. It reports whether a live process was adopted and a
// human-readable description of what happened.
func (p *ProcessBackend) TryAdopt() (adopted bool, msg string) {
	if !p.adopt {
		return false, "adoption disabled"
	}

	if p.restoredState == nil {
		return false, "no state to adopt"
	}

	ps := *p.restoredState

	// A live process we started is adopted whatever status was saved: an
	// adopt: false run saves "stopped" while its orphan is still alive.
	stopped := p.restoredStatus == "stopped"
	if ps.PID == 0 || !IsAlive(ps.PID) {
		if stopped {
			return false, "stopped (preserved from previous session)"
		}
		return false, fmt.Sprintf("stale pid %d (process dead)", ps.PID)
	}
	id, why := checkIdentity(ps)
	if id == identityMismatch {
		if stopped {
			return false, "stopped (preserved from previous session)"
		}
		// Alive but a different process (reboot or PID reuse)
		return false, fmt.Sprintf("pid %d reused by different process", ps.PID)
	}
	if id == identityUnverified {
		// A stopped record we cannot verify is not trustworthy: the pid
		// may have been reused after the child exited.
		if stopped {
			return false, fmt.Sprintf("stopped (pid %d cannot be verified: %s)", ps.PID, why)
		}
		p.warnUnverified(ps.PID, why)
	}

	// PID is alive and verified -- adopt it
	p.mu.Lock()
	p.pid = ps.PID
	p.pgid = ps.PGID
	p.logPath = ps.LogPath

	done := make(chan struct{})
	p.done = done
	p.mu.Unlock()

	// Poll since we can't cmd.Wait() on a process we didn't spawn
	go p.pollAlive(ps.PID, done)

	return true, fmt.Sprintf("adopted (pid %d)", ps.PID)
}

// PersistedProcessAlive reports whether the restored state refers to a
// process that is still alive and is, as far as can be verified, not some
// other process. It is used to avoid starting a duplicate of a child that
// outlived the previous manager. See recordAlive for the rules.
func (p *ProcessBackend) PersistedProcessAlive() bool {
	if p.restoredState == nil {
		return false
	}
	// A "stopped" record that is not marked as an orphan is what a child
	// that exited leaves behind; its pid may since have been reused.
	fromExit := p.restoredStatus == "stopped" && !p.restoredState.Orphan
	return p.recordAlive(*p.restoredState, fromExit)
}

// recordAlive reports whether ps.PID is alive and not known to be a
// different process: a recorded boot time must match the current one, and a
// recorded create time must match the live process. When neither is
// recorded (gopsutil can fail on darwin) the identity is unverified; to
// avoid a duplicate it is then assumed alive, unless the record came from an
// observed exit (fromExit), where the pid is likely stale.
func (p *ProcessBackend) recordAlive(ps processBackendState, fromExit bool) bool {
	if ps.PID == 0 || !IsAlive(ps.PID) {
		return false
	}
	id, why := checkIdentity(ps)
	switch id {
	case identityMismatch:
		return false
	case identityUnverified:
		if fromExit {
			return false
		}
		p.warnUnverified(ps.PID, why)
	}
	return true
}

// warnUnverified logs once per record that an identity could not be verified.
func (p *ProcessBackend) warnUnverified(pid int, why string) {
	if p.unverifiedLogged.CompareAndSwap(false, true) {
		log.Printf("**%s**: pid %d cannot be verified as our process (%s); assuming it is", p.name, pid, why)
	}
}

// MarkOrphan records the restored process as a live orphan that this
// backend does not manage. SaveBackendState keeps persisting its record
// while it stays alive, so later saves and reloads still see it.
func (p *ProcessBackend) MarkOrphan() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.restoredState != nil {
		ps := *p.restoredState
		ps.Orphan = true
		p.orphan = &ps
		p.unverifiedLogged.Store(false)
	}
}

// processBackend lets wrappers embedding *ProcessBackend be recognised as
// process-backed (see asProcessBackend).
func (p *ProcessBackend) processBackend() *ProcessBackend { return p }

// IsRunning returns whether the process is currently running (thread-safe).
func (p *ProcessBackend) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isRunning()
}

// WaitForExit returns a channel that is closed when the running process exits.
// Returns nil if no process is running, which makes the channel effectively
// disabled in a select (a nil channel blocks forever).
func (p *ProcessBackend) WaitForExit() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// isRunning checks if the process is alive (must be called with mu held).
func (p *ProcessBackend) isRunning() bool {
	if p.process == nil && p.pid == 0 {
		return false
	}
	if p.pid != 0 {
		return IsAlive(p.pid)
	}
	return false
}

// pollAlive monitors an adopted process since we can't cmd.Wait() on it.
func (p *ProcessBackend) pollAlive(pid int, done chan struct{}) {
	for {
		time.Sleep(2 * time.Second)
		if !IsAlive(pid) {
			p.mu.Lock()
			p.pid = 0
			p.pgid = 0
			p.process = nil
			p.done = nil
			p.mu.Unlock()
			close(done)
			return
		}
	}
}
