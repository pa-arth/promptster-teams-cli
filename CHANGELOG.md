# Changelog

All notable changes to `promptster-teams` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and the project
follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/pa-arth/promptster-teams-cli/compare/v0.5.5...HEAD
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
