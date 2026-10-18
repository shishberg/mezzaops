// Package telegram is a Telegram chat frontend for mezzaops, parallel to the
// Mattermost, Matrix and Discord frontends. It long-polls the Bot API for
// slash commands in one configured chat, dispatches them to the service
// manager, and posts notifications to the same chat. The Bot API is called
// directly over net/http; no client library is involved.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shishberg/mezzaops/internal/reconnect"
	"github.com/shishberg/mezzaops/internal/service"
)

// telegramMaxUTF16Units caps the length of a message posted to Telegram. The
// API limit is 4096 UTF-16 code units counted after entity parsing (so an
// emoji counts as 2); this leaves headroom for the scaffolding around
// truncated content.
const telegramMaxUTF16Units = 4000

const (
	defaultAPIURL         = "https://api.telegram.org"
	defaultPollTimeout    = 30 * time.Second
	defaultBackoffInitial = 1 * time.Second
	defaultBackoffMax     = 60 * time.Second

	// requestTimeout bounds every call except getUpdates, whose timeout is
	// the poll timeout plus slack so the HTTP request outlives the poll.
	requestTimeout = 30 * time.Second
	pollSlack      = 15 * time.Second

	// outboxSize bounds the send queue. When it is full new messages are
	// dropped rather than blocking the manager's notifier or a webhook.
	outboxSize = 100

	// maxSendRetries is how many times one message is retried after HTTP 429.
	// maxRetryAfter caps a single wait so a bad retry_after cannot stall the
	// queue for hours.
	maxSendRetries = 3
	maxRetryAfter  = 60 * time.Second
)

// Command is a parsed bot command.
type Command struct {
	Action  string
	Service string
}

// ParseCommand returns a Command when the first whitespace-separated token of
// text is a slash command. In groups Telegram appends @botusername to the
// command; the suffix is stripped, and a suffix naming a different bot yields
// nil so two bots in one chat don't answer each other's commands. The action
// is lower-cased and '_' is mapped to '-' (Telegram commands cannot contain
// '-', so /start_all dispatches as start-all). The second token, if any, is
// the service.
func ParseCommand(text, botUsername string) *Command {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return nil
	}
	word := fields[0][1:]
	if i := strings.IndexByte(word, '@'); i >= 0 {
		if !strings.EqualFold(word[i+1:], botUsername) {
			return nil
		}
		word = word[:i]
	}
	if word == "" {
		return nil
	}
	cmd := &Command{Action: strings.ReplaceAll(strings.ToLower(word), "_", "-")}
	if len(fields) >= 2 {
		cmd.Service = fields[1]
	}
	return cmd
}

// Config holds everything telegram.New needs to construct a Bot. The token is
// loaded from env, the rest from config.yaml.
type Config struct {
	Token  string
	ChatID int64
	// APIURL is the Bot API base URL. Empty means https://api.telegram.org.
	APIURL string
}

// validCommands is listed in the unknown-command response, in the spelling
// Telegram users type.
var validCommands = []string{
	"/status", "/start", "/stop", "/restart", "/logs", "/pull",
	"/deploy", "/confirm", "/reload", "/start_all", "/stop_all",
}

// commandDescriptions is registered with setMyCommands so clients
// autocomplete the commands.
var commandDescriptions = []struct{ command, description string }{
	{"status", "Show service status"},
	{"start", "Start a service"},
	{"stop", "Stop a service"},
	{"restart", "Restart a service"},
	{"logs", "Show a service's logs"},
	{"pull", "git pull a service"},
	{"deploy", "Deploy a service"},
	{"confirm", "Confirm a queued deploy"},
	{"reload", "Reload config"},
	{"start_all", "Start all services"},
	{"stop_all", "Stop all services"},
}

// Bot is the Telegram frontend.
type Bot struct {
	cfg     Config
	manager service.ServiceManager
	confirm service.ConfirmHandler
	http    *http.Client

	// outbox holds messages waiting to be sent. PostMessage enqueues; a single
	// sender goroutine started by Run drains it in order, so a slow Bot API
	// or a 429 never blocks the caller.
	outbox chan outbound

	// sleep waits between 429 retries. A field so tests don't wait.
	sleep func(ctx context.Context, d time.Duration)

	// pollTimeout and the backoff bounds are fields rather than constants so
	// tests don't wait 30s polls or 1s retries.
	pollTimeout    time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	// offset is the next update_id to request. It lives on the Bot, not in
	// runOnce, so a reconnect does not replay updates. Only the connect/poll
	// goroutine touches it.
	offset int64

	// drained records that the first connect has skipped updates left over
	// from before this process started. Telegram acknowledges an update only
	// on a later getUpdates with a higher offset, so an unacknowledged
	// /deploy of a self_deploy service (which restarts mezzaops) would
	// otherwise be redelivered and run again on every boot.
	drained bool

	// username is the bot's own @username, set from getMe. mu protects it
	// because runOnce writes it while handlers read it.
	mu       sync.Mutex
	username string
	botID    int64
	readyCh  chan struct{}
}

// outbound is one queued message. threadID is the forum topic to send to; zero
// means the chat's default topic.
type outbound struct {
	text     string
	threadID int64
}

// New constructs a Bot. No I/O is performed: validating the token and
// starting the poll loop happen in Run, so a failure on boot (e.g. DNS not
// ready yet) is retried rather than permanently disabling the frontend.
func New(cfg Config, manager service.ServiceManager) *Bot {
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.APIURL == "" {
		cfg.APIURL = defaultAPIURL
	}
	return &Bot{
		cfg:            cfg,
		manager:        manager,
		http:           &http.Client{},
		outbox:         make(chan outbound, outboxSize),
		sleep:          sleepCtx,
		pollTimeout:    defaultPollTimeout,
		backoffInitial: defaultBackoffInitial,
		backoffMax:     defaultBackoffMax,
		readyCh:        make(chan struct{}),
	}
}

// SetConfirmHandler installs the deploy-confirmation handler.
func (b *Bot) SetConfirmHandler(ch service.ConfirmHandler) { b.confirm = ch }

// Ready closes once getMe has succeeded, so callers know the token is valid
// and the bot is about to poll.
func (b *Bot) Ready() <-chan struct{} { return b.readyCh }

// markReady closes readyCh exactly once. Safe to call from multiple retry
// attempts; the second close is recovered.
func (b *Bot) markReady() {
	defer func() { _ = recover() }()
	close(b.readyCh)
}

// sleepCtx waits for d or until ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (b *Bot) ownID() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.botID
}

func (b *Bot) botUsername() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.username
}

// Run connects to the Bot API and polls for updates until ctx is cancelled.
// The connect path is wrapped in a reconnect loop with capped exponential
// backoff so a transient failure on boot does not permanently disable the
// frontend.
func (b *Bot) Run(ctx context.Context) error {
	// The sender starts here, not in New, so New does no I/O. Messages
	// queued before Run are sent once it starts.
	sendCtx, stopSender := context.WithCancel(ctx)
	senderDone := make(chan struct{})
	go func() { defer close(senderDone); b.sendLoop(sendCtx) }()
	defer func() { stopSender(); <-senderDone }()

	return reconnect.RunWithBackoff(ctx, "telegram", reconnect.Options{
		InitialDelay:   b.backoffInitial,
		MaxDelay:       b.backoffMax,
		Factor:         2.0,
		JitterFraction: 0.2,
		Logger: func(name string, attempt int, err error, nextDelay time.Duration) {
			log.Printf("%s: connect attempt %d failed: %v; retrying in %s", name, attempt, err, nextDelay)
		},
	}, b.runOnce)
}

// runOnce validates the token, registers the command list (best-effort),
// marks the bot ready, and polls until ctx is cancelled.
func (b *Bot) runOnce(ctx context.Context) error {
	me, err := b.getMe(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.username = me.Username
	b.botID = me.ID
	b.mu.Unlock()

	if err := b.setMyCommands(ctx); err != nil {
		log.Printf("telegram: setMyCommands: %v", err)
	}

	// Drain before marking ready: a command sent just after Ready must not be
	// mistaken for a leftover and skipped.
	if !b.drained {
		if err := b.drainPending(ctx); err != nil {
			return err
		}
		b.drained = true
	}

	b.markReady()
	log.Printf("telegram: connected as @%s, watching chat %d", me.Username, b.cfg.ChatID)

	b.pollLoop(ctx)
	return nil
}

// drainPending skips updates that arrived before this process started, by
// asking for the newest one (offset -1, no long poll) and moving the offset
// past it. This does not depend on the host clock, unlike filtering on
// message dates.
func (b *Bot) drainPending(ctx context.Context) error {
	updates, err := b.getUpdatesAt(ctx, -1, 0)
	if err != nil {
		return fmt.Errorf("draining pending updates: %w", err)
	}
	for _, u := range updates {
		b.offset = max(b.offset, u.UpdateID+1)
	}
	return nil
}

// pollLoop calls getUpdates until ctx is cancelled. Transient errors are
// retried here with their own backoff, reset after a successful poll, so
// RunWithBackoff only sees connect failures.
func (b *Bot) pollLoop(ctx context.Context) {
	delay := b.backoffInitial
	for ctx.Err() == nil {
		updates, err := b.getUpdates(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict {
				log.Printf("telegram: getUpdates conflict: a webhook is set, or another poller is running with this token: %v", err)
			} else {
				log.Printf("telegram: getUpdates: %v, retrying in %v", err, delay)
			}
			select {
			case <-time.After(delay):
				delay = min(delay*2, b.backoffMax)
			case <-ctx.Done():
				return
			}
			continue
		}
		delay = b.backoffInitial

		for _, u := range updates {
			// Advance before handling so a slow or panicking handler
			// never causes the same update to be fetched again.
			b.offset = max(b.offset, u.UpdateID+1)
			b.handleUpdate(ctx, u)
		}
	}
}

// handleUpdate filters to the configured chat, ignores the bot's own and other
// bots' messages, parses the command, and queues the response.
//
// An anonymous group admin sends as @GroupAnonymousBot (is_bot=true) with
// sender_chat set to the group itself, so a message whose sender_chat is the
// configured chat is accepted despite the bot flag.
func (b *Bot) handleUpdate(ctx context.Context, u update) {
	msg := u.Message
	if msg == nil || msg.Chat.ID != b.cfg.ChatID {
		return
	}
	anonymousAdmin := msg.SenderChat != nil && msg.SenderChat.ID == b.cfg.ChatID
	if !anonymousAdmin && msg.From != nil {
		if msg.From.IsBot {
			return
		}
		if id := b.ownID(); id != 0 && msg.From.ID == id {
			return
		}
	}
	cmd := ParseCommand(msg.Text, b.botUsername())
	if cmd == nil {
		return
	}
	var threadID int64
	if msg.IsTopicMessage {
		threadID = msg.MessageThreadID
	}
	b.enqueue(outbound{text: b.dispatchCommand(cmd), threadID: threadID})
}

// PostMessage queues a markdown message for the configured chat's default
// topic and returns immediately; a single sender goroutine started by Run
// sends it. If the queue is full the message is dropped and logged, so a slow
// or rate-limited Bot API never blocks the caller (the manager's notifier,
// or HandlePush under the webhook's 10s write timeout).
//
// The chat ID comes from config, so messages queued before Ready (or before
// Run) are sent once the sender starts rather than dropped.
func (b *Bot) PostMessage(_ context.Context, message string) {
	b.enqueue(outbound{text: message})
}

func (b *Bot) enqueue(m outbound) {
	if strings.TrimSpace(m.text) == "" {
		return
	}
	m.text = truncateTailUTF16(m.text, telegramMaxUTF16Units)
	select {
	case b.outbox <- m:
	default:
		log.Printf("telegram: send queue full, dropping message")
	}
}

// sendLoop sends queued messages in order until ctx is cancelled.
func (b *Bot) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-b.outbox:
			b.sendWithRetry(ctx, m)
		}
	}
}

// sendWithRetry delivers m, waiting out HTTP 429 (Too Many Requests) for the
// retry_after Telegram asks for, up to maxSendRetries times. Errors are
// logged, not returned, since notifier callers don't have a place to surface
// them.
func (b *Bot) sendWithRetry(ctx context.Context, m outbound) {
	for attempt := 0; ; attempt++ {
		err := b.deliver(ctx, m)
		if err == nil || ctx.Err() != nil {
			return
		}
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests && attempt < maxSendRetries {
			wait := min(max(time.Duration(apiErr.RetryAfter)*time.Second, time.Second), maxRetryAfter)
			log.Printf("telegram: rate limited, retrying in %v", wait)
			b.sleep(ctx, wait)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		log.Printf("telegram: send message: %v", err)
		return
	}
}

// deliver renders m to Telegram HTML and sends it. If Telegram rejects the
// markup (HTTP 400, "can't parse entities") the original text is resent once
// as plain text. Other failures, such as "chat not found", are returned as is.
func (b *Bot) deliver(ctx context.Context, m outbound) error {
	err := b.sendMessage(ctx, markdownToHTML(m.text), "HTML", m.threadID)
	if isEntityError(err) {
		log.Printf("telegram: send message as HTML rejected, resending as plain text: %v", err)
		err = b.sendMessage(ctx, m.text, "", m.threadID)
	}
	return err
}

func isEntityError(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Description), "parse entities")
}

// dispatchCommand routes a parsed command to the manager and returns a
// markdown response.
func (b *Bot) dispatchCommand(cmd *Command) string {
	switch cmd.Action {
	case "status":
		if cmd.Service == "" {
			return service.FormatStatusOverview(b.manager.GetAllStates())
		}
		return b.manager.Do(cmd.Service, "status")

	case "start", "stop", "restart", "pull":
		return b.manager.Do(cmd.Service, cmd.Action)

	case "logs":
		const format = "%s:\n```\n%s\n```"
		budget := telegramMaxUTF16Units - utf16Len(fmt.Sprintf(format, cmd.Service, ""))
		out := truncateTailUTF16(defuseFences(b.manager.Do(cmd.Service, "logs")), budget)
		return fmt.Sprintf(format, cmd.Service, out)

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

// defuseFences stops ``` in command or log output from closing a code fence
// early. A single pass is not enough: "````" becomes "```", so it repeats until
// no run of three backticks is left.
func defuseFences(s string) string {
	for strings.Contains(s, "```") {
		s = strings.ReplaceAll(s, "```", "``")
	}
	return s
}

// --- Bot API ---

// update, message, chat and user are the subset of the Bot API types used.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Text string `json:"text"`
	Chat chat   `json:"chat"`
	From *user  `json:"from"`
	// SenderChat is set when the message is sent on behalf of a chat: the
	// group itself for anonymous admins, or a channel.
	SenderChat      *chat `json:"sender_chat"`
	IsTopicMessage  bool  `json:"is_topic_message"`
	MessageThreadID int64 `json:"message_thread_id"`
}

type chat struct {
	ID int64 `json:"id"`
}

type user struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

// apiError is a Bot API response with ok=false (or a non-JSON error status).
type apiError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is the wait in seconds Telegram asks for on HTTP 429.
	RetryAfter int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// call POSTs params as JSON to <api_url>/bot<token>/<method> and decodes the
// result field into out (if non-nil). Transport errors are rewrapped without
// the *url.Error, whose message contains the request URL and so the token.
func (b *Bot) call(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: encoding request: %w", method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := b.cfg.APIURL + "/bot" + b.cfg.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return b.scrub(method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return b.scrub(method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return b.scrub(method, err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &apiError{Method: method, Code: resp.StatusCode, Description: http.StatusText(resp.StatusCode)}
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &apiError{Method: method, Code: code, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decoding result: %w", method, err)
		}
	}
	return nil
}

// scrub turns a transport error into one that cannot leak the token: it drops
// the *url.Error wrapper (which prints the URL) and masks the token if it
// appears anywhere in what remains.
func (b *Bot) scrub(method string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if b.cfg.Token != "" {
		msg = strings.ReplaceAll(msg, b.cfg.Token, "<token>")
	}
	return fmt.Errorf("telegram %s: %s", method, msg)
}

// me is the part of getMe's result the bot uses.
type me struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// getMe validates the token and returns the bot's identity.
func (b *Bot) getMe(ctx context.Context) (me, error) {
	var m me
	if err := b.call(ctx, "getMe", struct{}{}, &m, requestTimeout); err != nil {
		return me{}, err
	}
	if m.Username == "" {
		return me{}, errors.New("telegram getMe: empty username (check token)")
	}
	return m, nil
}

// setMyCommands registers the command list so clients autocomplete it.
func (b *Bot) setMyCommands(ctx context.Context) error {
	type botCommand struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	cmds := make([]botCommand, 0, len(commandDescriptions))
	for _, c := range commandDescriptions {
		cmds = append(cmds, botCommand{c.command, c.description})
	}
	return b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil, requestTimeout)
}

// getUpdates long-polls for new messages starting at the current offset.
func (b *Bot) getUpdates(ctx context.Context) ([]update, error) {
	return b.getUpdatesAt(ctx, b.offset, b.pollTimeout)
}

// getUpdatesAt calls getUpdates with an explicit offset and long-poll timeout.
func (b *Bot) getUpdatesAt(ctx context.Context, offset int64, pollTimeout time.Duration) ([]update, error) {
	params := map[string]any{
		"offset":          offset,
		"timeout":         int(pollTimeout / time.Second),
		"allowed_updates": []string{"message"},
	}
	var updates []update
	if err := b.call(ctx, "getUpdates", params, &updates, pollTimeout+pollSlack); err != nil {
		return nil, err
	}
	return updates, nil
}

// sendMessage posts text to the configured chat, in forum topic threadID if
// nonzero. An empty parseMode sends plain text.
func (b *Bot) sendMessage(ctx context.Context, text, parseMode string, threadID int64) error {
	params := map[string]any{
		"chat_id":              b.cfg.ChatID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	if threadID != 0 {
		params["message_thread_id"] = threadID
	}
	return b.call(ctx, "sendMessage", params, nil, requestTimeout)
}
