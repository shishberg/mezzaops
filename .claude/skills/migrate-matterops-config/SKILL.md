---
name: migrate-matterops-config
description: Migrate matterops config.yaml + services/ to the new unified mezzaops format
argument-hint: <path-to-matterops-dir>
allowed-tools: Read Write Bash Glob
---

# Migrate matterops config to mezzaops

Convert a matterops project's config into the new unified mezzaops config format.

## Old matterops format

### config.yaml
```yaml
mattermost:
  url: "http://localhost:8065"
  channel: "myteam/town-square"
webhook:
  port: 8080
dashboard:
  port: 8081
services_dir: "./services"
```

### .env
```
MATTERMOST_TOKEN=xoxb-...
GITHUB_WEBHOOK_SECRET=secret123
```

### services/*.yaml
```yaml
branch: main
repo: "github.com/org/myapp"
working_dir: "."
deploy:
  - "git pull"
  - "go build"
process:
  cmd: "echo 'running' && sleep 3600"
service_name: "com.example.myapp"
user_service: false
require_confirmation: false
```

## New mezzaops format

### config.yaml
```yaml
services_dir: "./services"
log_dir: "./logs"
state_dir: "./state"

process:
  adopt: true

mattermost:
  url: "http://localhost:8065"
  channel: "myteam/town-square"

webhook:
  port: 8080

dashboard:
  port: 8081
```

### .env
```
MATTERMOST_TOKEN=xoxb-...
GITHUB_WEBHOOK_SECRET=secret123
```

### services/*.yaml
```yaml
branch: main
repo: "github.com/org/myapp"
dir: "."
deploy:
  - "git pull"
  - "go build"
process:
  cmd: "echo 'running' && sleep 3600"
service_name: "com.example.myapp"
user_service: false
require_confirmation: false
```

## Steps

1. Read the matterops directory at `$ARGUMENTS` (default: current directory)
2. Read its `config.yaml` and `.env`
3. Read all files in its `services_dir`
4. Generate the new `config.yaml`:
   - Copy `mattermost`, `webhook`, `dashboard` sections as-is
   - Add `log_dir: "./logs"`, `state_dir: "./state"`, `process.adopt: true`
   - Do NOT include `discord:` section (matterops didn't have Discord)
5. Copy `.env` as-is (the variable names are the same: MATTERMOST_TOKEN, GITHUB_WEBHOOK_SECRET)
6. Copy service files, renaming `working_dir` to `dir`
7. Report what was created

## Key differences

| matterops field | mezzaops field | Notes |
|---|---|---|
| `working_dir` | `dir` | Renamed |
| `process.cmd` | `process.cmd` | Same |
| `service_name` | `service_name` | Same |
| (none) | `log_dir` | New, defaults to `./logs` |
| (none) | `state_dir` | New, defaults to `./state` |
| (none) | `process.adopt` | New, defaults to `true` |
| (none) | `entrypoint` | New option, alternative to process.cmd |
