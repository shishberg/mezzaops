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

A `chat_id` of 0 (key missing or misspelled) would start a bot that silently
ignores everything, so `app.New` logs a warning and leaves the frontend
disabled. It does not return an error: one bad section must not stop the
supervisor booting.

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
anyone).

Senders are filtered as follows:

- A message whose `sender_chat.id` equals the configured `chat_id` is
  accepted. This is how an anonymous group admin ("Remain anonymous")
  appears: `from` is `@GroupAnonymousBot` (`is_bot: true`) and `sender_chat`
  is the group itself.
- Otherwise a message is ignored if `from.is_bot` is true, or if `from.id` is
  the bot's own ID (stored from `getMe`).

There is no date filter: it would depend on the host clock, and a clock that
is fast or not yet synced would drop real commands. Instead, on the **first**
successful connect only, the bot drains pending updates server-side before it
reports ready: `getUpdates` with `offset=-1` and `timeout=0` returns the
newest pending update, and the bot sets its offset to that update's
`update_id + 1`. Later reconnects keep the in-memory offset.

The reason is that Telegram acknowledges an update only on the next
`getUpdates` with a higher offset, so an update that was received but not yet
acknowledged is redelivered after a restart. The specific failure:
`/deploy mezzaops` on a `self_deploy` service restarts mezzaops before the
next poll, so the deploy command is redelivered and runs again on every boot.

Replies to a command sent in a forum topic (`is_topic_message` true) carry the
same `message_thread_id`, so they land in that topic rather than General.
Notifications go to the chat's default topic.

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

Block elements are separated from following content by a newline: Telegram
entities are ranges over the text, so without it the next line would run on
from the block.

If `sendMessage` fails with HTTP 400 and the description is an entity-parse
error (`Bad Request: can't parse entities: ...`), resend the original text
once without `parse_mode`. Other 400s (for example `chat not found`) are
logged once and not resent.

Telegram limits a message to 4096 UTF-16 code units, counted after entity
parsing, so HTML tags and entity escapes do not count, and characters outside
the BMP (most emoji) count as 2. Define `telegramMaxUTF16Units = 4000` and a
tail-truncation helper that measures in UTF-16 units. Callers that build
fenced output (the notifier's `DeployFailed` and the `logs` response) truncate
the content inside the fence with it, so the fence markers survive. Backtick
runs of three or more in that content are reduced until no ``` remains (a
single pass would turn four backticks into a fence). As a fallback, the send
path truncates any text over the limit with the same helper before
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
- `Ready()` closes once `getMe` has succeeded and the first-connect drain
  (see Message filtering) is done; draining first means a command sent right
  after Ready is not mistaken for a leftover. The app adds it to
  `waitForReady`.
- `PostMessage(ctx, text)` does not call the API. It enqueues onto a bounded
  buffered channel (100) and returns, so a slow Bot API never blocks the
  manager's notifier or `HandlePush` (whose webhook has a 10s timeout). When
  the queue is full the message is logged and dropped. Command replies use the
  same queue, so ordering is preserved.
- A single sender goroutine, started by `Run` (so `New` still does no I/O),
  drains the queue in order and stops when the `Run` context is cancelled. It
  does not need `getMe` (the chat ID comes from config), so messages queued
  before Ready are sent once the sender starts rather than dropped.
- On HTTP 429 the sender reads `parameters.retry_after`, sleeps that long
  (at least 1s, capped at 60s, cancellable), and retries the same message, up
  to 3 retries; after that the message is logged and dropped. The sleep is an
  injectable field so tests stay fast.

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
