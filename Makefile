.DEFAULT_GOAL := build

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE     ?= $(shell TZ=UTC date +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

GOBIN ?= $(shell go env GOPATH)/bin

CLAUDE_DIR ?= $(HOME)/.claude
SKILL_DIR  ?= $(CLAUDE_DIR)/skills/ibkr
SKILL_SRC  ?= skills/ibkr

.PHONY: build install test test-pkg test-daemon clean install-skill uninstall-skill all check

build:
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/ibkr ./cmd/ibkr
	go build -ldflags '$(LDFLAGS)' -o bin/ibkrd ./cmd/ibkrd

install: build
	install -d $(GOBIN)
	install -m 0755 bin/ibkr $(GOBIN)/ibkr
	install -m 0755 bin/ibkrd $(GOBIN)/ibkrd
	@echo "Installed ibkr + ibkrd to $(GOBIN)"

# `make test` runs the full gate: check, then unit + integration.
test: check test-pkg test-daemon

# Binding pre-commit gate: formatting + go vet + staticcheck + govulncheck.
# Fails on stdlib vulnerabilities too — keep Go patched.
# One-time installs:
#   go install honnef.co/go/tools/cmd/staticcheck@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$unformatted"; \
		echo "fix with: gofmt -w ."; \
		exit 1; \
	fi
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck not on PATH; install: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }
	staticcheck ./...
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not on PATH; install: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

# Library tests. Some require a live gateway; CI should run these against a
# paper account with IBKR_LIVE_TESTS=1.
test-pkg:
	go test -count=1 -timeout=120s ./pkg/ibkr/...

# Daemon + CLI integration tests. -race is on for the daemon path because
# this layer carries the goroutines (subscriptions, idle timer, signal
# handlers); race detector earns its slot here.
test-daemon:
	go test -race -count=1 -timeout=180s ./internal/...
	go test -race -count=1 -timeout=360s ./test/integration/...

# Install the Claude Code skill bundle directly under ~/.claude/skills/.
# Dogfood path only — end users get the skill via `/plugin install ibkr`.
# Idempotent: re-running updates files in place.
install-skill: build
	install -d $(SKILL_DIR)
	install -m 0644 $(SKILL_SRC)/SKILL.md $(SKILL_DIR)/SKILL.md
	install -m 0644 $(SKILL_SRC)/schemas.md $(SKILL_DIR)/schemas.md
	@echo "Installed skill to $(SKILL_DIR)"
	@echo
	@echo "Prefer the plugin install path for end users:"
	@echo "  /plugin marketplace add osauer/ibkr"
	@echo "  /plugin install ibkr"
	@echo
	@echo "Then optional: ./install.sh --merge-settings to pre-allow read-only commands"
	@echo "(plugins cannot ship permissions; this stays the canonical permissions step)."

uninstall-skill:
	rm -rf $(SKILL_DIR)

clean:
	rm -rf bin

all: build test
