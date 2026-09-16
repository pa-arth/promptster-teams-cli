# promptster-teams CLI — Feature Map

Every user-facing subcommand, its real flags, its real output strings, and what
proves it works. Read this before driving; it costs far fewer tokens than
re-deriving the command tree from `internal/cli/cli.go`.

Every drive command below is:

```bash
node .agents/skills/verify-teams-cli/control-teams-cli.mjs run <subcmd> [args…]
```

## The command surface

Dispatch lives in one `switch` in [`internal/cli/cli.go`](../../../../internal/cli/cli.go).
There are no hidden subcommands; anything not listed there exits 1 with
`unknown command: <x>` plus the usage block.

| Feature | Subcommands | File |
|---|---|---|
| [Login & credentials](login.md) | `login` | `internal/cli/cmd_login.go`, `internal/ingest/credentials.go` |
| [Doctor](doctor.md) | `doctor` | `internal/cli/teams_status.go`, `queue_health.go`, `capture_process_doctor.go` |
| [Status](status.md) | `status` | `internal/cli/teams_status.go`, `status_tui.go` |
| [Capture daemon](capture-daemon.md) | `start`, `stop`, `watch` | `internal/capture/daemon.go` |
| [Autostart](autostart.md) | `autostart enable\|disable\|status\|repair` | `internal/cli/autostart.go`, `internal/service/*` |
| [Statusline](statusline.md) | `statusline enable\|disable\|status\|run` | `internal/cli/cmd_statusline.go`, `internal/capture/statusline*.go` |
| [Update](update.md) | `update`, `version` | `internal/cli/cmd_update.go`, `internal/selfupdate/*` |
| [Discover & uninstall](discover-uninstall.md) | `discover`, `uninstall` | `internal/cli/cmd_discover.go`, `uninstall.go` |

**Not user-facing, do not drive:** `claude-watch`, `codex-watch`,
`cursor-watch`, `git-watch` (foreground watchers spawned by `watch`) and
`cursor-hook` (invoked by Cursor with a payload on stdin, and always exits 0 by
design so it can never break the engineer's agent loop).

## Before anything: exit codes are not verdicts

`doctor`, `status`, `statusline status` and `autostart status` print `✗` and `!`
lines and **still exit 0**. The only exits that carry information:

- `1` — unknown command, unknown `autostart`/`statusline` subcommand, a
  malformed `login` key, `login` with no key on a non-TTY, or a watcher error.
- `2` — `login` flag parse failure (`flag.ContinueOnError` → `os.Exit(2)`).

Assert on the printed lines, and cross-check with `inspect`.

## The isolation gotcha that invalidates most proofs

`PROMPTSTER_STATE_DIR` looks like the isolation seam — 300-odd references —
but it is not the whole one:

- `ingest.CredentialsPath()` → `state.GlobalPromptsterDir()` → `os.UserHomeDir()`.
  **`PROMPTSTER_STATE_DIR` does not move the credential file.**
- `internal/service/service_darwin.go` `plistPath()` → `os.UserHomeDir()`.
- `claudeConfigDir()` → `CLAUDE_CONFIG_DIR` or `~/.claude`.

`HOME` covers all of those. What `HOME` does **not** cover is `launchctl`:
`guiTarget()` builds `gui/<os.Getuid()>` — keyed on the UID. `autostart enable`,
`autostart disable`, `stop` and `uninstall` all reach the engineer's real
launchd domain from inside any `HOME` you choose. The harness shims
`launchctl`/`systemctl`/`schtasks` on `PATH` for exactly this reason; `inspect`
reports the absorbed calls as `escapedCommandAttempts`, and that list is how you
verify autostart without touching the machine.

`discover` is the other escape, deliberately: it enumerates **other OS users'**
home directories (it will print `/Users/<you>` even from a sandbox). That is the
feature, it is read-only, and it is not an isolation failure.

## Two surfaces describe one state

`doctor`, `status` and the files under `inspect` all report the resolved key,
the ingest URL, daemon liveness and the buffered event count. A defect that
moves only one of them is precisely what a single-surface proof misses — the
`status-reports-default-ingest` eval case is that defect. Cross-check.

## `unavailable` is not broken

These are the app working correctly in a sandbox, not findings:

- `! ingest not reachable: 127.0.0.1:9` — nothing listens there. By design.
- `! couldn't reach 127.0.0.1:9 — saved anyway` (from `login`) — same.
- `! Claude Code transcript dir not found yet: …/.claude/projects` — a fresh
  sandbox home has no Claude history.
- `✓ Cursor not installed for this user (no ~/.cursor) — nothing to enroll`.
- `! capture not running` before you have run `login`/`start`.
- `✓ delivery queue empty — every captured event has shipped` on a fresh
  sandbox: the queue is empty because nothing was ever captured, not because a
  delivery succeeded. Do not read it as proof of delivery.
