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

	"github.com/rs/zerolog"
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
	client       matrixClient
	realClient   *mautrix.Client
	newClientErr error
	stateDir     string
	userID       id.UserID
	roomID       id.RoomID
	readyCh      chan struct{}
}

// New constructs a Bot. It does no I/O — the real connection, alias
// resolution, and crypto bootstrap happen in Run. stateDir is used to derive
// the default crypto database path when cfg.CryptoDB is empty.
//
// A malformed homeserver URL is recorded and surfaces from Run; matching the
// Mattermost frontend, construction is cheap and cannot itself fail.
func New(cfg Config, stateDir string, manager ServiceManager) *Bot {
	prefix := cfg.CommandPrefix
	if prefix == "" {
		prefix = "!mezzaops"
	}
	cfg.CommandPrefix = prefix

	bot := &Bot{
		cfg:      cfg,
		manager:  manager,
		stateDir: stateDir,
		userID:   id.UserID(cfg.UserID),
		readyCh:  make(chan struct{}),
	}

	client, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		bot.newClientErr = err
		return bot
	}
	client.DeviceID = id.DeviceID(cfg.DeviceID)
	// Route mautrix's internal logs to stderr so they land alongside the
	// standard library log output the rest of mezzaops uses.
	client.Log = zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	bot.client = client
	bot.realClient = client
	return bot
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

// Run resolves the configured room, initialises crypto, registers sync
// handlers, and blocks on SyncWithContext until ctx is cancelled. mautrix
// reconnects internally; an error here means the loop exited for good.
func (b *Bot) Run(ctx context.Context) error {
	if b.newClientErr != nil {
		return fmt.Errorf("matrix client: %w", b.newClientErr)
	}
	if b.realClient == nil {
		return fmt.Errorf("matrix client not initialised")
	}

	if err := b.resolveRoom(ctx); err != nil {
		return fmt.Errorf("resolving room: %w", err)
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
		return fmt.Errorf("initialising crypto helper: %w", err)
	}
	b.realClient.Crypto = helper

	syncer, ok := b.realClient.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return fmt.Errorf("unexpected syncer type %T", b.realClient.Syncer)
	}
	syncer.OnEventType(event.EventMessage, b.handleMessage)
	syncer.OnEventType(event.StateMember, b.handleInvite)

	close(b.readyCh)

	syncErr := b.realClient.SyncWithContext(ctx)
	if cerr := helper.Close(); cerr != nil {
		log.Printf("matrix: closing crypto helper: %v", cerr)
	}
	if ctx.Err() != nil {
		return nil
	}
	return syncErr
}

// resolveRoom turns either a room ID (!) or a room alias (#) into the
// canonical id.RoomID stored on the bot. Anything else is a config error.
func (b *Bot) resolveRoom(ctx context.Context) error {
	room := b.cfg.Room
	switch {
	case strings.HasPrefix(room, "!"):
		b.roomID = id.RoomID(room)
		return nil
	case strings.HasPrefix(room, "#"):
		resp, err := b.client.ResolveAlias(ctx, id.RoomAlias(room))
		if err != nil {
			return fmt.Errorf("resolving alias %q: %w", room, err)
		}
		b.roomID = resp.RoomID
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
// when mautrix.NewClient failed in New, and roomID is unset until resolveRoom
// runs. The bot is appended to the manager's notifier list during app.New, so
// either condition is reachable before Run finishes (or at all, if it
// errored).
func (b *Bot) PostMessage(ctx context.Context, message string) {
	if b.client == nil || b.roomID == "" {
		log.Printf("matrix: PostMessage called before bot is ready, dropping message")
		return
	}
	content := format.RenderMarkdown(message, true, false)
	content.MsgType = event.MsgText
	content.Body = message
	content.Format = event.FormatHTML
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, content); err != nil {
		log.Printf("matrix: send message: %v", err)
	}
}

// handleMessage filters to the configured room, ignores our own messages,
// parses the command, and posts the response back.
func (b *Bot) handleMessage(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID {
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
	if evt.RoomID != b.roomID {
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
	if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
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
