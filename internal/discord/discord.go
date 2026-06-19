package discord

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
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

	mu       sync.Mutex
	commands []*discordgo.ApplicationCommand
}

// New creates a Discord bot. Does not connect yet — call Run() for that.
func New(cfg Config, manager ServiceManager) *Bot {
	return &Bot{
		cfg:     cfg,
		manager: manager,
	}
}

// Notifier returns a Notifier that sends messages to the bot's channel using
// the bot's session. Because the session is only created in Run(), the notifier
// resolves it lazily — messages sent before Run() are logged to stdout.
func (b *Bot) Notifier() *Notifier {
	return &Notifier{
		channelID: b.cfg.ChannelID,
		sendFunc: func(msg string) {
			if b.session == nil || b.cfg.ChannelID == "" {
				log.Println(msg)
				return
			}
			if _, err := b.session.ChannelMessageSend(b.cfg.ChannelID, msg); err != nil {
				log.Printf("discord send error: %v", err)
			}
		},
	}
}

// Run connects to Discord, registers commands, and blocks until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	session, err := discordgo.New("Bot " + b.cfg.Token)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}
	b.session = session

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}

	// Build and register commands first; if registration fails we want to
	// bail before the status goroutine and other long-lived handlers are
	// started, so a C2 retry of Run doesn't accumulate orphaned goroutines.
	b.rebuildCommands()

	b.mu.Lock()
	cmds := b.commands
	b.mu.Unlock()

	if err := b.setCommands(b.session, cmds); err != nil {
		_ = b.session.Close()
		return fmt.Errorf("discord register commands: %w", err)
	}

	// Set up presence status via a channel + goroutine so counter reads are
	// always serialized after the state change that triggered them.
	type statusEvent struct {
		name, event string
	}
	statusCh := make(chan statusEvent, 16)

	go func() {
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
				_ = b.session.UpdateGameStatus(0, lastStatus)
			case <-ticker.C:
				if lastStatus == "" {
					running, total := b.manager.CountRunning()
					lastStatus = fmt.Sprintf("%d/%d tasks running", running, total)
				}
				_ = b.session.UpdateGameStatus(0, lastStatus)
			case <-ctx.Done():
				return
			}
		}
	}()

	b.manager.SetOnChange(func(name, event string) {
		select {
		case statusCh <- statusEvent{name, event}:
		default: // drop if full — next event will refresh
		}
	})

	// Re-set presence on connect/reconnect/resume — Discord doesn't persist it.
	b.session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		statusCh <- statusEvent{"", "connected"}
	})
	b.session.AddHandler(func(s *discordgo.Session, r *discordgo.Resumed) {
		statusCh <- statusEvent{"", "reconnected"}
	})

	// Handle interactions.
	b.session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
	_ = b.setCommands(b.session, nil)
	_ = b.session.Close()

	return nil
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
				_ = b.setCommands(b.session, cmds)
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
