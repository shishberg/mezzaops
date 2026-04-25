# Matrix bot frontend

**Status:** Design
**Date:** 2026-04-25

## Summary

Add a Matrix chat frontend to mezzaops, parallel to the existing Mattermost
and Discord frontends. The bot listens for `!mezzaops <action> [service]`
commands in one configured room, dispatches them to the service manager, and
posts deploy/service notifications to the same room. Built on
[mautrix/go](https://github.com/mautrix/go) with end-to-end encryption
support via its `crypto/cryptohelper` package.

## Goals

- One more chat frontend with the same operational surface as Mattermost.
- Works in encrypted rooms (E2EE on by default).
- Token-based authentication, no interactive login.
- Same wiring shape as the Mattermost bot — no new abstractions in
  `internal/app` or `internal/service` unless extraction is obviously
  cheaper than duplication.

## Non-goals

- Multi-room support (one configured room, mirrors the Mattermost model).
- Password login / interactive verification flows.
- Cross-signing UX, key backup, SAS verification UI.
- Bridging to other networks.

## Design decisions

The following choices were settled during brainstorming and are recorded
here so the implementation plan can flow from them.

| Decision | Choice | Reason |
|---|---|---|
| Trigger style | Command prefix `!mezzaops` (configurable) | Standard idiom for Matrix bots (maubot etc.). |
| Authentication | Pre-issued access token (user_id + device_id + token) | Matches Mattermost/Discord pattern, no startup login round-trip. |
| Encryption | E2EE on, via `cryptohelper` | Many Matrix rooms default to encrypted; the extra cost (sqlite + pickle key) is paid once. |
| Room scoping | One configured room | Same model as the Mattermost frontend. |
| Missing secrets | Silently skip the frontend | Matches existing Discord/Mattermost behaviour. |

## Package layout

```
internal/matrix/
  matrix.go         Bot struct, command parsing, dispatch, sync loop
  matrix_test.go    Unit tests for ParseCommand, dispatch, invite handling
  notifier.go       service.Notifier implementation (markdown -> matrix HTML)
  notifier_test.go  Unit tests for Notifier methods
```

The package is wired into `internal/app/app.go` next to the Mattermost bot:
`a.matrixBot = matrix.New(...)`, appended to the notifier list, run as a
goroutine in `App.Run`, and waited on via a `Ready()` channel before the
manager is signalled ready.

## Configuration

### `config.yaml` — optional `matrix` section

```yaml
matrix:
  homeserver: "https://matrix.example.org"   # required
  room: "!abc:matrix.example.org"            # room ID or "#alias:server"
  command_prefix: "!mezzaops"                # optional; default "!mezzaops"
  crypto_db: ""                              # optional; default "<state_dir>/matrix-crypto.db"
```

Added to `internal/config/config.go`:

```go
type MatrixConfig struct {
    Homeserver    string `yaml:"homeserver"`
    Room          string `yaml:"room"`
    CommandPrefix string `yaml:"command_prefix"`
    CryptoDB      string `yaml:"crypto_db"`
}

type Config struct {
    // ... existing fields
    Matrix *MatrixConfig `yaml:"matrix"`
}
```

### `.env` — four new secrets

```
MATRIX_USER_ID=@mezzaops:matrix.example.org
MATRIX_DEVICE_ID=MEZZAOPS01
MATRIX_ACCESS_TOKEN=syt_...
MATRIX_PICKLE_KEY=<random passphrase, generated once and kept stable>
```

`Env` struct gains four matching fields. `LoadEnv` reads them via godotenv
and falls back to `os.Getenv`, mirroring the existing pattern.

If `cfg.Matrix == nil` or any of the four secrets is empty, the matrix bot
is not constructed.

The pickle key encrypts the crypto state at rest in the SQLite store. It
must remain stable across restarts; rotating it forces the bot to
re-establish device identity and re-share keys.

## Connection lifecycle

`matrix.New(cfg, manager)`:

1. `client, _ := mautrix.NewClient(cfg.Homeserver, "", "")`
2. Set `client.UserID`, `client.DeviceID`, `client.AccessToken` from env.
3. Assign `client.Log` — a `zerolog.Logger` writing to `os.Stderr` at
   info level, so mautrix's internal logs land alongside the standard
   library `log` output the rest of mezzaops uses.

`Bot.Run(ctx)`:

1. **Resolve room.** If `cfg.Room` starts with `#` use
   `client.ResolveAlias` and store the resulting `id.RoomID`. If it starts
   with `!`, parse and store directly. Otherwise return a config error.
2. **Init crypto.**
   `cryptoHelper, _ := cryptohelper.NewCryptoHelper(client, []byte(pickleKey), cryptoDB)`,
   leave `LoginAs` nil (we already set the access token), call
   `cryptoHelper.Init(ctx)`, then assign `client.Crypto = cryptoHelper`.
3. **Register sync handlers** on `client.Syncer.(*mautrix.DefaultSyncer)`:
   - `OnEventType(event.EventMessage, handleMessage)` — filter to the
     configured room, ignore self, parse and dispatch.
   - `OnEventType(event.StateMember, handleInvite)` — auto-join only if
     the invite targets our user **and** the room ID matches the
     configured room. Invites to other rooms are ignored.
4. **Close `readyCh`** so `app.Run` can signal manager-ready.
5. **Run sync.** `client.SyncWithContext(ctx)` blocks until ctx is
   cancelled. Mautrix handles reconnection internally; no exponential
   backoff loop is needed.
6. **On exit**, call `cryptoHelper.Close()` to flush the SQLite store.

`Bot.Ready() <-chan struct{}` — closes once the room is resolved and
crypto is initialised.

`Bot.PostMessage(ctx, markdown string)`:

- Renders markdown to HTML via `format.RenderMarkdown(text, true, false)`.
- Sends `event.MessageEventContent{ MsgType: event.MsgText, Body: text,
  Format: event.FormatHTML, FormattedBody: html }` via
  `client.SendMessageEvent`. With `client.Crypto` set, mautrix
  transparently encrypts when the room is encrypted.

## Commands

`ParseCommand(message, prefix string) *Command` returns a `Command{Action,
Service}` if the first token equals the configured prefix, otherwise nil.

Dispatch mirrors `mattermost.Bot.dispatchCommand` — same set of actions:
`status`, `start`, `stop`, `restart`, `logs`, `pull`, `deploy`, `confirm`,
`reload`, `start-all`, `stop-all`. `status` with no service shows the
overview; `confirm` is delegated to a `ConfirmHandler` set by the app.

The `ServiceManager` and `ConfirmHandler` interfaces are identical to the
ones in `internal/mattermost`. During implementation, decide whether to
duplicate them (cheap, keeps packages independent) or extract to
`internal/service` (one canonical interface, but pulls a UI concern into
the service package). Default to duplication unless extraction is
obviously cleaner — both are local refactors and easy to revisit.

## Notifier

`internal/matrix/notifier.go` mirrors `internal/mattermost/notifier.go`,
implementing `service.Notifier`:

- `ServiceEvent(name, event)` — `` `<name>` <event>. ``
- `DeployStarted(name)` — `` Deploying `<name>`... ``
- `DeploySucceeded(name, output)` — `` Deploy of `<name>` succeeded. ``
- `DeployFailed(name, step, output)` — same template as Mattermost,
  truncated via `service.TruncateTailToRuneBudget` so the whole post fits
  the matrix message limit. Use a conservative budget constant
  (`matrixMaxRunes = 32 * 1024`) — well below the typical 64 KiB
  homeserver limit.
- `WebhookReceived(name, info)` — uses `info.FormatMessage` like the
  others.

All messages are markdown; `PostMessage` renders to HTML for the
`formatted_body`.

## Confirm flow

When a webhook arrives for a service with `require_confirmation: true`,
`app.HandlePush` already posts a "Deploy queued, reply `@mezzaops confirm
<svc>`" message to Mattermost. Add a parallel branch for matrix that
posts the equivalent message with the configured matrix prefix
("`!mezzaops confirm <svc>`"). The bot dispatches `confirm <svc>` to
`a.Confirm(svc)` exactly like Mattermost does.

## App wiring

`internal/app/app.go` changes:

- Add `matrixBot *matrix.Bot` field to `App`.
- After the Mattermost block in `New`:
  ```go
  if cfg.Matrix != nil &&
      env.MatrixUserID != "" && env.MatrixDeviceID != "" &&
      env.MatrixAccessToken != "" && env.MatrixPickleKey != "" {
      mxcfg := matrix.Config{ /* ... */ }
      a.matrixBot = matrix.New(mxcfg, a.manager)
      a.matrixBot.SetConfirmHandler(a)
      notifiers = append(notifiers, matrix.NewNotifier(a.matrixBot))
  }
  ```
- In `Run`, start `a.matrixBot.Run(ctx)` in a goroutine like the
  Mattermost bot, and add `<-a.matrixBot.Ready()` to the readiness wait.
- In `HandlePush`, post the confirmation prompt to the matrix bot too
  when one is configured.

## Testing strategy

Unit tests only. The mautrix sync loop and crypto store are not exercised
in tests — same call we made for the Mattermost websocket and REST
clients.

- `TestParseCommand` — pure-function table-driven tests covering: matching
  prefix, non-matching prefix, missing service, leading whitespace,
  case sensitivity of the action.
- `TestDispatch_*` — mock `ServiceManager` (and `ConfirmHandler`),
  feed parsed commands, assert returned message and which manager
  methods were called.
- `TestHandleInvite_*` — mock the message-sender / room-joiner interface,
  verify the bot joins only when invited to the configured room and only
  when the invite targets our user.
- `TestNotifier_*` — mock the message-sender, assert the markdown that
  reaches `PostMessage` for each notifier method.
- `TestDeployFailed_Truncates` — long output, assert post fits the
  configured rune budget and the tail is preserved (mirrors the
  truncation test in `internal/mattermost/mattermost_test.go`).

To make these tractable, define a small interface inside
`internal/matrix/matrix.go` covering only the methods we call on
`*mautrix.Client` (`SendMessageEvent`, `JoinRoomByID`, `ResolveAlias`,
maybe one or two more). The Bot holds the interface; the real client
satisfies it; tests pass a fake.

## Dependencies

`go get maunium.net/go/mautrix` brings in:

- `maunium.net/go/mautrix` (client, sync)
- `maunium.net/go/mautrix/crypto/cryptohelper` (E2EE)
- `maunium.net/go/mautrix/event`, `id`, `format`
- `github.com/mattn/go-sqlite3` (cgo SQLite driver, required by
  cryptohelper)
- `github.com/rs/zerolog` (transitive — already used by mautrix)
- `go.mau.fi/util` (transitive)

`go-sqlite3` requires cgo. Confirm `CGO_ENABLED=1` works in the build
environment; if not, the alternative is `modernc.org/sqlite` via a build
tag, but that's a follow-up if the default breaks the build.

## Documentation

- `README.md` — add a "Matrix" row to the Frontends table, a `matrix:`
  block to the config example, and the four `MATRIX_*` lines to the
  `.env` example. Brief note about the SQLite crypto store path.
- `config.yaml` — add a commented-out `matrix:` block.

## Risks / open questions

- **cgo for SQLite.** If the project is built with `CGO_ENABLED=0`,
  `go-sqlite3` won't link. Switch to `modernc.org/sqlite` if needed.
- **Pickle key rotation.** Documented as "keep stable"; if a user
  rotates it, the device loses its keys and rooms have to re-share.
  Acceptable; not silently recovered.
- **Federation / homeserver outages.** `client.SyncWithContext`
  reconnects internally, but if it returns an error we log and the
  bot stays down until the process restarts. Matches Mattermost's
  effective behaviour after retry exhaustion.
