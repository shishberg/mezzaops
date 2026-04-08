package service

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

// State represents the persisted state of a managed process.
type State struct {
	Status     string `json:"status"`
	PID        int    `json:"pid,omitempty"`
	PGID       int    `json:"pgid,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
	BootTime   int64  `json:"boot_time,omitempty"`
	CreateTime int64  `json:"create_time,omitempty"`
}

func statePath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

// SaveState atomically writes the state to a JSON file.
// It writes to a temporary file first, then renames to prevent corruption.
func SaveState(dir, name string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := statePath(dir, name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir, name))
}

// LoadState reads the state from a JSON file.
func LoadState(dir, name string) (State, error) {
	data, err := os.ReadFile(statePath(dir, name))
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// RemoveState deletes the state file.
func RemoveState(dir, name string) {
	_ = os.Remove(statePath(dir, name))
}

// RunningState creates a State for a running process, capturing boot time
// and process create time for identity verification on re-adoption.
func RunningState(pid int, logPath string) State {
	s := State{
		Status:  "running",
		PID:     pid,
		PGID:    pid,
		LogPath: logPath,
	}
	if bootTime, err := host.BootTime(); err == nil {
		s.BootTime = int64(bootTime)
	}
	if proc, err := process.NewProcess(int32(pid)); err == nil {
		if ct, err := proc.CreateTime(); err == nil {
			s.CreateTime = ct
		}
	}
	return s
}
