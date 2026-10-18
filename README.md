# MezzaOps

MezzaOps is a chat-ops process supervisor and deploy tool. It manages services via Discord slash commands, Mattermost `@mezzaops` mentions, Matrix and Telegram chat commands, GitHub webhooks, and a web dashboard — with a single config file controlling which frontends are active.

## Quick start

```sh
go install .
cp config.yaml config.local.yaml   # edit to enable the frontends you want
cp .env.example .env                # add your tokens/secrets
mezzaops --config config.local.yaml
```

Or interactively for local testing:

```sh
make cli   # runs with config.dev.yaml -i
```

## Configuration

**`config.yaml`** — top-level config. Frontend sections are optional; omit a section to disable it.

```yaml
services_dir: "./services"
log_dir: "./logs"
state_dir: "./state"

discord:
  guild_id: ""
  channel_id: ""

mattermost:
  url: "http://mm.example.com:8065"
  channel: "team/ops"

matrix:
  homeserver: "https://matrix.example.org"   # full URL, or a bare server name (e.g. "example.org") resolved via .well-known/matrix/client
  room: "!abc:matrix.example.org"      # room ID or "#alias:server"
  command_prefix: "!mezzaops"
  crypto_db: ""                         # default: "<state_dir>/matrix-crypto.db"

telegram:
  chat_id: -1001234567890               # int64; group/supergroup IDs are negative
  api_url: ""                           # optional; default "https://api.telegram.org" (set for a self-hosted Bot API server)

webhook:
  port: 8080

dashboard:
  port: 8081
```

Unknown top-level keys in `config.yaml` are logged as warnings and otherwise
ignored — this catches misplaced keys (e.g. a stray `process: { adopt: true }`
block that was once hoped to be a global default). `process` and `adopt` are
per-service fields, not top-level ones. The recognised top-level keys are
`services_dir`, `log_dir`, `state_dir`, `discord`, `mattermost`, `matrix`,
`telegram`, `webhook`, `dashboard`, `readiness_timeout`, and `start_on_boot`.

`readiness_timeout` (default 30s, parsed as a Go duration like `45s`) bounds
how long the supervisor waits for the chat frontends to connect on boot
before signalling readiness. A chat server that's unreachable on boot
(e.g. Tailscale MagicDNS not ready yet) cannot block `adopt: false` /
`autostart` services indefinitely — the frontends reconnect in the
background via exponential backoff, and the gate opens after the timeout.

`start_on_boot` (default false) is the global autostart default for
process-backed services. When a process service's recorded pid is dead
on cold boot and the persisted state is not `"stopped"`, the manager
issues a Start once the frontends are ready. Per-service `autostart:
true|false` in `services/*.yaml` overrides the global. A service with
`autostart_force: true` is started even if the previous run ended
with `status: "stopped"` — use with care, it ignores the operator's
last command.

A service with `adopt: false` is autostarted only after a host reboot
(the boot ID recorded in its state differs from the current one), or
when it has no state file yet (it has never run here). The boot ID is the
kernel's per-boot UUID (`/proc/sys/kernel/random/boot_id` on Linux,
`kern.bootsessionuuid` on macOS). In a container it belongs to the host
kernel, so with persistent state `adopt: false` services autostart only after
a host reboot, not after a container restart. If the boot ID cannot be read,
`adopt: false` services that have run before are never autostarted (this is
logged); a service with no state file has never run here and still autostarts. If
mezzaops itself restarts without a reboot, the previous child may still
be running, so nothing is started and a log line says so.

**`services/*.yaml`** — one file per managed service:

```yaml
dir: /opt/mybot
entrypoint: ["./mybot", "--flag"]   # explicit argv
# OR
process:
  cmd: "./mybot --flag"             # sh -c wrapper
deploy:
  - "git pull"
  - "go build ."
branch: main
repo: "github.com/org/mybot"
service_name: "com.example.mybot"   # for launchctl/systemctl
require_confirmation: false
adopt: true                         # default true; re-attach to a running child
autostart: true                     # default: Config.start_on_boot; cold-boot Start
autostart_force: false              # default false; if true, ignore persisted "stopped"
```

**`.env`** — secrets (or set as real env vars):

```
DISCORD_TOKEN=
MATTERMOST_TOKEN=
GITHUB_WEBHOOK_SECRET=
MATRIX_USER_ID=
MATRIX_DEVICE_ID=
MATRIX_ACCESS_TOKEN=
MATRIX_PICKLE_KEY=
TELEGRAM_TOKEN=
```

The Matrix bot stores end-to-end-encryption keys in a SQLite database at
`<state_dir>/matrix-crypto.db` (override with `matrix.crypto_db`). The pickle
key encrypts that store; rotating it forces the bot to re-establish device
identity and re-share room keys.

To set up the Telegram bot:

1. Create the bot with [@BotFather](https://t.me/BotFather) (`/newbot`) and put the token in `TELEGRAM_TOKEN`.
2. Add the bot to the group (or message it directly) that should control mezzaops.
3. Find the chat ID: send any message in that chat, then open
   `https://api.telegram.org/bot<token>/getUpdates` and read `message.chat.id`
   (group and supergroup IDs are negative). Put it in `telegram.chat_id`.

Only messages from that one chat are handled; everything else is ignored. The
bot only needs `/` commands, so Telegram's privacy mode can stay on. The
frontend is enabled when the `telegram` section is present and `TELEGRAM_TOKEN`
is non-empty.

## Frontends

| Frontend | Trigger | Capabilities |
|---|---|---|
| **Discord** | `/ops start <svc>` slash commands | Full ops + deploy + presence status |
| **Mattermost** | `@mezzaops start <svc>` mentions | Full ops + deploy + confirm |
| **Matrix** | `!mezzaops start <svc>` (configurable prefix) in one configured room | Full ops + deploy + confirm; supports E2EE rooms |
| **Telegram** | `/start_all`, `/restart <svc>` etc. (slash commands; `/cmd@botname` also works) in one configured chat | Full ops + deploy + confirm |
| **Webhook** | `POST /webhook/github` push events | Auto-deploy on push |
| **Dashboard** | `GET /` and `GET /api/status` | Read-only status table |
| **CLI** | `mezzaops -i` | Interactive REPL for local testing |

## Service backends

Backend is selected per-service based on config fields:

| Config | Backend | Logs via |
|---|---|---|
| `entrypoint` or `process.cmd` | Child process (with adoption) | Log files on disk |
| `service_name` (macOS) | `launchctl` | `log show` |
| `service_name` (Linux) | `systemctl` | `journalctl` |

## Commands

All frontends support: `start`, `stop`, `restart`, `status`, `logs`, `pull`, `deploy`, `reload`, `start-all`, `stop-all`.

Mattermost, Matrix and Telegram additionally support `confirm` (for services with `require_confirmation: true`).

In Telegram, commands are slash commands and `start-all` / `stop-all` are spelled `/start_all` / `/stop_all` (Telegram commands cannot contain `-`). The bot registers its command list with `setMyCommands` on connect so clients autocomplete them.

## Process adoption

Per-service `adopt: true` (the default) makes MezzaOps re-adopt child processes that survive a restart. Set `adopt: false` per-service in dev to get a clean slate each time. MezzaOps verifies process identity via boot time and process creation time to detect PID reuse.

Note: `adopt` is a per-service field on each entry in `services/*.yaml`, not a top-level key in `config.yaml`. Older configs sometimes carry a stray top-level `process: { adopt: true }` block; mezzaops will log a warning naming that key and otherwise ignore it.

## Building

The Matrix bot's E2EE crypto helper has two olm backends:

- **`-tags goolm`** (what the Makefile uses) — pure Go, no system dependency.
- **default** — cgo wrapper around `libolm`, which needs the `libolm` C
  headers at build time: `apt install libolm-dev` on Debian/Ubuntu,
  `dnf install libolm-devel` on RHEL/Fedora, `brew install libolm` on macOS.

If you build with a plain `go build` (e.g. in a deploy step) on a host
without libolm, either install libolm or add `-tags goolm` to the build
command.

## Development

```sh
make check   # lint + test + build
make dev     # run with config.dev.yaml
make cli     # interactive CLI with config.dev.yaml
```
