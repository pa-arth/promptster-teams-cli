# Capture surfaces

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

## Cursor runs TWO rails: a hook (primary) and the transcript (fallback)

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
re-execing. Nobody reinstalls anything.

**We clobbered engineers' configs for months, and this paragraph used to say we
never did.** The claim rested on two real guards — `loadCursorHookConfig`
preserves top-level keys it does not model, and refuses to overwrite a
hooks.json that does not parse — neither of which is about the thing that broke:

- **Cursor's validation is all-or-nothing at FILE granularity.** One entry it
  rejects and it discards the whole config, every step, every tool's hooks. The
  only trace is
  `~/Library/Application Support/Cursor/logs/*/window*/output_*/cursor.hooks.*.log`,
  which nobody reads. Ours going quiet looks identical to Cursor being idle.
- **A hook entry MAY omit `type`** (it defaults to `command`), so
  `{"command": "…"}` is legal and common. `cursorHookCmd` modelled `type` as a
  plain string with no omitempty, so reading a legal type-less neighbour entry
  and writing the file back produced `"type": ""` — invalid, whole file rejected.
  Measured on one machine: neighbour entry legal on Aug 18 16:36, our watcher
  round-tripped it, `Invalid user config: sessionStart[1]` from Aug 20 21:41
  onward, **17 hooks across four tools dead for two days.**
- **The same struct silently deleted `timeout`, `matcher`, `loop_limit` and
  `failClosed`** from neighbours' entries on every write, because it marshalled
  only the four keys it modelled. `matcher` decides which files their hook fires
  on; `failClosed` decides whether its failure blocks their agent.

What holds now, all pinned in `cursor_hooks_validate_test.go`:

1. **Entries round-trip verbatim.** `cursorHookCmd` keeps the raw keys and
   marshals from them; typed fields are decode-side conveniences only.
2. **`saveCursorHookConfig` refuses to write a config Cursor would reject.** The
   gate is on the WRITE side because a read-side check could not have caught
   this — the file we read was fine; our serialisation is what broke it.
3. **Pre-existing damage is repaired** at `watch` startup, but only by deleting a
   meaningless `type` key so Cursor's documented default applies. We never delete
   a neighbour's entry: that would be right only while our validator is right,
   and down-and-loud beats quietly destructive. Anything else is reported by
   `doctor`/`status` and left alone.
4. **The validator is one-directional.** It answers "can I prove Cursor rejects
   this?", never "do I recognise this?" — an unfamiliar hook type is `unknown`,
   not invalid, so the vendor shipping a third kind makes us quiet rather than
   wrong. `CursorHooksDoctor` says how many entries it could not judge instead of
   implying it checked everything.
5. **Repairs leave a record** (`~/.promptster-teams/cursor-hook-repairs.json`,
   plus an untouched pre-repair copy), because a successful repair is otherwise
   invisible — the same silence the defect was made of.

The general shape, worth carrying to any shared config file: **a validator that
only reads is blind to a defect its own writer creates**, and **whole-file
rejection means being a good neighbour is a correctness property, not manners.**

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
them from documentation. The second one was settled WRONG for two weeks, and its
correction is the reusable lesson on this page:**

- **`duration` is fractional MILLISECONDS.** Read as seconds, a 2-second
  `go build ./...` (`duration: 2021.129`) reported as **33 minutes**. Nothing
  downstream can tell that a duration is implausible.
  `TestCursorHookCommandDurationIsMilliseconds` pins it.
- **Cursor token counts ARE captured, via `stop` — and the two years of "it
  exposes none" is the part worth keeping.** Transcripts, `state.vscdb`,
  `conversation-search.db` and per-session `chats/*/store.db` genuinely expose
  none; that half was right and stands. The hook half was wrong. A 2026-08-03
  probe enrolled our own logger, saw no `stop` payloads, and concluded the vendor
  declined us — but Cursor's dispatcher **early-returns on any step nobody
  registered** (`hasHookForStep`, `workbench.desktop.main.js` @25399531, 3.12.17),
  and `stop` was not in `cursorHookSteps`. The probe measured our configuration
  and filed the result under Cursor's capabilities. **A probe that never ran and
  a vendor that sends nothing produce byte-identical evidence**; the 2026-08-18
  re-probe added `beforeSubmitPrompt` as a POSITIVE CONTROL to tell them apart,
  and `stop` then delivered all four counts per generation.

  What that buys and what it costs:
  - One `ai_response` **per generation**, `usageScope: "request"`, carrying
    `input/output/cacheRead/cacheWrite`. Per-turn, not cumulative — output FELL
    902 → 525 across consecutive generations, which no running total can do. The
    scope tag is mandatory: absent, the backend differences the row against a
    running maximum and drops the first turn as a baseline.
  - An **aborted** turn arrives with the token keys ABSENT, not zero, and they
    stay absent. There is deliberately no `status == "completed"` filter — an
    unenumerated status that does bill is then kept for free.
  - `stop` reports `model`/`model_id: "default"`, so the model is joined **on
    device by `generation_id`** from `afterAgentThought` (`cursor_generations.go`,
    bounded by count and age). No entry ⇒ the row ships with tokens and **no
    model**, which the backend declines to price. It is never defaulted and never
    inherited from another turn: Cursor auto-routes and switches models
    mid-conversation.
  - `afterAgentResponse` stays **unregistered**. It reads the same
    `turnTokenUsage` object, so registering both double-counts every generation,
    and it carries the assistant's full message where `stop` carries none.
  - `stop` is a **GATING** step: Cursor submits a `followup_message` found on the
    handler's stdout as a new chat turn. `RunCursorHook` therefore prints a
    compile-time constant and serialises no part of the payload.

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
that appears on a later poll is tailed from 0. This remains Cursor-only behavior:
Claude and Codex carry timestamps and safely backfill a bounded 28-day window.
Both Cursor directions matter: seed everything and every new session loses its
opening prompt (a transcript only becomes classifiable at its first tool call,
several records past the prompt); tail everything from 0 and the first daemon run
re-uploads months of unbounded history.

## Cursor egresses counts, never code

### Cursor vendor-usage credential and egress disclosure

On macOS, the background watcher also measures the engineer's current Cursor
billing period every 15 minutes when the organization policy explicitly permits
`cursorVendorUsage`. It creates a private copy-on-write temporary clone of
`~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` (falling
back to a byte copy), reads only `cursorAuth/accessToken` and
`cursorAuth/refreshToken` from that clone, then deletes it. It never opens the
live database and never reads neighbouring keys.

The credential is used only as an Authorization header for three operations at
`api2.cursor.sh`: `GetTeams` (to refuse individual-account collection for Teams
accounts), `GetCurrentPeriodUsage`, and paginated `GetFilteredUsageEvents`. The
credential never enters an event, log, error, request body, or Promptster
request. This is the CLI's first outbound host other than Promptster and GitHub,
and the endpoint is undocumented; the customer's acceptance of possible Cursor
agreement consequences is contractual risk that disabling the collector cannot
undo. Operationally, the policy kill switch stops calls within the 15-minute
cache TTL.

This disclosure is repeated during enrollment and retained here so it remains
available after the enrollment output is gone. It describes the operations our
code performs; it does not claim the credential itself is read-only.

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

## What this means per surface

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

## The real gate is cwd, not surface

`classifyClaudeTranscript` (`cmd_claude_watch.go`) ingests a transcript only
if its recorded `cwd` sits inside the capture workspace or one of its registered
git worktrees (`workspaceMatchRoots`, same file). Codex applies the same test
(`classifyCodexRollout`, `cmd_codex_watch.go`), and Cursor applies it to observed
paths instead of a recorded cwd (`cursorClassify`, `cmd_cursor_watch.go`). The
workspace defaults to `os.Getwd()` and is overridable with
`PROMPTSTER_TEAMS_WATCH_DIR` (`watchDirFromEnv`, `teams.go`).

The Claude and Codex decisions are CACHED per file in the progress file, and that
cache is keyed to the root set that produced them (`captureRootsFingerprint` /
`syncMatchCacheToRoots`, `capture_roots.go`): any change to the effective roots
drops every cached decision, in both directions — widening admits a prior
mismatch, narrowing revokes a prior match so a removed workspace stops uploading.
Byte offsets live in a separate map and survive, so revalidation never re-uploads
consumed bytes.

So a session is dropped when it runs outside the watched workspace — e.g. the
desktop app opened on a different folder — no matter which surface produced it.
When triaging "why didn't X get captured", check cwd before suspecting the
surface. Transcripts carry no surface marker at all, so capture could not
distinguish CLI from IDE even if it wanted to.

## Claude + Codex replay 28 days of history through the LIVE funnel

`transcriptHistoryCutoff` (`session.go`) bounds it, both watch loops recompute it
**every poll** (frozen at boot it is an absolute date a long-lived daemon drifts
away from), and a progress-schema bump (v2) grants the replay exactly once.
Cursor is excluded — no trustworthy timestamp.

The hazard is not the reading, it is that replayed events go through
`queueClaudeWatchEvent` / `emitCodexEvent`, the same funnel live events use — and
parts of that funnel assumed "this just happened". Five rules, all pinned by
`history_backfill_test.go` and all mutation-tested:

- **The attribution ledgers are stamped from the EVENT, never the wall clock.**
  `recordAiTouchedPathAt` takes the stamp; `dedupeFileDiff` passes `e.Ts`. Stamp
  `time.Now()` instead and a file an agent last touched 20 days ago re-enters the
  ai-paths ledger as touched TODAY — the git watcher then tags the next commit's
  purely human lines `likely_ai` and `aiPathKnown` seeds them as AI durability
  spans. That is the fabrication class the durability ledger exists to refuse.
  A replayed write also must not win the per-path collision bump: the bump
  distinguishes two LIVE writes in one millisecond, and letting history take it
  manufactures the "the agent wrote this again" evidence `durabilitySeedAuthorized`
  is checking for.
- **A replayed `file_diff` skips the dedup claim entirely.** The claim key is the
  file's CURRENT content hash, so keeping it collapses every replayed edit of one
  path onto today's bytes: first wins, rest vanish, and the claim then blocks a
  genuine live edit for the next 5 minutes.
- **`replay` is a PRODUCER signal, never inferred inside the shared funnel.**
  `dedupeFileDiff` and `recordAiBashWindow` take it as an argument. Only Claude
  and Codex pass `transcriptEventIsHistorical` (age vs `diffDedupTTL`), because
  age means age only where every record carries its OWN timestamp. **Cursor
  passes a hard `false` on both rails** — it never backfills, and
  `CursorTranscriptProcessor.eventTs` stamps every action in a turn with that
  TURN'S START anchor, so a 46-minute turn's live edits look 46 minutes old.
  Inferring from age there silently disabled live cross-channel dedupe and froze
  the per-path stamp `durabilitySeedAuthorized` reads as "the agent wrote this
  again". A live observation is stamped NOW whatever the transcript claims; only
  a replay is stamped from `e.Ts`.
- **The age gate fails CLOSED.** An absent or unparseable transcript timestamp
  routes to `…YesPreexisting` (seed to EOF), because mtime is the only other
  bound and a six-month-old session resumed today has today's mtime.
- **A poll's reads are BUDGETED** (`claudeWatchMaxBytesPerPoll` /
  `codexWatchMaxBytesPerPoll`, 8 MiB, the byte analogue of
  `gitWatchMaxCommitsPerPollTotal`), progress is saved per file, and the budget
  is zeroed outright while `outbox.UnderPressure()`. Deferring costs nothing —
  the transcript IS the durable buffer and the offset has not moved — whereas an
  unbounded first pass blocks every shutdown for its full duration (the signal
  select sits after the poll) and races the outbox to `OutboxMaxBytes`, where
  `Append` DROPS and takes live capture down with the backfill. The budget is a
  RECORD boundary, not a byte one: both rails read through
  `readTranscriptRecords` (`transcript_read.go`), which defers a record that
  does not fit rather than half-reading it. The one read allowed past the budget
  is a single per-poll probe establishing that a record exceeds
  `transcriptMaxRecordBytes`, which is the only case a record is discarded
  instead of deferred — read that file's header before touching either, and note
  that classification measures against the SAME maximum so the two paths cannot
  disagree about which record is unsupported.

## A progress-schema bump is a FLEET-WIDE replay event, and its cost is declared

`claudeProgressSchemaV` / `codexProgressSchemaV` gate a one-time migration in
`loadClaudeWatchProgress` / `loadCodexWatchProgress`. Bumping either one clears
`Offsets`, so **every in-window transcript on every device is read again from
byte zero.** One line of diff; the entire 28-day window re-enters the funnel.

**Measured, v0.12.3's v2 bump, 2026-08-04:**

| device | CLI | replayed |
|---|---|---|
| alex@ops.ai | 0.12.3 | **62,302 events**, ≈10.4h of replay |
| ajamdagneya | 0.12.3 | 1,312 events |
| paarthguardian | 0.12.2 | **114,041 events** queued, ≈19h — had not fired yet |

The delivery backlog that produced reached **20,761 events with its oldest 15
days stale**, draining at ~74/min against a per-minute ingest budget. Nothing was
lost — the outbox is durable and every event eventually landed — but the fleet
spent days reporting stale numbers, and the backlog was deep enough to saturate
ingest and start costing the presence beat its own delivery.

Three facts that make this easy to get wrong:

- **It fires on the first daemon start of the UPGRADED binary — which in practice
  IS a restart. Do not read this as "restarts are innocent."** Self-update
  re-execs, so on a real device the restart and the upgrade are the same event,
  and it is the moment to look at when a fleet suddenly replays.

  The distinction that matters is *which* restart. A restart on the **same**
  schema version rescans nothing — `clearCodexWatcherState` removes only
  `codex-watcher.json` (pid, heartbeat) and never `codex-watcher-progress.json`,
  so the offsets survive it. What clears them is the **new binary's** progress
  loader running its `p.V < N` migration. The restart is the occasion; the
  version bump is the cause.

  That is why the question this investigation opened with — "why does a RESTART
  rescan?" — leads somewhere useless. Hardening restart would have fixed nothing,
  because restart was never the variable. **Ask what changed between the two
  binaries, not what the restart did.**
- **The cost is not paid by whoever bumps it.** It lands on every enrolled device
  at once, days later, as ingest pressure — long after the PR that caused it is
  out of mind. The version transition is readable from
  `engineer_keys.latest_cli_version`; **read it rather than inferring exposure
  from release timing**, because a device that has not restarted has not fired.
- **The replay is usually the POINT, not a bug.** v2 deliberately reopened
  matched files so the new bounded-history policy got exactly one chance to
  import the last 28 days. Pulling such a release denies users that import.
  Ship the fix forward instead.

**So: declare the cost in the PR that bumps the version.** Name (a) the window
being re-read, (b) a rough events-per-device figure from the table above, and (c)
what makes it worth a fleet-wide replay. A bump with no declared cost is
indistinguishable from an accident, which is exactly how this one was diagnosed —
after the fact, from a backlog.
