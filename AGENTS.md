# promptster-teams-cli

Go CLI that captures how engineers work with AI coding agents, for the Promptster **teams**
product. It writes no `settings.json`, injects nothing into any editor, and **never egresses
source code**.

The invariants below are the ones that change what you do in almost any session. Each section
names the doc holding the full reasoning — those docs record *why*, including options that
were evaluated and rejected on evidence. **Read the relevant one before changing that
subsystem**; several facts there are counterintuitive and were gotten wrong by reasoning from
documentation instead of measuring.

| Doc | Covers |
|---|---|
| [`docs/capture-surfaces.md`](docs/capture-surfaces.md) | The three watchers, Cursor's two rails, cwd gating, 28-day history replay |
| [`docs/self-update.md`](docs/self-update.md) | Update dispatch, npm packaging, autostart, version resolution, local testing |
| [`docs/uninstall.md`](docs/uninstall.md) | Why `uninstall` must exist and what it reverses |
| [`docs/durability-ledger.md`](docs/durability-ledger.md) | Attribution ledgers, tombstones, branch scoping |

## Read this before answering any "is X captured / why is Y missing" question

**The daemon is a different binary from the one you are typing to.** This is the single most
expensive confusion in this repo's support history.

`doctor`, `status`, `version` and every nudge describe the **foreground process**. Capture is a
**separate long-lived process** that may have started months ago from a different path, and the
two are routinely different builds — self-update re-execs, autostart bakes an absolute path, a
running process holds its inode after its file is deleted, and `npx` makes the foreground copy
the newest thing on the machine by definition.

Every one of those machines LOOKS healthy — capture running, doctor green, ingest reachable —
while features shipped weeks ago are silently absent. **Anything that installs itself at
`watch` startup (the Cursor hook rail most of all) never runs on a daemon that never
restarted.** A recorded version is proof; an absent one is not evidence of health. Never infer
the daemon's build from the foreground version — that inference IS the bug.

## Capture

Transcript tailing for all three tools, plus one hook rail for Cursor alone. Three watchers
poll the filesystem every 3s:

- **Claude Code** — `internal/capture/cmd_claude_watch.go`, tails `~/.claude/projects/…`
- **Codex** — `internal/capture/cmd_codex_watch.go`, tails `~/.codex/sessions/…`
- **Cursor** — `internal/capture/cmd_cursor_watch.go`, tails `~/.cursor/projects/…`

**The real gate is cwd, not surface.** A transcript is ingested only if its recorded cwd sits
inside the capture workspace or a registered git worktree. Every Claude Code surface (terminal,
desktop app, VS Code, JetBrains) writes to the same tree, so capture is surface-agnostic and
nothing in this repo greps for "vscode" or "ide". When triaging "why didn't X get captured",
**check cwd before suspecting the surface.**

**Cursor is the only tool with two rails** (a user-scope hook at `~/.cursor/hooks.json` as
primary, transcript as fallback) because its transcript is the only one genuinely too thin —
no model, no cwd, no timestamp, no tokens. Never write the project-local
`<workspace>/.cursor/hooks.json`: it is a tracked file inside the customer's repo, and it
enrolls every teammate who pulls it.

Four facts settled empirically — **do not re-derive them from documentation**:

- **Cursor `duration` is fractional MILLISECONDS.** Read as seconds, a 2-second `go build`
  reported as 33 minutes. Nothing downstream can tell a duration is implausible.
- **We do not capture Cursor token counts.** Token fields stay **ABSENT, never zero**.
  `model_id: "default"` is a routing sentinel and is rejected — omit the field.
- **Claude + Codex replay 28 days of history through the LIVE funnel.** Parts of that funnel
  assumed "this just happened"; the attribution ledgers must be stamped from the EVENT, never
  the wall clock. Cursor never backfills and passes a hard `false`.
- **A progress-schema bump is a FLEET-WIDE replay event, and its cost is DECLARED, not
  implied.** Bumping `claudeProgressSchemaV`/`codexProgressSchemaV` clears `Offsets`, so every
  in-window transcript on every device is re-read from zero — one line of diff, the whole
  28-day window back through the funnel. v0.12.3's v2 bump replayed **62,302 events on one
  device** and left a 20,761-event backlog whose oldest entry was 15 days stale. It fires on
  the first daemon start after the upgrade, **not** on restart: `clearCodexWatcherState`
  removes only `codex-watcher.json` (pid, heartbeat) and never the progress file holding the
  offsets, so **hardening restart fixes nothing — an UPGRADE across a bump is what rescans.**
  The cost lands on every device at once, days later, and never on whoever bumped it. Declare
  the window, the rough per-device event count, and why it is worth a replay. Read exposure
  from `engineer_keys.latest_cli_version` — never infer it from release timing.

### Counts, never code

**This is the constraint the whole product operates under.** Exactly two functions read
`old_string`/`new_string` — one per rail — and **both return only integers**. No patch, no file
body, no diff, ever, including in `RawPayload` and in every error and log path. The hook rail
additionally drops `user_email`, `tool_output` and `error_message`.

Four mutation-tested tests pin this. `outcome_events` has a DB CHECK rejecting patches and file
bodies, but **the CLI must not rely on that.**

**Do NOT read `~/.cursor/chats/<hash>/<uuid>/store.db`.** It would cheaply solve the transcript
rail's cwd and timestamp gaps, and it holds full unified diffs, file contents and raw stdout.
The neighbouring `meta.json` is safe; the sibling is a loaded gun.

## Durability ledger

The authoritative invariants live in `internal/capture/durability.go`'s header comment — read
it before changing anything in the ledger.

The framing to carry in: this subsystem's worst failure mode is **FABRICATION, not loss**.
Losing a tracked line understates AI impact; inventing one — attributing human code as AI, or
emitting `durable` for lines that did not survive — corrupts the only number the product exists
to report. Every tradeoff resolves toward a conservative undercount. **A change that trades an
undercount for any kind of invention is wrong even when it improves coverage.**

`rework.go` is a SECOND ledger holding the SAME invariants, and both known fabrication classes
were reachable through both. Fix them together or the fix is half a fix.

## Install & update

- **`uninstall` is the ONLY uninstall path that exists, and it has to be.** `npm rm -g` runs no
  uninstall lifecycle script and leaves files postinstall created outside `node_modules` — so
  it reports success while capture keeps running and returns at the next login.
- **The binary that runs is never the copy npm is tracking.** postinstall copies it to
  `~/.promptster-teams/bin/`, which is what keeps `npm ls` honest by construction.
- **postinstall must never fail an install and must never downgrade.** A non-zero exit aborts
  `npm i -g` and leaves the engineer with no CLI at all.
- **npm is not a reliable place to hang install work** — newer npm gates install scripts behind
  an approval and reports a completely successful install having run none of ours. Both
  `scripts/postinstall.js` and `bin/promptster-teams.js` call the same converge path.
- **`autostart` bakes an ABSOLUTE path.** Moving the binary is a migration, and it fails
  silently: the running daemon holds its inode, then at the next login launchd runs a path
  that is gone.
- **Global installs only.** A project-local install is pinned by its lockfile and is left alone.

## Gotchas when testing locally

- **Local builds never update** — the gate skips on version `"dev"` or `""`. Build with
  `-ldflags "-X …/internal/version.Version=0.0.1"`.
- **Isolate state or the single-instance lock refuses.** Set `HOME` and `PROMPTSTER_STATE_DIR`
  to throwaway dirs. Never kill the developer's real capture process to free the lock.
- **Killing the npm shim does NOT kill the daemon** — it's a node wrapper that `spawnSync`s the
  Go binary. Kill the Go pid (`pgrep -f 'promptster-teams.*watch'`) or run `stop`, and check for
  strays, filtered by the scratchpad path.
- **macOS autostart cannot be tested by sandboxing `HOME`** — `launchctl bootout/bootstrap`
  target `gui/$UID` by LABEL, so a sandboxed HOME still tears down the developer's real
  `ai.promptster.teams` job. Same trap applies to the uninstall tests, which is why they fake
  only autostart + daemon-stop.
- **macOS has no `timeout(1)`.** A `timeout … | grep` pipeline fails and silently looks like
  "the feature didn't fire".
- **A fake key is enough** to get past `watch`'s early exit: `PSE-` + six 4-char groups, base32
  alphabet, no `I`/`O`/`0`/`1`.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When a subsystem's reasoning grows past a few lines, it belongs in its `docs/` page — this file
holds the invariant and the pointer, not the derivation.
