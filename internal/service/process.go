package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

	restoredState  *processBackendState // set by RestoreBackendState
	restoredStatus string               // status from the state file
}

// NewProcessBackend creates a new ProcessBackend.
func NewProcessBackend(name, dir string, entrypoint []string, cmd string, logDir string, adopt bool) *ProcessBackend {
	return &ProcessBackend{
		name:       name,
		dir:        dir,
		entrypoint: entrypoint,
		cmd:        cmd,
		logDir:     logDir,
		adopt:      adopt,
	}
}

// Start launches the process. If already running, returns nil.
func (p *ProcessBackend) Start(ctx context.Context) error {
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
		close(done)
	}()

	return nil
}

// Stop sends SIGTERM to the process group, waits up to 5 seconds,
// then sends SIGKILL. Returns nil if the process is not running.
func (p *ProcessBackend) Stop(ctx context.Context) error {
	p.mu.Lock()

	if !p.isRunning() {
		p.mu.Unlock()
		return nil
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
	p.mu.Unlock()

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
// the restored state. Returns a string describing what happened.
func (p *ProcessBackend) TryAdopt() string {
	if !p.adopt {
		return "adoption disabled"
	}

	if p.restoredState == nil {
		return "no state to adopt"
	}

	ps := *p.restoredState

	// Desired state is stopped -- respect it
	if p.restoredStatus == "stopped" {
		return "stopped (preserved from previous session)"
	}

	// PID is dead -- process crashed while we were down
	if !IsAlive(ps.PID) {
		return fmt.Sprintf("stale pid %d (process dead)", ps.PID)
	}

	// PID is alive but belongs to a different process (reboot or PID reuse)
	if !VerifyProcess(ps) {
		return fmt.Sprintf("pid %d reused by different process", ps.PID)
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

	return fmt.Sprintf("adopted (pid %d)", ps.PID)
}

// IsRunning returns whether the process is currently running (thread-safe).
func (p *ProcessBackend) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isRunning()
}

// WaitForExit returns a channel that is closed when the process exits.
func (p *ProcessBackend) WaitForExit() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done == nil {
		// Return a closed channel if not running
		ch := make(chan struct{})
		close(ch)
		return ch
	}
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
			p.mu.Unlock()
			close(done)
			return
		}
	}
}
