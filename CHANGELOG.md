# Changelog

All notable changes to `promptster-teams` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and the project
follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.7.0...HEAD
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
