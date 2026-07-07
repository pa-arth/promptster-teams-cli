# docs/

Durable, reviewer-facing documentation for `promptster-teams-cli`.

- [redaction-titus-vs-gitleaks.md](redaction-titus-vs-gitleaks.md) — why the
  on-device secret scanner is Praetorian's Titus rather than gitleaks.
- [repo-hardening.md](repo-hardening.md) — the CI, branch-protection, and
  supply-chain controls protecting this repository.

`docs/internal/` is git-ignored scratch space for point-in-time working notes
(e.g. engineering findings, e2e run logs). Nothing under it is committed, so it
never ships to anyone reading this repository.
