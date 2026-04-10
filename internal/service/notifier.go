package service

import (
	"fmt"
	"strings"
)

// WebhookInfo carries details about a received webhook for notifications.
type WebhookInfo struct {
	Repo      string
	Branch    string
	Compare   string
	Pusher    string
	CommitID  string
	CommitMsg string
	CommitURL string
	Author    string
	Timestamp string
}

// ShortSHA returns the first 7 characters of the commit SHA, or the full ID
// if it's shorter than 7 characters.
func (w WebhookInfo) ShortSHA() string {
	if len(w.CommitID) < 7 {
		return w.CommitID
	}
	return w.CommitID[:7]
}

// ShortMessage returns the first line of the commit message, truncated to 300
// characters with "..." appended if truncation occurred.
func (w WebhookInfo) ShortMessage() string {
	msg := w.CommitMsg
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const maxLen = 300
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "..."
	}
	return msg
}

// FormatMessage builds a Markdown message describing this webhook event.
// wrappedName is the service name already wrapped in the target platform's
// code/bold syntax (e.g. "`svc`" for Mattermost, "**svc**" for Discord).
// The rest of the formatting (Markdown links, blockquotes, code spans) is
// compatible with both renderers.
func (w WebhookInfo) FormatMessage(wrappedName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Webhook for %s: ", wrappedName)

	if w.CommitID == "" {
		fmt.Fprintf(&b, "push to `%s`", w.Branch)
		if w.Pusher != "" {
			fmt.Fprintf(&b, " by %s", w.Pusher)
		}
		return b.String()
	}

	short := w.ShortSHA()
	if w.CommitURL != "" {
		fmt.Fprintf(&b, "[%s@%s](%s)", w.Repo, short, w.CommitURL)
	} else {
		fmt.Fprintf(&b, "%s@%s", w.Repo, short)
	}
	fmt.Fprintf(&b, " on `%s`", w.Branch)
	if w.Author != "" {
		fmt.Fprintf(&b, " by %s", w.Author)
	}

	if msg := w.ShortMessage(); msg != "" {
		fmt.Fprintf(&b, "\n> %s", msg)
	}

	if w.Compare != "" {
		fmt.Fprintf(&b, "\n[compare](%s)", w.Compare)
	}

	return b.String()
}

// Notifier receives service lifecycle and deploy events.
type Notifier interface {
	ServiceEvent(name, event string)
	DeployStarted(name string)
	DeploySucceeded(name, output string)
	DeployFailed(name, step, output string)
	WebhookReceived(name string, info WebhookInfo)
}

// MultiNotifier fans out events to multiple notifiers.
type MultiNotifier []Notifier

// ServiceEvent notifies all registered notifiers.
func (m MultiNotifier) ServiceEvent(name, event string) {
	for _, n := range m {
		n.ServiceEvent(name, event)
	}
}

// DeployStarted notifies all registered notifiers.
func (m MultiNotifier) DeployStarted(name string) {
	for _, n := range m {
		n.DeployStarted(name)
	}
}

// DeploySucceeded notifies all registered notifiers.
func (m MultiNotifier) DeploySucceeded(name, output string) {
	for _, n := range m {
		n.DeploySucceeded(name, output)
	}
}

// DeployFailed notifies all registered notifiers.
func (m MultiNotifier) DeployFailed(name, step, output string) {
	for _, n := range m {
		n.DeployFailed(name, step, output)
	}
}

// WebhookReceived notifies all registered notifiers.
func (m MultiNotifier) WebhookReceived(name string, info WebhookInfo) {
	for _, n := range m {
		n.WebhookReceived(name, info)
	}
}

// NopNotifier discards all events. Useful as a default or in tests.
type NopNotifier struct{}

func (NopNotifier) ServiceEvent(string, string)         {}
func (NopNotifier) DeployStarted(string)                {}
func (NopNotifier) DeploySucceeded(string, string)      {}
func (NopNotifier) DeployFailed(string, string, string) {}
func (NopNotifier) WebhookReceived(string, WebhookInfo) {}
