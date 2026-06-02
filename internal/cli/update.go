package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/osauer/ibkr/internal/update"
)

// updateOptions is the parsed flag state for `ibkr update`. Carries
// the four orthogonal axes plus the installed-version string the
// caller injects (cmd/ibkr/main.go stamps it from `var version`).
type updateOptions struct {
	check     bool
	force     bool
	restart   bool
	noRestart bool

	// installedVersion is the running binary's version string (e.g.
	// "v0.32.0" or "dev"). Injected via RunUpdate so this package
	// stays buildable without a circular dependency on cmd/ibkr.
	installedVersion string

	// in/out/err are the I/O streams. Stdin is read for the
	// interactive [Y/n] prompt; tests inject a buffer.
	in  io.Reader
	out io.Writer
	err io.Writer

	// isTTY is the result of TTY detection for in. Tests inject
	// directly rather than touching os.Stdin.Fd().
	isTTY bool
}

// RunUpdate is the entrypoint cmd/ibkr/main.go dispatches to. It does
// not match the CommandFunc signature because update has no Env (no
// daemon connection) — `update` is registered in cli.commands with
// Fn=nil and the binary's main.go calls this function directly, the
// same pattern `setup` uses.
//
// args are the raw CLI args after `ibkr update`. version is the
// installed binary's version string (cmd/ibkr stamps it at build).
// stdin / stdout / stderr are the process I/O streams.
//
// Returns the process exit code.
func RunUpdate(ctx context.Context, args []string, version string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts := updateOptions{
		installedVersion: version,
		in:               stdin,
		out:              stdout,
		err:              stderr,
		isTTY:            isStdinTTY(stdin),
	}
	if exit, ok := parseUpdateFlags(args, &opts, stdout, stderr); !ok {
		return exit
	}
	return runUpdateCore(ctx, &opts, fetchLatestReleaseAdapter, runInstallAdapter, restartDaemonAdapter)
}

// parseUpdateFlags wires a flag.FlagSet against opts. Returns
// (exit-code, ok); ok=false means the caller should return exit-code
// immediately (flag parse failure or --help).
func parseUpdateFlags(args []string, opts *updateOptions, stdout, stderr io.Writer) (int, bool) {
	env := &Env{Stdout: stdout, Stderr: stderr}
	fs := flagSet(env, "update")
	fs.BoolVar(&opts.check, "check", false, "dry-run: report what would change, do not install")
	fs.BoolVar(&opts.force, "force", false, "install latest even if same version as current (corrupt-binary recovery)")
	fs.BoolVar(&opts.restart, "restart", false, "restart the daemon after install (skip prompt)")
	fs.BoolVar(&opts.noRestart, "no-restart", false, "do not restart the daemon after install (skip prompt)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err), false
	}
	if opts.restart && opts.noRestart {
		fmt.Fprintln(stderr, "ibkr update: --restart and --no-restart are mutually exclusive")
		return 2, false
	}
	return 0, true
}

// runUpdateCore is the testable core: takes injectable adapters for
// the three side-effectful operations (fetch metadata, run install,
// restart daemon) so unit tests can exercise the flag matrix and
// version branches without HTTP / disk / signals.
type fetchFunc func(ctx context.Context) (*update.Release, error)
type installFunc func(ctx context.Context, plan *update.Plan) error
type restartFunc func(pid int) error

func runUpdateCore(ctx context.Context, opts *updateOptions, fetch fetchFunc, doInstall installFunc, restart restartFunc) int {
	// TTY ambiguity gate: in non-interactive mode without an explicit
	// restart decision, refuse to install. Silent default-to-N would
	// be a footgun for systemd timers expecting auto-restart. The
	// `--check` flag is a query, not an install, so it is exempt.
	if !opts.check && !opts.isTTY && !opts.restart && !opts.noRestart {
		fmt.Fprintln(opts.err, "ibkr update: ambiguous in non-interactive mode — pass --restart or --no-restart")
		return 2
	}

	rel, err := fetch(ctx)
	if err != nil {
		fmt.Fprintf(opts.err, "ibkr update: could not reach GitHub releases API: %v\n", err)
		return 1
	}

	installed := normalizeVersion(opts.installedVersion)
	latest := normalizeVersion(rel.TagName)

	// Version-compare decision. --force always installs (bypasses).
	// Otherwise: install iff latest > installed. Equal or behind
	// prints "already on latest" and exits 0.
	needsInstall := opts.force || versionNewer(latest, installed)

	if opts.check {
		renderCheck(opts.out, installed, latest, needsInstall, opts.force)
		return 0
	}

	if !needsInstall {
		fmt.Fprintf(opts.out, "ibkr update: already on %s (latest is %s)\n", installed, latest)
		return 0
	}

	// Confirm the platform has an asset before fetching anything.
	if _, _, ok := rel.AssetForHost(); !ok {
		fmt.Fprintf(opts.err, "ibkr update: no release asset for %s/%s on %s\n", runtime.GOOS, runtime.GOARCH, latest)
		return 1
	}
	plan, err := update.PlanFor(rel)
	if err != nil {
		fmt.Fprintf(opts.err, "ibkr update: %v\n", err)
		return 1
	}

	fmt.Fprintf(opts.out, "ibkr update: installing %s -> %s\n", latest, plan.DestPath)
	if err := doInstall(ctx, plan); err != nil {
		if errors.Is(err, update.ErrInstallInProgress) {
			fmt.Fprintln(opts.err, "ibkr update: another ibkr update is already running")
			return 1
		}
		fmt.Fprintf(opts.err, "ibkr update: install failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.out, "ibkr update: installed %s (prior binary stashed as %s.bak)\n", latest, plan.DestPath)

	// Daemon restart decision.
	doRestart := opts.restart
	if !opts.restart && !opts.noRestart && opts.isTTY {
		doRestart = promptRestart(opts.in, opts.out)
	}

	pid, running := update.IsDaemonRunning()
	if !running {
		// No daemon to restart — silent no-op regardless of flag.
		return 0
	}
	if doRestart {
		fmt.Fprintf(opts.out, "ibkr update: restarting daemon (pid %d)\n", pid)
		if err := restart(pid); err != nil {
			fmt.Fprintf(opts.err, "ibkr update: %v\n", err)
			if errors.Is(err, update.ErrStopTimeout) {
				fmt.Fprintln(opts.err, "ibkr update: run `ibkr restart --force` if the daemon is still stuck")
			}
			return 1
		}
		fmt.Fprintln(opts.out, "ibkr update: daemon stopped; next `ibkr` command will respawn it")
	} else {
		fmt.Fprintf(opts.out, "ibkr update: daemon (pid %d) is still on the old binary; run `ibkr restart` to pick up the new version\n", pid)
	}
	return 0
}

// promptRestart reads stdin for a [Y/n] response. Defaults to Y on
// enter / EOF (matches the design's "Default Y on enter" matrix
// entry). Returns true to restart.
func promptRestart(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Restart daemon now? [Y/n] ")
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		// EOF before any input — treat as "Y" (the default).
		return true
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" || line == "y" || line == "yes" {
		return true
	}
	return false
}

// renderCheck prints the dry-run summary. Exit code per design
// (open-decision #1): 0 on already-latest, 0 on update-available
// (informational), non-zero only on actual fetch failures. So
// `ibkr update --check && ibkr update` is the idiomatic confirm-then-
// install pattern.
func renderCheck(w io.Writer, installed, latest string, needsInstall bool, forced bool) {
	switch {
	case forced:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\n--force was set; `ibkr update` would re-install %s\n", installed, latest, latest)
	case needsInstall:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\n`ibkr update` would install %s\n", installed, latest, latest)
	default:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\nalready on latest; nothing to do\n", installed, latest)
	}
}

// normalizeVersion canonicalises a version string so semver.Compare
// works regardless of whether the source ("dev", "0.32.0", "v0.32.0")
// included the v prefix. "dev" stays as-is and triggers the
// "installed unknown" branch in versionNewer.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// versionNewer reports whether latest > installed. A "dev" or empty
// installed version is treated as "always older than any tagged
// release" — a dev build should still be willing to install a
// shipped tagged release on demand.
func versionNewer(latest, installed string) bool {
	if installed == "" || installed == "dev" {
		// Any tagged release is newer than the dev placeholder.
		return semver.IsValid(latest)
	}
	if !semver.IsValid(latest) || !semver.IsValid(installed) {
		// Can't compare — be conservative and say "not newer" so
		// we don't install on a parse failure. --force overrides
		// this branch via runUpdateCore.
		return false
	}
	return semver.Compare(latest, installed) > 0
}

// isStdinTTY reports whether stdin is a real terminal. Mirrors the
// pattern used in internal/cli/color.go (os.ModeCharDevice). Tests
// pass a *bytes.Buffer so the type assertion fails and isTTY=false,
// which matches headless / piped-input behaviour.
func isStdinTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// --- production adapters (thin shims so runUpdateCore stays pure). ---

func fetchLatestReleaseAdapter(ctx context.Context) (*update.Release, error) {
	return update.FetchLatestRelease(ctx)
}

func runInstallAdapter(ctx context.Context, plan *update.Plan) error {
	return update.RunInstall(ctx, plan)
}

func restartDaemonAdapter(pid int) error {
	return update.RestartDaemon(pid)
}
