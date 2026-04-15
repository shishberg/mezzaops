package mattermost

import (
	"context"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/shishberg/mezzaops/internal/service"
)

// Compile-time check that Notifier implements service.Notifier.
var _ service.Notifier = (*Notifier)(nil)

// Notifier implements service.Notifier by posting to Mattermost.
type Notifier struct {
	bot *Bot
}

// NewNotifier creates a Notifier that posts events to the bot's channel.
func NewNotifier(bot *Bot) *Notifier {
	return &Notifier{bot: bot}
}

// ServiceEvent posts a service lifecycle event.
func (n *Notifier) ServiceEvent(name, event string) {
	n.bot.PostMessage(context.Background(), fmt.Sprintf("`%s` %s.", name, event))
}

// DeployStarted posts a deploy-started notification.
func (n *Notifier) DeployStarted(name string) {
	n.bot.PostMessage(context.Background(), fmt.Sprintf("Deploying `%s`...", name))
}

// DeploySucceeded posts a deploy-succeeded notification.
func (n *Notifier) DeploySucceeded(name, output string) {
	n.bot.PostMessage(context.Background(), fmt.Sprintf("Deploy of `%s` succeeded.", name))
}

// DeployFailed posts a deploy-failed notification with the failed step and output.
// The output is truncated from the head (keeping the tail, where errors tend to
// be) so the whole message fits within Mattermost's server-side rune limit.
func (n *Notifier) DeployFailed(name, step, output string) {
	const format = "Deploy of `%s` failed at step `%s`.\n```\n%s\n```"
	scaffolding := fmt.Sprintf(format, name, step, "")
	budget := model.PostMessageMaxRunesV2 - len([]rune(scaffolding))
	truncated := service.TruncateTailToRuneBudget(output, budget)
	n.bot.PostMessage(context.Background(), fmt.Sprintf(format, name, step, truncated))
}

// WebhookReceived posts a notification describing an incoming webhook that
// matched the named service.
func (n *Notifier) WebhookReceived(name string, info service.WebhookInfo) {
	n.bot.PostMessage(context.Background(), info.FormatMessage(fmt.Sprintf("`%s`", name)))
}
