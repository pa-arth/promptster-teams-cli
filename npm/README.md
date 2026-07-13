# @promptster/teams-cli

On-device, auditable AI-coding capture for internal engineering teams.

```sh
npm install -g @promptster/teams-cli

promptster-teams login             # paste your PSE-XXXX-XXXX key — capture starts automatically
promptster-teams autostart enable  # keep capturing across reboots (starts at login)
promptster-teams status            # confirm it's running
```

Background capture from `login` doesn't survive a reboot on its own —
`autostart enable` installs a per-OS login service (launchd / systemd / Task
Scheduler) so it comes back after every restart.

Tails Claude Code + Codex transcripts, redacts secrets on-device, signs each
event into a tamper-evident chain, and streams to your team's backend. Capture
is content, not surveillance — no keystroke logging, no behavioral analysis.

Source (public, auditable): https://github.com/pa-arth/promptster-teams-cli
