package service

import (
	"sync"
	"testing"
)

// recordingNotifier captures all calls for assertion. Thread-safe.
type recordingNotifier struct {
	mu              sync.Mutex
	serviceEvents   []notifierCall
	deployStarted   []string
	deploySucceeded []notifierCall
	deployFailed    []notifierCall
}

type notifierCall struct {
	name, a, b string
}

func (r *recordingNotifier) ServiceEvent(name, event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceEvents = append(r.serviceEvents, notifierCall{name: name, a: event})
}

func (r *recordingNotifier) DeployStarted(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployStarted = append(r.deployStarted, name)
}

func (r *recordingNotifier) DeploySucceeded(name, output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deploySucceeded = append(r.deploySucceeded, notifierCall{name: name, a: output})
}

func (r *recordingNotifier) DeployFailed(name, step, output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployFailed = append(r.deployFailed, notifierCall{name: name, a: step, b: output})
}

// Thread-safe accessors for reading from test goroutines.

func (r *recordingNotifier) getServiceEvents() []notifierCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]notifierCall, len(r.serviceEvents))
	copy(cp, r.serviceEvents)
	return cp
}

func (r *recordingNotifier) getDeployStarted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.deployStarted))
	copy(cp, r.deployStarted)
	return cp
}

func (r *recordingNotifier) getDeploySucceeded() []notifierCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]notifierCall, len(r.deploySucceeded))
	copy(cp, r.deploySucceeded)
	return cp
}

func (r *recordingNotifier) getDeployFailed() []notifierCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]notifierCall, len(r.deployFailed))
	copy(cp, r.deployFailed)
	return cp
}

func TestMultiNotifierServiceEvent(t *testing.T) {
	r1 := &recordingNotifier{}
	r2 := &recordingNotifier{}
	multi := MultiNotifier{r1, r2}

	multi.ServiceEvent("svc", "started")

	if len(r1.serviceEvents) != 1 || r1.serviceEvents[0].name != "svc" || r1.serviceEvents[0].a != "started" {
		t.Fatalf("r1 got %+v", r1.serviceEvents)
	}
	if len(r2.serviceEvents) != 1 || r2.serviceEvents[0].name != "svc" || r2.serviceEvents[0].a != "started" {
		t.Fatalf("r2 got %+v", r2.serviceEvents)
	}
}

func TestMultiNotifierDeployStarted(t *testing.T) {
	r1 := &recordingNotifier{}
	r2 := &recordingNotifier{}
	multi := MultiNotifier{r1, r2}

	multi.DeployStarted("svc")

	if len(r1.deployStarted) != 1 || r1.deployStarted[0] != "svc" {
		t.Fatalf("r1 got %+v", r1.deployStarted)
	}
	if len(r2.deployStarted) != 1 || r2.deployStarted[0] != "svc" {
		t.Fatalf("r2 got %+v", r2.deployStarted)
	}
}

func TestMultiNotifierDeploySucceeded(t *testing.T) {
	r1 := &recordingNotifier{}
	r2 := &recordingNotifier{}
	multi := MultiNotifier{r1, r2}

	multi.DeploySucceeded("svc", "output")

	if len(r1.deploySucceeded) != 1 || r1.deploySucceeded[0].name != "svc" || r1.deploySucceeded[0].a != "output" {
		t.Fatalf("r1 got %+v", r1.deploySucceeded)
	}
	if len(r2.deploySucceeded) != 1 {
		t.Fatalf("r2 got %+v", r2.deploySucceeded)
	}
}

func TestMultiNotifierDeployFailed(t *testing.T) {
	r1 := &recordingNotifier{}
	r2 := &recordingNotifier{}
	multi := MultiNotifier{r1, r2}

	multi.DeployFailed("svc", "step1", "error output")

	if len(r1.deployFailed) != 1 || r1.deployFailed[0].name != "svc" || r1.deployFailed[0].a != "step1" || r1.deployFailed[0].b != "error output" {
		t.Fatalf("r1 got %+v", r1.deployFailed)
	}
	if len(r2.deployFailed) != 1 {
		t.Fatalf("r2 got %+v", r2.deployFailed)
	}
}

func TestMultiNotifierEmpty(t *testing.T) {
	multi := MultiNotifier{}
	// Should not panic
	multi.ServiceEvent("svc", "started")
	multi.DeployStarted("svc")
	multi.DeploySucceeded("svc", "out")
	multi.DeployFailed("svc", "step", "err")
}

func TestNopNotifier(t *testing.T) {
	n := NopNotifier{}
	// Should not panic
	n.ServiceEvent("svc", "started")
	n.DeployStarted("svc")
	n.DeploySucceeded("svc", "out")
	n.DeployFailed("svc", "step", "err")
}
