package service

// Notifier receives service lifecycle and deploy events.
type Notifier interface {
	ServiceEvent(name, event string)
	DeployStarted(name string)
	DeploySucceeded(name, output string)
	DeployFailed(name, step, output string)
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

// NopNotifier discards all events. Useful as a default or in tests.
type NopNotifier struct{}

func (NopNotifier) ServiceEvent(string, string)         {}
func (NopNotifier) DeployStarted(string)                {}
func (NopNotifier) DeploySucceeded(string, string)      {}
func (NopNotifier) DeployFailed(string, string, string) {}
