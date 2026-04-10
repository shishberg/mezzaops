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

// DeployFailed posts a deploy-failed message with the failed step and output.
func (n *Notifier) DeployFailed(name, step, output string) {
	msg := fmt.Sprintf("Deploy of **%s** failed at step `%s`.\n```\n%s\n```", name, step, output)
	n.send(msg)
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
