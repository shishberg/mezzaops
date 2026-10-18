package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- defuseFences ---

func TestDefuseFences_NeverLeavesAFence(t *testing.T) {
	for _, n := range []int{3, 4, 5, 6, 7, 12} {
		in := "a" + strings.Repeat("`", n) + "b"
		assert.NotContains(t, defuseFences(in), "```", "%d backticks", n)
	}
}

func TestDefuseFences_LeavesShortRunsAlone(t *testing.T) {
	assert.Equal(t, "a `b` ``c``", defuseFences("a `b` ``c``"))
}

func TestDefuseFences_Idempotent(t *testing.T) {
	once := defuseFences("x````````y")
	assert.Equal(t, once, defuseFences(once))
}

// --- UTF-16 truncation ---

func TestUTF16Len(t *testing.T) {
	assert.Equal(t, 5, utf16Len("hello"))
	assert.Equal(t, 2, utf16Len("😀"))
	assert.Equal(t, 1, utf16Len("é"))
	assert.Equal(t, 5, utf16Len("a😀b"+"c"))
}

func TestTruncateTailUTF16_NoOpWithinBudget(t *testing.T) {
	assert.Equal(t, "hello", truncateTailUTF16("hello", 5))
	assert.Equal(t, "😀😀", truncateTailUTF16("😀😀", 4))
}

func TestTruncateTailUTF16_EmojiRespectsUnitBudget(t *testing.T) {
	in := strings.Repeat("😀", 3000) + "TAIL"
	got := truncateTailUTF16(in, 4000)
	assert.LessOrEqual(t, utf16Len(got), 4000)
	assert.True(t, utf8.ValidString(got))
	assert.Contains(t, got, "truncated")
	assert.True(t, strings.HasSuffix(got, "TAIL"))
	// Rune-based truncation to 4000 runes would have been 8000 units.
	assert.Greater(t, utf16Len(got), 3500, "should use most of the budget")
}

func TestTruncateTailUTF16_NonPositiveBudget(t *testing.T) {
	assert.Equal(t, "", truncateTailUTF16("hello", 0))
	assert.Equal(t, "", truncateTailUTF16("hello", -3))
}

func TestPostMessage_EmojiTruncatedByUTF16Units(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), strings.Repeat("😀", 3500))
	texts := flush(t, b, api)
	require.Len(t, texts, 1)
	assert.LessOrEqual(t, utf16Len(texts[0]), telegramMaxUTF16Units)
}

func TestDeployFailed_EmojiOutputWithinUnitBudget(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	NewNotifier(b).DeployFailed("svc", "build", strings.Repeat("😀", 3500)+"END")
	texts := flush(t, b, api)
	require.Len(t, texts, 1)
	visible := strings.NewReplacer("<pre>", "", "</pre>", "", "<code>", "", "</code>", "").Replace(texts[0])
	assert.LessOrEqual(t, utf16Len(visible), telegramMaxUTF16Units)
	assert.Contains(t, texts[0], "END</pre>")
}

func TestDispatch_LogsEmojiWithinUnitBudget(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = strings.Repeat("😀", 3500) + "END"
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "logs", Service: "app1"})
	assert.LessOrEqual(t, utf16Len(got), telegramMaxUTF16Units)
	assert.True(t, strings.HasSuffix(got, "END\n```"))
}

// --- Queue ---

func TestQueue_PreservesOrder(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	for i := 0; i < 20; i++ {
		b.PostMessage(context.Background(), fmt.Sprintf("m%02d", i))
	}
	got := flush(t, b, api)
	require.Len(t, got, 20)
	for i, text := range got {
		assert.Equal(t, fmt.Sprintf("m%02d", i), text)
	}
}

func TestQueue_RepliesAndNotificationsShareOrder(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "before")
	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/status app1", false)))
	b.PostMessage(context.Background(), "after")
	assert.Equal(t, []string{"before", "ok", "after"}, flush(t, b, api))
}

func TestQueue_PostMessageDoesNotBlockOnSlowAPI(t *testing.T) {
	api := newFakeAPI(t)
	release := make(chan struct{})
	api.handle("sendMessage", func(apiCall) (int, string) {
		<-release
		return 200, `{"ok":true,"result":true}`
	})
	b := newSendingBot(t, api, nil, nil)
	t.Cleanup(func() { close(release) })

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.PostMessage(context.Background(), "x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PostMessage blocked on a slow Bot API")
	}
}

func TestQueue_MessagesBeforeRunAreSentOnceRunning(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "early 1")
	b.PostMessage(context.Background(), "early 2")
	assert.Empty(t, api.callsTo("sendMessage"), "New/PostMessage must not do I/O before Run")

	startRun(t, b)
	waitFor(t, "queued messages", func() bool { return len(api.sentTexts()) == 2 })
	assert.Equal(t, []string{"early 1", "early 2"}, api.sentTexts())
}

func TestQueue_FullQueueDropsAndLogs(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil) // sender not running, so nothing drains
	for i := 0; i < outboxSize+5; i++ {
		b.PostMessage(context.Background(), fmt.Sprintf("m%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.sendLoop(ctx) }()
	waitFor(t, "buffered messages", func() bool { return len(api.sentTexts()) == outboxSize })
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	texts := api.sentTexts()
	assert.Len(t, texts, outboxSize, "overflow is dropped, not blocked on")
	assert.Equal(t, "m0", texts[0])
	assert.Equal(t, fmt.Sprintf("m%d", outboxSize-1), texts[len(texts)-1])
}

func TestQueue_SenderStopsWhenRunContextCancelled(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)
	stop := startRun(t, b)
	<-b.Ready()
	stop()

	b.PostMessage(context.Background(), "late")
	time.Sleep(40 * time.Millisecond)
	assert.Empty(t, api.sentTexts(), "nothing is sent after Run has returned")
}

// --- 429 handling ---

type sleepRecorder struct {
	mu sync.Mutex
	ds []time.Duration
}

func (r *sleepRecorder) sleep(_ context.Context, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ds = append(r.ds, d)
}

func (r *sleepRecorder) durations() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.ds...)
}

func rateLimited(retryAfter int) string {
	return fmt.Sprintf(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after %d","parameters":{"retry_after":%d}}`, retryAfter, retryAfter)
}

func TestSend_429RetriesAfterRetryAfter(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	n := 0
	api.handle("sendMessage", func(apiCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return 429, rateLimited(3)
		}
		return 200, `{"ok":true,"result":true}`
	})
	rec := &sleepRecorder{}
	b := newTestBot(t, api, nil, nil)
	b.sleep = rec.sleep
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.sendLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	b.PostMessage(context.Background(), "hello")
	waitFor(t, "retry", func() bool { return len(api.callsTo("sendMessage")) == 2 })
	assert.Equal(t, []time.Duration{3 * time.Second}, rec.durations())
	assert.Equal(t, []string{"hello", "hello"}, api.sentTexts(), "the same message is retried")
}

func TestSend_429RetryAfterIsCapped(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	n := 0
	api.handle("sendMessage", func(apiCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return 429, rateLimited(500)
		}
		return 200, `{"ok":true,"result":true}`
	})
	rec := &sleepRecorder{}
	b := newTestBot(t, api, nil, nil)
	b.sleep = rec.sleep
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.sendLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	b.PostMessage(context.Background(), "hello")
	waitFor(t, "retry", func() bool { return len(api.callsTo("sendMessage")) == 2 })
	assert.Equal(t, []time.Duration{maxRetryAfter}, rec.durations())
}

func TestSend_429RetriesAreBounded(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("sendMessage", func(c apiCall) (int, string) {
		if c.Body["text"] == "next" {
			return 200, `{"ok":true,"result":true}`
		}
		return 429, rateLimited(1)
	})
	rec := &sleepRecorder{}
	b := newTestBot(t, api, nil, nil)
	b.sleep = rec.sleep
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.sendLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	b.PostMessage(context.Background(), "doomed")
	b.PostMessage(context.Background(), "next")
	waitFor(t, "next message", func() bool {
		for _, text := range api.sentTexts() {
			if text == "next" {
				return true
			}
		}
		return false
	})
	doomed := 0
	for _, text := range api.sentTexts() {
		if text == "doomed" {
			doomed++
		}
	}
	assert.Equal(t, maxSendRetries+1, doomed)
	assert.Len(t, rec.durations(), maxSendRetries)
}

func TestSend_DefaultSleepStopsOnContextCancel(t *testing.T) {
	b := New(Config{Token: testToken, ChatID: testChatID}, newMockServiceManager())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.sleep(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("default sleep ignored ctx cancel")
	}
}
