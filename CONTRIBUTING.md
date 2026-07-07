# Contributing

Thanks for helping improve `promptster-teams`. It's a small, auditable Go CLI;
the bar for changes is **clarity and a clean privacy boundary** over cleverness.

## Prerequisites

- Go (see the version in [`go.mod`](go.mod)).
- Node 22+ only if you're touching the npm distribution wrapper under `npm/`.

## Build & test

```sh
make build      # -> bin/promptster-teams
make test       # go test ./...
go vet ./...
gofmt -l .      # must print nothing (CI fails on unformatted files)
```

Run the same checks CI runs before pushing:

```sh
go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
```

## The one rule that matters

**Source code, secrets, and PII must never leave the machine unredacted.** Any
change on the capture path has to preserve the layered guarantee:

1. `project.go` — a default-deny, per-kind field allowlist. New event kinds or
   fields are dropped unless explicitly allowlisted here.
2. `redact.go` — Titus secret scanning + supplemental credential patterns.
3. Only then is an event buffered, signed, and sent.

If you add a field to an event, assume it is **excluded** until you've added it
to the allowlist and covered it with a test in `project_test.go`. The presence
heartbeat has its own guard (`presence_test.go`) — don't grow it a
content-bearing field.

## Style & commits

- `gofmt`-clean, idiomatic Go. Match the surrounding code.
- Tests live beside the code they cover (`*_test.go`). Add tests for behavior
  changes, especially on the redaction path.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `chore:`, …) — the changelog and release notes derive from
  them.

## Pull requests

Open a PR against `main`. CI must be green: cross-platform build/test, race,
lint, gosec, and the "audit cleanliness" guard (which fails if any of the
sibling hiring product's anti-cheat/surveillance terms appear in the source).

## Reporting security issues

Do **not** open a public issue — see [SECURITY.md](SECURITY.md).
