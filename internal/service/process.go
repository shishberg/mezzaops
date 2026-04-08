package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ProcessBackend manages a service as a child process, merging mezzaops's
// process adoption with matterops's graceful-shutdown approach.
type ProcessBackend struct {
	name       string
	dir        string
	entrypoint []string // explicit argv (mezzaops style)
	cmd        string   // sh -c wrapper (matterops style) -- used if entrypoint is empty
	logDir     string
	stateDir   string
	adopt      bool // whether to attempt process adoption

	mu      sync.Mutex
	pid     int
	pgid    int
	logPath string
	process *exec.Cmd
	done    chan struct{} // closed when process exits
}

// NewProcessBackend creates a new ProcessBackend.
func NewProcessBackend(name, dir string, entrypoint []string, cmd string, logDir, stateDir string, adopt bool) *ProcessBackend {
	return &ProcessBackend{
		name:       name,
		dir:        dir,
		entrypoint: entrypoint,
		cmd:        cmd,
		logDir:     logDir,
		stateDir:   stateDir,
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

	// Save running state with process identity
	_ = SaveState(p.stateDir, p.name, RunningState(pid, p.logPath))

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

	// Save stopped state so adoption knows intent
	_ = SaveState(p.stateDir, p.name, State{Status: "stopped"})

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

// TryAdopt attempts to re-adopt a process from a previous session using
// the saved state file. Returns a string describing what happened.
func (p *ProcessBackend) TryAdopt() string {
	if !p.adopt {
		return "adoption disabled"
	}

	s, err := LoadState(p.stateDir, p.name)
	if err != nil {
		return "no state file, starting fresh"
	}

	// Desired state is stopped -- respect it
	if s.Status == "stopped" {
		return "stopped (preserved from previous session)"
	}

	// PID is dead -- process crashed while we were down
	if !IsAlive(s.PID) {
		return fmt.Sprintf("stale pid %d (process dead)", s.PID)
	}

	// PID is alive but belongs to a different process (reboot or PID reuse)
	if !VerifyProcess(s) {
		return fmt.Sprintf("pid %d reused by different process", s.PID)
	}

	// PID is alive and verified -- adopt it
	p.mu.Lock()
	p.pid = s.PID
	p.pgid = s.PGID
	p.logPath = s.LogPath

	done := make(chan struct{})
	p.done = done
	p.mu.Unlock()

	// Poll since we can't cmd.Wait() on a process we didn't spawn
	go p.pollAlive(s.PID, done)

	return fmt.Sprintf("adopted (pid %d)", s.PID)
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
