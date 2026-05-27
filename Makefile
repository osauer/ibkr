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
RELEASE_TEST_JOBS ?= 3
MCPB_PACKAGE ?= @anthropic-ai/mcpb@2.1.2
MCP_PUBLISHER ?= $(if $(wildcard bin/mcp-publisher),bin/mcp-publisher,mcp-publisher)

.PHONY: help build install uninstall test test-pkg test-daemon clean install-skill uninstall-skill all check fmt release release-binaries release-mcpb release-checksums release-registry-server registry-publish release-publish release-verify release-smoke smoke smoke-build smoke-only version plugin-check parity-check modernize modernize-check refresh-spx-members hook-regex-check changelog-lint changelog-stub discovery-check release-prep

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
# staticcheck and govulncheck are pinned as go.mod tool dependencies and
# invoked via `go tool`, so CI and local runs use the same versions.
#
# CHECK_DEPS gates the optional pieces of the check matrix. Default is the
# full strict gate (plugin-check + parity-check). CI without the `claude`
# CLI on PATH overrides with CHECK_DEPS=parity-check — the MCP↔CLI drift
# gate (parity-check) is what we cannot skip; plugin-manifest validation
# is recoverable because the schema is small and changes go through PR
# review anyway.
CHECK_DEPS ?= plugin-check parity-check
check: $(CHECK_DEPS) modernize-check docs-check discovery-check ## gofmt + go vet + staticcheck + govulncheck + modernize-check + plugin-check + parity-check + docs-check + discovery-check (binding pre-commit gate)
	@# `gofmt -l .` walks every subdirectory and trips on gitignored paths
	@# (Claude Code agent worktrees, /dist, etc.). `git ls-files` respects
	@# .gitignore by listing tracked + untracked-but-not-ignored files —
	@# the right scope for a pre-commit format gate.
	@#
	@# Filter out paths git knows about but that don't exist on disk
	@# (staged-for-deletion mid-commit), otherwise gofmt prints
	@# `lstat …: no such file or directory` to stderr for each one.
	@unformatted=$$( \
		git ls-files --cached --others --exclude-standard '*.go' | \
		while IFS= read -r f; do [ -e "$$f" ] && printf '%s\n' "$$f"; done | \
		xargs gofmt -l \
	); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$unformatted"; \
		echo "fix with: make fmt"; \
		exit 1; \
	fi
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...

# Validate the Claude Code plugin + marketplace manifests with the official
# `claude plugin validate` tool. The Go test in internal/cli that asserts
# every cli.commands name appears in SKILL.md catches the drift class
# `claude plugin validate` doesn't see (it checks the JSON, not the prose).
plugin-check: ## Validate plugin/marketplace manifests with `claude plugin validate`
	@command -v claude >/dev/null 2>&1 || { echo "claude CLI not on PATH; install Claude Code or skip with: make check plugin-check= "; exit 1; }
	claude plugin validate .
	@$(MAKE) --no-print-directory hook-regex-check

# Single-source gate for the trading-verb defense. The PreToolUse hook is
# duplicated across hooks/hooks.json (the bundled plugin) and
# settings/ibkr.settings.json (the user-copyable settings template); both
# must run the same jq regex against `.tool_input.command`, otherwise the
# defense drifts between distribution paths. Strips the human-readable
# label prefix before diffing so the two commands can name themselves
# distinctly in their failure-closed messages.
hook-regex-check: ## Ensure plugin + settings PreToolUse regexes match
	@command -v jq >/dev/null 2>&1 || { echo "jq missing on PATH; install jq or skip"; exit 1; }
	@plugin=$$(jq -r '.hooks.PreToolUse[0].hooks[0].command' hooks/hooks.json | sed "s/'ibkr plugin: /'LABEL: /"); \
	settings=$$(jq -r '.hooks.PreToolUse[0].hooks[0].command' settings/ibkr.settings.json | sed "s/'ibkr settings: /'LABEL: /"); \
	if [ "$$plugin" != "$$settings" ]; then \
		echo "hooks/hooks.json and settings/ibkr.settings.json PreToolUse commands differ:" >&2; \
		echo "  plugin:   $$plugin" >&2; \
		echo "  settings: $$settings" >&2; \
		exit 1; \
	fi

# Drift gate for the MCP surface: TestParity in internal/mcp asserts that
# every cli.Commands() entry has a matching ibkr_<name> MCP tool (or is on
# the documented exclude list). TestStreamingParity is the streaming-
# resource counterpart — it pins the ibkr://… template inventory the
# server actually exposes. Cheap enough to live in the pre-commit gate.
parity-check: ## Verify MCP tool inventory matches the CLI surface
	go test -count=1 -run 'TestParity|TestStreamingParity|TestNoTradingTools|TestSchemasAreValidJSON' ./internal/mcp/

# Idiom-drift gate. `go fix -diff` is the toolchain-native fixer (tracks the
# Go version pinned in go.mod); `go tool modernize` runs the broader gopls
# analyzer suite (range N, wg.Go, b.Loop, maps.Copy, SplitSeq, new(expr), …).
# Version of modernize is pinned via the `tool` directive in go.mod, so this
# gate is reproducible without an `@latest` install step.
#
# Stream discipline + chatter filter:
#   - `go fix -diff` writes the unified diff to stdout, download chatter to
#     stderr → capture stdout (no redirect needed; stderr stays visible).
#   - `go tool modernize` writes diagnostics AND `go: downloading …` lines to
#     stderr (the latter when go.mod's tool deps aren't cached — every fresh
#     CI run hits this). Same stream means we can't separate by redirection;
#     instead we capture stderr via stream-swap and grep the chatter out.
# A future kindness: `go: downloading` is the only chatter we've observed, so
# if the tool ever grows another routine stderr message, extend the filter
# explicitly instead of weakening it.
modernize-check: ## go fix -diff + modernize gate (Go idiom drift vs go.mod's go version)
	@out=$$(go fix -diff ./...); \
	if [ -n "$$out" ]; then \
		echo "go fix found pending changes:"; echo "$$out"; \
		echo "apply with: make modernize"; exit 1; \
	fi
	@out=$$(go tool modernize ./... 2>&1 1>/dev/null | grep -v '^go: downloading'); \
	if [ -n "$$out" ]; then \
		echo "modernize found pending changes:"; echo "$$out"; \
		echo "apply with: make modernize"; exit 1; \
	fi

modernize: ## Apply go fix + modernize rewrites in place
	go fix ./...
	go tool modernize -fix ./...

# Regenerate the docs/reference/*.md pages from their generators. The
# generators live under scripts/docgen/; each emits one markdown file
# from the canonical source (Go struct tags + `// docgen:env` comments
# for config-ref; the internal/mcp.Tools registry for mcp-tools). Run
# this after editing internal/config/config.go, internal/mcp/tools.go,
# or adding/changing a // docgen:env comment, and commit the diff
# alongside the source change. `make docs-check` enforces no drift.
docs-regen: ## Regenerate docs/reference/*.md from generators
	go run ./scripts/docgen/config-ref
	go run ./scripts/docgen/mcp-tools
	go run ./scripts/docgen/site-html

# docs-check is the CI gate: regenerate to a tempfile, diff against the
# checked-in copy, fail if they differ. Catches the "I changed a struct
# tag but forgot to regen" case. Wired into `make check` so it cannot
# be skipped. Uses POSIX tempfiles (not bash process substitution) so
# the recipe runs under /bin/sh on every host.
docs-check: ## Verify checked-in docs/reference/*.md match what the generators emit
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	fail=0; \
	for gen in config-ref mcp-tools; do \
		case $$gen in \
			config-ref) ref=docs/reference/config.md ;; \
			mcp-tools) ref=docs/reference/mcp-tools.md ;; \
		esac; \
		go run ./scripts/docgen/$$gen -o "$$tmp/$$gen.md" || exit 1; \
		if ! diff -u "$$ref" "$$tmp/$$gen.md" > /dev/null 2>&1; then \
			echo "docs-check: $$ref out of date; run \`make docs-regen\`"; \
			diff -u "$$ref" "$$tmp/$$gen.md" || true; \
			fail=1; \
		fi; \
	done; \
	go run ./scripts/docgen/site-html -check || fail=1; \
	exit $$fail

discovery-check: ## Verify public discovery metadata, sitemap, llms.txt, and JSON-LD stay in sync
	go run ./scripts/discovery-check

release-prep: ## Update public discovery metadata before release; needs RELEASE_VERSION=vX.Y.Z
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-prep: RELEASE_VERSION is required, e.g. make release-prep RELEASE_VERSION=v1.0.9" >&2; \
		exit 1; \
	fi
	go run ./scripts/release-prep $(RELEASE_VERSION)

# Pull the current S&P-500 membership list from Wikipedia and rewrite
# internal/breadth/spx/members_data.go. Invoked by `make release` so a
# freshly-tagged binary always carries a current list; a release that
# would change the file fails-closed with a "commit and re-run" message
# so the tag and binary stay coherent (see the refresh-spx-members
# block inside `release:` for the dirty-tree guard).
refresh-spx-members: ## Refresh internal/breadth/spx/members_data.go from Wikipedia
	go run ./scripts/refresh-spx-members

fmt: ## Apply gofmt -w to every tracked / non-gitignored .go file
	@# Same scope as `make check` so `make fmt && make check` is idempotent.
	git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -w

# Library tests. The pkg/ibkr suite is fully hermetic — wire-level
# captured fixtures (wire_fixtures_test.go, scanner_test.go) plus net.Pipe-
# driven handshake tests; no live gateway is required. The end-to-end
# gateway path is covered by test/integration. Timeout sized for CI's
# slower runners — local runs typically finish in <30s.
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
RELEASE_BUILD_JOBS ?= 4

# Release builds resolve the commit hash from the *tag*, not from HEAD,
# so the binary's stamped commit matches the git tag a colleague would
# `git checkout`. -buildvcs=false suppresses runtime/debug.BuildInfo's
# vcs.modified flag — the -ldflags vars are authoritative for releases,
# and the dirty/clean signal is only useful for in-tree dev builds.
release-verify: ## Smoke-test the local bin/ibkr against a live gateway (called by `make release`)
	@# Standalone so a release-flow failure can be diagnosed in isolation:
	@#   make release-verify RELEASE_VERSION=v0.15.1
	@# The script spawns an isolated daemon under /tmp, runs a fixed
	@# matrix (version, status, account, positions, quote SPY), asserts
	@# the v0.15+ data_type contract on each surface, and tears the
	@# daemon down. Requires a reachable IBKR Gateway — the gate is
	@# binding by design (see release-verify.sh).
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-verify: RELEASE_VERSION is required, e.g. make release-verify RELEASE_VERSION=v0.15.1" >&2; \
		exit 1; \
	fi
	@if [ ! -x bin/ibkr ]; then \
		echo "release-verify: bin/ibkr missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	./scripts/release-verify.sh bin/ibkr $(RELEASE_VERSION)

release-smoke: smoke-build ## Release gate: JSON contract + wire smoke in one live-gateway daemon session
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-smoke: RELEASE_VERSION is required, e.g. make release-smoke RELEASE_VERSION=v0.15.1" >&2; \
		exit 1; \
	fi
	@if [ ! -x bin/ibkr ]; then \
		echo "release-smoke: bin/ibkr missing — run 'make build VERSION=$(RELEASE_VERSION)' first" >&2; \
		exit 1; \
	fi
	IBKR_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/release-smoke.sh bin/ibkr $(RELEASE_VERSION) bin/wire-assert

smoke-build: ## Compile the bin/wire-assert helper used by `make smoke`
	@mkdir -p bin
	go build -o bin/wire-assert ./cmd/wire-assert

# Run wire-smoke against the *existing* bin/ibkr without rebuilding it.
# The release flow uses this so it can exercise the version-stamped
# binary produced by `make build VERSION=$(RELEASE_VERSION)`, instead
# of clobbering that stamp with a `git describe` rebuild via the smoke
# dep chain.
#
# Drives bin/ibkr against a live gateway with the wire interceptor
# enabled and asserts per-command protocol-level invariants — catches
# bugs the unit suite can't see (e.g. the v0.24.x productionLegFetcher
# bug where the gateway sent the right ticks but the daemon read the
# wrong field).
#
# SMOKE_STRICT controls the no-gateway posture (forwarded to the script
# as IBKR_SMOKE_STRICT):
#   SMOKE_STRICT=0 (default) → SKIP cleanly when no gateway is up; lets
#                              user-invoked `make smoke` work on a laptop
#                              without paper-account IBKR access.
#   SMOKE_STRICT=1 → FAIL when no gateway is up; the release path passes
#                    this so a vanished gateway can't silently bypass
#                    the wire gate ("we must exercise TWS — not doing so
#                    is a failed release").
SMOKE_STRICT ?= 0

# SPX_EXPECTED_REACHABLE — default ON in this repo because this is the
# dev machine with CBOE OPRA entitlement; the user's standing guardrail
# (per docs/design/gamma-spx-coverage.md §11.2): "no SPX data would be
# a bug on my setup." If `ibkr gamma --only=spx` returns the
# entitlement-skipped banner, fail loudly rather than silently passing
# the smoke. Override with `make smoke SPX_EXPECTED_REACHABLE=0` on
# accounts that legitimately lack SPX entitlement.
SPX_EXPECTED_REACHABLE ?= 1

smoke-only: smoke-build ## Run wire smoke against existing bin/ibkr (no rebuild); SMOKE_STRICT=1 makes no-gateway a failure
	@if [ ! -x bin/ibkr ]; then \
		echo "smoke-only: bin/ibkr missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	IBKR_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/wire-smoke.sh bin/ibkr bin/wire-assert

smoke: build smoke-only ## Wire-level smoke vs. a live gateway (rebuilds bin/ibkr; SKIP if no gateway)

release-binaries: ## Cross-compile release tarballs + MCPB into dist/ — needs RELEASE_VERSION=vX.Y.Z
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-binaries: RELEASE_VERSION is required, e.g. make release-binaries RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if ! git rev-parse --verify --quiet $(RELEASE_VERSION) >/dev/null; then \
		echo "release-binaries: tag $(RELEASE_VERSION) does not exist; run \`make release RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	$(eval RELEASE_COMMIT := $(shell git rev-parse $(RELEASE_VERSION)^{commit}))
	$(eval RELEASE_DATE := $(shell git show -s --format=%cI $(RELEASE_VERSION)^{commit}))
	$(eval RELEASE_LDFLAGS := $(STRIP_LDFLAGS) -X main.version=$(RELEASE_VERSION) -X main.commit=$(RELEASE_COMMIT) -X main.date=$(RELEASE_DATE))
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	@printf '%s\n' $(RELEASE_TARGETS) | xargs -P $(RELEASE_BUILD_JOBS) -I {} ./scripts/build-release-target.sh {} "$(RELEASE_VERSION)" "$(RELEASE_LDFLAGS)" "$(DIST_DIR)"
	$(MAKE) release-mcpb RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-checksums RELEASE_VERSION=$(RELEASE_VERSION)
	@echo
	@echo "Built artefacts in $(DIST_DIR)/:"
	@ls -la $(DIST_DIR)

release-mcpb: ## Build the cross-platform MCP Bundle from release tarballs
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-mcpb: RELEASE_VERSION is required, e.g. make release-mcpb RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	MCPB_PACKAGE=$(MCPB_PACKAGE) ./scripts/build-mcpb.sh "$(RELEASE_VERSION)" "$(DIST_DIR)" "$(RELEASE_TARGETS)"

release-checksums: ## Sign SHA256SUMS for tarballs and MCPB assets
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-checksums: RELEASE_VERSION is required, e.g. make release-checksums RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	@if ! ls $(DIST_DIR)/ibkr-$(RELEASE_VERSION)-*.tar.gz >/dev/null 2>&1; then \
		echo "release-checksums: missing release tarballs in $(DIST_DIR)" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-checksums: missing $(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb; run make release-mcpb" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr.mcpb" ]; then \
		echo "release-checksums: missing $(DIST_DIR)/ibkr.mcpb; run make release-mcpb" >&2; \
		exit 1; \
	fi
	@( cd $(DIST_DIR) && shasum -a 256 ibkr-$(RELEASE_VERSION)-*.tar.gz ibkr-$(RELEASE_VERSION).mcpb ibkr.mcpb > SHA256SUMS )
	@command -v gpg >/dev/null 2>&1 || { \
		echo "release-checksums: gpg not on PATH — required to sign SHA256SUMS for v1.0+ releases" >&2; \
		exit 1; \
	}
	@expected_fp=$$(awk -F\" '/ReleaseSigningKeyFingerprint =/{print $$2; exit}' internal/update/keyring.go); \
	gpg --list-secret-keys --with-colons "$$expected_fp" >/dev/null 2>&1 || { \
		echo "release-checksums: signing key $$expected_fp is not in the local gpg keyring — see SECURITY.md for setup" >&2; \
		exit 1; \
	}; \
	echo "==> signing SHA256SUMS with $$expected_fp"; \
	( cd $(DIST_DIR) && gpg --batch --yes --local-user "$$expected_fp" --armor --detach-sign --output SHA256SUMS.asc SHA256SUMS ) || exit 1; \
	( cd $(DIST_DIR) && gpg --verify SHA256SUMS.asc SHA256SUMS ) >/dev/null 2>&1 || { \
		echo "release-checksums: produced SHA256SUMS.asc but it failed self-verify — aborting" >&2; \
		exit 1; \
	}

release-registry-server: ## Generate and validate dist/server.json for MCP Registry publishing
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-registry-server: RELEASE_VERSION is required, e.g. make release-registry-server RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-registry-server: missing $(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb; run make release-mcpb" >&2; \
		exit 1; \
	fi
	go run ./scripts/release-registry-server $(RELEASE_VERSION) "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" "$(DIST_DIR)/server.json"
	$(MCP_PUBLISHER) validate "$(DIST_DIR)/server.json"

registry-publish: release-registry-server ## Publish dist/server.json to the MCP Registry
	$(MCP_PUBLISHER) publish "$(DIST_DIR)/server.json"

# Compose the GitHub Release notes by substituting __VERSION__ and
# __HIGHLIGHTS__ in the install-header template, then appending the
# matching CHANGELOG.md entry. __HIGHLIGHTS__ is pulled from the entry's
# `### What's new` section, so the release body's top stanza is mechanically
# derived from CHANGELOG — no second place to drift. Marks the new release
# as latest.
release-publish: ## Create the GitHub Release page (notes + binaries) — RELEASE_VERSION required
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-publish: RELEASE_VERSION is required, e.g. make release-publish RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if [ ! -d "$(DIST_DIR)" ] || [ ! -f "$(DIST_DIR)/SHA256SUMS" ]; then \
		echo "release-publish: $(DIST_DIR)/ missing or empty; run \`make release-binaries RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/SHA256SUMS.asc" ]; then \
		echo "release-publish: $(DIST_DIR)/SHA256SUMS.asc missing — `ibkr update` from v1.0+ requires the signature; re-run release-binaries" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-publish: $(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb missing — re-run release-binaries" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr.mcpb" ]; then \
		echo "release-publish: $(DIST_DIR)/ibkr.mcpb missing — re-run release-binaries" >&2; \
		exit 1; \
	fi
	@command -v gh >/dev/null 2>&1 || { echo "release-publish: gh CLI not on PATH; brew install gh" >&2; exit 1; }
	$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION)
	@notes=$$(mktemp -t ibkr-release-notes.XXXXXX) && \
	highlights=$$(mktemp -t ibkr-release-highlights.XXXXXX) && \
	trap 'rm -f $$notes $$highlights' EXIT && \
	awk -v ver='$(RELEASE_VERSION)' '/^## v[0-9]/{ if(in_ver) exit; in_ver = ($$0 ~ "^## "ver" "); next } in_ver && /^### What.s new$$/{ in_new=1; next } in_ver && in_new && /^###/{ exit } in_new' CHANGELOG.md > $$highlights && \
	awk -v ver='$(RELEASE_VERSION)' -v hf="$$highlights" '{ gsub(/__VERSION__/, ver) } /__HIGHLIGHTS__/{ while ((getline line < hf) > 0) print line; close(hf); next } { print }' .github/release-notes-template.md > $$notes && \
	awk -v ver='$(RELEASE_VERSION)' '/^## v[0-9]/{ in_section = ($$0 ~ "^## " ver " "); skip=0; if(in_section){ next } } in_section && /^### What.s new$$/{ skip=1; next } in_section && skip && /^### /{ skip=0 } in_section && !skip' CHANGELOG.md >> $$notes && \
	title="$${MESSAGE:-$(RELEASE_VERSION)}" && \
	gh release create $(RELEASE_VERSION) --notes-file $$notes --title "$$title" --latest $(DIST_DIR)/*.tar.gz $(DIST_DIR)/*.mcpb $(DIST_DIR)/SHA256SUMS $(DIST_DIR)/SHA256SUMS.asc

changelog-lint: ## Validate the topmost CHANGELOG.md entry matches RELEASE_VERSION and has required shape
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "changelog-lint: RELEASE_VERSION is required, e.g. make changelog-lint RELEASE_VERSION=v0.27.12" >&2; \
		exit 1; \
	fi
	@RELEASE_VERSION=$(RELEASE_VERSION) ./scripts/check-changelog-entry.sh

changelog-stub: ## Prepend a CHANGELOG.md entry skeleton for RELEASE_VERSION
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "changelog-stub: RELEASE_VERSION is required, e.g. make changelog-stub RELEASE_VERSION=v0.27.12" >&2; \
		exit 1; \
	fi
	@RELEASE_VERSION=$(RELEASE_VERSION) ./scripts/changelog-stub.sh

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
# Sequence: gate → build with VERSION override → smoke against live
# gateway → create annotated tag → push tag. The smoke runs BEFORE
# tagging so a gateway failure leaves no orphan tag to clean up; the
# build target accepts VERSION=$(RELEASE_VERSION) so the smoke daemon
# stamps the target version even before the tag exists.
# Does NOT push commits — push those first.
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
	@# Validate the CHANGELOG entry shape before any expensive step. A
	@# malformed entry (wrong version heading, missing ### What's new, or
	@# no Keep-a-Changelog subsection) fails here, not after refresh-spx /
	@# test / build / smoke have already run.
	$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION)
	@# Refresh the S&P-500 membership list from Wikipedia. The release
	@# flow runs this on every cut so every tagged binary carries a
	@# current list; a same-day refresh that produces no diff is a no-op.
	@# A real diff fails the release here: the maintainer commits the
	@# membership update separately and re-runs `make release`, so the
	@# git tag and the binary's checked-in list stay in lockstep. This
	@# guard runs AFTER the dirty-tree check above so we can attribute a
	@# dirty tree to the refresh, not to stray edits.
	$(MAKE) refresh-spx-members
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "release: refresh-spx-members produced uncommitted changes." >&2; \
		echo "        commit the S&P-500 membership update and re-run \`make release\`." >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@# Run the existing full test gate with parallel prerequisites. This
	@# keeps the same checks/tests but overlaps check, pkg, and daemon work.
	$(MAKE) -j$(RELEASE_TEST_JOBS) test
	@# Build the release binary with the target version stamped BEFORE
	@# tagging — pass VERSION explicitly so the build doesn't fall back
	@# to `git describe` (which wouldn't see the tag yet). The smoke
	@# script asserts `bin/ibkr version == $(RELEASE_VERSION)`, so the
	@# stamp has to match.
	$(MAKE) build VERSION=$(RELEASE_VERSION)
	@# Binding live-gateway JSON + wire smoke against the freshly-stamped
	@# binary. Runs one isolated daemon session and fails on no gateway.
	$(MAKE) release-smoke RELEASE_VERSION=$(RELEASE_VERSION) SMOKE_STRICT=1
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	@$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION) || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	git push origin $(RELEASE_VERSION)
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	claude plugin tag . --push --message "$$msg"
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	$(MAKE) release-publish RELEASE_VERSION=$(RELEASE_VERSION) MESSAGE="$$msg"
	$(MAKE) registry-publish RELEASE_VERSION=$(RELEASE_VERSION)
	@echo
	@echo "Released $(RELEASE_VERSION):"
	@echo "  https://github.com/osauer/ibkr/releases/tag/$(RELEASE_VERSION)"
	@echo
	@echo "Verify:"
	@echo "  bin/ibkr version"
	@echo "  gh release view $(RELEASE_VERSION) --json assets -q '.assets[].name'"
	@echo "  gh api repos/osauer/ibkr/git/refs/tags/ibkr--$(RELEASE_VERSION) --jq '.object.sha'"
