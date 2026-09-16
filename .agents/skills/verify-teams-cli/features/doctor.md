# Doctor

The one command an engineer runs when something is wrong. It diagnoses the
credential, ingest reachability, the transcript dir, the delivery queue, which
build is actually capturing, autostart, the Cursor hook, and the Claude
statusline. Read-only by contract.

## Sub-features

Lines, in the order `cmdTeamsDoctor` prints them
(`internal/cli/teams_status.go`):

1. `✓ version <v>` — the build **printing** this, not the one capturing.
2. auto-update state (`printAutoUpdateStatus`).
3. key: `✗ no developer key — run promptster-teams login`, or
   `✓ key PSE-…WXYZ  (<source>)`, or a `!` line when a key is set but is not a
   `PSE-` developer key. Source is one of `--key flag`,
   `PROMPTSTER_TEAMS_TOKEN env`, `stored credentials`.
4. `✓ ingest reachable: <host>` / `! ingest not reachable: <host>` — a plain GET
   to the API base, **not** an auth probe against the ingest endpoint.
5. `✓ installation health id: <id>`.
6. Claude transcripts dir present / `! … not found yet`.
7. presence heartbeat cadence.
8. delivery-queue health (`checkQueueHealth`), then history replay
   (`reconstructionLines`), then the progress-write fault (`progressWriteFaultLines`).
9. which build is capturing vs. which runs at next login (`captureProcessLines`).
10. autostart (`autostartLines`), Cursor hooks (`capture.CursorHooksDoctor`),
    Claude statusline drift (`capture.StatuslineDoctor(cwd)`).
11. Closing line: `Ready. Run promptster-teams watch from a repo.` when the
    credential checks passed, else `Run promptster-teams login to get set up.`

## How to get to it (user POV)

`promptster-teams doctor`, any time, from anywhere.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run doctor                                   # unconfigured sandbox
node $CT run login --key PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ
node $CT run doctor                                   # configured + capturing
node $CT run stop
```

Drive it **twice**, before and after `login`. Half the value of this command is
that its lines change; a single snapshot cannot show that they do.

## Proves it works

- Unconfigured: the key line is `✗ no developer key — run promptster-teams
  login` and the closing line is `Run promptster-teams login to get set up.`
- After `login`: the key line is `✓ key PSE-…WXYZ  (stored credentials)` — the
  source string is part of the proof, it is what tells an engineer *which*
  credential the daemon will use — and the closing line flips to `Ready.`
- `installation health id` is non-empty and stable across both runs.
- The ingest host matches the configured URL (`127.0.0.1:9` in the sandbox), and
  matches what `status` prints. If the two disagree, one of them is lying — that
  is the `status-reports-default-ingest` eval case.

## Gotchas

- **Exit code is always 0.** Even with `✗ no developer key`. Asserting
  `exitCode === 0` proves the process ran, nothing more.
- **It is not silent on the network.** `printAutoUpdateStatus` calls
  `selfupdate.LatestVersionBestEffort(3s)` against GitHub unless
  `PROMPTSTER_TEAMS_NO_AUTO_UPDATE` disables auto-update or the version is
  `dev`. The harness sets that env var, so doctor stays offline; without it,
  doctor is a network command.
- **`ok` deliberately does not track everything.** A stuck delivery queue and a
  progress-write fault both print `!` without clearing `ok` — the closing line
  is about *setup*, not about storage. A proof that reads the closing line as an
  overall health verdict is wrong.
- The version on line 1 and the version in the capture-process lines are
  routinely different: one is this process, the other is the daemon. That
  mismatch is a feature (it is exactly what `captureProcessLines` exists to
  surface), not a defect.
- `StatuslineDoctor(cwd)` resolves the **effective** statusline across every
  settings layer, so its answer depends on the run's cwd. The harness always
  runs with cwd = the sandbox home; do not compare against a run made from the
  repo root.
