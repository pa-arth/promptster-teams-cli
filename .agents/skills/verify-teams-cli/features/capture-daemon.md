# Capture daemon — start, stop, watch

The thing the product actually is: a process tailing Claude Code, Codex and
Cursor transcripts, normalizing, redacting on-device, signing, and shipping.

## Sub-features

- `watch` — **foreground**. Holds the terminal until Ctrl-C. Tails all three
  vendors' transcripts. Errors print `watch error: …` and exit 1.
- `start` — asks the one-time update-consent question, then
  `capture.StartTeamsDaemon`: a detached supervisor running `watch`. Returns the
  shell. Idempotent — a second `start` no-ops on the supervisor and instead
  registers the new directory as an additional capture root.
- `stop` — `capture.StopTeamsDaemon`. Collects candidate pids from **three**
  pidfiles (`supervisor.json`, `claude-watcher.json`, `codex-watcher.json`),
  because a daemon launched as a bare `watch` writes only the watcher pidfiles
  and reading `supervisor.json` alone silently misses it.
- Liveness is the capture **flock** (`watchRunning()`), not the pid: a dead
  holder's lock is released by the kernel, so it cannot report a phantom.
- `PROMPTSTER_DEBUG=1` before `watch`/`start` turns on per-event logging.

## How to get to it (user POV)

Almost never directly — `login` starts capture for you. `stop` is the one an
engineer actually types, and `start` is what they type after `stop`.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run stop                # on a clean sandbox: the "nothing was running" path
node $CT run start
node $CT inspect .promptster-teams/supervisor.json
node $CT run status --once
node $CT run stop
node $CT inspect                 # supervisor.json is gone
```

**Never `run watch`.** It is foreground and will burn the 60s timeout. `run`
returns `{ok:false, error:"the command did not exit"}` with that hint if you do.

## Proves it works

`stop` has three mutually exclusive outcomes on **stderr**, and picking the
right one is the whole contract — this command exists because a `stop` that
reports success while capture is still running is worse than one that admits it
failed:

| Situation | Line |
|---|---|
| nothing was running | `promptster-teams: no tracked background capture was running — if one is running without a pidfile, find it with pgrep -fl promptster-teams and stop it manually` |
| something was, and is now gone | `promptster-teams: background capture stopped` |
| something survived | `promptster-teams: warning: capture is STILL running after stop — …` |

So the proof is a **pair** of runs: `stop` on a clean sandbox must print the
"nothing was running" line, and `stop` after `start` must print "background
capture stopped". A build that prints "stopped" both times is the
`stop-claims-success-when-nothing-running` eval case — and its output looks
completely healthy in isolation.

Then verify the side effect: the pid from `startedPids` is gone (`cleanup`
reports it as `already exited`), and `supervisor.json` / `claude-watcher.json` /
`codex-watcher.json` have been cleared from the sandbox — `stop` removes them
explicitly because SIGINT/SIGKILL pre-empt the watchers' deferred cleanup.

## Gotchas

- **`stop` boots the launchd job out first.** `StopTeamsDaemon` calls
  `newServiceManager().Stop()` before signalling any pid, to disarm launchd's
  restart policy (launchd respawns almost immediately; `ThrottleInterval` caps
  the restart *rate*, it does not delay the first restart). On macOS that is
  `launchctl bootout gui/<uid>/ai.promptster.teams` — **keyed on the UID, not on
  `HOME`**. Observed: this takes out the engineer's real capture daemon. The
  harness's `launchctl` shim is what makes `run stop` safe; the attempt shows up
  in `inspect` as `escapedCommandAttempts`.
- `Stop()` early-returns when `Status().Installed` is false, and `Installed` is
  read from the plist path under `$HOME`. So a sandbox that never ran
  `autostart enable`/`login` never reaches launchctl at all.
- One pid appears in all four pidfiles. The watchers are goroutines under a
  single `watch` process; that is not duplicate state.
- `pidLooksLikeOurs` guards against a stale pidfile whose pid the OS reused.
  `cleanup` mirrors that guard with `ps -o comm=` — never kill by process name,
  the engineer's own daemon has the same one.
- `start` calls `PromptForUpdateConsent()` first. With empty stdin and no TTY it
  does not block, but it is why `start` can print consent copy before anything
  about capture.
