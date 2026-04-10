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
	webhookReceived []webhookCall
}

type webhookCall struct {
	name string
	info WebhookInfo
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

func (r *recordingNotifier) WebhookReceived(name string, info WebhookInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webhookReceived = append(r.webhookReceived, webhookCall{name: name, info: info})
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

func (r *recordingNotifier) getWebhookReceived() []webhookCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]webhookCall, len(r.webhookReceived))
	copy(cp, r.webhookReceived)
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
	multi.WebhookReceived("svc", WebhookInfo{})
}

func TestMultiNotifierWebhookReceived(t *testing.T) {
	r1 := &recordingNotifier{}
	r2 := &recordingNotifier{}
	multi := MultiNotifier{r1, r2}

	info := WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		Compare:   "https://example/compare",
		Pusher:    "alice",
		CommitID:  "deadbeef",
		CommitMsg: "fix things",
		CommitURL: "https://example/commit",
		Author:    "Alice",
		Timestamp: "2026-04-10T00:00:00Z",
	}
	multi.WebhookReceived("svc", info)

	got1 := r1.getWebhookReceived()
	if len(got1) != 1 || got1[0].name != "svc" || got1[0].info != info {
		t.Fatalf("r1 got %+v", got1)
	}
	got2 := r2.getWebhookReceived()
	if len(got2) != 1 || got2[0].name != "svc" || got2[0].info != info {
		t.Fatalf("r2 got %+v", got2)
	}
}

func TestNopNotifier(t *testing.T) {
	n := NopNotifier{}
	// Should not panic
	n.ServiceEvent("svc", "started")
	n.DeployStarted("svc")
	n.DeploySucceeded("svc", "out")
	n.DeployFailed("svc", "step", "err")
	n.WebhookReceived("svc", WebhookInfo{})
}
