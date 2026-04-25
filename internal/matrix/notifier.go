package matrix

import (
	"context"
	"fmt"

	"github.com/shishberg/mezzaops/internal/service"
)

// matrixMaxRunes caps the rune count of a notification posted to a Matrix
// room. Matrix homeservers typically accept event JSON up to 64 KiB; this is
// a conservative budget that leaves room for envelope overhead and HTML
// formatting expansion.
const matrixMaxRunes = 32 * 1024

// messageSender is the slice of Bot used by Notifier. Defining it here keeps
// notifier tests independent of the rest of Bot's surface.
type messageSender interface {
	PostMessage(ctx context.Context, message string)
}

var _ service.Notifier = (*Notifier)(nil)

// Notifier implements service.Notifier by posting markdown messages to a
// configured Matrix room via Bot.PostMessage.
type Notifier struct {
	sender messageSender
}

// NewNotifier creates a Notifier that posts events through sender.
func NewNotifier(sender messageSender) *Notifier {
	return &Notifier{sender: sender}
}

// ServiceEvent posts a service lifecycle event.
func (n *Notifier) ServiceEvent(name, ev string) {
	n.sender.PostMessage(context.Background(), fmt.Sprintf("`%s` %s.", name, ev))
}

// DeployStarted posts a deploy-started notification.
func (n *Notifier) DeployStarted(name string) {
	n.sender.PostMessage(context.Background(), fmt.Sprintf("Deploying `%s`...", name))
}

// DeploySucceeded posts a deploy-succeeded notification.
func (n *Notifier) DeploySucceeded(name, _ string) {
	n.sender.PostMessage(context.Background(), fmt.Sprintf("Deploy of `%s` succeeded.", name))
}

// DeployFailed posts a deploy-failed notification with the failed step and
// output. The output is truncated from the head (keeping the tail, where the
// real error usually is) so the whole message stays under matrixMaxRunes.
func (n *Notifier) DeployFailed(name, step, output string) {
	const format = "Deploy of `%s` failed at step `%s`.\n```\n%s\n```"
	scaffolding := fmt.Sprintf(format, name, step, "")
	budget := matrixMaxRunes - len([]rune(scaffolding))
	truncated := service.TruncateTailToRuneBudget(output, budget)
	n.sender.PostMessage(context.Background(), fmt.Sprintf(format, name, step, truncated))
}

// WebhookReceived posts a notification describing an incoming webhook that
// matched the named service.
func (n *Notifier) WebhookReceived(name string, info service.WebhookInfo) {
	n.sender.PostMessage(context.Background(), info.FormatMessage(fmt.Sprintf("`%s`", name)))
}
