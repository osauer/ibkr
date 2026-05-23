package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRenderCommandHero_TitleOnly pins the minimal hero: a title with
// no timestamp / anchor / summary still emits the title line and a
// leading blank, matching the rhythm of the other section renderers.
func TestRenderCommandHero_TitleOnly(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	renderCommandHero(&b, "Risk Regime", "", "", "")
	out := b.String()
	if !strings.HasPrefix(out, "\n") {
		t.Errorf("expected leading blank line, got %q", out)
	}
	if !strings.Contains(out, "Risk Regime") {
		t.Errorf("title missing: %q", out)
	}
	if strings.Contains(out, "  ·  ") {
		t.Errorf("title-only must not emit ·-separator: %q", out)
	}
}

// TestRenderCommandHero_TitlePlusTimestamp pins the two-field shape: the
// title and timestamp share one line, joined by "  ·  ".
func TestRenderCommandHero_TitlePlusTimestamp(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	renderCommandHero(&b, "Dealer γ-zero · SPY+SPX", "06:25 CEST", "", "")
	out := b.String()
	if !strings.Contains(out, "Dealer γ-zero · SPY+SPX  ·  06:25 CEST") {
		t.Errorf("title and timestamp should share one line with · separator:\n%q", out)
	}
}

// TestRenderCommandHero_TitleTimestampAnchorSummary pins the full
// four-arg shape: title+timestamp line, blank, indented anchor, indented
// summary, trailing blank.
func TestRenderCommandHero_TitleTimestampAnchorSummary(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	renderCommandHero(&b,
		"Risk Regime",
		"2026-05-23 06:25 CEST",
		"SPY 743.73  +1.01 (+0.14%)    VIX 16.70 (−0.36%)",
		"Normal regime · 3 of 5 ranked")
	out := b.String()
	for _, want := range []string{
		"Risk Regime  ·  2026-05-23 06:25 CEST",
		"  SPY 743.73", // 2-space indent
		"  Normal regime",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hero missing %q:\n%q", want, out)
		}
	}
	// The anchor line should be indented exactly two spaces.
	if !strings.Contains(out, "\n  SPY 743.73") {
		t.Errorf("anchor must sit on its own indented line:\n%q", out)
	}
}

// TestRenderCommandHero_EmptySummary pins that an empty summary doesn't
// inject a stray blank line between anchor and the trailing newline.
// The expected shape: title+timestamp, blank, anchor, blank.
func TestRenderCommandHero_EmptySummary(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	renderCommandHero(&b, "Dealer γ-zero", "06:25 CEST",
		"SPY 743.73  ·  computed 06:25 CEST · 34m ago", "")
	out := b.String()
	lines := strings.Split(out, "\n")
	// Expected lines: "", "Dealer γ-zero  ·  06:25 CEST", "", "  SPY ...", "", "".
	if len(lines) != 6 {
		t.Fatalf("expected 6 line slots (got %d):\n%q", len(lines), out)
	}
	if lines[3] == "" || !strings.Contains(lines[3], "SPY") {
		t.Errorf("anchor expected on line 3, got %q (full %q)", lines[3], out)
	}
}

// TestRenderCommandHero_MultiLineAnchorCollapsed pins the single-line
// invariant: if a caller hands the hero an anchor with embedded
// newlines, they're collapsed to spaces so the header layout stays
// predictable.
func TestRenderCommandHero_MultiLineAnchorCollapsed(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	renderCommandHero(&b, "Title", "10:00",
		"SPY 743.73\nVIX 16.70", "")
	out := b.String()
	// Anchor line should be one line containing both values.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "SPY 743.73") && !strings.Contains(line, "VIX") {
			t.Errorf("anchor newline should have been collapsed; line %q missing VIX", line)
		}
	}
	if !strings.Contains(out, "SPY 743.73 VIX 16.70") {
		t.Errorf("multi-line anchor should collapse to single space:\n%q", out)
	}
}
