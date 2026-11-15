# Cairn OBS: `make` builds the web UI and both binaries into bin/.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cairnobs/cairnobs/internal/version.Version=$(VERSION)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
DIST    := internal/webui/dist

.PHONY: all web go check test dev clean release
# web must finish before go embeds its output.
.NOTPARALLEL:

all: web go

# Build the Svelte app and copy it where obs-beacond embeds it from.
web:
	cd web && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run build
	find $(DIST) -mindepth 1 ! -name .keep -delete
	cp -r web/build/. $(DIST)/

go:
	$(GOBUILD) -o bin/obs-beacond ./cmd/obs-beacond
	$(GOBUILD) -o bin/obs-beacon-agent ./cmd/obs-beacon-agent

# What a reviewer runs before a PR: formatting, vet, tests, and the UI's type check.
check: test
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	cd web && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run check
	go run ./scripts/notices -check

test:
	go vet ./...
	GOOS=windows go vet ./cmd/obs-beacon-agent/ ./internal/certfiles/ ./internal/collect/ ./internal/units/ ./internal/backup/ ./internal/netmap/
	go test ./...

# Run obs-beacond, a helper and an agent against this machine. See docs/development.md.
dev: all
	./dev/run.sh

# A release build into dist/, as CI makes it: make release TAG=v0.1.0
release:
	scripts/build-release.sh $(TAG) dist

clean:
	rm -rf bin dist web/build web/.svelte-kit
	find $(DIST) -mindepth 1 ! -name .keep -delete
