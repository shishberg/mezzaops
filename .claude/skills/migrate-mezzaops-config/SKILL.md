---
name: migrate-mezzaops-config
description: Migrate old mezzaops tasks.yaml to the new config.yaml + services/ format
argument-hint: <path-to-tasks.yaml>
allowed-tools: Read Write Bash Glob
---

# Migrate old mezzaops tasks.yaml

Convert the old mezzaops `tasks.yaml` format into the new unified config format.

## Old format (tasks.yaml)

```yaml
task:
- name: mybot
  dir: /opt/mybot
  entrypoint:
    - ./mybot
    - --flag
```

## New format

Creates two things:

### 1. `config.yaml` (if it doesn't exist)

```yaml
services_dir: "./services"
log_dir: "./logs"
state_dir: "./state"

process:
  adopt: true

# discord:
#   guild_id: ""
#   channel_id: ""
```

### 2. One file per task in `services/`

For each task in the old config, create `services/<name>.yaml`:

```yaml
dir: /opt/mybot
entrypoint: ["./mybot", "--flag"]
```

## Steps

1. Read the old tasks.yaml file at `$ARGUMENTS` (default: `tasks.yaml`)
2. Parse the `task:` list
3. For each task, create `services/<name>.yaml` with the `dir` and `entrypoint` fields
4. If `config.yaml` doesn't exist, create it with sensible defaults
5. Report what was created

## Notes

- The `entrypoint` field maps directly (array of strings)
- The `dir` field maps to `dir` in the new format
- Old mezzaops had no deploy steps, repo, branch, or service_name — leave those out
- Preserve the Discord config if the user has command-line flags (--guild-id, --channel-id) — ask them for the values to put in config.yaml
