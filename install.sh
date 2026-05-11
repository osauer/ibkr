#!/bin/sh
# ibkr installer — one-shot binary install for darwin & linux.
#
#   curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
#
# Detects your OS/arch, downloads the matching pre-built tarball from the
# latest GitHub release, verifies the SHA-256 checksum, installs the binary
# to ~/.local/bin/ibkr, clears the macOS quarantine flag, and adds
# ~/.local/bin to your PATH if it's not there yet. Idempotent — safe to
# re-run to upgrade.
#
# Paranoid? Download and inspect first instead of piping:
#   curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh -o install.sh
#   less install.sh   # read it
#   sh install.sh

set -eu

REPO="osauer/ibkr"
INSTALL_DIR="${IBKR_INSTALL_DIR:-$HOME/.local/bin}"

# --- pretty printing ---------------------------------------------------------
# Detect a TTY for color output. Pipes / CI lose the colors gracefully.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD=$(printf '\033[1m')
	GREEN=$(printf '\033[32m')
	YELLOW=$(printf '\033[33m')
	RED=$(printf '\033[31m')
	DIM=$(printf '\033[2m')
	RESET=$(printf '\033[0m')
else
	BOLD=""; GREEN=""; YELLOW=""; RED=""; DIM=""; RESET=""
fi

info()  { printf '%s==>%s %s\n' "$GREEN" "$RESET" "$*"; }
warn()  { printf '%s!!%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
fail()  { printf '%sERROR:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }
step()  { printf '%s%s%s\n' "$DIM" "$*" "$RESET"; }

# --- prereqs -----------------------------------------------------------------
command -v curl >/dev/null 2>&1 || fail "curl is required but not on PATH"
command -v tar  >/dev/null 2>&1 || fail "tar is required but not on PATH"

# Pick a checksum verifier — macOS has shasum, most Linux distros have
# sha256sum. We need one or the other.
if command -v shasum >/dev/null 2>&1; then
	SHA256_CMD="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	SHA256_CMD="sha256sum"
else
	fail "need shasum (macOS) or sha256sum (linux) to verify the download"
fi

# --- platform detection ------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)

case "$os" in
	Darwin) os=darwin ;;
	Linux)  os=linux ;;
	MINGW*|MSYS*|CYGWIN*)
		fail "Windows is not supported — the daemon uses Unix-only primitives. Try WSL." ;;
	*)
		fail "unsupported OS: $os (need Darwin or Linux)" ;;
esac

case "$arch" in
	arm64|aarch64) arch=arm64 ;;
	x86_64|amd64)  arch=amd64 ;;
	*)
		fail "unsupported architecture: $arch (need arm64 or amd64)" ;;
esac

PLATFORM="${os}-${arch}"
info "Platform detected: $BOLD$PLATFORM$RESET"

# --- resolve latest release tag ---------------------------------------------
# Trick: curl -I against /releases/latest follows the redirect; the final URL
# ends in the version tag (e.g. .../tag/v0.6.2). No API call, no rate limit.
step "Looking up latest release..."
final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")
VERSION=$(printf '%s' "$final_url" | sed 's|.*/||')

case "$VERSION" in
	v[0-9]*) : ;;
	*) fail "could not resolve latest release tag (got '$VERSION' from $final_url)" ;;
esac
info "Latest version:    $BOLD$VERSION$RESET"

# --- download tarball + checksums into a scratch dir ------------------------
TARBALL="ibkr-${VERSION}-${PLATFORM}.tar.gz"
TARBALL_URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"
SUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS"

tmp=$(mktemp -d -t ibkr-install.XXXXXX)
trap 'rm -rf "$tmp"' EXIT

step "Downloading $TARBALL..."
curl -fSL --progress-bar -o "$tmp/$TARBALL" "$TARBALL_URL"
curl -fsSL -o "$tmp/SHA256SUMS" "$SUMS_URL"

# --- verify checksum ---------------------------------------------------------
step "Verifying SHA-256 checksum..."
( cd "$tmp" && $SHA256_CMD -c SHA256SUMS --ignore-missing ) >/dev/null || \
	fail "checksum verification failed for $TARBALL — aborting (the download may be corrupted or tampered with)"
info "Checksum OK"

# --- extract + install -------------------------------------------------------
step "Extracting..."
tar -xzf "$tmp/$TARBALL" -C "$tmp"
extracted_dir="$tmp/ibkr-${VERSION}-${PLATFORM}"
[ -x "$extracted_dir/ibkr" ] || fail "extracted tree missing the ibkr binary (tried $extracted_dir/ibkr)"

step "Installing to $INSTALL_DIR/ibkr..."
mkdir -p "$INSTALL_DIR"
# `install -m 0755` is more portable than mv+chmod and atomic-ish on most
# filesystems. Falls back to cp on systems without `install` (rare).
if command -v install >/dev/null 2>&1; then
	install -m 0755 "$extracted_dir/ibkr" "$INSTALL_DIR/ibkr"
else
	cp "$extracted_dir/ibkr" "$INSTALL_DIR/ibkr"
	chmod 0755 "$INSTALL_DIR/ibkr"
fi

# macOS Gatekeeper marks downloads with com.apple.quarantine; clearing it
# avoids "cannot verify developer" prompts on first run. Silent on linux.
xattr -d com.apple.quarantine "$INSTALL_DIR/ibkr" 2>/dev/null || true

# --- PATH handling -----------------------------------------------------------
# Auto-edit shell rc files ONLY when installing to the default location.
# A user who set IBKR_INSTALL_DIR is doing something custom; touching their
# shell config without asking would be rude (and was a real bug pre-v0.6.2).
DEFAULT_INSTALL_DIR="$HOME/.local/bin"

if [ "$INSTALL_DIR" = "$DEFAULT_INSTALL_DIR" ]; then
	# Already on PATH? Nothing to do.
	case ":${PATH}:" in
		*":${INSTALL_DIR}:"*) need_path_update=0 ;;
		*) need_path_update=1 ;;
	esac

	if [ "$need_path_update" = "1" ]; then
		# Pick the rc file and the export syntax from $SHELL.
		case "${SHELL:-}" in
			*/fish)
				rc="$HOME/.config/fish/config.fish"
				line="set -gx PATH \$HOME/.local/bin \$PATH"
				;;
			*/zsh)
				rc="$HOME/.zshrc"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
			*/bash)
				rc="$HOME/.bashrc"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
			*)
				rc="$HOME/.profile"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
		esac

		# Idempotent: only append if our line (or a moral equivalent) isn't already there.
		if [ -f "$rc" ] && grep -Fq '$HOME/.local/bin' "$rc" 2>/dev/null; then
			info "$INSTALL_DIR already referenced in $rc — leaving it alone"
		else
			printf '\n# Added by ibkr installer\n%s\n' "$line" >> "$rc"
			info "Added $INSTALL_DIR to PATH in $rc"
			warn "Open a new terminal (or run: source $rc) for ibkr to be on PATH in this shell"
		fi
	fi
else
	# Custom install dir: don't touch rc files; just tell the user.
	case ":${PATH}:" in
		*":${INSTALL_DIR}:"*) ;;
		*) warn "$INSTALL_DIR is not on \$PATH; add it manually or invoke ibkr by absolute path" ;;
	esac
fi

# --- verify install ----------------------------------------------------------
step "Verifying..."
installed_version=$("$INSTALL_DIR/ibkr" version 2>/dev/null | head -n1 || true)
case "$installed_version" in
	"ibkr $VERSION"*) info "Installed: $BOLD$installed_version$RESET" ;;
	*)                warn "Installed binary reports unexpected version: $installed_version" ;;
esac

# --- next steps --------------------------------------------------------------
printf '\n'
printf '%sNext steps%s\n' "$BOLD" "$RESET"
printf '  %s•%s Try the CLI:           %sibkr account%s   (needs IB Gateway running)\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
printf '  %s•%s Wire Claude Desktop:   %sibkr setup claude-desktop%s\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
printf '  %s•%s Full docs:             https://github.com/%s\n' "$GREEN" "$RESET" "$REPO"
printf '\n'
