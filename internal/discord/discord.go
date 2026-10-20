package discord

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/shishberg/mezzaops/internal/reconnect"
)

// Config holds Discord bot configuration.
type Config struct {
	Token     string
	GuildID   string
	ChannelID string
}

// ServiceManager is the interface the Discord frontend needs from the manager.
// Uses interface segregation — only the methods Discord actually calls.
type ServiceManager interface {
	Do(name, op string) string
	RequestDeploy(name string) error
	StartAll()
	StopAll()
	Reload() error
	ServiceNames() []string
	CountRunning() (int, int)
	SetOnChange(fn func(name, event string))
}

// Bot is the Discord frontend.
type Bot struct {
	cfg     Config
	manager ServiceManager
	session *discordgo.Session

	mu        sync.Mutex
	commands  []*discordgo.ApplicationCommand
	readyCh   chan struct{} // closed once the first setCommands succeeds
	readyOnce sync.Once
}

// New creates a Discord bot. Does not connect yet — call Run() for that.
func New(cfg Config, manager ServiceManager) *Bot {
	return &Bot{
		cfg:     cfg,
		manager: manager,
		readyCh: make(chan struct{}),
	}
}

// Ready returns a channel that is closed once the first setCommands
// registration succeeds. Mirrors the other frontends' Ready() so the
// app-layer C3 bounded readiness wait can treat all three frontends
// uniformly. The channel is closed exactly once across all retry attempts;
// the second markReady is a no-op.
func (b *Bot) Ready() <-chan struct{} {
	return b.readyCh
}

// markReady closes readyCh exactly once; later calls are no-ops.
func (b *Bot) markReady() {
	b.readyOnce.Do(func() { close(b.readyCh) })
}

// Notifier returns a Notifier that sends messages to the bot's channel using
// the bot's session. Because the session is only created in Run(), the notifier
// resolves it lazily — messages sent before Run() are logged to stdout.
//
// The session is snapshotted under b.mu on every send so a retry in Run
// that closes the old session and swaps in a new one cannot race with an
// in-flight notifier call.
func (b *Bot) Notifier() *Notifier {
	return &Notifier{
		channelID: b.cfg.ChannelID,
		sendFunc: func(msg string) {
			sess := b.currentSession()
			if sess == nil || b.cfg.ChannelID == "" {
				log.Println(msg)
				return
			}
			if _, err := sess.ChannelMessageSend(b.cfg.ChannelID, msg); err != nil {
				log.Printf("discord send error: %v", err)
			}
		},
	}
}

// currentSession returns b.session under the lock, or nil if no session
// is currently set. The result is a snapshot the caller can use without
// worrying about a concurrent Run retry.
func (b *Bot) currentSession() *discordgo.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

// setSession sets b.session under the lock. Used by runOnce to publish
// a freshly opened session and to clear it on failure.
func (b *Bot) setSession(s *discordgo.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.session = s
}

// Run connects to Discord, registers commands, and blocks until ctx is done.
//
// The full connect path (discordgo.New + Open + setCommands + status
// goroutine + reconnect handlers) is wrapped in a reconnect loop with
// capped exponential backoff so a transient failure on boot (e.g.
// Tailscale MagicDNS not ready yet) does not permanently disable the
// frontend. The session is closed and recreated on each attempt. The
// status goroutine and handlers are started only after a successful
// setCommands, so a failed attempt cannot leak goroutines.
//
// readyCh is closed on the first successful setCommands and not
// re-closed on subsequent retries — by then the C3 app-layer readiness
// wait has already fired, and the gate is past.
func (b *Bot) Run(ctx context.Context) error {
	return reconnect.RunWithBackoff(ctx, "discord", reconnect.Options{}, b.runOnce)
}

// runOnce performs one connect attempt: build a session, open it,
// register commands, set up presence and interaction handlers, and
// block until ctx is cancelled. On any failure the session is closed
// and the error is returned for the retry loop to handle.
//
// The session is held in the local `session` variable and only published
// to b.session via setSession after it is fully open. The status
// goroutine and AddHandler calls capture the local session directly, so
// they are not affected by a concurrent retry that swaps b.session.
func (b *Bot) runOnce(ctx context.Context) error {
	session, err := discordgo.New("Bot " + b.cfg.Token)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}

	if err := session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	b.setSession(session)

	// Build and register commands first; if registration fails we want to
	// bail before the status goroutine and other long-lived handlers are
	// started, so a C2 retry of Run doesn't accumulate orphaned goroutines.
	b.rebuildCommands()

	b.mu.Lock()
	cmds := b.commands
	b.mu.Unlock()

	if err := b.setCommands(session, cmds); err != nil {
		_ = session.Close()
		b.setSession(nil)
		return fmt.Errorf("discord register commands: %w", err)
	}

	// First successful connect — close readyCh exactly once.
	b.markReady()

	// Set up presence status via a channel + goroutine so counter reads are
	// always serialized after the state change that triggered them.
	statusCh := make(chan statusEvent, 16)

	// The status goroutine captures the local `session` directly, not
	// b.session, so a concurrent retry that swaps b.session cannot
	// affect this goroutine's view of the session.
	go b.runStatusLoop(ctx, session, statusCh)

	b.manager.SetOnChange(func(name, event string) {
		select {
		case statusCh <- statusEvent{name, event}:
		default: // drop if full — next event will refresh
		}
	})

	// Re-set presence on connect/reconnect/resume — Discord doesn't persist it.
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		statusCh <- statusEvent{"", "connected"}
	})
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Resumed) {
		statusCh <- statusEvent{"", "reconnected"}
	})

	// Handle interactions.
	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.ApplicationCommandData().Name != "ops" {
			return
		}
		resp := b.routeInteraction(i)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: resp,
			},
		})
	})

	log.Println("Discord bot running.")

	// Block until context is cancelled.
	<-ctx.Done()

	log.Println("Discord bot shutting down.")

	// Deregister commands (best-effort) and close session.
	_ = b.setCommands(session, nil)
	_ = session.Close()
	b.setSession(nil)

	return nil
}

// statusEvent is one update for the presence goroutine. The presence
// goroutine drains queued events and only the latest matters.
type statusEvent struct {
	name, event string
}

// runStatusLoop drives the presence status updates for one session. It
// runs until ctx is cancelled. Captures the session parameter directly
// rather than reading b.session, so a concurrent retry that swaps the
// field does not affect this goroutine.
func (b *Bot) runStatusLoop(ctx context.Context, session *discordgo.Session, statusCh chan statusEvent) {
	var lastStatus string
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case ev := <-statusCh:
			// Drain queued events — only the latest matters.
			for {
				select {
				case newer := <-statusCh:
					ev = newer
				default:
					goto drained
				}
			}
		drained:
			running, total := b.manager.CountRunning()
			lastStatus = fmt.Sprintf("%d/%d | %s %s", running, total, ev.name, ev.event)
			_ = session.UpdateGameStatus(0, lastStatus)
		case <-ticker.C:
			if lastStatus == "" {
				running, total := b.manager.CountRunning()
				lastStatus = fmt.Sprintf("%d/%d tasks running", running, total)
			}
			_ = session.UpdateGameStatus(0, lastStatus)
		case <-ctx.Done():
			return
		}
	}
}

// setCommands registers (or deregisters, when cmds is nil) the bot's
// slash commands with Discord. It guards against partial-open sessions
// where the WebSocket is connected but the Ready event has not yet
// populated State.User — the previous inline dereference would panic the
// whole process. Returning an error here lets the C2 retry loop take over.
func (b *Bot) setCommands(sess *discordgo.Session, cmds []*discordgo.ApplicationCommand) error {
	if sess == nil {
		return fmt.Errorf("discord: nil session")
	}
	if sess.State == nil {
		return fmt.Errorf("discord: session state is nil")
	}
	if sess.State.User == nil {
		return fmt.Errorf("discord: session state user is nil (Ready not yet received)")
	}
	_, err := sess.ApplicationCommandBulkOverwrite(sess.State.User.ID, b.cfg.GuildID, cmds)
	return err
}

// rebuildCommands rebuilds the slash command list from current service names.
func (b *Bot) rebuildCommands() {
	names := b.manager.ServiceNames()
	cmds := buildCommands(names)
	b.mu.Lock()
	b.commands = cmds
	b.mu.Unlock()
}

// routeInteraction inspects the interaction data and calls the appropriate
// manager method. Returns the response string.
func (b *Bot) routeInteraction(i *discordgo.InteractionCreate) string {
	var opOpt, taskOpt *discordgo.ApplicationCommandInteractionDataOption
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Type == discordgo.ApplicationCommandOptionSubCommandGroup {
			opOpt = opt
			break
		}
		if opt.Type == discordgo.ApplicationCommandOptionSubCommand {
			switch opt.Name {
			case "reload":
				err := b.manager.Reload()
				// Rebuild and re-register commands after reload.
				b.rebuildCommands()
				b.mu.Lock()
				cmds := b.commands
				b.mu.Unlock()
				_ = b.setCommands(b.currentSession(), cmds)
				if err != nil {
					return "Config reload error: " + err.Error()
				}
				return "Config reloaded"
			case "start-all":
				b.manager.StartAll()
				return "all tasks starting"
			case "stop-all":
				b.manager.StopAll()
				return "all tasks stopping"
			}
		}
	}
	if opOpt == nil {
		return "operation required"
	}
	for _, opt := range opOpt.Options {
		if opt.Type == discordgo.ApplicationCommandOptionSubCommand {
			taskOpt = opt
			break
		}
	}
	if taskOpt == nil {
		return "task required"
	}

	svcName := taskOpt.Name
	opName := opOpt.Name

	// deploy is special — it uses RequestDeploy instead of Do.
	if opName == "deploy" {
		if err := b.manager.RequestDeploy(svcName); err != nil {
			return fmt.Sprintf("Deploy error: %s", err.Error())
		}
		return fmt.Sprintf("Deploy requested for %s", svcName)
	}

	result := b.manager.Do(svcName, opName)
	if opName == "logs" {
		// Stop ``` in log output from closing the fence early.
		safe := strings.ReplaceAll(result, "```", "``")
		return fmt.Sprintf("%s:\n```\n%s\n```", svcName, safe)
	}
	return fmt.Sprintf("%s: %s", svcName, result)
}

// buildCommands creates the /ops application command structure for the given
// service names.
func buildCommands(serviceNames []string) []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "ops",
			Description: "MezzaOps",
			Type:        discordgo.ChatApplicationCommand,
			Options: []*discordgo.ApplicationCommandOption{
				subCommand("reload", "Reload config"),
				subCommand("start-all", "Start all tasks"),
				subCommand("stop-all", "Stop all tasks"),
				subCommandGroup("start", "Start", serviceNames),
				subCommandGroup("stop", "Stop", serviceNames),
				subCommandGroup("restart", "Restart", serviceNames),
				subCommandGroup("logs", "Logs", serviceNames),
				subCommandGroup("status", "Status", serviceNames),
				subCommandGroup("pull", "git pull", serviceNames),
				subCommandGroup("deploy", "Deploy", serviceNames),
			},
		},
	}
}

func subCommand(name, desc string) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Name:        name,
		Description: desc,
		Type:        discordgo.ApplicationCommandOptionSubCommand,
	}
}

func subCommandGroup(name, desc string, serviceNames []string) *discordgo.ApplicationCommandOption {
	aco := &discordgo.ApplicationCommandOption{
		Name:        name,
		Description: desc,
		Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
	}
	for _, sn := range serviceNames {
		aco.Options = append(aco.Options, &discordgo.ApplicationCommandOption{
			Name:        sn,
			Description: sn,
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		})
	}
	return aco
}
