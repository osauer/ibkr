// version.go renders `ibkr version` / `ibkr daemon --version`.
//
// Three independent signals describe "which binary is this":
//
//   - ldflags vars (`version`, `commit`, `date`) are stamped by the
//     Makefile via `-X main.* `. They are authoritative when the build
//     went through `make build`, but they can lag behind reality if the
//     user rebuilt under bin/ but didn't `make install` (so $PATH still
//     resolves to the older copy in ~/.local/bin), or if Go's link cache
//     handed back the prior artifact.
//   - The on-disk mtime of os.Executable() catches the
//     rebuilt-but-not-installed case: when the timestamps disagree with
//     "I just compiled this," the user's binary isn't the one they think
//     it is.
//   - runtime/debug.BuildInfo's `vcs.modified` catches the inverse: a
//     plain `go build` without the Makefile produces a binary with no
//     `-dirty` suffix in the version string but still records dirty-tree
//     state in the embedded VCS metadata.
//
// Surfacing all three in the version block is what makes "I rebuilt but
// version didn't change" debuggable without the user having to check
// `which ibkr` and `stat` and `go version -m` separately.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// versionInfo is the wire+text shape returned by collectVersionInfo.
// Field order matches the rendered text block; JSON tags are
// snake_case for parity with the rest of `ibkr --json`. omitempty
// drops the optional fields when the build doesn't have them.
type versionInfo struct {
	Program     string `json:"program"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	VCSState    string `json:"vcs_state,omitempty"`
	Built       string `json:"built,omitempty"`
	Binary      string `json:"binary,omitempty"`
	BinaryMtime string `json:"binary_mtime,omitempty"`
	GoVersion   string `json:"go_version"`
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
}

// collectVersionInfo gathers the three signal sources (ldflags vars,
// runtime/debug BuildInfo, on-disk mtime) into one struct. All file/
// runtime probes are best-effort: empty strings on failure so the
// renderer (and JSON via omitempty) silently skips them.
func collectVersionInfo(program string) versionInfo {
	v := versionInfo{
		Program:   program,
		Version:   version,
		Commit:    commit,
		Built:     date,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	// "none" / "unknown" are the ldflags zero-values from main.go. Map
	// them to empty so omitempty drops them from JSON and the text
	// renderer suppresses the corresponding line.
	if v.Commit == "none" {
		v.Commit = ""
	}
	if v.Built == "unknown" {
		v.Built = ""
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		// `go install github.com/osauer/ibkr/cmd/ibkr@vX.Y.Z` doesn't run
		// the Makefile, so the -ldflags vars stay at their "dev" / "none"
		// defaults. The Go module system embeds the resolved version in
		// BuildInfo regardless — surface it so go-install users see the
		// release tag instead of "dev".
		if v.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.modified" {
				if s.Value == "true" {
					v.VCSState = "modified"
				} else {
					v.VCSState = "clean"
				}
				break
			}
		}
	}

	if bin, err := os.Executable(); err == nil {
		v.Binary = bin
		if fi, err := os.Stat(bin); err == nil {
			v.BinaryMtime = fi.ModTime().Local().Format(time.RFC3339)
		}
	}
	return v
}

// shortCommit truncates a git sha to the conventional 7-char prefix
// for the human-readable text block. JSON keeps the full value.
func shortCommit(c string) string {
	if len(c) >= 7 {
		return c[:7]
	}
	return c
}

// printVersion renders the version info to w in the requested format.
// Text form mirrors Docker/kubectl/helm: `<program> <version>` headline
// followed by indented `Key:   Value` lines.
func printVersion(w io.Writer, program string, jsonOut bool) {
	v := collectVersionInfo(program)
	if jsonOut {
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(w, `{"error":%q}`+"\n", err.Error())
			return
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		return
	}
	fmt.Fprintf(w, "%s %s\n", v.Program, v.Version)
	// Commit line: print whenever there's *something* to say. A stripped
	// binary may have no commit but still carry vcs.modified — surface
	// the state alone in that case.
	switch {
	case v.Commit != "" && v.VCSState != "":
		fmt.Fprintf(w, "  %-8s %s (%s)\n", "Commit:", shortCommit(v.Commit), v.VCSState)
	case v.Commit != "":
		fmt.Fprintf(w, "  %-8s %s\n", "Commit:", shortCommit(v.Commit))
	case v.VCSState != "":
		fmt.Fprintf(w, "  %-8s (%s)\n", "Commit:", v.VCSState)
	}
	if v.Built != "" {
		fmt.Fprintf(w, "  %-8s %s\n", "Built:", v.Built)
	}
	if v.Binary != "" {
		if v.BinaryMtime != "" {
			fmt.Fprintf(w, "  %-8s %s (mtime %s)\n", "Binary:", v.Binary, v.BinaryMtime)
		} else {
			fmt.Fprintf(w, "  %-8s %s\n", "Binary:", v.Binary)
		}
	}
	fmt.Fprintf(w, "  %-8s %s %s/%s\n", "Go:", v.GoVersion, v.GOOS, v.GOARCH)
}

// hasJSONFlag is the version subcommand's mini-parser. The subcommand
// has no other flags, so spinning up a flag.FlagSet for one bool would
// be more code than this and would refuse unknown args (which the
// user might pass by mistake — let the caller handle that).
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}
