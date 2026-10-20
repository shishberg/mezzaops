package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State represents the persisted state of a managed service.
//
// Status is the live status as last observed (running, stopped, deploying,
// failed). It is overwritten on every save based on the backend's
// current status, so an unexpected process exit looks identical to an
// operator-initiated stop in this field.
//
// DesiredStatus is the operator's last expressed intent. It is set by
// the "start" and "stop" ops and never overwritten by live status.
// C1's autostart-on-cold-boot consults this field (not Status) to
// decide whether the operator explicitly stopped the service, so a
// crashed process is correctly distinguished from an intentional
// stop. An empty DesiredStatus means "no explicit intent recorded";
// in that case autostart proceeds.
type State struct {
	Status        string          `json:"status"`
	DesiredStatus string          `json:"desired_status,omitempty"`
	LastDeploy    time.Time       `json:"last_deploy,omitzero"`
	LastRestart   time.Time       `json:"last_restart,omitzero"`
	LastResult    string          `json:"last_result,omitempty"`
	LastOutput    string          `json:"last_output,omitempty"`
	FailedStep    string          `json:"failed_step,omitempty"`
	Backend       json.RawMessage `json:"backend,omitempty"`
	// BootTime is the host boot time (unix seconds) when the state was last
	// saved. A different value on the next load means the host rebooted.
	BootTime int64 `json:"boot_time,omitempty"`
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
