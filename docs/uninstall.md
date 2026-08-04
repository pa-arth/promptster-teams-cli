# Uninstall (`uninstall`, `internal/cli/uninstall.go`)

`uninstall` is the ONLY uninstall path that exists, and it has to be, because
**the package manager cannot perform one.** Both halves measured against npm
11.5.2 in a sandboxed global prefix — do not re-derive either from npm's docs,
which list the lifecycle scripts as though they run:

- **`npm rm -g` runs NO uninstall lifecycle script.** `preuninstall` and
  `postuninstall` fired ZERO times on removal; `postinstall` fired on install and
  again on upgrade. So a `preuninstall` hook is not a fix that was skipped — it
  is a fix that does not exist to wire up. (It would also be the wrong shape if it
  did fire: firing on upgrade would unenroll hooks on every `npm i -g`.)
- **`npm rm -g` leaves files postinstall created outside `node_modules`.** The
  managed binary is exactly such a file, deliberately — that indirection is what
  keeps `npm ls` honest (see the self-update section).

Composed, those give the worst available outcome: `npm rm -g
@promptster/teams-cli` reports success while capture keeps running, keeps
self-updating, and returns at the next login from a binary the engineer believes
they deleted. That is why README, `help`, and `install.sh`'s closing hint all
name `uninstall` explicitly.

What it reverses, in order: autostart (Disable, not Stop — Stop leaves the unit
registered and capture returns at login) → the daemon → `~/.cursor/hooks.json` →
the wrapped `statusLine`. `--purge` additionally deletes the state dir(s).

- **THE CURSOR HOOK IS THE ITEM THAT OUTRANKS THE REST**, small as it looks.
  Delete the binary by hand — the obvious move for a curl install — and the
  hooks.json entry survives naming a path that is gone, so Cursor execs a missing
  command *inside the engineer's agent loop* on every prompt, edit and shell
  call. That degrades THEIR tool rather than our data. It is also the one piece
  nothing on our side can self-heal: if the binary is gone, none of our code runs.

  `doctor` is therefore the ONLY place that state can ever be reported
  (`CursorHooksDoctor`, `cursor_hooks_doctor.go`) — it needs a working binary to
  say anything, and doctor is the command engineers run while something is
  already wrong. It is strictly READ-ONLY: enrolling or repairing from a
  diagnostic would write hooks.json on a machine whose owner only asked what was
  broken, and a check that mutates what it measures cannot be trusted to describe
  it (`TestCursorHooksDoctorWritesNothing`).

  **The dangerous direction there is the FALSE ALARM, not the miss.**
  `cursorHookCommand` renders the path with `%q`, so a Windows entry arrives
  backslash-escaped; a naive scan to the next quote returns the path with every
  separator doubled, `os.Stat` calls it missing, and every healthy Windows
  machine prints the most alarming line doctor has. `cursorHookCommandBinary`
  closes the quoted token with escape awareness and `strconv.Unquote`s it, and
  the mutation to the naive scan is pinned by
  `TestCursorHooksDoctorDoesNotFalseAlarmOnAnEscapedPath`.
- **Every step runs even when an earlier one fails.** Short-circuiting on the
  first error is precisely how the hook entry survives a removal.
- **Liveness is read BEFORE and AFTER the stop** (`capture.CaptureRunning`, the
  lock — not `DaemonStatus`'s recorded PID, which a bare `watch` under autostart
  never writes). "stopped" printed over a machine where nothing ran is a harmless
  lie; printed over capture that is still alive it is the lie an engineer acts on,
  so that case is a FAILURE with a non-zero exit.
- **`--purge` on Windows cannot unlink the running image.** A plain `RemoveAll`
  would abort part-way and leave an arbitrary remainder — quite possibly the
  credentials. So the running binary is skipped explicitly, named in the output,
  and everything else still goes (`removeAllExcept`).
- **The tests fake ONLY autostart + daemon-stop, and must keep doing so.**
  `launchctl bootout/bootstrap` target `gui/$UID` by LABEL, so a sandboxed `HOME`
  does not protect the developer's real `ai.promptster.teams` job — same trap as
  the autostart section below. The file-touching steps do respect `HOME` and run
  for real. Four assertions mutation-tested (broken, confirmed failing, reverted):
  hook removal wired up, no short-circuit, `--purge` gated on the flag, post-stop
  liveness check.

