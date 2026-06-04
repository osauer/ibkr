package tui

import (
	"context"
	"os"
	"unicode/utf8"
)

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyTab
	keyEsc
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyCtrlQ
	keyCtrlR
	keyPageUp
	keyPageDown
	keyHome
	keyEnd
)

type keyEvent struct {
	kind keyKind
	r    rune
}

func decodeKeyBytes(buf []byte) []keyEvent {
	keys := []keyEvent{}
	for len(buf) > 0 {
		b := buf[0]
		switch b {
		case '\r', '\n':
			keys = append(keys, keyEvent{kind: keyEnter})
			buf = buf[1:]
		case '\t':
			keys = append(keys, keyEvent{kind: keyTab})
			buf = buf[1:]
		case 0x7f, 0x08:
			keys = append(keys, keyEvent{kind: keyBackspace})
			buf = buf[1:]
		case 0x03:
			keys = append(keys, keyEvent{kind: keyCtrlC})
			buf = buf[1:]
		case 0x04:
			keys = append(keys, keyEvent{kind: keyCtrlD})
			buf = buf[1:]
		case 0x0c:
			keys = append(keys, keyEvent{kind: keyCtrlL})
			buf = buf[1:]
		case 0x11:
			keys = append(keys, keyEvent{kind: keyCtrlQ})
			buf = buf[1:]
		case 0x12:
			keys = append(keys, keyEvent{kind: keyCtrlR})
			buf = buf[1:]
		case 0x1b:
			key, n := decodeEscape(buf)
			keys = append(keys, key)
			buf = buf[n:]
		default:
			r, n := utf8.DecodeRune(buf)
			if r == utf8.RuneError && n == 1 {
				buf = buf[1:]
				continue
			}
			keys = append(keys, keyEvent{kind: keyRune, r: r})
			buf = buf[n:]
		}
	}
	return keys
}

func decodeEscape(buf []byte) (keyEvent, int) {
	if len(buf) < 2 || buf[1] != '[' {
		return keyEvent{kind: keyEsc}, 1
	}
	if len(buf) >= 3 {
		switch buf[2] {
		case 'A':
			return keyEvent{kind: keyUp}, 3
		case 'B':
			return keyEvent{kind: keyDown}, 3
		case 'C':
			return keyEvent{kind: keyRight}, 3
		case 'D':
			return keyEvent{kind: keyLeft}, 3
		case 'H':
			return keyEvent{kind: keyHome}, 3
		case 'F':
			return keyEvent{kind: keyEnd}, 3
		}
	}
	if len(buf) >= 4 && buf[3] == '~' {
		switch buf[2] {
		case '1', '7':
			return keyEvent{kind: keyHome}, 4
		case '3':
			return keyEvent{kind: keyDelete}, 4
		case '4', '8':
			return keyEvent{kind: keyEnd}, 4
		case '5':
			return keyEvent{kind: keyPageUp}, 4
		case '6':
			return keyEvent{kind: keyPageDown}, 4
		}
	}
	return keyEvent{kind: keyEsc}, min(len(buf), 2)
}

func readInput(ctx context.Context, in *os.File, events chan<- uiEvent) {
	buf := make([]byte, 64)
	for {
		n, err := in.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				events <- uiEvent{kind: eventLiveError, err: err}
				return
			}
		}
		for _, key := range decodeKeyBytes(buf[:n]) {
			select {
			case events <- uiEvent{kind: eventKey, key: key}:
			case <-ctx.Done():
				return
			}
		}
	}
}
