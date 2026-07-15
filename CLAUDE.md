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

### How the check resolves a version (NOT the JSON API)

`fetchLatestTag` issues a **HEAD that does not follow redirects** against
`github.com/<slug>/releases/latest` and reads the tag out of the `Location`
(`.../releases/tag/v0.6.1`). It deliberately does **not** touch
`api.github.com/repos/.../releases/latest`. Measured difference:

| | releases/latest redirect | api.github.com JSON |
|---|---|---|
| Response | ~0 bytes (headers only) | **~20,600 bytes** |
| Rate limit | none (CDN, no `x-ratelimit` at all) | **60/hr unauthenticated per IP** |

That 60/hr per-IP ceiling — shared by an entire fleet behind one corporate NAT — is the
sole reason the cadence used to be 24h. Reading the redirect removes the cage, which is
what makes a 30m cadence affordable. **Do not "simplify" this back to the JSON API for a
tidier parse**; the parse is not the point, the rate limit is. `LatestVersionBestEffort`
(`doctor`) reads the same redirect for the same reason — it is the one command engineers
run repeatedly while something is already wrong.

The tag is treated as untrusted input (it is interpolated into the download URL), so
`tagFromReleaseLocation` rejects anything with a path separator. That is defence in depth,
not the trust boundary: minisign-over-SHA256SUMS still gates every installed byte.

### Timing

- `updateCheckInterval = 30 * time.Minute`, `updateCheckPoll = 5 * time.Minute`
  (`selfupdate.go`). Worst case release→installed is **~35m** (was ~24h05m).
- `runAutoUpdate` checks **once at startup unconditionally** — it ignores the persisted
  cursor. So a restart always forces a check.
- Steady state: the poll compares now against the cursor and acts once the current
  interval has elapsed.
- Cursor: `state.GlobalPromptsterDir()/last-update-check`, RFC3339, mode 0600. Unreadable
  or unparseable → zero time → treated as stale → checks on the next tick.
- **No backoff.** The cursor advances after every check including failures, so a broken
  release is retried at most once per interval rather than hot-looping.

### Why 30m, and what the 24h was actually buying

The old note here claimed *"the 24h stagger is the only canary window that exists"*. That
was half-true and the half that was false mattered: the stagger is keyed to whenever each
daemon happened to start, so it was never a **deliberate** canary — just randomly-spread
blast radius. What it did genuinely buy is time to **yank a bad release** before most of
the fleet took it, and that lever still works at 30m: a deleted release stops being
`releases/latest`, so machines that have not updated never will.

What tips the balance: **a fast cadence cuts time-to-RECOVER by the same factor it cuts
time-to-break.** At 24h a bad release poisons machines for a day *and* the fix takes
another day to land. At 30m, both are 30m.

A real canary is a **channel** — a `stable` pointer lagging `latest`, which is what Claude
Code does (`downloads.claude.ai/claude-code-releases/{latest,stable}` were 8 versions apart
when measured). That is the follow-up. **Do not re-approximate it by raising the
interval.**

### The minCliVersion escalation floor

`u.checkInterval()` returns `belowMinCheckInterval` (15m) instead of the 30m cadence while
the running version is below the org's `minCliVersion`. It is the emergency lever for a
security fix. Absent/empty field ⇒ nothing changes.

Note the floor now buys much less than it did: 15m against a 30m base, versus 15m against
24h. It is kept because it still halves the worst case under an active security rollout and
costs nothing when unset — **not** because it is still the load-bearing thing it was.

Two properties that remain load-bearing:

- **The floor moves the CADENCE only.** `checkAndApply` still enforces the org auto-update
  switch and any pin, so a floor can neither override an opt-out nor drag a pinned fleet
  past its pin. It never changes *which* tag is installed.
- **15m is a RETRY FLOOR, not a target.** A fleet below the floor that cannot update
  (upstream down, release yanked) should not re-check every poll. The 60/hr rate limit that
  originally made this critical is gone (the check no longer hits the JSON API), so this is
  now ordinary politeness rather than self-preservation.

The lever only works on CLIs that already understand the field, and the CLI is the
slow-propagating side — so it cannot help the fleet that is live when you need it. That is
why it shipped before there was a server to send it.

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
writes the older binary back. It self-heals within 30m because `isNewer` only moves
forward — cosmetic, but it will confuse fleet debugging.

### The not-writable nudge must update THE COPY THAT PRINTED IT

When the install dir fails the `dirWritable` probe, `checkAndApply` prints `nudgeFor(self)`.

The invariant: any hint that installs somewhere other than `self` drops a second binary in
a different PATH entry, leaves a coin flip over which one runs, and leaves the stale copy
stale — the exact failure the hint exists to fix. Three ways to violate it, all easy to
walk back into:

- **Pointing `nudgeCurl` at the wrong PRODUCT.** It shipped as
  `curl -fsSL https://get.promptster.ai | sh` — which is the **hiring CLI's** installer
  (`promptster` from `pa-arth/promptster-cli-releases` into `~/.promptster/bin`). It
  installed an unrelated product and left promptster-teams exactly as stale as it was.
  `nudgeCurl` must name **this** repo's `install.sh`
  (`raw.githubusercontent.com/pa-arth/promptster-teams-cli/main/install.sh`), which writes
  `~/.promptster-teams/bin/promptster-teams` — the same path a curl-installed `self`
  resolves to. `TestNudgeCurlInstallsThisProduct` pins the CONTENT; every other nudge test
  compares against the `nudgeCurl` *constant* and therefore stayed green for the entire
  life of the bug. **A constant-vs-constant assertion proves nothing about a URL.**
- Telling an npm-installed engineer to run the curl installer.
- Telling a **project-local** or pnpm install to `npm i -g` — that updates the global
  prefix and leaves the local copy untouched. Global-vs-local matters more than
  npm-vs-pnpm.

So only the documented **global** layouts (`<prefix>/lib/node_modules`,
`<AppData>\npm\node_modules`, pnpm's `global`) get a copyable command. Anything else under
`node_modules` names the package and the project dir and stops there: the path cannot tell
npm from yarn, and guessing is the same second-install bug again.

Path checks match a `node_modules` **path segment** (not a substring) and split on both
`/` and `\` — deliberately NOT `filepath.ToSlash`, which only rewrites `\` when
GOOS=windows and would make the checks host-dependent and untestable from a unix CI runner.

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

- **A fake key is enough.** `watch` exits early with `no developer key configured` before
  it ever reaches `StartAutoUpdate`. Any format-valid key gets you past it
  (`PSE-` + six 4-char groups, base32 alphabet — no `I`/`O`/`0`/`1`); ingest then 401s
  harmlessly, which does not touch the update path.
- **Evidence the swap really happened**: the binary's sha256 changes, and the startup
  banner prints **twice** — the second is the `syscall.Exec` re-exec. Confirm the installed
  bytes against the published `SHA256SUMS` rather than trusting the version string.

### Open edge (largely defused, not gone)

`runAutoUpdate`'s startup check ignores the cursor, so a crash-looping watch under launchd
(`ThrottleInterval` 10s, `internal/service/service.go`) re-checks on every respawn.
`KeepAlive{SuccessfulExit: false}` limits this to genuine crashes.

This used to risk burning the 60/hr `api.github.com` limit. Since the check is now a
header-only HEAD against a CDN with no rate limit, the blast radius is down to wasted
requests. Still worth honoring the cursor at startup unless it is older than ~1h — but note
that the unconditional startup check is also **the documented way to force an update now**
(restart the daemon), so anything here must keep that escape hatch.
