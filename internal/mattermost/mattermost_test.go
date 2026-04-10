package mattermost

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ParseCommand tests ---

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    *Command
	}{
		{"status command", "@mezzaops status", &Command{Action: "status"}},
		{"deploy command", "@mezzaops deploy myapp", &Command{Action: "deploy", Service: "myapp"}},
		{"restart command", "@mezzaops restart myapp", &Command{Action: "restart", Service: "myapp"}},
		{"confirm command", "@mezzaops confirm myapp", &Command{Action: "confirm", Service: "myapp"}},
		{"start command", "@mezzaops start myapp", &Command{Action: "start", Service: "myapp"}},
		{"stop command", "@mezzaops stop myapp", &Command{Action: "stop", Service: "myapp"}},
		{"logs command", "@mezzaops logs myapp", &Command{Action: "logs", Service: "myapp"}},
		{"pull command", "@mezzaops pull myapp", &Command{Action: "pull", Service: "myapp"}},
		{"reload command", "@mezzaops reload", &Command{Action: "reload"}},
		{"start-all command", "@mezzaops start-all", &Command{Action: "start-all"}},
		{"stop-all command", "@mezzaops stop-all", &Command{Action: "stop-all"}},
		{"with extra whitespace", "  @mezzaops   deploy   myapp  ", &Command{Action: "deploy", Service: "myapp"}},
		{"single word", "hello", nil},
		{"empty after mention", "@mezzaops", nil},
		{"unknown command", "@mezzaops foobar", &Command{Action: "foobar"}},
		{"case insensitive mention", "@MezzaOps deploy myapp", &Command{Action: "deploy", Service: "myapp"}},
		{"case insensitive mixed", "@MEZZAOPS status", &Command{Action: "status"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommand(tt.message)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- Mocks ---

// mockWSClient implements wsClient with controllable channels.
type mockWSClient struct {
	events      chan *model.WebSocketEvent
	pingTimeout chan bool
	listenErr   *model.AppError
	mu          sync.Mutex
	closed      bool
}

func newMockWSClient() *mockWSClient {
	return &mockWSClient{
		events:      make(chan *model.WebSocketEvent, 10),
		pingTimeout: make(chan bool, 1),
	}
}

func (m *mockWSClient) Listen() {}

func (m *mockWSClient) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.events)
	}
}

func (m *mockWSClient) EventChan() chan *model.WebSocketEvent { return m.events }
func (m *mockWSClient) PingTimeoutChan() chan bool            { return m.pingTimeout }

func (m *mockWSClient) GetListenError() *model.AppError {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listenErr
}

func (m *mockWSClient) SetListenError(err *model.AppError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listenErr = err
}

// mockRestClient implements restClient for testing.
type mockRestClient struct {
	mu    sync.Mutex
	posts []*model.Post
}

func (m *mockRestClient) GetMe(_ context.Context, _ string) (*model.User, *model.Response, error) {
	return &model.User{Id: "bot-user-id"}, &model.Response{}, nil
}

func (m *mockRestClient) CreatePost(_ context.Context, post *model.Post) (*model.Post, *model.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, post)
	return post, &model.Response{}, nil
}

func (m *mockRestClient) GetChannel(_ context.Context, channelId string) (*model.Channel, *model.Response, error) {
	return &model.Channel{Id: channelId}, &model.Response{}, nil
}

func (m *mockRestClient) GetChannelByNameForTeamName(_ context.Context, _, _ string, _ string) (*model.Channel, *model.Response, error) {
	return &model.Channel{Id: "channel-id-123"}, &model.Response{}, nil
}

func (m *mockRestClient) getPosts() []*model.Post {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*model.Post, len(m.posts))
	copy(result, m.posts)
	return result
}

// mockServiceManager implements ServiceManager for testing.
type mockServiceManager struct {
	mu          sync.Mutex
	lastOp      string
	lastService string
	doResult    string
	deployErr   error
	reloadErr   error
	names       []string
	states      map[string]service.ServiceState
}

func newMockServiceManager() *mockServiceManager {
	return &mockServiceManager{
		doResult: "ok",
		names:    []string{"app1", "app2"},
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
	return m.names
}

func (m *mockServiceManager) CountRunning() (int, int) {
	return 1, 2
}

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

// mockConfirmHandler implements ConfirmHandler for testing.
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

// --- Reconnection tests ---

func TestConnectAndListen_GracefulShutdown(t *testing.T) {
	ws := newMockWSClient()
	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			return ws, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- bot.connectAndListen(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("connectAndListen did not return after cancel")
	}
}

func TestConnectAndListen_PingTimeout(t *testing.T) {
	ws := newMockWSClient()
	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			return ws, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- bot.connectAndListen(context.Background())
	}()

	ws.PingTimeoutChan() <- true

	select {
	case err := <-done:
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ping timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("connectAndListen did not return on ping timeout")
	}
}

func TestConnectAndListen_ListenError(t *testing.T) {
	ws := newMockWSClient()
	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			return ws, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- bot.connectAndListen(context.Background())
	}()

	ws.SetListenError(model.NewAppError("ws", "connection lost", nil, "", 0))
	ws.EventChan() <- nil

	select {
	case err := <-done:
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "listen error")
	case <-time.After(2 * time.Second):
		t.Fatal("connectAndListen did not return on listen error")
	}
}

func TestListenWebSocket_ReconnectsAfterDisconnect(t *testing.T) {
	var connectCount atomic.Int32

	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			connectCount.Add(1)
			ws := newMockWSClient()
			go func() {
				ws.PingTimeoutChan() <- true
			}()
			return ws, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- bot.listenWebSocket(ctx)
	}()

	require.Eventually(t, func() bool {
		return connectCount.Load() >= 2
	}, 5*time.Second, 100*time.Millisecond, "expected at least 2 connection attempts")

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("listenWebSocket did not exit after cancel")
	}
}

func TestListenWebSocket_CancelDuringBackoff(t *testing.T) {
	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			ws := newMockWSClient()
			go func() { ws.PingTimeoutChan() <- true }()
			return ws, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- bot.listenWebSocket(ctx)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("listenWebSocket did not exit during backoff")
	}
}

func TestListenWebSocket_ConnectFailureReconnects(t *testing.T) {
	var attempts atomic.Int32

	bot := &Bot{
		cfg: Config{URL: "http://localhost"},
		connectWS: func(_, _ string) (wsClient, error) {
			attempts.Add(1)
			return nil, fmt.Errorf("connection refused")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- bot.listenWebSocket(ctx)
	}()

	require.Eventually(t, func() bool {
		return attempts.Load() >= 2
	}, 5*time.Second, 100*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("listenWebSocket did not exit after cancel")
	}
}

// --- Event handling tests ---

func makePostEvent(channelID, userID, message string) *model.WebSocketEvent {
	postJSON := fmt.Sprintf(`{"id":"post1","user_id":%q,"channel_id":%q,"message":%q}`, userID, channelID, message)
	event := model.NewWebSocketEvent(model.WebsocketEventPosted, "", channelID, "", nil, "")
	return event.SetData(map[string]any{
		"channel_id": channelID,
		"post":       postJSON,
	})
}

func makePostEventWithMentions(channelID, userID, message string, mentions []string) *model.WebSocketEvent {
	postJSON := fmt.Sprintf(`{"id":"post1","user_id":%q,"channel_id":%q,"message":%q}`, userID, channelID, message)
	mentionsJSON := "["
	for i, m := range mentions {
		if i > 0 {
			mentionsJSON += ","
		}
		mentionsJSON += fmt.Sprintf("%q", m)
	}
	mentionsJSON += "]"
	event := model.NewWebSocketEvent(model.WebsocketEventPosted, "", channelID, "", nil, "")
	return event.SetData(map[string]any{
		"channel_id": channelID,
		"post":       postJSON,
		"mentions":   mentionsJSON,
	})
}

func TestHandleEvent_DispatchesCommand(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops deploy myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "deploy", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Deploy requested")
}

func TestHandleEvent_IgnoresOwnMessages(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "bot-user-id", "@mezzaops status")
	bot.handleEvent(context.Background(), event)

	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, rest.getPosts())
}

func TestHandleEvent_MentionFromOtherChannel(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEventWithMentions("other-channel", "other-user", "@mezzaops status", []string{"bot-user-id"})
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "status", mgr.getLastOp())
}

func TestHandleEvent_IgnoresOtherChannelsWithoutMention(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("other-channel", "other-user", "@mezzaops status")
	bot.handleEvent(context.Background(), event)

	assert.Empty(t, mgr.getLastOp())
	assert.Empty(t, rest.getPosts())
}

func TestHandleEvent_IgnoresNonPostedEvents(t *testing.T) {
	mgr := newMockServiceManager()

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := model.NewWebSocketEvent(model.WebsocketEventTyping, "", "channel-123", "", nil, "")
	bot.handleEvent(context.Background(), event)

	assert.Empty(t, mgr.getLastOp())
}

// --- New command routing tests ---

func TestHandleEvent_Start(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops start myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "start", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Equal(t, "ok", posts[0].Message)
}

func TestHandleEvent_StartUpperCase(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops START myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "start", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleEvent_Stop(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops stop myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "stop", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleEvent_Restart(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops restart myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "restart", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleEvent_Logs(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops logs myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "logs", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleEvent_Pull(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops pull myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "pull", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
}

func TestHandleEvent_StatusSingleService(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.doResult = "running"
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops status myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "status", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Equal(t, "running", posts[0].Message)
}

func TestHandleEvent_StatusOverview(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops status")
	bot.handleEvent(context.Background(), event)

	// status without service name should call GetAllStates for overview
	assert.Equal(t, "status", mgr.getLastOp())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Service Status")
	assert.Contains(t, posts[0].Message, "app1")
	assert.Contains(t, posts[0].Message, "app2")
}

func TestHandleEvent_Reload(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops reload")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "reload", mgr.getLastOp())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "reloaded")
}

func TestHandleEvent_ReloadError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.reloadErr = fmt.Errorf("bad config")
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops reload")
	bot.handleEvent(context.Background(), event)

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "bad config")
}

func TestHandleEvent_StartAll(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops start-all")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "start-all", mgr.getLastOp())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "starting")
}

func TestHandleEvent_StopAll(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops stop-all")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "stop-all", mgr.getLastOp())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "stopping")
}

func TestHandleEvent_Deploy(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops deploy myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "deploy", mgr.getLastOp())
	assert.Equal(t, "myapp", mgr.getLastService())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Deploy requested")
}

func TestHandleEvent_DeployError(t *testing.T) {
	mgr := newMockServiceManager()
	mgr.deployErr = fmt.Errorf("service not found")
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops deploy myapp")
	bot.handleEvent(context.Background(), event)

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "service not found")
}

func TestHandleEvent_Confirm(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}
	ch := &mockConfirmHandler{result: true}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		confirm:   ch,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops confirm myapp")
	bot.handleEvent(context.Background(), event)

	assert.Equal(t, "myapp", ch.getLastService())
	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Confirmed")
}

func TestHandleEvent_ConfirmNoPending(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}
	ch := &mockConfirmHandler{result: false}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		confirm:   ch,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops confirm myapp")
	bot.handleEvent(context.Background(), event)

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "No pending")
}

func TestHandleEvent_ConfirmNoHandler(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		confirm:   nil, // no confirm handler
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops confirm myapp")
	bot.handleEvent(context.Background(), event)

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "not configured")
}

func TestHandleEvent_UnknownCommand(t *testing.T) {
	mgr := newMockServiceManager()
	rest := &mockRestClient{}

	bot := &Bot{
		cfg:       Config{URL: "http://localhost"},
		manager:   mgr,
		rest:      rest,
		userID:    "bot-user-id",
		channelID: "channel-123",
	}

	event := makePostEvent("channel-123", "other-user", "@mezzaops foobar")
	bot.handleEvent(context.Background(), event)

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Unknown command")
	assert.Contains(t, posts[0].Message, "foobar")
	// Should list valid commands
	assert.Contains(t, posts[0].Message, "status")
	assert.Contains(t, posts[0].Message, "deploy")
}

// --- REST integration tests ---

func TestResolveIdentity(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{rest: rest}

	err := bot.resolveIdentity(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "bot-user-id", bot.userID)
}

func TestResolveChannel_TeamSlashChannel(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		cfg:  Config{Channel: "myteam/ops"},
		rest: rest,
	}

	err := bot.resolveChannel(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "channel-id-123", bot.channelID)
}

func TestResolveChannel_RawID(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		cfg:  Config{Channel: "abcdefghijklmnopqrstuvwxyz"}, // 26-char lowercase
		rest: rest,
	}

	err := bot.resolveChannel(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz", bot.channelID)
}

func TestResolveChannel_InvalidFormat(t *testing.T) {
	bot := &Bot{
		cfg: Config{Channel: "invalid"},
	}

	err := bot.resolveChannel(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team/channel-name")
}

// --- Notifier tests ---

func TestNotifier_ServiceEvent(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.ServiceEvent("myapp", "started")

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "myapp")
	assert.Contains(t, posts[0].Message, "started")
}

func TestNotifier_DeployStarted(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.DeployStarted("myapp")

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "Deploying")
	assert.Contains(t, posts[0].Message, "myapp")
}

func TestNotifier_DeploySucceeded(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.DeploySucceeded("myapp", "build output here")

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "succeeded")
	assert.Contains(t, posts[0].Message, "myapp")
}

func TestNotifier_DeployFailed(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.DeployFailed("myapp", "build", "error output")

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Message, "failed")
	assert.Contains(t, posts[0].Message, "myapp")
	assert.Contains(t, posts[0].Message, "build")
	assert.Contains(t, posts[0].Message, "error output")
}

func TestNotifier_WebhookReceived_Full(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.WebhookReceived("myapp", service.WebhookInfo{
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

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	msg := posts[0].Message
	assert.Contains(t, msg, "myapp")
	assert.Contains(t, msg, "acme/myapp")
	assert.Contains(t, msg, "main")
	assert.Contains(t, msg, "deadbee")
	assert.Contains(t, msg, "https://github.com/acme/myapp/commit/deadbeef")
	assert.Contains(t, msg, "feat: add a thing")
	assert.NotContains(t, msg, "more details here")
	assert.Contains(t, msg, "Alice Author")
	assert.Contains(t, msg, "https://github.com/acme/myapp/compare/abc...def")
}

func TestNotifier_WebhookReceived_NoHeadCommit(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	n.WebhookReceived("myapp", service.WebhookInfo{
		Repo:   "acme/myapp",
		Branch: "main",
		Pusher: "alice",
	})

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	msg := posts[0].Message
	assert.Contains(t, msg, "myapp")
	assert.Contains(t, msg, "main")
	assert.Contains(t, msg, "alice")
}

func TestNotifier_WebhookReceived_LongCommitMsg(t *testing.T) {
	rest := &mockRestClient{}
	bot := &Bot{
		rest:      rest,
		channelID: "channel-123",
	}

	n := NewNotifier(bot)
	long := strings.Repeat("x", 400)
	n.WebhookReceived("myapp", service.WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		CommitID:  "deadbeef",
		CommitMsg: long,
	})

	posts := rest.getPosts()
	require.Len(t, posts, 1)
	msg := posts[0].Message
	assert.Contains(t, msg, "...")
	assert.NotContains(t, msg, long)
}

// --- Status overview format test ---

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

// --- SetConfirmHandler test ---

func TestSetConfirmHandler(t *testing.T) {
	bot := &Bot{}
	ch := &mockConfirmHandler{}

	bot.SetConfirmHandler(ch)
	assert.Equal(t, ch, bot.confirm)
}

// --- Status overview is deterministic (sorted) ---

func TestStatusOverview_Sorted(t *testing.T) {
	states := map[string]service.ServiceState{
		"zeta":  {Status: "running"},
		"alpha": {Status: "stopped"},
		"mu":    {Status: "running"},
	}

	overview := formatStatusOverview(states)
	lines := strings.Split(overview, "\n")

	// Find lines with service names and verify they're sorted
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
