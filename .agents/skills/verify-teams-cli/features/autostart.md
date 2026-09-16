# Autostart

The per-user OS service that relaunches capture at login so it survives reboots.
launchd on macOS, `systemd --user` on Linux, Task Scheduler on Windows. This is
the feature with the sharpest isolation edge in the repo — read the gotchas
before driving it.

## Sub-features

`cmdAutostart` (`internal/cli/autostart.go`) dispatches four subcommands; an
empty subcommand is `status`, and anything else exits **1** with
`unknown autostart subcommand: <x>` plus `usage: promptster-teams autostart
<enable|disable|status|repair>`.

- `enable` — writes the unit and loads it. macOS: renders
  `~/Library/LaunchAgents/ai.promptster.teams.plist` (0600), then `launchctl
  bootout` (ignored), `launchctl bootstrap gui/<uid> <plist>`, `launchctl
  kickstart -k`.
- `disable` — `launchctl bootout`, then removes the plist.
- `status` — reads the plist; `not enabled` when absent, otherwise reports the
  baked `ProgramPath` and whether `launchctl print` finds it loaded
  (`installed but not loaded (try re-running enable)`).
- `repair` — re-points an already-enabled unit at the binary running right now.
  This is what npm's postinstall calls, because the unit bakes an **absolute**
  path (`state.SelfBin()`) at enable time and nothing else ever revisits it.
  **It never returns non-zero** — it runs inside `npm install`, where a non-zero
  exit would abort the install and leave the engineer with no CLI at all.

## How to get to it (user POV)

`login` enables it automatically. An engineer types `promptster-teams autostart
status` when capture stopped surviving reboots, or `disable` to opt out.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run autostart status          # fresh sandbox
node $CT run autostart enable
node $CT inspect                       # escapedCommandAttempts + the plist
node $CT run autostart status
node $CT run autostart disable
node $CT run autostart nonsense        # expect exitCode 1
```

## Proves it works

Two artefacts, and the second one is the real proof:

1. **The unit file.** After `enable`, `inspect` lists
   `Library/LaunchAgents/ai.promptster.teams.plist` under the sandbox home, and
   `status` flips from `! not enabled` to an installed reading.
2. **The exact supervisor calls.** `inspect` →`escapedCommandAttempts` must show,
   in order:

   ```
   launchctl bootout gui/<uid>/ai.promptster.teams
   launchctl bootstrap gui/<uid> <sandbox>/home/Library/LaunchAgents/ai.promptster.teams.plist
   launchctl kickstart -k gui/<uid>/ai.promptster.teams
   ```

   That the bootstrap argument is the **sandbox** plist is what proves `enable`
   loads the unit it just wrote. Verified end to end against the real binary.

For `repair`, the proof is the printed
`promptster-teams: autostart re-pointed at <path>` naming the current binary —
and an exit code of 0 even when it failed, which is the documented contract, not
a bug to report.

## Gotchas

- **`launchctl` is keyed on the UID, not on `HOME`.** `guiTarget()` builds
  `gui/<os.Getuid()>`. No `HOME` redirection can contain it. The harness shims
  `launchctl`/`systemctl`/`schtasks` on `PATH` and logs every call. Driving
  `autostart enable` or `stop` **without** that shim boots the engineer's own
  capture job out of launchd — observed, not hypothetical.
- Because the shim always exits 0, `status` inside the sandbox will report the
  job as loaded whether or not any real supervisor exists. That is intentional:
  the shim log, not `status`, is the ground truth here.
- `enable` bakes `state.SelfBin()`. Driven from the harness, that is the skill's
  `bin/promptster-teams`, not `~/.promptster-teams/bin/promptster-teams`. Do not
  report the path difference as a defect.
- On this machine, `launchctl bootstrap` against a plist under `/private/tmp`
  fails with `Bootstrap failed: 5: Input/output error`. That is launchd refusing
  the path, not the CLI misbehaving — one more reason the shim is the right
  verification surface.
- `autostart` with **no** subcommand is `status`, not usage. `autostart` and
  `autostart status` must print the same thing.
