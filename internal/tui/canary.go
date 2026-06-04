package tui

import "strings"

func canaryWelcomeText() string {
	return strings.Join([]string{
		styleCanary("       .-====-."),
		styleCanary("    .-#########-."),
		styleCanary("   /####( o )###>"),
		styleCanary("  /############/"),
		styleCanary("  \\#######__.--'") + " " + styleCanary("ibkr canary"),
		styleCanary("   `-.__.-'"),
		"",
		"Type a command such as `status`, `account`, or `positions --quotes`. Use :help for TUI commands.",
	}, "\n")
}

func canaryPanelTitle(width int) string {
	return fit(" "+styleCanary(">(#)")+" "+styleCanary("CANARY"), width)
}
