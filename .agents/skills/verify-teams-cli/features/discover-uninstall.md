# Discover & uninstall

The two commands that deliberately look outside their own installation.

## Sub-features

### `discover`

`cmdDiscover` (`internal/cli/cmd_discover.go`) finds **other local OS user
homes** that have Claude, Codex or Cursor. It never opens transcripts,
configuration, databases or credentials — it only detects presence, and prints
setup instructions for each environment found.

Observed output shape:

```
! found 1 additional AI environment(s)
/Users/<other>
Products: Claude, Codex, Cursor
Promptster state found — run `promptster-teams doctor` as that user to verify it is sending
```

No flags. Exits 0.

### `uninstall`

`cmdUninstall` (`internal/cli/uninstall.go`) is the **only** uninstall path that
exists: `npm rm -g` runs no uninstall script and leaves the managed binary in
place, so removing the package alone stops nothing.

It stops capture, removes the autostart unit, unenrolls the Cursor hook, and
restores the Claude statusline. `--purge` additionally deletes
`~/.promptster-teams` — the key, the unsent event queue, and the managed binary
itself. `uninstall --help` prints:

```
usage: promptster-teams uninstall [--purge]
  Stops capture, removes the autostart unit, unenrolls the Cursor hook,
  and restores the Claude statusline.
  --purge also deletes ~/.promptster-teams (your key, the unsent event
  queue, and the managed binary itself).
```

## How to get to it (user POV)

`promptster-teams discover` after `login` mentions other environments.
`promptster-teams uninstall` when they are done with it.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run discover

node $CT run login --key PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ
node $CT inspect                       # note credentials + plist + settings.json
node $CT run uninstall --purge
node $CT inspect                       # and note what is gone
```

## Proves it works

**discover** — it names a home directory that is not the sandbox, with a
product list. On a machine where the real user has Claude/Codex/Cursor, the
correct result from inside the sandbox is a report about `/Users/<real user>`.
That is the feature working, not an isolation leak (see gotchas).

**uninstall** — a full `login` → `uninstall --purge` round trip, verified by
`inspect` on both sides:

| After `login` | After `uninstall --purge` |
|---|---|
| `.promptster-teams/credentials` present | gone |
| `.promptster-teams/buffer.jsonl` present | gone |
| `Library/LaunchAgents/ai.promptster.teams.plist` present | gone |
| `.claude/settings.json` wraps `statusline run` | restored to the prior command |
| daemon pid alive | `cleanup` reports it `already exited` |

An `uninstall` that prints success and leaves the credential file is the exact
failure this command exists to prevent — stdout cannot catch it, `inspect` can.

## Gotchas

- **`discover` escapes the sandbox by design.** It enumerates `/Users`, not
  `$HOME`, so it reads the real machine even with `HOME` redirected. It is
  read-only and presence-only. Do not "fix" it, and do not count it as an
  isolation failure — `doctor`'s `sandboxIsolated` check runs the CLI's `doctor`
  subcommand precisely because `discover` would false-positive it.
- **`uninstall` reaches launchd** the same way `stop` does (`launchctl bootout`
  against `gui/<uid>`). It is safe from the harness only because of the PATH
  shim. Check `escapedCommandAttempts` after driving it.
- `--purge` deletes the sandbox's `.promptster-teams` — including the evidence
  of everything you drove before it. Drive `inspect` **before** purging, and
  remember the harness's own evidence dir is outside the sandbox and survives.
- `uninstall` in a sandbox cannot delete the managed binary, because the harness
  never installs one; `state.CanonicalInstallBin()` points into the sandbox home
  where nothing was written. That branch is unverifiable here — say so rather
  than reporting it as a pass.
