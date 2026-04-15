package discord

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/shishberg/mezzaops/internal/service"
)

// Notifier implements service.Notifier by posting messages to a Discord channel.
type Notifier struct {
	session   *discordgo.Session
	channelID string

	// sendFunc overrides the default send behavior for testing.
	// When nil, uses the Discord session to send messages.
	sendFunc func(msg string)
}

// NewNotifier creates a Notifier that posts to the given Discord channel.
// If session is nil or channelID is empty, messages are logged to stdout.
func NewNotifier(session *discordgo.Session, channelID string) *Notifier {
	return &Notifier{
		session:   session,
		channelID: channelID,
	}
}

// ServiceEvent posts a service event message.
func (n *Notifier) ServiceEvent(name, event string) {
	n.send(fmt.Sprintf("**%s**: %s", name, event))
}

// DeployStarted posts a deploy-started message.
func (n *Notifier) DeployStarted(name string) {
	n.send(fmt.Sprintf("Deploying **%s**...", name))
}

// DeploySucceeded posts a deploy-succeeded message.
func (n *Notifier) DeploySucceeded(name, output string) {
	n.send(fmt.Sprintf("Deploy of **%s** succeeded.", name))
}

// discordMessageRuneLimit is the maximum size of a single Discord message.
// See https://discord.com/developers/docs/resources/channel#create-message --
// the `content` field is capped at 2000 characters. The discordgo SDK
// (v0.27.0) does not expose this as a named constant.
const discordMessageRuneLimit = 2000

// DeployFailed posts a deploy-failed message with the failed step and output.
// The output is truncated from the head (keeping the tail, where errors tend
// to be) so the whole message fits within Discord's 2000-character limit.
func (n *Notifier) DeployFailed(name, step, output string) {
	const format = "Deploy of **%s** failed at step `%s`.\n```\n%s\n```"
	scaffolding := fmt.Sprintf(format, name, step, "")
	budget := discordMessageRuneLimit - len([]rune(scaffolding))
	truncated := service.TruncateTailToRuneBudget(output, budget)
	n.send(fmt.Sprintf(format, name, step, truncated))
}

// WebhookReceived posts a notification describing an incoming webhook that
// matched the named service.
func (n *Notifier) WebhookReceived(name string, info service.WebhookInfo) {
	n.send(info.FormatMessage(fmt.Sprintf("**%s**", name)))
}

func (n *Notifier) send(msg string) {
	if n.sendFunc != nil {
		n.sendFunc(msg)
		return
	}
	if n.session == nil || n.channelID == "" {
		log.Println(msg)
		return
	}
	if _, err := n.session.ChannelMessageSend(n.channelID, msg); err != nil {
		log.Printf("discord send error: %v", err)
	}
}
