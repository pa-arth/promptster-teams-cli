# Status

One panel answering "is capture actually running, and what is it capturing?".
Two renderings of the same state: a live TUI and a static snapshot.

## Sub-features

- `status` on a TTY → `runStatusTUI()`, a full-screen view refreshing every
  second. Falls through to the static print if the TUI cannot start.
- `status --once` / `--plain` / `-1` → one static snapshot, exit.
- Non-TTY stdout → static snapshot regardless of flags, so pipes and CI stay
  clean.
- Rows in `printStatusStatic`: `key`, `ingest`, `watch`, `daemon`, `autostart`,
  `installation`, `identity`, `presence`, `buffered`.
- Two conditional rows: a reconstruction/replay row only while a replay is
  running, and a progress-write-fault row (a fault, so it appears here **and**
  in `doctor`).
- `liveWatchScope` reports what the **live daemon** is watching, falling back to
  `PROMPTSTER_TEAMS_WATCH_DIR` then cwd only when nothing is capturing.

## How to get to it (user POV)

`promptster-teams status` after `login`, to confirm capture is running.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run status --once
node $CT run login --key PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ
node $CT run status --once
node $CT run stop
node $CT run status --once
```

## Proves it works

The `daemon` row is the contract, and it must move:

- Before `login`: `daemon  not running — promptster-teams login starts it, …`
- After `login`: `daemon  running (pid N, <version>)` where **N is the pid in
  `startedPids` from the `login` run**. A "running" row with a pid nobody
  started is not proof.
- After `stop`: back to `not running`.

Then cross-check against `doctor` in the same sandbox: `key`, `ingest` and
`installation` must agree on both surfaces. They read the same resolvers
(`ingest.ResolveToken`, `ingest.ResolveAPIURL`, `capture.DeviceID`), so
disagreement is always a defect in one of the two renderers.

`buffered  N events` should be > 0 after a `login` run: the daemon emits a
presence event immediately and the dead ingest port forces it to buffer locally.
That non-zero count is the cheapest available proof that the capture pipeline
(normalize → redact → sign → buffer) ran end to end.

## Gotchas

- **Never `run status` without `--once`.** Even though a non-TTY forces the
  static path, pass the flag explicitly; relying on TTY detection means a change
  to that detection turns your verification into a 60s timeout.
- `printStatusStatic` takes **one** `capture.Snapshot()` for every row on
  purpose. Sampling per row let capture start or exit mid-render and printed a
  `watch` scope and a `daemon` liveness that never held at the same instant.
  Rows in this panel are mutually consistent by construction — if two of them
  contradict each other, that is a real finding.
- Cosmetic, not a defect: `kvPanel` wraps the long `installation` key across two
  lines (`installati` / `on`). Observed in a real run; do not report it as
  broken output.
- `--once` and `--plain` and `-1` are matched by a literal loop over `args`, not
  by `flag`, so they work in any position and unknown flags are ignored
  silently. `status --nonsense` exits 0 with the normal panel.
- The `watch` row is a directory, and before any daemon exists it shows the
  process's cwd. The harness runs with cwd = sandbox home, so `~` is the correct
  reading there — running from the repo root would show the repo path and imply
  a scope the daemon never had.
