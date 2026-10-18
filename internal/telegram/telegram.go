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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shishberg/mezzaops/internal/reconnect"
	"github.com/shishberg/mezzaops/internal/service"
)

// telegramMaxRunes caps the rune count of a message posted to Telegram. The
// API limit is 4096 characters counted after entity parsing; this leaves
// headroom for the scaffolding around truncated content.
const telegramMaxRunes = 4000

const (
	defaultAPIURL         = "https://api.telegram.org"
	defaultPollTimeout    = 30 * time.Second
	defaultBackoffInitial = 1 * time.Second
	defaultBackoffMax     = 60 * time.Second

	// requestTimeout bounds every call except getUpdates, whose timeout is
	// the poll timeout plus slack so the HTTP request outlives the poll.
	requestTimeout = 30 * time.Second
	pollSlack      = 15 * time.Second
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

// ServiceManager is the slice of the manager API the Telegram frontend uses.
// Duplicated from internal/matrix so the packages stay independent; extract
// to internal/service if a fourth frontend wants the same shape.
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
	manager ServiceManager
	confirm ConfirmHandler
	http    *http.Client

	// started is when the Bot was constructed. Messages dated before it are
	// skipped: Telegram only acknowledges an update on the next getUpdates
	// with a higher offset, so a command that restarts mezzaops itself
	// (/deploy on a self_deploy service) would otherwise be redelivered and
	// run again on every boot.
	started time.Time

	// pollTimeout and the backoff bounds are fields rather than constants so
	// tests don't wait 30s polls or 1s retries.
	pollTimeout    time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	// offset is the next update_id to request. It lives on the Bot, not in
	// runOnce, so a reconnect does not replay updates. Only the poll loop
	// touches it.
	offset int64

	// username is the bot's own @username, set from getMe. mu protects it
	// because runOnce writes it while handlers read it.
	mu       sync.Mutex
	username string
	readyCh  chan struct{}
}

// New constructs a Bot. No I/O is performed: validating the token and
// starting the poll loop happen in Run, so a failure on boot (e.g. DNS not
// ready yet) is retried rather than permanently disabling the frontend.
func New(cfg Config, manager ServiceManager) *Bot {
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.APIURL == "" {
		cfg.APIURL = defaultAPIURL
	}
	return &Bot{
		cfg:            cfg,
		manager:        manager,
		http:           &http.Client{},
		started:        time.Now(),
		pollTimeout:    defaultPollTimeout,
		backoffInitial: defaultBackoffInitial,
		backoffMax:     defaultBackoffMax,
		readyCh:        make(chan struct{}),
	}
}

// SetConfirmHandler installs the deploy-confirmation handler.
func (b *Bot) SetConfirmHandler(ch ConfirmHandler) { b.confirm = ch }

// Ready closes once getMe has succeeded, so callers know the token is valid
// and the bot is about to poll.
func (b *Bot) Ready() <-chan struct{} { return b.readyCh }

// markReady closes readyCh exactly once. Safe to call from multiple retry
// attempts; the second close is recovered.
func (b *Bot) markReady() {
	defer func() { _ = recover() }()
	close(b.readyCh)
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
	username, err := b.getMe(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.username = username
	b.mu.Unlock()

	if err := b.setMyCommands(ctx); err != nil {
		log.Printf("telegram: setMyCommands: %v", err)
	}

	b.markReady()
	log.Printf("telegram: connected as @%s, watching chat %d", username, b.cfg.ChatID)

	b.pollLoop(ctx)
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

// handleUpdate filters to the configured chat, ignores bots and messages from
// before the Bot was constructed, parses the command, and posts the response.
func (b *Bot) handleUpdate(ctx context.Context, u update) {
	msg := u.Message
	if msg == nil || msg.Chat.ID != b.cfg.ChatID {
		return
	}
	if msg.From != nil && msg.From.IsBot {
		return
	}
	if msg.Date < b.started.Unix() {
		return
	}
	cmd := ParseCommand(msg.Text, b.botUsername())
	if cmd == nil {
		return
	}
	b.PostMessage(ctx, b.dispatchCommand(cmd))
}

// PostMessage renders the markdown to Telegram HTML and sends it to the
// configured chat. If Telegram rejects the markup (HTTP 400) the original text
// is resent once as plain text. Errors are logged, not returned, since
// notifier callers don't have a place to surface them.
//
// The chat ID comes from config, so PostMessage does not wait for getMe:
// notifications before Ready are sent rather than dropped.
func (b *Bot) PostMessage(ctx context.Context, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	message = service.TruncateTailToRuneBudget(message, telegramMaxRunes)

	err := b.sendMessage(ctx, markdownToHTML(message), "HTML")
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest {
		log.Printf("telegram: send message as HTML rejected, resending as plain text: %v", err)
		err = b.sendMessage(ctx, message, "")
	}
	if err != nil {
		log.Printf("telegram: send message: %v", err)
	}
}

// dispatchCommand routes a parsed command to the manager and returns a
// markdown response.
func (b *Bot) dispatchCommand(cmd *Command) string {
	switch cmd.Action {
	case "status":
		if cmd.Service == "" {
			return formatStatusOverview(b.manager.GetAllStates())
		}
		return b.manager.Do(cmd.Service, "status")

	case "start", "stop", "restart", "pull":
		return b.manager.Do(cmd.Service, cmd.Action)

	case "logs":
		const format = "%s:\n```\n%s\n```"
		budget := telegramMaxRunes - len([]rune(fmt.Sprintf(format, cmd.Service, "")))
		out := service.TruncateTailToRuneBudget(defuseFences(b.manager.Do(cmd.Service, "logs")), budget)
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
// early.
func defuseFences(s string) string {
	return strings.ReplaceAll(s, "```", "``")
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

// --- Bot API ---

// update, message, chat and user are the subset of the Bot API types used.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Date int64  `json:"date"`
	Text string `json:"text"`
	Chat chat   `json:"chat"`
	From *user  `json:"from"`
}

type chat struct {
	ID int64 `json:"id"`
}

type user struct {
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

// apiError is a Bot API response with ok=false (or a non-JSON error status).
type apiError struct {
	Method      string
	Code        int
	Description string
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
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return b.scrub(method, err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &apiError{Method: method, Code: resp.StatusCode, Description: http.StatusText(resp.StatusCode)}
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &apiError{Method: method, Code: code, Description: env.Description}
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

// getMe validates the token and returns the bot's username.
func (b *Bot) getMe(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := b.call(ctx, "getMe", struct{}{}, &me, requestTimeout); err != nil {
		return "", err
	}
	if me.Username == "" {
		return "", errors.New("telegram getMe: empty username (check token)")
	}
	return me.Username, nil
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
	params := map[string]any{
		"offset":          b.offset,
		"timeout":         int(b.pollTimeout / time.Second),
		"allowed_updates": []string{"message"},
	}
	var updates []update
	if err := b.call(ctx, "getUpdates", params, &updates, b.pollTimeout+pollSlack); err != nil {
		return nil, err
	}
	return updates, nil
}

// sendMessage posts text to the configured chat. An empty parseMode sends
// plain text.
func (b *Bot) sendMessage(ctx context.Context, text, parseMode string) error {
	params := map[string]any{
		"chat_id":              b.cfg.ChatID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	return b.call(ctx, "sendMessage", params, nil, requestTimeout)
}
