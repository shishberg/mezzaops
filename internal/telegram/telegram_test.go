package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testToken   = "123456:SECRET-token"
	testChatID  = int64(-1001234567890)
	testBotName = "mezzaops_bot"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *Command
	}{
		{"status no service", "/status", &Command{Action: "status"}},
		{"status with service", "/status myapp", &Command{Action: "status", Service: "myapp"}},
		{"deploy", "/deploy myapp", &Command{Action: "deploy", Service: "myapp"}},
		{"confirm", "/confirm myapp", &Command{Action: "confirm", Service: "myapp"}},
		{"extra whitespace", "  /restart   myapp  ", &Command{Action: "restart", Service: "myapp"}},
		{"extra tokens ignored", "/logs myapp extra", &Command{Action: "logs", Service: "myapp"}},
		{"own bot suffix", "/status@mezzaops_bot", &Command{Action: "status"}},
		{"own bot suffix with service", "/deploy@mezzaops_bot myapp", &Command{Action: "deploy", Service: "myapp"}},
		{"own bot suffix case-insensitive", "/status@MezzaOps_Bot", &Command{Action: "status"}},
		{"other bot suffix", "/status@other_bot", nil},
		{"other bot suffix with service", "/deploy@other_bot myapp", nil},
		{"underscore to hyphen", "/start_all", &Command{Action: "start-all"}},
		{"stop_all", "/stop_all", &Command{Action: "stop-all"}},
		{"hyphen typed by hand", "/start-all", &Command{Action: "start-all"}},
		{"underscore with bot suffix", "/stop_all@mezzaops_bot", &Command{Action: "stop-all"}},
		{"action lowercased", "/Status myapp", &Command{Action: "status", Service: "myapp"}},
		{"service case preserved", "/start MyApp", &Command{Action: "start", Service: "MyApp"}},
		{"unknown action", "/foobar", &Command{Action: "foobar"}},
		{"plain text", "hello there", nil},
		{"slash not first", "hello /status", nil},
		{"bare slash", "/", nil},
		{"bare slash with bot", "/@mezzaops_bot", nil},
		{"empty", "", nil},
		{"whitespace", "   ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseCommand(tt.text, testBotName))
		})
	}
}

func TestMarkdownToHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"escapes", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{"bold", "a **bold** b", "a <b>bold</b> b"},
		{"inline code", "run `ls -l` now", "run <code>ls -l</code> now"},
		{"inline code escaped", "`a<b>&c`", "<code>a&lt;b&gt;&amp;c</code>"},
		{"code content not processed", "`**x** [a](b)`", "<code>**x** [a](b)</code>"},
		{"bold containing code", "**a `b` c**", "<b>a <code>b</code> c</b>"},
		{"bold with escapes", "**a<b**", "<b>a&lt;b</b>"},
		{"link", "[site](https://example.com/x)", `<a href="https://example.com/x">site</a>`},
		{"link escapes url", `[t](http://x/?a=1&b="2")`, `<a href="http://x/?a=1&amp;b=&quot;2&quot;">t</a>`},
		{"link text bold", "[**t**](http://x)", `<a href="http://x"><b>t</b></a>`},
		{"fenced block", "```\nfoo <b>\nbar\n```", "<pre>foo &lt;b&gt;\nbar</pre>"},
		{"fenced block no inline", "```\n**x** `y` [a](b)\n```", "<pre>**x** `y` [a](b)</pre>"},
		{"fenced block info string dropped", "```go\nx := 1\n```", "<pre>x := 1</pre>"},
		{"fence surrounded by text", "before\n```\ncode\n```\nafter", "before\n<pre>code</pre>\nafter"},
		{"blockquote", "> a\n> b", "<blockquote>a\nb</blockquote>"},
		{"blockquote inline", "> **a** `b`", "<blockquote><b>a</b> <code>b</code></blockquote>"},
		{"blockquote then text", "> a\nafter", "<blockquote>a</blockquote>\nafter"},
		{"text then blockquote", "before\n> a", "before\n<blockquote>a</blockquote>"},
		{"two separate quotes", "> a\nmid\n> b", "<blockquote>a</blockquote>\nmid\n<blockquote>b</blockquote>"},
		{"gt without space is not a quote", ">a", "&gt;a"},
		{"unmatched bold", "a ** b", "a ** b"},
		{"unclosed bold", "**bold", "**bold"},
		{"empty bold", "a **** b", "a **** b"},
		{"unclosed backtick", "`open", "`open"},
		{"unclosed link", "[t](u", "[t](u"},
		{"bracket without paren", "[t] (u)", "[t] (u)"},
		{"unmatched opening fence", "```\nlast", "```\nlast"},
		{"unmatched closing fence", "tail\n```", "tail\n```"},
		{"multiline plain", "a\nb", "a\nb"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, markdownToHTML(tt.in))
		})
	}
}

// --- Mocks ---

type mockServiceManager struct {
	mu          sync.Mutex
	lastOp      string
	lastService string
	doResult    string
	deployErr   error
	reloadErr   error
	states      map[string]service.ServiceState
}

func newMockServiceManager() *mockServiceManager {
	return &mockServiceManager{
		doResult: "ok",
		states: map[string]service.ServiceState{
			"app1": {Status: "running"},
			"app2": {Status: "stopped", LastResult: "deploy failed"},
		},
	}
}

func (m *mockServiceManager) Do(name, op string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastService = name
	m.lastOp = op
	return m.doResult
}

func (m *mockServiceManager) RequestDeploy(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastService = name
	m.lastOp = "deploy"
	return m.deployErr
}

func (m *mockServiceManager) StartAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastOp = "start-all"
}

func (m *mockServiceManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastOp = "stop-all"
}

func (m *mockServiceManager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastOp = "reload"
	return m.reloadErr
}

func (m *mockServiceManager) ServiceNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.states))
	for k := range m.states {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *mockServiceManager) CountRunning() (int, int) { return 1, 2 }

func (m *mockServiceManager) GetAllStates() map[string]service.ServiceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastOp = "status"
	return m.states
}

func (m *mockServiceManager) getLastOp() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastOp
}

func (m *mockServiceManager) getLastService() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastService
}

type mockConfirmHandler struct {
	mu          sync.Mutex
	lastService string
	result      bool
}

func (h *mockConfirmHandler) Confirm(svc string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastService = svc
	return h.result
}

func (h *mockConfirmHandler) getLastService() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastService
}

// apiCall is one request received by fakeAPI.
type apiCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeAPI is an httptest.Server standing in for the Telegram Bot API. Each
// method has a default success response; tests override per-method behaviour
// through the handlers map. getUpdates mimics Telegram's acknowledgement:
// updates with update_id below the request's offset are dropped, the rest are
// (re)delivered.
type fakeAPI struct {
	*httptest.Server

	mu       sync.Mutex
	calls    []apiCall
	updates  []map[string]any
	handlers map[string]func(call apiCall) (int, string)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{handlers: make(map[string]func(apiCall) (int, string))}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := apiCall{Path: r.URL.Path}
	if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 {
		call.Method = r.URL.Path[i+1:]
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &call.Body)
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handlers[call.Method]
	var pending []map[string]any
	if call.Method == "getUpdates" {
		offset, _ := call.Body["offset"].(float64)
		if offset < 0 {
			// Telegram returns only the newest update for a negative offset.
			if n := len(f.updates); n > 0 {
				pending = append(pending, f.updates[n-1])
			}
		} else {
			for _, u := range f.updates {
				if u["update_id"].(float64) >= offset {
					pending = append(pending, u)
				}
			}
		}
	}
	f.mu.Unlock()

	status, body := http.StatusOK, `{"ok":true,"result":true}`
	switch {
	case handler != nil:
		status, body = handler(call)
	case call.Method == "getMe":
		body = `{"ok":true,"result":{"id":42,"is_bot":true,"username":"` + testBotName + `"}}`
	case call.Method == "getUpdates":
		if len(pending) == 0 {
			select {
			case <-time.After(10 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "result": append([]map[string]any{}, pending...)})
		body = string(b)
	case call.Method == "sendMessage":
		body = `{"ok":true,"result":{"message_id":1}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeAPI) handle(method string, h func(call apiCall) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

func (f *fakeAPI) queueUpdate(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, u)
}

func (f *fakeAPI) callsTo(method string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// sentTexts returns the text field of each sendMessage call, in order.
func (f *fakeAPI) sentTexts() []string {
	var out []string
	for _, c := range f.callsTo("sendMessage") {
		s, _ := c.Body["text"].(string)
		out = append(out, s)
	}
	return out
}

// updateJSON builds a getUpdates update as Telegram would deliver it.
func updateJSON(id int, chatID int64, date time.Time, text string, isBot bool) map[string]any {
	return map[string]any{
		"update_id": float64(id),
		"message": map[string]any{
			"message_id": float64(id),
			"date":       float64(date.Unix()),
			"text":       text,
			"chat":       map[string]any{"id": float64(chatID)},
			"from":       map[string]any{"id": float64(7), "is_bot": isBot, "username": "someone"},
		},
	}
}

// withSenderChat marks the update as sent on behalf of a chat, as Telegram
// does for anonymous group admins.
func withSenderChat(u map[string]any, chatID int64) map[string]any {
	u["message"].(map[string]any)["sender_chat"] = map[string]any{"id": float64(chatID)}
	return u
}

// withFrom replaces the update's sender.
func withFrom(u map[string]any, from map[string]any) map[string]any {
	u["message"].(map[string]any)["from"] = from
	return u
}

// withTopic marks the update as sent inside a forum topic.
func withTopic(u map[string]any, threadID int64) map[string]any {
	m := u["message"].(map[string]any)
	m["is_topic_message"] = true
	m["message_thread_id"] = float64(threadID)
	return u
}

// newSendingBot is newTestBot with the sender goroutine running, so
// PostMessage and command replies reach the fake API. It is stopped on test
// cleanup.
func newSendingBot(t *testing.T, api *fakeAPI, mgr service.ServiceManager, ch service.ConfirmHandler) *Bot {
	t.Helper()
	b := newTestBot(t, api, mgr, ch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.sendLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return b
}

// flush waits until everything queued so far has been sent by posting a
// sentinel behind it, and returns the texts sent before the sentinel.
func flush(t *testing.T, b *Bot, api *fakeAPI) []string {
	t.Helper()
	const sentinel = "flush-sentinel"
	b.PostMessage(context.Background(), sentinel)
	var texts []string
	waitFor(t, "flush sentinel", func() bool {
		texts = api.sentTexts()
		return len(texts) > 0 && texts[len(texts)-1] == sentinel
	})
	return texts[:len(texts)-1]
}

// newTestBot builds a Bot pointed at api with fast poll/backoff timings and
// the bot username pre-populated so handlers can be exercised without Run.
func newTestBot(t *testing.T, api *fakeAPI, mgr service.ServiceManager, ch service.ConfirmHandler) *Bot {
	t.Helper()
	if mgr == nil {
		mgr = newMockServiceManager()
	}
	b := New(Config{Token: testToken, ChatID: testChatID, APIURL: api.URL}, mgr)
	b.confirm = ch
	b.username = testBotName
	b.pollTimeout = 20 * time.Millisecond
	b.backoffInitial = time.Millisecond
	b.backoffMax = 5 * time.Millisecond
	b.sleep = func(context.Context, time.Duration) {}
	return b
}

func decodeUpdate(t *testing.T, m map[string]any) update {
	t.Helper()
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var u update
	require.NoError(t, json.Unmarshal(raw, &u))
	return u
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startRun runs b.Run in a goroutine and returns a stop function that cancels
// it and waits for it to return.
func startRun(t *testing.T, b *Bot) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// --- Dispatch ---

func TestDispatch_StatusOverview(t *testing.T) {
	mgr := newMockServiceManager()
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "status"})
	assert.Contains(t, got, "**Service Status**")
	assert.Contains(t, got, "- **app1**: running")
	assert.Contains(t, got, "- **app2**: stopped (last: deploy failed)")
	assert.Less(t, strings.Index(got, "app1"), strings.Index(got, "app2"))
}

func TestDispatch_StatusOverviewEmpty(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.states = map[string]service.ServiceState{}
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	assert.Equal(t, "No services configured.", b.dispatchCommand(&Command{Action: "status"}))
}

func TestDispatch_StatusSingleService(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = "app1 is running"
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "status", Service: "app1"})
	assert.Equal(t, "app1 is running", got)
	assert.Equal(t, "status", mgr.getLastOp())
	assert.Equal(t, "app1", mgr.getLastService())
}

func TestDispatch_SimpleOps(t *testing.T) {
	for _, op := range []string{"start", "stop", "restart", "pull"} {
		t.Run(op, func(t *testing.T) {
			mgr := newMockServiceManager()
			mgr.doResult = "did " + op
			b := newTestBot(t, newFakeAPI(t), mgr, nil)
			got := b.dispatchCommand(&Command{Action: op, Service: "app1"})
			assert.Equal(t, "did "+op, got)
			assert.Equal(t, op, mgr.getLastOp())
			assert.Equal(t, "app1", mgr.getLastService())
		})
	}
}

func TestDispatch_Deploy(t *testing.T) {
	mgr := newMockServiceManager()
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "deploy", Service: "app1"})
	assert.Equal(t, "Deploy requested for **app1**.", got)
	assert.Equal(t, "deploy", mgr.getLastOp())
	assert.Equal(t, "app1", mgr.getLastService())
}

func TestDispatch_DeployError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.deployErr = errors.New("boom")
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	assert.Equal(t, "Deploy error: boom", b.dispatchCommand(&Command{Action: "deploy", Service: "app1"}))
}

func TestDispatch_Confirm(t *testing.T) {
	ch := &mockConfirmHandler{result: true}
	b := newTestBot(t, newFakeAPI(t), nil, ch)
	got := b.dispatchCommand(&Command{Action: "confirm", Service: "app1"})
	assert.Equal(t, "Confirmed deploy for **app1**.", got)
	assert.Equal(t, "app1", ch.getLastService())
}

func TestDispatch_ConfirmNoPending(t *testing.T) {
	ch := &mockConfirmHandler{result: false}
	b := newTestBot(t, newFakeAPI(t), nil, ch)
	got := b.dispatchCommand(&Command{Action: "confirm", Service: "app1"})
	assert.Equal(t, "No pending deploy confirmation for **app1** (or it expired).", got)
}

func TestDispatch_ConfirmNoHandler(t *testing.T) {
	b := newTestBot(t, newFakeAPI(t), nil, nil)
	assert.Equal(t, "Confirm handler not configured.", b.dispatchCommand(&Command{Action: "confirm", Service: "app1"}))
}

func TestDispatch_Reload(t *testing.T) {
	mgr := newMockServiceManager()
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	assert.Equal(t, "Config reloaded.", b.dispatchCommand(&Command{Action: "reload"}))
	assert.Equal(t, "reload", mgr.getLastOp())
}

func TestDispatch_ReloadError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.reloadErr = errors.New("bad yaml")
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	assert.Equal(t, "Reload error: bad yaml", b.dispatchCommand(&Command{Action: "reload"}))
}

func TestDispatch_StartAllStopAll(t *testing.T) {
	mgr := newMockServiceManager()
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	assert.Equal(t, "All services starting.", b.dispatchCommand(&Command{Action: "start-all"}))
	assert.Equal(t, "start-all", mgr.getLastOp())
	assert.Equal(t, "All services stopping.", b.dispatchCommand(&Command{Action: "stop-all"}))
	assert.Equal(t, "stop-all", mgr.getLastOp())
}

func TestDispatch_Unknown(t *testing.T) {
	b := newTestBot(t, newFakeAPI(t), nil, nil)
	got := b.dispatchCommand(&Command{Action: "frobnicate"})
	assert.Contains(t, got, `Unknown command: "frobnicate"`)
	for _, c := range []string{"/status", "/start", "/stop", "/restart", "/logs", "/pull",
		"/deploy", "/confirm", "/reload", "/start_all", "/stop_all"} {
		assert.Contains(t, got, c)
	}
}

func TestDispatch_LogsFenced(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = "line1\nline2"
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "logs", Service: "app1"})
	assert.Equal(t, "app1:\n```\nline1\nline2\n```", got)
	assert.Equal(t, "logs", mgr.getLastOp())
	assert.Equal(t, "app1", mgr.getLastService())
}

func TestDispatch_LogsBackticksDefused(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = "a ``` b"
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "logs", Service: "app1"})
	assert.Equal(t, "app1:\n```\na `` b\n```", got)
}

func TestDispatch_LogsTruncatedInsideFence(t *testing.T) {
	mgr := newMockServiceManager()
	const tailMarker = "===TAIL==="
	mgr.doResult = strings.Repeat("x\n", 5000) + tailMarker
	b := newTestBot(t, newFakeAPI(t), mgr, nil)
	got := b.dispatchCommand(&Command{Action: "logs", Service: "app1"})
	assert.LessOrEqual(t, utf16Len(got), telegramMaxUTF16Units)
	assert.True(t, strings.HasPrefix(got, "app1:\n```\n"))
	assert.True(t, strings.HasSuffix(got, tailMarker+"\n```"), "fence must survive and tail be kept")
	assert.Contains(t, got, "truncated")
}

// --- Message handling ---

func TestHandleUpdate_DispatchesCommandAndReplies(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	u := decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/restart app1", false))
	b.handleUpdate(context.Background(), u)

	assert.Equal(t, "restart", mgr.getLastOp())
	assert.Equal(t, "app1", mgr.getLastService())
	assert.Equal(t, []string{"ok"}, flush(t, b, api))
}

func TestHandleUpdate_BotSuffixAndUnderscore(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/start_all@mezzaops_bot", false)))
	assert.Equal(t, "start-all", mgr.getLastOp())
	assert.Equal(t, []string{"All services starting."}, flush(t, b, api))
}

func TestHandleUpdate_IgnoresOtherBotSuffix(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/restart@other_bot app1", false)))
	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, flush(t, b, api))
}

func TestHandleUpdate_IgnoresOtherChats(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID+1, time.Now(), "/restart app1", false)))
	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, flush(t, b, api), "nothing should be sent to a stranger")
}

func TestHandleUpdate_IgnoresBotSenders(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/restart app1", true)))
	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, flush(t, b, api))
}

func TestHandleUpdate_IgnoresMessageDate(t *testing.T) {
	// A host clock that is fast, slow or unsynced must not drop real commands.
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	old := time.Now().Add(-48 * time.Hour)
	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, old, "/deploy app1", false)))
	assert.Equal(t, "deploy", mgr.getLastOp())
	future := time.Now().Add(48 * time.Hour)
	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(2, testChatID, future, "/restart app1", false)))
	assert.Equal(t, "restart", mgr.getLastOp())
}

func TestHandleUpdate_AnonymousAdminAccepted(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	u := withSenderChat(updateJSON(1, testChatID, time.Now(), "/restart app1", true), testChatID)
	withFrom(u, map[string]any{"id": float64(1087968824), "is_bot": true, "username": "GroupAnonymousBot"})
	b.handleUpdate(context.Background(), decodeUpdate(t, u))
	assert.Equal(t, "restart", mgr.getLastOp())
	assert.Equal(t, []string{"ok"}, flush(t, b, api))
}

func TestHandleUpdate_SenderChatOfOtherChatIsIgnored(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	// A channel posting into the group: is_bot sender, sender_chat is not the group.
	u := withSenderChat(updateJSON(1, testChatID, time.Now(), "/restart app1", true), testChatID-5)
	b.handleUpdate(context.Background(), decodeUpdate(t, u))
	assert.Empty(t, mgr.getLastOp())
}

func TestHandleUpdate_IgnoresOwnMessages(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)
	b.botID = 42

	u := withFrom(updateJSON(1, testChatID, time.Now(), "/restart app1", false),
		map[string]any{"id": float64(42), "is_bot": false})
	b.handleUpdate(context.Background(), decodeUpdate(t, u))
	assert.Empty(t, mgr.getLastOp())
}

func TestHandleUpdate_NonBotUserWithOtherIDAccepted(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)
	b.botID = 42

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/restart app1", false)))
	assert.Equal(t, "restart", mgr.getLastOp())
}

func TestHandleUpdate_RepliesInTopic(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, withTopic(updateJSON(1, testChatID, time.Now(), "/status app1", false), 99)))
	flush(t, b, api)
	calls := api.callsTo("sendMessage")
	require.GreaterOrEqual(t, len(calls), 1)
	assert.Equal(t, float64(99), calls[0].Body["message_thread_id"])
}

func TestHandleUpdate_NoThreadIDOutsideTopics(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)

	// A reply thread in an ordinary group carries message_thread_id but is
	// not a topic message; sending with that ID would be rejected.
	u := updateJSON(1, testChatID, time.Now(), "/status app1", false)
	u["message"].(map[string]any)["message_thread_id"] = float64(55)
	b.handleUpdate(context.Background(), decodeUpdate(t, u))
	flush(t, b, api)
	calls := api.callsTo("sendMessage")
	require.GreaterOrEqual(t, len(calls), 1)
	_, has := calls[0].Body["message_thread_id"]
	assert.False(t, has)
}

func TestNotifications_GoToDefaultTopic(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "note")
	flush(t, b, api)
	_, has := api.callsTo("sendMessage")[0].Body["message_thread_id"]
	assert.False(t, has)
}

func TestHandleUpdate_IgnoresNonCommandAndNonMessage(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newSendingBot(t, api, mgr, nil)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "just chatting", false)))
	b.handleUpdate(context.Background(), update{UpdateID: 2})
	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, flush(t, b, api))
}

func TestHandleUpdate_ConfirmCommand(t *testing.T) {
	api := newFakeAPI(t)
	ch := &mockConfirmHandler{result: true}
	b := newSendingBot(t, api, nil, ch)

	b.handleUpdate(context.Background(), decodeUpdate(t, updateJSON(1, testChatID, time.Now(), "/confirm app1", false)))
	assert.Equal(t, "app1", ch.getLastService())
	assert.Equal(t, []string{"Confirmed deploy for <b>app1</b>."}, flush(t, b, api))
}

// --- Run / polling ---

func TestRun_ReadyClosesAfterGetMe(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)
	b.username = ""

	select {
	case <-b.Ready():
		t.Fatal("Ready must not be closed before Run")
	default:
	}

	startRun(t, b)
	select {
	case <-b.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never closed")
	}
	assert.Equal(t, testBotName, b.botUsername())
	assert.Len(t, api.callsTo("getMe"), 1)
}

func TestRun_SetsMyCommands(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)
	startRun(t, b)
	<-b.Ready()

	waitFor(t, "setMyCommands", func() bool { return len(api.callsTo("setMyCommands")) > 0 })
	call := api.callsTo("setMyCommands")[0]
	cmds, _ := call.Body["commands"].([]any)
	var names []string
	for _, c := range cmds {
		names = append(names, c.(map[string]any)["command"].(string))
	}
	assert.ElementsMatch(t, []string{"status", "start", "stop", "restart", "logs", "pull",
		"deploy", "confirm", "reload", "start_all", "stop_all"}, names)
}

func TestRun_SetMyCommandsFailureNotFatal(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("setMyCommands", func(apiCall) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"nope"}`
	})
	b := newTestBot(t, api, nil, nil)
	startRun(t, b)
	select {
	case <-b.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never closed after setMyCommands failure")
	}
}

func TestRun_RetriesWhenGetMeFails(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	attempts := 0
	api.handle("getMe", func(apiCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return 502, "bad gateway"
		}
		return 200, `{"ok":true,"result":{"id":42,"is_bot":true,"username":"` + testBotName + `"}}`
	})
	b := newTestBot(t, api, nil, nil)
	b.username = ""
	startRun(t, b)

	select {
	case <-b.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never closed after getMe recovered")
	}
	assert.Len(t, api.callsTo("getMe"), 3)
}

func TestRun_NotReadyWhileGetMeFails(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("getMe", func(apiCall) (int, string) {
		return 401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`
	})
	b := newTestBot(t, api, nil, nil)
	startRun(t, b)

	waitFor(t, "several getMe attempts", func() bool { return len(api.callsTo("getMe")) >= 3 })
	select {
	case <-b.Ready():
		t.Fatal("Ready must stay open while getMe fails")
	default:
	}
}

func TestRun_ProcessesUpdatesAndAdvancesOffset(t *testing.T) {
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newTestBot(t, api, mgr, nil)

	startRun(t, b)
	<-b.Ready()
	api.queueUpdate(updateJSON(10, testChatID, time.Now(), "/status", false))
	api.queueUpdate(updateJSON(11, testChatID, time.Now(), "/reload", false))

	waitFor(t, "two replies", func() bool { return len(api.sentTexts()) >= 2 })
	// Let a few more polls happen; a replayed update would send more replies.
	waitFor(t, "offset-bearing polls", func() bool {
		for _, c := range api.callsTo("getUpdates") {
			if off, _ := c.Body["offset"].(float64); off == 12 {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, api.sentTexts(), 2, "updates must not be reprocessed")

	polls := api.callsTo("getUpdates")
	first := polls[0]
	assert.Equal(t, []any{"message"}, first.Body["allowed_updates"])
	assert.NotNil(t, first.Body["timeout"])
}

func TestRun_DrainsPendingUpdatesOnFirstConnect(t *testing.T) {
	// Updates delivered but not acknowledged before a restart (e.g. a
	// self-deploy /deploy) must not run again.
	api := newFakeAPI(t)
	mgr := newMockServiceManager()
	b := newTestBot(t, api, mgr, nil)
	api.queueUpdate(updateJSON(20, testChatID, time.Now(), "/deploy app1", false))
	api.queueUpdate(updateJSON(21, testChatID, time.Now(), "/deploy app1", false))

	startRun(t, b)
	<-b.Ready()
	waitFor(t, "drained offset", func() bool {
		for _, c := range api.callsTo("getUpdates") {
			if off, _ := c.Body["offset"].(float64); off == 22 {
				return true
			}
		}
		return false
	})
	drain := api.callsTo("getUpdates")[0]
	assert.Equal(t, float64(-1), drain.Body["offset"])
	assert.Equal(t, float64(0), drain.Body["timeout"])
	time.Sleep(30 * time.Millisecond)
	assert.Empty(t, mgr.getLastOp(), "pending updates must not be executed")

	api.queueUpdate(updateJSON(22, testChatID, time.Now(), "/status app1", false))
	waitFor(t, "new update processed", func() bool { return mgr.getLastOp() == "status" })
}

func TestRun_DrainWithNoPendingUpdates(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)
	startRun(t, b)
	<-b.Ready()
	api.queueUpdate(updateJSON(1, testChatID, time.Now(), "/status", false))
	waitFor(t, "update processed", func() bool { return len(api.sentTexts()) == 1 })
}

func TestRun_OffsetSurvivesReconnect(t *testing.T) {
	api := newFakeAPI(t)
	b := newTestBot(t, api, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = b.runOnce(ctx); close(done) }()
	<-b.Ready()
	api.queueUpdate(updateJSON(5, testChatID, time.Now(), "/status", false))
	sender, stopSender := context.WithCancel(context.Background())
	defer stopSender()
	go b.sendLoop(sender)
	waitFor(t, "first reply", func() bool { return len(api.sentTexts()) == 1 })
	cancel()
	<-done

	// Queued while disconnected. A second connect must not drain it (the
	// drain is first-connect only) and must not replay update 5.
	api.queueUpdate(updateJSON(6, testChatID, time.Now(), "/status", false))
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = b.runOnce(ctx2); close(done2) }()
	waitFor(t, "second reply", func() bool { return len(api.sentTexts()) == 2 })
	time.Sleep(40 * time.Millisecond)
	cancel2()
	<-done2
	assert.Len(t, api.sentTexts(), 2, "update 5 must not be replayed, update 6 must not be lost")
}

func TestPollLoop_BacksOffOnErrorsAndRecovers(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	fails := 0
	api.handle("getUpdates", func(apiCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if fails < 3 {
			fails++
			return 500, "oops"
		}
		return 200, `{"ok":true,"result":[]}`
	})
	b := newTestBot(t, api, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.pollLoop(ctx); close(done) }()
	waitFor(t, "recovery polls", func() bool { return len(api.callsTo("getUpdates")) >= 5 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollLoop did not return on cancel")
	}
}

func TestPollLoop_409IsLoggedWithExplanation(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("getUpdates", func(apiCall) (int, string) {
		return 409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`
	})
	b := newTestBot(t, api, nil, nil)

	var buf syncBuffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.pollLoop(ctx); close(done) }()
	waitFor(t, "409 log", func() bool { return strings.Contains(buf.String(), "webhook") })
	cancel()
	<-done
	assert.Contains(t, buf.String(), "another")
	assert.NotContains(t, buf.String(), testToken)
}

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb.String()
}

func TestRequestsGoToBotTokenPath(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	flush(t, b, api)
	calls := api.callsTo("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "/bot"+testToken+"/sendMessage", calls[0].Path)
}

func TestNew_DefaultAPIURL(t *testing.T) {
	b := New(Config{Token: testToken, ChatID: testChatID}, newMockServiceManager())
	assert.Equal(t, "https://api.telegram.org", b.cfg.APIURL)
	b = New(Config{Token: testToken, ChatID: testChatID, APIURL: "http://x:8081/"}, newMockServiceManager())
	assert.Equal(t, "http://x:8081", b.cfg.APIURL, "trailing slash is trimmed")
}

// --- PostMessage ---

func TestPostMessage_SendsHTML(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)

	b.PostMessage(context.Background(), "**hi** <there>")
	waitFor(t, "send", func() bool { return len(api.callsTo("sendMessage")) == 1 })
	calls := api.callsTo("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, float64(testChatID), calls[0].Body["chat_id"])
	assert.Equal(t, "<b>hi</b> &lt;there&gt;", calls[0].Body["text"])
	assert.Equal(t, "HTML", calls[0].Body["parse_mode"])
	assert.Equal(t, map[string]any{"is_disabled": true}, calls[0].Body["link_preview_options"])
}

func TestPostMessage_FallsBackToPlainTextOn400(t *testing.T) {
	api := newFakeAPI(t)
	n := 0
	api.handle("sendMessage", func(apiCall) (int, string) {
		n++
		if n == 1 {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
		}
		return 200, `{"ok":true,"result":{"message_id":1}}`
	})
	b := newSendingBot(t, api, nil, nil)

	b.PostMessage(context.Background(), "**hi** <there>")
	waitFor(t, "fallback", func() bool { return len(api.callsTo("sendMessage")) == 2 })
	calls := api.callsTo("sendMessage")
	require.Len(t, calls, 2)
	assert.Equal(t, "HTML", calls[0].Body["parse_mode"])
	assert.Equal(t, "**hi** <there>", calls[1].Body["text"], "fallback sends the original text")
	_, hasMode := calls[1].Body["parse_mode"]
	assert.False(t, hasMode)
}

func TestPostMessage_NoFallbackOtherErrors(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("sendMessage", func(apiCall) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`
	})
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "hi")
	waitFor(t, "send", func() bool { return len(api.callsTo("sendMessage")) >= 1 })
	b.PostMessage(context.Background(), "again")
	waitFor(t, "second send", func() bool { return len(api.callsTo("sendMessage")) >= 2 })
	assert.Len(t, api.callsTo("sendMessage"), 2, "one attempt per message")
}

func TestPostMessage_NoFallbackOnNon400EntityLookalike(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("sendMessage", func(apiCall) (int, string) {
		return 500, `{"ok":false,"error_code":500,"description":"can't parse entities"}`
	})
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "hi")
	b.PostMessage(context.Background(), "again")
	waitFor(t, "sends", func() bool { return len(api.callsTo("sendMessage")) >= 2 })
	assert.Len(t, api.callsTo("sendMessage"), 2)
}

func TestPostMessage_Other400DoesNotFallBackAndLogsOnce(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("sendMessage", func(apiCall) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	})
	b := newSendingBot(t, api, nil, nil)

	var buf syncBuffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	b.PostMessage(context.Background(), "hi")
	waitFor(t, "log", func() bool { return strings.Contains(buf.String(), "chat not found") })
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, api.callsTo("sendMessage"), 1, "chat not found is not a formatting problem")
	assert.Equal(t, 1, strings.Count(buf.String(), "chat not found"), "logged once")
}

func TestPostMessage_FallbackOnlyOnce(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("sendMessage", func(apiCall) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unclosed tag"}`
	})
	b := newSendingBot(t, api, nil, nil)
	b.PostMessage(context.Background(), "hi")
	waitFor(t, "fallback", func() bool { return len(api.callsTo("sendMessage")) >= 2 })
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, api.callsTo("sendMessage"), 2)
}

func TestPostMessage_TruncatesLongText(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)

	const tailMarker = "===TAIL==="
	b.PostMessage(context.Background(), strings.Repeat("y", 10000)+tailMarker)
	texts := flush(t, b, api)
	require.Len(t, texts, 1)
	assert.LessOrEqual(t, utf16Len(texts[0]), telegramMaxUTF16Units)
	assert.Contains(t, texts[0], "truncated")
	assert.True(t, strings.HasSuffix(texts[0], tailMarker))
}

func TestPostMessage_WorksBeforeReady(t *testing.T) {
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	select {
	case <-b.Ready():
		t.Fatal("not ready yet")
	default:
	}
	b.PostMessage(context.Background(), "early")
	assert.Equal(t, []string{"early"}, flush(t, b, api))
	assert.Empty(t, api.callsTo("getMe"))
}

// --- Token hygiene ---

func TestErrorsNeverContainToken(t *testing.T) {
	// A closed server makes every call fail with a *url.Error whose URL
	// contains the token.
	api := newFakeAPI(t)
	b := newSendingBot(t, api, nil, nil)
	api.Close()

	var buf syncBuffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	_, err := b.getMe(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	assert.NotContains(t, err.Error(), "SECRET")

	_, err = b.getUpdates(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")

	b.PostMessage(context.Background(), "hello")
	waitFor(t, "send failure logged", func() bool { return strings.Contains(buf.String(), "sendMessage") })
	assert.NotContains(t, buf.String(), "SECRET")
}

func TestAPIErrorsNeverContainToken(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("getMe", func(apiCall) (int, string) {
		return 401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`
	})
	b := newTestBot(t, api, nil, nil)
	_, err := b.getMe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unauthorized")
	assert.NotContains(t, err.Error(), "SECRET")
}
