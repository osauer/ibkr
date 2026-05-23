package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/osauer/ibkr/internal/xdgcache"
)

// ErrInstallInProgress signals that another `ibkr update` already holds
// the install-time flock. Re-exported as a typed sentinel so the CLI
// command can detect it without string-matching the error message.
var ErrInstallInProgress = errors.New("another ibkr update is already running")

// defaultInstallSubdir is the user-default install root when neither
// IBKR_INSTALL_DIR nor an explicit override is provided. Matches
// `make install` conventions and the install.sh script.
const defaultInstallSubdir = ".local/bin"

// ResolveInstallDir returns the directory where the updated `ibkr`
// binary should land. IBKR_INSTALL_DIR overrides — used by the Phase 2
// release pipeline to sandbox dog-food installs into a tmp dir, and by
// tests to avoid touching the host's real ~/.local/bin. Falls back to
// $HOME/.local/bin otherwise.
//
// Documented in the design as the single env-var knob between Path B
// today and Phase 2's release pipeline dog-fooding: Phase 1 plumbs it,
// Phase 2 wires the Makefile.
func ResolveInstallDir() (string, error) {
	if v := os.Getenv("IBKR_INSTALL_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve $HOME: %w", err)
	}
	return filepath.Join(home, defaultInstallSubdir), nil
}

// CacheDir returns the update cache directory: where the tarball,
// SHA256SUMS, extracted binary, and lock file live for the duration of
// an install. Thin wrapper around xdgcache.CacheDir so callers in this
// package can reference one constant ("update").
func CacheDir() (string, error) {
	return xdgcache.CacheDir("update")
}

// AcquireLock takes the install-time flock at <cacheDir>/update.lock.
// Returns ErrInstallInProgress on contention so the CLI can print the
// friendly message without unwrapping a wrapped syscall error.
//
// The lock covers the full flow (download + verify + extract + rename
// + .bak rotation). Two parallel `ibkr update` invocations queue rather
// than race on .bak — the loser exits immediately with the friendly
// "another update is running" message.
func AcquireLock(cacheDir string) (*xdgcache.Lock, error) {
	lock, err := xdgcache.OpenLock(filepath.Join(cacheDir, "update.lock"))
	if err != nil {
		if errors.Is(err, xdgcache.ErrLocked) {
			return nil, ErrInstallInProgress
		}
		return nil, fmt.Errorf("acquire install lock: %w", err)
	}
	return lock, nil
}

// VerifyChecksum reads SHA256SUMS (one `<sha>  <filename>` line per
// asset), looks up assetName, and compares against the SHA256 of the
// file at tarballPath. Returns nil on match.
//
// The two-space-separator format is what shasum / sha256sum / GNU
// coreutils produce by default and what the release pipeline emits.
// Lines for other assets are ignored — the same SHA256SUMS file may
// list every published artefact for the release.
func VerifyChecksum(tarballPath, sumsPath, assetName string) error {
	expected, err := lookupChecksum(sumsPath, assetName)
	if err != nil {
		return err
	}

	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash tarball: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, got)
	}
	return nil
}

// lookupChecksum scans the SHA256SUMS file for assetName and returns
// the hex SHA. Tolerates the optional binary-mode "*" prefix that GNU
// sha256sum may emit before the filename.
func lookupChecksum(sumsPath, assetName string) (string, error) {
	f, err := os.Open(sumsPath)
	if err != nil {
		return "", fmt.Errorf("open SHA256SUMS: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "<hex>  <filename>" or "<hex> *<filename>" (binary mode).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", assetName)
}

// magicNumbers are the leading bytes a freshly-extracted Linux or
// macOS binary should start with. The smoke check rejects garbage
// (e.g. an HTML 404 page mistakenly tarred up) before we hand the
// file to os.Rename and inherit it as the live ibkr binary.
var magicNumbers = [][]byte{
	{0x7F, 0x45, 0x4C, 0x46}, // ELF (Linux)
	{0xFE, 0xED, 0xFA, 0xCE}, // Mach-O 32-bit LE
	{0xFE, 0xED, 0xFA, 0xCF}, // Mach-O 64-bit LE
	{0xCE, 0xFA, 0xED, 0xFE}, // Mach-O 32-bit BE (legacy)
	{0xCF, 0xFA, 0xED, 0xFE}, // Mach-O 64-bit BE
	{0xCA, 0xFE, 0xBA, 0xBE}, // Mach-O fat binary (multi-arch)
	{0xCA, 0xFE, 0xBA, 0xBF}, // Mach-O fat binary 64-bit
}

// hasExecutableMagic reports whether the first bytes of the file at
// path match a known ELF or Mach-O magic. False on read failure too —
// caller treats that as "not executable; reject" rather than crashing.
func hasExecutableMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4)
	n, _ := io.ReadFull(f, buf)
	if n < 4 {
		return false
	}
	for _, m := range magicNumbers {
		if buf[0] == m[0] && buf[1] == m[1] && buf[2] == m[2] && buf[3] == m[3] {
			return true
		}
	}
	return false
}

// ExtractTarball untars+ungzips tarballPath into destDir, expecting a
// single `ibkr` binary entry at the archive root. Returns the absolute
// path of the extracted binary on success, or an error if the archive
// is malformed, the binary entry is missing, or the magic-byte smoke
// check rejects the extracted file as non-executable.
//
// Hardening:
//   - Reject any entry whose resolved path escapes destDir
//     (path-traversal defence — the tarball isn't fully trusted
//     even after SHA verification because verification only proves
//     the bytes match what we asked for, not that they're benign).
//   - Cap per-entry read at 200MiB so a malformed tar header
//     (size: math.MaxInt64) can't OOM the CLI.
//   - File mode is forced to 0o755 — the archive's stored mode is
//     taken as informational only.
func ExtractTarball(tarballPath, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir destDir: %w", err)
	}
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var binPath string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			// Skip directories, symlinks, etc. — the ibkr tarball
			// only contains a single regular file at the root.
			continue
		}
		// Path-traversal defence. After filepath.Clean+Join, the
		// resolved path must still be under destDir.
		name := filepath.Base(filepath.Clean("/" + hdr.Name))
		if name == "" || name == "." || name == ".." {
			continue
		}
		if name != "ibkr" {
			continue
		}
		out := filepath.Join(destDir, name)
		if !strings.HasPrefix(out, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return "", fmt.Errorf("tar entry %q escapes destDir", hdr.Name)
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", fmt.Errorf("create %s: %w", out, err)
		}
		if _, err := io.Copy(w, io.LimitReader(tr, 200<<20)); err != nil {
			_ = w.Close()
			return "", fmt.Errorf("write %s: %w", out, err)
		}
		if err := w.Close(); err != nil {
			return "", fmt.Errorf("close %s: %w", out, err)
		}
		// Force the mode — archive/tar would have set it from the
		// header but we want a single source of truth here.
		if err := os.Chmod(out, 0o755); err != nil {
			return "", fmt.Errorf("chmod %s: %w", out, err)
		}
		binPath = out
		// Don't break — one entry per tarball, but if a malformed
		// archive had duplicates we'd take the last one. Cheap.
	}
	if binPath == "" {
		return "", errors.New("tarball did not contain an `ibkr` binary entry")
	}
	if !hasExecutableMagic(binPath) {
		return "", fmt.Errorf("extracted file %s is not a valid ELF/Mach-O binary", binPath)
	}
	return binPath, nil
}

// StripQuarantine removes the com.apple.quarantine extended attribute
// from path on macOS. On other platforms the call is a no-op.
//
// Critically this MUST run on the staging binary BEFORE the os.Rename
// into place — strip-after-rename leaves a quarantined live binary
// with no rollback signal if the strip fails. Strip-before-rename
// gives us a single point of failure with the prior binary intact.
//
// The xattr command exits non-zero with "No such xattr" when the
// attribute isn't present (e.g. binary was built locally and never
// downloaded through Gatekeeper). That's the steady state we expect
// in tests and on Linux — tolerated.
func StripQuarantine(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cmd := exec.Command("xattr", "-d", "com.apple.quarantine", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// xattr's stderr language is "No such xattr" on macOS 10.13+ —
	// matched case-insensitively across the combined output. If the
	// xattr binary itself is missing (stripped macOS) exec.Command
	// returns an *exec.Error; treat that as a non-fatal warning so
	// the install can still proceed (the binary wasn't downloaded
	// through Gatekeeper if there's no xattr tool, so there's no
	// quarantine attr to remove either).
	combined := strings.ToLower(string(out))
	if strings.Contains(combined, "no such xattr") {
		return nil
	}
	var notFound *exec.Error
	if errors.As(err, &notFound) {
		// `xattr` binary not on PATH. Not fatal; nothing to strip.
		return nil
	}
	return fmt.Errorf("strip quarantine from %s: %w (output: %s)", path, err, strings.TrimSpace(string(out)))
}

// Install atomically replaces destPath with srcBinary. The prior
// binary is stashed as `destPath + ".bak"` (overwriting any existing
// .bak — per design, .bak is the *immediately prior* binary, not
// "before everything went wrong"). Atomic via os.Rename so a running
// daemon keeps its prior inode and new invocations pick up the new
// binary on next exec.
//
// The destination directory is created if missing — covers a fresh
// install where ~/.local/bin/ doesn't exist yet.
func Install(srcBinary, destPath string) error {
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	// Stash prior binary, if any. .bak is overwritten — there is
	// exactly one slot of rollback history per design.
	if _, err := os.Stat(destPath); err == nil {
		bak := destPath + ".bak"
		// os.Rename overwrites on Unix when source and dest are on
		// the same filesystem, which they always are here (both
		// under destDir).
		if err := os.Rename(destPath, bak); err != nil {
			return fmt.Errorf("stash prior binary to %s: %w", bak, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", destPath, err)
	}
	if err := os.Rename(srcBinary, destPath); err != nil {
		return fmt.Errorf("install %s -> %s: %w", srcBinary, destPath, err)
	}
	if err := os.Chmod(destPath, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", destPath, err)
	}
	return nil
}

// CleanupOnSignal installs a SIGTERM/SIGINT handler that removes the
// given tempfiles when the signal fires. Returns a cancel function the
// caller should defer — calling cancel removes the signal handler
// AND removes the tempfiles, so the same cleanup path runs on both
// successful exit (defer cancel) and signal interruption.
//
// The handler exits the process after cleanup so a Ctrl-C during
// download doesn't leave the user dropped back at a half-installed
// state. Cleanup is best-effort; remove errors are swallowed because
// the user already wants out.
func CleanupOnSignal(paths ...string) (cancel func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			for _, p := range paths {
				_ = os.Remove(p)
			}
			// Exit non-zero so callers wrapping `ibkr update` see
			// the interruption. 130 == 128 + SIGINT by convention.
			os.Exit(130)
		case <-done:
			return
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
}

// Plan is the full sequence of artefacts an install touches. Exposed
// so tests can construct partial state and exercise per-step branches
// without re-running the network layer.
type Plan struct {
	CacheDir    string // ~/.cache/ibkr/update/
	TarballPath string // CacheDir/<asset>.tar.gz
	SumsPath    string // CacheDir/SHA256SUMS
	ExtractDir  string // CacheDir/extract/
	InstallDir  string // $IBKR_INSTALL_DIR or ~/.local/bin
	DestPath    string // InstallDir/ibkr
	AssetName   string // <asset>.tar.gz (used for SHA lookup)
	AssetURL    string // GitHub asset URL
	SumsURL     string // SHA256SUMS asset URL
}

// PlanFor builds a Plan for the given release on the current host. The
// caller has already confirmed the release has an asset for this host;
// this is structure-only, no I/O.
func PlanFor(rel *Release) (*Plan, error) {
	assetName, assetURL, ok := rel.AssetForHost()
	if !ok {
		return nil, fmt.Errorf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sumsName, sumsURL, ok := rel.SHA256SUMSAsset()
	if !ok {
		return nil, errors.New("release is missing SHA256SUMS asset")
	}
	_ = sumsName
	cacheDir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	installDir, err := ResolveInstallDir()
	if err != nil {
		return nil, err
	}
	return &Plan{
		CacheDir:    cacheDir,
		TarballPath: filepath.Join(cacheDir, assetName),
		SumsPath:    filepath.Join(cacheDir, "SHA256SUMS"),
		ExtractDir:  filepath.Join(cacheDir, "extract"),
		InstallDir:  installDir,
		DestPath:    filepath.Join(installDir, "ibkr"),
		AssetName:   assetName,
		AssetURL:    assetURL,
		SumsURL:     sumsURL,
	}, nil
}

// RunInstall executes the install flow end-to-end against a planned
// release: download → verify → extract → quarantine-strip → atomic
// install. Holds an exclusive flock for the duration. Cleans up
// tempfiles on success, error, and SIGINT/SIGTERM. Returns nil on
// success; the prior binary is intact on every error path.
//
// The CLI wrapper layers version comparison and TTY-aware restart on
// top; this function is the pure transport+install primitive so the
// install_test exercises the whole flow with a synthetic tarball.
func RunInstall(ctx context.Context, plan *Plan) error {
	if err := os.MkdirAll(plan.CacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	lock, err := AcquireLock(plan.CacheDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	// Signal-handler cleanup. Tempfiles get removed on Ctrl-C, on
	// SIGTERM (e.g. systemd timer killing the process), and on the
	// happy-path defer below.
	cleanup := CleanupOnSignal(plan.TarballPath, plan.SumsPath, plan.ExtractDir)
	defer cleanup()

	if err := DownloadAsset(ctx, plan.SumsURL, plan.SumsPath); err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if err := DownloadAsset(ctx, plan.AssetURL, plan.TarballPath); err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	if err := VerifyChecksum(plan.TarballPath, plan.SumsPath, plan.AssetName); err != nil {
		return err
	}
	// Fresh extract dir per install — prior runs may have left
	// artefacts. The signal-handler cleanup also removes the dir
	// on exit.
	if err := os.RemoveAll(plan.ExtractDir); err != nil {
		return fmt.Errorf("clear extract dir: %w", err)
	}
	binPath, err := ExtractTarball(plan.TarballPath, plan.ExtractDir)
	if err != nil {
		return err
	}
	// MUST strip quarantine BEFORE rename: strip-after-rename leaves
	// a quarantined live binary with no rollback signal if the strip
	// fails. See StripQuarantine doc.
	if err := StripQuarantine(binPath); err != nil {
		return err
	}
	if err := Install(binPath, plan.DestPath); err != nil {
		return err
	}
	return nil
}
