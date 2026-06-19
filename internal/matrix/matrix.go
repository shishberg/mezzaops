// Package matrix is a Matrix chat frontend for mezzaops, parallel to the
// Mattermost and Discord frontends. It listens for command-prefixed messages
// in one configured room, dispatches them to the service manager, and posts
// notifications to the same room. End-to-end encryption is handled
// transparently by mautrix's cryptohelper using an on-disk SQLite store.
package matrix

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/shishberg/mezzaops/internal/reconnect"
	"github.com/shishberg/mezzaops/internal/service"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// Command is a parsed bot command.
type Command struct {
	Action  string
	Service string
}

// ParseCommand returns a Command when the first whitespace-separated token of
// message exactly equals prefix. Otherwise it returns nil. The remaining
// tokens after the prefix become Action and (optionally) Service. The prefix
// match is case-sensitive; the action's case is preserved for the caller to
// fold as it sees fit.
func ParseCommand(message, prefix string) *Command {
	fields := strings.Fields(message)
	if len(fields) < 2 {
		return nil
	}
	if fields[0] != prefix {
		return nil
	}
	cmd := &Command{Action: fields[1]}
	if len(fields) >= 3 {
		cmd.Service = fields[2]
	}
	return cmd
}

// ServiceManager is the slice of the manager API the Matrix frontend uses.
// Duplicated from internal/mattermost so the two packages stay independent;
// extract to internal/service if a third frontend wants the same shape.
type ServiceManager interface {
	Do(name, op string) string
	RequestDeploy(name string) error
	StartAll()
	StopAll()
	Reload() error
	ServiceNames() []string
	CountRunning() (int, int)
	GetAllStates() map[string]service.ServiceState
}

// ConfirmHandler completes a deploy confirmation initiated by a webhook for a
// service with require_confirmation: true.
type ConfirmHandler interface {
	Confirm(service string) bool
}

// Config holds everything matrix.New needs to construct a Bot. The four
// secrets (UserID, DeviceID, AccessToken, PickleKey) are loaded from env, the
// rest from config.yaml.
type Config struct {
	Homeserver    string
	Room          string
	CommandPrefix string
	CryptoDB      string

	UserID      string
	DeviceID    string
	AccessToken string
	PickleKey   string
}

// matrixClient is the slice of *mautrix.Client used by the bot. The real
// client satisfies it; tests pass a fake so they can exercise dispatch and
// invite handling without a live homeserver.
type matrixClient interface {
	SendMessageEvent(ctx context.Context, roomID id.RoomID, eventType event.Type, contentJSON interface{}, extra ...mautrix.ReqSendEvent) (*mautrix.RespSendEvent, error)
	JoinRoomByID(ctx context.Context, roomID id.RoomID) (*mautrix.RespJoinRoom, error)
	ResolveAlias(ctx context.Context, alias id.RoomAlias) (*mautrix.RespAliasResolve, error)
}

// validCommands is listed in the unknown-command response.
var validCommands = []string{
	"status", "start", "stop", "restart", "logs", "pull",
	"deploy", "confirm", "reload", "start-all", "stop-all",
}

// Bot is the Matrix frontend.
type Bot struct {
	cfg     Config
	manager ServiceManager
	confirm ConfirmHandler

	// client is the abstracted slice we call: tests pass a fake. realClient
	// is the same *mautrix.Client (when not under test); we keep the typed
	// reference so we can set Crypto, register Sync handlers, and call
	// SyncWithContext, which the abstraction deliberately does not expose.
	//
	// Both client and realClient are nil until Run makes the first
	// successful connect. New() does not perform I/O; the discovery +
	// client construction is part of the retried connect path so a
	// transient failure on boot (e.g. Tailscale MagicDNS not ready
	// yet) does not permanently disable the frontend.
	//
	// client, realClient, and roomID are written by runOnce and read by
	// PostMessage and the event handlers. connMu protects them so a
	// notification issued while runOnce is still building the client (or
	// tearing it down on retry) sees a consistent snapshot.
	client     matrixClient
	realClient *mautrix.Client
	stateDir   string
	userID     id.UserID
	roomID     id.RoomID
	readyCh    chan struct{}
	connMu     sync.Mutex
}

// discoverFunc resolves a Matrix server name to its client-server API
// well-known document. It mirrors mautrix.DiscoverClientAPI but drops the
// context parameter so tests can inject a fake without managing one.
type discoverFunc func(serverName string) (*mautrix.ClientWellKnown, error)

// discoverClientAPI is the package-level default used by resolveHomeserverURL.
// Tests override it; the variable is read once per New call from a single
// goroutine, so tests must not call t.Parallel while mutating it.
var discoverClientAPI discoverFunc = func(serverName string) (*mautrix.ClientWellKnown, error) {
	return mautrix.DiscoverClientAPI(context.Background(), serverName)
}

// resolveHomeserverURL turns a homeserver config value into the client-server
// API URL. A value with a scheme (https:// or http://) is returned unchanged.
// A bare server name is resolved via /.well-known/matrix/client per the
// Matrix client-server spec:
//   - mautrix returns (nil, nil) for a 404 — FAIL_PROMPT in spec terms — and
//     we fall back to https://<serverName>.
//   - mautrix returns a non-nil error for transport/JSON failures, which we
//     propagate.
//   - A 200 with an empty base_url is FAIL_ERROR per spec; we surface it as
//     an error so an operator misconfiguration doesn't silently route the
//     bot at the apex domain.
func resolveHomeserverURL(homeserver string, discover discoverFunc) (string, error) {
	if strings.Contains(homeserver, "://") {
		return homeserver, nil
	}
	wk, err := discover(homeserver)
	if err != nil {
		return "", fmt.Errorf("discovering client API for %q: %w", homeserver, err)
	}
	if wk == nil {
		return "https://" + homeserver, nil
	}
	if wk.Homeserver.BaseURL == "" {
		return "", fmt.Errorf(".well-known for %q returned empty m.homeserver.base_url", homeserver)
	}
	return wk.Homeserver.BaseURL, nil
}

// New constructs a Bot. No I/O is performed: the real connection, homeserver
// discovery, alias resolution, and crypto bootstrap happen in Run. Doing
// the construction lazily is what lets the C2 retry loop recover from a
// transient failure on boot (e.g. Tailscale MagicDNS not ready yet) —
// a failure during discovery or NewClient no longer permanently disables
// the frontend. stateDir is used to derive the default crypto database
// path when cfg.CryptoDB is empty.
func New(cfg Config, stateDir string, manager ServiceManager) *Bot {
	prefix := cfg.CommandPrefix
	if prefix == "" {
		prefix = "!mezzaops"
	}
	cfg.CommandPrefix = prefix

	return &Bot{
		cfg:      cfg,
		manager:  manager,
		stateDir: stateDir,
		userID:   id.UserID(cfg.UserID),
		readyCh:  make(chan struct{}),
	}
}

// SetConfirmHandler installs the deploy-confirmation handler.
func (b *Bot) SetConfirmHandler(ch ConfirmHandler) { b.confirm = ch }

// CommandPrefix returns the normalised prefix the bot listens for, so callers
// (e.g. the app's webhook confirmation prompt) can advertise the same string
// the bot will actually match.
func (b *Bot) CommandPrefix() string { return b.cfg.CommandPrefix }

// Ready closes once Run has resolved the room and finished the crypto
// bootstrap, so callers know it is safe to call PostMessage.
func (b *Bot) Ready() <-chan struct{} { return b.readyCh }

// markReady closes readyCh exactly once. Safe to call from multiple retry
// attempts; the second close is recovered.
func (b *Bot) markReady() {
	defer func() { _ = recover() }()
	close(b.readyCh)
}

// Run resolves the configured room, initialises crypto, and blocks on
// SyncWithContext until ctx is cancelled. The crypto init + sync loop is
// wrapped in a reconnect loop with capped exponential backoff so a
// transient failure on boot (e.g. Tailscale MagicDNS not ready yet) does
// not permanently disable the frontend. mautrix reconnects internally
// within a single sync loop; the retry here covers the loop exiting.
//
// resolveRoom is called once and cached: the room ID is stable for the
// life of the bot, and re-resolving an alias on every retry is wasted
// work that could itself fail.
// Run is the entry point for the C2 retry loop. Discovery + client
// construction are part of the first attempt and may be retried on
// transient failure. ctx cancellation aborts the loop.
func (b *Bot) Run(ctx context.Context) error {
	return reconnect.RunWithBackoff(ctx, "matrix", reconnect.Options{
		InitialDelay:   1 * time.Second,
		MaxDelay:       60 * time.Second,
		Factor:         2.0,
		JitterFraction: 0.2,
		Logger: func(name string, attempt int, err error, nextDelay time.Duration) {
			log.Printf("%s: connect attempt %d failed: %v; retrying in %s", name, attempt, err, nextDelay)
		},
	}, b.runOnce)
}

// runOnce performs the per-attempt connect path: build a fresh mautrix
// client (every attempt, because the previous attempt's crypto helper
// closes the SQLite database and re-using the client would point its
// StateStore at a closed connection), resolve the room (cached after
// the first success), initialise the crypto helper, mark the bot ready,
// and then block on SyncWithContext until ctx is cancelled.
//
// Per-attempt client construction also means the syncer handlers are
// registered once per attempt against a fresh syncer, which avoids the
// duplicate-dispatch bug that would occur if we re-registered them
// against a reused syncer (DefaultSyncer has no Reset).
func (b *Bot) runOnce(ctx context.Context) error {
	if err := b.connectClient(); err != nil {
		return err
	}

	if b.roomID == "" {
		if err := b.resolveRoom(ctx); err != nil {
			return fmt.Errorf("resolving room: %w", err)
		}
	}

	cryptoDB := b.cfg.CryptoDB
	if cryptoDB == "" {
		cryptoDB = filepath.Join(b.stateDir, "matrix-crypto.db")
	}

	helper, err := cryptohelper.NewCryptoHelper(b.realClient, []byte(b.cfg.PickleKey), cryptoDB)
	if err != nil {
		return fmt.Errorf("creating crypto helper: %w", err)
	}
	if err := helper.Init(ctx); err != nil {
		// Close the helper so its SQLite store is not leaked. The
		// next attempt will rebuild the client and helper from scratch.
		if cerr := helper.Close(); cerr != nil {
			log.Printf("matrix: closing crypto helper after failed init: %v", cerr)
		}
		return fmt.Errorf("initialising crypto helper: %w", err)
	}
	b.realClient.Crypto = helper

	b.markReady()

	syncErr := b.realClient.SyncWithContext(ctx)
	if cerr := helper.Close(); cerr != nil {
		log.Printf("matrix: closing crypto helper: %v", cerr)
	}
	if ctx.Err() != nil {
		return nil
	}
	return syncErr
}

// connectClient performs the homeserver discovery, builds the mautrix
// client, and registers the sync handlers exactly once. It is called
// from runOnce on the first attempt, and its result is reused across
// subsequent retries. A failure here bubbles up to the retry loop.
func (b *Bot) connectClient() error {
	homeserverURL, err := resolveHomeserverURL(b.cfg.Homeserver, discoverClientAPI)
	if err != nil {
		return fmt.Errorf("matrix client: %w", err)
	}
	if homeserverURL != b.cfg.Homeserver {
		log.Printf("matrix: homeserver %q resolved to %s", b.cfg.Homeserver, homeserverURL)
	}

	client, err := mautrix.NewClient(homeserverURL, id.UserID(b.cfg.UserID), b.cfg.AccessToken)
	if err != nil {
		return fmt.Errorf("matrix client: %w", err)
	}
	client.DeviceID = id.DeviceID(b.cfg.DeviceID)
	// Route mautrix's internal logs to stderr so they land alongside the
	// standard library log output the rest of mezzaops uses.
	client.Log = zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()

	syncer, ok := client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return fmt.Errorf("matrix client: unexpected syncer type %T", client.Syncer)
	}
	syncer.OnEventType(event.EventMessage, b.handleMessage)
	syncer.OnEventType(event.StateMember, b.handleInvite)

	b.connMu.Lock()
	b.client = client
	b.realClient = client
	b.connMu.Unlock()
	return nil
}

// publishRoomID stores the resolved roomID under the connection lock so
// concurrent readers in PostMessage / event handlers see a consistent
// snapshot.
func (b *Bot) publishRoomID(r id.RoomID) {
	b.connMu.Lock()
	b.roomID = r
	b.connMu.Unlock()
}

// resolveRoom turns either a room ID (!) or a room alias (#) into the
// canonical id.RoomID stored on the bot. Anything else is a config error.
func (b *Bot) resolveRoom(ctx context.Context) error {
	room := b.cfg.Room
	switch {
	case strings.HasPrefix(room, "!"):
		b.publishRoomID(id.RoomID(room))
		return nil
	case strings.HasPrefix(room, "#"):
		b.connMu.Lock()
		client := b.client
		b.connMu.Unlock()
		if client == nil {
			return fmt.Errorf("resolving alias %q: client not initialised", room)
		}
		resp, err := client.ResolveAlias(ctx, id.RoomAlias(room))
		if err != nil {
			return fmt.Errorf("resolving alias %q: %w", room, err)
		}
		b.publishRoomID(resp.RoomID)
		return nil
	default:
		return fmt.Errorf("invalid room %q: must start with '!' (room ID) or '#' (alias)", room)
	}
}

// PostMessage renders the markdown to HTML and sends it to the configured
// room. mautrix transparently encrypts when client.Crypto is set and the room
// is encrypted. Errors are logged, not returned, since notifier callers don't
// have a place to surface them.
//
// Drops the message if called before Run has completed setup: client is nil
// when mautrix.NewClient failed, and roomID is unset until resolveRoom runs.
// The bot is appended to the manager's notifier list during app.New, so
// either condition is reachable before Run finishes (or at all, if it
// errored). The C3 readiness timeout explicitly allows notifications
// before all frontends are connected, so this race is a real possibility.
//
// The (client, roomID) pair is snapshotted under connMu so concurrent
// retry-driven teardown cannot leave PostMessage using a half-updated
// pair (a client whose roomID has been cleared, or vice versa).
func (b *Bot) PostMessage(ctx context.Context, message string) {
	b.connMu.Lock()
	client := b.client
	roomID := b.roomID
	b.connMu.Unlock()

	if client == nil || roomID == "" {
		log.Printf("matrix: PostMessage called before bot is ready, dropping message")
		return
	}
	content := format.RenderMarkdown(message, true, false)
	content.MsgType = event.MsgText
	content.Body = message
	content.Format = event.FormatHTML
	if _, err := client.SendMessageEvent(ctx, roomID, event.EventMessage, content); err != nil {
		log.Printf("matrix: send message: %v", err)
	}
}

// handleMessage filters to the configured room, ignores our own messages,
// parses the command, and posts the response back.
func (b *Bot) handleMessage(ctx context.Context, evt *event.Event) {
	b.connMu.Lock()
	roomID := b.roomID
	b.connMu.Unlock()
	if evt.RoomID != roomID {
		return
	}
	if evt.Sender == b.userID {
		return
	}
	mec, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok || mec == nil {
		return
	}
	cmd := ParseCommand(mec.Body, b.cfg.CommandPrefix)
	if cmd == nil {
		return
	}
	b.PostMessage(ctx, b.dispatchCommand(cmd))
}

// handleInvite auto-joins only when the invite is for our user and the
// configured room. Invites to any other room are ignored.
func (b *Bot) handleInvite(ctx context.Context, evt *event.Event) {
	b.connMu.Lock()
	roomID := b.roomID
	b.connMu.Unlock()
	if evt.RoomID != roomID {
		return
	}
	if evt.GetStateKey() != string(b.userID) {
		return
	}
	mec, ok := evt.Content.Parsed.(*event.MemberEventContent)
	if !ok || mec == nil {
		return
	}
	if mec.Membership != event.MembershipInvite {
		return
	}
	b.connMu.Lock()
	client := b.client
	b.connMu.Unlock()
	if client == nil {
		return
	}
	if _, err := client.JoinRoomByID(ctx, evt.RoomID); err != nil {
		log.Printf("matrix: join room %s: %v", evt.RoomID, err)
	}
}

// dispatchCommand routes a parsed command to the manager and returns a
// markdown response.
func (b *Bot) dispatchCommand(cmd *Command) string {
	switch strings.ToLower(cmd.Action) {
	case "status":
		if cmd.Service == "" {
			return formatStatusOverview(b.manager.GetAllStates())
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
		if b.confirm == nil {
			return "Confirm handler not configured."
		}
		if !b.confirm.Confirm(cmd.Service) {
			return fmt.Sprintf("No pending deploy confirmation for **%s** (or it expired).", cmd.Service)
		}
		return fmt.Sprintf("Confirmed deploy for **%s**.", cmd.Service)

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

// formatStatusOverview formats service states as a markdown list, sorted by
// service name so the output is deterministic.
func formatStatusOverview(states map[string]service.ServiceState) string {
	if len(states) == 0 {
		return "No services configured."
	}
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString("**Service Status**\n")
	for _, name := range names {
		state := states[name]
		fmt.Fprintf(&sb, "- **%s**: %s", name, state.Status)
		if state.LastResult != "" {
			fmt.Fprintf(&sb, " (last: %s)", state.LastResult)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
