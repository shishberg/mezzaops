package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/shishberg/mezzaops/internal/reconnect"
	"github.com/shishberg/mezzaops/internal/service"
)

// Command represents a parsed bot command.
type Command struct {
	Action  string
	Service string
}

// ParseCommand extracts a Command from a message directed at the bot.
// The first word (the @mention) is skipped. Returns nil if there is no command.
func ParseCommand(message string) *Command {
	fields := strings.Fields(message)
	if len(fields) < 2 {
		return nil
	}

	// Skip the @mention
	cmd := &Command{
		Action: fields[1],
	}
	if len(fields) >= 3 {
		cmd.Service = fields[2]
	}
	return cmd
}

// Config holds all configuration needed to connect to Mattermost.
type Config struct {
	URL     string
	Token   string
	Channel string
}

// restClient abstracts the Mattermost REST API methods we use.
type restClient interface {
	GetMe(ctx context.Context, etag string) (*model.User, *model.Response, error)
	CreatePost(ctx context.Context, post *model.Post) (*model.Post, *model.Response, error)
	GetChannel(ctx context.Context, channelId string) (*model.Channel, *model.Response, error)
	GetChannelByNameForTeamName(ctx context.Context, channelName, teamName string, etag string) (*model.Channel, *model.Response, error)
}

// wsClient abstracts the Mattermost WebSocket client for testability.
type wsClient interface {
	Listen()
	Close()
	EventChan() chan *model.WebSocketEvent
	PingTimeoutChan() chan bool
	GetListenError() *model.AppError
}

// realWSClient wraps model.WebSocketClient to implement wsClient.
type realWSClient struct {
	client *model.WebSocketClient
}

func (r *realWSClient) Listen() { r.client.Listen() }
func (r *realWSClient) Close()  { r.client.Close() }

func (r *realWSClient) EventChan() chan *model.WebSocketEvent {
	return r.client.EventChannel
}

func (r *realWSClient) PingTimeoutChan() chan bool {
	return r.client.PingTimeoutChannel
}

func (r *realWSClient) GetListenError() *model.AppError {
	return r.client.ListenError
}

// wsConnectFunc creates a new WebSocket client connection.
type wsConnectFunc func(url, token string) (wsClient, error)

// validCommands lists the commands accepted by the bot, used in error messages.
var validCommands = []string{
	"status", "start", "stop", "restart", "logs", "pull",
	"deploy", "confirm", "reload", "start-all", "stop-all",
}

// Bot connects to Mattermost via the SDK and dispatches commands.
type Bot struct {
	cfg     Config
	manager service.ServiceManager
	confirm service.ConfirmHandler // may be nil
	// userID and channelID are written by runOnce (resolveIdentity,
	// resolveChannel) and read by PostMessage / event handlers. idMu
	// protects them so a notification issued while runOnce is still
	// building the connection (or tearing it down on retry) sees a
	// consistent snapshot.
	userID    string
	channelID string
	rest      restClient
	connectWS wsConnectFunc
	readyCh   chan struct{} // closed when the bot is connected and ready
	readyOnce sync.Once
	idMu      sync.Mutex
}

// New creates a new Bot from the given Config and service.ServiceManager.
func New(cfg Config, manager service.ServiceManager) *Bot {
	client := model.NewAPIv4Client(cfg.URL)
	client.SetToken(cfg.Token)

	return &Bot{
		cfg:     cfg,
		manager: manager,
		rest:    client,
		readyCh: make(chan struct{}),
		connectWS: func(url, token string) (wsClient, error) {
			wsURL := strings.Replace(url, "https://", "wss://", 1)
			wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
			c, err := model.NewWebSocketClient4(wsURL, token)
			if err != nil {
				return nil, err
			}
			return &realWSClient{client: c}, nil
		},
	}
}

// SetConfirmHandler sets the handler for deploy confirmations.
func (b *Bot) SetConfirmHandler(ch service.ConfirmHandler) {
	b.confirm = ch
}

// Run connects to Mattermost, resolves the channel, and listens for events
// until ctx is cancelled.
//
// The whole connect path (resolveIdentity, resolveChannel, WebSocket
// listen) runs inside reconnect.RunWithBackoff, which is the single backoff
// policy: a failed initial connect (e.g. Tailscale MagicDNS not ready yet)
// and a dropped WebSocket are both retried with capped exponential backoff.
// Re-running the two REST lookups on each reconnect is cheap and also
// revalidates the token and channel after an outage. readyCh is closed on
// the first successful connect and not re-closed on later ones; by then
// the C3 bounded readiness wait in app.go has already fired.
func (b *Bot) Run(ctx context.Context) error {
	return reconnect.RunWithBackoff(ctx, "mattermost", reconnect.Options{}, func(ctx context.Context) error {
		if err := b.resolveIdentity(ctx); err != nil {
			return fmt.Errorf("resolving bot identity: %w", err)
		}
		if err := b.resolveChannel(ctx); err != nil {
			return fmt.Errorf("resolving channel: %w", err)
		}
		b.markReady()
		return b.connectAndListen(ctx)
	})
}

// markReady closes readyCh exactly once; later calls are no-ops.
func (b *Bot) markReady() {
	b.readyOnce.Do(func() { close(b.readyCh) })
}

// Ready returns a channel that is closed when the bot has connected
// and is ready to send messages.
func (b *Bot) Ready() <-chan struct{} {
	return b.readyCh
}

// PostMessage sends a message to the configured Mattermost channel.
// channelID is snapshotted under idMu so concurrent retry-driven
// re-resolution cannot leave PostMessage using a half-updated value.
func (b *Bot) PostMessage(ctx context.Context, message string) {
	b.idMu.Lock()
	channelID := b.channelID
	b.idMu.Unlock()
	if channelID == "" {
		log.Printf("mattermost: PostMessage called before channel resolved, dropping message")
		return
	}
	post := &model.Post{
		ChannelId: channelID,
		Message:   message,
	}
	if _, _, err := b.rest.CreatePost(ctx, post); err != nil {
		log.Printf("mattermost: post message: %v", err)
	}
}

// resolveIdentity fetches the bot's own user ID from the REST API.
func (b *Bot) resolveIdentity(ctx context.Context) error {
	user, _, err := b.rest.GetMe(ctx, "")
	if err != nil {
		return err
	}
	if user.Id == "" {
		return fmt.Errorf("empty user ID (check token)")
	}
	b.idMu.Lock()
	b.userID = user.Id
	b.idMu.Unlock()
	return nil
}

// resolveChannel finds the channel ID for the configured channel name.
// Accepts "team/channel-name" (looked up via API) or a raw 26-char channel ID.
func (b *Bot) resolveChannel(ctx context.Context) error {
	if strings.Contains(b.cfg.Channel, "/") {
		parts := strings.SplitN(b.cfg.Channel, "/", 2)
		ch, _, err := b.rest.GetChannelByNameForTeamName(ctx, parts[1], parts[0], "")
		if err != nil {
			return fmt.Errorf("looking up channel %q in team %q: %w", parts[1], parts[0], err)
		}
		b.idMu.Lock()
		b.channelID = ch.Id
		b.idMu.Unlock()
		log.Printf("mattermost: resolved channel %q to ID %s", b.cfg.Channel, ch.Id)
		return nil
	}

	if !model.IsValidId(b.cfg.Channel) {
		return fmt.Errorf("invalid channel %q: use \"team/channel-name\" format or a 26-char channel ID", b.cfg.Channel)
	}

	// Validate the ID actually exists.
	ch, _, err := b.rest.GetChannel(ctx, b.cfg.Channel)
	if err != nil {
		return fmt.Errorf("looking up channel ID %q: %w", b.cfg.Channel, err)
	}
	b.idMu.Lock()
	b.channelID = ch.Id
	b.idMu.Unlock()
	return nil
}

// connectAndListen opens a single WebSocket connection and processes events
// until the connection drops or ctx is cancelled.
func (b *Bot) connectAndListen(ctx context.Context) error {
	ws, err := b.connectWS(b.cfg.URL, b.cfg.Token)
	if err != nil {
		return fmt.Errorf("websocket connect: %w", err)
	}
	defer ws.Close()

	ws.Listen()
	b.idMu.Lock()
	channelID := b.channelID
	b.idMu.Unlock()
	log.Printf("mattermost: connected to %s, watching channel %s", b.cfg.URL, channelID)

	for {
		select {
		case <-ctx.Done():
			return nil

		case event := <-ws.EventChan():
			if event != nil {
				b.handleEvent(ctx, event)
			}

		case <-ws.PingTimeoutChan():
			return fmt.Errorf("ping timeout")
		}

		if listenErr := ws.GetListenError(); listenErr != nil {
			return fmt.Errorf("listen error: %w", listenErr)
		}
	}
}

func (b *Bot) handleEvent(ctx context.Context, event *model.WebSocketEvent) {
	if event.EventType() != model.WebsocketEventPosted {
		return
	}

	data := event.GetData()

	postJSON, ok := data["post"].(string)
	if !ok {
		return
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		log.Printf("mattermost: failed to unmarshal post: %v", err)
		return
	}

	b.idMu.Lock()
	userID := b.userID
	channelID := b.channelID
	b.idMu.Unlock()

	if post.UserId == userID {
		return // ignore own messages
	}

	// Filter: must be in our channel OR mention us
	if post.ChannelId != channelID && !b.isMentioned(data) {
		return
	}

	cmd := ParseCommand(post.Message)
	if cmd == nil {
		return
	}

	response := b.dispatchCommand(cmd)
	b.PostMessage(ctx, response)
}

// dispatchCommand routes a parsed command to the appropriate manager method.
func (b *Bot) dispatchCommand(cmd *Command) string {
	switch strings.ToLower(cmd.Action) {
	case "status":
		if cmd.Service == "" {
			return b.handleStatusOverview()
		}
		return b.manager.Do(cmd.Service, "status")

	case "start", "stop", "restart", "logs", "pull":
		return b.manager.Do(cmd.Service, strings.ToLower(cmd.Action))

	case "deploy":
		if err := b.manager.RequestDeploy(cmd.Service); err != nil {
			return fmt.Sprintf("Deploy error: %v", err)
		}
		return fmt.Sprintf("Deploy requested for **%s**.", cmd.Service)

	case "confirm":
		return b.handleConfirm(cmd.Service)

	case "reload":
		if err := b.manager.Reload(); err != nil {
			return fmt.Sprintf("Reload error: %v", err)
		}
		return "Config reloaded."

	case "start-all":
		b.manager.StartAll()
		return "All services starting."

	case "stop-all":
		b.manager.StopAll()
		return "All services stopping."

	default:
		return fmt.Sprintf("Unknown command: %q. Valid commands: %s",
			cmd.Action, strings.Join(validCommands, ", "))
	}
}

// handleStatusOverview builds a markdown overview of all service states.
func (b *Bot) handleStatusOverview() string {
	states := b.manager.GetAllStates()
	return service.FormatStatusOverview(states)
}

// handleConfirm dispatches to the confirm handler if set.
func (b *Bot) handleConfirm(svc string) string {
	if b.confirm == nil {
		return "Confirm handler not configured."
	}
	if !b.confirm.Confirm(svc) {
		return fmt.Sprintf("No pending deploy confirmation for **%s** (or it expired).", svc)
	}
	return fmt.Sprintf("Confirmed deploy for **%s**.", svc)
}

// isMentioned checks the "mentions" field in the event data for our user ID.
func (b *Bot) isMentioned(data map[string]any) bool {
	mentionsStr, ok := data["mentions"].(string)
	if !ok {
		return false
	}
	var mentions []string
	if err := json.Unmarshal([]byte(mentionsStr), &mentions); err != nil {
		return false
	}
	b.idMu.Lock()
	userID := b.userID
	b.idMu.Unlock()
	return slices.Contains(mentions, userID)
}
