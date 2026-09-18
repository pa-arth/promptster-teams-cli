---
name: verify-teams-cli
description: Drive the real promptster-teams Go CLI (login, start/stop, status, doctor, autostart, statusline, uninstall) inside a throwaway sandbox and capture proof. Use whenever a change to promptster-teams-cli needs to be shown working — "verify this", "prove it works", "did that actually ship" — or when reproducing a bug report against the real binary. `go test ./...` proves the units behave; this proves the command does.
---

# Verify promptster-teams CLI

This is a short-lived process, not a server. So verification is: **build the
binary once, then drive each run in its own isolated sandbox.** Nothing you do
here may touch the engineer's real `~/.promptster-teams`, their credentials,
their launchd job, or the hosted ingest API.

Everything runs through one helper. Every command returns JSON; every failure
returns `{ok:false, error, hint}` where `hint` says what to do next.

```bash
CT=".agents/skills/verify-teams-cli/control-teams-cli.mjs"   # from the repo root
node $CT help
```

## 1. Launch

There is no server to keep alive. "Launch" is two steps:

```bash
node $CT build       # go build ./cmd/promptster-teams → .agents/skills/verify-teams-cli/bin/
node $CT doctor      # is this build worth driving?
```

`build` stamps the same `-ldflags -X …/internal/version.Version` the `Makefile`
does, so `run version` prints a real version string and not `dev`. It writes
beside the skill and **never** installs to `/usr/local/bin` or
`~/.promptster-teams/bin`.

Requires the Go toolchain. Nothing else — no npm install, no browser.

## 2. Doctor

```bash
node $CT doctor
```

Run this first whenever anything looks off. It answers the only question that
matters before driving: **is this build worth driving, and is the sandbox
actually a sandbox?** Each failed check names its fix in `hints`.

The checks exist because each one is a state where a proof *looks* fine and
measures nothing:

| Check | What a green proof would actually have measured |
|---|---|
| `buildCurrent` | A `.go` file is newer than the binary — you drove the **previous** build. Your change was never executed. |
| `sandboxIsolated` | The CLI printed a path under the **real** `$HOME`. Your proof is a statement about the engineer's machine, not about the change. |
| `ingestPointsAtDeadPort` | Ingest is reachable, so a drive could POST real captured events to a real backend. |
| `noStrayDaemons` | A detached daemon from an earlier run is still writing state. The files you inspect were not written by the command you just ran. |
| `noRealTokenInEnv` | `PROMPTSTER_TEAMS_TOKEN` in your shell would make an unauthenticated sandbox look logged in. |
| `sandboxClean` | Leftover state from an earlier run; a "fresh install" proof is not fresh. |

`sandboxIsolated` is not a config read — doctor actually runs the binary and
greps its output for the real home path.

**`exitCodeIsNotAVerdict`.** `doctor`, `status`, `statusline status` and
`autostart status` print `✗` and `!` lines and still **exit 0**. Only an unknown
subcommand, a bad key, and hard errors exit 1. Assert on the printed lines.

## 3. Drive

Read [`features/README.md`](features/README.md) first — it maps every
subcommand to its flags, its real output strings, and the end state that proves
it works. Driving from the map costs far fewer tokens than re-deriving the
command tree from `internal/cli/cli.go`.

```bash
node $CT run doctor
node $CT run status --once
node $CT run login --key PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ
node $CT inspect                                   # what the run wrote
node $CT inspect .promptster-teams/credentials     # read one file
node $CT run stop
```

`run` executes one subcommand with `cwd` = the sandbox home, **no TTY and empty
stdin**, and a 60s timeout (`PROMPTSTER_VERIFY_TIMEOUT_MS`). It returns
`{argv, exitCode, signal, stdout, stderr, startedPids, evidence}` and writes
`stdout.txt` / `stderr.txt` / `cmd.json` under the evidence dir.

Do **not** `run watch`, `run status` without `--once`, or `run statusline run` —
they are foreground/streaming and will hit the timeout. The map says which.

### The sandbox

`run` builds its environment from scratch (nothing inherited), so a token or
API URL in your own shell cannot leak in:

| Var | Points at | Would otherwise be |
|---|---|---|
| `HOME` | `$SBX/home` | `~/.promptster-teams/credentials`, `~/.claude`, `~/.cursor`, `~/Library/LaunchAgents` |
| `PROMPTSTER_STATE_DIR` | `$SBX/home/.promptster-teams` | watcher state, progress, pidfiles |
| `PROMPTSTER_BUFFER_PATH` | `$SBX/home/.promptster-teams/buffer.jsonl` | the captured-event buffer |
| `PROMPTSTER_TEAMS_API_URL` / `PROMPTSTER_API_URL` | `http://127.0.0.1:9` | `https://api.promptster.ai` |
| `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `PROMPTSTER_CURSOR_HOME` | under `$SBX/home` | the real vendor dirs |
| `PROMPTSTER_TEAMS_NO_AUTO_UPDATE` | `1` | a GitHub releases probe on every `doctor`/`status` |

**`HOME` is the master seam, and it is not enough.** `PROMPTSTER_STATE_DIR`
redirects most capture state but NOT `credentials` (`ingest.CredentialsPath()`
→ `state.GlobalPromptsterDir()` → `os.UserHomeDir()`), NOT the launchd plist,
NOT `~/.claude`. Those all resolve through `os.UserHomeDir()`, i.e. `$HOME`.

And `launchctl` escapes even that: `internal/service/service_darwin.go` targets
`gui/<uid>`, keyed on the **UID**, not on `HOME`. So `$SBX/bin` holds no-op
shims for `launchctl`, `systemctl` and `schtasks` and is first on `PATH`.
`inspect` prints what they absorbed as `escapedCommandAttempts` — that list is
the evidence for autostart behaviour, and without the shim `run stop` would boot
the engineer's own capture job out of launchd. This is observed, not theory.

## 4. Evidence and proof standards

Evidence goes to `~/.promptster-verify/teams-cli/evidence/<timestamp>-<subcmd>/`
(override with `PROMPTSTER_VERIFY_EVIDENCE`). Report those paths in your summary.

A proof that does not meet these is not evidence:

- **Drive the real user path.** A user reaches a configured key through `login`,
  not by you writing `credentials` yourself. Hand-writing the state you are
  about to assert on proves nothing.
- **Capture the command AND the resulting state.** stdout is half the proof;
  `inspect` is the other half.
- **Verify the side effect.** After `login`, the credential must be *on disk*
  with the right token. After `stop`, the pid must be *gone*. A CLI that prints
  `✓` and writes nothing is the bug, not the proof.
- **Never assert on `exitCode` alone** — see `exitCodeIsNotAVerdict` above.
- **Cross-check two surfaces.** `status`, `doctor` and the files under
  `inspect` describe the same state. A defect that moves only one of them is
  exactly what a single-surface proof misses.
- **An unreachable ingest is CORRECT here.** `! ingest not reachable:
  127.0.0.1:9` and `couldn't reach 127.0.0.1:9 — saved anyway` are the sandbox
  working as designed. Never "fix" a proof by pointing it at the real API.

## 4b. Attest on the PR, or it will not merge

Automerge requires a verify-teams-cli attestation naming the **head commit**.
Marking a PR ready for review used to be the only signal that a proof happened,
and nothing read it. Post this after the drive, from the branch:

```bash
gh pr comment <N> --body "<!-- verified: verify-teams-cli sha=$(git rev-parse HEAD) -->
Drove: <what you drove>. Evidence: <the paths you captured>."
```

The SHA is the whole point. Push another commit and the attestation stops
matching, so the PR blocks until you re-verify — verifying commit A and merging
commit B is the failure this closes. Automerge also needs Greptile at 5/5 with
no P1s; see `scripts/automerge-decision.mjs`.

This is still your own claim. It does not prove you drove anything — it means
not driving is now a thing you had to assert, not a thing you could skip.

## 5. Cleanup

```bash
node $CT cleanup
```

Kills only pids the harness recorded from the sandbox pidfiles, and only after
confirming via `ps -o comm=` that the pid still belongs to **our** build — the
engineer's own daemon has the same process name, so killing by name would take
it out. Then it deletes the sandbox.

It does **not** delete evidence. Verified: after `cleanup`, `evidenceKept` still
lists every run directory. A verification whose artifacts were torn down with
the instance proved nothing.

## 6. Helpers

| Command | Does |
|---|---|
| `node $CT build` | compile to `bin/promptster-teams` with a real version stamp |
| `node $CT doctor` | build currency, isolation, strays, auth, version |
| `node $CT run <subcmd> [args…]` | one sandboxed run; stdout/stderr/exit + evidence |
| `node $CT inspect [file]` | list the sandbox home, or read one file under it |
| `node $CT cleanup` | kill only what we started, drop the sandbox, keep evidence |
| `node evals/run-eval.mjs --agent claude` | score how well an agent catches planted defects |

## 7. Keeping this skill honest

The feature map is only useful while it is true — an agent trusts it. When you
drive a subcommand and find the map wrong (a renamed flag, a changed output
string, a new gotcha), fix the map file in the same change. Same for eval
anchors: `node evals/run-eval.mjs --validate` re-checks that every planted
defect still matches its file exactly once.
