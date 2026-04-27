package matrix

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSender captures messages handed to PostMessage so notifier tests can
// assert what the Notifier sends without standing up a Bot or mautrix client.
type fakeSender struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeSender) PostMessage(_ context.Context, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, message)
}

func (f *fakeSender) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.messages))
	copy(out, f.messages)
	return out
}

func TestNotifier_ServiceEvent(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).ServiceEvent("myapp", "started")

	sent := fake.sent()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0], "myapp")
	assert.Contains(t, sent[0], "started")
}

func TestNotifier_DeployStarted(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).DeployStarted("myapp")

	sent := fake.sent()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0], "Deploying")
	assert.Contains(t, sent[0], "myapp")
}

func TestNotifier_DeploySucceeded(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).DeploySucceeded("myapp", "build output")

	sent := fake.sent()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0], "succeeded")
	assert.Contains(t, sent[0], "myapp")
}

func TestNotifier_DeployFailed(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).DeployFailed("myapp", "build", "error output")

	sent := fake.sent()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0], "failed")
	assert.Contains(t, sent[0], "myapp")
	assert.Contains(t, sent[0], "build")
	assert.Contains(t, sent[0], "error output")
}

func TestNotifier_DeployFailed_LargeOutputTruncated(t *testing.T) {
	fake := &fakeSender{}

	// Build ~50 KB of "x\n", plus a distinctive tail that must survive
	// truncation since real failures put the error at the end.
	const tailMarker = "===FINAL_ERROR_MARKER_AT_TAIL==="
	var b strings.Builder
	for i := 0; i < 25000; i++ {
		b.WriteString("x\n")
	}
	b.WriteString(tailMarker)
	output := b.String()

	NewNotifier(fake).DeployFailed("slurp", "go test ./...", output)

	sent := fake.sent()
	require.Len(t, sent, 1)
	body := sent[0]

	runes := len([]rune(body))
	assert.LessOrEqual(t, runes, matrixMaxRunes,
		"posted message exceeds matrix rune budget: %d > %d", runes, matrixMaxRunes)

	assert.Contains(t, body, "truncated")
	assert.Contains(t, body, tailMarker)
	assert.Contains(t, body, "```\n")
	assert.True(t, strings.HasSuffix(body, "\n```"),
		"message should end with closing code fence; got tail %q", tailRunes(body, 30))
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func TestNotifier_WebhookReceived_Full(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).WebhookReceived("myapp", service.WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		Compare:   "https://github.com/acme/myapp/compare/abc...def",
		Pusher:    "alice",
		CommitID:  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		CommitMsg: "feat: add a thing\n\nmore details here",
		CommitURL: "https://github.com/acme/myapp/commit/deadbeef",
		Author:    "Alice Author",
		Timestamp: "2026-04-10T12:34:56Z",
	})

	sent := fake.sent()
	require.Len(t, sent, 1)
	body := sent[0]
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "acme/myapp")
	assert.Contains(t, body, "main")
	assert.Contains(t, body, "deadbee")
	assert.Contains(t, body, "https://github.com/acme/myapp/commit/deadbeef")
	assert.Contains(t, body, "feat: add a thing")
	assert.NotContains(t, body, "more details here")
	assert.Contains(t, body, "Alice Author")
	assert.Contains(t, body, "https://github.com/acme/myapp/compare/abc...def")
}

func TestNotifier_WebhookReceived_NoHeadCommit(t *testing.T) {
	fake := &fakeSender{}
	NewNotifier(fake).WebhookReceived("myapp", service.WebhookInfo{
		Repo:   "acme/myapp",
		Branch: "main",
		Pusher: "alice",
	})

	sent := fake.sent()
	require.Len(t, sent, 1)
	body := sent[0]
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "main")
	assert.Contains(t, body, "alice")
}
