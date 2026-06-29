VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY  := promptster-teams
LDFLAGS := -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: build install release clean test

build:
	go build $(LDFLAGS) -o bin/$(BINARY) .

install: build
	cp bin/$(BINARY) /usr/local/bin/

test:
	go test ./...

# Cross-compile the same 4-platform matrix the hiring CLI ships.
release:
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64 .

clean:
	rm -rf bin/ dist/
