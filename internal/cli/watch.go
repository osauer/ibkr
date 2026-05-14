package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// runWatch polls render() at rate until ctx is cancelled. The renderer
// writes its full snapshot to a fresh buffer; runWatch flushes it.
//
// In a TTY (Stdout is a character device), each refresh clears the screen
// and reprints in place — same UX as `top`. In a pipe or buffer, snapshots
// are appended separated by a dim rule so log captures stay parseable.
//
// label is shown in the TTY footer ("account · refresh every 1s · Ctrl-C
// to stop"). render returns a non-zero exit code to abort the loop — the
// first failed render aborts; subsequent transient failures during a long
// session do not (the loop keeps trying so a brief gateway hiccup doesn't
// kill the watch).
func runWatch(ctx context.Context, env *Env, rate time.Duration, label string, render func(out io.Writer) int) int {
	if rate <= 0 {
		rate = time.Second
	}
	tty := isTerminal(env.Stdout)
	ticker := time.NewTicker(rate)
	defer ticker.Stop()

	first := true
	var lastErr int
	for {
		var buf bytes.Buffer
		code := render(&buf)
		if code != 0 {
			// Abort on first render failure (likely gateway down). Once
			// we have at least one good snapshot, keep going on transient
			// errors so a brief hiccup doesn't end the session.
			if first {
				_, _ = env.Stdout.Write(buf.Bytes())
				return code
			}
			lastErr = code
		} else {
			lastErr = 0
			if tty {
				// Clear screen + cursor home. Hidden behind isTerminal so
				// pipes don't get raw escapes baked into their capture.
				fmt.Fprint(env.Stdout, "\x1b[2J\x1b[H")
			} else if !first {
				fmt.Fprintln(env.Stdout, env.dim(strings.Repeat("─", 60)))
			}
			_, _ = env.Stdout.Write(buf.Bytes())
			if tty {
				fmt.Fprintf(env.Stdout, "  %s · refresh every %s · Ctrl-C to stop\n",
					env.dim(label), rate)
			}
			first = false
		}

		select {
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
		}
	}
}

// isTerminal reports whether w writes to a character device (an
// interactive terminal). False for files, pipes, and bytes.Buffer (the
// canonical test target). Used to switch between in-place redraw and
// append-with-rule on watch loops.
func isTerminal(w io.Writer) bool {
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
