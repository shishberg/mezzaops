// Package matrix is a Matrix chat frontend for mezzaops, parallel to the
// Mattermost and Discord frontends. It listens for command-prefixed messages
// in one configured room, dispatches them to the service manager, and posts
// notifications to the same room. The Matrix runtime — sync loop, crypto,
// invite handling, dispatch — comes from github.com/shishberg/matrixbot;
// this package adds mezzaops-specific glue (homeserver discovery, alias
// resolution, command parsing, dispatch to the service manager).
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
	"github.com/shishberg/matrixbot"
	"github.com/shishberg/mezzaops/internal/service"
	"maunium.net/go/mautrix"
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

// aliasResolver is the slice of *mautrix.Client used to turn a #alias into a
// !roomID. Defined as an interface so tests can supply a fake without a live
// homeserver.
type aliasResolver interface {
	ResolveAlias(ctx context.Context, alias id.RoomAlias) (*mautrix.RespAliasResolve, error)
}

// validCommands is listed in the unknown-command response.
var validCommands = []string{
	"status", "start", "stop", "restart", "logs", "pull",
	"deploy", "confirm", "reload", "start-all", "stop-all",
}

// Bot is the Matrix frontend. It wraps a *matrixbot.Bot — the runtime that
// owns the mautrix client, sync loop, crypto helper, and event dispatch — and
// adds mezzaops-specific behaviour (alias resolution, command dispatch,
// outbound post helper).
type Bot struct {
	cfg      Config
	manager  ServiceManager
	confirm  ConfirmHandler
	stateDir string

	// resolvedHomeserver is the URL we hand to matrixbot.NewBot. Computed at
	// construction time via /.well-known/matrix/client when cfg.Homeserver is
	// a bare server name.
	resolvedHomeserver string
	// newErr captures any setup failure encountered in New (homeserver
	// discovery). Surfaced from Run so the surface matches the Mattermost
	// frontend, where construction never returns an error.
	newErr error

	// aliasClient resolves room aliases to room IDs before the bot starts.
	// matrixbot doesn't do alias resolution, so we do it here. A standalone
	// mautrix.Client is cheap to build for this single call. Tests inject a
	// fake.
	aliasClient aliasResolver

	// roomID is the resolved !room ID, populated in Run by resolveRoom.
	// Used by PostMessage to address outbound notifications.
	roomID id.RoomID

	// matrixBot is the underlying runtime. Constructed lazily in Run, since
	// matrixbot.NewBot needs the resolved room ID for AutoJoinRooms and the
	// route registration.
	matrixBot *matrixbot.Bot

	readyCh chan struct{}
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

// New constructs a Bot. The actual matrixbot runtime, alias resolution, and
// crypto bootstrap happen in Run; the only I/O performed here is the optional
// /.well-known/matrix/client lookup when cfg.Homeserver is a bare server
// name. stateDir is used to derive the default crypto database path when
// cfg.CryptoDB is empty.
//
// A malformed homeserver URL or a failed discovery is recorded and surfaces
// from Run; matching the Mattermost frontend, construction itself does not
// return an error.
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
		readyCh:  make(chan struct{}),
	}

	homeserverURL, err := resolveHomeserverURL(cfg.Homeserver, discoverClientAPI)
	if err != nil {
		bot.newErr = err
		return bot
	}
	if homeserverURL != cfg.Homeserver {
		log.Printf("matrix: homeserver %q resolved to %s", cfg.Homeserver, homeserverURL)
	}
	bot.resolvedHomeserver = homeserverURL

	// Build a thin mautrix.Client for the alias-resolution call performed in
	// Run. matrixbot owns the real client used for sync and sending; this one
	// is only ever used for the single ResolveAlias roundtrip.
	client, err := mautrix.NewClient(homeserverURL, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		bot.newErr = err
		return bot
	}
	bot.aliasClient = client
	return bot
}

// SetConfirmHandler installs the deploy-confirmation handler.
func (b *Bot) SetConfirmHandler(ch ConfirmHandler) { b.confirm = ch }

// CommandPrefix returns the normalised prefix the bot listens for, so callers
// (e.g. the app's webhook confirmation prompt) can advertise the same string
// the bot will actually match.
func (b *Bot) CommandPrefix() string { return b.cfg.CommandPrefix }

// Ready closes once Run has resolved the room and constructed the underlying
// matrixbot runtime, so callers know it is safe to call PostMessage.
func (b *Bot) Ready() <-chan struct{} { return b.readyCh }

// Run resolves the configured room, hands credentials to matrixbot, registers
// the command route, and blocks on matrixbot.Bot.Run until ctx is cancelled.
// matrixbot reconnects internally; an error here means the loop exited for
// good.
func (b *Bot) Run(ctx context.Context) error {
	if b.newErr != nil {
		return fmt.Errorf("matrix client: %w", b.newErr)
	}
	if b.aliasClient == nil {
		return fmt.Errorf("matrix client not initialised")
	}

	if err := b.resolveRoom(ctx); err != nil {
		return fmt.Errorf("resolving room: %w", err)
	}

	cryptoDB := b.cfg.CryptoDB
	if cryptoDB == "" {
		cryptoDB = filepath.Join(b.stateDir, "matrix-crypto.db")
	}

	// Plumb the existing zerolog/stderr pattern into matrixbot so its mautrix
	// client logs land alongside the rest of mezzaops's stderr output.
	logger := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()

	mbBot, err := matrixbot.NewBot(matrixbot.BotConfig{
		Homeserver:    b.resolvedHomeserver,
		UserID:        id.UserID(b.cfg.UserID),
		DeviceID:      id.DeviceID(b.cfg.DeviceID),
		AccessToken:   b.cfg.AccessToken,
		PickleKey:     b.cfg.PickleKey,
		CryptoDB:      cryptoDB,
		AutoJoinRooms: []id.RoomID{b.roomID},
		Logger:        &logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrixbot: %w", err)
	}
	mbBot.RouteIn(b.roomID,
		matrixbot.CommandTrigger{Prefix: b.cfg.CommandPrefix, BotUserID: id.UserID(b.cfg.UserID)},
		matrixbot.HandlerFunc(b.handleCommand),
	)
	b.matrixBot = mbBot

	close(b.readyCh)

	return mbBot.Run(ctx)
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
		resp, err := b.aliasClient.ResolveAlias(ctx, id.RoomAlias(room))
		if err != nil {
			return fmt.Errorf("resolving alias %q: %w", room, err)
		}
		b.roomID = resp.RoomID
		return nil
	default:
		return fmt.Errorf("invalid room %q: must start with '!' (room ID) or '#' (alias)", room)
	}
}

// handleCommand is the matrixbot.Handler that wraps mezzaops's dispatchCommand.
// CommandTrigger has already stripped the prefix and trimmed whitespace, so
// req.Input is the action-and-optional-service tail (e.g. "deploy myapp").
func (b *Bot) handleCommand(_ context.Context, req matrixbot.Request) (matrixbot.Response, error) {
	fields := strings.Fields(req.Input)
	if len(fields) == 0 {
		return matrixbot.Response{}, nil
	}
	cmd := &Command{Action: fields[0]}
	if len(fields) >= 2 {
		cmd.Service = fields[1]
	}
	return matrixbot.Response{Reply: b.dispatchCommand(cmd)}, nil
}

// PostMessage sends markdown to the configured room via matrixbot.Bot.Send.
// Errors are logged, not returned, since notifier callers don't have a place
// to surface them.
//
// Drops the message if called before Run has constructed the matrixbot
// runtime: matrixBot is nil when New failed or Run hasn't reached the
// post-construction point. The bot is appended to the manager's notifier list
// during app.New, so either condition is reachable before Run completes (or
// at all, if it errored).
func (b *Bot) PostMessage(ctx context.Context, message string) {
	if b.matrixBot == nil || b.roomID == "" {
		log.Printf("matrix: PostMessage called before bot is ready, dropping message")
		return
	}
	if err := b.matrixBot.Send(ctx, b.roomID, message); err != nil {
		log.Printf("matrix: send message: %v", err)
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
