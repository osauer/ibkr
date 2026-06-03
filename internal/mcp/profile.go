package mcp

import (
	"fmt"
	"strings"
)

type Profile string

const (
	ProfileFull    Profile = "full"
	ProfileMonitor Profile = "monitor"
)

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
