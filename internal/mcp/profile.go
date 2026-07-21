package mcp

import (
	"fmt"
	"strings"
)

// Profile selects the MCP capability inventory exposed to a client.
type Profile string

// Supported MCP capability profiles.
const (
	ProfileFull    Profile = "full"
	ProfileMonitor Profile = "monitor"
)

// ParseProfile validates raw and maps an empty value to [ProfileFull].
func ParseProfile(raw string) (Profile, error) {
	raw = strings.TrimSpace(raw)
	switch Profile(raw) {
	case "", ProfileFull:
		return ProfileFull, nil
	case ProfileMonitor:
		return ProfileMonitor, nil
	default:
		return "", fmt.Errorf("profile must be one of full, monitor (got %q)", raw)
	}
}
