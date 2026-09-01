# Changelog

All notable changes to `promptster-teams` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and the project
follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.26.0] — 2026-09-01

### Fixed

- Self-update could never install anything. The consent prompt required an
  interactive terminal, but the update check only ever runs inside the watch
  daemon, which `start` detaches and `autostart` runs under launchd/systemd —
  so it declined itself on every cycle, and the declined outcome printed
  nothing anywhere. Machines stayed on the version they were installed at
  while `doctor` reported auto-update healthy.
- An organization's `autoUpdate: false` could be silently undone. The policy
  cache is discarded on any read or parse error, after which the switch
  reverts to its enabled default, so a single corrupt or deleted state file
  re-enabled self-update on a fleet that had turned it off. The organization's
  stated intent is now mirrored to its own durable file, which fills gaps in
  the cache but never overrides it.
- `login --key ... < /dev/null` in a provisioning script reported an
  interactive terminal and prompted into end-of-input, because the terminal
  check treated `/dev/null` as a character device.

### Added

- Added `promptster-teams update`: install a newer signed release on demand,
  with `--check` to report what one would do, `--yes` for unattended runs, and
  `--ask-each` / `--enable-auto` / `--disable-auto` to set how this machine
  handles updates.
- `login` and `start` now ask once how updates should be handled and remember
  the answer: ask about each release (default), update automatically, or never.
  Organizations that set an update policy decide for their own fleet, and
  individual engineers are not asked.
- When set to ask, a notification names the new version and links its release
  notes, so a machine with no terminal attached can still get an answer. It
  appears at most once per release. Verified on macOS; Windows and Linux
  support is implemented but not yet verified, and a notification that cannot
  be shown never counts as a refusal.
- Every outcome where an update exists but is not installed now says so, on
  `doctor` and at watch startup.

### Changed

- Skill-use capture is now consistent across tools: direct Codex and Cursor
  `SKILL.md` reads emit canonical skill-use events, nested Codex system-skill
  paths are recognized, search commands no longer fabricate activations, and
  cache-only plugin skills no longer contribute listing tax at capture time.
  Cursor reads discarded by older clients cannot be reconstructed
  historically; new captures are accurate from this release onward.

## [0.25.0] — 2026-09-01

### Added

- Added metadata-only discovery for additional OS user homes containing Claude,
  Codex, or Cursor state. `promptster-teams discover` gives user-scoped setup
  instructions, and login reports additional environments without opening
  their transcripts, configuration, databases, or credentials.
- Capture identity is now installation-scoped instead of machine-scoped. Each
  home keeps an independent anonymous identity, heartbeat, signing chain, queue,
  and progress state, giving fleet health a stable key with which to distinguish
  two homes on the same physical machine.

## [0.24.0] — 2026-08-27

### Added

- Added the opt-in Cursor vendor usage collector for individual Pro accounts.
  It reads the current credential from Cursor's local macOS store each cycle,
  calls only the three disclosed Cursor dashboard methods, and publishes
  current-billing-period usage, quota, and shape-health snapshots.
- Added fail-closed policy enforcement, explicit absence reasons, mutable
  snapshot replacement, hook-rail cost deduplication, and credential-leak
  guards for the new collector.

## [0.23.0] — 2026-08-27

### Added

**Codex compactions read as never happening — and a candidate was docked for
it.** `SOURCE_CAPABILITIES.codex.resetEvents` was false, on the recorded grounds
that Codex has "NO /clear or /compact concept". That was never true. codex-cli
ships `/compact`, `/clear` and `/new`, its hook matrix carries pre-compact and
post-compact, and the ROLLOUT FILE — the rail this normalizer already reads line
by line — writes a `type:"compacted"` record plus an `event_msg` of
`"context_compacted"` on every compaction. We were parsing the file that held
the answer and skipping the two lines that were it. The cost was not abstract: a
hiring candidate came back `context_hygiene [developing] — zero clears or
compactions despite prolonged discovery, implementation, and verification
phases`, docked for a ritual our capture could not have observed.

- **Keyed off the marker, not the record.** `event_msg`/`context_compacted` and
  `type:"compacted"` arrive 1:1 — 8 of each across the 5 local rollouts that
  compact, never one without the other — so either would count the same resets.
  The marker wins twice over. Its sibling's payload is the entire
  `replacement_history`: every prior user turn verbatim, base64 image data
  included, the single largest blob in the file and exactly what the teams
  projection exists never to carry. And `context_compacted` is an `EventMsg`
  VARIANT, so it survives the 0.149 stream rename that moved user and agent
  messages under `item_completed` — checked against the installed 0.149.1
  binary's variant table.
- **Nothing is invented on the way out.** No `trigger`: the Claude path reads
  auto-vs-manual off `compactMetadata`, the codex rollout records nothing
  equivalent, and it does not log slash commands at all (0 slash-prefixed user
  turns across 41 local rollouts). No token counts: `total_token_usage` is a
  running SESSION total, not the live context size — the same reason the
  ai_response path leaves `usageScope` unset — so it cannot say how big the
  context was before the reset. Manufacturing either would be the same class of
  mistake as the false `developing` this undoes.
- A delegated thread's compaction is the subagent's context wall, not the
  human's, and is dropped like its prompts and answers.
- **No wire widening.** `context_compact` was already allowlisted on both sides,
  projecting `summary` and `trigger`.
- **`resetEvents` stays false, deliberately.** That map is static per
  source-family with no capture-version awareness, so flipping it now would
  claim the capability for every codex session already captured by an older
  build. It waits until the fleet is on this one.

Verified by replaying all 41 local rollouts through the normalizer: 6
`context_compact` emitted, and the 2 not emitted are both in one subagent
thread — 8 total, every compaction accounted for.

### Fixed

**We wrapped their statusline and then replaced it anyway.** The shim runs the
engineer's own `statusLine` command and passes its stdout through — that is what
makes taking the slot defensible. But it bounded that command at **2500ms** and,
on overrun, drew OUR line (`promptster · 5h x% · wk y%`) in its place. 2.5s is
below what a real statusline costs: claude-hud (node, a plugin-cache glob, a
user extra-cmd subprocess) measured **0.6s–4.5s over eight consecutive runs on
an idle laptop and exceeded the bound on five of them**. The engineer's
statusline was being replaced by ours on most ticks, and the failure was
self-concealing — the line that would have said so is the line that got
replaced.

- **Our line is never the fallback for a command we wrapped.** The ladder is now
  last-good output → this run's partial stdout → nothing at all. Complete-but-
  one-tick-stale beats fresh-but-truncated (a killed command's stdout can end
  mid-escape-sequence and bleed color), and a blank tick is something the
  engineer can attribute to their own script — ours is a takeover. The compact
  promptster line survives for the one case it was ever for: an empty slot.
- **The last-good render is cached** (`statusline-lastgood`, 0600, 64KB cap) so
  a slow tick redraws THEIR line. It holds rendered third-party content, is read
  by exactly one function, and never reaches the spools or the wire. It is
  dropped on re-wrap and on `statusline disable` / `uninstall`.
- **The timeout now actually bounds the render.** stdout is a buffer, so
  `os/exec` plumbs it through a pipe and `Wait` blocks until every writer closes
  it — and a killed `sh` leaves GRANDCHILDREN holding it open. claude-hud is
  exactly that shape. A 100ms context took **30s** to return; `cmd.WaitDelay`
  makes it 0.6s.
- **The bound is now 10s**, a wedge guard rather than a latency budget.
  Unwrapped, Claude Code runs that same command with no timeout of ours, so any
  value a real statusline can reach makes us the cause of a regression the
  engineer would not otherwise have had.

**A statusline another tool evicts stays evicted.** `statusLine` is one key in
one file and every tool writes it directly — claude-hud's `/claude-hud:setup`
sets `statusLine.command` to its own command, full stop. Re-running it removed
our shim and killed every Claude window and context-window reading on that
machine until the next `login`. The eviction is invisible from the engineer's
side: their statusline looks FINE, because it is the other tool's, rendering
normally. The only symptom is data that stops arriving on someone else's
dashboard days later.

- **The watcher re-wraps a displaced shim** on a 5-minute check, wrapping
  whatever took the slot so that tool's line keeps rendering. In the poll loop
  and not at startup on purpose — a daemon that never restarts would never run a
  startup-only check, and this daemon is precisely the process that does not
  restart.
- **It will not touch four cases**, each a real one: no prior record (never
  enabled, or `statusline disable` cleared it — an off switch something reverses
  on a timer is not an off switch); an ABSENT `statusLine` (the engineer deleted
  the key and filling the hole invents config they removed); a managed-policy
  statusLine that outranks the user layer; and project layers, which are per-cwd
  and which the daemon has no cwd to judge from.
- **A fight is bounded.** If something else also rewrites the slot on a timer,
  we stop after five consecutive displacements rather than churn settings.json
  forever, and `doctor` says a human has to pick a winner. The counter resets
  whenever a check finds the shim in place, so an occasional eviction never
  accumulates toward that bound.
- Re-wrapping is only defensible because the wrap is now genuinely transparent.
  If that stops being true, this heal becomes a takeover on a timer and must be
  removed with it.

**Global state was keyed to a directory designed to stop being global.**
`~/.claude/settings.json` is machine-global and has no workspace concept, but
its wrap record, render cache and lock were keyed to `StateDir()` — whose
per-workspace branch is documented as a future mode waiting to be lit up. The
day it is, one `settings.json` gets two disagreeing records: `statusline
disable` in one workspace leaves the other workspace's daemon still believing it
is enabled and re-wrapping. A lock keyed to something narrower than what it
guards is not a lock. `state.GlobalStateDir()` draws the distinction now, with
identical behaviour today — which is why it had to be drawn before it was
reachable.

**One repo's statusline could render another repo's branch.** The last-good
render cache above arrived as a single machine-wide entry keyed only on the
command fingerprint, and two sessions in different repos run the same command
string — so a failed tick in one repo redrew the other repo's line. The cache is
now per session, in the shape already used for the context spools, swept by the
same pruner, with the old single file migrated.

**`claude watch` never released a transcript processor.** One
`*ClaudeTranscriptProcessor` accumulated per transcript ever seen and none was
ever freed, including after its transcript aged out of the rolling history
window — unbounded growth on a daemon meant to run for weeks across many
projects. Entries absent for two consecutive polls are now evicted, flushing any
pending accumulated message first; two polls rather than one so that a single
`filepath.Walk` hiccup cannot drop live state.

## [0.22.0] — 2026-08-26

### Added

**A lane could say WHICH invocation and WHAT KIND, and not what it was for.**
`agentId` names the invocation, `attributionAgent` names the type, and neither
separates concurrent delegates of the SAME type. Measured over 143 Claude
sidechains across 53 sessions: **68 of them (48%) sit in a cluster of
concurrent same-type lanes**, cluster sizes 2-7, and the cost spread inside one
cluster is p50 2.2x and max **11.3x**. "An Explore lane cost 11x another Explore
lane" is not actionable; naming which one is the point.

- **Claude Code** stamps the dispatch LABEL on `subagent_usage`, read from the
  `.meta.json` sidecar Claude writes beside every sidechain
  (`{"agentType","description","toolUseId","spawnDepth"}`, present on 143/143,
  20-50 chars, distinct in all 21 clusters). Read from the CHILD side on
  purpose: the parent knows the description but not which sidechain it produced
  — its Task call carries a `toolUseId`, not an agent id — so pairing them from
  the parent is a join with no key. The child's sidecar carries all three
  together, so there is nothing to join. Only `description` is read.
- **Codex** now recognises the `thread_spawn` arm. `SubAgentSource` is a
  five-arm union — review | compact | thread_spawn | memory_consolidation |
  other — and only `other` carries a bare string. A user-spawned delegate takes
  `thread_spawn`, whose value is an OBJECT, so the old single-string scan found
  nothing and **every user-spawned Codex delegate arrived with no attribution at
  all**. Silent: absent reads exactly like a rail that does not report one.
  `agent_role` becomes `attributionAgent`; `agent_nickname` ("Aristotle") is a
  per-invocation IDENTITY, not a type, so it becomes `summary` — putting it in
  the type field would make every lane its own delegate type and invert the
  32x-undercount axis. `agent_path` is deliberately unused: it is a path, the
  clamp exists to reject paths, and its basename would be a guess.
- **Cursor** gets nothing, and not by oversight: zero sidecars across 11
  sessions, child rows carry only `role`/`message`, **0 of 582 carry any
  timestamp**, and a child uuid appears in its parent in **1 of 40** cases.
  There is no child-side metadata to read and no key to join on.

**Not a widening of what leaves the machine.** `summary` is already allowlisted
on `task_dispatch`, and the CLI already emits that identical string there for
every dispatch. Both halves — the label and the money — already shipped and
could not be paired. This restamps a sanctioned field onto the event that
carries the spend, capped by the same helper at the same 100 bytes.

It is a dispatch LABEL, not sidechain prose. Claude sidechains stay
counters-only ("sidechain prose is agent-authored and must not leave the
machine"); the string is the parent's model-authored tool-call description.

The server half is promptster-backend#808 and **must be deployed first**: the
device projector default-denies, so a field the server does not allowlist is
dropped at ingest with a `201` and no error anywhere.

**Two honest limits.** Codex `agent_role` was null on both observed rollouts, so
the `attributionAgent` half of the Codex change is correct-by-construction and
UNOBSERVED; the `summary` half has a wire fixture behind it. And the fix is
forward-only — historical rows that arrived with no attribution stay that way.


**38% of Cursor turns produced no usage row, and nothing counted one.** Measured
2026-08-25 against Cursor's own session records on one live customer machine.
Every counter this rail had was incremented *after* the early return that
dropped the turn, so the one outcome worth measuring was the one outcome that
left no record — and a rail that drops work invisibly is indistinguishable from
a rail with nothing to do. It ran that way for weeks on a machine reporting
`cursorHooks: ok` throughout: the install state was observable, the coverage
was not.

Five cumulative counters now sit beside the existing model-coverage pair:
`stopSeen` (the denominator `usageRows` never had), `stopEmpty` (declined for no
tokens *and* no model), `unparsed`, `overruns`, and `usageRows`. `stopEmpty` is
an **honest** drop — an aborted turn the vendor told us nothing about — and
separating it is what turns "38% missing" into an answer rather than an alarm.
The residual `stopSeen - stopEmpty - usageRows` is left as arithmetic rather
than given its own counter that could disagree with its parts.

The overrun counter keeps **its own file and its own lock**, deliberately.
`RunCursorHook` abandons its worker at the budget without waiting, and the
worker may be running precisely because it is blocked in a `flock` on
`cursor-generations.json`; recording the overrun through that same lock would
park the parent on the very wedge it is reporting. The hook runs synchronously
inside the engineer's agent loop, so that cost is not theoretical.
`TestOverrunIsRecordedWhileTheGenerationsLockIsHeld` is a design assertion — it
fails the moment someone moves this counter in "so the counters live together".

### Fixed

**A moved agent rewrites the conversation, and we called it a new session.**
When a Cursor agent calls `cursor-app-control`'s `move_agent_to_root` or
`move_agent_to_cloned_root`, Cursor ends the transcript on a `turn_ended` error
and re-writes the ENTIRE conversation under a NEW uuid in a DIFFERENT project
directory. `cursorSessionIDFromPath` reads the id off the filename, so the
rewritten history arrived as a brand-new session at offset 0 and every prompt
and tool call the engineer had already made was ingested a second time.

Nothing downstream caught it and nothing downstream could: the teams ingest
index is `UNIQUE (ts, md5(org||session||kind||data))` with
`onConflictDoNothing`, so content-identical events collapse **only within one
`session_id`**. Unifying the session id is therefore the whole fix, and it
needs no backend change.

Measured on 142 real transcripts: 16 first-record-duplicate groups over 19
redundant files (**13.4% of the corpus**); 6 are true continuations, every one
crossing a project directory, one a chain of three. Simulated against the
index's identity fields, **61 of 61 predecessor events are absorbed** under a
unified id.

Adoption requires all four of: a byte-identical first record, a **different**
project directory, a move referenced in the predecessor, and a shared prefix of
K=2 records. Conservative on purpose — a duplicate is absorbed by the server's
index, whereas a bad merge silently eats real events into a session they did not
belong to, so every condition fails toward minting today's id. Three findings
changed the rules from their first draft: rule 3 cannot require the invocation
(in **4 of 6** continuations the move killed the turn before the call record was
written, leaving only the schema fetch), it cannot require
`server == "cursor-app-control"` (the dynamic-tool shape carries no `server`
field), and K is a **ceiling** rather than a floor — two same-directory
look-alikes are byte-identical for their entire length, so no prefix separates
them and rule 2 does the work.

Two interactions closed that were not in the original scope. A **hook-claimed**
transcript keeps its filename id: the hook rail reads its session id from
Cursor's payload, which after a move is the new uuid, so adopting on the
transcript side would strand `task_dispatch`/`mcp_call` on a session with no
prompts — trading a duplicate for a split. And **subagent** rollup targets route
through the same map, so delegation does not strand on the abandoned id.

Known limit: candidates come from the watcher's own progress map, so a move
whose predecessor was never watched still duplicates. Inherent to
reconstructing identity from what we track.

**The MCP board went quiet on a rename, and two thirds of what was left was not
MCP.** Cursor renamed its MCP invocation around **2026-08-22T20:29Z** —
`CallMcpTool` with `{server, toolName, arguments}` became `CallDynamicTool` with
`{namespace, toolName, arguments}`. `toolEvent` only handled the old name, so
every post-cut Cursor MCP call was silently dropped, and a dropped `mcp_call` is
invisible: the board reads it as "this engineer used no MCP", not as a gap.
Production `mcp_call` per day: 307 on 08-21, 238 on 08-22, then **0** on 08-23,
08-24, 08-25 and 08-26, while `command` and `task_dispatch` kept flowing across
the same boundary. **~300 MCP calls a day, lost since 08-23.**

`CallDynamicTool` is also a **superset** of MCP — all five corpus calls are
Cursor's own chat management — so a one-line name mapping would have put five of
five non-MCP invocations on the MCP board. One
`case "CallMcpTool", "CallDynamicTool":` clause now shares one
`isCursorBuiltinNamespace` predicate, and it is a **denylist on `cursor-`, not
an allowlist on `user-`**: production has five namespace families (bare 1,595,
`plugin-*` 1,432, `project-*` 309, `user-*` 75, `cursor-*` 52), so an allowlist
would have dropped **~97.8%** of live MCP rows — a customer's entire Linear,
Grafana and Granola history — which is the same invisible-loss defect this patch
exists to fix. `CallMcpTool` stays mapped: the watcher re-reads back to
`transcriptHistoryWindow` (28 days), so old-format records are live input for
four weeks.

**Durability fingerprints grew forever and reloaded per path.**
`durability-fingerprints.json` was the only unbounded file in the state dir.
Expiry ran only over the paths the current commit re-captured, so a repo that
stopped receiving commits kept its fingerprints permanently — and the leak was
invisible to `fingerprintsForPath`, which filtered on read without writing back,
so the store looked correct while the file grew. Measured on one device:
**444,618 entries / 68.4 MB, 65% of them already past the 14-day TTL**, oldest
35.7 days. `pruneDurabilityFingerprints` now sweeps every root and path on each
write and deletes emptied paths and roots: **68.4 MB → 23.3 MB, 244 roots → 82**
over a copy of that real store. Behind the TTL sits a hard cap of 200,000
entries evicted oldest-first across the whole store — TTL is the policy, the cap
only bounds what TTL cannot. Eviction can only ever **lose** transfer evidence,
never invent it.

Second defect, same store: seeding resolved fingerprints one path at a time,
each call re-reading and re-unmarshalling the whole file under the lock. On that
68.4 MB store a 20-file commit spent **24.3s** answering 20 questions about one
snapshot; one load takes **1.0s (23.3x)**. Reading one snapshot is also the more
correct semantics — the per-path version could observe a concurrent write
partway through and seed a single commit against two generations of evidence.

Existing on-disk stores shrink on the engineer's next AI-attributed commit. No
migration, no startup sweep, nothing fleet-wide.

## [0.21.0] — 2026-08-24

### Added

**Within-session parallelism was unmeasurable, and the reason was one field on
one kind.** Lane identity — `agentId`, the per-INVOCATION handle for a concurrent
agent process — rode `subagent_usage` and nothing else. That is a SPEND event. It
answers what the delegates cost and it cannot answer how many ran at once,
because a lane's interval has to come from the first and last moment it was
WORKING, and the kinds that carry work carried no lane.

The consequence landed hardest on the rail with the least to lose: **Cursor had
no lane at all.** It emits no `subagent_usage` on purpose — its transcripts carry
no token counts, and a spend event with absent counts reads downstream as a
measured zero — so on the one rail where the id could not ride spend, there was
nothing else for it to ride. Its delegates' work arrived indistinguishable from
the main chain's.

- **Cursor** now stamps the lane on every event from a subagent transcript, taken
  from the child transcript's own uuid. `cursorLaneIDFromPath` is deliberately
  NOT the inverse of `cursorSessionIDFromPath`: the session is the parent, the
  lane is the child, and deriving either from the other is what collapsed them.
- **Codex** now stamps the lane on a delegated thread's work events, not just its
  usage — a Codex delegate is its own rollout file merged into the parent
  conversation, so without the stamp its file_diffs read as the parent's own. The
  thread a human types into stamps NOTHING: its thread id is the session, and
  stamping it would assert "one lane" where the truth is "the main chain, plus
  whatever it delegated". `session_start` stays conversation-scoped.
- **Codex** also now carries the delegate's TYPE name as `attributionAgent`, the
  key the Claude rail already uses, parsed from `session_meta.source`
  (`{"subagent":{"other":"guardian"}}`). The id says WHICH invocation, the name
  says WHAT KIND; deduplicating concurrent delegates on the name instead of the
  id is a measured 32x undercount.
- **Claude Code** needed no change: its delegates already emit timestamped
  `subagent_usage` carrying `agentId`, which is enough to bound a lane.

An empty lane stamps nothing, on purpose. "This rail did not tell us" and "this
session ran one lane" are opposite readings, and a placeholder would merge every
unidentifiable lane into a single fake one.

**Minor, not patch — the lane leaves the machine on eight kinds** (`prompt`,
`tool_use`, `file_diff`, `file_create`, `file_delete`, `command`, `mcp_call`,
`task_dispatch`). The server half is promptster-backend#803 and **must be
deployed first**: the device projector default-denies, so a field the server does
not allowlist is dropped at ingest with a `201` and no error anywhere. It merged
2026-08-24 18:13 UTC as `00adbc3`, and that is the exact commit both
`api.promptster.ai/health` and `worker.promptster.ai/health` report — the server
allowlist is live, verified against the running services rather than inferred
from the merge. That ordering is not a convention here —
`allowlist_lockstep_test.go` refuses the device-first build by name, and it
caught this one.

Eight kinds rather than a convenient subset, because the subset is
shape-dependent: a delegate that only prompted and called one MCP tool would
silently not exist.

### Security

**The lane id is clamped to an opaque shape at the projector.** An allowlisted
key is one an emitter can fill with anything, and the cheapest identity to hand a
lane is the file it came from — so the leak canary, which proves the projector
drops what is NOT allowlisted, could no longer see this field. `agentId` is now
accepted only as `[A-Za-z0-9_-]{1,64}`: `/`, `\`, `~`, `:`, `.` and whitespace
are what a path is made of, and what remains cannot spell a directory. Anything
else is dropped to ABSENT and logged. Measured against the real corpus before the
clamp was written — every `agentId` on the wire is lowercase hex and every Cursor
child transcript name is a hyphenated uuid — so it rejects nothing we mint.

`meta` stays unallowlisted for every kind and raw `cwd` stays dropped for every
kind. The lane is not a way around either.


## [0.20.0] — 2026-08-24

**A commit two tools touched was credited wholly to one of them, and a commit no
AI touched was given a session identity.** Both are the same defect at different
scales: `commit_attribution` published less than the reconciler already knew.

**Minor, not patch — a new field leaves the machine** (`files[].sessionId`). The
server half is promptster-backend#790 and **must be deployed first**: the device
projector default-denies, so a field the server does not allowlist is dropped at
ingest with a `201` and no error anywhere. It merged 2026-08-23 20:42 UTC and is
live — verified as an ancestor of the commit `worker.promptster.ai/health` reports
(`b1a16b3`), rather than inferred from the merge.

### Added

**`files[].sessionId` — the AI session that touched THAT file.**
`reconcileCommitAttribution` has always resolved it per file, counted it into
`sessionFiles`, and then discarded it; the event carried one commit-level session
picked by `mostFrequentSession`. That is winner-take-all: a commit two tools
touched is credited wholly to the one that touched more files, and on a
tool-dominant org the tie-break is self-reinforcing. Measured against production
2026-08-23, multi-session commits are **1.08%** of the live external org's 4,073
and **3.61%** of the internal org's 1,826 — small, which is exactly why the wire
has to be able to express it. A file no AI session touched omits the key.

The envelope `sessionId` still carries the modal session, deliberately: a consumer
cutover and a producer deletion in one release leaves no working state to roll
back to.

### Changed

**A commit no AI session touched now carries `unattributed:<deviceId>`, not the
bare device id.** The old fallback defended itself by analogy to `config_census`
and `presence` — device-scoped events nothing joins to a session. This one IS
joined to a session, and the join does not fail loudly: the device id used to BE
the session id, so it collides with a real session row rather than dangling.
Measured 2026-08-23 — **1,709 of the live external org's 2,134** attribution
events carried a device id and **all 1,709 reached the join**; **22 of its 113
resolving merged PRs (19.5%)** resolved only through one, with no AI session
behind them at all.

The marker is a prefix so a consumer can exclude it by name without a lookup, and
keeps the device suffix so the local signature chain — grouped by session id —
stays per-device instead of interleaving every device into one chain and reading
as tamper. The event is still emitted: a human-authored commit is evidence.

## [0.19.0] — 2026-08-22

**The Cursor rail reported tokens it could not price, and could not say so.**
Two halves of one silence. 0.18.0 began metering Cursor turns; the model that
prices them arrives on a different hook, and the join between the two asked for
a key the cache never held — so every auto-routed turn shipped its tokens with
no model and the backend correctly declined to price it. Measured against
production 2026-08-22: **29.1% of the live external org's metered Cursor turns
carry no model.** That is spend which exists, is captured, and is invisible.

**Minor, not patch — three new fields leave the machine** (`cursorHooks`,
`cursorHookRepairs`, `cursorHookUnverifiable`, on the presence beat). The server
half is promptster-backend#777 and **must be deployed first**: the device
projector default-denies, so a field the server does not allowlist is dropped at
ingest with a `201` and no error anywhere. It was deployed 2026-08-22 16:55 UTC,
before this release.

### Fixed

**The Cursor model join was keyed on two different ids and could never fire.**
Every auto-routed Cursor turn shipped its tokens with no model, so the backend
declined to price it — the `$0` on the Cursor rail was partly this, not the rate
table.

Cursor delivers one turn's facts across two hooks: `afterAgentThought` carries
the resolved model, `stop` carries the tokens and reports `"default"` on both
model spellings. `cursorGenerationModel` joined them on `generation_id`. They do
not share one. Measured on IDE 3.17.8 with the picker on Auto, 2026-08-22:

```
afterAgentThought  gen 3a8b6e45-…-617844887809         model_id "default"
afterAgentThought  gen 3a8b6e45-…-617844887809-3-1v0i  model_id "composer-2.5"
stop               gen 3a8b6e45-…-617844887809         model_id "default"   in=823043 out=4914
```

Each thought fires **twice** — once on the bare turn UUID reporting the sentinel,
once on a `-<n>-<slug>` sub-request id carrying the real model. `modelLabel`
correctly rejects the sentinel, so the cache only ever learned suffixed ids,
while the lookup only ever asked for bare ones. Guaranteed miss.

Both sides now reduce to the base turn UUID (`cursorGenerationBaseID`). The
36-char prefix is verified to be a canonical UUID before it is used: an id of an
unrecognised shape is kept whole, because truncating one would mint a key that
silently joins unrelated turns — quieter and worse than the miss being fixed.

**Why it survived review.** On a *pinned* model `stop` resolves its own model and
never consults the cache, so the join is dead code on exactly the turns that
work. It is reached only under Auto, where it missed every time — invisible in
aggregate, and perfectly correlated with the one case it existed to serve. The
regression test asserted the bug: it used `"g1"` for both halves, a shape Cursor
never emits. It now uses the ids Cursor actually sent, and fails without the fix.
### Added — the Cursor hook rail can say it is broken

The 0.18.1 repair works and was **completely unobservable**. It runs at watch
startup, fixes the file, and nothing leaves the machine to say so — so "did the
fleet actually recover" could only be answered by running `doctor` on every
laptop. That is the same shape as the defect itself: state that exists only on
the device.

The presence beat now carries three fields, read at build time alongside the
outbox backlog so every number in the beat describes the instant its `ts` does:

- `cursorHooks` — one word from a **closed** enum, ranked worst-first:
  `not_installed`, `unreadable`, `rejected`, `dangling`, `unenrolled`,
  `partial`, `ok`. `rejected` outranks `partial` because a rejected file makes
  enrollment irrelevant — Cursor runs none of it.
- `cursorHookRepairs` — cumulative, so "did the repair ever run here" survives
  the repair succeeding. Afterwards the file is correct and nothing on disk
  admits we changed it, which is the silence this defect was made of.
- `cursorHookUnverifiable` — the honesty term on `ok`, which means "nothing
  *provably* wrong". Our validator refuses to condemn a hook type it does not
  recognise, and where this is non-zero that claim is weaker than it sounds.

**Never a reason string, and that is the point.** `hooks.json` is shared with
every other tool on the machine, so free text here could carry a neighbour's
command line. A closed enum cannot. The doctor's prose stays on the device,
where it is safe.

Two review findings from #176 landed with it, both of which were the same
false-green shape the beacon exists to end: `fileExists` answered "is there a
file here" where the rail needed "can Cursor run this" (a binary present
without its executable bit reported `ok` and produced nothing — now
`dangling`, checked on unix only, since Windows has no such bit); and the
repair count was the length of a record window trimmed at 50, so a
"cumulative" number stopped moving on exactly the machines being repaired
most. The log now carries a `total` that survives the trim.

Reported at zero rather than omitted — a measured "we repaired nothing of
yours" must not be indistinguishable from a CLI too old to report at all.

Requires promptster-backend#777; a device ahead of the server has these fields
dropped silently at ingest, which is why the server ships first.


## [0.18.1] — 2026-08-22

**We were writing the entry that killed every Cursor hook on the machine.**
`~/.cursor/hooks.json` is user-scope — every tool an engineer runs writes into
the same file — and Cursor validates it **all-or-nothing**: one entry it rejects
and it discards the *whole* config. Not the entry, not the step. The file.

Cursor's schema lets a hook entry omit `type` (it defaults to `"command"`), so
`{"command": "…"}` is legal and common. Our hook entry struct modelled `type` as
a plain string with no `omitempty`, so reading a legal type-less neighbour entry
and writing the file back produced `"type": ""` — invalid, whole file rejected.
Measured on one machine, from its own backups and Cursor's log:

```
Aug 18 16:36  neighbour entry reads {"command": "bash …"}   legal
Aug 18-20     our watcher round-trips the file              we break it
Aug 20 21:41  Invalid user config: sessionStart[1]          all hooks off
```

**17 hooks across four tools, dead for two days, silently.** The only trace is a
per-window Cursor log nobody reads, and our rail going quiet is indistinguishable
from Cursor being idle.

**Upgrade to repair.** The damage is on disk and outlives the fix that caused it:
this version repairs it at `watch` startup. Machines with no Cursor install, or
no type-less neighbour entry, were never affected.

### Fixed

- **Hook entries round-trip verbatim** (#173). The same struct that manufactured
  `"type": ""` also **silently deleted `timeout`, `matcher`, `loop_limit` and
  `failClosed`** from neighbours' entries on every write, because it marshalled
  only the four keys it modelled. `matcher` decides which files their hook fires
  on; `failClosed` decides whether its failure blocks their agent. Unknown keys
  are now preserved as raw JSON and written back untouched.

- **`saveCursorHookConfig` refuses to write a config Cursor would reject** (#173).
  The gate is on the **write** side deliberately: a read-side check could not have
  caught this, because the file we read was always valid and our own serialisation
  is what broke it. A validator that only reads is blind to a defect its own
  writer creates.

- **Damage already on disk is repaired** (#173), but narrowly: by deleting a
  meaningless `type` key so Cursor's own documented default applies. We never
  delete a neighbour's entry — that would be correct only while our validator is
  correct, and down-and-loud beats quietly destructive. Anything else is reported
  and left alone. Repairs leave a record at
  `~/.promptster-teams/cursor-hook-repairs.json` plus an untouched pre-repair
  copy, because a successful repair is otherwise invisible.

- **`doctor` and `status` lead with the whole-file verdict** (#173). "Enrolled for
  all 8 steps" is a false green when Cursor is running none of them. This is
  separate from the repair on purpose: *the daemon is a different binary*, so a
  machine on an old daemon never executes the repair, and `doctor` — typed at a
  fresh binary while something is already wrong — is the only place it hears.

  A repair record is a **note**, not a problem: it must not suppress the health
  verdict or the usage-coverage numbers. Caught in review, since the tail gated
  its healthy output on "is the list non-empty?" while meaning "did I find a
  problem?".

### Notes

The validator is transcribed from Cursor 3.12.17's own (`workbench.desktop.main.js`,
module `hooksConfig.ts`) rather than from its docs, which disagree with it. It is
deliberately **one-directional**: it answers "can I prove Cursor rejects this?",
never "do I recognise this?". An unfamiliar hook type is `unknown`, not invalid —
so the day Cursor ships a third hook kind we go quiet rather than "repairing" a
hook that works. Same reason `matcher` is checked for stringness only: Cursor
compiles it as a JavaScript RegExp, and Go's RE2 rejects lookahead and
backreferences that Cursor loads happily.

## [0.18.0] — 2026-08-21

**Two facts the device already had, and never sent.** When the provider reported
no rate-limit window the capture path emitted *nothing*, so "the provider reported
no window" and "the shim never ran" were byte-identical on the wire — and the
console resolved that one observation into the least likely of its five meanings,
printing **"Usage-billed — no window · Pay-as-you-go account"** about a named
person. Four of the five causes are flat-fee subscribers whose marginal cost is
`$0`, which is exactly the population the "% of window" currency exists to serve.
Separately, Cursor's `generation_id` was parsed on every turn and fed to a hash,
but never written into the payload — so the only key that can resolve a Cursor
row's model never left the machine.

Minor, not patch: two new fields leave the machine.

**Upgrading is the only fix for either.** Both are decided on-device and folded
into the event before it is signed, so a session captured by 0.17.0 has no
`signalState` and no `generationId`, and no backend re-derivation puts them back.

### Added

- **An absent window is now a positive fact** (#170). `signalState` on
  `windowUsage` — a closed enum of three content-free words. `provider_absent`
  (the Claude shim parsed the blob and there was no `rate_limits`) is the **only**
  evidence that earns "usage-billed"; `plan_unsupported` (Codex reported a span we
  cannot carry) says "your plan's window isn't one we carry yet", which is not a
  billing claim; `reported` is the normal case and is omitted from the wire.

  **An absence carries no percentage key at all** — not `null`, not `0` —
  constructed through `absenceReading()` rather than at each call site so a future
  caller cannot reintroduce one, and pinned in the emitter tests *and* the
  projection test. The spool's drain also had to change: it discarded anything
  `empty()`, which is what an absence is by construction.

  Throttled to one per hour per provider and **never gated on `lastObserved`**. An
  absence's `observedAt` is the tick time, so it moves every tick and de-dup cannot
  bound it — unthrottled that is one event per statusline render against a fact
  that does not change (6,893 `windowUsage` events measured on one machine, 87%
  carrying no new information). Keeping the throttle off the `lastObserved` path is
  what lets a plan change produce a reading *immediately* after an absence rather
  than an hour later, and there is a test for that direction specifically.

  `ok=false` from the shim parser now means only "this is not a statusline blob".
  We assert nothing from a payload we could not read.

- **Cursor rows carry `generationId`** (#171). The field was parsed and handed to
  the row-id discriminator, and never written into `data` — settled against prod
  rather than by reading code: zero rows carried it, all orgs, all time. **A hash
  input is not a join key.** `afterAgentThought` resolves `model_id` against the
  *generation*, not the session, so a row that arrives modelless can never be
  repaired server-side.

  The write sits **after** the empty-payload guard, and is commented as such.
  Written before it, a fully aborted turn stops being empty and the normalizer
  begins emitting rows that say nothing — while inflating this rail's response
  count against the rails it is compared with.

  Both releases re-sync `testdata/capture-allowlist-canonical.json`, which is
  maintained **by hand**. Between a server-side allowlist landing and its emitter
  shipping here, the lockstep guard compares against a stale server surface and
  passes — so the window in which a divergence exists is precisely the window in
  which the guard cannot see it. It reports on whenever someone last remembered.

## [0.17.0] — 2026-08-20

**Three rails gained a number they had been generating all along.** Cursor turns
were captured without their token counts, Codex stated its context window into a
field the allowlist dropped, and the Claude rail had no window at all — so the
console divided a peak by a hardcoded 160,000 and called it a share.

Minor, not patch: three new fields leave the machine, and one of them changes
what the statusline shim discloses.

**Upgrading is the only fix for all three.** Each of these is read on-device and
folded into the event before it is signed, so a session captured by 0.16.0 has no
window and no Cursor tokens, and no backend re-derivation puts them back. The
under-count stops accruing forward; it does not heal backward.

### Added

- **Cursor turns carry their token counts** (#166). `stop` is now registered, and
  it delivers `input`/`output`/`cache_read`/`cache_write` per generation. The
  step was never declining us — Cursor's dispatcher early-returns on any step
  nobody registered, so "Cursor requested zero stop steps" measured OUR
  configuration and got filed under the vendor's capabilities. A probe that never
  ran and a vendor that sends nothing produce byte-identical evidence; the
  re-probe added `beforeSubmitPrompt` as a positive control to tell them apart.
  Measured 2026-08-18, one live org held 3,579 `cursor-hook` `ai_response` events
  and **not one** carried a token count.

  Tagged `usageScope: "request"`, because absent that tag the backend reads a row
  as cumulative and differences it against a running maximum — which drops the
  first row as a baseline. Output FELL 902 → 525 between consecutive generations,
  which no running total can do. The model is joined on-device by
  `generation_id` against `afterAgentThought`, since `stop` reports the routing
  sentinel rather than the resolved id; no join entry means the row ships with
  tokens and no model, and the backend declines to price it. Never defaulted,
  never inherited — Cursor auto-routes, so an inherited model id is a fabricated
  price.

- **Codex's `model_context_window` reaches the wire** (#167). The rollout log
  states it; the on-device allowlist was dropping it. Emitted as
  `contextWindowTokens` on `ai_response` and `subagent_usage`.

- **The Claude rail has a context window for the first time** (#168). The
  transcript does not carry one — measured, not assumed — and the statusline
  stdin blob is the only channel that does. The shim spools the reading per
  session; the watcher pairs it onto `ai_response` within a **15-minute skew
  bound** and refuses the pairing outside it, because a window read hours later
  may describe a different model. Deliberately not stamped on `subagent_usage`:
  a subagent's window is not the parent's.

  A model-id lookup table cannot substitute for this, in principle and not merely
  in staleness: `claude-opus-5` reports **both** 1,000,000 and 200,000 depending
  on the session.

  **This changes the shim's disclosure.** Three things now leave the machine
  where two did before — your two usage percentages, their reset times, and your
  model's context-window size. `login` and `statusline` say so, and that sentence
  is required to stay exhaustive.

### Internal

- Per-session spool files with atomic temp-and-rename, and a 24h TTL with an
  hourly prune (#168). A single fixed `.tmp` path is shared state between two
  shim processes: the payload lands in one write syscall, so the failure is a
  **stale** reading and a dropped update, never a torn file — which is why the
  test asserts that no concurrent write returns an error rather than asserting
  torn content. The content assertion passed against the broken code.
- `staticcheck` pinned to `@v0.7.0` in CI (#167). `@latest` began resolving to
  v0.8.0, which requires Go ≥ 1.26 against this module's 1.25 floor.

## [0.16.0] — 2026-08-18

**The config census was reporting one instruction file as forty.**
`projectClaudeMdTokens` summed one `CLAUDE.md` per linked git worktree, because
it reused the root set the transcript *watcher* needs. Those two callers want
opposite things from one list: the watcher asks *which paths belong to this
repo*, and every worktree does; the census asks *what standing context does a
session here load*, and the answer is exactly one. Nothing said so, and no test
spanned the seam.

Measured on one machine, 2026-08-18:

| repo | worktrees | one checkout | reported | over |
|---|---|---|---|---|
| promptster-backend | 42 | 5,856 | 239,088 | **40.8x** |
| promptster-teams-cli | 17 | 2,436 | 122,689 | **50.4x** |
| cc-audit | 1 | 2,247 | 2,247 | 1.0x |

Every inflated repo is multi-worktree; every exact one is a single checkout,
which is why this was invisible on a demo machine and fired hardest on the
worktree-per-session workflow. Downstream it is `configTax`, and it made the
console tell one manager that **90.9% of their AI bill was recoverable** — with
"delete your CLAUDE.md" as the implied action.

Minor, not patch. Two reasons an operator is entitled to read in the version
number: the census now emits a **materially different number** for the same
disk, and Codex capture carries a **new usage field** it did not before.

**Upgrading is the only fix.** A census is summed on-device before the event is
signed, so no backend re-derivation can repair a stored one. Until a device
runs 0.16.0 its config tax stays inflated.

### Fixed

- **Always-loaded project memory is sized from ONE checkout, never the worktree
  set** (#164). Sizing takes `primaryWorkspaceRoot` and nowhere else — the
  nested-package fallback included, which had the same defect one level down.
  That fallback already took the MAX and never a sum, on the argument that
  sibling packages' memories don't co-load on one request; worktrees are that
  identical argument one level up, and the root branch simply never received it.
  `workspaceMatchRoots` keeps its worktree expansion — the watcher is right to
  want it — and now records in its own doc comment that it is **not a sizing
  input**, naming this defect.

### Added

- **Codex `cache_write_input_tokens` is captured** (#163). OpenAI began charging
  for cache writes at the GPT-5.6 GA (2026-07-09) at 1.25x the uncached input
  rate; the normalizer read input/output/cached and discarded the write count.
  A field never captured cannot be backfilled from stored rows, so this is an
  under-count that only stops accruing forward. Emitted as
  `cacheWriteInputTokens` and deliberately never folded into `cacheWriteTokens`:
  that key is the Anthropic ADDEND beside `inputTokens`, while Codex's write
  count is a SUBSET of its input — the same inversion that, for
  `cached_input`, double-counted 89% of a cache-heavy session. Absent by
  omission rather than defaulted to 0, because on this field the absence carries
  information a fabricated zero would erase.

### Internal

- The on-device redaction allowlist is diffed against the server's **in both
  directions** (#161). A translator/allowlist name mismatch strips 100% of a
  field and reads as "the vendor doesn't send it"; checking one direction only
  cannot see the half where the server admits a key the device never emits.
- GitHub Actions dependencies bumped (#138).

## [0.15.0] — 2026-08-05

**The census was still only looking where skills are kept by hand.** 0.13.0
taught it to walk Codex and Cursor, but all three walks stopped at the personal
root — `~/.claude/skills`, `~/.codex/skills`, `~/.cursor/skills-cursor`. On one
real machine that missed the two largest sources outright: skills installed by a
plugin, which live under `plugins/cache/<marketplace>/<plugin>/<version>/skills`,
and the cross-client `~/.agents/skills` root that the Agent Skills convention
gives every tool. Twenty skills that engineer uses constantly were inventoried
nowhere, while twenty-five he rarely touches were the whole of his ledger.

Minor, not patch: like 0.13.0, this version READS directories no previous
version opened. What leaves the machine is unchanged in kind — skill names,
description token counts, and now which plugin a skill came from; never a path,
never a file body.

### Added

- **Plugin-cache skills** are censused for both Claude Code and Codex, folded to
  the newest installed version. Several versions of one plugin routinely sit on
  disk at once; without the fold a single skill inventories two, four or six
  times and its always-on carry is billed once per copy.
- **`~/.agents/skills`**, the client-neutral root, is walked and recorded with no
  tool attributed — because no one tool owns it. Naming a client there would
  invent the very attribution the census exists to establish.
- Every skill now carries its **provenance** (`user`, `agents`, `plugin`,
  `plugin-cache`) and, where one applies, the **plugin** that installed it. A
  skill's carry is only actionable if you can tell someone where it came from:
  "uninstall this plugin" is an action, "you have a skill called review" is not.
- Enablement is honored where it is knowable. Claude Code records which plugins
  are enabled, and a disabled plugin's skills are excluded — they cost no tokens
  because the model never sees them. Codex publishes no equivalent, so its cache
  is reported as installed and marked `plugin-cache` rather than guessed at.

### Fixed

- **Symlinked skill directories were skipped entirely.** `os.ReadDir` reports a
  symlink as a link, not a directory, so a skill linked into a client's root
  was invisible — 4 of 17 on the machine this was found on. Linking a shared
  skill into a per-tool root is a documented workflow, not an edge case. All
  four walks now resolve links, and the resolved path deduplicates the same
  skill reached by two routes.
- **Plugin skills were billed twice.** Their description tokens counted toward
  both the skill-listing line and the plugin-listing line, which the rollup sums
  as two always-on costs. Skills sourced from a plugin are now excluded from the
  skill-listing tally, where the plugin line already accounts for them.
- Two plugins shipping the same skill slug collapsed into one entry, silently
  attributing the second plugin's activity to the first.

## [0.14.0] — 2026-08-04

Recorded after the fact: 0.14.0 shipped as a version bump with no changelog
entry. It carried six merged changes, none of them capture-format changes.

### Added

- Batch ingest, probed and fallen back from losslessly (#152).
- Delivery is split into **live** and **backfill** lanes with separate durable
  queues, so a device replaying history no longer delays what is happening now
  (#154). The live queue deliberately keeps its old filename, so a device
  upgrading mid-backlog drains what it already queued instead of stranding it.
- Progress-schema migrations declare their replay cost, and a bump is documented
  as a fleet-wide replay event (#153, #155).

### Fixed

- History reconstruction is observable, and a lost progress file reports itself
  rather than silently restarting (#156, #157).

## [0.13.0] — 2026-08-04

**If you work in Codex or Cursor, this is the release where Promptster starts
seeing your setup at all.** Until now the config census looked in `~/.claude`
and nowhere else, so an engineer working in another agent was recorded as owning
zero skills, zero MCP servers and zero subagents. That zero was never a
measurement — it was the shape of a walk that never looked — but on the Skill &
Asset Health board it was indistinguishable from someone who had genuinely
configured nothing. On one real machine the new walk finds 39 skills where the
old one found 13.

Minor, not patch: this version READS two directories no previous version opened.
Nothing more is uploaded from them than was already uploaded from `~/.claude` —
config-identity names and token counts, never file contents — but "we now look
in a new place on your disk" is a change an operator is entitled to see in the
version number rather than discover in a diff.

### Added

- The config census walks **Codex** (`~/.codex`) and **Cursor** (`~/.cursor`)
  alongside Claude Code, and stamps every skill, plugin and MCP server with the
  tool it belongs to. The same name under two agents stays two assets — folding
  them would price one tool's carry against the other's usage. Cursor's skills
  live in `skills-cursor`, not `skills`, and its MCP servers are invoked as
  `user-<name>` while `mcp.json` keys them bare; both are handled, and getting
  either wrong returns a convincing zero rather than an error.
- `toolsExamined` records which agent roots actually existed when the census
  ran. Examined-and-empty and never-looked-at are different facts, and an empty
  list cannot tell them apart — this is what stops the second being read as the
  first.
- Codex MCP servers are read from `config.toml` section headers only. No
  key/value line is ever parsed, so commands, arguments, environment variables
  and URLs stay on the machine by construction rather than by a filter applied
  afterwards.
- Cursor **subagent dispatches** and **MCP calls** are now captured. Cursor
  records no token usage for them on any rail, so they arrive as activity with
  no cost attached — deliberately, because a fabricated `$0` is a stronger claim
  than we can support.

### Fixed

- Cursor sessions covered by the hooks integration were skipped whole by the
  transcript watcher. Cursor exposes no hook for an MCP call or a subagent
  dispatch, so on a hook-enrolled machine — the recommended setup — those two
  were captured by neither rail. The watcher now reads exactly those two kinds
  from a hook-covered session and nothing else, so nothing is captured twice.
- A comment on a Codex `config.toml` table header (`[mcp_servers.db] # staging
  creds in 1password`) was parsed as part of the server's NAME and uploaded with
  it. Header parsing now stops at the closing bracket.
- The census read `~/.codex` and `~/.cursor` literally while the watchers honor
  `CODEX_HOME`. With that variable set, Promptster inventoried one location's
  configuration and captured another location's sessions.

## [0.12.4] — 2026-08-04

**If you are still on 0.12.2 or earlier, install this one BEFORE you next
restart capture.** 0.12.3 raised the transcript progress-schema version, and
crossing that bump clears every stored byte offset so the watchers re-read the
last 28 days of transcripts from the beginning. That import is intentional. What
was not intentional is the order it ran in: the upload queue is strictly
first-in first-out, so three weeks of history went out ahead of the work you
were doing right now, and on a machine with a lot of history that could take
hours. The whole time, your dashboard would show you as having no active
sessions — the daemon was healthy, uploads were succeeding, and every event
arriving was correctly dated three weeks ago. This release fixes the ordering,
so the same import happens with today's work at the front of the line.

### Fixed

- **History replay no longer puts live capture last in line.** Both the Claude
  Code and Codex watchers now walk candidate transcripts newest-first — ordered
  by each file's most recent modification, with a stable tie-break so two files
  touched in the same instant do not reorder between runs. Reading *inside* a
  file is still oldest-to-newest, so turn correlation is untouched. The
  practical difference is what an interrupted backfill leaves behind:
  previously it left the recent window unread, which is the only window anyone
  is looking at.

### Added

- **The heartbeat now reports how far behind its own upload queue is.** Every
  presence beat carries `pendingEvents` (undelivered events still queued) and
  `pendingOldestEventAt` (the timestamp of the oldest one). The device is the
  only party that can know this, and without it a machine that is connected and
  hours behind is indistinguishable on the wire from one that is connected and
  idle — which is exactly how a working engineer showed up as inactive. A
  measured zero is sent as a zero and never omitted, so "caught up" and "too old
  to tell you" stay different answers.

  The age is the load-bearing half. 62,000 events queued from the last five
  minutes is a busy afternoon; 62,000 whose oldest is dated three weeks ago is
  an outage, and the count alone cannot tell them apart. The reported age is the
  **oldest** pending event, not the one at the head of the queue — because with
  replay now running newest-first, the head is recent work and the old events
  sit behind it.

  No transcript content is involved. These are two numbers about the queue, and
  nothing about what is in it.

## [0.12.3] — 2026-08-04

**Upgrade this one by restarting capture, not just by installing it.** Several
fixes here run at `watch` startup or inside the daemon, so a machine that
installs the new binary but leaves the old daemon running gets none of them. On
a daemon predating this release nothing on our side can fix that — it does not
carry the code that would. Run `promptster-teams stop && promptster-teams start`
once; after that the daemon keeps itself current on its own.

### Fixed

- **The daemon now adopts a newer binary already sitting on its own disk,
  instead of running the old one until the next reboot.** Self-update only ever
  compared the running version against a GitHub release tag, so the daemon was
  blind to the most common way it goes stale: a newer binary arriving by a route
  that is not us — `npm i -g`, `install.sh`, an MDM push, another invocation
  swapping the shared managed path. The machine already had the fix, and since
  nothing outside a process can change the code that process is running, only a
  reboot or a human typing `stop && start` ever picked it up. **That is the root
  cause behind every "doctor is green but the daemon is months old" report.**

  Every 5-minute poll now checks the binary on disk before it asks the network,
  and re-execs into it when it is strictly newer. It downloads nothing and
  verifies no signature because it installs nothing — which is also why it is
  **not** gated on `--no-auto-update` or a pinned version: those govern what gets
  fetched and installed onto a machine, and this executes what is already there.
  A pin's enforcement point is still the download. A project-local install is
  left alone entirely; its lockfile is a deliberate pin.

  It follows the daemon's own path first, then the managed one — the second is
  what reaches a daemon `autostart` launches from a baked absolute path that is
  now stale, and a daemon whose own directory is not writable, which can never
  self-update but can exec.

- **`doctor` now describes the process that is capturing, not the one printing
  the report.** Those are routinely different builds — self-update swaps the
  file and re-execs, `autostart` bakes an absolute path, a running process keeps
  its inode after its file is deleted, and under `npx` the foreground binary is
  the newest thing on the machine by definition. A machine running 0.12.2 in the
  foreground with a pre-0.12.0 daemon printed "up to date" with a straight face
  while the Cursor hook rail, which enrolls at `watch` startup, had never run.

  The watcher now stamps its version and executable under the capture lock, and
  that record is trusted only while the recorded PID is the live lock holder's —
  a record left by a dead process reads as **unknown**, never as whatever holds
  the lock now. `doctor` splits four outcomes that were previously one silence:
  not running / older (an error, naming the fix) / unknown build / newer, which
  is deliberately not an error, because there the foreground copy is the stale
  one and a restart would install an older build.

  `start` acts on it too: a live daemon on a strictly older build is restarted
  rather than met with "already running", and the restart is confirmed by process
  identity before it is reported.

- **`stop && start` no longer leaves capture running unsupervised until the next
  login.** Our own `stop` boots the autostart unit out of its supervisor domain,
  and nothing re-armed it. `start` now re-enables a unit left installed-but-
  unloaded — the only state reachable from our `stop` — which also re-renders a
  baked binary path that an `npm` upgrade has since deleted. It never enrolls
  autostart for anyone who has not enabled it.

- **The npm launcher now installs the managed binary itself**, rather than
  trusting `postinstall` to have done it. Newer npm gates install scripts behind
  an approval and reports a **fully successful install** while running none of
  ours — observed in the field, twice on one machine — and `--ignore-scripts`
  lands in the same place. What silently did not happen was everything that
  matters: the managed binary was never written and autostart was never
  re-pointed at it. Running the CLI at all now converges the machine, gated
  behind a marker file so the common "nothing to do" answer costs one small read.

- **A branch's AI history now survives being opened in a new local copy.** A
  fresh `git worktree add` is a new path and therefore a cold start: the cursor
  was baselined straight to head and none of the branch's existing commits were
  ever surfaced, so that copy read as though the branch held no AI work at all
  and every later rewrite of its AI lines emitted no rework verdict. A
  cold-started root now re-folds its branch's own commits for their state alone —
  nothing is re-attributed, no verdict is re-emitted.

  It is gated on this device **already holding attribution** for a commit in the
  range, and that gate, not the cursor, is what leaves a genuinely fresh install
  untouched: a new machine has nothing attributed, so the range is dropped
  without reading a single commit. This recovers history the device measured
  through another copy; it does not import history it never saw. The range is
  taken whole or not at all — a partial rebuild starts mid-branch, and both cut
  ends fabricate.

- **Surviving-line figures move DOWN for any repository whose default branch
  contains merges — the same edits were previously counted twice.** A merge
  commit's diff is read with `git show -m --first-parent`, so it already carries
  everything the merged-away branch brought in. The durability watcher
  nevertheless folded that branch's own commits as well, applying the same hunks
  once in the merged-away branch's coordinate space and again in the merge's.
  Every tracked AI span then slid by an offset already applied, until it
  addressed lines nobody wrote — and a span sitting on lines the AI never touched
  is not churned when the AI's real lines are rewritten, so it quietly survived
  its 30 days and reported as `durable`. The corrected numbers are lower, and
  they are the honest ones.

  **If a durability figure dropped after this release, this is why**, and the
  size of the drop tracks how many merges the branch has. Nothing was lost: the
  merge itself is still folded, so the lines it brought in are still tracked —
  once.

  The pre-merge rework ledger stopped double-folding merges in 0.12.2, and that
  entry said fixing durability was a separate change — this is it. Until now
  **the two ledgers disagreed with each other on any history containing merges**
  and neither was authoritative there. They now agree.

- **A `git rebase` no longer leaves a feature branch's AI spans pointing at lines
  nobody wrote.** HEAD is detached for the whole of a rebase, and the watcher
  polls every 60 seconds — an interactive rebase spends far longer than that
  waiting on the editor. A poll landing in that window attributed and recorded
  every replayed commit while folding none of them into the rework ledger, and a
  recorded commit is never revisited. The branch's tracked spans kept coordinates
  the replayed hunks had already moved, so the next rewrite of the *human* lines
  that took over those coordinates emitted a `rework_verdict` against them. Those
  spurious verdicts are gone.

  A rebase now releases that branch's rework tracking instead, which is an
  undercount rather than an invention — and a rebase rewrites the very history
  those coordinates were measured against. A poll that surfaces no commits
  releases nothing, so a bare `git checkout --detach` keeps its tracking.

- Commit attribution is unchanged by both fixes. Every commit in the detected
  range is still emitted exactly once, including one reachable only through a
  merge's second parent.

- **Context history now appears after first boot instead of starting on install
  day.** Claude Code and Codex capture now backfill the previous 28 days of local
  sessions whose recorded cwd is inside a watched root. The bound matches the
  context re-read heatmap, and each transcript's own timestamp is authoritative;
  older sessions remain go-forward only. Existing installs receive the backfill
  once through a progress-state migration, with deterministic event ids keeping
  overlap idempotent. Cursor remains go-forward because its transcripts expose
  neither a trustworthy session timestamp nor the token telemetry this view
  needs.

- **Removing a capture root now actually stops that directory's sessions from
  uploading.** 0.12.2 re-checked only the transcripts cached as mismatches, so
  widening the roots took effect but narrowing them did not: a transcript
  already accepted kept being tailed after its directory was no longer watched.
  Any change to the effective root set now revalidates every cached
  classification in both directions. Byte offsets are kept, so nothing already
  consumed is uploaded twice.

- **A single oversized transcript record no longer stalls a session's capture
  indefinitely.** Records are read whole or deferred whole against the per-poll
  byte budget, so a partial record is never half-consumed. A record larger than
  the 8 MiB supported maximum — which no future poll could complete — is
  discarded in bounded chunks and reported on stderr, rather than being re-read
  from the same offset on every poll while the rest of the session goes
  uncaptured. Classification measures against that same maximum, so it skips
  such a record instead of restarting from byte zero forever.

- **AI attribution now agrees across every checkout of a repository.** The
  AI-path ledger records a file under the checkout the agent edited in, but a
  commit belongs to the repository, not to one of its checkouts — so a commit
  reconciled from a *sibling* `git worktree` reported `unknown` where the
  original checkout reported `likely_ai`. Anyone running more than one worktree
  against one repository was under-reporting AI attribution on every commit made
  in another worktree, and the same gap capped the cross-checkout replays added
  in 0.12.2 and #128: they replayed the right commits and found no AI evidence to
  match them against.

  **Reported figures will move UPWARD for anyone running more than one
  worktree** — `commit_attribution`, and the durability and rework verdicts
  downstream of it. Nothing is re-attributed retroactively: commits already
  recorded keep the attribution they were given, so the change appears going
  forward.

  A lookup now falls through to the repository's other worktrees, own checkout
  first. It is scoped **to** the checkouts `git worktree list` reports for that
  one repository *whose HEAD reaches the commit* — that checkout holds it — and
  **against** everything else: a different repository cannot contribute evidence,
  two unrelated files that merely share a relative path cannot collide, neither
  can a *clone* of the same upstream (its own object store, its own worktree
  list), and neither can a worktree parked on a **divergent branch** or on the
  commit's own **branch point**. The gate runs in that one direction only. "The
  commit descends from the sibling's HEAD" looks like the same fact from the other
  side and gates nothing: `git worktree add -b feat` leaves the new checkout
  sitting on the branch point, and evidence is recorded the moment the agent
  writes a file, so every later commit on every branch descends from it — which
  would hand an agent's write in the feature worktree to a human's commit on the
  default branch. The widening is over checkouts, never over paths: the committed
  path must still match exactly, so a path no agent wrote in any checkout stays
  `unknown`. Evidence already on disk is read exactly as before and is never
  invalidated.

  **What it deliberately does not recover:** evidence held by a checkout that does
  not hold the commit — the agent's worktree switched branches, was reset, or
  never committed what it wrote. Those commits keep reading `unknown`. That is a
  conservative under-count on purpose; an invented number is the failure this
  product cannot afford, a missing one is not.

  The reachability answers are computed **once per sibling per poll** over the
  whole range being processed and then read from memory — one `git rev-list` per
  sibling per commit loop, a number that does not grow with the commit count.

- **A seed tombstone is no longer dropped, or written empty, because a sibling
  worktree moved.** Tombstones record which AI-write evidence has already been
  *spent* on a path, and only a strictly newer write may re-authorize seeding.
  They were being read through the same commit-gated view as the seed gate itself,
  which is the wrong question for bookkeeping that can only ever suppress: a
  sibling checkout moving onto its own branch pruned tombstones whose evidence
  lived there — re-arming first-touch seeding, so the next purely human commit to
  that path was recorded as fresh AI — and a path leaving the ledger was
  tombstoned at "no evidence", letting the write already spent on it clear the
  gate and seed the path a second time. Both ledgers (durability and rework) now
  read every checkout for that bookkeeping while the seed gate keeps the narrow
  view, and a tombstone may only ever rise.

## [0.12.2] — 2026-08-03

### Added

- **`doctor` now reports the Cursor hook rail.** The state worth catching is an
  enrolled entry naming a binary that no longer exists — created by deleting the
  binary *without* running `uninstall` (an `rm -rf ~/.promptster-teams`, an image
  rebuild, a restored home directory). Cursor then execs a missing command inside
  the engineer's agent loop on every prompt, edit and shell call, and nothing on
  our side can detect or repair it from the inside, because once the binary is
  gone none of our code runs. `doctor` — which needs a working binary and is the
  command engineers run while something is already wrong — is the only place the
  message can land, and it names `uninstall` as the fix.

  It also distinguishes not-enrolled (a missing signal, warning), a partial
  enrollment that a capture restart will repair, a `hooks.json` that does not
  parse (enrollment refuses to touch it, so it stays off until a human fixes it),
  an entry pointed somewhere other than the managed path, and no Cursor installed
  at all. The check is strictly read-only — it never enrolls, repairs, or writes
  `hooks.json`.

### Changed

- Secret scanner `github.com/praetorian-inc/titus` 1.2.6 → 1.2.7, and the
  GitHub Actions group across six actions.

### Fixed

- **Every Cursor edit was attributed as human.** The wiring was already there —
  `emitCursorEvent` relativizes paths and records each one via
  `recordAiTouchedPath` — but the keys were written in the wrong space.
  `TaskRoot` falls back to `os.Getwd()`, which is correct for a process the
  engineer starts and wrong for one **Cursor** spawns, because Cursor picks the
  cwd and it is not the daemon's workspace. `TaskRoot` decides both the base
  paths are relativized against and the ledger key that gets written, so a wrong
  root does not mislabel a path — it writes keys
  `reconcileCommitAttribution` can never look up.

  The failure is silent by construction: a missed ledger hit is indistinguishable
  from a human edit. So every Cursor edit landed as unknown/human, with no
  `likely_ai`, no `ai_revised_by_human`, and nothing for durability to follow.
  Measured on a live device before the fix: 35 of 40 absolute Cursor paths were
  under `HOME` and should have relativized, against 0 of 33 for `claude-code` and
  0 of 75 for Codex — both daemon-driven, and therefore already holding the right
  root. The hook now reads the workspace the daemon persisted. Cursor durability
  lands at PATH granularity, since the line ranges come from the git commit
  rather than from us.

- **Cursor's routing sentinel was recorded as a model.** `model_id` of `default`
  is rejected rather than stored, and a real model already on the session is
  preserved across hook claims that carry no model at all. Stop-token hooks stay
  deliberately unregistered, which is now documented rather than merely true.

- **A second `promptster-teams start` captured nothing, and said "already
  running".** The single-instance lock lives at `<state>/watch.lock` — one per
  user account, not one per directory. An engineer with two checkouts who ran
  `start` in both got a message that reads like success from the second one,
  while every session in that tree was classified as a working-directory
  mismatch and dropped. Nothing surfaced it: `status` showed the first
  directory, the daemon was healthy, and the events simply never existed.

  A second `start` now **registers** its directory with the running capture
  instead of declining, and the daemon picks it up on its next poll — no
  restart. `start`, `login`, and `status` print every watched directory, since
  widening what leaves the machine should never be silent. The Codex matcher
  went multi-directory alongside the Claude one, which had been the only half
  that honored more than a single root. Sessions in the newly added directory
  that were *already* classified as mismatches are re-checked rather than
  staying dropped — otherwise the fix would do nothing for exactly the person
  who needs it, someone who notices the gap only after the fact.

  One lock is still correct: two watchers would double-count presence and
  corrupt seat utilization. What was missing was a way to say "also capture
  here".

- **A fresh install re-ran a one-time cache migration on its first reload.**
  The watcher progress files were written with schema `v0` when no file existed
  yet, so the next load treated a brand-new install as a legacy one and dropped
  its whole classification cache for nothing.

- **The AI-line ledgers reported human code as AI, and reported lines as
  surviving at a path they had left.** Both are FABRICATION rather than loss —
  they corrupt the only numbers `durability_verdict` and `rework_verdict` exist
  to report — and both were reachable through ordinary git usage.

  Seeding a path is deliberately first-touch-only, so that a later human rewrite
  of an AI line is never re-attributed as fresh AI. But a path *leaves* the
  ledger by three routes — every tracked range churned, every range matured into
  a durable verdict, or the file renamed away — and each of those deletions
  re-armed the first-touch branch, so the next purely-human commit to that path
  was seeded as fresh AI off stale AI-path evidence. "First touch" is now a
  property of the repo, not of the ledger's current contents: every route out
  leaves a mark, and the path-level seeding fallback refuses to fire while one
  stands. Line-precise fingerprint transfer — the only carrier of lineage through
  a squash-merge — is deliberately unaffected.

  A pure rename emits no `@@` hunks under the tracked path, so its spans were
  neither moved nor churned: they sat at a path that no longer existed and
  matured into a `durable` verdict for lines that had stopped existing there
  weeks earlier. Renames are now read out of the diff git already emits and the
  spans carried to the new path with their birth timestamp and lineage intact, so
  a rename no longer restarts the 30-day clock or breaks lineage. `copy from` is
  deliberately ignored — a copy leaves the source file in place.

  Two related corrections in the pre-merge rework ledger: its state is now bound
  to the branch it describes, so `git switch -c` off a feature branch or a
  per-branch worktree expires it instead of carrying stale ranges across (the
  clear on returning to the default branch never fired in either case); and a
  pre-merge commit that only *deletes* files now emits its rework verdicts
  immediately rather than stranding them until merge.

  Once a path has been seeded, mere *presence* in the 7-day AI-paths cache never
  re-seeds it again, however long it has been out of the ledger — that presence
  is the evidence already spent. Only a strictly newer per-path AI **write**
  re-opens it, and that re-entry takes a fresh lineage and a fresh birth stamp.
  This cuts both ways and both directions were wrong before: the previous mark
  was renewed on every blocked attempt, so a file the AI kept working on kept
  burying itself, and every AI contribution to it after the first `durable`
  harvest went unreported.

- **An adopted branch's AI lines read as though they were never written.**
  Rework state describes one branch and is dropped when a root switches to
  another, while the attributed-commits ledger is keyed by SHA alone and shared
  across roots. Both are right on their own, and together they lost data: the
  commits that would rebuild an adopted branch's spans are exactly the ones the
  loop skips as already attributed, so the branch came back holding nothing and
  every later rewrite of its AI lines emitted no `rework_verdict`. Two ordinary
  moves reached it — park on the default branch and come back, or run several
  worktrees against one repository.

  Those commits are now re-folded for their STATE alone on a root that owes the
  rebuild: nothing is re-attributed, no fingerprints are re-recorded, and no
  verdict is re-emitted. The obligation is recorded on disk rather than inferred
  from a single poll, because a branch's commits arrive over several polls under
  the per-poll caps — an authorization that expired with the first poll left the
  rest of the branch either silently missing or, worse, holding spans positioned
  mid-branch for the next commit to churn into a verdict about lines nobody
  wrote. Reports move for existing branches: spans reappear, and rewrites that
  previously emitted nothing now emit the verdict they always should have.

- **A new local copy of a repository showed none of the branch's existing AI
  history.** A fresh `git worktree add` is a new absolute path, which is a new
  watcher root, which cold-starts: the cursor is baselined straight to the
  branch's head and none of its existing commits are ever surfaced. The branch
  adoption that PR #128 added still fired — a new root has no recorded branch —
  so the root declared it owed a rebuild and then finished it in the same poll
  having rebuilt nothing. The copy read as though the branch held no AI work at
  all, and every later rewrite of its AI lines emitted no `rework_verdict`. It is
  the same silent under-report as the adopted-branch loss above, reached through
  cold start instead of through the already-attributed skip, and running several
  worktrees against one repository is the ordinary move that hits it.

  A cold-started root now re-folds its branch's own commits (`default..HEAD`) for
  their STATE alone, on the same terms as the adoption rebuild: nothing is
  re-attributed, no fingerprints are re-recorded, and no verdict is re-emitted.
  It is gated on this device *already holding attribution* for at least one
  commit in the range, which is what leaves a genuinely fresh install untouched —
  a new machine has nothing attributed, so the range is dropped without a single
  `git show` and cold start stays the silent baseline it has always been. This
  recovers history the device measured through another copy; it does not import
  history it never saw.

  The range is taken whole or not at all. A branch further past its default
  branch than the per-root cap allows is refused rather than replayed in part,
  because a partial rebuild has to start mid-branch and both ends of that cut
  invent rather than lose: the older end leaves spans positioned a commit behind
  head, where they address whatever lives there now, and the newer end makes the
  first commit replayed look like a first touch, so a human's added lines on an
  AI-touched path are seeded as AI. Those branches keep the old behaviour and
  under-report, which is the direction these ledgers always resolve toward.

  **What this delivers is the route, not yet the number.** The cold-start path no
  longer baselines past a branch's existing commits, so the rebuild now RUNS
  where it previously did not, and it runs against the branch's head rather than
  mid-history. Because AI-path evidence is still keyed per checkout, a genuine
  second directory replays the right commits and finds no AI ranges to seed, so
  those spans do not come back until that keying gap is closed — which is tracked
  as its own task. Nothing is re-sent for commits already accounted for either
  way, so no total can rise from double counting.

- **An ordinary `git merge sidebranch` counted the merged-in edits twice, so
  RECORDED REWORK FIGURES MOVE DOWN for anyone whose history contains merges.**
  Every commit is read with `git show -m --first-parent`, so a merge's own diff
  already carries everything the second-parent side brought in — while the range
  the watcher walks (`rev-list <cursor>..HEAD`) returns the merge AND the side
  branch's own commits. Each merged-in edit was therefore applied to the AI-line
  ledger twice: once in the side branch's coordinate space and once in the
  merge's. Tracked AI spans drifted by an offset that had already been applied,
  and a later rewrite of the file reported churn over whatever now sat at those
  coordinates. A number that drops after this lands was counting the same edits
  more than once; it was never counting extra work.

  Only the rework FOLD is narrowed, to the merge's first-parent chain. **What
  gets attributed does not change**: a commit reachable only through a merge's
  second parent is still reported exactly as before, because whether work that
  arrived through a merge counts as AI-written is a product question and this is
  not the change that answers it. Where the two ranges now disagree the ledger
  can only lose state, never invent it — a second-parent commit no longer seeds
  its own AI ranges, and a rebuild backed solely by such a commit is declined —
  which is an under-report, the direction these ledgers always resolve toward.

  **The two entries above move figures in OPPOSITE directions, and they are
  independent.** The cold-start entry can only make a copy report MORE, by
  running a rebuild that previously did not run at all; this entry makes any
  history containing merges report LESS, by removing a double count. Neither
  re-sends a commit already accounted for.

  **The merge double-count is fixed in the rework ledger ONLY, and the two
  ledgers therefore disagree on a history containing merges.** Stated plainly,
  because a reader must not conclude from this release that merges are now
  handled consistently:

  - the **rework** ledger (`rework_verdict`, pre-merge iteration) folds the
    first-parent chain, so a merge's edits are counted once;
  - the **durability** ledger (`durability_verdict`, surviving AI lines on the
    default branch) still folds the full range, so its figures still
    double-count on a merge;
  - so on such a history the two ledgers' numbers are not reconcilable with each
    other, and that is a known, tracked gap rather than intended behaviour.

  Fixing durability is a separate change, not an oversight in this one: it
  advances its cursor per commit inside its own ledger transaction, so skipping
  commits there needs a deliberate answer for where that cursor lands. It is
  tracked as its own task.

- **A failed internal `git rev-list` could strand tracked AI spans at stale
  coordinates.** Resolving which commits sit on the merge's first-parent chain
  is a second `rev-list`, and when it failed to run, the result was read as "no
  commit is foldable" instead of "we do not know". The poll then attributed and
  recorded every commit in the range while folding none of them, so the tracked
  spans kept coordinates the recorded commits had already moved — and because a
  recorded commit is never revisited, they stayed there. The next rewrite of
  whatever now sat at those lines emitted a `rework_verdict` for code the AI
  never wrote. That is FABRICATION, the failure mode these ledgers rank above
  every other, and it was reachable from a plain fork/exec failure.

  A probe that cannot run now makes the whole comparison inconclusive: the
  cursor stays where it is and the entire poll — attribution and folding alike
  — retries on the next one. A commit is never recorded as accounted for on the
  strength of a fold that was silently skipped. The same rule closes a narrower
  variant where the two `rev-list` calls were bounded differently, so a commit
  outside one window read as though it were off the chain.

  **That rule is NOT yet enforced everywhere, and the remaining hole is reached
  by an ordinary `git rebase -i`.** While a repository's default branch cannot be
  resolved, or while `HEAD` is detached — which is what a rebase does — the
  watcher deliberately keeps the branch's tracked AI spans rather than wiping
  them, but it folds nothing, while still attributing and recording every commit
  it sees. Those commits are never revisited, so their hunks never move the spans
  that were kept, and a later rewrite of the human lines that shifted into those
  coordinates emits a `rework_verdict` for code the AI never wrote. **The watcher
  polls every 60 seconds, so any rebase taking longer than that lands inside one,
  and an interactive rebase blocks on your editor for far longer than a minute.
  This is routine usage, not a corner case, and the wrong position is permanent
  rather than transient.** It is pre-existing behaviour, unchanged by this
  release, and it is tracked on the same task as the durability double-count
  above — the two are one invariant, that recording a commit and folding its
  hunks must never disagree — to be fixed together in a dedicated pass.

- **`doctor` contradicted the updater it reports on.** Its auto-update line
  restated three facts that `internal/selfupdate` owns, and every one of them had
  since drifted: it promised an update "installs on the next **24h** check" for
  every release after the cadence became 30m; it decided whether an update
  existed by comparing version *strings*, so a machine running **ahead** of the
  published release (a local build, or a release yanked after it shipped) was
  told a newer one was available, and a tag differing only by a `v` prefix read
  as an upgrade; and it matched the opt-out variable without trimming or folding
  case, so `PROMPTSTER_TEAMS_NO_AUTO_UPDATE=TRUE` silenced the daemon while
  `doctor` cheerfully reported auto-update on.

  All three now read from `selfupdate` — `CheckInterval`, `IsNewer` (the same
  predicate that authorizes an actual swap), and `EnvDisablesAutoUpdate` — so the
  screen cannot disagree with the mechanism again. This is more than a copy fix:
  `doctor` is the one command an engineer runs while they *already* suspect
  capture or updating is broken, so a wrong line there sends the investigation
  the wrong way.

  The line also reports the version actually **running** when it is up to date
  (claiming "up to date (0.12.1)" while running 0.13.0 is the same lie inverted),
  and cadences render as `30m` / `5m` rather than Go's `30m0s` / `5m0s`.

### Security

- **Heredoc bodies shipped unredacted from CRLF machines.**
  `findHeredocTerminator` trimmed only space and tab, so a terminator line of
  `EOF\r` never equalled `EOF`. The heredoc read as UNTERMINATED, and
  `scrubHeredocBodies` took its "leave as-is" branch — keeping the entire body
  verbatim. On a CRLF machine the on-device scrub, whose whole purpose is running
  before a command leaves the laptop, did not scrub:

  ```
  in    cat <<EOF\r\nexport const KEY = "sk-live-123";\r\nEOF\r\n
  got   (unchanged — the key ships)
  want  cat <<EOF\r\n<inline-code-redacted>\nEOF\r\n
  ```

  This is a fail-OPEN in the redactor, so the exposure is silent on both ends:
  nothing on the device reports a skipped scrub, and the backend cannot tell a
  body that was scrubbed from one that never needed to be. Fixed by trimming
  `\r` alongside space and tab; the scanner is otherwise untouched, and a CR
  *before* the tag is still not a terminator.

  Found by extracting the functions into a standalone harness and running them
  against the backend's TypeScript mirror (`scrubInlineCode`) over shared inputs,
  rather than by reading either implementation — 5 of 18 cases diverged, 0 of 20
  after. The two sides cannot share source (RE2 has no backreferences and no
  lookbehind, which is why this side scans procedurally), so lockstep means
  identical OUTPUT, now pinned by a differential case table on the backend.

### Upgrading

- **Reported rework figures move upward after this release, and the movement is
  the fix rather than a regression.** A pre-merge commit that only *deletes*
  files now emits its `rework_verdict`s immediately instead of stranding them
  until merge, so verdicts that previously went unreported start being counted.
  Expect a step change in rework rate at the upgrade boundary on any fleet that
  deletes files before merging — it is not a behaviour change in how anyone
  writes code, and it is not comparable to the pre-upgrade series.

- **Cursor attribution starts from zero at the upgrade, not from history.** Every
  Cursor edit before this release was attributed as human, and nothing
  backfills — the ledger keys those edits needed were never written. A fleet
  using Cursor should expect its AI share to rise once the new binary is
  running, and should not read the earlier flat line as a real measurement.

## [0.12.1] — 2026-07-31

### Added

- **`promptster-teams uninstall` — there was no way to undo an install.** As of
  0.12.0 installing enrolls the machine in three places nothing removed: a hook
  entry in `~/.cursor/hooks.json`, a login service (launchd / `systemd --user` /
  Task Scheduler), and — if `statusline enable` was run — a wrapped `statusLine`
  in `~/.claude/settings.json`.

  The package manager cannot do this, which is measured rather than assumed
  (npm 11.5.2, sandboxed global prefix): **`npm rm -g` runs no uninstall
  lifecycle script** — `preuninstall`/`postuninstall` fired zero times on
  removal — **and it does not delete the managed binary**, because that binary
  lives outside `node_modules` by design. So removing the package reported
  success while capture kept running, kept self-updating from GitHub Releases,
  and came back at the next login, from a binary the engineer believed they had
  deleted.

  The sharper failure is the Cursor hook. Delete the binary by hand — the obvious
  move for a curl install — and the hooks.json entry survives pointing at a path
  that no longer exists, so Cursor execs a missing command *inside the engineer's
  agent loop* on every prompt, edit and shell call. That degrades their tool
  rather than our data, and nothing on our side can self-heal it: once the binary
  is gone, none of our code runs.

  `uninstall` reverses all of it — autostart deregistered, capture stopped, the
  Cursor hook unenrolled, the statusline restored verbatim — and touches no
  config it did not write. Every step runs even if an earlier one fails, since
  short-circuiting is exactly how the hook entry would survive a removal. It
  reports what it observed rather than what it attempted, and capture that is
  still alive after the stop is a failure with a non-zero exit, not a success
  line. Without `--purge` it deletes no data; `--purge` also removes
  `~/.promptster-teams` (key, unsent event queue, and the managed binary).

### Fixed

- **`statusline disable` no longer resurrects a statusLine you deleted.** If you
  had let us wrap an existing `statusLine` and later deleted the key outright,
  disable wrote your old command back — recreating configuration you had
  deliberately removed. Deleting the key removes our shim as surely as replacing
  it does, so the slot is now left alone (and the stored prior dropped) whenever
  no statusLine is present. Latent since the statusline shim shipped; it matters
  now because `uninstall` runs this on every invocation rather than only when
  someone deliberately manages their statusline.

- **`uninstall` deregisters autostart even when it cannot read the unit's
  status.** `Disable` is idempotent on every platform, so attempting it blind
  costs nothing, while skipping it on a failed status probe left the one residue
  that undoes an uninstall by itself: a registered unit that brings capture back
  at the next login.

### Changed

- `install.sh` and `help` now name `uninstall`, and `help` no longer advertises
  the old 24h self-update cadence (it has been 30m since 0.7.0).

## [0.12.0] — 2026-07-31

### Added

- **Cursor sessions are captured, on two rails.** An engineer working entirely in
  Cursor produced a session row with no `cursor` in `source_service`, which the
  dashboard rendered identically to an engineer who installed the CLI and never
  used AI — two very different facts, one indistinguishable number.

  The primary rail is a **user-scope** hook config, `~/.cursor/hooks.json`: one
  global file outside every repository, enrolled automatically at watch startup.
  Cursor's *project*-scope `<workspace>/.cursor/hooks.json` is never written — it
  is a tracked file inside a customer repo and it enrolls per-repo, so a repo an
  engineer forgets would read as "captured nothing".

  The fallback rail tails `~/.cursor/projects/…/agent-transcripts`, needs no
  enrollment, and therefore covers sessions that predate install and machines
  where hook enrollment failed. A session is captured by exactly one of the two:
  the hook payload names the exact `transcript_path`, so the handoff is an
  identity rather than a guess, and the watcher stands down for a claimed
  transcript while advancing its offset so an expired claim resumes instead of
  replaying.

  Events carry `source: cursor`: `prompt` (human), `file_diff` / `file_create` /
  `file_delete` and `command` (AI), plus `session_start` / `session_end`,
  `tool_result`, and an `ai_response` carrying the model name and nothing else.

  **Counts, never code.** Cursor's hook payload hands over every edit's
  `old_string` and `new_string`, raw command output, and the engineer's email
  address on every single event. Exactly two functions read the edit bodies, one
  per rail, and both return only integers; `RawPayload` is never populated on
  either rail. Four tests pin this and all four were mutation-tested.

  **No token counts are reported, and that is not an omission.** Cursor exposes
  none — not in transcripts, `state.vscdb`, `conversation-search.db`,
  `ai-tracking`, the per-session `store.db`, or any hook payload. The fields are
  absent rather than zero, because a zero would read as "this turn cost nothing"
  rather than "unknown". (#122)

### Upgrading

Nothing to do. An installed daemon self-updates within ~30 minutes, re-execs, and
enrolls the Cursor hooks itself. An existing `~/.cursor/hooks.json` keeps every
entry and top-level key it already had, and one that does not parse is left alone
— capture falls back to the transcript rail rather than overwriting it.

Cursor rows in the dashboard also require the server-side roster flip
(promptster-backend #594); until that deploys, Cursor events are ingested but not
displayed.

## [0.11.4] — 2026-07-31

### Fixed

- **A repeated Codex user record no longer mints duplicate prompts.** 0.11.3's
  human-turn recovery flushed its buffered turn whenever the next line was not the
  authoritative `event_msg`. Some rollouts repeat the user turn on the
  `response_item` channel — three copies within a millisecond for a single turn —
  and each repeat read as "the turn moved on", producing one prompt per copy. A
  line carrying the same user text as the buffered turn now claims that turn
  instead of flushing it, so one turn yields exactly one prompt; a repeat with
  different text is still a distinct turn. Measured on live data: zero
  millisecond-apart duplicate prompts across 3,291 captured before, 38 across the
  405 captured after 0.11.3, codex only. (#121)

## [0.11.3] — 2026-07-30

### Fixed

- **Codex sessions no longer lose the engineer's own prompts.** The rollout
  normalizer read human turns only from the `event_msg`/`user_message` record and
  skipped the `response_item` copy as a duplicate. On a host that writes only the
  latter, every human turn vanished: one org captured **zero** prompts across 29
  sessions and a full day of work — 1,400+ tool calls and 400+ file diffs, and not
  one prompt — so the fluency dashboard graded an engineer on nothing they had
  written. The `response_item` copy is now taken whenever the `event_msg` never
  arrives. Because Codex writes that copy one line *ahead* of the authoritative
  record, it is held for exactly one line and discarded the moment the `event_msg`
  lands, so hosts that emit both are completely unaffected — replaying real
  rollouts through the old and new normalizer produces byte-identical output.
  Codex's own injections into that channel (`<environment_context>`,
  `<user_instructions>`, `<recommended_plugins>`, `# AGENTS.md instructions for`)
  are dropped rather than mistaken for prompts, and delegated subagent threads stay
  excluded exactly as before. (#118)

## [0.11.0] — 2026-07-23

### Fixed

- **Codex sessions that began before the capture daemon started are no longer
  dropped.** The Codex rollout watcher now uses the same go-forward capture window
  the Claude watcher adopted in 0.8.0: a session already in progress when the
  daemon launches — or relaunches after a laptop sleep/restart — is captured from
  that point forward, instead of being silently ignored because its first event
  predated the watcher. Previously only Codex sessions started *after* the current
  daemon launch were captured, so long or pre-restart sessions were lost fleet-wide
  (worst on sleep-heavy laptops running long sessions). Capture is go-forward only:
  a pre-existing session is tailed from the daemon's start point, and its earlier
  history is not re-uploaded. A one-time progress-file migration also clears stale
  "skip" decisions cached by the buggy watcher so previously-dropped sessions
  re-qualify. (#109)

### Added

- **Sessions now report whether the working directory was a git repository at all
  (`repoTracked`).** The repository identity has two fallbacks that produce an
  identical-looking opaque hash: a real git repo with no `origin` remote, and a
  directory that is not a repo at all — a home directory, or a container folder
  someone launched the agent from one level too high. Nothing downstream could
  tell them apart, so non-repositories occupied rows on a board titled "Top
  repos" under a 16-character hash no human can read. The bit that separates the
  two cases was already being computed on-device and thrown away one line later;
  it is now reported. `repoRoot` keeps its exact current value in every case —
  this adds information *about* the key, it never changes the key, and that is
  pinned by a test which re-runs the previous resolution and compares byte for
  byte. The field is emitted **explicitly as `true` or `false`** whenever a
  `repoRoot` is emitted, and omitted entirely when one is not: absent means "this
  CLI did not look", which is a different statement from "this is not a repo",
  and a positive-only emit would collapse the two back together. What leaves the
  device is one boolean about the filesystem — no path, no directory name.

### Fixed

- **The same commit was being attributed over and over.** The git watcher kept a
  per-repository cursor answering "has HEAD moved since I last looked here",
  which is not the same question as "have I already reported this commit".
  Whenever a cursor became unreachable — a rebase, a `gc`, or a deleted worktree
  — the watcher fell back to re-listing the newest commits wholesale and sent
  every one of them again, with nothing to suppress the repeat. On a machine
  that creates and deletes worktrees continuously this is not an edge case: of
  the 63 repository roots discovered on the measured device, 41 were worktree
  directories that no longer existed. The result was ~126,000 attribution posts
  carrying ~4,200 distinct commits over two days — roughly 30× redundant — which
  inflated one session's stored context to 41,000 rows and pushed its queries
  past 30 seconds. The watcher now keeps a ledger of the commits it has already
  attributed, keyed by SHA alone: a commit is content-addressed, so the same SHA
  reached through a second worktree of the same repository is the same commit,
  and is reported once. The ledger expires on the same horizon the watcher uses
  to discover repositories, so a repository can never still be polled after its
  commits have been forgotten, and it is hard-bounded at 20,000 entries with
  oldest-first eviction. Skipping covers rework as well as attribution —
  re-running rework over a commit already accounted for would double-count
  churn. Nothing about *what* leaves the device changes; this only stops the
  same facts being sent repeatedly.

## [0.10.0] — 2026-07-22

### Security

- **Secret names with a prefix reached the wire in plaintext.** The generic
  `KEY=value` and `"key": "value"` redaction rules were anchored with `\b`, and
  `_` is a word character — so the anchor could never match inside a prefixed
  name. `API_KEY=…` was masked; `STRIPE_API_KEY=…`, `ACME_DB_PASSWORD=…`,
  `MY_CLIENT_SECRET=…` and `"STRIPE_API_KEY":"…"` were **stored in the clear**
  unless the value happened to carry a vendor shape the scanner recognizes on
  its own (`ghp_`, `sk-ant-`, `AKIA`). An org-internal secret with an opaque
  value — the common case in a customer `.env` — matched nothing. Both rules now
  accept a name prefix. Deliberately no trailing wildcard: `TOKEN` is a prefix of
  `TOKENS`, and `*TOKEN*` would redact `MAX_TOKENS`/`INPUT_TOKENS` and destroy
  your spend telemetry to mask a number. Redaction also now runs in three ordered
  stages (precise vendor shapes → the wide scanner → the shape-blind generic
  fallbacks), because the marker is the only provenance left once a value is gone
  and the old order destroyed it in both directions — a partial vendor match left
  a bare `sk-` on the wire, and the generic rules rewrote an already-attributed
  `[REDACTED_ANTHROPIC_KEY]` back down to `[REDACTED]`. `[REDACTED_LLM_KEY]` is
  now split into `[REDACTED_ANTHROPIC_KEY]` / `[REDACTED_OPENAI_KEY]` so a
  dashboard can say *whose* key was pasted; consumers accept both spellings, so
  older CLIs in the fleet are unaffected.
- **Secrets pasted mid-sentence were not caught.** A webhook secret in prose —
  "use this webhook secret to check my db: `whsec_…`" — went through the
  redactor untouched. The bug was invisible from the rule list, because every one
  of these shapes *is* caught the moment it appears as `NAME=value`: the
  assignment rules mask on the name alone, so testing the way a config file looks
  showed full coverage. Prose is the form a human actually pastes. A 31-shape
  probe across the real pipeline found 7 bare leaks; four are now caught by
  value-shape alone — `whsec_` (Svix/Supabase/Stripe webhook signing), `hf_`
  (Hugging Face), `sntrys_` (Sentry), `secret_` (Notion). All four collapse to a
  single `[REDACTED_SECRET_KEY]` marker, which grades **critical** downstream
  (unlike the generic `[REDACTED]`, which fires on a *name* with an unverified
  value — these fire on a value shape only one thing produces). The remaining
  prefixless shapes (AWS secret access key, Datadog, Twilio API key secret) are
  left uncaught **on purpose** and pinned by a test that fails if someone
  "fixes" them: catching a shape with no prefix needs a bare-entropy pass that
  collapses `msg_`/`toolu_`/`call_` provider ids and breaks turn dedup.

### Added

- **Credential-file reads now name the keys, not just the file.** "`acme-api/.env`
  was opened" is a notification; "…and it holds `STRIPE_SECRET_KEY` and
  `DATABASE_URL`" is a rotation list. Reads of the dotenv family and
  `~/.aws/{credentials,config}` now emit the credential **key names** found in
  the body. **Values can never be harvested** — every parse discards the
  right-hand side at the call site, so it is a structural property of the code
  rather than a careful one, and the outbound projection additionally bounds the
  count, length and character shape of every name (a name that fails is dropped
  whole, never truncated — a truncated secret is still a secret prefix).
  Placeholder dotenvs (`.example`, `.sample`, `.template`, `.dist`) are skipped.
  A file that yields no usable names reports **nothing at all** rather than an
  empty list, so "this CLI did not harvest" never reads as a fabricated
  all-clear. `.npmrc` and `.pypirc` are deliberately not harvested: npm's only
  key that matters (`//registry.npmjs.org/:_authToken`) cannot pass the name
  filter, and `.pypirc`'s keys are `username`/`password`, which the file path
  already told you.
- **Sessions now report the git host beside the repository slug (`repoHost`).**
  The canonical slug added in 0.9.2 discards scheme and host by design, so
  `gitlab.com/acme/api` and `github.com/acme/api` both reduce to the identical
  string `acme/api`. Without a host, deciding whether a repo belongs to your
  connected GitHub org has to treat a colliding owner name as a match — which at
  a GitLab or Bitbucket shop misclassifies repos for every engineer at once. Two
  invariants hold: a host is never reported without a slug, and it is non-empty
  **only** when the repo has a real remote (the opaque-hash cases report `""`
  rather than guessing). Resolved in the same single `git config` spawn as the
  slug, so there is no added cost. What leaves the device is a provider name and
  structurally cannot be a path, a URL, or your OS username.
- **The config census now reports *where* project memory sits, not just how big
  it is.** Claude Code loads `CLAUDE.md` at or above the working directory **at
  launch**; a file nested in a sub-package loads lazily — only once the agent
  first reads something in that directory — and is not re-injected after
  `/compact`. One token count was being read by two dashboard metrics that need
  opposite answers for that repo: coverage ("does this repo have project
  memory?" — yes) and the context tax ("what standing per-turn cost?" — close to
  none). Each census entry now carries `root` / `nested` / `absent` alongside the
  count, so covered-but-latent memory stops being billed at always-on rates and
  can be called out for what it is. Position tracks what was counted, not how
  deep the path looks: a sub-package that is itself a watched root reports
  `root`, because a session started there really does load it. Derived from paths
  already walked — no file contents are read or transmitted.
- **Durability is now measurable before day 30.** `durablePct` was 0% by
  construction on every install and would have stayed there for a month: a
  durable verdict only emits after a line has survived 30 days, while churn
  emits at commit time, and the 30-day clock starts when the CLI first *sees* a
  line — i.e. at install — not when the line was authored. A healthy team read
  "0% durable / 100% churned". The watcher now emits a throttled (24h) inventory
  of the AI-authored ranges still alive and undecided, as a third range array on
  the existing durability event. This is purely additive: inventorying does not
  consume a span, so the real 30-day verdict is untouched, and the ledger format
  gained a field without a version bump (bumping it would make the reader discard
  the file and wipe every in-flight lineage's birth timestamp on fleet upgrade —
  re-inflicting this exact bug on purpose). The first AI line on a fresh install
  reports immediately rather than up to 24h later.

### Fixed

- **Subagent delegations stopped being captured when Claude Code renamed its
  delegation tool `Task` → `Agent`.** The normalizer branched on the old name, so
  the rename did not throw — it stopped matching, and every spawn fell through to
  a generic tool-use record. Measured in the live database on 2026-07-22:
  **zero** delegation events for all time, against 658 `Agent` tool-use rows
  across 104 sessions. Any dashboard counting subagent usage read 0 for every
  session. `Task` is still matched for older clients in the field. The same case
  also emitted its payload under a field name that has never been carried by
  either the on-device or the server-side allowlist, so fixing only the tool name
  would have shipped delegation events with an **empty** body, stripped silently
  with no error. It now emits the task description preview and the subagent type
  under already-sanctioned names; the full delegated instruction is never
  emitted.
- **Modern Codex rollouts are captured again.** Current Codex versions wrap tool
  calls in generic `custom_tool_call` / `exec` envelopes that differ from the
  older transcript shapes. Local normalization now understands those envelopes,
  correlates a detached command with its later completion, and preserves safe
  metadata for unknown tools without emitting any content-bearing field. Also
  aligns Go's canonical-JSON signing with JavaScript `JSON.stringify` for
  HTML-sensitive punctuation, so events containing `<`, `>` or `&` verify
  correctly server-side. Validated end to end by replaying a real local rollout:
  104 events through normalization, redaction, projection, signing, ingest and
  worker analysis, all accepted and verified.

## [0.9.2] — 2026-07-20

### Added
- **Canonical per-session repository identity (`repoRoot`).** Each session now
  emits the repository it ran in as a stable `owner/name` git-remote slug (or, for
  a repo with no remote / a non-git directory, a non-reversible opaque hash),
  independent of how deep in the tree you were — repo root, a subdirectory, or a
  worktree all resolve to the **same** identity. This lets the dashboard attribute
  a session to the right repo and join it to merged pull requests exactly, instead
  of guessing from a directory name (which split one repo into several rows and
  missed the PR count when you worked from a subdirectory). Only the public
  `owner/name` slug or a non-reversible hash leaves the device — never a
  filesystem path. `workdir` is unchanged (it still identifies the specific
  checkout/worktree).

### Fixed
- **Config census now reports one entry per repository, not one for your home
  folder.** The autostart daemon runs with its working directory set to `~`, so a
  single `config_census` was emitted keyed to your home directory — which is not a
  repo — leaving per-repo CLAUDE.md/skills/MCP coverage reading empty (0%). The
  daemon now discovers the repositories you actually work in and emits one census
  per repo, each keyed to its canonical identity; a home directory with no repo
  yields a device-only census rather than a fake pseudo-repo.
- **Commit attribution now works in the autostart daemon.** The 0.9.0 git
  watcher (`commit_attribution`, `durability_verdict`, `rework_verdict`) assumed
  its workspace *was* a single git repo. The installed daemon's workspace is your
  home directory, which is not a repo — so the watcher polled only `~`, detected
  no commits, and the durability track never fired outside the standalone
  `git-watch` subcommand. The watcher now discovers the repos you actually work
  in from the AI-touched-paths ledger (a stat-only walk to each path's `.git`
  root — no git spawns, off the 60s-timer's constant-time budget) and reconciles
  each commit's attribution against that ledger through a workspace→repo path
  translation. AI evidence recorded home-relative now matches commits in the
  sub-repo that produced them; a file with no AI evidence stays `unknown` as
  before. No change to what leaves the device (repo-relative paths, line ranges,
  SHAs, workspace key — never contents).

## [0.9.1] — 2026-07-18

### Fixed
- **Stop the config census from walking your home folder (macOS permission
  prompts).** The autostart daemon runs with its working directory set to your
  home directory and no explicit workspace, so its workspace fell back to `~`.
  The nested-CLAUDE.md scan added in 0.9.0 then walked that root five levels
  deep — which on macOS enumerates `~/Documents`, `~/Downloads`, `~/Music`, and
  the other protected folders, triggering "promptster-teams wants to access your
  Downloads/Documents/Music" consent prompts from a tool that only ever *stat*s
  file sizes (no contents are read or transmitted). The scan now descends only
  into actual git repositories (gated on a `.git` at the root), so a non-repo
  workspace like a home directory is never walked. A one-time stat per root;
  legitimate watched repos are unaffected. If you granted any of those folder
  permissions, you can revoke them under **System Settings → Privacy &
  Security → Files and Folders** (and **Music**).

## [0.9.0] — 2026-07-18

### Added
- **Per-commit AI line attribution.** A new periodic git watcher emits a
  `commit_attribution` event per commit on the watched repos, recording which
  committed line ranges were AI-authored. Ranges are reconciled against the
  *real committed diff*, so a silent formatter hook that reflows AI lines
  doesn't lose the attribution. Events carry repository-relative paths, integer
  line ranges, the commit SHA, and a workspace key (the origin remote's
  `owner/name` slug, or a local path hash when there is no remote) — never file
  contents or diff bytes. Runs on a 60s timer off any latency-sensitive path.
- **AI-line durability.** AI-authored lines are followed forward on the default
  branch; once a line survives 30 days it emits a `durability_verdict` — a
  measure of how much AI code actually persists rather than getting reverted.
  Lineage follows through squash-merges, cherry-picks, and rebases via on-device
  content fingerprints — the fingerprints themselves never leave the machine;
  only line ranges, SHAs, the `sha:path` lineage handle, and the workspace key
  are emitted (never file contents or diff bytes).
- **AI-line rework.** On a pre-merge branch, a `rework_verdict` emits the moment
  AI-authored lines are churned or rewritten before they land, measuring
  pre-merge iteration on AI output. No maturity window; carries line ranges, the
  commit SHA, the path, and the workspace key — never file contents or diff
  bytes. The ledger clears when the branch merges back to the default branch.
- **Codex per-turn model + reasoning tokens.** Codex rollout normalization now
  attaches the exact per-turn model from `turn_context` and carries OpenAI's
  reasoning-output token count through the privacy projector. The count is
  attached only when the provider actually reports it — a turn that reports no
  reasoning tokens omits the field rather than sending a fabricated zero.

### Fixed
- **CLAUDE.md coverage no longer reads 0% for monorepos.** The config census
  looked for `CLAUDE.md` only at the workspace ROOT, but Claude Code discovers it
  hierarchically — a repo may keep its memory in a sub-package (e.g.
  `my-clerk-next-app/CLAUDE.md`). Any such repo reported zero project-CLAUDE.md
  tokens, which the dashboard's cc-audit "CLAUDE.md coverage" check scored as 0%
  even though the workspace carried a healthy memory file. The census still
  reports the always-loaded root `CLAUDE.md` when present (so repos that already
  worked are unchanged); only when no root file exists does it fall back to the
  largest `CLAUDE.md` nested in a sub-package (bounded depth; skips
  dependency/build/vendor and hidden trees, incl. `.claude/worktrees`). Sibling
  packages' files are never summed — they don't co-load on one request.
  Stat-only as before — no file contents leave the machine. Takes effect on the
  next census (watch start, or within 24h of a running watch) after upgrading.

## [0.8.0] — 2026-07-17

### Fixed
- **Stop dropping sessions that started before the watcher launched.** The daemon reset its
  capture window on every start, and the LaunchAgent restarts on every laptop sleep/wake — so
  any long-running, resumed, or restart-spanning Claude Code session was classified as
  out-of-window and silently never captured. A heavy user with few long sessions could show up
  almost entirely uncaptured while a many-short-sessions user looked fine. Such sessions are now
  captured go-forward from the point the watcher first sees them (from current end-of-file, so no
  pre-existing history is re-uploaded), regardless of when they started.

### Added
- **Capture-health beacon.** The config census now reports two content-free counts — the total
  number of Claude Code transcript files on disk and how many were active in the last 7 days
  (stat-only: no paths, filenames, or repo names ever leave the machine). This lets the dashboard
  distinguish an engineer whose capture is broken (transcripts on disk, nothing ingested) from one
  who simply isn't running Claude Code locally. On an unreadable transcript tree the counts are
  omitted rather than reported as a misleading zero.

## [0.7.0] — 2026-07-15

### Changed
- **Updates now land in ~35 minutes instead of ~24 hours.** The check used to GET
  `api.github.com/repos/.../releases/latest` — a ~20KB JSON response behind a **60/hr
  unauthenticated per-IP limit**. Behind a corporate NAT an entire fleet shares that one
  IP, so the interval could never drop without exhausting the quota and starving the very
  update it was chasing. That limit, not the release process, is why the cadence was 24h.
  The tag now comes from the `releases/latest` **redirect** on `github.com`, which is
  CDN-served and carries no rate limit at all, so the cadence is free to be 30m. `doctor`
  reads the same redirect — it is the one command engineers run repeatedly *while
  something is already wrong*, so it is the last place that should share a rate-limit
  budget with the daemon.
- **An npm install now downloads ~12MB instead of ~74.5MB.** The package shipped all six
  platform binaries to every engineer. The binary now arrives as a per-platform
  `optionalDependency` gated on npm's `os`/`cpu` fields, so only the host's match is
  fetched. The wrapper itself is 18.4kB.

### Added
- **`promptster-teams autostart repair`** re-points an existing launchd/systemd/scheduled
  task at the binary running now. npm's postinstall calls it automatically. See the
  autostart fix below for why it exists.

### Fixed
- **`npm ls` and `npm outdated` lied about the installed version.** Self-update renames a
  verified build over its own executable; when that executable lived inside
  `node_modules`, npm's metadata went stale the moment the daemon updated, and a reinstall
  wrote the older binary back. The binary now installs to `~/.promptster-teams/bin` — the
  same path `install.sh` writes — and npm's copy is never mutated, so npm's metadata is
  correct by construction. Project-local installs are left alone entirely: a lockfile is a
  deliberate pin, and self-update no longer touches a copy it selected.
- **Autostart pointed at a binary that upgrading deletes.** `autostart enable` bakes an
  absolute path in once and nothing revisits it, so units enabled before this release name
  a path inside `node_modules` that no longer exists. Nothing failed loudly — the running
  daemon holds its inode, so the upgrade looks clean and capture only dies at the **next
  login**, which is precisely the failure autostart exists to prevent. postinstall now
  repairs the unit during the upgrade.
- **The "update available" hint installed a different product.** When the install
  directory was not writable, a curl-installed engineer was told to run
  `curl -fsSL https://get.promptster.ai | sh` — the **hiring** CLI's installer, which
  fetches `promptster` into `~/.promptster/bin` and leaves `promptster-teams` exactly as
  stale as it was. It now names this repo's `install.sh`, and only a binary already at the
  managed path is told to re-run it: `install.sh` writes one fixed path, so telling a
  root-owned `/usr/local/bin` copy to re-run it just drops a second binary and lets `PATH`
  pick the winner. Anything else is told which file to replace and stops there.
- **`planning` had zero rows, ever.** Claude Code renamed `TodoWrite`/`TodoRead` to
  `TaskCreate`/`TaskUpdate`/`TaskList`; the normalizer matched only the old names, so the
  kind never fired while the agent kept planning as much as ever. A rejected `Task` call
  no longer records as planning either — `tool_input` holds what was *asked for*
  regardless of outcome, so a failed create used to invent a plan.

## [0.6.1] — 2026-07-15

### Fixed
- **Bash mode was shipping command output off the machine.** The `!` prefix
  writes both the invocation and its captured stdout/stderr into the transcript
  as user lines, so `<bash-input>` / `<bash-stdout>` / `<bash-stderr>` were
  leaving as `prompt.text` — shell commands, absolute filesystem paths, infra
  hostnames and raw output. This is the exact category the redaction projector
  exists to exclude ("Command-family: invocation + result metadata — never
  stdout/stderr"), and none of the three layers caught it: the projector
  allowlists `prompt.text` because it is the product, `scrubInlineCommand` only
  runs on shell-command kinds, and the source-exclusion DB constraint guards a
  `stdout` *key*, not stdout *inside* text. Secrets were still redacted upstream,
  so no credential left the machine. They are now dropped at capture rather than
  filtered later: source exclusion is a guarantee, not a preference the server
  may revisit, and a source-bearing line that reaches the buffer has already
  broken the promise.

### Added
- **`promptSource` is now emitted, so the server can tell a turn you typed from
  one the harness injected.** Claude Code writes background-task notifications
  into the transcript as ordinary `user` messages; they are indistinguishable
  from prompts by shape, and roughly a quarter of captured "prompts" are these.
  Nothing downstream could separate them, so they were being graded as an
  engineer's own weak prompting. The CLI deliberately does **not** drop them: a
  client-side drop is irreversible and bakes into every installed build forever,
  while a server-side filter can change its mind. Ship the signal, not the
  verdict. The value is shape-clamped to a short lower-snake token rather than
  matched against a fixed vocabulary, so an unknown future value is carried
  without waiting for a CLI release — and a value that is not enum-shaped can
  never reach the wire.

### Removed
- **The unused `meta` map on prompt events.** It assembled `ideSessionId`,
  `permissionMode`, `promptId` and `cwd` — an absolute filesystem path — and was
  stripped before the buffer, the signature and the wire, so nothing ever
  received it. It had already been removed once and grew back. The projector
  allowlists *keys*, so a map can only ever be kept whole: keeping any field
  inside it would have taken `cwd` along. Fields that are genuinely needed now go
  on the envelope or get their own individually-allowlistable key.

## [0.6.0] — 2026-07-14

### Added
- **An org `minCliVersion` floor can escalate the update cadence for an emergency
  fix.** While the running version is below the org's `minCliVersion`, the
  updater re-checks every 15 minutes instead of every 24 hours. Normal releases
  keep the 24h stagger, which is the only canary window that exists — self-update
  is forward-only, so a bad release cannot be recalled. The floor moves the
  *cadence* only: the org auto-update switch and any version pin are still
  enforced, so a floor can neither override an opt-out nor drag a pinned fleet
  past its pin, and it never changes which tag gets installed. The 15m is a retry
  floor rather than a target — a fleet below the floor that cannot update
  (upstream down, release yanked) would otherwise re-hit the releases API every
  poll and exhaust the 60/hr per-IP limit, starving the very update it is
  chasing. The CLI has to understand the field before a backend can ever send it,
  so this ships ahead of the server side and only works on versions from here on.
- **`doctor` now reports delivery-queue health, so a stuck send queue is visible
  where engineers actually look.** The durable send queue drains in the
  background and shouts about failures on stderr — which lands in `daemon.log`,
  which nobody tails. A revoked key could therefore 401 every upload for days
  while `doctor` cheerfully reported "Ready". It now checks the queue, and is
  careful about when it complains: a raw pending count is not a health signal,
  because a machine that captured events and then stopped watching legitimately
  holds a backlog forever. Doctor warns only when the queue is *not draining
  while something is supposed to be draining it* — using the cursor's mtime as
  the progress probe, and falling back to the watcher's start time when no cursor
  exists at all (delivery that has never once succeeded, i.e. exactly what a
  revoked key looks like). A backlog with no watcher running is reported as the
  normal idle state, not a problem, and liveness is judged by a watcher's
  heartbeat rather than by a supervisor pidfile whose PID the OS may have
  recycled. It also warns at 75% of the queue's 64 MB cap and reports an error at
  the cap, where events are being dropped outright. The check is a diagnostic: it
  stats files and never advances the cursor, compacts the queue, touches the
  ledger, or sends anything.

### Fixed
- **The dashboard reported 1 session while 7 were running.** The envelope's
  `sessionId` was actually the *device* id — `loadSession` set it from
  `DeviceID()` and every watcher handed that to its normalizer, so all concurrent
  sessions on a machine reported as one, and always had. Session ids now come
  from the transcript each watcher tails, derived from its path so a processor
  knows its session before reading a line. The real session id *was* being
  captured into `data.meta.ideSessionId` and then silently dropped, because the
  projector allowlists no `meta` key; identity now lives on the envelope, which
  the projector cannot touch. `deviceId` ships as a separate unsigned envelope
  field sourced from the environment, so the two cannot re-conflate. This also
  defused two landmines that had not gone off yet: presence data read the
  envelope session id, so every watch restart would have looked like a new device
  and inflated seat counts, and the ai-paths ledger held a single session and
  wiped itself on a new id, so concurrent Claude and Codex sessions would have
  erased each other.
- **`stop` reported success while the OS supervisor quietly revived capture.**
  With autostart enabled, `stop` signalled the watcher's PID but left the
  launchd/systemd job loaded and its restart policy armed. The 2s SIGINT→SIGKILL
  budget was also shorter than a single 5s-timeout ingest send, so a busy watcher
  got SIGKILLed — which the supervisor read as a crash and revived, verified live
  on macOS at ~2s. `stop` now disarms the supervisor before signalling, the grace
  window is 8s (guarded by a test asserting it exceeds the ingest client
  timeout), watchers handle SIGTERM as well as SIGINT so supervisor-driven
  teardown still runs their state cleanup, and the final report is derived from
  an observed post-state rather than from intent.
- **The same transcript was captured twice, and the duplicates blew the ingest
  rate limit.** Watcher progress was keyed by absolute path, but one Claude Code
  session is reachable under several `~/.claude/projects/` slugs — a git-worktree
  slug and the bare repo slug — and the file moves between them when a worktree
  is removed. Each slug looked like a brand-new file, so the watcher re-read it
  from offset 0 and re-emitted the whole transcript: 25 tracked paths did exactly
  this, sending 2,182 events twice (~32% of all traffic) and pushing a real
  rolling-60s peak of 105 against the 100/min cap. Progress is now keyed by the
  transcript's identity — its slug-relative path, i.e. the globally-unique
  session UUID — so every alias of one transcript shares one offset. Existing
  progress files are re-keyed on load, keeping the highest offset on collision so
  the upgrade itself can never re-emit. The rate limit is correct and has not
  been raised.
- **Rate-limited and failed events were destroyed, not retried.** The parse loop
  POSTed inline and advanced the transcript offset regardless of the outcome, and
  there was no retry anywhere in the CLI — so every 429, 5xx, timeout and offline
  moment permanently dropped the event (653+ lost to 429s alone). Parsing and
  sending are now separate: the parse loop appends to a durable on-disk queue, and
  advancing the offset is safe because the queue, not the network, is what
  remembers. A background drain delivers in order from a persisted cursor,
  honouring the backend's `Retry-After` on 429 and backing off exponentially with
  jitter on 5xx/network errors. Only a 2xx or a permanent 400/422 rejection
  advances the cursor. A head-of-queue that keeps failing — a revoked engineer
  key, an unreachable API — is now reported loudly instead of retrying in silence.
- **A network outage masqueraded as a broken parser.** The degraded-watcher
  detector exists to notice the transcript format changing under us, but it was
  fed a *send* count, so a total delivery failure looked identical to a dead
  parser (`degraded — 271744 bytes consumed`). It then handed capture to the
  hooks, which only cover the live tail and cannot replay — so the outage window
  died twice. With delivery moved off the poll loop the detector counts real
  parses and is unaffected by the network.
- **The config census is queued rather than fired inline.** It is emitted at most
  once per 24h and its cursor advances whether or not the send lands, so a single
  429 silently cost a full day of census — and fleet health reported "no census"
  for a device that had dutifully collected one. Presence heartbeats deliberately
  stay fire-and-forget: a heartbeat redelivered minutes later is a stale liveness
  claim, and the next one is seconds away.
- **`promptster-teams status` stopped inventing an upload backlog.** "N events
  pending upload" counted the local signed ledger, which nothing drains — so it
  reported every event ever captured as perpetually pending, and "all events
  shipped" could only appear on a device that had captured nothing at all. It now
  counts the send queue, so both states mean what they say.
- **`start` could not launch capture on any install except the curl installer.**
  It spawned its detached `watch` child from a hardcoded
  `~/.promptster-teams/bin/promptster-teams`, a path that only exists for one
  install channel — so npm, pnpm and `go build` users got `fork/exec ...: no such
  file or directory` and could not start capture at all. What to exec is a
  property of the running process, not the install channel, so it is now resolved
  from the running executable (with symlinks resolved, since the npm global bin
  is one). The install-path helper that caused this is no longer reachable as a
  footgun: it survives only as a fallback for a host where the running executable
  cannot be resolved. Autostart had already hit and locally fixed this same bug;
  the two now share one resolver instead of each having their own.
- **`login` started a watcher but never installed the login service**, so capture
  died at the next reboot and never came back. The only signal was an
  "autostart not enabled" warning in `status`/`doctor` that landed directly under
  "capturing in the background (pid N)" and read as a failure rather than a
  reboot gap. `login` now installs the service and says so. A failed enable stays
  a warning: capture is already running, only the reboot guarantee is missing.
  Separately, `status` recomputed the watch dir from its own cwd, so running it
  inside a repo reported that repo while the daemon was really scoped to `$HOME`;
  it now reports the scope live capture recorded at spawn.
- **The "can't update in place" nudge pointed npm installs at the curl
  installer**, which lands a *second* binary in a different PATH entry, leaves a
  coin flip over which one runs, and leaves the stale copy stale — the exact
  failure the hint exists to prevent. The hint is now chosen from the running
  binary's path, and only the documented global layouts get a copyable command;
  a project-local or pnpm install is named rather than guessed at, because
  `npm i -g` there would update the global prefix and leave the copy that printed
  the nudge untouched.

## [0.5.6] — 2026-07-14

### Fixed
- **The config census and presence heartbeats shipped empty.** Both built their
  payload as a Go struct and assigned it to `Event.Data` (an `interface{}`). The
  redaction pass requires a map and default-denies anything else, so the type
  assertion failed and the entire payload was replaced with `{}` — before
  signing, before the wire. Every census reported zero skills, zero MCP servers
  and zero CLAUDE.md tokens regardless of the machine, and every heartbeat
  arrived with no CLI version. Payloads now convert through their JSON tags
  before projection, and a non-map `Data` is logged rather than dropped in
  silence. Upgrading re-emits a correct census immediately on watch start.
  - Downstream, this is what made **always-on config tax** and **dead-weight
    skills** read `$0` for every engineer, and left fleet health with no CLI
    version or heartbeat to report.
- **`workspaceKey` is no longer stripped from the census.** It was collected but
  missing from the projection allowlist, so the backend never received the field
  it uses to count distinct workspaces — the denominator for CLAUDE.md coverage.
  Needs the matching backend allowlist entry to take effect.
- **Event signatures now chain per session rather than per device.** The chain
  was global to the buffer file, so concurrent sessions interleaved into a single
  chain no verifier could walk. Each session's tip now lives in a derived index,
  rebuilt from the ledger whenever it is missing or corrupt; a pre-upgrade buffer
  reproduces its old device-wide tip exactly, so the legacy chain continues
  unbroken.

## [0.5.5] — 2026-07-13

### Fixed
- **`login` now accepts current developer keys.** The backend mints six-group
  (120-bit) engineer keys (`PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX`), but the CLI still
  required the retired two-group format and rejected every real key with "that
  doesn't look like a developer key". Key validation now accepts any number of
  groups, so it won't break again when the backend tunes key entropy.
- **Security: six-group keys are now redacted from captured content.** The
  redaction pass shared the same two-group pattern, so a current engineer key
  pasted into a transcript survived unredacted. It now matches any key length.

## [0.5.4] — 2026-07-13

### Changed
- Onboarding now points at the canonical `login` → `autostart enable` →
  `status` flow across `install.sh`, the README, and the npm README, so capture
  is set up to survive reboots from the first run.
- The live `status` dashboard shows an **autostart** row, so an
  installed-but-not-armed seat is visible instead of silently dying on the next
  reboot.

### Fixed
- The live status dashboard no longer probes the OS service manager on every
  render tick/keypress — a slow `launchctl` / `systemctl` / `schtasks` could
  stutter or block the dashboard. It now probes once and on manual refresh, and
  an installed-but-inactive autostart service shows a warning instead of a
  healthy green indicator.

## [0.5.3] — 2026-07-13

### Added
- **Silent self-update** — the `watch` daemon checks GitHub Releases at startup
  and every 24h, and when a newer **signed** release exists it downloads the
  platform binary, verifies it (a minisign signature over `SHA256SUMS` against a
  public key embedded in the binary, then a per-file SHA-256 match), atomically
  swaps it in, and re-execs in place so capture never drops. Opt out per machine
  with `--no-auto-update` / `PROMPTSTER_TEAMS_NO_AUTO_UPDATE`, or org-wide via the
  capture policy (`autoUpdate` / `pinnedCliVersion`, fetched from
  `GET /v1/teams/policy`). `doctor` now reports the running version and
  auto-update status. Fail-open by design: a network or policy hiccup never
  strands a machine on an old build. (Activates for clients on this version or
  newer; a fresh install/upgrade to ≥0.5.3 is the one-time bootstrap.)

### Changed
- Releases now sign `SHA256SUMS` with minisign and publish `SHA256SUMS.minisig`.
  The release job verifies that signature against the public key committed to the
  repo, so a wrong signing key fails the release instead of shipping a signature
  every client rejects.

## [0.5.2] — 2026-07-11

### Fixed
- **Deterministic transcript event IDs (Claude + Codex)** — a session
  resume/fork/compact writes a new transcript/rollout file that copies prior
  history verbatim; because the watcher runs one processor per file, those
  re-observed lines were re-emitted with fresh random event IDs, so the backend
  stored each event twice (visible as doubled rows in session replay). Event IDs
  are now derived deterministically (UUIDv5) from each source line's stable
  identity — the transcript line `uuid` / `message.id` / tool `call_id` (Claude),
  and the rollout session `id` / `call_id` / line timestamp (Codex) — so a
  re-observed line yields the same ID and collapses instead of duplicating.

### Changed
- Repository reorganized for external security review: dead hook/Cursor
  normalization code removed, source moved into `cmd/` + `internal/` packages.
- CI hardened: cross-platform build/test matrix, race + coverage, `gofmt` and
  `staticcheck` gates, `gosec` SAST and `govulncheck` (both publishing SARIF to
  the GitHub Security tab), CodeQL, and a gitleaks self-scan. GitHub Actions
  pinned by commit SHA; Dependabot enabled.
- Release integrity: `SHA256SUMS` published per release, `install.sh` verifies
  the download before executing it, and npm publishes with build provenance.

### Removed
- Internal `docs/teams-cli-e2e-findings.md` engineering work-log (no longer part
  of the published repository).

## [0.5.1] — 2026-07-10

### Added
- **Opt-in assistant-prose capture (org-gated, default off)** — when an org
  turns it on, the CLI keeps assistant response narration instead of dropping it
  entirely. The policy is fetched from `GET /v1/teams/policy`
  (`{ captureAssistantProse }`), refreshed at watch start and every 10 minutes,
  cached to the state dir, and **fail-closed**: any error (network, non-200,
  unparseable, teams-not-configured 503, or no cache) resolves to false, so prose
  is only ever captured when the backend affirmatively opts the org in.
- **`scrubAssistantProse` on-device scrubber** — before any kept assistant text
  is signed, buffered, or sent, code is stripped out on-device: fenced code
  blocks and anchored diff/patch runs collapse to a `<code-redacted>` marker, and
  over-long inline backtick spans are redacted, while narration and short symbol
  references (`useState`, `src/x.ts`) survive. Byte-for-byte lockstep with the
  backend scrubber, so the never-store-source guarantee holds even with prose
  capture enabled.

### Changed
- Policy refresh runs on a background goroutine (`Resolver.StartBackground`)
  instead of inline in the capture loops, so a slow policy fetch never stalls
  transcript capture. Response body is size-capped (64 KB) before decode, and the
  disk cache is written via a per-process temp file for safe concurrent writes
  across `claude-watch`/`codex-watch` (Windows-safe atomic rename).

## [0.4.0] — 2026-07-07

### Added
- **Client-side source exclusion** — a default-deny field allowlist strips
  diffs, file contents, command output, and assistant text on-device, so source
  content is never transmitted.

## [0.3.0] — 2026-07-02

### Added
- **Efficiency telemetry** — usage attribution, prompt commands, compact
  triggers, and a config census (skills/plugins/MCP token accounting).
- **Presence heartbeat** — a content-free event that distinguishes an
  installed-but-idle seat from one where the CLI was never installed.
- **On-device PII redaction** and broader credential coverage.

## [0.2.0] — 2026-06-30

### Added
- `login` setup TUI and per-developer (`PSE-`) keys.

## [0.1.1] — 2026-06-30

### Fixed
- Default ingest path set to `/v1/teams/ingest`.

## [0.1.0] — 2026-06-29

### Added
- Initial release: on-device, auditable AI-coding capture for teams — tails
  Claude Code + Codex transcripts, redacts on-device, signs into a
  tamper-evident chain, and streams to a team backend.

[Unreleased]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.25.0...HEAD
[0.25.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.24.0...v0.25.0
[0.24.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.18.1...v0.19.0
[0.18.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.18.0...v0.18.1
[0.18.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.12.4...v0.13.0
[0.12.4]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.12.3...v0.12.4
[0.12.3]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.12.2...v0.12.3
[0.12.2]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.12.1...v0.12.2
[0.12.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.11.4...v0.12.0
[0.11.4]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.11.3...v0.11.4
[0.11.3]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.11.2...v0.11.3
[0.11.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.10.1...v0.11.0
[0.10.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.9.2...v0.10.0
[0.9.2]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.6...v0.6.0
[0.5.6]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.5...v0.5.6
[0.5.5]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.4...v0.5.5
[0.5.4]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.3...v0.5.4
[0.5.3]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.2...v0.5.3
[0.5.2]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.0...v0.5.1
[0.4.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/pa-arth/promptster-teams-cli/releases/tag/v0.1.0
