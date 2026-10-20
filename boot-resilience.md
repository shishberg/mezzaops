# Spec: Boot resilience & startup correctness

## Status

Proposed. Author: discovered while debugging a failed-to-recover reboot on `oshirasama` (2026-06-19).

## Audience

Implementing agent. Follow the repo `CLAUDE.md` workflow: **write failing tests first**, implement to green,
`gofmt -w .`, run `make check` before committing, commit per task, push once at the end.

---

## Background / how we got here

Two `mezzaops` instances run on the host under systemd:

- `mezzaops.service` — User=linuxuser, `/home/linuxuser/mezzaops`, **Matrix** frontend. Manages
  foodbot/mopoke/slurp/etc. plus the two self-referential services (`mezzaops`, `mezzacotta-mezzaops`).
- `mezzacotta-mezzaops.service` — User=mezza, `/home/mezza/mezzaops`, **Discord** frontend. Manages
  process-backed services `bom`, `mezzagen`, `mezzanova`, `mezzaword`.

After a machine reboot, **none of the process-backed services came back up**, and the operator believed they
were configured to autostart. Investigation showed they were not — and surfaced several related correctness
and resilience gaps. This spec covers the code changes; the systemd boot-ordering fix is tracked separately
(see Appendix A).

---

## Problems (with code references)

### P1 — No autostart for process-backed services (the core gap)

`Manager.newManagedService` (`internal/service/manager.go`) only ever **adopts** a `ProcessBackend` on startup:

```go
adopt := svc.ShouldAdopt()
if pb, ok := backend.(*ProcessBackend); ok {
    pb.adopt = adopt
    msg := pb.TryAdopt()   // re-attaches to a LIVE pid; never Start()s
    ...
} else if !adopt {
    ms.restartOnStartup = true   // only non-process backends get a startup (re)start
}
```

`ProcessBackend.TryAdopt` (`internal/service/process.go`) returns `"stale pid … (process dead)"` /
`"no state to adopt"` and **never calls `Start()`**. So after a reboot (all child PIDs dead), process services
stay down forever unless a human issues `start-all` via a frontend. There is no config that changes this —
adoption is the *only* startup action for process backends.

**Deleting the `state/*.json` files does not help** — it just changes the log line to `"no state to adopt"`;
the service still isn't launched (confirmed by reading `TryAdopt`).

### P2 — Frontends connect once and never retry

In `internal/app/app.go` `Run`, each frontend is launched as a fire-and-forget goroutine:

```go
go func() {
    if err := a.discordBot.Run(ctx); err != nil {
        log.Printf("discord bot error: %v", err)   // goroutine then exits — no retry
    }
}()
```

Same pattern for Matrix and Mattermost. On this reboot both Discord and Matrix failed their initial connect
with `network is unreachable` resolving `discord.com` / `mcleish.id.au` on `100.100.100.100:53` (Tailscale
MagicDNS not ready yet). Because there is no retry, the frontend stayed **offline until a manual restart** —
i.e. no control channel to even issue `start-all`.

### P3 — `SignalReady` is gated behind a frontend that may never become ready

`internal/app/app.go`:

```go
go func() {
    if a.mmBot != nil { select { case <-a.mmBot.Ready(): case <-ctx.Done(): return } }
    if a.matrixBot != nil { select { case <-a.matrixBot.Ready(): case <-ctx.Done(): return } }
    a.manager.SignalReady()
}()
```

If a frontend's `Run` errors before signalling `Ready()`, the channel never fires, `SignalReady()` is never
called, and **every `restartOnStartup` (adopt:false) service blocks forever** in `serviceLoop` waiting on
`m.readyCh`. On this reboot that stalled the main instance's `adopt:false` services (e.g.
`mezzacotta-mezzaops`) until the unit was manually restarted.

### P4 — Nil-pointer panic during rapid restart

During overlapping restarts, an instance panicked:

```
panic: runtime error: invalid memory address or nil pointer dereference
  internal/discord.(*Bot).Run  internal/discord/discord.go:143
```

Line 143 is `b.session.ApplicationCommandBulkOverwrite(b.session.State.User.ID, …)`. If the session opened but
`State.User` (or `session`) is not yet populated / the open partially failed, this dereferences nil and crashes
the whole process. systemd `Restart=on-failure` recovered it, but it should not panic.

### P5 — Two instances can race on the same state dir

Existing `TODO` in `internal/service/manager.go` (`NewManager`):

```go
// TODO: take an exclusive flock or pidfile on cfg.StateDir here to prevent
// a second mezzaops instance from adopting/killing the same processes.
// We hit this in production: a stale binary kept running alongside the live
// one and killed adopted services out from under it.
```

This contributed to today's confusion (a direct `systemctl restart` overlapped with the main instance's
`adopt:false` restart of the same unit, producing the P4 panic).

### P6 — Silently-ignored top-level config key

`config.yaml` in the field contains a top-level `process: { adopt: true }` block. `config.Config`
(`internal/config/config.go`) has **no `Process` field**, so this is silently discarded. `adopt` is actually a
*per-service* field (`ServiceConfig.Adopt`). Operators reasonably assume the top-level key does something.

---

## Proposed changes

### C1 — Add autostart on startup (addresses P1) — **primary deliverable**

Introduce an explicit "start on boot" capability for process-backed services.

- **Per-service field** `autostart` (`*bool`) on `ServiceConfig`, plus an optional **global default**
  `start_on_boot` (`bool`) on `Config`. Resolution: per-service `autostart` overrides the global; if neither is
  set, default **false** (preserve today's behaviour — do not surprise existing deployments).
- In `newManagedService` / `serviceLoop`: when adoption does **not** result in a running process (stale pid /
  no state / adoption disabled) **and** the resolved autostart is true **and** the persisted desired-state is
  not `stopped`, issue a `Start` once frontends are ready (reuse the `restartOnStartup`/`readyCh` machinery so
  notifications are deliverable).
- Respect a persisted `"stopped"` desired-state by default (do **not** autostart something the operator
  explicitly stopped) unless an additional `autostart_force` is set. Document this clearly.
- Make `StartAll` idempotent w.r.t. already-running services (it already no-ops via `ProcessBackend.Start`'s
  `isRunning()` guard — verify and keep).

Acceptance:

- With `autostart: true`, a process service whose recorded pid is dead is **launched** on startup (after
  `SignalReady`).
- With `autostart` unset/false, behaviour is unchanged (adopt-only).
- A service with persisted `status:"stopped"` is **not** autostarted (unless force).

### C2 — Reconnect with backoff for all frontends (addresses P2)

Wrap each frontend's `Run` in a retry loop with capped exponential backoff (e.g. 1s→…→60s, jittered), cancellable
via `ctx`. A failed initial connect (DNS not ready, transient network) must not permanently disable the frontend.
Log at most one line per failed attempt; collapse repeats. Applies to Discord, Matrix, Mattermost.

Acceptance: simulate `Run` returning an error N times then succeeding; the frontend ends up connected and
`Ready()` fires. No busy-loop (assert backoff between attempts).

### C3 — Make `SignalReady` robust to frontend failure (addresses P3)

`SignalReady()` must fire even if a frontend never connects. Options (pick one, document choice):

- Add a bounded timeout to the readiness wait (e.g. signal ready after `min(all frontends ready, T seconds)`),
  **or**
- Have each frontend's `Ready()` resolve on terminal failure too (ready-or-failed), so the gate never hangs.

With C2 in place the frontend will reconnect later regardless; the point is that `adopt:false`/autostart services
must not be blocked indefinitely by an unreachable chat server.

Acceptance: a frontend whose connect never succeeds must not prevent `restartOnStartup`/autostart services from
starting; assert they start within the timeout.

### C4 — Guard the Discord ready/registration path against nil session (addresses P4)

In `internal/discord/discord.go` around `Run`/line 143: do not call `ApplicationCommandBulkOverwrite` (or
dereference `b.session.State.User`) unless the session is open and `State.User` is populated. Handle a
failed/partial `Open()` by returning an error (to be retried by C2) rather than panicking. Add a nil guard and a
regression test that drives `Run` with a session that fails to populate `State.User`.

Acceptance: the failure path returns an error and does not panic; covered by a test.

### C5 — Single-instance lock on the state dir (addresses P5)

Implement the existing TODO: acquire an exclusive advisory lock (flock) or a pidfile on `cfg.StateDir` in
`NewManager`. If another live instance holds it, exit with a clear error instead of co-managing the same
processes. Ensure the lock is released on shutdown and that a stale lock from a crashed instance is recoverable
(flock on an open fd handles this naturally).

Acceptance: a second instance pointed at the same `state_dir` fails fast with a descriptive error; a single
instance starts normally; after the holder exits, a new instance can acquire the lock.

### C6 — Surface unknown/misplaced config keys (addresses P6)

Either (a) add a real `Process` struct to `Config` with an `Adopt` default that feeds per-service resolution, or
(b) enable strict YAML decoding (`KnownFields(true)`) and **warn** (not fatal) on unknown top-level keys so a
misplaced `process.adopt` is visible in logs. Prefer (b) plus a one-line log warning; document the precedence in
the README.

Acceptance: loading a config with an unknown top-level key logs a warning naming the key; valid configs load
silently.

---

## Out of scope / non-goals

- Changing the adoption model itself (adoption stays the mechanism for surviving supervisor restarts; autostart
  is additive for cold boots).
- A general IPC/admin socket for an out-of-process CLI client (would be nice — see Appendix B — but not required
  here).

## Appendix A — systemd boot-order race (ops, not code)

Both frontends (and the host nginx) failed at boot because **Tailscale MagicDNS (`100.100.100.100`) was not ready**
when units started under `After=network.target`. Fix at the unit level: order the mezzaops units (and nginx)
`After=`/`Wants=` a Tailscale-online/DNS-ready target, or add a readiness `ExecStartPre` that waits for
`100.100.100.100:53` to resolve. C2 (frontend reconnect) makes the app tolerant regardless; the systemd fix
removes the first-boot failure window. This appendix is informational for the implementing agent — the unit
files live outside this repo.

## Appendix B — possible follow-up: admin control socket

Today the only way to command a running instance is via a chat frontend (Discord/Matrix/Mattermost) or `-i`
interactive stdin (unavailable under systemd). A small unix-socket admin endpoint (or authenticated HTTP POST on
the existing dashboard, which is currently GET-only) would allow `mezzaops ctl start-all` against the live
instance without a second Manager. Not required for this spec; noted because it would have made today's recovery
a one-liner.

## Suggested implementation order

1. C4 (panic guard) — smallest, removes a crash.
2. C6 (config visibility) — cheap, prevents future confusion.
3. C2 + C3 (frontend reconnect + ready robustness) — related; do together.
4. C1 (autostart) — the headline feature; depends on C3's ready machinery.
5. C5 (single-instance lock) — independent; do last.

---

## Implementation progress

Tracking the actual implementation of the changes above. Each entry lists
the change, commits, validation, and any deviations from the spec.

Status legend: `[ ]` not started · `[~]` in progress · `[x]` done

- [x] **C4** — Discord nil-session panic guard. Final commit: `4aff882` (amended from `9c64a90` after codex review). Extracted `Bot.setCommands(sess, cmds)` helper with nil guards for session, State, and State.User; routed the three call sites (post-open registration, shutdown deregistration, reload re-registration) through it. Codex also flagged a status-goroutine leak that the C2 retry loop would have amplified — fixed by moving the status goroutine and reconnect handlers to start only after `setCommands` succeeds, so a registration failure bails before any long-lived goroutine is registered. Tests: `TestSetCommands_NilSession`, `TestSetCommands_NilState`, `TestSetCommands_NilUser` against a constructed `discordgo.Session`. `make check` clean.
- [x] **C6** — Config strict-decoding / unknown-key warning. Final commit: `817f444` (amended from `7951b31` after codex review). Loads still go through `yaml.Unmarshal`; after the load, the file is unmarshaled a second time into `map[string]yaml.Node` (which resolves YAML merge keys and aliases) and `log.Printf` warns for each top-level key not in `knownTopLevelConfigKeys`. Load is non-fatal. README updated to document the strictness behaviour and the recognised top-level keys. Codex review caught two issues that were fixed in the amend: (1) the original walker operated on the raw AST and warned about `<<` itself instead of the merged-in keys (or skipped alias keys entirely); (2) the original tests asserted the substring `process`, which the help text happens to always contain for any unknown key. The fixed tests parse the actual unknown keys out of the warning line and add a YAML-merge regression test.
- [x] **C2** + **C3** — Frontend reconnect + bounded readiness wait. Final commit: `97b8ddd` (amended from `e7e817d` after two codex review passes). New package `internal/reconnect` with `RunWithBackoff(ctx, name, opts, op)` (1s→60s, ±20% jitter clamped to MaxDelay). All three frontends wrap their connect path in it: mattermost retries the initial REST setup and drops into its existing WebSocket reconnect loop; matrix wraps discovery + client construction + crypto + sync (New() is now lazy so boot-time discovery failures are retried, every attempt builds a fresh client because the previous attempt's crypto helper closes the SQLite database); discord wraps the full connect path with the status goroutine captured per-attempt and `b.session` mutex-protected. New `Config.ReadinessTimeout` (YAML `readiness_timeout`, default 30s) is the maximum time the app layer waits for all frontends to be Ready before firing `manager.SignalReady()`. The wait is implemented in `waitForReadyChannels` (a free function) and wired by `App.waitForReady`. Discord gained a `Ready()` channel that closes after the first successful `setCommands`. Concurrency hardening (from codex review): new `connMu` in matrix protects client/roomID/realClient; new `idMu` in mattermost protects userID/channelID; discord `b.session` is mutex-protected. Codex review of the amend pass caught a high-severity plaintext race (client was being published before `helper.Init` completed, so `PostMessage` could send plaintext into a room the operator expected to be encrypted) and a medium-severity discovery-cancel bug (`discoverClientAPI` used `context.Background()`). Both fixed: `runOnce` now builds a local `*mautrix.Client`, inits the crypto helper, then publishes the ready client atomically under `connMu`; `PostMessage` also drops messages when `client.Crypto == nil` as a belt-and-braces guard. `discoverFunc` and `resolveHomeserverURL` now thread `ctx` through to `mautrix.DiscoverClientAPI`. New regression tests: `TestPostMessage_DropsWhenCryptoUnset` and `TestRunOnce_DiscoveryRespectsContextCancel`. `make check` clean.
- [x] **C1** — Per-service `autostart` + global `start_on_boot` (cold-boot Start). Final commit: `75136a3` (amended from `00b95e5` after codex review). New `ServiceConfig.Autostart` (tri-state `*bool`) and `ServiceConfig.AutostartForce` (bool). New `Config.StartOnBoot` (default false). In `newManagedService`, after a ProcessBackend has tried adoption, the manager schedules a cold-boot "start" (not "restart") via a new `autostartOnReady` flag on `managedService` when (a) adoption failed, (b) autostart is enabled, and (c) the persisted `DesiredStatus` is not "stopped" (or AutostartForce is set). `DesiredStatus` is a new state field set by handleOp "start" / "stop" / "restart"; it is preserved across `saveServiceState` overwrites of the live `Status` field. This was added in response to a codex review finding: the first pass used `Status` (which is overwritten on process exit) and would suppress autostart for a crashed process. `serviceConfigEqual` also now compares `Adopt`, `Autostart`, and `AutostartForce` so reload picks up changes. Codex review of the amend caught that `start_on_boot` was missing from `knownTopLevelConfigKeys` so a valid C1 config was always logged as an unknown key — fixed by adding it to the map and adding `TestLoadConfig_StartOnBootNotFlaggedUnknown`.
- [x] **C5** — Single-instance lock on state dir (flock / pidfile). Final commit: `75136a3` (same commit as C1, since they were implemented together). New `acquireStateLock` opens `<state_dir>/mezzaops.lock`, takes `syscall.Flock` with `LOCK_EX|LOCK_NB`, writes `pid=<pid>`, and returns the file handle held for the Manager's lifetime. `Stop` calls `releaseStateLock`. A second `NewManager` on the same state dir fails fast with a descriptive error. The lock is per-process so a crashed instance does not leave a stale lock. Build-tagged: `lock_unix.go` covers `linux || darwin`, which are the only supported platforms; other platforms fail to build rather than silently skipping the lock.

## Follow-ups (deferred from this work)

Codex review of the final C1 + C5 + C2 + C3 commits surfaced
remaining concerns that touch the broader op-lifecycle and are
out of scope for the spec change. Documented here so a future pass
can pick them up.

### F1 — Legacy `status:"stopped"` state-file migration

Pre-C1 state files lack `desired_status` and produce
`status:"stopped"` for both operator stops and crashed processes;
the two are indistinguishable on disk. The current implementation
autostarts in the ambiguous case (a service the operator stopped
before upgrading will start once after the upgrade, and they can
re-stop it). Codex suggested migrating absent `desired_status` +
`status:"stopped"` to `DesiredStatus="stopped"` unless process
metadata shows an unexpected crash. A more refined heuristic could
use the presence of a non-stale PID or a process create-time on
the saved backend state to distinguish. Deferred because the
heuristic is still imperfect and the current behaviour is the
safer one for cold-boot recovery. Documented inline in
`newManagedService`.

### F2 — Reload records its internal stop as operator intent

`Manager.Reload` calls `m.Do(svc.Name, "stop")` on the old service
before swapping in a replacement. The `stop` op currently writes
`DesiredStatus="stopped"` (which is correct for an operator stop
but wrong here — this is an internal stop for the swap). The
replacement's `DesiredStatus` ends up "stopped" and a later cold
boot would suppress autostart. Fix: add an internal stop path
that preserves desired state, or have `Reload` read the desired
state before stopping and re-apply it to the replacement.

### F3 — Failed start/restart do not record the operator's new intent

`handleOp` "start" and "restart" write `DesiredStatus="running"`
**after** the backend call succeeds. If the backend call fails,
the intent is not recorded, so a prior `DesiredStatus="stopped"`
persists and a later cold boot suppresses autostart despite the
operator having asked for a start. Fix: set desired state when
accepting the command, before calling the backend, and cover the
failed-start case in `TestNewManager_FailedStartPreservesIntent`
(or similar).
