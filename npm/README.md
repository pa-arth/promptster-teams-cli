# @promptster/teams-cli

On-device, auditable AI-coding capture for internal engineering teams.

```sh
npm install -g @promptster/teams-cli

export PROMPTSTER_TEAMS_API_URL="https://<your-team-ingest-host>"
export PROMPTSTER_TEAMS_TOKEN="<your-org/device-token>"

promptster-teams doctor
promptster-teams watch
```

Tails Claude Code + Codex transcripts, redacts secrets on-device, signs each
event into a tamper-evident chain, and streams to your team's backend. Capture
is content, not surveillance — no keystroke logging, no behavioral analysis.

Source (public, auditable): https://github.com/pa-arth/promptster-teams-cli
