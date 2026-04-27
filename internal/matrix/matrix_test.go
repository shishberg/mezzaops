package matrix

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/shishberg/matrixbot"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestParseCommand(t *testing.T) {
	const prefix = "!mezzaops"
	tests := []struct {
		name    string
		message string
		want    *Command
	}{
		{"status command", "!mezzaops status", &Command{Action: "status"}},
		{"deploy command", "!mezzaops deploy myapp", &Command{Action: "deploy", Service: "myapp"}},
		{"restart command", "!mezzaops restart myapp", &Command{Action: "restart", Service: "myapp"}},
		{"confirm command", "!mezzaops confirm myapp", &Command{Action: "confirm", Service: "myapp"}},
		{"start command", "!mezzaops start myapp", &Command{Action: "start", Service: "myapp"}},
		{"stop command", "!mezzaops stop myapp", &Command{Action: "stop", Service: "myapp"}},
		{"logs command", "!mezzaops logs myapp", &Command{Action: "logs", Service: "myapp"}},
		{"pull command", "!mezzaops pull myapp", &Command{Action: "pull", Service: "myapp"}},
		{"reload command", "!mezzaops reload", &Command{Action: "reload"}},
		{"start-all command", "!mezzaops start-all", &Command{Action: "start-all"}},
		{"stop-all command", "!mezzaops stop-all", &Command{Action: "stop-all"}},
		{"with extra whitespace", "  !mezzaops   deploy   myapp  ", &Command{Action: "deploy", Service: "myapp"}},
		{"single word", "hello", nil},
		{"empty after prefix", "!mezzaops", nil},
		{"unknown action", "!mezzaops foobar", &Command{Action: "foobar"}},
		{"non-matching prefix", "@mezzaops status", nil},
		{"prefix-substring no match", "!mezzaops-other status", nil},
		{"case sensitive prefix", "!MEZZAOPS status", nil},
		{"case preserves action", "!mezzaops Status myapp", &Command{Action: "Status", Service: "myapp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommand(tt.message, prefix)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCommand_CustomPrefix(t *testing.T) {
	got := ParseCommand("!ops deploy svc", "!ops")
	assert.Equal(t, &Command{Action: "deploy", Service: "svc"}, got)
}

// --- Mocks for dispatch tests ---

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
			"app2": {Status: "stopped"},
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

// botForTest returns a Bot wired with the given mocks, ready for direct
// dispatch / handleCommand calls without the matrixbot runtime.
func botForTest(mgr ServiceManager, ch ConfirmHandler) *Bot {
	return &Bot{
		cfg: Config{
			CommandPrefix: "!mezzaops",
			UserID:        "@bot:example.org",
		},
		manager: mgr,
		confirm: ch,
		readyCh: make(chan struct{}),
	}
}

// --- Dispatch tests ---

func TestDispatch_StatusOverview(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "status"})
	assert.Contains(t, resp, "Service Status")
	assert.Contains(t, resp, "app1")
	assert.Contains(t, resp, "app2")
	assert.Equal(t, "status", mgr.getLastOp())
}

func TestDispatch_StatusSingleService(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = "running"
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "status", Service: "myapp"})
	assert.Equal(t, "running", resp)
	assert.Equal(t, "status", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestDispatch_StartStopRestartLogsPull(t *testing.T) {
	for _, op := range []string{"start", "stop", "restart", "logs", "pull"} {
		t.Run(op, func(t *testing.T) {
			mgr := newMockServiceManager()
			bot := botForTest(mgr, nil)

			resp := bot.dispatchCommand(&Command{Action: op, Service: "myapp"})
			assert.Equal(t, "ok", resp)
			assert.Equal(t, op, mgr.getLastOp())
			assert.Equal(t, "myapp", mgr.getLastService())
		})
	}
}

func TestDispatch_ActionCaseInsensitive(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	bot.dispatchCommand(&Command{Action: "START", Service: "myapp"})
	assert.Equal(t, "start", mgr.getLastOp())
}

func TestDispatch_Deploy(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "deploy", Service: "myapp"})
	assert.Contains(t, resp, "Deploy requested")
	assert.Equal(t, "deploy", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestDispatch_DeployError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.deployErr = fmt.Errorf("service not found")
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "deploy", Service: "myapp"})
	assert.Contains(t, resp, "Deploy error")
	assert.Contains(t, resp, "service not found")
}

func TestDispatch_Confirm(t *testing.T) {
	mgr := newMockServiceManager()
	ch := &mockConfirmHandler{result: true}
	bot := botForTest(mgr, ch)

	resp := bot.dispatchCommand(&Command{Action: "confirm", Service: "myapp"})
	assert.Contains(t, resp, "Confirmed")
	assert.Equal(t, "myapp", ch.getLastService())
}

func TestDispatch_ConfirmNoPending(t *testing.T) {
	ch := &mockConfirmHandler{result: false}
	bot := botForTest(newMockServiceManager(), ch)

	resp := bot.dispatchCommand(&Command{Action: "confirm", Service: "myapp"})
	assert.Contains(t, resp, "No pending")
}

func TestDispatch_ConfirmNoHandler(t *testing.T) {
	bot := botForTest(newMockServiceManager(), nil)

	resp := bot.dispatchCommand(&Command{Action: "confirm", Service: "myapp"})
	assert.Contains(t, resp, "not configured")
}

func TestDispatch_Reload(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "reload"})
	assert.Contains(t, resp, "reloaded")
	assert.Equal(t, "reload", mgr.getLastOp())
}

func TestDispatch_ReloadError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.reloadErr = fmt.Errorf("bad config")
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "reload"})
	assert.Contains(t, resp, "Reload error")
	assert.Contains(t, resp, "bad config")
}

func TestDispatch_StartAll(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "start-all"})
	assert.Contains(t, resp, "starting")
	assert.Equal(t, "start-all", mgr.getLastOp())
}

func TestDispatch_StopAll(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp := bot.dispatchCommand(&Command{Action: "stop-all"})
	assert.Contains(t, resp, "stopping")
	assert.Equal(t, "stop-all", mgr.getLastOp())
}

func TestDispatch_Unknown(t *testing.T) {
	bot := botForTest(newMockServiceManager(), nil)

	resp := bot.dispatchCommand(&Command{Action: "foobar"})
	assert.Contains(t, resp, "Unknown command")
	assert.Contains(t, resp, "foobar")
	assert.Contains(t, resp, "status")
	assert.Contains(t, resp, "deploy")
}

// --- handleCommand tests (the matrixbot.Handler wrapper) ---

func TestHandleCommand_DispatchesParsedInput(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp, err := bot.handleCommand(context.Background(), matrixbot.Request{Input: "deploy myapp"})
	require.NoError(t, err)
	assert.Contains(t, resp.Reply, "Deploy requested")
	assert.Equal(t, "deploy", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleCommand_EmptyInputStaysQuiet(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp, err := bot.handleCommand(context.Background(), matrixbot.Request{Input: ""})
	require.NoError(t, err)
	assert.Empty(t, resp.Reply, "empty input should produce no reply")
	assert.Empty(t, mgr.getLastOp(), "empty input should not touch the manager")
}

func TestHandleCommand_ActionOnly(t *testing.T) {
	mgr := newMockServiceManager()
	bot := botForTest(mgr, nil)

	resp, err := bot.handleCommand(context.Background(), matrixbot.Request{Input: "reload"})
	require.NoError(t, err)
	assert.Contains(t, resp.Reply, "reloaded")
	assert.Equal(t, "reload", mgr.getLastOp())
}

// --- Status overview format tests ---

func TestStatusOverview_Format(t *testing.T) {
	states := map[string]service.ServiceState{
		"web":    {Status: "running", LastResult: "success"},
		"worker": {Status: "stopped"},
	}
	overview := formatStatusOverview(states)
	assert.Contains(t, overview, "Service Status")
	assert.Contains(t, overview, "web")
	assert.Contains(t, overview, "running")
	assert.Contains(t, overview, "worker")
	assert.Contains(t, overview, "stopped")
	assert.Contains(t, overview, "success")
}

func TestStatusOverview_Empty(t *testing.T) {
	overview := formatStatusOverview(map[string]service.ServiceState{})
	assert.Contains(t, overview, "No services")
}

func TestStatusOverview_Sorted(t *testing.T) {
	states := map[string]service.ServiceState{
		"zeta":  {Status: "running"},
		"alpha": {Status: "stopped"},
		"mu":    {Status: "running"},
	}
	overview := formatStatusOverview(states)
	lines := strings.Split(overview, "\n")
	var svcLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "- **") {
			svcLines = append(svcLines, line)
		}
	}
	require.Len(t, svcLines, 3)
	sorted := make([]string, len(svcLines))
	copy(sorted, svcLines)
	sort.Strings(sorted)
	assert.Equal(t, sorted, svcLines)
}

// --- Setter / getter tests ---

func TestSetConfirmHandler(t *testing.T) {
	bot := &Bot{}
	ch := &mockConfirmHandler{}
	bot.SetConfirmHandler(ch)
	assert.Equal(t, ch, bot.confirm)
}

func TestCommandPrefix_ReturnsNormalisedValue(t *testing.T) {
	bot := &Bot{cfg: Config{CommandPrefix: "!ops"}}
	assert.Equal(t, "!ops", bot.CommandPrefix())
}

func TestNew_DefaultsCommandPrefix(t *testing.T) {
	bot := New(Config{Homeserver: "https://example.org", UserID: "@bot:example.org"}, "/tmp", newMockServiceManager())
	assert.Equal(t, "!mezzaops", bot.CommandPrefix())
}

// --- PostMessage drop-when-not-ready tests ---
//
// matrixbot owns the rendering and send path; mezzaops only owns the guard
// that prevents sends before Run has wired up the underlying runtime.

func TestPostMessage_DropsWhenMatrixBotNil(t *testing.T) {
	bot := &Bot{roomID: "!room:example.org"} // matrixBot deliberately nil
	// Must not panic, must not send anywhere.
	bot.PostMessage(context.Background(), "hello")
}

func TestPostMessage_DropsWhenRoomUnset(t *testing.T) {
	bot := &Bot{} // both matrixBot and roomID empty
	bot.PostMessage(context.Background(), "hello")
}

// --- resolveRoom tests ---

type fakeAliasResolver struct {
	resolve map[id.RoomAlias]id.RoomID
	err     error
}

func (f *fakeAliasResolver) ResolveAlias(_ context.Context, alias id.RoomAlias) (*mautrix.RespAliasResolve, error) {
	if f.err != nil {
		return nil, f.err
	}
	rid, ok := f.resolve[alias]
	if !ok {
		return nil, fmt.Errorf("no canned response for alias %s", alias)
	}
	return &mautrix.RespAliasResolve{RoomID: rid}, nil
}

func TestResolveRoom_PassesThroughRoomID(t *testing.T) {
	bot := &Bot{cfg: Config{Room: "!already-an-id:example.org"}}
	require.NoError(t, bot.resolveRoom(context.Background()))
	assert.Equal(t, id.RoomID("!already-an-id:example.org"), bot.roomID)
}

func TestResolveRoom_ResolvesAlias(t *testing.T) {
	bot := &Bot{
		cfg: Config{Room: "#ops:example.org"},
		aliasClient: &fakeAliasResolver{
			resolve: map[id.RoomAlias]id.RoomID{
				"#ops:example.org": "!resolved:example.org",
			},
		},
	}
	require.NoError(t, bot.resolveRoom(context.Background()))
	assert.Equal(t, id.RoomID("!resolved:example.org"), bot.roomID)
}

func TestResolveRoom_AliasError(t *testing.T) {
	bot := &Bot{
		cfg:         Config{Room: "#ops:example.org"},
		aliasClient: &fakeAliasResolver{err: fmt.Errorf("homeserver unreachable")},
	}
	err := bot.resolveRoom(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "#ops:example.org")
}

func TestResolveRoom_RejectsUnprefixedRoom(t *testing.T) {
	bot := &Bot{cfg: Config{Room: "ops"}}
	err := bot.resolveRoom(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with")
}

// --- Homeserver resolution tests ---

func TestResolveHomeserverURL_PassesThroughHTTPSURL(t *testing.T) {
	called := false
	discover := func(string) (*mautrix.ClientWellKnown, error) {
		called = true
		return nil, nil
	}
	got, err := resolveHomeserverURL("https://matrix.example.org", discover)
	require.NoError(t, err)
	assert.Equal(t, "https://matrix.example.org", got)
	assert.False(t, called, "discover must not be called for full URL input")
}

func TestResolveHomeserverURL_PassesThroughHTTPURL(t *testing.T) {
	called := false
	discover := func(string) (*mautrix.ClientWellKnown, error) {
		called = true
		return nil, nil
	}
	got, err := resolveHomeserverURL("http://localhost:8008", discover)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8008", got)
	assert.False(t, called, "discover must not be called for full URL input")
}

func TestResolveHomeserverURL_ResolvesServerNameViaWellKnown(t *testing.T) {
	var gotName string
	discover := func(serverName string) (*mautrix.ClientWellKnown, error) {
		gotName = serverName
		return &mautrix.ClientWellKnown{
			Homeserver: mautrix.HomeserverInfo{BaseURL: "https://matrix.example.org"},
		}, nil
	}
	got, err := resolveHomeserverURL("example.org", discover)
	require.NoError(t, err)
	assert.Equal(t, "https://matrix.example.org", got)
	assert.Equal(t, "example.org", gotName)
}

func TestResolveHomeserverURL_FallsBackWhenNoWellKnown(t *testing.T) {
	// mautrix.DiscoverClientAPI returns (nil, nil) when the server's
	// .well-known endpoint responds with 404 — i.e. no discovery info
	// published. The helper must fall back to https://<serverName>.
	discover := func(string) (*mautrix.ClientWellKnown, error) {
		return nil, nil
	}
	got, err := resolveHomeserverURL("example.org", discover)
	require.NoError(t, err)
	assert.Equal(t, "https://example.org", got)
}

func TestResolveHomeserverURL_ErrorsWhenWellKnownEmptyBaseURL(t *testing.T) {
	// A 200 with an empty m.homeserver.base_url is FAIL_ERROR in the Matrix
	// client-server discovery spec, distinct from the 404 → FAIL_PROMPT
	// fallback. Surface it instead of silently sending the bot to the apex.
	discover := func(string) (*mautrix.ClientWellKnown, error) {
		return &mautrix.ClientWellKnown{}, nil
	}
	_, err := resolveHomeserverURL("example.org", discover)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "example.org")
	assert.Contains(t, err.Error(), "base_url")
}

func TestResolveHomeserverURL_PropagatesDiscoverError(t *testing.T) {
	discoverErr := fmt.Errorf("network exploded")
	discover := func(string) (*mautrix.ClientWellKnown, error) {
		return nil, discoverErr
	}
	_, err := resolveHomeserverURL("example.org", discover)
	require.Error(t, err)
	assert.ErrorIs(t, err, discoverErr)
	assert.Contains(t, err.Error(), `"example.org"`)
}

func TestNew_ResolvesServerName(t *testing.T) {
	prev := discoverClientAPI
	discoverClientAPI = func(serverName string) (*mautrix.ClientWellKnown, error) {
		assert.Equal(t, "example.org", serverName)
		return &mautrix.ClientWellKnown{
			Homeserver: mautrix.HomeserverInfo{BaseURL: "https://matrix.example.org"},
		}, nil
	}
	t.Cleanup(func() { discoverClientAPI = prev })

	bot := New(Config{Homeserver: "example.org", UserID: "@bot:example.org"}, "/tmp", newMockServiceManager())

	require.NoError(t, bot.newErr)
	assert.Equal(t, "https://matrix.example.org", bot.resolvedHomeserver)
}
