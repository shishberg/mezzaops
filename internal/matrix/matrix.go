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
	"strings"
	"sync"

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
	manager service.ServiceManager
	confirm service.ConfirmHandler

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
	readyOnce  sync.Once
	connMu     sync.Mutex
}

// discoverFunc resolves a Matrix server name to its client-server API
// well-known document. It accepts a context so a stuck discovery (e.g.
// a black-holed Tailscale MagicDNS) is cancellable: C2's acceptance
// criteria require the retry loop to be cancellable via ctx.
type discoverFunc func(ctx context.Context, serverName string) (*mautrix.ClientWellKnown, error)

// discoverClientAPI is the package-level default used by resolveHomeserverURL.
// Tests override it; the variable is read once per runOnce call from a
// single goroutine, so tests must not call t.Parallel while mutating it.
var discoverClientAPI discoverFunc = func(ctx context.Context, serverName string) (*mautrix.ClientWellKnown, error) {
	return mautrix.DiscoverClientAPI(ctx, serverName)
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
//
// The ctx is forwarded to the discovery call so a stuck HTTP request is
// cancellable from the caller's context.
func resolveHomeserverURL(ctx context.Context, homeserver string, discover discoverFunc) (string, error) {
	if strings.Contains(homeserver, "://") {
		return homeserver, nil
	}
	wk, err := discover(ctx, homeserver)
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
func New(cfg Config, stateDir string, manager service.ServiceManager) *Bot {
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
func (b *Bot) SetConfirmHandler(ch service.ConfirmHandler) { b.confirm = ch }

// CommandPrefix returns the normalised prefix the bot listens for, so callers
// (e.g. the app's webhook confirmation prompt) can advertise the same string
// the bot will actually match.
func (b *Bot) CommandPrefix() string { return b.cfg.CommandPrefix }

// Ready closes once Run has resolved the room and finished the crypto
// bootstrap, so callers know it is safe to call PostMessage.
func (b *Bot) Ready() <-chan struct{} { return b.readyCh }

// markReady closes readyCh exactly once; later calls are no-ops.
func (b *Bot) markReady() {
	b.readyOnce.Do(func() { close(b.readyCh) })
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
	return reconnect.RunWithBackoff(ctx, "matrix", reconnect.Options{}, b.runOnce)
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
//
// Publish-late: the client is published under connMu only AFTER
// helper.Init succeeds, so PostMessage can never observe a client
// without a working Crypto helper (which would otherwise let
// SendMessageEvent send plaintext into a room the operator expects to
// be encrypted). A failed Init returns without publishing. The client is
// always cleared again before runOnce returns, so no state carries over
// between attempts.
func (b *Bot) runOnce(ctx context.Context) error {
	client, err := b.buildLocalClient(ctx)
	if err != nil {
		return err
	}

	if b.roomID == "" {
		if err := b.resolveRoomOnClient(ctx, client); err != nil {
			return fmt.Errorf("resolving room: %w", err)
		}
	}

	cryptoDB := b.cfg.CryptoDB
	if cryptoDB == "" {
		cryptoDB = filepath.Join(b.stateDir, "matrix-crypto.db")
	}

	helper, err := cryptohelper.NewCryptoHelper(client, []byte(b.cfg.PickleKey), cryptoDB)
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
	// Set Crypto on the local client BEFORE publishing. PostMessage's
	// defensive Crypto==nil check ensures that even if a future code
	// path publishes the client before this assignment, a plaintext
	// send cannot happen.
	client.Crypto = helper

	b.connMu.Lock()
	b.client = client
	b.realClient = client
	b.connMu.Unlock()

	b.markReady()

	syncErr := client.SyncWithContext(ctx)
	// Teardown: clear the published client under connMu so PostMessage
	// stops calling SendMessageEvent on it, then close the helper so
	// its SQLite store is released for the next attempt.
	b.connMu.Lock()
	b.client = nil
	b.realClient = nil
	b.connMu.Unlock()
	if cerr := helper.Close(); cerr != nil {
		log.Printf("matrix: closing crypto helper: %v", cerr)
	}
	if ctx.Err() != nil {
		return nil
	}
	return syncErr
}

// buildLocalClient performs the homeserver discovery, builds a fresh
// mautrix client, and registers the syncer handlers. The returned
// client is NOT published; the caller is responsible for publishing it
// under connMu once the crypto helper has initialised successfully.
// This is the "publish-late" approach: PostMessage will only ever see
// a client whose Crypto is set, so it cannot send plaintext into a
// room the operator expects to be encrypted.
//
// Per-attempt construction (rather than reusing the previous attempt's
// client) also avoids the duplicate-dispatch bug that mautrix's
// DefaultSyncer would otherwise produce if we re-registered handlers
// against a reused syncer (DefaultSyncer has no Reset).
func (b *Bot) buildLocalClient(ctx context.Context) (*mautrix.Client, error) {
	homeserverURL, err := resolveHomeserverURL(ctx, b.cfg.Homeserver, discoverClientAPI)
	if err != nil {
		return nil, fmt.Errorf("matrix client: %w", err)
	}
	if homeserverURL != b.cfg.Homeserver {
		log.Printf("matrix: homeserver %q resolved to %s", b.cfg.Homeserver, homeserverURL)
	}

	client, err := mautrix.NewClient(homeserverURL, id.UserID(b.cfg.UserID), b.cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("matrix client: %w", err)
	}
	client.DeviceID = id.DeviceID(b.cfg.DeviceID)
	// Route mautrix's internal logs to stderr so they land alongside the
	// standard library log output the rest of mezzaops uses.
	client.Log = zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()

	syncer, ok := client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return nil, fmt.Errorf("matrix client: unexpected syncer type %T", client.Syncer)
	}
	syncer.OnEventType(event.EventMessage, b.handleMessage)
	syncer.OnEventType(event.StateMember, b.handleInvite)
	return client, nil
}

// publishRoomID stores the resolved roomID under the connection lock so
// concurrent readers in PostMessage / event handlers see a consistent
// snapshot.
func (b *Bot) publishRoomID(r id.RoomID) {
	b.connMu.Lock()
	b.roomID = r
	b.connMu.Unlock()
}

// resolveRoomOnClient turns either a room ID (!) or a room alias (#) into
// the canonical id.RoomID stored on the bot, using the supplied client
// directly (so the call does not depend on the client being published).
// Anything else is a config error. The caller in runOnce uses the local
// client from buildLocalClient rather than b.client, because the
// publish-late invariant means b.client is still nil at this point.
func (b *Bot) resolveRoomOnClient(ctx context.Context, client matrixClient) error {
	room := b.cfg.Room
	switch {
	case strings.HasPrefix(room, "!"):
		b.publishRoomID(id.RoomID(room))
		return nil
	case strings.HasPrefix(room, "#"):
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
//
// Defensive Crypto check: the publish-late invariant in runOnce means
// b.client/b.realClient are only ever both set AND b.realClient.Crypto
// is non-nil, so this is a belt-and-braces check for the case where a
// future code path publishes the client before its Crypto helper is
// initialised. Without it, SendMessageEvent on an unencrypted client
// would send plaintext into a room the operator expects to be
// encrypted.
func (b *Bot) PostMessage(ctx context.Context, message string) {
	b.connMu.Lock()
	client := b.client
	realClient := b.realClient
	roomID := b.roomID
	b.connMu.Unlock()

	if client == nil || roomID == "" || (realClient != nil && realClient.Crypto == nil) {
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
			return service.FormatStatusOverview(b.manager.GetAllStates())
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
