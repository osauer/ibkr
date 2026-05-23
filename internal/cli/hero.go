package cli

import (
	"fmt"
	"io"
	"strings"
)

// renderCommandHero writes the unified header for human-mode CLI commands.
//
// Layout:
//
//	<title>  ·  <timestamp>
//	  <anchor>
//	  <summary>     (preceded by blank line when non-empty)
//
// All four fields are pre-rendered strings. Empty inputs are skipped
// cleanly so callers don't have to branch — a bare title still renders
// as the title line + trailing blank, matching the rhythm of the other
// section renderers in this package.
//
// Anchors and summaries are forced single-line: any embedded newlines
// are collapsed to a single space so the header layout stays
// predictable. Callers that want multi-line context should print it
// below the hero, not inside it.
func renderCommandHero(w io.Writer, title, timestamp, anchor, summary string) {
	fmt.Fprintln(w)
	switch {
	case title != "" && timestamp != "":
		fmt.Fprintf(w, "%s  ·  %s\n", title, timestamp)
	case title != "":
		fmt.Fprintln(w, title)
	case timestamp != "":
		fmt.Fprintln(w, timestamp)
	}
	if anchor != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", collapseLine(anchor))
	}
	if summary != "" {
		if anchor == "" {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  %s\n", collapseLine(summary))
	}
	if anchor != "" || summary != "" {
		fmt.Fprintln(w)
	}
}

// collapseLine flattens any embedded newlines to single spaces so the
// hero's one-line invariant holds even if a caller passes multi-line
// pre-formatted text.
func collapseLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
