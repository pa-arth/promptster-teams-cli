# Repository hardening posture

The controls protecting this public repository, for reviewers auditing the
project's supply chain and process — not just its code.

## Enforced in-repo (visible here)

- **CI gates** (`.github/workflows/ci.yml`): cross-platform build/test matrix,
  race detector + coverage, `gofmt` + `staticcheck` lint, `gosec` SAST
  (SARIF → Security tab, gate on HIGH), and the "audit cleanliness" guard that
  fails the build if the sibling hiring product's surveillance/anti-cheat terms
  appear in source.
- **CodeQL** (`.github/workflows/codeql.yml`), `security-extended` query set.
- **Secret scanning of our own source** (`.github/workflows/secret-scan.yml`,
  gitleaks over full history) — plus GitHub-native secret scanning below.
- **Dependency CVE scanning** (`govulncheck`) on the latest Go; SARIF →
  Security tab. Non-blocking by design: standard-library findings track the Go
  release cycle and are cleared by a toolchain bump, so they must not wedge
  merges.
- **Dependabot** (`.github/dependabot.yml`): weekly updates for Go modules, the
  npm wrapper, and the (SHA-pinned) GitHub Actions.
- **CODEOWNERS** (`.github/CODEOWNERS`): the redaction/signing/ingest privacy
  boundary and the release/install supply chain require owner review to change.
- **Release integrity**: every release publishes `SHA256SUMS`; `install.sh`
  verifies the download before executing it; npm is published with build
  provenance (SLSA). GitHub Actions are pinned by commit SHA.

## Enabled in GitHub settings (not expressible in-repo)

- **Branch protection on `main`**: require PR + 1 approving review + Code Owner
  review, dismiss stale reviews, require linear history and conversation
  resolution, block force-pushes and deletions.
- **Secret scanning + push protection**: on (blocks committing credentials).
- **Private vulnerability reporting**: on — the intake channel referenced by
  [SECURITY.md](../SECURITY.md).
- **Dependabot alerts + security updates**: on.
- **npm account**: 2FA required; publish uses a scoped automation token.

## Not applicable

- Required status checks are attached to branch protection once the CI
  workflows land on `main` (a check context must exist before it can be
  required).
