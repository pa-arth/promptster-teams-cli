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
