package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SystemctlBackend manages a systemd service unit via systemctl.
type SystemctlBackend struct {
	unit     string
	userMode bool
	sudo     bool
}

// NewSystemctlBackend returns a Backend that controls the given systemd unit.
// If userMode is true, commands include the --user flag.
// If sudo is true, commands are run via sudo (for system services managed by
// a non-root user).
func NewSystemctlBackend(unit string, userMode bool, sudo bool) *SystemctlBackend {
	return &SystemctlBackend{unit: unit, userMode: userMode, sudo: sudo}
}

func (s *SystemctlBackend) systemctl(ctx context.Context, args ...string) *exec.Cmd {
	if s.userMode {
		args = append([]string{"--user"}, args...)
	}
	if s.sudo {
		args = append([]string{"systemctl"}, args...)
		return exec.CommandContext(ctx, "sudo", args...)
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
	var cmd *exec.Cmd
	if s.sudo {
		args = append([]string{"journalctl"}, args...)
		cmd = exec.CommandContext(ctx, "sudo", args...)
	} else {
		cmd = exec.CommandContext(ctx, "journalctl", args...)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("journalctl: %w", err)
	}
	return string(out), nil
}

// SaveBackendState returns nil (systemctl manages its own state).
func (s *SystemctlBackend) SaveBackendState() json.RawMessage { return nil }

// RestoreBackendState is a no-op for systemctl.
func (s *SystemctlBackend) RestoreBackendState(json.RawMessage) {}
