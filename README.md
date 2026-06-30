# promptster-teams-cli

On-device capture of AI-assisted coding for internal engineering teams.

`promptster-teams` tails the transcript files your AI coding tools already write
to disk (Claude Code, Codex), redacts secrets **on your machine**, signs each
event into a tamper-evident chain, and streams the result to your team's
Promptster backend so managers and engineers get live, accurate dashboards of
how AI is actually being used.

It is intentionally small and **fully auditable** — this repository is public so
your security team can read every line that decides what leaves a developer's
machine. There is no hidden telemetry, no keystroke logging, and no "integrity"
or anti-cheat instrumentation. (Promptster's hiring product is a separate, private
codebase; none of its assessment, honeypot, or behavioral-analysis logic exists
here. CI fails the build if any of it is reintroduced.)

## What it captures

Read straight from the AI tool's own transcript `.jsonl`:

- Prompts you send and the assistant's responses
- Tool calls (file edits, commands) and their results
- Per-request token usage and the exact model, for cost estimation
- Timestamps, so the timeline reflects when work happened

## What it does NOT capture

- Keystrokes, clipboard, screen, webcam/microphone
- Any file you didn't open through the AI tool
- Secrets and credentials — these are **redacted on-device before anything is
  sent** (see below)
- Behavioral signals: no typing-cadence, no paste detection, no authorship
  scoring. Capture is *content*, not surveillance of the developer.

## Redaction (on-device, before transmission)

Every captured line passes through two redaction layers locally, before it is
buffered, signed, or sent:

1. **Titus** (Praetorian's entropy-aware scanner, ~490 provider rules: AWS,
   GitHub, Anthropic/OpenAI, Slack, JWT, PEM private keys, …)
2. **Supplemental patterns** for `KEY=value` assignments, bearer headers, and
   other generic credential shapes.

If you need to verify exactly what would leave a machine, the local buffer at
`~/.promptster-teams/buffer.jsonl` holds the **already-redacted, already-signed**
event stream.

## Tamper-evident signing

On first run the CLI generates a per-device Ed25519 keypair, storing only the
private seed at `~/.promptster-teams/session.key` (mode 0600) — it never leaves
the machine. Every event is signed and chained to the previous event's
signature (`prevSig`). The **public** verifying key is sent with each ingest
request (`X-Promptster-Device-Pubkey`) so the backend can confirm the stream
wasn't altered in transit; the backend pins the first key it sees per device.

## Install

```sh
npm install -g @promptster/teams-cli   # default
```

(Or `curl -fsSL https://raw.githubusercontent.com/pa-arth/promptster-teams-cli/main/install.sh | sh`.)

## Usage

Your manager mints you a **developer key** (`PSE-XXXX-XXXX`) in the Promptster
dashboard. Paste it once with `login`, then `watch`:

```sh
promptster-teams login    # paste your PSE-XXXX-XXXX key (or: login --key PSE-…)
promptster-teams watch    # capture from the current repo (Ctrl-C to stop)
promptster-teams status   # show config + locally buffered event count
promptster-teams doctor   # check key, ingest reachability, transcript dir
```

The key identifies your sessions to your team and nothing else; it is the only
identity stamped on captured events. `login` stores it at
`~/.promptster-teams/credentials` (mode `0600`).

### Configuration

The developer key is resolved with this precedence: **`--key` flag → `PROMPTSTER_TEAMS_TOKEN` env → stored credentials file** (written by `login`). The ingest URL resolves the same way, defaulting to the hosted backend.

| Variable | Purpose |
|---|---|
| `PROMPTSTER_TEAMS_TOKEN` | Your developer key (`PSE-XXXX-XXXX`). Usually set via `login` instead. |
| `PROMPTSTER_TEAMS_API_URL` | Ingest base URL (default: hosted). Override for a self-hosted backend. |
| `PROMPTSTER_TEAMS_WATCH_DIR` | Directory whose transcripts to capture (default: cwd) |
| `PROMPTSTER_TEAMS_INGEST_PATH` | Override the ingest path (default `/v1/teams/ingest`) |

`watch` and `login` also accept `--key PSE-…` and `--api-url <url>` flags.

## Build

```sh
make build      # -> bin/promptster-teams
make test
make release    # cross-compile linux/darwin × amd64/arm64 -> dist/
```

## Status

This is the initial capture-only release: foreground `watch`, configurable
ingest, on-device redaction, and signed streaming. Persistent background
running, org → team → developer enrollment, customer-configurable redaction
rules, a metadata-only fidelity tier, and a "preview exactly what would be sent"
dry-run are on the roadmap.
