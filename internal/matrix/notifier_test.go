package matrix

import (
	"strings"
	"testing"

	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notifierBot(t *testing.T) (*Bot, *fakeMatrixClient) {
	t.Helper()
	fake := newFakeMatrixClient()
	bot := botForTest(t, fake, newMockServiceManager(), nil)
	return bot, fake
}

func TestNotifier_ServiceEvent(t *testing.T) {
	bot, fake := notifierBot(t)
	NewNotifier(bot).ServiceEvent("myapp", "started")

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "started")
}

func TestNotifier_DeployStarted(t *testing.T) {
	bot, fake := notifierBot(t)
	NewNotifier(bot).DeployStarted("myapp")

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
	assert.Contains(t, body, "Deploying")
	assert.Contains(t, body, "myapp")
}

func TestNotifier_DeploySucceeded(t *testing.T) {
	bot, fake := notifierBot(t)
	NewNotifier(bot).DeploySucceeded("myapp", "build output")

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
	assert.Contains(t, body, "succeeded")
	assert.Contains(t, body, "myapp")
}

func TestNotifier_DeployFailed(t *testing.T) {
	bot, fake := notifierBot(t)
	NewNotifier(bot).DeployFailed("myapp", "build", "error output")

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
	assert.Contains(t, body, "failed")
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "build")
	assert.Contains(t, body, "error output")
}

func TestNotifier_DeployFailed_LargeOutputTruncated(t *testing.T) {
	bot, fake := notifierBot(t)

	// Build ~50 KB of "x\n", plus a distinctive tail that must survive
	// truncation since real failures put the error at the end.
	const tailMarker = "===FINAL_ERROR_MARKER_AT_TAIL==="
	var b strings.Builder
	for i := 0; i < 25000; i++ {
		b.WriteString("x\n")
	}
	b.WriteString(tailMarker)
	output := b.String()

	NewNotifier(bot).DeployFailed("slurp", "go test ./...", output)

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])

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
	bot, fake := notifierBot(t)
	NewNotifier(bot).WebhookReceived("myapp", service.WebhookInfo{
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

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
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
	bot, fake := notifierBot(t)
	NewNotifier(bot).WebhookReceived("myapp", service.WebhookInfo{
		Repo:   "acme/myapp",
		Branch: "main",
		Pusher: "alice",
	})

	sends := fake.getSends()
	require.Len(t, sends, 1)
	body := messageBody(t, sends[0])
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "main")
	assert.Contains(t, body, "alice")
}
