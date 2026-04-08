package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SystemctlBackend manages a systemd service unit via systemctl.
type SystemctlBackend struct {
	unit     string
	userMode bool
}

// NewSystemctlBackend returns a Backend that controls the given systemd unit.
// If userMode is true, all commands include the --user flag.
func NewSystemctlBackend(unit string, userMode bool) *SystemctlBackend {
	return &SystemctlBackend{unit: unit, userMode: userMode}
}

func (s *SystemctlBackend) systemctl(ctx context.Context, args ...string) *exec.Cmd {
	if s.userMode {
		args = append([]string{"--user"}, args...)
	}
	return exec.CommandContext(ctx, "systemctl", args...)
}

// Start starts the service.
func (s *SystemctlBackend) Start(ctx context.Context) error {
	return s.systemctl(ctx, "start", s.unit).Run()
}

// Stop stops the service.
func (s *SystemctlBackend) Stop(ctx context.Context) error {
	return s.systemctl(ctx, "stop", s.unit).Run()
}

// Restart restarts the service.
func (s *SystemctlBackend) Restart(ctx context.Context) error {
	return s.systemctl(ctx, "restart", s.unit).Run()
}

// Status returns "running", "stopped", or "failed" based on systemctl is-active.
func (s *SystemctlBackend) Status(ctx context.Context) (string, error) {
	out, err := s.systemctl(ctx, "is-active", s.unit).Output()
	state := strings.TrimSpace(string(out))

	if err != nil {
		switch state {
		case "inactive", "dead":
			return "stopped", nil
		case "failed":
			return "failed", nil
		}
		return "", fmt.Errorf("systemctl is-active %s: %w", s.unit, err)
	}

	if state == "active" {
		return "running", nil
	}
	return state, nil
}

// Logs returns recent log output from journalctl.
func (s *SystemctlBackend) Logs(ctx context.Context, tail int) (string, error) {
	args := []string{"-u", s.unit, "-n", fmt.Sprintf("%d", tail), "--no-pager"}
	if s.userMode {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
	if err != nil {
		return "", fmt.Errorf("journalctl: %w", err)
	}
	return string(out), nil
}
