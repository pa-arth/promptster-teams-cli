## What & why

<!-- What does this change do, and why? -->

## Privacy-boundary checklist

The core guarantee is that **source code, secrets, and PII never leave the
machine unredacted** (see [CONTRIBUTING.md](../CONTRIBUTING.md)). Confirm:

- [ ] No new event field carries source content unless it is added to the
      default-deny allowlist in `internal/redact/project.go` **and** covered by a
      test in `internal/redact/project_test.go`.
- [ ] The presence heartbeat gained no content-bearing field
      (`internal/capture/presence_test.go` still passes).
- [ ] No secret/credential shape slips past `internal/redact/redact.go`.

## Checks

- [ ] `go test ./...` (and `-race`) pass
- [ ] `gofmt` / `staticcheck` clean
- [ ] Commit messages follow Conventional Commits
