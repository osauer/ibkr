//go:build darwin

package tui

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TIOCGETA
	termiosSetRequest = unix.TIOCSETA
)
