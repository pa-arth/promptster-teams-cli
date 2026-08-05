VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY  := promptster-teams
LDFLAGS := -ldflags="-s -w -X github.com/pa-arth/promptster-teams-cli/internal/version.Version=$(VERSION)"

.PHONY: build install release clean test sync-capture-allowlist

# Where promptster-backend is checked out. Override for a worktree:
#   make sync-capture-allowlist BACKEND=~/repos/promptster-backend/.claude/worktrees/xyz
BACKEND         ?= ../promptster-backend
CAPTURE_SRC     := $(BACKEND)/packages/shared/artifacts/capture-allowlist-canonical.json
CAPTURE_DEST    := internal/redact/testdata/capture-allowlist-canonical.json

build:
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/promptster-teams

install: build
	cp bin/$(BINARY) /usr/local/bin/

test:
	go test ./...

# Cross-compile the full matrix (linux/darwin/windows × amd64/arm64) — matches
# the npm build.js targets so every channel ships the same platforms.
release:
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64 ./cmd/promptster-teams
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64 ./cmd/promptster-teams
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-amd64 ./cmd/promptster-teams
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64 ./cmd/promptster-teams
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-windows-amd64.exe ./cmd/promptster-teams
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-windows-arm64.exe ./cmd/promptster-teams

clean:
	rm -rf bin/ dist/

# Re-copy the backend's capture-allowlist artifact, which
# internal/redact/allowlist_lockstep_test.go diffs projectFieldAllowlist against.
#
# This is the mechanical half of that test's failure message: a guard whose fix is
# "hand-edit some JSON until it's green" gets forged instead of followed, so the
# fix is one command and the artifact carries a checksum that makes hand-editing
# obvious. Regenerate on the backend side FIRST (`pnpm gen:capture-manifest`) —
# this target only copies, and a stale source copies a stale artifact.
sync-capture-allowlist:
	@test -f "$(CAPTURE_SRC)" || { \
	  echo "✗ $(CAPTURE_SRC) not found."; \
	  echo "  Point BACKEND at your promptster-backend checkout and regenerate there first:"; \
	  echo "    (cd \$$BACKEND && pnpm gen:capture-manifest)"; \
	  echo "    make sync-capture-allowlist BACKEND=/path/to/promptster-backend"; \
	  exit 1; }
	cp "$(CAPTURE_SRC)" "$(CAPTURE_DEST)"
	@echo "✓ synced $(CAPTURE_DEST)"
	go test ./internal/redact/ -run CaptureAllowlist
