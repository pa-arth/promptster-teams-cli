# Update & version

Self-update over signed GitHub releases. Every release is verified against an
embedded minisign key (`minisign.pub`) before a byte is installed.

## Sub-features

`cmdUpdate` (`internal/cli/cmd_update.go`) parses real flags with `flag`:

| Flag | Meaning |
|---|---|
| `--check` | Report what an update would do, install nothing |
| `--yes` | Install without asking (scripts, non-interactive shells) |
| `--enable-auto` | Allow background self-installs on this machine |
| `--disable-auto` | Stop background self-installs on this machine |
| `--ask-each` | Notify per release and let the engineer decide (the default policy) |

- `version` / `--version` / `-v` print `version.Version`, stamped at build time
  via `-ldflags -X …/internal/version.Version`.
- Authority order: an **org policy** can disable self-update entirely or pin an
  exact version, and then the engineer is never asked. Otherwise `login` and
  `start` ask once and remember (`selfupdate.LoadConsent()`).
- Per-machine opt-outs: `watch`/`start --no-auto-update`, or
  `PROMPTSTER_TEAMS_NO_AUTO_UPDATE=1`.
- The background updater runs detached with no terminal and therefore cannot ask
  anything. Every path where it declines points here — that is why this command
  carries the policy switches at all.

## How to get to it (user POV)

`promptster-teams update` when they want a newer build now, or
`promptster-teams update --disable-auto` to stop the machine updating itself.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run version
node $CT run update --disable-auto
node $CT run doctor          # the auto-update line must reflect the switch
node $CT run update --enable-auto
node $CT run doctor
```

The policy switches are local state writes and are safe. `--check` and a bare
`update` reach GitHub — see gotchas.

## Proves it works

- `run version` prints exactly what `node $CT build` reported as `version`
  (e.g. `v0.31.0-1-g621effe`). If it prints `dev`, the binary was built without
  the ldflags and **the whole auto-update surface changes behaviour** —
  `printAutoUpdateStatus` skips its network probe when the version is `dev`, so
  a `dev` build silently verifies a different code path than the one that ships.
- `--disable-auto` then `--enable-auto` must each move `doctor`'s auto-update
  line, and the change must survive into a **separate** process — that is the
  point of the durable local mirror. Assert across two `run` invocations, never
  within one.

## Gotchas

- **The harness sets `PROMPTSTER_TEAMS_NO_AUTO_UPDATE=1`** so `doctor` and
  `status` stay offline. That env var also changes the auto-update line itself,
  so when you are verifying *that line* you are verifying the env-disabled
  branch. `autoUpdateStatusLine` is pure and unit-tested precisely because every
  branch is otherwise hard to reach.
- `update --check` and a bare `update` hit `api.github.com`. They are read-only
  but they are network, and a bare `update` with `--yes` would **replace the
  binary**. Do not drive `--yes` from the harness; it would overwrite the build
  you are verifying, which makes every subsequent drive meaningless.
- Doctor reads org policy from a **local mirror** rather than fetching, because
  doctor must work offline. A sandbox has no mirror, so it always reports the
  unmanaged branch; org-managed behaviour cannot be verified here.
- `version` is the cheapest `doctor` cross-check in the repo: `node $CT doctor`
  reports `version` by running the binary, so a disagreement between that and
  `run version` means the harness and your drives are using different binaries.
