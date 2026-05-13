.DEFAULT_GOAL := help

# `--match='v*'` excludes the `ibkr--vX.Y.Z` plugin tags created by
# `claude plugin tag` so the binary always stamps itself with the
# nearest binary-release tag (e.g. v0.4.4) and not the lexicographically
# earlier plugin tag at the same commit.
VERSION  ?= $(shell git describe --tags --match='v*' --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE     ?= $(shell TZ=UTC date +%Y-%m-%dT%H:%M:%SZ)

# `-s -w` strip the external symbol table and DWARF debug info. Cuts the
# binary by ~32% (9.6 MB → 6.5 MB on darwin/arm64). Go runtime keeps its
# own function metadata so panic stack traces remain readable; what's
# lost is delve symbolication, `go tool nm`/`objdump`, and external
# profilers that read external symbols. Startup time is unchanged —
# this is a size optimisation, not a speed one.
STRIP_LDFLAGS = -s -w

LDFLAGS = $(STRIP_LDFLAGS) -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Install location for `make install`. Defaults to ~/.local/bin (XDG
# user-local convention; usually already on PATH). Override for a system
# install: make install PREFIX=/usr/local (needs sudo). Note: $GOBIN is
# the wrong target here — that's a Go-developer convention for source
# tools, but ibkr is an end-user CLI binary and shouldn't require Go to
# be installed at runtime.
PREFIX ?= $(HOME)/.local

CLAUDE_DIR ?= $(HOME)/.claude
SKILL_DIR  ?= $(CLAUDE_DIR)/skills/ibkr
SKILL_SRC  ?= skills/ibkr

MAIN_BRANCH ?= main

.PHONY: help build install uninstall test test-pkg test-daemon clean install-skill uninstall-skill all check fmt release release-binaries release-publish version plugin-check parity-check

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; print "Available targets (default: help):\n"} \
		/^[a-zA-Z][a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@echo
	@echo "Common flow:  make fmt && make check && make test && make build"
	@echo "Release flow: make release RELEASE_VERSION=vX.Y.Z   (clean tree + HEAD == origin/$(MAIN_BRANCH))"
	@echo "              tags + pushes + cross-compiles + creates GitHub Release with binaries attached"

build: ## Compile bin/ibkr with version stamped via ldflags
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/ibkr ./cmd/ibkr

install: build ## Install ibkr to $(PREFIX)/bin (default ~/.local/bin)
	install -d $(PREFIX)/bin
	install -m 0755 bin/ibkr  $(PREFIX)/bin/ibkr
	@echo "Installed ibkr to $(PREFIX)/bin"
	@echo "Make sure $(PREFIX)/bin is on your PATH."

uninstall: ## Remove ibkr from $(PREFIX)/bin
	rm -f $(PREFIX)/bin/ibkr
	@echo "Removed ibkr from $(PREFIX)/bin"

test: check test-pkg test-daemon ## Full gate: check + pkg tests + daemon/integration tests (-race)

# Binding pre-commit gate: formatting + go vet + staticcheck + govulncheck +
# plugin manifest validation. Fails on stdlib vulnerabilities too — keep Go
# patched.
# One-time installs:
#   go install honnef.co/go/tools/cmd/staticcheck@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
#
# CHECK_DEPS gates the optional pieces of the check matrix. Default is the
# full strict gate (plugin-check + parity-check). CI without the `claude`
# CLI on PATH overrides with CHECK_DEPS=parity-check — the MCP↔CLI drift
# gate (parity-check) is what we cannot skip; plugin-manifest validation
# is recoverable because the schema is small and changes go through PR
# review anyway.
CHECK_DEPS ?= plugin-check parity-check
check: $(CHECK_DEPS) ## gofmt + go vet + staticcheck + govulncheck + plugin-check + parity-check (binding pre-commit gate)
	@# `gofmt -l .` walks every subdirectory and trips on gitignored paths
	@# (Claude Code agent worktrees, /dist, etc.). `git ls-files` respects
	@# .gitignore by listing tracked + untracked-but-not-ignored files —
	@# the right scope for a pre-commit format gate.
	@unformatted=$$(git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -l); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$unformatted"; \
		echo "fix with: make fmt"; \
		exit 1; \
	fi
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck not on PATH; install: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }
	staticcheck ./...
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not on PATH; install: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

# Validate the Claude Code plugin + marketplace manifests with the official
# `claude plugin validate` tool. The Go test in internal/cli that asserts
# every cli.commands name appears in SKILL.md catches the drift class
# `claude plugin validate` doesn't see (it checks the JSON, not the prose).
plugin-check: ## Validate plugin/marketplace manifests with `claude plugin validate`
	@command -v claude >/dev/null 2>&1 || { echo "claude CLI not on PATH; install Claude Code or skip with: make check plugin-check= "; exit 1; }
	claude plugin validate .

# Drift gate for the MCP surface: TestParity in internal/mcp asserts that
# every cli.Commands() entry has a matching ibkr_<name> MCP tool (or is on
# the documented exclude list). Cheap enough to live in the pre-commit gate.
parity-check: ## Verify MCP tool inventory matches the CLI surface
	go test -count=1 -run 'TestParity|TestNoTradingTools|TestSchemasAreValidJSON' ./internal/mcp/

fmt: ## Apply gofmt -w to every tracked / non-gitignored .go file
	@# Same scope as `make check` so `make fmt && make check` is idempotent.
	git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -w

# Library tests. Some require a live gateway; CI should run these against a
# paper account with IBKR_LIVE_TESTS=1. Timeout sized for CI's slower
# runners — local runs typically finish in <30s.
test-pkg: ## Run pkg/ibkr/... tests (TWS protocol library)
	go test -count=1 -timeout=180s ./pkg/ibkr/...

# Daemon + CLI integration tests. -race is on for the daemon path because
# this layer carries the goroutines (subscriptions, idle timer, signal
# handlers); race detector earns its slot here. Integration tests skip
# cleanly when no IBKR gateway is reachable.
test-daemon: ## Run internal/... and test/integration/... under -race
	go test -race -count=1 -timeout=240s ./internal/...
	go test -race -count=1 -timeout=420s ./test/integration/...

# Install the Claude Code skill bundle directly under ~/.claude/skills/.
# Dogfood path only — end users get the skill via `/plugin install ibkr`.
# Idempotent: re-running updates files in place.
install-skill: build ## Install SKILL.md to ~/.claude/skills/ibkr/ (dogfood path)
	install -d $(SKILL_DIR)
	install -m 0644 $(SKILL_SRC)/SKILL.md $(SKILL_DIR)/SKILL.md
	install -m 0644 $(SKILL_SRC)/schemas.md $(SKILL_DIR)/schemas.md
	@echo "Installed skill to $(SKILL_DIR)"
	@echo
	@echo "Prefer the plugin install path for end users:"
	@echo "  /plugin marketplace add osauer/ibkr"
	@echo "  /plugin install ibkr"
	@echo
	@echo "For a global Bash(ibkr ...) allowlist, copy settings/ibkr.settings.json"
	@echo "into your ~/.claude/settings.json by hand (the SKILL frontmatter already"
	@echo "grants the read patterns when the skill is active)."

uninstall-skill: ## Remove the dogfood skill install at ~/.claude/skills/ibkr/
	rm -rf $(SKILL_DIR)

clean: ## Remove bin/ and dist/
	rm -rf bin dist

# Cross-compile release tarballs for the OS/arch matrix this project actually
# supports. The daemon uses Unix-only primitives (Setsid, flock, AF_UNIX
# sockets); Windows is intentionally out of scope and would require a port.
# Each tarball contains the stamped binary plus LICENSE + README.md so a
# colleague can extract, drop into ~/.local/bin, and run.
RELEASE_TARGETS = darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
DIST_DIR = dist

# Release builds resolve the commit hash from the *tag*, not from HEAD,
# so the binary's stamped commit matches the git tag a colleague would
# `git checkout`. -buildvcs=false suppresses runtime/debug.BuildInfo's
# vcs.modified flag — the -ldflags vars are authoritative for releases,
# and the dirty/clean signal is only useful for in-tree dev builds.
release-binaries: ## Cross-compile release tarballs into dist/ — needs RELEASE_VERSION=vX.Y.Z
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-binaries: RELEASE_VERSION is required, e.g. make release-binaries RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if ! git rev-parse --verify --quiet $(RELEASE_VERSION) >/dev/null; then \
		echo "release-binaries: tag $(RELEASE_VERSION) does not exist; run \`make release RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	$(eval RELEASE_COMMIT := $(shell git rev-parse $(RELEASE_VERSION)^{commit}))
	$(eval RELEASE_LDFLAGS := $(STRIP_LDFLAGS) -X main.version=$(RELEASE_VERSION) -X main.commit=$(RELEASE_COMMIT) -X main.date=$(DATE))
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	@for target in $(RELEASE_TARGETS); do \
		os=$$(echo $$target | cut -d- -f1); \
		arch=$$(echo $$target | cut -d- -f2); \
		base=ibkr-$(RELEASE_VERSION)-$$target; \
		stage=$(DIST_DIR)/$$base; \
		echo "==> $$os/$$arch"; \
		mkdir -p $$stage; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -buildvcs=false -ldflags '$(RELEASE_LDFLAGS)' -o $$stage/ibkr ./cmd/ibkr || exit 1; \
		cp LICENSE README.md $$stage/; \
		( cd $(DIST_DIR) && tar -czf $$base.tar.gz $$base ); \
		rm -rf $$stage; \
	done
	@( cd $(DIST_DIR) && shasum -a 256 *.tar.gz > SHA256SUMS )
	@echo
	@echo "Built artefacts in $(DIST_DIR)/:"
	@ls -la $(DIST_DIR)

# Compose the GitHub Release notes by concatenating the install header
# template (with __VERSION__ substituted) and the matching CHANGELOG.md
# section, then create the GitHub Release with the dist/ tarballs +
# SHA256SUMS attached. Marks the new release as latest.
release-publish: ## Create the GitHub Release page (notes + binaries) — RELEASE_VERSION required
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-publish: RELEASE_VERSION is required, e.g. make release-publish RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if [ ! -d "$(DIST_DIR)" ] || [ ! -f "$(DIST_DIR)/SHA256SUMS" ]; then \
		echo "release-publish: $(DIST_DIR)/ missing or empty; run \`make release-binaries RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	@command -v gh >/dev/null 2>&1 || { echo "release-publish: gh CLI not on PATH; brew install gh" >&2; exit 1; }
	@notes=$$(mktemp -t ibkr-release-notes.XXXXXX) && \
	trap 'rm -f $$notes' EXIT && \
	sed "s/__VERSION__/$(RELEASE_VERSION)/g" .github/release-notes-template.md > $$notes && \
	awk -v ver='$(RELEASE_VERSION)' '/^## v[0-9]/{ in_section = ($$0 ~ "^## " ver " ") ; if(in_section){ next } } in_section' CHANGELOG.md >> $$notes && \
	title="$${MESSAGE:-$(RELEASE_VERSION)}" && \
	gh release create $(RELEASE_VERSION) --notes-file $$notes --title "$$title" --latest $(DIST_DIR)/*.tar.gz $(DIST_DIR)/SHA256SUMS

all: build test ## build + test

version: ## Print the version string the next build would embed
	@echo "VERSION=$(VERSION)"
	@echo "COMMIT=$(COMMIT)"
	@echo "DATE=$(DATE)"

# Tag and push a new release. RELEASE_VERSION is a separate variable from
# build-time VERSION (which auto-derives from git describe and is always
# populated) so the "missing arg" guard actually fires.
#
# Guards against the foot-guns:
# - missing RELEASE_VERSION arg
# - dirty working tree (would bake "-dirty" into the binary)
# - HEAD not synced with origin/<MAIN_BRANCH> (tag would point at a commit
#   GitHub doesn't have)
# - tag already exists locally or on origin
# Sequence: gate → create annotated tag → rebuild binaries (now stamped
# with the new tag) → push tag. Does NOT push commits — push those first.
release: ## Tag and push a release: make release RELEASE_VERSION=vX.Y.Z [MESSAGE="..."]
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release: RELEASE_VERSION is required, e.g. make release RELEASE_VERSION=v0.3.1" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "release: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@expected=$$(echo "$(RELEASE_VERSION)" | sed 's/^v//'); \
	if ! grep -q "\"version\": \"$$expected\"" .claude-plugin/plugin.json; then \
		echo "release: .claude-plugin/plugin.json version != $$expected — bump it before releasing so plugin tag agrees with binary tag" >&2; \
		grep '"version"' .claude-plugin/plugin.json >&2; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "release: working tree is dirty; commit or stash first" >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@head=$$(git rev-parse HEAD); \
	main=$$(git rev-parse origin/$(MAIN_BRANCH) 2>/dev/null) || { \
		echo "release: origin/$(MAIN_BRANCH) ref missing locally; run 'git fetch origin $(MAIN_BRANCH)' first" >&2; \
		exit 1; \
	}; \
	if [ "$$head" != "$$main" ]; then \
		echo "release: HEAD ($$head) does not match origin/$(MAIN_BRANCH) ($$main); push your commits first" >&2; \
		exit 1; \
	fi
	@if git rev-parse --verify --quiet $(RELEASE_VERSION) >/dev/null; then \
		echo "release: tag $(RELEASE_VERSION) already exists locally" >&2; \
		exit 1; \
	fi
	@if git ls-remote --tags --exit-code origin $(RELEASE_VERSION) >/dev/null 2>&1; then \
		echo "release: tag $(RELEASE_VERSION) already exists on origin" >&2; \
		exit 1; \
	fi
	$(MAKE) check test
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	$(MAKE) build
	git push origin $(RELEASE_VERSION)
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	claude plugin tag . --push --message "$$msg"
	$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION)
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	$(MAKE) release-publish RELEASE_VERSION=$(RELEASE_VERSION) MESSAGE="$$msg"
	@echo
	@echo "Released $(RELEASE_VERSION):"
	@echo "  https://github.com/osauer/ibkr/releases/tag/$(RELEASE_VERSION)"
	@echo
	@echo "Verify:"
	@echo "  bin/ibkr version"
	@echo "  gh release view $(RELEASE_VERSION) --json assets -q '.assets[].name'"
	@echo "  gh api repos/osauer/ibkr/git/refs/tags/ibkr--$(RELEASE_VERSION) --jq '.object.sha'"
