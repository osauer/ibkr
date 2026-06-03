package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// Spec thresholds, baked from docs/specs/risk-regime-dashboard.md. The
// daemon stays threshold-free (the spec calls these user-tunable) — the
// renderer is the right home for defaults. When a real user asks for
// tuning, lift these to env vars or a config file; until then, YAGNI.
const (
	vixRatioGreen      = 0.92 // VIX/VIX3M below this is healthy contango
	vixRatioRed        = 1.00 // above this is backwardation
	vvixYellow         = 90.0
	vvixRed            = 110.0
	hyOASYellow        = 4.0
	hyOASRed           = 5.5
	hyOASWidenYellow   = 0.50 // percentage points over ~20 observations
	hyOASWidenRed      = 1.00
	fundingYellowBps   = 25.0
	fundingRedBps      = 75.0
	usdJpyMoveYellow   = 1.0  // % weekly yen strengthening
	usdJpyMoveRed      = 2.0  // % weekly yen strengthening
	gammaGapYellow     = 2.0  // spot within ±X% of zero-gamma
	hygSpyNearHighProx = 0.97 // SPY ≥ 0.97 × 52-w high = "near highs"
	breadthGreen       = 55.0 // % SPX constituents above 50-DMA
	breadthRed         = 40.0 // < this with SPX at highs is classic divergence
)

// regimeBand is the classified state of one indicator. unranked covers
// computing / unavailable / error — these don't contribute to the
// composite count (user-confirmed decision; honest about coverage).
type regimeBand int

const (
	bandUnranked regimeBand = iota
	bandGreen
	bandYellow
	bandRed
)

// regimeRow is the rendered shape of one indicator: a fixed-width row
// the layout assembles top-to-bottom. Kept as a struct so the composite
// counter and the row renderer share one source of truth.
type regimeRow struct {
	name      string     // "VIX/VIX3M"
	cluster   string     // cluster token for composite de-duplication
	value     string     // value cell, plain; dim-suffix attached at render time
	asOf      string     // compact freshness badge ("live", "close D-1", "cached 11:42")
	band      regimeBand // for glyph + band-word coloring
	reason    string     // parenthetical band justification ("<0.92 contango")
	status    string     // rpc.RegimeStatus*; drives glyph for unranked + stale suffix
	stateNote string     // override for unranked / loading rows ("42s ETA · 40% done")
	// quality is the row's compact provenance tag, e.g. "· est 18s" or
	// "· modelled". Empty string when every value on the row came from
	// a firm-live tick — the default case stays unannotated to keep the
	// rendering uncluttered. Each row builder computes this from the
	// rpc.Quality pointers attached to the values it consumed.
	quality string
	// streak summarises the consecutive-sessions-in-band counter on a
	// short inline marker like "day 3" — appended next to the band word
	// so a reader sees "yellow · day 3" without scanning sideways. The
	// streak counter is daemon-classified using the spec defaults; a
	// renderer with custom thresholds reads the raw value cell instead.
	streak string
}

func runRegime(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON for tooling")
	explain := fs.Bool("explain", false, "show concise threshold, reading, and provenance notes")
	diagnostics := fs.Bool("diagnostics", false, "with --explain, include raw source, missing-field, and quality diagnostics")
	watch := fs.Bool("watch", false, "re-poll continuously; in-place redraw on a TTY")
	rate := fs.Duration("rate", 5*time.Minute, "poll interval for --watch (default 5m)")
	logPath := fs.String("log", "", "append snapshot to a JSONL file at <path> (one line per call)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "regime: takes no positional args (got %v)", fs.Args())
	}
	if *diagnostics && !*explain {
		return fail(env, "regime: --diagnostics requires --explain")
	}

	fetchAndRender := func(out io.Writer) int {
		var res rpc.RegimeSnapshotResult
		if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
			return fail(env, "regime: %v", err)
		}
		rpc.StripRegimeGammaProfiles(&res)
		// Append to the JSONL log before rendering. If the write fails
		// the user still sees the snapshot — log persistence is a
		// side effect, not the primary goal. Stderr-equivalent warning
		// surfaces via env so a non-TTY consumer can still parse the
		// regime envelope cleanly.
		if *logPath != "" {
			if err := appendRegimeLog(*logPath, res); err != nil {
				_, _ = fmt.Fprintf(env.Stderr, "regime: log append failed: %v\n", err)
			}
		}
		if *jsonOut {
			if !*explain {
				rpc.CompactRegimeSnapshot(&res)
			} else {
				rpc.StripRegimeGammaProfiles(&res)
			}
			return printJSONTo(env, out, res)
		}
		rpc.StripRegimeGammaProfiles(&res)
		return renderRegimeTextWithOptions(env, out, &res, regimeRenderOptions{Explain: *explain, Diagnostics: *diagnostics})
	}

	if *watch {
		if *jsonOut {
			return fail(env, "regime: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "regime", fetchAndRender)
	}
	// Single-call TTY path: the daemon's fan-out can sit silent for
	// 10-20 s on a cold cache while the fetchers race contract-
	// resolution against history pulls. Surface motion so the user
	// sees the command is alive instead of staring at a blank
	// terminal. Pipe / non-TTY callers and --json keep their atomic
	// stdout shape unchanged.
	if *jsonOut || !isTerminal(env.Stdout) {
		return fetchAndRender(env.Stdout)
	}
	stop := startRegimeSpinner(env)
	code := fetchAndRender(env.Stdout)
	stop()
	return code
}

// appendRegimeLog writes one JSONL line to path: an object with the
// current ISO-8601 timestamp and the full regime envelope. Each line
// stands alone; a partial trailing line on crash is salvageable (drop
// the malformed line, the rest of the file parses). The wrap object's
// shape: `{"timestamp": "<RFC 3339>", "regime": {...}}`.
//
// File is opened with O_APPEND|O_CREATE|O_WRONLY at 0o644 — no atomic
// temp+rename needed for an append-only log. Concurrent writers on
// the same file may interleave lines if each line exceeds PIPE_BUF
// (typically 4 KB; a regime envelope is small enough in practice but
// not guaranteed), so don't spawn many parallel calls against the
// same path. For the daily-cadence ritual the spec recommends, that's
// not a concern.
func appendRegimeLog(path string, snap rpc.RegimeSnapshotResult) error {
	envelope := struct {
		Timestamp time.Time                `json:"timestamp"`
		Regime    rpc.RegimeSnapshotResult `json:"regime"`
	}{
		Timestamp: time.Now().UTC(),
		Regime:    snap,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	// One write of <line>\n. POSIX guarantees atomicity up to PIPE_BUF;
	// a typical regime envelope (~2-4 KB) sits well inside that for the
	// daily-cadence ritual, so we don't bother with file locking.
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// startRegimeSpinner writes a single dim status line to stderr-style
// stdout (so it never appears in captured stdout that consumers might
// parse) and animates a braille spinner until the returned stop
// function is called. The clear-line sequence is written one last time
// on stop so the regime renderer's own header lands at column 0 with
// no leftover spinner characters underneath.
//
// Lives in stdout (not stderr) because env.Stdout is what the rest of
// the CLI writes to — keeping motion on the same stream prevents a
// stderr capture (e.g. `bin/ibkr regime 2>/dev/null`) from swallowing
// the spinner while the final output stays on stdout. The non-TTY
// guard at the call site means a pipe never sees these escapes.
func startRegimeSpinner(env *Env) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	// Braille spinner — narrow, dense, reads as motion at 10 fps.
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	indicators := "VIX VIX3M VVIX HYG SPY OAS funding USD.JPY gamma breadth"
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				// Clear the spinner line so the renderer's first
				// fmt.Fprintln(out) blank line lands at col 0.
				fmt.Fprint(env.Stdout, "\r\x1b[K")
				return
			case <-ticker.C:
				fmt.Fprintf(env.Stdout, "\r\x1b[K%s %s",
					env.dim("Fetching regime: "+indicators), frames[i])
				i = (i + 1) % len(frames)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// renderRegimeText is the writer-less entry point preserved for tests
// and the preview tool. The redesign reads to and writes from an
// explicit io.Writer (so --watch can buffer); this wrapper keeps the
// PreviewRenderRegime and test signature stable.
func renderRegimeText(env *Env, r *rpc.RegimeSnapshotResult) int {
	return renderRegimeTextTo(env, env.Stdout, r, false)
}

type regimeRenderOptions struct {
	Explain     bool
	Diagnostics bool
}

// renderRegimeTextTo writes the dashboard to out. Layout: header with
// timestamp; bold composite verdict + ranked-count summary; market tape;
// one-line indicator rows; optional --explain footer with the spec's prose
// per row. Pass explain=true for the verbose mode.
func renderRegimeTextTo(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, explain bool) int {
	return renderRegimeTextWithOptions(env, out, r, regimeRenderOptions{Explain: explain})
}

func renderRegimeTextWithOptions(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, opts regimeRenderOptions) int {
	width := outputColumns(out)
	if width == 0 {
		width = 120
	}
	return renderRegimeTextWidthWithOptions(env, out, r, opts, width)
}

func renderRegimeTextWidthWithOptions(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, opts regimeRenderOptions, width int) int {
	if width < 40 {
		width = 80
	}
	now := r.AsOf
	rows := []regimeRow{
		rowVIXTerm(now, r.VIXTermStructure),
		rowVolOfVol(now, r.VolOfVol),
		rowHYGSPY(now, r.HYGSPYDivergence),
		rowCreditSpreads(now, r.CreditSpreads),
		rowFundingStress(now, r.FundingStress),
		rowUSDJPY(now, r.USDJPY),
		rowGamma(now, r.GammaZero),
		rowBreadth(now, r.Breadth),
	}
	c := tallyCompositeFromSnapshot(r, rows)

	// Shared hero: only the title + timestamp live here. The regime
	// renderer keeps its own flow below so the verdict/punch-line block
	// reads first and the SPY/VIX tape lands immediately above the audit
	// rows it anchors.
	renderCommandHero(env, out,
		"Risk Regime",
		r.AsOf.Format("2006-01-02 15:04 MST"),
		"",
		"")
	fmt.Fprintln(out)

	// Hero summary: regime label + usable-signal coverage. Raw band and
	// rankability words stay out of the default readout; the table below is
	// the audit trail and --explain carries the mechanics.
	fmt.Fprintf(out, "  %s %s\n", env.bold(colorRegimeLabel(env, c, c.verdict())), env.dim("· "+c.evidence()))
	fmt.Fprintln(out)
	renderRegimeSummaryLine(out, "Read:", regimeReadLine(c), width, nil)
	renderRegimeSummaryLine(out, "Input health:", regimeInputHealthLine(c, rows), width, regimeInputHealthColor(env, c, rows))
	if support := regimeSupportLine(rows); support != "" {
		renderRegimeSummaryLine(out, "Support:", support, width, nil)
	}
	if watch := regimeWatchLine(rows); watch != "" {
		renderRegimeSummaryLine(out, "Watch:", watch, width, nil)
	}
	if aside := regimeSetAsideLine(rows); aside != "" {
		renderRegimeSummaryLine(out, "Set aside:", aside, width, env.dim)
	}
	if len(r.DataQuality) > 0 {
		if context := regimeDataQualityContext(r.DataQuality); context != "" {
			renderRegimeSummaryLine(out, "Data context:", context, width, env.dim)
		}
	}
	fmt.Fprintln(out)

	if headline := renderRegimeHeadline(env, r); headline != "" {
		fmt.Fprintf(out, "  %s\n", headline)
		fmt.Fprintln(out)
	}

	// Header row + horizontal rule give the reader a key for the
	// columns. Dim-colored so the band rows stay visually dominant. The
	// rule width matches the widest row layout (glyph + name + value +
	// band + note), recomputed once here so it tracks layout changes.
	layout := regimeTableLayout(rows, width)
	fmt.Fprintln(out, renderRegimeHeader(env, layout))
	for _, row := range rows {
		for _, line := range renderRow(env, row, layout) {
			fmt.Fprintln(out, line)
		}
	}

	fmt.Fprintln(out)
	if opts.Explain {
		renderExplainBlock(env, out, r, opts.Diagnostics)
		return 0
	}
	for _, line := range wrapVisibleText("Pass --explain for thresholds and reading notes; add --diagnostics for raw source/provenance details.", max(width-2, 24)) {
		fmt.Fprintln(out, env.dim("  "+line))
	}
	return 0
}

func renderRegimeSummaryLine(out io.Writer, label, value string, width int, color func(string) string) {
	prefix := "  " + label + " "
	available := max(width-visibleLen(prefix), 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			labelText := label
			if color != nil {
				labelText = color(labelText)
			}
			fmt.Fprintf(out, "  %s %s\n", labelText, line)
			continue
		}
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", visibleLen(prefix)), line)
	}
}

// renderRegimeHeader returns the dim column-header line displayed above
// the indicator rows. Aligned with renderRow's column widths so labels
// land directly over their cells. Reads as a key for the reader without
// needing to consult docs.
type regimeTableWidths struct {
	nameW   int
	valueW  int
	asOfW   int
	bandW   int
	whyW    int
	detailW int
	compact bool
}

func regimeTableLayout(rows []regimeRow, width int) regimeTableWidths {
	w := regimeTableWidths{nameW: 17, valueW: 30, asOfW: 12, bandW: 8}
	for _, row := range rows {
		w.nameW = min(max(w.nameW, visibleLen(row.name)), 18)
		w.valueW = max(w.valueW, visibleLen(rowDisplayValue(row)))
		w.asOfW = min(max(w.asOfW, visibleLen(regimeWhenLabel(row))), 12)
	}
	// Wide mode keeps the familiar SIGNAL / READING / WHEN / CALL / WHY
	// table, but caps READING so WHY can wrap inside the terminal instead
	// of spilling into the shell prompt.
	const minWhyW = 24
	fixedWithoutValue := 2 + 1 + 2 + w.nameW + 2 + 2 + w.asOfW + 2 + w.bandW + 2
	allowedValue := width - fixedWithoutValue - minWhyW
	if width < 96 || allowedValue < 22 {
		w.compact = true
		w.valueW = 0
		w.detailW = max(width-(2+1+2+w.nameW+2+w.bandW+2+w.asOfW+2), 24)
		return w
	}
	w.valueW = min(w.valueW, allowedValue)
	w.whyW = max(width-(2+1+2+w.nameW+2+w.valueW+2+w.asOfW+2+w.bandW+2), minWhyW)
	return w
}

func renderRegimeHeader(env *Env, w regimeTableWidths) string {
	if w.compact {
		header := fmt.Sprintf("     %s  %s  %s  %s",
			padRightVisible("SIGNAL", w.nameW),
			padRightVisible("CALL", w.bandW),
			padRightVisible("WHEN", w.asOfW),
			"READING / WHY")
		return env.dim(header)
	}
	// "  " = 2-space indent + " " = where the glyph sits + 2 spaces.
	header := fmt.Sprintf("     %s  %s  %s  %s  %s",
		padRightVisible("SIGNAL", w.nameW),
		padRightVisible("READING", w.valueW),
		padRightVisible("WHEN", w.asOfW),
		padRightVisible("CALL", w.bandW),
		"WHY")
	return env.dim(header)
}

// renderRow lays out one indicator line: 2-space indent, glyph, indicator
// name (left-padded to 17 — fits the combined-scope "γ-zero (SPY+SPX)"
// label), value cell (left-padded to 30), band word (color, padded to 7
// visible cells), reason (dim parenthetical), optional streak marker
// ("day 3"), optional quality/stale suffix. The band-word color is
// applied AFTER padding so column alignment stays correct under ANSI
// escapes — same trick as the account renderer's padLeftVisible.
func renderRow(env *Env, r regimeRow, w regimeTableWidths) []string {
	value := rowDisplayValue(r)
	callWord, colorFn := r.callLabel()
	callCell := padRightVisible(callWord, w.bandW)
	if colorFn != nil {
		callCell = padRightVisible(colorFn(env, callWord), w.bandW)
	}
	// In default view, NOTE = the reason string verbatim (no surrounding
	// parens, no quality clock). The reader sees a clean column with
	// short interpretive text per indicator. Quality / streak / stale
	// markers are promoted to --explain since they're noise on the
	// default scan and useful only when auditing one indicator's
	// provenance.
	if w.compact {
		detail := value
		if r.reason != "" {
			detail += " — " + r.reason
		}
		lines := wrapVisibleText(detail, w.detailW)
		out := make([]string, 0, len(lines))
		prefix := fmt.Sprintf("  %s  %s  %s  %s  ",
			r.glyph(env),
			padRightVisible(r.name, w.nameW),
			callCell,
			padRightVisible(regimeWhenLabel(r), w.asOfW))
		continuation := strings.Repeat(" ", visibleLen(prefix))
		for i, line := range lines {
			if i == 0 {
				out = append(out, prefix+line)
				continue
			}
			out = append(out, continuation+env.dim(line))
		}
		return out
	}
	valueLines := wrapVisibleText(value, w.valueW)
	whyLines := wrapVisibleText(r.reason, w.whyW)
	lineCount := max(len(valueLines), len(whyLines))
	out := make([]string, 0, lineCount)
	for i := range lineCount {
		valueCell := ""
		if i < len(valueLines) {
			valueCell = valueLines[i]
		}
		whyCell := ""
		if i < len(whyLines) {
			whyCell = env.dim(whyLines[i])
		}
		if i == 0 {
			out = append(out, fmt.Sprintf("  %s  %s  %s  %s  %s  %s",
				r.glyph(env), padRightVisible(r.name, w.nameW), padRightVisible(valueCell, w.valueW),
				padRightVisible(regimeWhenLabel(r), w.asOfW), callCell, whyCell))
			continue
		}
		out = append(out, fmt.Sprintf("     %s  %s  %s  %s  %s",
			strings.Repeat(" ", w.nameW), padRightVisible(valueCell, w.valueW),
			strings.Repeat(" ", w.asOfW), strings.Repeat(" ", w.bandW), whyCell))
	}
	return out
}

func rowDisplayValue(r regimeRow) string {
	value := strings.TrimSpace(r.value)
	if value != "" && value != "—" {
		return r.value
	}
	if r.stateNote != "" {
		return r.stateNote
	}
	if value != "" {
		return r.value
	}
	return "—"
}

// streakMarker formats a *rpc.StreakInfo into the compact "day N"
// marker used inline with each row. Returns "" when the info is nil
// (computing/unavailable/etc.) so the renderer never paints a stale
// streak on a row that just lost its band.
func streakMarker(s *rpc.StreakInfo) string {
	if s == nil || s.Sessions <= 0 {
		return ""
	}
	return fmt.Sprintf("day %d", s.Sessions)
}

// qualityTag compresses a set of *rpc.Quality pointers into a short
// suffix string for the row's right edge. Returns "" when every
// attached Quality is firm-live (the default-case row reads as fresh
// with no extra ink). Picks the worst-of across attached values:
//
//   - any modelled/proxy → "· modelled"  (e.g. gamma's BS-sweep γ-zero)
//   - any derived/estimate → "· est"     (e.g. SPY 52w-high fallback)
//   - any firm/frozen → "· frozen"       (gateway-frozen tick)
//   - otherwise → ""                     (all firm/live)
//
// Age suffix appends when the worst Quality.AsOf is older than the
// per-class threshold. Tick-data (est/frozen) decays over seconds, so
// any age > 5 s surfaces as "· est 18s". Modelled outputs are stable
// over the snapshot horizon — a 37 s old BS-sweep result is no more
// stale than a 1 s old one — so the age suffix only fires past 5 min,
// as a stale-model warning rather than a freshness clock.
func qualityTag(now time.Time, qs ...*rpc.Quality) string {
	worstAt := time.Time{}
	rank := func(q *rpc.Quality) int {
		if q == nil {
			return 0
		}
		switch {
		case q.FreshnessClass == rpc.FreshnessModelled || q.Confidence == rpc.ConfidenceProxy:
			return 5
		case q.FreshnessClass == rpc.FreshnessDerived && q.Confidence == rpc.ConfidenceFirm:
			return 2
		case q.FreshnessClass == rpc.FreshnessDerived || q.Confidence == rpc.ConfidenceEstimate:
			return 4
		case q.FreshnessClass == rpc.FreshnessFrozen:
			return 3
		case q.FreshnessClass == rpc.FreshnessLive:
			return 1
		}
		return 0
	}
	worstRank := 0
	for _, q := range qs {
		if r := rank(q); r > worstRank {
			worstRank = r
			worstAt = q.AsOf
		}
	}
	type tagSpec struct {
		label     string
		threshold time.Duration
		ageFmt    string // %d unit
		ageUnit   func(time.Duration) int
	}
	specs := map[int]tagSpec{
		4: {"· est", 5 * time.Second, "%s %ds", func(d time.Duration) int { return int(d.Seconds()) }},
		5: {"· modelled", 5 * time.Minute, "%s %dm old", func(d time.Duration) int { return int(d.Minutes()) }},
		3: {"· frozen", 5 * time.Second, "%s %ds", func(d time.Duration) int { return int(d.Seconds()) }},
		2: {"· official", 36 * time.Hour, "%s %dd old", func(d time.Duration) int { return int(d.Hours() / 24) }},
	}
	s, ok := specs[worstRank]
	if !ok {
		return ""
	}
	if !worstAt.IsZero() {
		if age := now.Sub(worstAt); age > s.threshold {
			return fmt.Sprintf(s.ageFmt, s.label, s.ageUnit(age))
		}
	}
	return s.label
}

// glyph picks the row badge from the row's band and status. Ranked rows
// use a filled circle colored by band; unranked rows use a distinct
// glyph per failure mode so the reader can scan the column.
func (r regimeRow) glyph(env *Env) string {
	switch r.band {
	case bandGreen:
		return env.green("●")
	case bandYellow:
		return env.yellow("●")
	case bandRed:
		return env.red("●")
	}
	// Unranked — split by status so each mode has its own glyph.
	switch r.status {
	case rpc.RegimeStatusComputing:
		return env.yellow("◌")
	case rpc.RegimeStatusError:
		return env.red("✕")
	default: // unavailable or unknown
		return env.dim("○")
	}
}

// callLabel returns the trader-facing call for the row. The raw
// green/yellow/red/unranked terms are still present in JSON and --explain,
// but the default terminal view reads as calm/watch/stress/no-vote language.
func (r regimeRow) callLabel() (string, func(*Env, string) string) {
	switch r.band {
	case bandGreen:
		return "calm", func(e *Env, s string) string { return e.green(s) }
	case bandYellow:
		return "watch", func(e *Env, s string) string { return e.yellow(s) }
	case bandRed:
		return "stress", func(e *Env, s string) string { return e.red(s) }
	}
	switch r.status {
	case rpc.RegimeStatusComputing:
		return "building", func(e *Env, s string) string { return e.yellow(s) }
	case rpc.RegimeStatusError:
		return "retry", func(e *Env, s string) string { return e.red(s) }
	case rpc.RegimeStatusUnavailable:
		return "skip", nil
	default:
		return "no vote", nil
	}
}

func asOfLabel(meta *rpc.RegimeAsOfSummary, status string) string {
	if meta != nil && meta.Label != "" {
		return meta.Label
	}
	switch status {
	case rpc.RegimeStatusOK:
		return "live"
	case rpc.RegimeStatusStale:
		return "stale"
	case rpc.RegimeStatusComputing:
		return "computing"
	case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		return "unavailable"
	default:
		return "—"
	}
}

func regimeWhenLabel(row regimeRow) string {
	label := strings.TrimSpace(row.asOf)
	switch strings.ToLower(label) {
	case "":
		label = asOfLabel(nil, row.status)
	case "stale", "frozen":
		return "cached"
	case "computing":
		return "building"
	case "unavailable":
		return "missing"
	}
	if before, ok := strings.CutSuffix(strings.ToLower(label), " delayed"); ok {
		return "delayed " + before
	}
	return ifNonEmpty(label, "—")
}

// ----------------------------------------------------------------------------
// Composite tally + verdict

type regimeComposite struct {
	green, yellow, red int
	ranked, unranked   int
	total              int
	clusterGreen       int
	clusterYellow      int
	clusterRed         int
	clusterRanked      int
	clusterUnranked    int
	clusterTotal       int
}

func tallyComposite(rows []regimeRow) regimeComposite {
	c := regimeComposite{total: len(rows)}
	for _, r := range rows {
		switch r.band {
		case bandGreen:
			c.green++
			c.ranked++
		case bandYellow:
			c.yellow++
			c.ranked++
		case bandRed:
			c.red++
			c.ranked++
		default:
			c.unranked++
		}
	}
	clusters := map[string]regimeBand{}
	for _, r := range rows {
		key := r.cluster
		if key == "" {
			key = r.name
		}
		clusters[key] = worstRegimeBand(clusters[key], r.band)
	}
	c.clusterTotal = len(clusters)
	for _, b := range clusters {
		switch b {
		case bandGreen:
			c.clusterGreen++
			c.clusterRanked++
		case bandYellow:
			c.clusterYellow++
			c.clusterRanked++
		case bandRed:
			c.clusterRed++
			c.clusterRanked++
		default:
			c.clusterUnranked++
		}
	}
	return c
}

func tallyCompositeFromSnapshot(r *rpc.RegimeSnapshotResult, rows []regimeRow) regimeComposite {
	c := tallyComposite(rows)
	if r == nil {
		return c
	}
	rendered := renderedBandSnapshot(*r, rows)
	bands := confirmedRegimeClusterBands(rendered, rawRegimeClusterBands(rendered))
	c.clusterGreen = 0
	c.clusterYellow = 0
	c.clusterRed = 0
	c.clusterRanked = 0
	c.clusterUnranked = 0
	c.clusterTotal = len(bands)
	for _, band := range bands {
		switch band {
		case "green":
			c.clusterGreen++
			c.clusterRanked++
		case "yellow":
			c.clusterYellow++
			c.clusterRanked++
		case "red":
			c.clusterRed++
			c.clusterRanked++
		default:
			c.clusterUnranked++
		}
	}
	return c
}

func renderedBandSnapshot(r rpc.RegimeSnapshotResult, rows []regimeRow) rpc.RegimeSnapshotResult {
	for _, row := range rows {
		band := row.band.string()
		switch row.name {
		case "VIX/VIX3M":
			r.VIXTermStructure.Band = band
		case "VVIX":
			r.VolOfVol.Band = band
		case "HYG vs SPY":
			r.HYGSPYDivergence.Band = band
		case "HY/IG OAS":
			r.CreditSpreads.Band = band
		case "funding spread":
			r.FundingStress.Band = band
		case "USD/JPY":
			r.USDJPY.Band = band
		case "SPX breadth":
			r.Breadth.Band = band
		default:
			if strings.Contains(row.name, "γ-zero") {
				r.GammaZero.Band = band
			}
		}
	}
	return r
}

func (b regimeBand) string() string {
	switch b {
	case bandGreen:
		return "green"
	case bandYellow:
		return "yellow"
	case bandRed:
		return "red"
	default:
		return ""
	}
}

func worstRegimeBand(a, b regimeBand) regimeBand {
	if a == bandRed || b == bandRed {
		return bandRed
	}
	if a == bandYellow || b == bandYellow {
		return bandYellow
	}
	if a == bandGreen || b == bandGreen {
		return bandGreen
	}
	return bandUnranked
}

// verdictFloor is the minimum ranked-row count required for the
// renderer to claim a verdict above "insufficient signal." The spec's
// interpretation table assumes the dashboard is built from multiple
// evidence clusters; below this floor any positive claim ("Normal regime")
// is based on too few measurements to be honest. Three was chosen
// because the spec's yellow- and red-band thresholds both reference
// "majority of indicators" — a verdict needs to see at least majority
// coverage before it stops being a guess.
const verdictFloor = 3

// verdict maps the (red, yellow) tally onto the spec's interpretation
// table (docs/specs/risk-regime-dashboard.md:109-115). Honest about
// "no ranked indicators" — a renderer that pretends to have a verdict
// when every row is computing/unavailable would just be lying. The
// verdictFloor extends the same honesty: a bold-green "Normal regime"
// with 1-of-5 ranked is misleading even when the count summary
// underneath names the unranked tally, because readers scan the bold
// line first.
func (c regimeComposite) verdict() string {
	switch {
	case c.clusterRanked == 0:
		return "No usable signal yet"
	case c.clusterRanked < verdictFloor:
		return "Insufficient signal — too few inputs ready"
	case c.clusterRanked == c.clusterTotal && c.clusterRed == c.clusterTotal:
		return "Full risk-off conditions"
	case c.clusterRed >= 3:
		return "Broad stress regime"
	case c.clusterRed >= 1:
		return "Stress signal present"
	case c.clusterYellow >= 3:
		return "Elevated stress watch"
	default:
		return "Normal regime"
	}
}

func (c regimeComposite) evidence() string {
	var parts []string
	if c.clusterGreen > 0 {
		parts = append(parts, fmt.Sprintf("%d calm", c.clusterGreen))
	}
	if c.clusterYellow > 0 {
		parts = append(parts, fmt.Sprintf("%d watch", c.clusterYellow))
	}
	if c.clusterRed > 0 {
		parts = append(parts, fmt.Sprintf("%d stress", c.clusterRed))
	}
	if c.clusterUnranked > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", c.clusterUnranked))
	}
	coverage := fmt.Sprintf("%d/%d evidence groups usable", c.clusterRanked, c.clusterTotal)
	if len(parts) == 0 {
		return coverage
	}
	return coverage + " · " + strings.Join(parts, " / ")
}

func colorRegimeLabel(env *Env, c regimeComposite, label string) string {
	switch {
	case c.clusterRed > 0 || c.clusterRanked == c.clusterTotal && c.clusterRed == c.clusterTotal:
		return env.red(label)
	case c.clusterYellow >= 3 || c.clusterRanked < verdictFloor:
		return env.yellow(label)
	default:
		return env.green(label)
	}
}

func regimeReadLine(c regimeComposite) string {
	coverage := fmt.Sprintf("%d/%d evidence groups usable", c.clusterRanked, c.clusterTotal)
	switch {
	case c.clusterRanked == 0:
		return "No regime read yet — every evidence group is still building or missing."
	case c.clusterRanked < verdictFloor:
		return "Thin read — " + coverage + "; treat the regime label as context until more inputs load."
	case c.clusterRed >= 3:
		return "Broad stress is confirmed across independent groups."
	case c.clusterRed > 0:
		return "Stress is visible, but not broad enough to dominate the read."
	case c.clusterYellow > 0:
		return "Watch conditions are visible; not a confirmed broad stress regime."
	default:
		return "Constructive tape across the loaded evidence groups."
	}
}

func regimeInputHealthLine(c regimeComposite, rows []regimeRow) string {
	coverage := fmt.Sprintf("%d/%d evidence groups loaded", c.clusterRanked, c.clusterTotal)
	switch {
	case c.clusterRanked == 0:
		return "Blocked — no evidence group has loaded."
	case c.clusterRanked < verdictFloor:
		return "Degraded — " + coverage + "; wait for more inputs before trusting the label."
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable)) > 0:
		return "Needs confirmation — " + coverage + "; " + joinHumanList(regimeRowDisplayNames(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable))) + " did not load."
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusComputing)) > 0:
		return "Warming — " + coverage + "; " + joinHumanList(regimeRowDisplayNames(regimeRowsByStatuses(rows, rpc.RegimeStatusComputing))) + " still building."
	default:
		return "OK — " + coverage + "."
	}
}

func regimeInputHealthColor(env *Env, c regimeComposite, rows []regimeRow) func(string) string {
	switch {
	case c.clusterRanked == 0 || c.clusterRanked < verdictFloor:
		return env.red
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable, rpc.RegimeStatusComputing)) > 0:
		return env.yellow
	default:
		return env.green
	}
}

func regimeSupportLine(rows []regimeRow) string {
	names := regimeNamesByBand(rows, bandGreen)
	if len(names) == 0 {
		return ""
	}
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("%s %s calm.", joinHumanList(names), verb)
}

func regimeWatchLine(rows []regimeRow) string {
	var parts []string
	if names := regimeNamesByBand(rows, bandRed); len(names) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s showing stress%s",
			joinHumanList(names), areOrIs(names), cachedContextSuffix(rows, bandRed)))
	}
	if names := regimeNamesByBand(rows, bandYellow); len(names) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s on watch%s",
			joinHumanList(names), areOrIs(names), cachedContextSuffix(rows, bandYellow)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func regimeSetAsideLine(rows []regimeRow) string {
	var building, missing []string
	type noVoteRow struct {
		name   string
		reason string
	}
	var noVotes []noVoteRow
	for _, row := range rows {
		if row.band != bandUnranked {
			continue
		}
		name := regimeRowDisplayName(row)
		switch row.status {
		case rpc.RegimeStatusComputing:
			building = append(building, name)
		case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
			missing = append(missing, name)
		default:
			noVotes = append(noVotes, noVoteRow{name: name, reason: row.reason})
		}
	}
	var parts []string
	if len(building) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s still building", joinHumanList(building), areOrIs(building)))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s not in this read yet", joinHumanList(missing), areOrIs(missing)))
	}
	switch len(noVotes) {
	case 0:
	case 1:
		part := noVotes[0].name + " did not vote"
		if noVotes[0].reason != "" {
			part += ": " + noVotes[0].reason
		}
		parts = append(parts, part)
	default:
		names := make([]string, 0, len(noVotes))
		for _, row := range noVotes {
			names = append(names, row.name)
		}
		parts = append(parts, fmt.Sprintf("%s did not vote", joinHumanList(names)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func regimeNamesByBand(rows []regimeRow, band regimeBand) []string {
	var names []string
	for _, row := range rows {
		if row.band == band {
			names = append(names, regimeRowDisplayName(row))
		}
	}
	return names
}

func regimeRowsByStatuses(rows []regimeRow, statuses ...string) []regimeRow {
	if len(statuses) == 0 {
		return nil
	}
	statusSet := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		statusSet[status] = true
	}
	var out []regimeRow
	for _, row := range rows {
		if statusSet[row.status] {
			out = append(out, row)
		}
	}
	return out
}

func regimeRowDisplayNames(rows []regimeRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, regimeRowDisplayName(row))
	}
	return names
}

func cachedContextSuffix(rows []regimeRow, band regimeBand) string {
	for _, row := range rows {
		if row.band == band && row.status == rpc.RegimeStatusStale {
			return " from cached data"
		}
	}
	return ""
}

func areOrIs(parts []string) string {
	if len(parts) == 1 {
		return "is"
	}
	return "are"
}

func regimeDataQualityContext(items []rpc.DataQualityHealth) string {
	value := formatDataQualityValue(items)
	value = strings.ReplaceAll(value, "degraded", "context")
	value = strings.ReplaceAll(value, "stale", "cached")
	return value
}

func regimeEvidenceName(name string) string {
	switch {
	case strings.HasPrefix(name, "VIX/"):
		return "volatility term structure"
	case name == "VVIX":
		return "vol-of-vol"
	case strings.HasPrefix(name, "HYG"):
		return "ETF credit proxy"
	case strings.Contains(name, "OAS"):
		return "cash credit spreads"
	case strings.Contains(name, "funding"):
		return "funding spread"
	case strings.HasPrefix(name, "USD"):
		return "FX carry proxy"
	case strings.Contains(name, "γ-zero"):
		return "dealer gamma"
	case strings.Contains(name, "breadth"):
		return "breadth"
	default:
		return name
	}
}

func regimeRowDisplayName(row regimeRow) string {
	evidence := regimeEvidenceName(row.name)
	if evidence == row.name {
		return row.name
	}
	return row.name + " (" + evidence + ")"
}

func joinHumanList(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// renderRegimeHeadline returns the SPY + VIX tape line shown immediately
// above the indicator rows: spot price + day's dollar / percent change
// for SPY, spot + percent change for VIX (which has no shares so dollar
// change is meaningless). Each side renders only when its primary price
// arrived; either half can be missing without dropping the line. Returns
// "" when neither half has data.
//
// Color convention:
//   - SPY change green when positive, red when negative — the reader's
//     account is long SPY-correlated by default.
//   - VIX change red when positive, green when negative — vol expanding
//     is risk-off. Inverted on purpose; matches every trading desk's
//     intuition.
//
// The change cells fall back to "—" (dim) when the prev-close anchor
// didn't arrive, so the reader sees the price even without the delta.
func renderRegimeHeadline(env *Env, r *rpc.RegimeSnapshotResult) string {
	var parts []string
	if cell := spyHeadlineCell(env, r.HYGSPYDivergence); cell != "" {
		parts = append(parts, cell)
	}
	if cell := vixHeadlineCell(env, r.VIXTermStructure); cell != "" {
		parts = append(parts, cell)
	}
	return strings.Join(parts, "    ")
}

func spyHeadlineCell(env *Env, r rpc.RegimeHYGSPYDivergence) string {
	if r.SPYPrice == nil {
		return ""
	}
	price := fmt.Sprintf("SPY %.2f", *r.SPYPrice)
	if r.SPYChange == nil || r.SPYChangePct == nil {
		return price + "  " + env.dim("(—)")
	}
	dollar := signedFloat(*r.SPYChange, 2)
	pct := signedFloat(*r.SPYChangePct, 2) + "%"
	color := env.green
	if *r.SPYChange < 0 {
		color = env.red
	}
	return price + "  " + color(dollar) + "  " + color("("+pct+")")
}

func vixHeadlineCell(env *Env, r rpc.RegimeVIXTerm) string {
	if r.VIX == nil {
		return ""
	}
	price := fmt.Sprintf("VIX %.2f", *r.VIX)
	if r.VIXChangePct == nil {
		return price + "  " + env.dim("(—)")
	}
	pct := signedFloat(*r.VIXChangePct, 2) + "%"
	// Inverted: vol UP is risk-off (red), vol DOWN is constructive
	// (green). Flat (== 0) reads as no-change; dim is the honest call.
	color := env.dim
	switch {
	case *r.VIXChangePct > 0:
		color = env.red
	case *r.VIXChangePct < 0:
		color = env.green
	}
	return price + "  " + color("("+pct+")")
}

// signedFloat formats v with a leading sign and N decimals. Used by
// the headline cells so "+1.20" reads symmetrically with "−1.20".
// The Unicode minus (U+2212) is intentional — it's the same width as
// the plus sign, keeping the alignment clean.
func signedFloat(v float64, decimals int) string {
	sign := "+"
	if v < 0 {
		sign = "−"
		v = -v
	}
	return fmt.Sprintf("%s%.*f", sign, decimals, v)
}

// ----------------------------------------------------------------------------
// Per-indicator row builders. Each one consumes a raw RPC row and
// emits the (name, value, band, reason, status) tuple the renderer
// lays out. Threshold derivation lives here, with the spec defaults
// from the top of the file.

func rowVIXTerm(now time.Time, r rpc.RegimeVIXTerm) regimeRow {
	row := regimeRow{name: "VIX/VIX3M", cluster: "equity_vol", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Ratio == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "ratio unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "VIX/VIX3M not in this read")
		return row
	}
	row.value = fmt.Sprintf("%.3f  (%s / %s)", *r.Ratio, floatPtr(r.VIX, 2), floatPtr(r.VIX3M, 2))
	row.quality = qualityTag(now, r.VIXQuality, r.VIX3MQuality)
	switch {
	case *r.Ratio < vixRatioGreen:
		row.band, row.reason = bandGreen, "vol curve in contango"
	case *r.Ratio < vixRatioRed:
		row.band, row.reason = bandYellow, "vol curve flattening"
	default:
		row.band, row.reason = bandRed, "vol curve inverted"
	}
	return row
}

func rowVolOfVol(now time.Time, r rpc.RegimeVolOfVol) regimeRow {
	row := regimeRow{name: "VVIX", cluster: "equity_vol", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.Last == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "VVIX unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "VVIX not in this read")
		return row
	}
	row.value = fmt.Sprintf("%.1f", *r.Last)
	if r.Change20D != nil {
		row.value += fmt.Sprintf("  %+.1f%%/20d", *r.Change20D)
	}
	row.quality = qualityTag(now, r.ValueQuality)
	switch {
	case *r.Last < vvixYellow:
		row.band, row.reason = bandGreen, "vol-of-vol calm"
	case *r.Last < vvixRed:
		row.band, row.reason = bandYellow, "vol-of-vol elevated"
	default:
		row.band, row.reason = bandRed, "vol-of-vol shock"
	}
	return row
}

func rowHYGSPY(now time.Time, r rpc.RegimeHYGSPYDivergence) regimeRow {
	row := regimeRow{name: "HYG vs SPY", cluster: "credit", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "HYG/SPY unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "credit proxy not in this read")
		return row
	}
	if r.HYGPrice == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "HYG price unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "HYG not in this read")
		return row
	}
	// Value cell: HYG vs its 50dma is the structural signal; SPY's
	// distance from the 52w high is the modifier (yellow band trigger).
	hyg50 := "—"
	if r.HYG50DMA != nil {
		hyg50 = fmt.Sprintf("%.2f", *r.HYG50DMA)
	}
	row.value = fmt.Sprintf("HYG %.2f vs 50d %s", *r.HYGPrice, hyg50)
	row.quality = qualityTag(now, r.HYGQuality, r.HYG50DMAQuality, r.SPYQuality, r.SPY52WHighQuality)
	// Banding. HYG below 50dma while SPY is near highs is the credit-
	// equity divergence this row exists to catch. Streaks carry the
	// "is this sustained?" context; the band itself should not hide the
	// current divergence.
	switch {
	case r.HYG50DMA == nil:
		row.band, row.reason = bandUnranked, "need HYG 50-day average"
	case *r.HYGPrice >= *r.HYG50DMA:
		row.band, row.reason = bandGreen, "credit holding above trend"
	case r.SPY52WHigh != nil && r.SPYPrice != nil && *r.SPYPrice >= hygSpyNearHighProx**r.SPY52WHigh:
		row.band, row.reason = bandRed, "credit lagging while SPY is near highs"
	case r.SPY52WHigh != nil:
		row.band, row.reason = bandYellow, "credit slipped below trend"
	default:
		// HYG < 50dma + SPY 52w high missing: we can't tell whether
		// the divergence is "near highs" or not. Surface honestly
		// rather than guess.
		row.band, row.reason = bandUnranked, "need SPY high anchor"
	}
	return row
}

func rowCreditSpreads(now time.Time, r rpc.RegimeCreditSpreads) regimeRow {
	row := regimeRow{name: "HY/IG OAS", cluster: "credit", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.HYOAS == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "OAS unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "official spreads not in this read")
		return row
	}
	ig := "—"
	if r.IGOAS != nil {
		ig = fmt.Sprintf("%.2f", *r.IGOAS)
	}
	row.value = fmt.Sprintf("HY %.2f / IG %s", *r.HYOAS, ig)
	if r.HY20DChange != nil {
		row.value += fmt.Sprintf("  Δ20d %+.2f", *r.HY20DChange)
	}
	row.quality = qualityTag(now, r.HYOASQuality, r.IGOASQuality, r.SpreadQuality)
	switch {
	case *r.HYOAS >= hyOASRed || (r.HY20DChange != nil && *r.HY20DChange >= hyOASWidenRed):
		row.band, row.reason = bandRed, "cash credit stress"
	case *r.HYOAS >= hyOASYellow || (r.HY20DChange != nil && *r.HY20DChange >= hyOASWidenYellow):
		row.band, row.reason = bandYellow, "cash spreads elevated/widening"
	default:
		row.band, row.reason = bandGreen, "cash spreads calm"
	}
	return row
}

func rowFundingStress(now time.Time, r rpc.RegimeFundingStress) regimeRow {
	row := regimeRow{name: "funding spread", cluster: "funding", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.SpreadBps == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "funding unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "official funding not in this read")
		return row
	}
	cp := "—"
	if r.CP3M != nil {
		cp = fmt.Sprintf("%.2f", *r.CP3M)
	}
	tb := "—"
	if r.TBill3M != nil {
		tb = fmt.Sprintf("%.2f", *r.TBill3M)
	}
	row.value = fmt.Sprintf("%.0fbp  CP %s / bills %s", *r.SpreadBps, cp, tb)
	row.quality = qualityTag(now, r.CP3MQuality, r.TBill3MQuality, r.SpreadQuality)
	switch {
	case *r.SpreadBps < fundingYellowBps:
		row.band, row.reason = bandGreen, "funding calm"
	case *r.SpreadBps < fundingRedBps:
		row.band, row.reason = bandYellow, "funding spread wider"
	default:
		row.band, row.reason = bandRed, "funding stress"
	}
	return row
}

func rowUSDJPY(now time.Time, r rpc.RegimeUSDJPY) regimeRow {
	row := regimeRow{name: "USD/JPY", cluster: "fx_carry", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "no FX tick"
		row.reason = shortUnavailableReason(r.ErrorMessage, "FX not in this read")
		return row
	}
	if r.Last == nil {
		if row.status == "" {
			row.status = rpc.RegimeStatusUnavailable
		}
		row.value = "—"
		row.stateNote = "FX tick unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "FX not in this read")
		return row
	}
	wkly := "—"
	if r.WeeklyChange != nil {
		sign := "+"
		if *r.WeeklyChange < 0 {
			sign = ""
		}
		wkly = fmt.Sprintf("%s%.2f%%/wk", sign, *r.WeeklyChange)
	}
	row.value = fmt.Sprintf("%.4f  %s", *r.Last, wkly)
	row.quality = qualityTag(now, r.LastQuality, r.Close7DAgoQuality)
	// Spec: yen strengthening (USD/JPY *falling*) is the risk signal.
	// Convention: WeeklyChange negative = yen strengthening.
	if r.WeeklyChange == nil {
		row.band, row.reason = bandUnranked, "need weekly move"
		return row
	}
	move := -*r.WeeklyChange // positive when yen strengthening
	switch {
	case move < usdJpyMoveYellow:
		row.band, row.reason = bandGreen, "carry stable"
	case move < usdJpyMoveRed:
		row.band, row.reason = bandYellow, "yen strengthening"
	default:
		row.band, row.reason = bandRed, "yen strengthening fast"
	}
	return row
}

// gammaRowLabel returns the regime row's indicator name, varying with
// the underlying gamma envelope's Scope so combined runs don't claim
// to be SPY. Falls back to "SPY γ-zero" for envelopes without a Scope
// (legacy daemons / pre-step-5 fixtures) or when no Result has landed
// yet — the legacy label keeps existing tests and dashboards stable.
func gammaRowLabel(r rpc.RegimeGammaZero) string {
	res := r.Envelope.Result
	if res == nil {
		// No envelope yet (cold / computing / error). Regime always
		// requests the combined SPY+SPX gamma — label accordingly so
		// the row name doesn't silently flip from "γ-zero (SPY+SPX)"
		// to "SPY γ-zero" depending on whether a compute has landed.
		return "γ-zero (SPY+SPX)"
	}
	switch res.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX γ-zero"
	case rpc.GammaZeroScopeCombined:
		return "γ-zero (SPY+SPX)"
	default:
		return "SPY γ-zero"
	}
}

func rowGamma(now time.Time, r rpc.RegimeGammaZero) regimeRow {
	row := regimeRow{name: gammaRowLabel(r), cluster: "dealer_gamma", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	switch r.Status {
	case rpc.RegimeStatusComputing:
		row.value = ""
		eta := r.Envelope.EtaSeconds
		note := fmt.Sprintf("building  %ds ETA", eta)
		if r.Envelope.Progress > 0 {
			note += fmt.Sprintf(" · %d%%", r.Envelope.Progress)
		}
		// If the in-flight compute is a retry of a recent failure,
		// surface the prior error context so the user sees what's
		// being re-attempted instead of a clean "first call of the
		// NY session" message that hides the previous abort.
		if r.Envelope.RetryOfErrorAt != nil && r.Envelope.RetryOfErrorSummary != "" {
			row.reason = "retrying last failed gamma refresh"
		} else {
			row.reason = "building dealer-gamma snapshot"
		}
		row.stateNote = note
		return row
	case rpc.RegimeStatusError:
		row.value = ""
		row.stateNote = ifNonEmpty(r.Envelope.Error, "compute failed")
		row.reason = "retry on the next regime call"
		return row
	case rpc.RegimeStatusUnavailable:
		row.value = ""
		row.stateNote = "unavailable"
		if r.Envelope.Status == rpc.GammaZeroStatusCold {
			row.reason = "no gamma snapshot yet"
		} else {
			row.reason = "gamma snapshot unavailable"
		}
		return row
	case rpc.RegimeStatusOK:
		c := r.Envelope.Result
		if c == nil {
			row.value = "—"
			row.stateNote = "envelope missing payload"
			return row
		}
		gammaRankable := gammaComputedExplicitlyRankable(c)
		if !gammaRankable {
			row.reason = regimeGammaNoVoteReason(c)
		} else if c.Quality != nil && c.Quality.RankabilityReason != "" {
			row.reason = regimeGammaQualityReason(c.Quality)
		}
		// Gamma's two scalars are always modelled (zero_gamma via the
		// BS sweep) or derived (|Γ|·OI sum from observed OI+IV); the
		// row will carry "· modelled" regardless of ranking.
		row.quality = qualityTag(now, r.ZeroGammaQuality, r.GammaTotalAbsQuality)
		if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
			row.value = formatRegimeGammaAgreement(c)
			if c.GammaTotalAbs > 0 {
				row.value += fmt.Sprintf("  |GEX| %.1fbn", c.GammaTotalAbs/1e9)
			}
			if gammaRankable {
				row.band = rankableGammaCombinedRegimeBand(c)
			} else {
				row.band = bandUnranked
			}
			switch row.band {
			case bandGreen:
				row.reason = regimeGammaCombinedReason(c, bandGreen)
			case bandRed:
				row.reason = regimeGammaCombinedReason(c, bandRed)
			case bandYellow:
				row.reason = regimeGammaCombinedReason(c, bandYellow)
			default:
				if row.reason == "" {
					row.reason = "no usable dealer-gamma profile"
				}
			}
			return row
		}
		// Three rendering paths:
		//
		//  1. A real crossing exists in the swept window → band on the
		//     gap from spot.
		//  2. No crossing, but the swept profile is one-signed → band
		//     on the sign (long-γ across window = stabilizing regime;
		//     short-γ across window = amplifying regime). The compute
		//     has already determined this; it's a strong regime
		//     statement, not "unavailable".
		//  3. No crossing AND no signal (empty profile) → stay
		//     unranked; this is the genuine no-data path.
		if c.ZeroGamma != nil && c.GapPct != nil {
			sign := "+"
			if *c.GapPct < 0 {
				sign = ""
			}
			row.value = fmt.Sprintf("spot %.2f → γ-zero %.2f  %s%.1f%%",
				c.SpotUnderlying, *c.ZeroGamma, sign, *c.GapPct)
			// Annotate horizon disagreement when the renderer would
			// otherwise mask it. "diverge" is the high-information
			// case: near vs term γ-zero straddle spot, meaning the
			// combined headline number cancels the real signal.
			if note := horizonAgreementNote(r.HorizonAgreement, c); note != "" {
				row.value += "  " + note
			}
			switch {
			case *c.GapPct > gammaGapYellow:
				if gammaRankable {
					row.band, row.reason = bandGreen, "spot >2% above γ-zero"
				} else {
					row.band = bandUnranked
				}
			case *c.GapPct >= -gammaGapYellow:
				if gammaRankable {
					row.band, row.reason = bandYellow, "spot within ±2% of γ-zero"
				} else {
					row.band = bandUnranked
				}
			default:
				if gammaRankable {
					row.band, row.reason = bandRed, "spot below γ-zero"
				} else {
					row.band = bandUnranked
				}
			}
			return row
		}
		// No crossing. The compute's GammaSign tells us which side of
		// zero the whole swept profile landed on — that IS the regime
		// statement the renderer should surface. Magnitude (|Γ|·OI) is
		// the convention-free co-primary; rendered inline only when it
		// landed non-zero so the no-aggregator-data case (zero from
		// either an empty profile or a v2 daemon) doesn't paint a
		// misleading "$0.0bn" in the value cell.
		mag := ""
		if c.GammaTotalAbs > 0 {
			mag = fmt.Sprintf("  |GEX| %.1fbn", c.GammaTotalAbs/1e9)
		}
		spotPrefix := fmt.Sprintf("spot %.2f · ", c.SpotUnderlying)
		switch c.GammaSign {
		case "positive":
			row.value = fmt.Sprintf("%slong-γ%s", spotPrefix, mag)
			if gammaRankable {
				row.band = bandGreen
				row.reason = "dealer long-γ · stabilizing"
			} else {
				row.band = bandUnranked
			}
		case "negative":
			row.value = fmt.Sprintf("%sshort-γ%s", spotPrefix, mag)
			if gammaRankable {
				row.band = bandRed
				row.reason = "dealer short-γ · amplifying"
			} else {
				row.band = bandUnranked
			}
		default:
			// "no_data" or empty: genuine no-signal case.
			row.value = fmt.Sprintf("spot %.2f", c.SpotUnderlying)
			row.band = bandUnranked
			row.reason = "sweep produced no signed profile"
		}
		return row
	}
	row.value = "—"
	row.stateNote = string(r.Status)
	return row
}

func formatRegimeGammaAgreement(c *rpc.GammaZeroComputed) string {
	switch {
	case c == nil:
		return "dealer gamma unavailable"
	case c.Summary != nil && c.Summary.Regime == "long_gamma":
		return "long-γ (stabilizing)"
	case c.Summary != nil && c.Summary.Regime == "short_gamma":
		return "short-γ (amplifying)"
	}
	switch c.RegimeAgreement {
	case "agree:long-gamma":
		return "long-γ (stabilizing)"
	case "agree:short-gamma":
		return "short-γ (amplifying)"
	case "disagree":
		return "mixed dealer-gamma read"
	default:
		value := formatRegimeAgreement(c)
		value = strings.ReplaceAll(value, "long-γ (stabilizing regime)", "long-γ (stabilizing)")
		value = strings.ReplaceAll(value, "short-γ (amplifying regime)", "short-γ (amplifying)")
		value = strings.ReplaceAll(value, " · no γ-zero transition found in sweep", "")
		value = strings.ReplaceAll(value, " · SPY/SPX agree", "")
		value = strings.ReplaceAll(value,
			" (DISAGREEMENT — model regimes differ; use per-index below as primary)",
			"")
		return strings.TrimSpace(value)
	}
}

func regimeGammaCombinedReason(c *rpc.GammaZeroComputed, band regimeBand) string {
	noCrossing := c != nil && c.Summary != nil && c.Summary.ZeroGammaStatus == "none_in_window"
	switch band {
	case bandGreen:
		if noCrossing {
			return "long-γ across sweep; dealer hedging can dampen moves"
		}
		return "dealer gamma stabilizing"
	case bandRed:
		if noCrossing {
			return "short-γ across sweep; dealer hedging can amplify moves"
		}
		return "dealer gamma amplifying"
	case bandYellow:
		return "mixed dealer-gamma read"
	default:
		return "dealer-gamma profile not usable"
	}
}

func gammaCombinedRegimeBand(c *rpc.GammaZeroComputed) regimeBand {
	if c != nil && c.Quality != nil && c.Quality.Rankability != rpc.GammaRankabilityRankable {
		return bandUnranked
	}
	type weightedBand struct {
		band   regimeBand
		weight float64
	}
	var bands []weightedBand
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil {
			continue
		}
		b := gammaSingleRegimeBand(sub)
		if b != bandUnranked {
			bands = append(bands, weightedBand{band: b, weight: gammaPerIndexWeight(key, sub)})
		}
	}
	if len(bands) == 0 {
		return bandUnranked
	}
	first := bands[0].band
	total := 0.0
	redWeight := 0.0
	for _, b := range bands[1:] {
		if b.band != first {
			first = bandUnranked
		}
	}
	for _, b := range bands {
		total += b.weight
		if b.band == bandRed {
			redWeight += b.weight
		}
	}
	if first != bandUnranked {
		return first
	}
	if total > 0 && redWeight/total >= 0.5 {
		return bandRed
	}
	return bandYellow
}

func rankableGammaCombinedRegimeBand(c *rpc.GammaZeroComputed) regimeBand {
	if !gammaComputedExplicitlyRankable(c) {
		return bandUnranked
	}
	type weightedBand struct {
		band   regimeBand
		weight float64
	}
	var bands []weightedBand
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if !gammaComputedExplicitlyRankable(sub) {
			continue
		}
		b := gammaSingleRegimeBand(sub)
		if b != bandUnranked {
			bands = append(bands, weightedBand{band: b, weight: gammaPerIndexWeight(key, sub)})
		}
	}
	if len(bands) == 0 {
		return bandUnranked
	}
	first := bands[0].band
	total := 0.0
	redWeight := 0.0
	for _, b := range bands[1:] {
		if b.band != first {
			first = bandUnranked
		}
	}
	for _, b := range bands {
		total += b.weight
		if b.band == bandRed {
			redWeight += b.weight
		}
	}
	if first != bandUnranked {
		return first
	}
	if total > 0 && redWeight/total >= 0.5 {
		return bandRed
	}
	return bandYellow
}

func gammaPerIndexWeight(key string, c *rpc.GammaZeroComputed) float64 {
	if c != nil && c.GammaTotalAbs > 0 {
		return c.GammaTotalAbs
	}
	if key == "SPX" {
		return 100
	}
	return 1
}

func gammaSingleRegimeBand(c *rpc.GammaZeroComputed) regimeBand {
	if c == nil {
		return bandUnranked
	}
	if c.Quality != nil && c.Quality.Rankability != rpc.GammaRankabilityRankable {
		return bandUnranked
	}
	if c.GapPct != nil {
		switch {
		case *c.GapPct > gammaGapYellow:
			return bandGreen
		case *c.GapPct >= -gammaGapYellow:
			return bandYellow
		default:
			return bandRed
		}
	}
	switch c.GammaSign {
	case "positive":
		return bandGreen
	case "negative":
		return bandRed
	default:
		return bandUnranked
	}
}

func gammaComputedExplicitlyRankable(c *rpc.GammaZeroComputed) bool {
	return c != nil && c.Quality != nil && c.Quality.Rankability == rpc.GammaRankabilityRankable
}

func regimeGammaQualityReason(q *rpc.GammaSignalQuality) string {
	if q == nil {
		return "gamma quality unavailable"
	}
	switch q.Rankability {
	case rpc.GammaRankabilityRankable:
		if q.Freshness == "closed_session_cache" {
			return "cached gamma usable"
		}
		return "fresh enough for regime evidence"
	case rpc.GammaRankabilityContextOnly:
		if q.Freshness == "closed_session_context" {
			return "after-hours cached gamma; not a fresh market-structure read"
		}
		return gammaPlainQualityReason(q)
	case rpc.GammaRankabilityBlocked:
		return "coverage not clean enough"
	case rpc.GammaRankabilityUnavailable:
		return "gamma unavailable"
	default:
		return gammaPlainQualityReason(q)
	}
}

func regimeGammaNoVoteReason(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "gamma payload missing"
	}
	if c.Quality == nil {
		return "quality missing"
	}
	if gammaIsSPYProxy(c) {
		return "SPX unavailable; proxy gamma cannot confirm S&P"
	}
	return regimeGammaQualityReason(c.Quality)
}

func rowBreadth(now time.Time, r rpc.RegimeBreadth) regimeRow {
	row := regimeRow{name: "SPX breadth", cluster: "breadth", status: r.Status, asOf: asOfLabel(r.AsOf, r.Status), streak: streakMarker(r.Streak)}
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		switch r.Status {
		case rpc.RegimeStatusUnavailable:
			row.stateNote = "unavailable"
			row.reason = "no breadth snapshot yet"
		case rpc.RegimeStatusComputing:
			row.stateNote = "building"
			// ~60 min is the IBKR-pacing-limited cold-start cost
			// (60 historical-data requests per 10-min sliding window
			// × 503 names ≈ 85 min in the worst case; observed ~60).
			// Mention --foreground so the user knows how to keep the
			// daemon alive long enough to finish.
			row.reason = "building breadth snapshot"
		default:
			row.stateNote = string(r.Status)
		}
		return row
	}
	v50 := r.Envelope.PctAbove50DMA
	v200 := r.Envelope.PctAbove200DMA
	row.value = fmt.Sprintf("%.0f%% above 50d · %.0f%% above 200d", v50, v200)
	if r.NewHighsToday > 0 || r.NewLowsToday > 0 {
		row.value += fmt.Sprintf("  net highs %+.1f%%", r.NetNewHighsPct)
	}
	row.quality = qualityTag(now, r.ValueQuality)
	// Renderer caveat: spec red band also requires "SPX within 3% of
	// 52w high" but we don't have SPX 52w-high context inside this row.
	// Conservative call: report red on raw 50-DMA breadth only; do not
	// downgrade to yellow. The spec is most concerned about the
	// breadth collapse itself; the SPX-at-highs modifier sharpens the
	// signal but doesn't invent it.
	switch {
	case v50 >= breadthGreen:
		row.band, row.reason = bandGreen, "participation broad"
	case v50 >= breadthRed:
		row.band, row.reason = bandYellow, "participation narrowing"
	default:
		row.band, row.reason = bandRed, "participation weak"
	}
	return row
}

// ----------------------------------------------------------------------------
// --explain block: compact audit notes for humans. The daemon still carries
// long methodology prose, but the terminal view should explain thresholds,
// provenance, and reading posture without becoming a wall of dim text.

type regimeExplainQuality struct {
	name string
	q    *rpc.Quality
}

type regimeExplainEntry struct {
	name       string
	thresholds *rpc.RegimeThresholds
	band       string
	bandReason string
	inputs     string
	source     string
	missing    []string
	quals      []regimeExplainQuality
}

func renderExplainBlock(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, diagnostics bool) {
	entries := []regimeExplainEntry{
		{"VIX/VIX3M", r.VIXTermStructure.Thresholds, r.VIXTermStructure.Band, r.VIXTermStructure.BandReason,
			"VIX is Cboe's 30-day S&P 500 implied-volatility index; VIX3M is the roughly three-month version.",
			"IBKR index market data for Cboe VIX and VIX3M; historical replay uses Cboe official CSVs.",
			r.VIXTermStructure.FieldsMissing, []regimeExplainQuality{
				{"VIX", r.VIXTermStructure.VIXQuality},
				{"VIX3M", r.VIXTermStructure.VIX3MQuality},
			}},
		{"VVIX", r.VolOfVol.Thresholds, r.VolOfVol.Band, r.VolOfVol.BandReason,
			"VVIX is Cboe's VIX-of-VIX index: expected volatility of VIX itself.",
			"Cboe official daily VVIX time series.",
			nil, []regimeExplainQuality{
				{"VVIX", r.VolOfVol.ValueQuality},
			}},
		{"HYG vs SPY", r.HYGSPYDivergence.Thresholds, r.HYGSPYDivergence.Band, r.HYGSPYDivergence.BandReason,
			"HYG is a high-yield corporate bond ETF; SPY is the large S&P 500 ETF used as the stock-market comparison.",
			"IBKR HYG/SPY quotes plus HMDS daily bars; SPY 52-week high uses Misc Stats tick 165 or daily-bar fallback.",
			r.HYGSPYDivergence.FieldsMissing, []regimeExplainQuality{
				{"HYG", r.HYGSPYDivergence.HYGQuality},
				{"HYG_50DMA", r.HYGSPYDivergence.HYG50DMAQuality},
				{"SPY", r.HYGSPYDivergence.SPYQuality},
				{"SPY_52w_high", r.HYGSPYDivergence.SPY52WHighQuality},
			}},
		{"HY/IG OAS", r.CreditSpreads.Thresholds, r.CreditSpreads.Band, r.CreditSpreads.BandReason,
			"HY OAS is high-yield corporate spread; IG OAS is investment-grade corporate spread. OAS is extra yield over Treasuries after option adjustment.",
			"FRED/St. Louis Fed CSVs for ICE BofA series BAMLH0A0HYM2 and BAMLC0A0CM.",
			r.CreditSpreads.FieldsMissing, []regimeExplainQuality{
				{"HY_OAS", r.CreditSpreads.HYOASQuality},
				{"IG_OAS", r.CreditSpreads.IGOASQuality},
				{"HY-IG_spread", r.CreditSpreads.SpreadQuality},
			}},
		{"funding spread", r.FundingStress.Thresholds, r.FundingStress.Band, r.FundingStress.BandReason,
			"90-day AA financial commercial paper is short-term financial-company borrowing; 3-month T-bills are short-term U.S. Treasury borrowing.",
			"Federal Reserve Commercial Paper DDP RIFSPPFAAD90_N.B and U.S. Treasury Daily Treasury Bill Rates 13-week bank discount.",
			r.FundingStress.FieldsMissing, []regimeExplainQuality{
				{"CP_3m", r.FundingStress.CP3MQuality},
				{"TBill_3m", r.FundingStress.TBill3MQuality},
				{"CP-TBill", r.FundingStress.SpreadQuality},
			}},
		{"USD/JPY", r.USDJPY.Thresholds, r.USDJPY.Band, r.USDJPY.BandReason,
			"USD/JPY is yen per U.S. dollar; a falling pair means yen strengthening, the carry-unwind direction.",
			"IBKR CASH/IDEALPRO USD.JPY tick plus HMDS midpoint bars; Tier 1 replay uses FRED DEXJPUS.",
			r.USDJPY.FieldsMissing, []regimeExplainQuality{
				{"Last", r.USDJPY.LastQuality},
				{"Close_7d_ago", r.USDJPY.Close7DAgoQuality},
			}},
		{gammaRowLabel(r.GammaZero), r.GammaZero.Thresholds, r.GammaZero.Band, r.GammaZero.BandReason,
			"SPY is the S&P 500 ETF; SPX is the S&P 500 index. Their option books are read separately and combined.",
			"IBKR SPY/SPX option chains, open interest, option quotes/model ticks, and the daemon's gamma cache.",
			r.GammaZero.FieldsMissing, []regimeExplainQuality{
				{"Zero_gamma", r.GammaZero.ZeroGammaQuality},
				{"|Gamma|.OI_sum", r.GammaZero.GammaTotalAbsQuality},
			}},
		{"SPX breadth", r.Breadth.Thresholds, r.Breadth.Band, r.Breadth.BandReason,
			"No single live symbol is used; this counts individual S&P 500 member stocks above their own moving averages.",
			"Local daemon compute from IBKR HMDS constituent daily bars and the generated S&P 500 membership list.",
			r.Breadth.FieldsMissing, []regimeExplainQuality{
				{"Value", r.Breadth.ValueQuality},
			}},
	}
	fmt.Fprintf(out, "  %s\n", env.bold("Explain"))
	if diagnostics && r.SpecDoc != "" {
		renderExplainKV(env, out, "Full methodology", r.SpecDoc, nil)
	}
	fmt.Fprintln(out)
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", env.bold(e.name))
		if bandLine := explainBandLine(e.band, e.bandReason); bandLine != "" {
			renderExplainKV(env, out, "Band", bandLine, styleRegimeExplainBand(env, e.band))
		}
		if thresholdLine := explainThresholdLine(e.thresholds); thresholdLine != "" {
			renderExplainKV(env, out, "Bands", thresholdLine, nil)
		}
		if qualityLine := explainQualityLine(r.AsOf, e.quals, diagnostics); qualityLine != "" {
			renderExplainKV(env, out, "Quality", qualityLine, nil)
		}
		if source := explainSourceSummary(e.name); source != "" {
			renderExplainKV(env, out, "Source", source, nil)
		}
		// The gamma row gets extra surfaces specific to its modelled
		// output: data-quality notes, BS-IV fallback disclosure, and a
		// plain-English read of what γ-zero means.
		if e.name == gammaRowLabel(r.GammaZero) {
			if note := regimeGammaDataNote(r.GammaZero.Envelope.Result, diagnostics); note != "" {
				renderExplainKV(env, out, "Data note", note, nil)
			}
			if res := r.GammaZero.Envelope.Result; res != nil && res.DerivedIVLegs > 0 {
				denom := res.PricedLegCount
				if denom == 0 {
					denom = res.LegCount
				}
				renderExplainKV(env, out, "Model note", fmt.Sprintf(
					"%d/%d priced legs used derived IV from option quote/close fallback.",
					res.DerivedIVLegs, denom), nil)
			}
		}
		if read := explainReadNote(e.name); read != "" {
			renderExplainKV(env, out, "Read", read, nil)
		}
		if diagnostics {
			if e.inputs != "" {
				renderExplainKV(env, out, "Inputs", e.inputs, nil)
			}
			if e.source != "" {
				renderExplainKV(env, out, "Raw source", e.source, nil)
			}
			if len(e.missing) > 0 {
				renderExplainKV(env, out, "Missing", strings.Join(e.missing, ", "), env.yellow)
			}
		}
		fmt.Fprintln(out)
	}
}

func explainSourceSummary(name string) string {
	switch name {
	case "VIX/VIX3M":
		return "IBKR Cboe index market data; historical replay uses official Cboe files."
	case "VVIX":
		return "Cboe official daily VVIX time series."
	case "HYG vs SPY":
		return "IBKR HYG/SPY quotes plus IBKR daily bars for trend and high anchors."
	case "HY/IG OAS":
		return "FRED/St. Louis Fed official daily ICE BofA high-yield and investment-grade OAS series."
	case "funding spread":
		return "Federal Reserve commercial-paper release plus U.S. Treasury Daily Treasury Bill Rates."
	case "USD/JPY":
		return "IBKR IDEALPRO USD/JPY quote plus IBKR midpoint bars for the weekly move."
	case "SPX breadth":
		return "Local daemon breadth cache computed from IBKR daily bars for S&P 500 members."
	}
	if strings.Contains(name, "γ-zero") {
		return "IBKR SPY/SPX option chains, open interest, option quotes/model ticks, and the daemon gamma cache."
	}
	return ""
}

func regimeGammaDataNote(c *rpc.GammaZeroComputed, diagnostics bool) string {
	details := gammaWarningDetailsForRender(c)
	for _, prefix := range []string{"spx_cache_fallback", "spx_unavailable:"} {
		for _, d := range details {
			if strings.HasPrefix(d.Code, prefix) {
				return formatRegimeGammaDataNote(d, diagnostics)
			}
		}
	}
	for _, d := range details {
		if d.Code == "cache_stale_off_hours" {
			return formatRegimeGammaDataNote(d, diagnostics)
		}
	}
	return ""
}

func formatRegimeGammaDataNote(d rpc.GammaWarningDetail, diagnostics bool) string {
	code := strings.ToLower(strings.TrimSpace(d.Code))
	if !diagnostics {
		switch {
		case strings.HasPrefix(code, "spx_cache_fallback"):
			return gammaSPXCacheFallbackContextLine(time.Now())
		case strings.HasPrefix(code, "spx_unavailable:"):
			return "SPX is unavailable; proxy gamma is awareness-only for S&P market structure."
		case code == "cache_stale_off_hours":
			return "Cached after-hours gamma is stale; refresh during option hours for a fresh market-structure read."
		}
	}
	msg := strings.TrimSpace(d.Message)
	if msg == "" {
		msg = d.Code
	}
	scope := strings.TrimSpace(d.Scope)
	lowerMsg := strings.ToLower(msg)
	lowerScope := strings.ToLower(scope)
	if scope != "" && !strings.HasPrefix(lowerMsg, lowerScope+" ") && !strings.HasPrefix(lowerMsg, lowerScope+":") {
		msg = scope + ": " + msg
	}
	parts := []string{msg}
	if impact := strings.TrimSpace(d.Impact); impact != "" {
		parts = append(parts, impact)
	}
	if diagnostics {
		if action := strings.TrimSpace(d.Action); action != "" {
			parts = append(parts, "Action: "+action)
		}
	}
	return strings.Join(parts, " ")
}

func renderExplainKV(env *Env, out io.Writer, label, value string, style func(string) string) {
	const maxWidth = 106
	prefix := "    " + label + ": "
	available := max(maxWidth-visibleLen(prefix), 32)
	for i, line := range wrapVisibleText(value, available) {
		if style != nil {
			line = style(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %s %s\n", env.yellow(label+":"), line)
			continue
		}
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", visibleLen(prefix)), line)
	}
}

func explainBandLine(band, reason string) string {
	switch {
	case band != "" && reason != "":
		return band + " · " + reason
	case band != "":
		return band
	case reason != "":
		return reason
	default:
		return ""
	}
}

func explainThresholdLine(t *rpc.RegimeThresholds) string {
	if t == nil {
		return ""
	}
	parts := []string{}
	if t.Green != "" {
		parts = append(parts, "green "+t.Green)
	}
	if t.Yellow != "" {
		parts = append(parts, "yellow "+t.Yellow)
	}
	if t.Red != "" {
		parts = append(parts, "red "+t.Red)
	}
	return strings.Join(parts, "; ")
}

func explainQualityLine(now time.Time, fields []regimeExplainQuality, diagnostics bool) string {
	parts := []string{}
	for _, field := range fields {
		if field.q == nil {
			continue
		}
		part := field.name + " " + field.q.Confidence + "/" + field.q.FreshnessClass
		if age := compactQualityAge(now.Sub(field.q.AsOf)); age != "" {
			part += ", " + age
		}
		if diagnostics && field.q.Source != "" {
			part += " (" + field.q.Source + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func compactQualityAge(age time.Duration) string {
	if age <= time.Second {
		return ""
	}
	switch {
	case age >= 48*time.Hour:
		return fmt.Sprintf("%dd old", int(age.Hours()/24))
	case age >= time.Hour:
		return fmt.Sprintf("%dh old", int(age.Hours()))
	default:
		return fmt.Sprintf("%ds old", int(age.Seconds()))
	}
}

func styleRegimeExplainBand(env *Env, band string) func(string) string {
	switch band {
	case "green":
		return env.green
	case "yellow":
		return env.yellow
	case "red":
		return env.red
	default:
		return nil
	}
}

func explainReadNote(name string) string {
	switch {
	case strings.HasPrefix(name, "VIX/"):
		return "Volatility term structure. Sustained inversion matters; one-day spikes are noise."
	case name == "VVIX":
		return "Vol-of-vol demand. Use beside VIX term structure because the two can diverge."
	case strings.HasPrefix(name, "HYG"):
		return "ETF credit proxy versus SPY. Watch sustained HYG weakness, especially while SPY stays near highs."
	case strings.Contains(name, "OAS"):
		return "Official cash-credit close. Slower than HYG, but cleaner spread evidence."
	case strings.Contains(name, "funding"):
		return "Short-term funding pressure. A wider CP/T-bill spread means more funding stress."
	case strings.HasPrefix(name, "USD"):
		return "FX carry proxy. Yen strengthening can flag carry-unwind pressure."
	case strings.Contains(name, "γ-zero"):
		return "Dealer-gamma model. Above γ-zero tends to dampen moves (stabilizing); below it tends to amplify (amplifying). No crossing means the row bands on profile sign."
	case strings.Contains(name, "breadth"):
		return "SPX participation. Weak breadth means fewer names are carrying the index."
	default:
		return ""
	}
}

// ----------------------------------------------------------------------------
// Tiny helpers — local to this file, not promoted to cli.go because no
// other renderer needs them.

func floatPtr(p *float64, decimals int) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.*f", decimals, *p)
}

func ifNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func shortUnavailableReason(message, fallback string) string {
	if message == "" {
		return fallback
	}
	switch {
	case strings.Contains(message, "no security definition"), strings.Contains(message, "verified IBKR contract"):
		return "no verified IBKR contract"
	case strings.Contains(message, "entitlement"):
		return "check market-data entitlement"
	case strings.Contains(message, "no spot tick"), strings.Contains(message, "no tick"):
		return "gateway delivered no tick"
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return "fetch timed out"
	default:
		return fallback
	}
}

// horizonAgreementNote returns a short parenthetical for the gamma row
// when the horizon-bucketed γ-zero readings disagree with the combined
// headline. v4 enum: "all_long" / "all_short" / "all_transition"
// agree with the combined reading and don't need a note; the renderer
// stays silent. "diverge:0dte_vs_term" is the high-information case —
// 0DTE and term γ regimes disagree, which the combined headline can
// average over.
func horizonAgreementNote(agreement string, c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	fmtBucket := func(p *float64, sign string) string {
		if p == nil {
			switch sign {
			case "positive":
				return "long"
			case "negative":
				return "short"
			default:
				return "—"
			}
		}
		return fmt.Sprintf("%.0f", *p)
	}
	switch agreement {
	case "diverge:0dte_vs_term":
		return fmt.Sprintf("(0DTE %s · term %s · diverge)",
			fmtBucket(c.ZeroGamma0DTE, c.GammaSign0DTE), fmtBucket(c.ZeroGammaTerm, c.GammaSignTerm))
	case "diverge:partial":
		return fmt.Sprintf("(0DTE %s · 1-7 %s · term %s · diverge)",
			fmtBucket(c.ZeroGamma0DTE, c.GammaSign0DTE),
			fmtBucket(c.ZeroGamma1to7, c.GammaSign1to7),
			fmtBucket(c.ZeroGammaTerm, c.GammaSignTerm))
	case "0dte_only":
		if c.ZeroGamma0DTE != nil {
			return fmt.Sprintf("(0DTE %.0f only · no 1-7 or term crossing)", *c.ZeroGamma0DTE)
		}
		return fmt.Sprintf("(0DTE %s only · no 1-7 or term signal)", fmtBucket(nil, c.GammaSign0DTE))
	case "1to7_only":
		if c.ZeroGamma1to7 != nil {
			return fmt.Sprintf("(1-7 %.0f only · no 0DTE or term crossing)", *c.ZeroGamma1to7)
		}
		return fmt.Sprintf("(1-7 %s only · no 0DTE or term signal)", fmtBucket(nil, c.GammaSign1to7))
	case "term_only":
		if c.ZeroGammaTerm != nil {
			return fmt.Sprintf("(term %.0f only · no near crossing)", *c.ZeroGammaTerm)
		}
		return fmt.Sprintf("(term %s only · no near signal)", fmtBucket(nil, c.GammaSignTerm))
	}
	return ""
}
