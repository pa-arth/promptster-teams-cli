# promptster-teams-cli

## Capture surfaces

Capture is **transcript tailing** for all three tools, plus **one hook rail for
Cursor alone** (see below — Cursor's transcript is the only one thin enough to
need it). It writes no `settings.json` and injects nothing into any editor.
Three watchers poll the filesystem every 3s:

- **Claude Code** — `internal/capture/cmd_claude_watch.go`. Tails
  `$CLAUDE_CONFIG_DIR/projects/<munged-cwd>/<session-uuid>.jsonl`
  (default `~/.claude/projects`, `claudeConfigDir()`/`ClaudeProjectsDir()`).
- **Codex** — `internal/capture/cmd_codex_watch.go`. Tails
  `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` (default `~/.codex`).
- **Cursor** — `internal/capture/cmd_cursor_watch.go`. Tails
  `~/.cursor/projects/<munged-workspace>/agent-transcripts/…` (three layouts:
  `<uuid>.jsonl`, `<uuid>/<uuid>.jsonl`, `<uuid>/subagents/<uuid>.jsonl`).
  `PROMPTSTER_CURSOR_HOME` is a **test-only** override — it is ours, not a
  documented Cursor variable, unlike `CODEX_HOME`/`CLAUDE_CONFIG_DIR`.

### Cursor runs TWO rails: a hook (primary) and the transcript (fallback)

Cursor is the only tool with two rails, because its transcript is the only one
that is genuinely too thin: it carries `role` + `message.content[]` and nothing
else — **no model, no cwd, no timestamp, no tool result, no tokens** (verified
exhaustively across 64 files / 1504 lines). Both other tools' transcripts carry
all of that.

**The hook config is USER-SCOPE, and that is the whole reason this rail exists.**
Cursor reads hooks from four scopes; the one we write is
**`~/.cursor/hooks.json`** — a single global file, one-and-done, exactly like
Claude Code and Codex enrollment. It is **not** the project-local
`<workspace>/.cursor/hooks.json`, which is the scope Cursor's docs lead with and
the one this CLI must never write:

- **A project-local hooks.json is a tracked file inside the customer's repo.**
  Writing into a customer repo is a line this CLI does not cross, and one
  committed by an engineer silently enrolls every teammate who pulls it.
- **It is per-workspace, so enrollment is per-repo** — every repo an engineer
  forgets reads as "captured nothing", the exact failure this work exists to fix.

(An earlier revision of this file claimed hooks had been *rejected outright* on
those two grounds. That was wrong: it read the docs' project scope as the only
scope. The user-level file has neither problem, and the mistake cost real time —
so state the scope, not just "hooks".)

`internal/capture/cursor_hooks.go` writes it, and enrollment happens
automatically at `watch` startup via `EnsureCursorHooksBestEffort()` — so an
already-installed fleet gets the hook rail purely by self-updating (~30m) and
re-execing. Nobody reinstalls anything. `loadCursorHookConfig` preserves
top-level keys it does not model and **refuses to overwrite a hooks.json that
does not parse** — an engineer's own config is never clobbered.

**Registered steps** (`cursorHookSteps`): `sessionStart`, `sessionEnd`,
`beforeSubmitPrompt`, `afterFileEdit`, `afterShellExecution`,
`postToolUseFailure`, and `afterAgentThought`. An unregistered step emits
nothing — its payload has not been audited for source content, so silence is the
safe answer, and `TestCursorHookUnregisteredStepsEmitNothing` pins that.

**`afterAgentThought` is registered for `model_id` ALONE.** Empirically it is the
step that sometimes carries a real model; every other step — and often this one
too — reports `model` / `model_id: "default"` (routing, not a model). The
normalizer rejects that sentinel (omit the field, never emit `"default"`). So it
emits one `ai_response` carrying `{model}` and nothing else; its reasoning text
is the agent's own prose and never leaves the machine. `model` was already
allowlisted on `ai_response` on **both** sides (`projectUsageFields` /
`eventFieldProjection.ts`), so this rail needed **zero allowlist change** —
which is why MUST-DO #2 is not in play here.

**Rail handoff is an identity, not a heuristic.** The hook payload names the
exact `transcript_path`, and `conversation_id == session_id ==` the transcript
uuid. `cursor_hooks.go` keeps a claims ledger (7-day TTL, `sign.WithBufferLock`)
and the watcher **skips a hook-claimed transcript while advancing its offset to
EOF without emitting** — so if the hook rail ever stops covering a session
(uninstall, TTL expiry) the watcher resumes from there instead of replaying the
whole file. Same session, one rail, never both.

**The hook runs synchronously inside the engineer's agent loop.** That is the one
way this rail can hurt someone the watcher never could, so `cmd_cursor_hook.go`
holds three invariants, in descending order of regret: (1) it always exits 0 and
never blocks past `cursorHookBudget` (2s, enforced with a goroutine + `select`,
not assumed); (2) it does **no network I/O** — events go to the durable outbox
and the daemon ships them, so ingest latency and ingest outages stay out of the
agent loop; (3) it **redacts before it parses**, matching the raw-JSON ordering
the other rails use, because the payload holds file bodies, command output and
the engineer's email.

**Two facts that cost real time, both settled empirically — do not re-derive
them from documentation:**

- **`duration` is fractional MILLISECONDS.** Read as seconds, a 2-second
  `go build ./...` (`duration: 2021.129`) reported as **33 minutes**. Nothing
  downstream can tell that a duration is implausible.
  `TestCursorHookCommandDurationIsMilliseconds` pins it.
- **Promptster does not capture Cursor token counts.** Transcripts,
  `state.vscdb`, `conversation-search.db`, and per-session `chats/*/store.db`
  expose none. Cursor's bundle *constructs* token fields for `stop` /
  `afterAgentResponse`; headless `cursor-agent -p` never fires those steps.
  IDE `stop` *is* requested and other enrolled hooks (claude-user →
  `presence.sh`) have been seen with nonzero `input_tokens` in Cursor's own
  hooks log — but a logger enrolled in **our** `~/.cursor/hooks.json` got
  **zero** stop scratch files in a 2026-08-03 IDE probe, so we do not register
  `stop` and do not claim those tokens. `afterAgentResponse`: zero platform
  requests in that session. Token fields stay **ABSENT, never zero**.
  `model` / `model_id: "default"` is a routing sentinel and is rejected (omit
  the field), including on `afterAgentThought`.

**Cursor transcripts carry no cwd and no timestamp.** Both gaps are worked around
in ways that are easy to "simplify" back into bugs:

- **cwd** — `cursorClassify` reads absolute paths the agent actually *touched*
  (`normalize.CursorObservedPath`) rather than parsing the munged directory name.
  Two munging behaviours are observable on one machine: a full-length name, and a
  stem truncated to 43 chars with a 7-hex suffix. The truncated form is not
  reversible, so the directory name cannot be a key. A transcript that has not
  revealed a path yet is **UNDECIDED, never NO** — caching a "no" on a
  still-growing file is what once dropped whole Codex sessions.
- **time** — a forward-carrying anchor parsed from the `<timestamp>` envelope on
  user turns, falling back to read time. Intra-turn latency is not recoverable
  from this surface; don't pretend otherwise downstream.
- **event ids** — `sessionID + kind + byteOffset + itemIndex` through
  `event.DeterministicUUID`. Deliberately not a counter: a counter restarts at 0
  mid-file after a daemon restart and mints ids that collide with earlier
  *different* records, at which point dedupe eats real events.

**Pre-existing vs new is decided by the FIRST POLL**, since there is no timestamp
to ask. Transcripts already on disk when the watcher starts are seeded to EOF; one
that appears on a later poll is tailed from 0. Both directions matter: seed
everything and every new session loses its opening prompt (a transcript only
becomes classifiable at its first tool call, several records past the prompt);
tail everything from 0 and the first daemon run re-uploads months of history.

### Cursor egresses counts, never code

**This is the constraint the whole Cursor port exists under, and the hook rail
makes it sharper, not looser** — a hook payload hands us *more* code than a
transcript does, not less. The hiring CLI's `buildDiffFromCursorEdits()`
synthesizes a unified diff from `old_string`/`new_string`; that is correct there
and forbidden here.

Exactly two functions in the repo read `old_string`/`new_string`, one per rail,
and **both return only integers**:

- transcript — `cursorLineCount` in `normalize_cursor.go`
- hook — `cursorHookEditLineCounts` in `normalize_cursor_hook.go`

No patch, no file body, no diff, ever — including in `RawPayload` (never
populated on either rail) and in every error and log path. The hook rail
additionally drops `user_email` (present on **every** hook payload), `tool_output`
and `error_message`, all of which carry code or PII. `outcome_events` has a DB
CHECK that rejects patches and file bodies, so this is enforced downstream too,
but the CLI must not rely on that.

Four tests pin it and all four were mutation-tested (deliberately broken,
confirmed failing, reverted): `TestCursorPrompt_DropsAttachedFileContents`,
`TestCursorFileDiff_CountsOnlyNeverCode`,
`TestCursorHookFileEdit_CountsOnlyNeverCode` and
`TestCursorHookDropsUserEmailAndOtherUnallowlistedFields`. Prompt extraction on
the transcript rail is a **whitelist** — only the `<user_query>` span survives —
so Cursor's attached-file context and rules blocks fail closed.

**Do NOT read `~/.cursor/chats/<hash>/<uuid>/store.db`.** It sits next to a
`meta.json` that would cheaply solve the transcript rail's cwd and timestamp
gaps, and it holds full unified diffs, file contents and raw stdout. The
neighbouring `meta.json` is safe; the sibling is a loaded gun.

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
- **Non-Claude-Code, non-Codex, non-Cursor assistants** (Windsurf, Copilot,
  Aider, …) — nothing is wired up for them.
- **Cursor's IDE-side chat that never invokes the agent** — only agent sessions
  land in `~/.cursor/projects/…/agent-transcripts`. Tab completions do not.

### The real gate is cwd, not surface

`classifyClaudeTranscript` (`cmd_claude_watch.go:501`) ingests a transcript only
if its recorded `cwd` sits inside the capture workspace or one of its registered
git worktrees (`workspaceMatchRoots`, `cmd_claude_watch.go:477`). Codex applies
the same test (`cmd_codex_watch.go:272`), and Cursor applies it to observed paths
instead of a recorded cwd (`cursorClassify`, `cmd_cursor_watch.go`). The
workspace defaults to `os.Getwd()` and is overridable with
`PROMPTSTER_TEAMS_WATCH_DIR` (`teams.go:90`).

So a session is dropped when it runs outside the watched workspace — e.g. the
desktop app opened on a different folder — no matter which surface produced it.
When triaging "why didn't X get captured", check cwd before suspecting the
surface. Transcripts carry no surface marker at all, so capture could not
distinguish CLI from IDE even if it wanted to.

## Uninstall (`uninstall`, `internal/cli/uninstall.go`)

`uninstall` is the ONLY uninstall path that exists, and it has to be, because
**the package manager cannot perform one.** Both halves measured against npm
11.5.2 in a sandboxed global prefix — do not re-derive either from npm's docs,
which list the lifecycle scripts as though they run:

- **`npm rm -g` runs NO uninstall lifecycle script.** `preuninstall` and
  `postuninstall` fired ZERO times on removal; `postinstall` fired on install and
  again on upgrade. So a `preuninstall` hook is not a fix that was skipped — it
  is a fix that does not exist to wire up. (It would also be the wrong shape if it
  did fire: firing on upgrade would unenroll hooks on every `npm i -g`.)
- **`npm rm -g` leaves files postinstall created outside `node_modules`.** The
  managed binary is exactly such a file, deliberately — that indirection is what
  keeps `npm ls` honest (see the self-update section).

Composed, those give the worst available outcome: `npm rm -g
@promptster/teams-cli` reports success while capture keeps running, keeps
self-updating, and returns at the next login from a binary the engineer believes
they deleted. That is why README, `help`, and `install.sh`'s closing hint all
name `uninstall` explicitly.

What it reverses, in order: autostart (Disable, not Stop — Stop leaves the unit
registered and capture returns at login) → the daemon → `~/.cursor/hooks.json` →
the wrapped `statusLine`. `--purge` additionally deletes the state dir(s).

- **THE CURSOR HOOK IS THE ITEM THAT OUTRANKS THE REST**, small as it looks.
  Delete the binary by hand — the obvious move for a curl install — and the
  hooks.json entry survives naming a path that is gone, so Cursor execs a missing
  command *inside the engineer's agent loop* on every prompt, edit and shell
  call. That degrades THEIR tool rather than our data. It is also the one piece
  nothing on our side can self-heal: if the binary is gone, none of our code runs.

  `doctor` is therefore the ONLY place that state can ever be reported
  (`CursorHooksDoctor`, `cursor_hooks_doctor.go`) — it needs a working binary to
  say anything, and doctor is the command engineers run while something is
  already wrong. It is strictly READ-ONLY: enrolling or repairing from a
  diagnostic would write hooks.json on a machine whose owner only asked what was
  broken, and a check that mutates what it measures cannot be trusted to describe
  it (`TestCursorHooksDoctorWritesNothing`).

  **The dangerous direction there is the FALSE ALARM, not the miss.**
  `cursorHookCommand` renders the path with `%q`, so a Windows entry arrives
  backslash-escaped; a naive scan to the next quote returns the path with every
  separator doubled, `os.Stat` calls it missing, and every healthy Windows
  machine prints the most alarming line doctor has. `cursorHookCommandBinary`
  closes the quoted token with escape awareness and `strconv.Unquote`s it, and
  the mutation to the naive scan is pinned by
  `TestCursorHooksDoctorDoesNotFalseAlarmOnAnEscapedPath`.
- **Every step runs even when an earlier one fails.** Short-circuiting on the
  first error is precisely how the hook entry survives a removal.
- **Liveness is read BEFORE and AFTER the stop** (`capture.CaptureRunning`, the
  lock — not `DaemonStatus`'s recorded PID, which a bare `watch` under autostart
  never writes). "stopped" printed over a machine where nothing ran is a harmless
  lie; printed over capture that is still alive it is the lie an engineer acts on,
  so that case is a FAILURE with a non-zero exit.
- **`--purge` on Windows cannot unlink the running image.** A plain `RemoveAll`
  would abort part-way and leave an arbitrary remainder — quite possibly the
  credentials. So the running binary is skipped explicitly, named in the output,
  and everything else still goes (`removeAllExcept`).
- **The tests fake ONLY autostart + daemon-stop, and must keep doing so.**
  `launchctl bootout/bootstrap` target `gui/$UID` by LABEL, so a sandboxed `HOME`
  does not protect the developer's real `ai.promptster.teams` job — same trap as
  the autostart section below. The file-touching steps do respect `HOME` and run
  for real. Four assertions mutation-tested (broken, confirmed failing, reverted):
  hook removal wired up, no short-circuit, `--purge` gated on the flag, post-stop
  liveness check.

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
`StartDaemon` → `exec.Command(state.SelfBin(), "watch")` (`internal/capture/daemon.go:147`),
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

### npm installs DO auto-update — and the binary does NOT run from node_modules

**The binary that runs is never the copy npm is tracking.** `npm/scripts/postinstall.js`
copies the platform binary out of `node_modules` to the MANAGED path
(`~/.promptster-teams/bin/promptster-teams` — the same file install.sh writes), and
`npm/bin/promptster-teams.js` execs that. So `os.Executable()` resolves to the managed
path, self-update swaps THAT file, and node_modules keeps a pristine copy nothing ever
mutates.

That is the whole point, and it is load-bearing: **`npm ls` / `npm outdated` stay true by
construction**, because npm's copy genuinely never changes.

Three paths must name the same file or an npm install and a curl install manage two
different binaries on one box and PATH decides which one actually runs:

- Go: `state.CanonicalInstallBin()` (`internal/state/hooks.go`) — handles the Windows
  `.exe` tail.
- npm: `managedBinPath()` in `npm/lib/resolve.js`.
- shell: `INSTALL_DIR` in `install.sh`.

**Why not the obvious alternatives** (both were evaluated and rejected on evidence):

- *Rewrite `package.json` after the swap.* Verified to work for global installs, but it is
  a hack no mature CLI does, it couples us to npm internals, and it does **nothing** for
  project-local installs — `npm ls` reads the lockfile there, not the installed
  package.json (verified: rewriting package.json to 0.6.1 left `npm ls` reporting 0.5.6).
- *Update by re-running npm*, which is what **claude and codex actually do** (Claude Code's
  binary contains `npm/bun global installs → npm view @anthropic-ai/claude-code@<channel>
  version --registry ...` and an `update_apply_no_permissions` path for a failed global
  npm install; Codex's `run_update_action` shells out to npm/bun/pnpm/brew). Correct for
  them, wrong for us: **they are interactive and we are a daemon.** Their nudge reaches a
  human who acts; ours reaches a log nobody reads, so "ask the package manager to update"
  degrades to "never update" — the original bug this package exists to fix.

**GLOBAL installs only. A project-local install is pinned by its lockfile** and is left
entirely alone — it keeps running its own copy out of `node_modules`. Two halves enforce
this and BOTH are needed:

- `isGlobalInstall()` (`npm/lib/resolve.js`) — postinstall skips, and the launcher runs the
  bundled binary, so a pinned project never executes the shared managed one.
- `isProjectLocalInstall()` (`internal/selfupdate`) — self-update refuses to swap it
  (`outcomeBlockedProjectLocal`) and nudges instead. Checked BEFORE `dirWritable`, because a
  project's `node_modules` is almost always writable: writability is not the question,
  ownership is.

Without the first half, a repo pinning 0.5.0 and one pinning 0.6.1 both execute whatever is
in `~/.promptster-teams/bin` and the lockfile selects *nothing* — strictly worse than the
drift. Without the second, the daemon swaps a pinned copy and the developer silently
diverges from `npm ci`. A lockfile is a deliberate pin and gets the same respect as the
org's `PinnedCliVersion`.

**Invariants, in descending order of how badly you'd regret breaking them:**

- **postinstall must never fail an install.** A non-zero exit aborts `npm i -g` and leaves
  the engineer with no CLI at all — far worse than the drift. Every path warns and exits 0;
  the launcher then falls back to the bundled binary and behaves exactly as it did before
  this existed.
- **postinstall must never downgrade.** The managed binary self-updates forward on its own,
  so it is routinely NEWER than the version npm is installing — that is the steady state,
  not an error. The guard compares the BUNDLED BINARY's `--version` against the MANAGED
  binary's, not `package.json`'s: package.json describing bytes it does not actually
  contain is exactly how a guard ends up deciding on a fiction.
- **`scripts/postinstall.js` and `lib/resolve.js` must ship in the tarball.** `.npmignore`
  lists `scripts/`; `files` in package.json currently wins (verified via
  `npm pack --dry-run`), but that hinges on a precedence rule between two files that both
  look authoritative. `check-binaries.js` (prepublishOnly) asks the packer directly and
  hard-fails the publish — because losing the postinstall reverts everything **silently**:
  the CLI keeps working, so nothing breaks until someone notices `npm ls` lying weeks later.

### autostart bakes an ABSOLUTE path — moving the binary is a migration

`autostart enable` renders `state.SelfBin()` into the launchd plist / systemd unit /
scheduled task **once**, and nothing revisits it. So any change to where the binary lives
silently orphans every already-enabled unit.

That is not hypothetical — it was caught on a real machine mid-review. A live plist read:

```
ProgramArguments: [.../node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64, watch]
```

The wrapper no longer ships `binaries/` at all, so the next `npm i -g` deletes exactly that
file. **Nothing fails loudly**: the running daemon holds its inode and capture looks fine,
then at the next login launchd runs a path that is gone and capture never comes back — the
precise failure autostart exists to prevent.

`autostart repair` (`internal/cli/autostart.go`) is the migration, and npm's postinstall
calls it after installing the managed binary. Rules:

- **It must never exit non-zero.** It runs inside `npm install`, where a non-zero exit
  aborts the install and leaves the engineer with no CLI at all — far worse than a stale
  unit path.
- **It re-renders unconditionally** rather than reading the unit's current path back:
  that would mean a plist parser, a systemd-unit parser and a schtasks-XML parser, three
  platforms to avoid one idempotent bootout/bootstrap.
- **It skips the key check** `autostart enable` does. The unit only exists because the
  engineer had a key when they enabled it; a transient key problem is no reason to leave
  the path broken.
- The linux smoke test in `ci.yml` proves it end-to-end: bake a unit with a path, delete
  that path, repair, assert the unit now names a binary that exists and the stale one is
  gone — plus the no-op-when-not-enabled case.

**macOS autostart is NOT covered by CI** (the smoke matrix is ubuntu + windows). It CAN be
tested on a dev machine, but NOT by sandboxing `HOME`: `launchctl bootout/bootstrap` target
`gui/$UID` by LABEL, so a sandboxed HOME still tears down the developer's real
`ai.promptster.teams` job and bootstraps the sandbox plist in its place. `Status()` is the
exception — it reads the plist under `os.UserHomeDir()`, so a sandboxed HOME correctly
reports "not enabled" and `repair` returns before touching launchctl.

To test it for real (done once, 2026-07-15, and it is how the stale-path bug was proven):
back up `~/Library/LaunchAgents/ai.promptster.teams.plist`, run the round-trip, then restore
the file and `launchctl bootstrap` it. Verify the restore by sha256 of the plist, and check
that the binary left capturing matches the published `SHA256SUMS` — a local build stamped
with the CURRENT version will NOT self-update away (`isNewer` is strict), so leaving one
behind strands the machine on unreleased code indefinitely.

Two traps that cost real time here:

- **`stop` boots the job out of the launchd domain** (it disarms the supervisor before
  signalling the watcher — see Manager.Stop). After a `stop`, `launchctl kickstart` fails
  with `Could not find service ... in domain`; you must `launchctl bootstrap` the plist
  again, which is also exactly what a real login does.
- **A running daemon holds the single-instance lock, so launchd's spawn exits 0** and the
  job reads `state = not running`. That is SUCCESS, not failure — and `last exit code = 0`
  is itself the proof launchd could execute the binary at that path (a missing path gives a
  spawn error instead). To see it actually capture, free the lock first.

### The binary ships as a per-platform optionalDependency

The wrapper carries **no binary**. It declares six packages
(`@promptster/teams-cli-darwin-arm64`, …) as `optionalDependencies` pinned to its exact
version; each carries one binary and is gated by npm's `os`/`cpu` fields. npm installs only
the match, so an install pulls **12MB instead of 74.5MB** (measured: wrapper 28KB + one
stripped binary; the old all-in-one tarball was 74.5MB unpacked). Same pattern as
esbuild/swc/rollup, and as Claude Code (`@anthropic-ai/claude-code-darwin-arm64`, each
pinned to the wrapper's exact version).

`npm/binaries/` still exists — it is the GitHub Release assets + `SHA256SUMS` that
`install.sh` and the Go self-updater download. It just no longer ships inside the npm
wrapper. `scripts/build.js` emits both from one compile and is the source of truth; it also
SYNCS `optionalDependencies` to the version so six pins cannot drift on a release bump.

**The tradeoff, and it is a sharp one: a missing optionalDependency is a SILENT SUCCESS.**
npm exits 0 with no warning (verified: installed the wrapper with the platform package
unresolvable — `npm i` reported success and the CLI had no binary). Three defences:

- **Publish order in `release.yml` is load-bearing.** Platform packages publish FIRST, then
  a step asserts every pin resolves on the registry, and only then the wrapper. Publishing
  the wrapper first would open a window where `npm i -g` yields a silently broken install;
  this way the worst case is a loud release failure with no wrapper published.
- **`check-binaries.js` (prepublishOnly)** fails if any pin ≠ the wrapper version (a
  split-brain release: binary from one release, wrapper from another) or any platform
  package was not built. Mutation-tested: both exit 1 naming the exact package.
- **`bundledBinPath()` returns null and every caller degrades.** The launcher names the
  missing package and the likely cause (`--omit=optional`) rather than printing "binary not
  found", because that error is the only signal the engineer will ever get.

**Remaining npm gaps** (all real, none fixed here):

- **postinstall races the daemon (known, accepted).** The version guard and the rename are
  not atomic against the Go self-updater, which renames onto the same path: read 1.0.0 →
  daemon installs 1.2.0 → postinstall renames 1.1.0 over it, guard defeated. postinstall
  re-checks immediately before the rename, which narrows the window to microseconds but does
  not close it. Closing it needs a lock protocol shared between a Node script and a Go
  daemon. Not worth it: rename is atomic so the file is always ONE whole valid binary
  (never corrupt), the only cost is running an older version, and the daemon re-updates
  forward within ≤30m. Raised by review on PR #64.
- `--ignore-scripts` skips postinstall, so the launcher falls back to the bundled binary and
  the old in-node_modules drift returns. Working-but-drifting beats not working.
- **The node wrapper stays, and it is now load-bearing** — do not "optimise" it away.
  Claude Code's postinstall hardlinks the native binary over `bin/claude.exe` so npm's bin
  IS the binary and no node process is involved. Copying that would put `os.Executable()`
  back inside node_modules, self-update would swap it, and the npm drift would return — it
  would undo this whole design. They can do it only because they do NOT auto-update the npm
  copy; we must, because nobody reads a daemon's stderr. The cost is ~30ms of node startup
  on short foreground commands ONLY: the daemon does not run under node (autostart writes
  `state.SelfBin()` — the Go binary — into the launchd plist / systemd ExecStart, and a live
  daemon shows PPID 1, no node parent).

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
  (`raw.githubusercontent.com/pa-arth/promptster-teams-cli/main/install.sh`).
  `TestNudgeCurlInstallsThisProduct` pins the CONTENT; every other nudge test compares
  against the `nudgeCurl` *constant* and therefore stayed green for the entire life of the
  bug. **A constant-vs-constant assertion proves nothing about a URL.**
- **Sending a standalone binary to `install.sh` at all.** `install.sh` hardcodes
  `INSTALL_DIR="${HOME}/.promptster-teams/bin"` — it writes ONE path. So `nudgeCurl` is
  correct *only* when `self` is already that exact file; a root-owned
  `/usr/local/bin/promptster-teams` or a Homebrew-prefix copy that re-runs it gets a
  second binary in a different PATH entry and stays stale. `nudgeFor(self, curlDest)` gates
  on `samePath`, and everything else falls to `nudgeStandalone`, which names the file and
  the releases URL and prescribes nothing. The path comes from
  `state.CanonicalInstallBin()` — the same helper npm's postinstall and install.sh target,
  and the one place that gets the Windows `.exe` tail right. Caught by review on PR #63,
  after the wrong-product fix had already been written and tested.
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
- **Killing the npm shim does NOT kill the daemon.** `npm/bin/promptster-teams.js` is a node
  wrapper that `spawnSync`s the Go binary, so backgrounding the shim and `kill`ing that pid
  leaves the Go child alive, reparented to pid 1, pointing at a sandbox you are about to
  delete. Four such orphans accumulated in one session this way. Kill the Go pid
  (`pgrep -f 'promptster-teams.*watch'`), or use `promptster-teams stop`, and check for
  strays before finishing — filter by the scratchpad path so the developer's real daemon is
  never in the blast radius.
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

## Durability ledger (`internal/capture/durability.go`)

The invariants live in that file's header comment — read it before changing
anything in the ledger; it is the authoritative statement and is kept current.

The one framing worth carrying in before you open it: this subsystem's worst
failure mode is **FABRICATION**, not loss. Losing a tracked line understates AI
impact; inventing one (attributing human code as AI) or inventing survival
(emitting `durable` for lines that did not survive) corrupts the only number the
product exists to report. Every tradeoff in there is resolved toward a
conservative undercount, and a change that trades an undercount for any kind of
invention is wrong even when it improves coverage.

Two fabrication classes were live in shipped code and are now pinned by
`durability_fabrication_test.go`: a path that leaves the ledger re-arming
first-touch seeding, and a rename stranding spans at a dead path. Both were
reachable through ordinary git usage, so treat any new path that removes or
relocates a ledger entry as suspect until it has a test.

**The pre-merge rework ledger (`rework.go`) is a SECOND ledger holding the SAME
invariants**, and both fabrication classes were reachable through both. Fix them
together or the fix is half a fix.

Two things about it that are easy to get wrong, both learned the expensive way:

- **A seed tombstone blocks INFERENCE, not seeding.** The unsafe act was never
  "seeding a path twice" — it was seeding a path as AI because we had merely seen
  it before. So a tombstone stores the AI-write evidence already spent on that
  path, and only a *strictly newer* per-path AI write stamp — an agent
  demonstrably wrote the file again — re-authorizes it; a stamp that merely
  differs does not. Blocking outright silences the canonical agent loop — write,
  rewrite, rewrite again — which is the very thing rework measures. Both failure
  directions are pinned by tests; do not "simplify" the token comparison back into
  a boolean. **Durability's tombstone holds the identical rule** (its own
  `durabilitySeedAuthorized`), and the two halves that took a second pass are worth
  not re-deriving: refreshing the mark on every BLOCKED attempt made live AI work
  the thing that kept a path buried — a file the agent kept editing went unreported
  for good after its first `durable` harvest — and a rename must carry the LATER of
  the source's spent stamp and the destination's own, since a mark holding only the
  source's is read against a different path's clock and the agent that renamed the
  file usually recorded the new name too. Durability's marks are pruned by EVIDENCE
  rather than by a TTL, and the pruner must read the SEED GATE'S OWN predicate —
  presence in the AI-paths ledger, shared as one `aiPathKnown` func value, never a
  second expression that merely looks compatible. Judging a mark by its write stamp
  is what re-opens the hole: a ledger written before per-path stamps carries its
  paths with a 0 stamp for the full 7-day TTL, so every tombstone on a deployed
  install is deleted while the gate is still consulting the path.
- **Its state is scoped to a BRANCH, and `reworkLedger.Branches` is what enforces
  that.** The clear on `scope == scopeDefault` alone is not a bound: `git switch
  -c` off a feature branch and a per-branch worktree never stand on the default
  branch at all.

- **Two ledgers with different keys lose data where they meet.** Rework state is
  per-ROOT and expires with its branch; the attributed-commits ledger is keyed by
  SHA ALONE and shared across roots. Each is right alone, so a root adopting a
  branch another worktree (or an earlier checkout) already attributed starts
  empty and has every rebuilding commit skipped — the branch reads as holding no
  AI work. `replayReworkForAdoptedCommit` rebuilds that state silently, and it
  runs ONLY while the root still OWES the rebuild (`reworkLedger.Adopting`, set by
  adoption and cleared the poll the root's cursor reaches its head): cursor
  recovery re-surfaces skipped commits with no branch change, and re-folding one
  there re-applies its insertion hunk and slides live spans out of the file's real
  line space. That obligation is ON DISK because the range it covers is not
  bounded by one poll — `pollGitWatch`'s shared budget defers a whole root and its
  burst clamp splits a branch across polls, and every poll after the first sees an
  unchanged branch. A per-poll flag therefore expired mid-range, which lost the
  deferred branch outright and left a clamped one holding spans positioned
  mid-branch: the next live commit remapped those into a verdict about lines
  nobody wrote. `pollGitWatch`'s second return is what says a root drained.

- **A NEW ROOT is a third way into that same loss, and it is one-shot.** A fresh
  `git worktree add` is a new `gitWatchRootKey`, so `pollGitWatch` cold-starts it:
  the cursor goes straight to head and the branch's commits are never surfaced,
  while adoption fires anyway and immediately finishes a rebuild that rebuilt
  nothing. `replayReworkForColdStartBranch` folds `default..head` for state,
  gated on this device ALREADY holding attribution for a commit in the range —
  that gate, not the cold-start cursor, is what keeps a fresh install from
  importing history it never measured. It takes the range WHOLE or not at all
  (`gitBranchCommitsSinceDefault`): unlike `clampCommitBurst`, there is no later
  poll to drain a remainder, so a partial replay starts mid-branch and both cut
  ends FABRICATE — the older leaves spans a commit behind head, the newer makes
  the first replayed commit look like a first touch and seeds a human's added
  lines on an AI-touched path as AI (measured, not predicted).

  Three things decide WHERE the rebuilt spans land, and each was a live
  mispositioning before it was pinned. **The range is resolved against the head
  SHA `pollGitWatch` baselined the cursor to** — `pollGitWatch`'s third return
  carries that SHA, not a bool — because re-reading `HEAD` a whole poll later lets
  a commit made in between be folded here AND detected as new next poll, folding
  its hunks twice. **`--first-parent`**, because `gitCommitRawDiff` reads every
  commit with `-m --first-parent`, so a merge's diff already carries the
  second-parent side; folding that side's own commits too applies the same hunks
  twice, in a sibling branch's coordinates. **`--topo-order`**, because rev-list's
  default is reverse COMMIT-DATE order, which git does not promise is topological:
  one skewed committer clock sorts a parent past its own descendants and the
  reversed fold applies it after its child. **And the caller gates on adoption
  having fired THIS poll (`justAdopted`), never on the persisted `Adopting`
  obligation** — that obligation deliberately outlives its poll, so a root whose
  cursor is lost while its ledger survives would otherwise replay the whole range
  over spans it already tracks. The same commit's hunks are never folded twice
  into the same ledger.

- **ATTRIBUTION AND THE REWORK FOLD TAKE DELIBERATELY DIFFERENT RANGES, and
  `gitNewCommits`' second return is that split.** `gitCommitRawDiff` reads every
  commit with `-m --first-parent`, so a merge's diff already carries the
  second-parent side; the live range (`rev-list <cursor>..HEAD`) returns the merge
  AND that side's own commits, so folding both applied each merged-in edit twice —
  once in a sibling's coordinate space, once in the merge's — and slid tracked
  spans by an offset already applied. So the fold walks the FIRST-PARENT chain
  (`gitFirstParentSet`) while EMISSION keeps the full range: a commit reachable
  only through a second parent is still attributed, because whether merged-in work
  counts as AI-written is a product question that was deliberately not answered
  here. Where the ranges disagree the ledger can only LOSE state — such a commit
  no longer seeds its own ranges, and a replay backed solely by one is declined
  by the attributed-in-range gate — never invent it.

  **"Not on the chain" and "the probe could not run" are DIFFERENT FACTS, and
  collapsing them into one empty set INFLATES.** An earlier revision of this file
  claimed a failed `rev-list` returning an empty set was safe because folding
  nothing under-reports. That was wrong, and writing it down was worse than the
  bug, because it is what would stop the next reader checking: an empty subset
  leaves every commit unfoldable while `pollGitWatchWorkspace` still attributes
  each one and `recordAttributedCommits` writes them all down, so the tracked
  spans keep coordinates the unfolded hunks have already moved and the next
  rewrite emits a verdict over lines nobody wrote. `commitsWithFoldableChain` is
  the one door both values come through, and a failed probe returns `ok=false`
  from `gitNewCommits` — cursor untouched, attribution and fold both retried next
  poll. It also takes ONE revision, never rev-list flags, so the probe cannot be
  bounded differently from the list it describes: the gc'd-cursor recovery path
  bounds its own list with `-n` and probes unbounded head, because a chain commit
  falling inside the all-parents window and outside a `-n`-bounded chain window
  reads as "not on the chain" and is the same collapse by another route.

- **THE TWO LEDGERS DISAGREE WITH EACH OTHER ON A HISTORY CONTAINING MERGES.**
  This is a known, tracked gap, not intended behaviour:

  - the REWORK ledger folds the first-parent chain, so a merge's edits are
    counted once;
  - the DURABILITY ledger still folds the FULL range (`pollDurability` takes
    `gitNewCommits`' first return and discards the foldable subset), so
    surviving-line figures still double-count on a merge — the same hunks
    applied once in the merged-away branch's coordinate space and again in the
    merge's;
  - so the two ledgers' numbers are not reconcilable on such a history, and
    neither is authoritative over the other there.

  It is filed as its own task (`cli-durability-ledger-double-counts-merg-ca`)
  rather than fixed alongside the rework narrowing, because `pollDurabilityCommit`
  advances the durability cursor per commit inside its own ledger transaction, so
  skipping commits needs a deliberate answer for where that cursor lands.

Rename handling has an extra trap rework does not share with durability: a pure
rename produces no `@@` hunks, so `commitAttributionFromDiff` reports `ok=false`
and the commit would never reach `pollReworkCommit` at all. It returns the raw
diff on that path specifically so renames still land.

**Known gap, unfixed: AI-path evidence is keyed PER WORKTREE, so a second
worktree reconciles the same commit as `unknown`.** `resolveLedgerScope` looks
evidence up under `rel(taskRoot, root)/<path>`, but capture recorded it under the
path of whichever worktree the agent actually edited in. Verified on one commit:
`likely_ai` from the original checkout, `unknown` from a `git worktree add` copy
of the same branch. This bounds every cross-worktree recovery path above — they
replay the right commits and find no AI ranges to seed — and it is upstream of
rework, so it affects `commit_attribution` itself.
`TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree` does not catch it:
it simulates the second worktree inside ONE directory.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
