package service

import (
	"fmt"
	"sort"
	"strings"
)

// ServiceManager is the slice of the manager API the chat frontends
// (Mattermost, Matrix, Telegram) use.
type ServiceManager interface {
	Do(name, op string) string
	RequestDeploy(name string) error
	StartAll()
	StopAll()
	Reload() error
	ServiceNames() []string
	CountRunning() (int, int)
	GetAllStates() map[string]ServiceState
}

// ConfirmHandler completes a deploy confirmation initiated by a webhook for a
// service with require_confirmation: true.
type ConfirmHandler interface {
	Confirm(service string) bool
}

// FormatStatusOverview formats service states as a markdown list, sorted by
// service name so the output is deterministic.
func FormatStatusOverview(states map[string]ServiceState) string {
	if len(states) == 0 {
		return "No services configured."
	}
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString("**Service Status**\n")
	for _, name := range names {
		state := states[name]
		fmt.Fprintf(&sb, "- **%s**: %s", name, state.Status)
		if state.LastResult != "" {
			fmt.Fprintf(&sb, " (last: %s)", state.LastResult)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
