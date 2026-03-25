// task/state.go
package task

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	return os.WriteFile(statePath(dir, name), data, 0644)
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
