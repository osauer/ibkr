package tui

import "strings"

func canaryWelcomeText() string {
	return strings.Join([]string{
		styleCanary("    ▄██████▄"),
		styleCanary("   ▐████ ███▌▸"),
		styleCanary("   ▐███████▀"),
		styleCanary("    ▀███▀  ") + styleCanary("ibkr canary"),
		"",
		"Type a command such as `status`, `account`, or `positions --quotes`. Use :help for TUI commands.",
	}, "\n")
}

func canaryPanelTitle(width int) string {
	return fit(" "+styleCanary("▐█▌▸")+" "+styleCanary("CANARY"), width)
}
