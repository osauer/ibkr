package cli

import (
	"io"
	"os"
)

// ANSI SGR codes used across renderers. Kept to a tight palette so colored
// output stays signal — green/red for sign, dim for the absent/zero
// placeholder, yellow for warning badges. Bold is reserved for in-table
// markers (e.g. ATM) and is not currently used.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
)

// ShouldColor reports whether ANSI color escapes should be emitted to w.
// Policy, in order:
//
//  1. IBKR_COLOR=always  → on (overrides TTY check)
//  2. IBKR_COLOR=never   → off
//  3. NO_COLOR set (any) → off (https://no-color.org)
//  4. w is a character device (interactive terminal) → on
//  5. otherwise → off (pipes, file redirects, bytes.Buffer in tests)
//
// Computed once per process and cached on Env.Color so colored renderers
// don't re-syscall on every value.
func ShouldColor(w io.Writer) bool {
	switch os.Getenv("IBKR_COLOR") {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// red returns s wrapped in red ANSI codes when env.Color is on, else s
// unchanged. The other helpers below mirror this contract; pre-checking
// env.Color at the call site is fine but not required.
func (e *Env) red(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiRed + s + ansiReset
}

func (e *Env) green(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiGreen + s + ansiReset
}

func (e *Env) yellow(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiYellow + s + ansiReset
}

func (e *Env) dim(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiDim + s + ansiReset
}
