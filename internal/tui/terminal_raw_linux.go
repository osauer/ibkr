//go:build linux

package tui

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TCGETS
	termiosSetRequest = unix.TCSETS
)
