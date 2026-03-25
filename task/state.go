package task

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

type State struct {
	Status     string `json:"status"`               // "running" or "stopped"
	PID        int    `json:"pid,omitempty"`         // 0 when stopped
	PGID       int    `json:"pgid,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
	BootTime   int64  `json:"boot_time,omitempty"`   // system boot time (unix seconds)
	CreateTime int64  `json:"create_time,omitempty"` // process creation time (unix millis)
}

func statePath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

func SaveState(dir, name string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Write to temp file then rename for atomic replacement.
	// Prevents corrupt state files if the process is killed mid-write.
	tmp := statePath(dir, name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir, name))
}

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

func RemoveState(dir, name string) {
	os.Remove(statePath(dir, name))
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
