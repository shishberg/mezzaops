package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State represents the persisted state of a managed service.
type State struct {
	Status      string          `json:"status"`
	LastDeploy  time.Time       `json:"last_deploy,omitzero"`
	LastRestart time.Time       `json:"last_restart,omitzero"`
	LastResult  string          `json:"last_result,omitempty"`
	LastOutput  string          `json:"last_output,omitempty"`
	FailedStep  string          `json:"failed_step,omitempty"`
	Backend     json.RawMessage `json:"backend,omitempty"`
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
// Returns the parsed State, the raw JSON bytes (for backend migration), and any error.
func LoadState(dir, name string) (State, json.RawMessage, error) {
	data, err := os.ReadFile(statePath(dir, name))
	if err != nil {
		return State{}, nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, nil, err
	}
	return s, json.RawMessage(data), nil
}

// RemoveState deletes the state file.
func RemoveState(dir, name string) {
	_ = os.Remove(statePath(dir, name))
}
