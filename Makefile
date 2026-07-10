VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY  := promptster-teams
LDFLAGS := -ldflags="-s -w -X github.com/pa-arth/promptster-teams-cli/internal/version.Version=$(VERSION)"

.PHONY: build install release clean test

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
