# Login & credentials

The one command a new engineer runs. It takes the per-engineer key their manager
minted, validates the shape, saves it, and then does four more things nobody
asked for on the tin — which is most of what there is to verify here.

## Sub-features

- `--key PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX` — non-interactive. Without it,
  `login` reads one line from a TTY; on a non-TTY it exits **1** with
  `no key provided. Pass --key …, or run login in a terminal.` on **stderr**.
- `--api-url <url>` — override the ingest base. Persisted to the credential file
  only when it differs from `ingest.DefaultAPIURL` (`https://api.promptster.ai`),
  so the hosted default stays implicit.
- Shape validation — `engineerKeyRe` is
  `^PSE-(?:[A-HJ-NP-Z2-9]{4}-)+[A-HJ-NP-Z2-9]{4}$`: base32 with no I/O/0/1, and
  the segment **count is deliberately not pinned** (pinning it is what broke
  login when the backend went from two segments to six). A malformed key exits 1
  with `that doesn't look like a developer key (expected PSE-XXXX-…)`.
- Credential persistence — `~/.promptster-teams/credentials`, mode `0600`, dir
  `0700`, written via a `.tmp` + rename. Shape: `{"token": …, "apiUrl": …?}`.
- Four side effects, in order: the one-time update-consent question, a detached
  capture daemon (`capture.StartDaemon`), `autostart enable`, `statusline
  enable`, then a `discover` notice about other OS users.
- Watch scope — `login` sets `PROMPTSTER_TEAMS_WATCH_DIR` to `$HOME` when unset,
  so the daemon spans every repo instead of login's incidental cwd.

## How to get to it (user POV)

`promptster-teams login`, paste the key. That is the entire onboarding; capture
starts by itself, so the engineer never runs `start`.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run login --key PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ
node $CT inspect .promptster-teams/credentials
node $CT inspect                       # startedPids, escapedCommandAttempts
node $CT run stop                      # always, before the next case
```

Failure paths:

```bash
node $CT run login --key nope          # expect exitCode 1 + "doesn't look like a developer key"
node $CT run login                     # expect exitCode 1 + stderr "no key provided"
```

`PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ` is shape-valid and backend-invalid on
purpose. Never drive `login` with a real engineer key — it would be written into
the evidence directory in clear text.

## Proves it works

All four, not just the first:

1. stdout contains `You're set`, the masked key `PSE-…WXYZ`, and
   `stored ~/.promptster-teams/credentials`.
2. `inspect .promptster-teams/credentials` returns the **exact token you
   passed** and `"apiUrl": "http://127.0.0.1:9"` (present because the sandbox
   URL differs from the hosted default).
3. `startedPids` is non-empty and the same pid appears in `supervisor.json` and
   the three watcher pidfiles — the watchers are goroutines under one `watch`
   process, so one pid in four files is correct, not a bug.
4. stdout contains `✓ capturing in the background (pid N)`,
   `✓ autostart enabled — capture resumes at every login`, and
   `✓ Claude usage tracking on`.

A `login` that prints `You're set` and leaves no credential file is the bug the
`login-credentials-not-persisted` eval case plants. stdout alone cannot catch it.

## Gotchas

- **It starts a daemon.** Every `run login` leaves a detached process behind.
  Always `run stop` (or `cleanup`) after, or the next case's `doctor` reports
  `noStrayDaemons: false` and you will be inspecting another run's state.
- **It reaches launchd.** `autostart enable` runs `launchctl bootout`,
  `bootstrap`, `kickstart` against `gui/<uid>`. Verified: without the harness's
  PATH shim those hit the engineer's real launchd domain. Check
  `escapedCommandAttempts` in `inspect` — three `launchctl` lines is the correct
  observation, not an error.
- **It rewrites `~/.claude/settings.json`** (statusline enable), in the sandbox
  home. Fine there; catastrophic if `HOME` were not redirected.
- The reachability probe is best-effort and never blocks the save:
  `! couldn't reach 127.0.0.1:9 — saved anyway` is a PASS.
- The long Cursor-vendor-usage disclosure paragraph is printed on every
  successful start. It is disclosure copy, not an error.
- `--api-url` is resolved through `ingest.ResolveAPIURL(flag)`, whose precedence
  is flag > `PROMPTSTER_TEAMS_API_URL` > stored file > default. The sandbox sets
  the env var, so a defect that ignores the **flag** is masked. To verify the
  flag itself you must pass a URL that differs from the sandbox's dead port.
