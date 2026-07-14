# promptster-teams-cli

## Capture surfaces

Capture is **transcript tailing only**. It installs no hooks, writes no
`settings.json`, and injects nothing into any editor. Two watchers poll the
filesystem every 3s:

- **Claude Code** — `internal/capture/cmd_claude_watch.go`. Tails
  `$CLAUDE_CONFIG_DIR/projects/<munged-cwd>/<session-uuid>.jsonl`
  (default `~/.claude/projects`, `claudeConfigDir()`/`ClaudeProjectsDir()`).
- **Codex** — `internal/capture/cmd_codex_watch.go`. Tails
  `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` (default `~/.codex`).

### What this means per surface

Every Claude Code surface — terminal CLI, the Mac/Windows desktop app, the
VS Code and JetBrains extensions — runs the same local engine and writes to the
same `~/.claude/projects` tree. Capture is therefore **surface-agnostic**: it
works across all of them for free, and no surface needs its own code path. That
is why nothing in this repo greps for "vscode", "desktop", or "ide" — the
distinction does not exist at the transcript layer.

**Not captured:**

- **Claude Desktop (the claude.ai chat app)** — a different product; it writes
  no `~/.claude/projects` transcripts. "Desktop" in a support question means
  the Claude Code desktop app, which *is* covered.
- **claude.ai/code web sessions** — run in the cloud, nothing lands on the
  developer's disk to tail.
- **Cursor and other non-Claude-Code assistants** — only Claude Code and Codex
  are wired up here.

### The real gate is cwd, not surface

`classifyClaudeTranscript` (`cmd_claude_watch.go:501`) ingests a transcript only
if its recorded `cwd` sits inside the capture workspace or one of its registered
git worktrees (`workspaceMatchRoots`, `cmd_claude_watch.go:477`). Codex applies
the same test (`cmd_codex_watch.go:272`). The workspace defaults to `os.Getwd()`
and is overridable with `PROMPTSTER_TEAMS_WATCH_DIR` (`teams.go:90`).

So a session is dropped when it runs outside the watched workspace — e.g. the
desktop app opened on a different folder — no matter which surface produced it.
When triaging "why didn't X get captured", check cwd before suspecting the
surface. Transcripts carry no surface marker at all, so capture could not
distinguish CLI from IDE even if it wanted to.

## Self-update (`internal/selfupdate`)

Facts below are verified against the code and by driving the real binary end-to-end.
Several are counterintuitive and were gotten wrong by reading summaries instead of the
dispatch — trust this section over inference.

### What triggers a check

Only **two** call sites outside the package:

- `internal/capture/teams.go` → `selfupdate.StartAutoUpdate(...)`, inside `RunTeamsWatch`.
- `internal/cli/teams_status.go` → `selfupdate.LatestVersionBestEffort(3s)`, read-only
  display for `doctor`. Never applies anything.

**`start` DOES check.** It is not a separate capture path — `StartTeamsDaemon` →
`StartDaemon` → `exec.Command(state.PromptsterBin(), "watch")` (`internal/capture/daemon.go`),
so the detached child runs the normal watch startup check within a second. Same for
`autostart`: launchd/systemd run `watch`, and `RunAtLoad` means a check every login.

Anything that never reaches `watch` never checks.

### Timing

- `updateCheckInterval = 24 * time.Hour`, `updateCheckPoll = time.Hour` (`selfupdate.go`).
- `runAutoUpdate` checks **once at startup unconditionally** — it ignores the persisted
  cursor. So a restart always forces a check.
- Steady state: an hourly ticker compares now against the cursor and acts once 24h have
  elapsed. Worst case release→update is therefore **~24–25h**, not 24h.
- Cursor: `state.GlobalPromptsterDir()/last-update-check`, RFC3339, mode 0600. Unreadable
  or unparseable → zero time → treated as stale → checks on the next tick.
- **No backoff.** The cursor advances after every check including failures, so a broken
  release is retried at most once per 24h rather than hot-looping.

### npm installs DO auto-update

The npm package is a wrapper: `npm/bin/promptster-teams.js` `spawnSync`s the real Go
binary from `<pkg>/binaries/promptster-teams-<platform>`. Self-update is a property of
the **running binary, not the install channel** — `os.Executable()` resolves to that
path inside `node_modules`, and the rename-over-self + `syscall.Exec` swap happens right
there. The node wrapper is waiting on a PID and does not care that the image changed
underneath it.

What is genuinely absent: there is **no `postinstall`**, so `npm install` itself never
triggers a check. That is the only npm gap.

**Known drift:** `npm/package.json` stays pinned at its published version while the
binary underneath self-updates, so `npm ls` / `npm outdated` will lie, and a reinstall
writes the older binary back. It self-heals within 24h because `isNewer` only moves
forward — cosmetic, but it will confuse fleet debugging.

### The not-writable nudge is channel-aware

When the install dir fails the `dirWritable` probe, `checkAndApply` prints `nudgeFor(self)`.
`isNpmInstall` looks for a `node_modules` **path segment** (not a substring) and splits on
both `/` and `\` — deliberately NOT `filepath.ToSlash`, which only rewrites `\` when
GOOS=windows and would make the check host-dependent and untestable from a unix CI runner.

Keep the hint matched to the channel: telling an npm-installed engineer to run the curl
installer drops a second binary in a different PATH entry and leaves a coin flip over
which one runs, while their stale copy stays stale.

### Gotchas when testing self-update locally

- **Local builds never update.** The gate skips when version is `"dev"` or `""`. To
  exercise the real path you must build with
  `-ldflags "-X github.com/pa-arth/promptster-teams-cli/internal/version.Version=0.0.1"`.
- **Isolate state or the single-instance lock refuses**: a real daemon on the dev machine
  makes `watch` print `capture already running (pid N) — not starting a second watcher`.
  Set `HOME` and `PROMPTSTER_STATE_DIR` to throwaway dirs. Never kill the developer's
  real capture process to free the lock.
- **macOS has no `timeout(1)`.** Background the process and `kill` it, or a
  `timeout ... | grep` pipeline fails and silently looks like "the feature didn't fire".

### Open edge

`runAutoUpdate`'s startup check ignores the cursor, so a crash-looping watch under launchd
(`ThrottleInterval` 10s, `internal/service/service.go`) hits
`api.github.com/repos/.../releases/latest` on every respawn and can burn the 60/hr
unauthenticated rate limit. `KeepAlive{SuccessfulExit: false}` limits this to genuine
crashes. Fix would be to honor the cursor at startup unless it is older than ~1h.
