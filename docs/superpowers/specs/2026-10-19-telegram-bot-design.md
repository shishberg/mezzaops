# Telegram bot frontend

## Goal

Add a Telegram frontend parallel to Discord, Mattermost and Matrix: it
accepts commands in one configured chat, dispatches them to the service
manager, and posts service/deploy notifications to the same chat.

## Transport

Call the Telegram Bot API directly with `net/http` (no new dependency).
Three methods are needed, plus one optional:

- `getMe` — validates the token and returns the bot's username.
- `getUpdates` — long polling (`timeout=30`, `allowed_updates=["message"]`,
  `offset` tracking). The HTTP client timeout must exceed the poll timeout.
- `sendMessage` — `chat_id`, `text`, `parse_mode=HTML`,
  `link_preview_options={"is_disabled":true}`.
- `setMyCommands` — best-effort on connect so clients autocomplete the
  commands. Failure is logged, not fatal.

The API base URL defaults to `https://api.telegram.org` and is configurable
(`telegram.api_url`) for a self-hosted Bot API server and for tests
(`httptest.Server`). Requests go to `<api_url>/bot<token>/<method>`. Error
messages and logs must never contain the token (wrap `url.Error`s so the URL
is not printed).

## Config

`config.yaml`:

```yaml
telegram:
  chat_id: -1001234567890   # int64; group/supergroup IDs are negative
  api_url: ""               # optional; default https://api.telegram.org
```

`.env`: `TELEGRAM_TOKEN=`. The frontend is enabled when the `telegram`
section is present and the token is non-empty (matching the other
frontends). `telegram` joins `knownTopLevelConfigKeys`.

## Commands

Telegram commands are `/word`, where the word may contain only `a-z`, `0-9`
and `_`. Commands:

`/status [svc]`, `/start svc`, `/stop svc`, `/restart svc`, `/logs svc`,
`/pull svc`, `/deploy svc`, `/confirm svc`, `/reload`, `/start_all`,
`/stop_all`.

`ParseCommand(text, botUsername string) *Command`:

- The first token must start with `/`.
- In groups Telegram appends `@botusername` (`/status@mezzaops_bot`). Strip
  it; if it names a different bot (case-insensitive compare), return nil.
- Action is lower-cased, and `_` is mapped to `-` so `start_all` dispatches
  as `start-all`. `start-all` typed by hand also works.
- Second token, if any, is Service.

Dispatch matches the Matrix frontend (same responses), with these
differences: the unknown-command message lists the Telegram spellings, and
`logs` output is wrapped in a code block like Discord does.

## Message filtering

Only messages whose `chat.id` equals the configured `chat_id` are handled;
everything else is ignored silently (a Telegram bot can be messaged by
anyone). Messages from bots are ignored.

Messages whose `date` is earlier than the time the Bot was constructed are
skipped. Without this, an update that was received but not yet acknowledged
(Telegram acknowledges on the next `getUpdates` with a higher offset) is
redelivered after a restart. The specific failure: `/deploy mezzaops` on a
`self_deploy` service restarts mezzaops before the next poll, so the deploy
command is redelivered and runs again on every boot.

## Formatting

Frontend messages are written in the Markdown subset the other frontends
use: `**bold**`, `` `code` ``, fenced ```` ``` ```` blocks, `[text](url)`
links, and `> ` quote lines (see `service.WebhookInfo.FormatMessage`).
Telegram's MarkdownV2 requires escaping almost every punctuation character,
so convert this subset to Telegram HTML instead (`markdownToHTML`):

- Escape `&`, `<`, `>` everywhere.
- Fenced blocks → `<pre>…</pre>` (no inline processing inside).
- Inline code → `<code>…</code>` (no inline processing inside).
- `**x**` → `<b>x</b>`; `[t](u)` → `<a href="u">t</a>` (escape `"` in u).
- Consecutive lines starting with `> ` → one `<blockquote>…</blockquote>`.
- Anything unmatched (a lone `**`, an unclosed backtick) is left as literal
  text.

If `sendMessage` fails with HTTP 400 (bad entities), resend the original
text once without `parse_mode`.

Telegram limits a message to 4096 characters, counted after entity parsing,
so HTML tags and entity escapes do not count. Define
`telegramMaxRunes = 4000`. Callers that build fenced output (the notifier's
`DeployFailed` and the `logs` response) truncate the content inside the fence,
computing the budget the same way the Matrix notifier does, so the fence
markers survive. As a fallback, `PostMessage` truncates any text over
`telegramMaxRunes` with `service.TruncateTailToRuneBudget` before
conversion. If that cuts an opening fence, the converter leaves the
unmatched closing fence as literal text, which is acceptable.

## Lifecycle

Follow the boot-resilience pattern (`internal/reconnect`):

- `New` does no I/O.
- `Run` wraps the connect path in `reconnect.RunWithBackoff(ctx,
  "telegram", …)`: `getMe` → `setMyCommands` (best-effort) → `markReady()`
  → poll loop.
- The poll loop handles transient `getUpdates` errors itself, with its own
  backoff (1s doubling to 60s, reset after a successful poll), like
  Mattermost's `listenWebSocket`. It returns only when ctx is cancelled.
  The offset is kept on the Bot so a reconnect does not replay updates.
  HTTP 409 (a webhook is set, or another poller is running) is logged with
  that explanation.
- `Ready()` closes once `getMe` succeeds. The app adds it to
  `waitForReady`.
- `PostMessage(ctx, text)` posts to the configured chat. It does not need
  `getMe` to have run (the chat ID comes from config), so notifications
  before Ready are sent rather than dropped.

## App wiring

- `App.telegramBot`, created when `cfg.Telegram != nil && env.TelegramToken
  != ""`, with `SetConfirmHandler(a)`, and `telegram.NewNotifier(bot)`
  appended to notifiers.
- `Run` starts it; `waitForReady` includes its Ready channel.
- `HandlePush` posts the confirmation prompt to Telegram:
  ``Deploy queued for **svc** (repo: …, branch: …). Reply `/confirm svc` to
  proceed.``

## Docs

Update `README.md` (config example, `.env` list, frontends table, commands
section, known top-level keys), `.env.example` and the commented block in
`config.yaml`. Mention: create the bot with @BotFather, add it to the
group, and how to find the chat ID (e.g. send a message and read
`getUpdates`). Privacy mode can stay on because the bot only needs `/`
commands.
