# Statusline

Claude Code rate-limit **window** capture. Claude Code has no API for usage
percentages, so the CLI wraps the engineer's `statusLine` command and reads the
blob Claude passes it on every tick.

## Sub-features

`cmdStatusline` (`internal/cli/cmd_statusline.go`); empty subcommand is
`status`, anything unknown exits **1** with `unknown statusline subcommand: <x>`
and `usage: promptster-teams statusline <enable|disable|status>`.

- `enable` — `capture.EnableStatusline()`. Four outcomes, each with its own
  line: `AlreadyEnabled` (`already tracking your Claude usage — nothing to
  change`), `WrappedExisting` (`wrapped your existing statusline to add usage
  tracking` + the prior command echoed back), `Rewrapped` (`your statusline
  changed — re-wrapped the new one`), `InstalledFresh` (`installed a statusline
  that shows your 5h/weekly usage`).
- A mandatory disclosure line follows every one of them: *only your two usage
  percentages, their reset times, and your model's context-window size leave
  your machine*. It is required to stay exhaustive — it grew a third item when
  the shim started spooling the context window.
- `disable` — unwraps, restoring the engineer's prior command.
- `status` — the effective-statusline drift check.
- `run` — the shim Claude Code invokes each tick. Reads stdin, spools the
  window reading, passes the prior line through. **Always exits 0**, fail-open
  and fast, because it runs inside the engineer's editor.

## How to get to it (user POV)

`login` enables it. Otherwise `promptster-teams statusline enable`, and
`disable` to turn it off.

## Driving it with control-teams-cli

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"
node $CT run statusline status
node $CT run statusline enable
node $CT inspect .claude/settings.json
node $CT run statusline status
node $CT run statusline disable
node $CT inspect .claude/settings.json
node $CT run statusline nonsense      # expect exitCode 1
```

**Never `run statusline run`** — it reads stdin and is driven by Claude Code,
not by you. The harness feeds empty stdin, so it would exit immediately having
proved nothing.

## Proves it works

The settings file is the proof, not the console line:

- Before: `inspect .claude/settings.json` fails (no such file in a fresh
  sandbox), and `status` prints `! Claude window capture off — run
  promptster-teams statusline enable to track your 5h/weekly usage`.
- After `enable`: `.claude/settings.json` exists and its `statusLine.command`
  names `promptster-teams statusline run`.
- After `disable`: the wrap is gone and `status` is back to the `!` line.

The round trip matters more than either half. `enable` on a settings file that
already has a custom statusline must report `wrapped your existing statusline`
and echo the prior command — and `disable` must put that exact command back.
An `enable`/`disable` pair that loses the engineer's own statusline is the
failure this feature is written to prevent.

## Gotchas

- **Exit code is 0 for `status` whether capture is on or off.** The `!` glyph
  carries the severity.
- `capture.StatuslineDoctor(dir)` resolves the statusline across **all** settings
  layers, not just the file the CLI writes, so a project-layer shadow can
  silently disable window capture while `~/.claude/settings.json` still looks
  correct. Verifying only the file the CLI wrote misses exactly that case.
- Statusline state is deliberately scoped to `GlobalStateDir()`, not
  `StateDir()`, because `~/.claude/settings.json` is machine-global and has no
  workspace concept. Workspace-scoping it would give one settings file two
  disagreeing wrap records.
- The harness points `CLAUDE_CONFIG_DIR` at the sandbox home, so `enable`
  rewrites a throwaway settings file. Without that redirection this command
  edits the engineer's real Claude Code configuration.
- `login` runs `statusline enable` for you, so a sandbox that has been through
  `login` is already wrapped — drive `statusline` on a fresh sandbox if you want
  to see the `InstalledFresh` path.
