package telegram

import (
	"strings"
	"testing"

	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notifierBot(t *testing.T) (*Bot, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(t)
	return newTestBot(t, api, nil, nil), api
}

func TestNotifier_ServiceEvent(t *testing.T) {
	bot, api := notifierBot(t)
	NewNotifier(bot).ServiceEvent("myapp", "started")

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	assert.Equal(t, "<code>myapp</code> started.", texts[0])
}

func TestNotifier_DeployStarted(t *testing.T) {
	bot, api := notifierBot(t)
	NewNotifier(bot).DeployStarted("myapp")

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	assert.Equal(t, "Deploying <code>myapp</code>...", texts[0])
}

func TestNotifier_DeploySucceeded(t *testing.T) {
	bot, api := notifierBot(t)
	NewNotifier(bot).DeploySucceeded("myapp", "build output")

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	assert.Equal(t, "Deploy of <code>myapp</code> succeeded.", texts[0])
}

func TestNotifier_DeployFailed(t *testing.T) {
	bot, api := notifierBot(t)
	NewNotifier(bot).DeployFailed("myapp", "build", "error <output>")

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	assert.Equal(t, "Deploy of <code>myapp</code> failed at step <code>build</code>.\n<pre>error &lt;output&gt;</pre>", texts[0])
}

func TestNotifier_DeployFailed_LargeOutputTruncated(t *testing.T) {
	bot, api := notifierBot(t)

	const tailMarker = "===FINAL_ERROR_MARKER_AT_TAIL==="
	output := strings.Repeat("x\n", 25000) + tailMarker

	NewNotifier(bot).DeployFailed("slurp", "go test ./...", output)

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	body := texts[0]

	// Tags do not count towards Telegram's limit; strip them before counting.
	visible := strings.NewReplacer("<pre>", "", "</pre>", "", "<code>", "", "</code>", "").Replace(body)
	assert.LessOrEqual(t, len([]rune(visible)), telegramMaxRunes)
	assert.Contains(t, body, "truncated")
	assert.Contains(t, body, tailMarker)
	assert.True(t, strings.HasSuffix(body, tailMarker+"</pre>"),
		"message should keep the fence so it renders as a code block; got tail %q", body[len(body)-40:])
	assert.Contains(t, body, "<pre>")
}

func TestNotifier_DeployFailed_BackticksInOutputDoNotBreakFence(t *testing.T) {
	bot, api := notifierBot(t)
	NewNotifier(bot).DeployFailed("myapp", "build", "a ``` b")

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "<pre>a `` b</pre>")
}

func TestNotifier_WebhookReceived(t *testing.T) {
	bot, api := notifierBot(t)
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

	texts := api.sentTexts()
	require.Len(t, texts, 1)
	body := texts[0]
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "acme/myapp")
	assert.Contains(t, body, "deadbee")
	assert.Contains(t, body, "https://github.com/acme/myapp/commit/deadbeef")
	assert.Contains(t, body, "feat: add a thing")
	assert.Contains(t, body, "Alice Author")
}
