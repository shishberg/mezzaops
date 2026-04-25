# MezzaOps

MezzaOps is a chat-ops process supervisor and deploy tool. It manages services via Discord slash commands, Mattermost `@mezzaops` mentions, GitHub webhooks, and a web dashboard — with a single config file controlling which frontends are active.

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

process:
  adopt: true        # re-adopt orphaned processes on restart

discord:
  guild_id: ""
  channel_id: ""

mattermost:
  url: "http://mm.example.com:8065"
  channel: "team/ops"

matrix:
  homeserver: "https://matrix.example.org"
  room: "!abc:matrix.example.org"      # room ID or "#alias:server"
  command_prefix: "!mezzaops"
  crypto_db: ""                         # default: "<state_dir>/matrix-crypto.db"

webhook:
  port: 8080

dashboard:
  port: 8081
```

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
```

The Matrix bot stores end-to-end-encryption keys in a SQLite database at
`<state_dir>/matrix-crypto.db` (override with `matrix.crypto_db`). The pickle
key encrypts that store; rotating it forces the bot to re-establish device
identity and re-share room keys.

## Frontends

| Frontend | Trigger | Capabilities |
|---|---|---|
| **Discord** | `/ops start <svc>` slash commands | Full ops + deploy + presence status |
| **Mattermost** | `@mezzaops start <svc>` mentions | Full ops + deploy + confirm |
| **Matrix** | `!mezzaops start <svc>` (configurable prefix) in one configured room | Full ops + deploy + confirm; supports E2EE rooms |
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

Mattermost and Matrix additionally support `confirm` (for services with `require_confirmation: true`).

## Process adoption

When `process.adopt: true` (the default), MezzaOps re-adopts child processes that survive a restart. It verifies process identity via boot time and process creation time to detect PID reuse. Set `process.adopt: false` in dev to get a clean slate each time.

## Development

```sh
make check   # lint + test + build
make dev     # run with config.dev.yaml
make cli     # interactive CLI with config.dev.yaml
```
