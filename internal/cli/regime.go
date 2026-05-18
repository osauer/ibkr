package cli

import (
	"context"
	"fmt"
	"io"
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
	value     string     // value cell, plain; dim-suffix attached at render time
	band      regimeBand // for glyph + band-word coloring
	reason    string     // parenthetical band justification ("<0.92 contango")
	status    string     // rpc.RegimeStatus*; drives glyph for unranked + stale suffix
	stateNote string     // override for unranked / loading rows ("42s ETA · 40% done")
}

func runRegime(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (the canonical surface for renderers + LLMs)")
	explain := fs.Bool("explain", false, "print the spec's per-indicator threshold prose under each row")
	watch := fs.Bool("watch", false, "re-poll on a fixed interval; in-place redraw on a TTY")
	rate := fs.Duration("rate", 5*time.Minute, "poll interval for --watch (default 5m — regime indicators move on minute-to-hour scales, not sub-minute)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "regime: takes no positional args (got %v)", fs.Args())
	}
	if *jsonOut && *explain {
		return fail(env, "regime: --explain only affects text output; --json carries notes verbatim already")
	}

	fetchAndRender := func(out io.Writer) int {
		var res rpc.RegimeSnapshotResult
		if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
			return fail(env, "regime: %v", err)
		}
		if *jsonOut {
			return printJSONTo(env, out, res)
		}
		return renderRegimeTextTo(env, out, &res, *explain)
	}

	if *watch {
		if *jsonOut {
			return fail(env, "regime: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "regime", fetchAndRender)
	}
	// Single-call TTY path: the daemon's fan-out can sit silent for
	// 10-20 s on a cold cache while the five fetchers race contract-
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
	indicators := "VIX VIX3M HYG SPY USD.JPY gamma breadth"
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

// renderRegimeTextTo writes the dashboard to out. Layout: header with
// timestamp; bold composite verdict + ranked-count summary; horizontal
// rule; five one-line indicator rows; optional --explain footer with
// the spec's prose per row. Pass explain=true for the verbose mode.
func renderRegimeTextTo(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, explain bool) int {
	rows := []regimeRow{
		rowVIXTerm(r.VIXTermStructure),
		rowHYGSPY(r.HYGSPYDivergence),
		rowUSDJPY(r.USDJPY),
		rowGamma(r.GammaZero),
		rowBreadth(r.Breadth),
	}
	c := tallyComposite(rows)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Risk Regime  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(out)

	// Composite line: bold verdict + dim count summary. The count
	// summary names ranked/unranked explicitly so a reader sees that
	// some indicators were excluded rather than assuming the verdict
	// was computed over all five.
	fmt.Fprintf(out, "  %s\n", env.bold(c.verdict()))
	fmt.Fprintf(out, "  %s\n", env.dim(c.summary()))
	fmt.Fprintln(out)

	for _, row := range rows {
		fmt.Fprintln(out, renderRow(env, row))
	}

	fmt.Fprintln(out)
	if explain {
		renderExplainBlock(env, out, r)
		return 0
	}
	fmt.Fprintln(out, env.dim("  Pass --explain for per-indicator spec thresholds and methodology notes."))
	return 0
}

// renderRow lays out one indicator line: 2-space indent, glyph, indicator
// name (left-padded to 12), value cell (left-padded to 26), band word
// (color, padded to 7 visible cells), reason (dim parenthetical), optional
// stale suffix. The band-word color is applied AFTER padding so column
// alignment stays correct under ANSI escapes — same trick as the account
// renderer's padLeftVisible.
func renderRow(env *Env, r regimeRow) string {
	const (
		nameW  = 12
		valueW = 26
		bandW  = 7
	)
	value := r.value
	if r.stateNote != "" {
		value = r.stateNote
	}
	bandWord, colorFn := r.band.label()
	bandCell := padRightVisible(bandWord, bandW)
	if colorFn != nil {
		bandCell = padRightVisible(colorFn(env, bandWord), bandW)
	}
	reason := ""
	if r.reason != "" {
		reason = env.dim("(" + r.reason + ")")
	}
	suffix := ""
	if r.status == rpc.RegimeStatusStale {
		suffix = "  " + env.dim("· stale tick")
	}
	return fmt.Sprintf("  %s  %s  %s  %s%s%s",
		r.glyph(env), padRightVisible(r.name, nameW), padRightVisible(value, valueW), bandCell, reason, suffix)
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

// label returns the (word, color-fn) pair for the band column. nil
// color-fn means render plain (the unranked path). The color-fn takes
// an *Env so the no-op path (color off) shares the same code shape.
func (b regimeBand) label() (string, func(*Env, string) string) {
	switch b {
	case bandGreen:
		return "green", func(e *Env, s string) string { return e.green(s) }
	case bandYellow:
		return "yellow", func(e *Env, s string) string { return e.yellow(s) }
	case bandRed:
		return "red", func(e *Env, s string) string { return e.red(s) }
	}
	return "—", nil
}

// ----------------------------------------------------------------------------
// Composite tally + verdict

type regimeComposite struct {
	green, yellow, red int
	ranked, unranked   int
	total              int
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
	return c
}

// verdictFloor is the minimum ranked-row count required for the
// renderer to claim a verdict above "insufficient signal." The spec's
// interpretation table assumes the dashboard is built from all five
// indicators; below this floor any positive claim ("Normal regime")
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
	case c.ranked == 0:
		return "No ranked indicators — see rows below for state"
	case c.ranked < verdictFloor:
		return "Insufficient signal — too few indicators ranked"
	case c.ranked == c.total && c.red == c.total:
		return "Full risk-off conditions"
	case c.red >= 3:
		return "Regime shift likely — execute pre-committed plan"
	case c.red >= 1:
		return "Watch closely, prep defensive moves"
	case c.yellow >= 3:
		return "Elevated alert — review positioning"
	default:
		return "Normal regime"
	}
}

// summary names the tally + coverage on one dim line. Format chosen so
// the unranked count is visible alongside the ranked one; the reader
// shouldn't have to do arithmetic to see what was excluded.
func (c regimeComposite) summary() string {
	parts := []string{
		fmt.Sprintf("%d green", c.green),
		fmt.Sprintf("%d yellow", c.yellow),
		fmt.Sprintf("%d red", c.red),
	}
	coverage := fmt.Sprintf("%d of %d ranked", c.ranked, c.total)
	if c.unranked > 0 {
		coverage += fmt.Sprintf(" · %d unranked", c.unranked)
	}
	return strings.Join(parts, " · ") + "  ·  " + coverage
}

// ----------------------------------------------------------------------------
// Per-indicator row builders. Each one consumes a raw RPC row and
// emits the (name, value, band, reason, status) tuple the renderer
// lays out. Threshold derivation lives here, with the spec defaults
// from the top of the file.

func rowVIXTerm(r rpc.RegimeVIXTerm) regimeRow {
	row := regimeRow{name: "VIX/VIX3M", status: r.Status}
	if r.Status == rpc.RegimeStatusError || r.Ratio == nil {
		row.value = "—"
		row.stateNote = ifNonEmpty(r.ErrorMessage, "ratio unavailable")
		return row
	}
	row.value = fmt.Sprintf("%.3f  (%.2f / %.2f)", *r.Ratio, deref(r.VIX), deref(r.VIX3M))
	switch {
	case *r.Ratio < vixRatioGreen:
		row.band, row.reason = bandGreen, fmt.Sprintf("<%.2f  contango", vixRatioGreen)
	case *r.Ratio < vixRatioRed:
		row.band, row.reason = bandYellow, "flattening"
	default:
		row.band, row.reason = bandRed, "backwardation"
	}
	return row
}

func rowHYGSPY(r rpc.RegimeHYGSPYDivergence) regimeRow {
	row := regimeRow{name: "HYG vs SPY", status: r.Status}
	if r.Status == rpc.RegimeStatusError {
		row.value = "—"
		row.stateNote = ifNonEmpty(r.ErrorMessage, "HYG/SPY unavailable")
		return row
	}
	// Value cell: HYG vs its 50dma is the structural signal; SPY's
	// distance from the 52w high is the modifier (yellow band trigger).
	hyg50 := "—"
	if r.HYG50DMA != nil {
		hyg50 = fmt.Sprintf("%.2f", *r.HYG50DMA)
	}
	row.value = fmt.Sprintf("HYG %.2f / 50dma %s", deref(r.HYGPrice), hyg50)
	// Banding. Cannot reach red without a multi-session history; the
	// renderer's worst-case for HYG-below-50dma is yellow even when
	// SPY is at highs. This is a documented v1 floor.
	switch {
	case r.HYG50DMA == nil:
		row.band, row.reason = bandUnranked, "50dma missing — cannot band"
	case *r.HYGPrice >= *r.HYG50DMA:
		row.band, row.reason = bandGreen, "HYG ≥ 50dma"
	case r.SPY52WHigh != nil && r.SPYPrice != nil && *r.SPYPrice >= hygSpyNearHighProx**r.SPY52WHigh:
		row.band, row.reason = bandYellow, "HYG < 50dma · SPY near highs"
	case r.SPY52WHigh != nil:
		row.band, row.reason = bandYellow, "HYG < 50dma"
	default:
		// HYG < 50dma + SPY 52w high missing: we can't tell whether
		// the divergence is "near highs" or not. Surface honestly
		// rather than guess.
		row.band, row.reason = bandUnranked, "HYG < 50dma · spy_52w_high missing"
	}
	return row
}

func rowUSDJPY(r rpc.RegimeUSDJPY) regimeRow {
	row := regimeRow{name: "USD/JPY", status: r.Status}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		row.value = "—"
		row.stateNote = ifNonEmpty(r.ErrorMessage, "no FX tick")
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
	row.value = fmt.Sprintf("%.4f  %s", deref(r.Last), wkly)
	// Spec: yen strengthening (USD/JPY *falling*) is the risk signal.
	// Convention: WeeklyChange negative = yen strengthening.
	if r.WeeklyChange == nil {
		row.band, row.reason = bandUnranked, "weekly_change_pct missing"
		return row
	}
	move := -*r.WeeklyChange // positive when yen strengthening
	switch {
	case move < usdJpyMoveYellow:
		row.band, row.reason = bandGreen, "<1% weekly"
	case move < usdJpyMoveRed:
		row.band, row.reason = bandYellow, "yen strengthening 1–2%"
	default:
		row.band, row.reason = bandRed, "yen strengthening ≥2%"
	}
	return row
}

func rowGamma(r rpc.RegimeGammaZero) regimeRow {
	row := regimeRow{name: "SPY γ-zero", status: r.Status}
	switch r.Status {
	case rpc.RegimeStatusComputing:
		row.value = ""
		eta := r.Envelope.EtaSeconds
		note := fmt.Sprintf("computing  %ds ETA", eta)
		if r.Envelope.Progress > 0 {
			note += fmt.Sprintf(" · %d%%", r.Envelope.Progress)
		}
		row.stateNote = note
		row.reason = "re-run later for the cached result"
		return row
	case rpc.RegimeStatusError:
		row.value = ""
		row.stateNote = ifNonEmpty(r.Envelope.Error, "compute failed")
		return row
	case rpc.RegimeStatusOK:
		c := r.Envelope.Result
		if c == nil {
			row.value = "—"
			row.stateNote = "envelope missing payload"
			return row
		}
		flip := "—"
		gap := ""
		if c.ZeroGamma != nil {
			flip = fmt.Sprintf("%.2f", *c.ZeroGamma)
			if c.GapPct != nil {
				sign := "+"
				if *c.GapPct < 0 {
					sign = ""
				}
				gap = fmt.Sprintf("  %s%.1f%%", sign, *c.GapPct)
			}
		}
		row.value = fmt.Sprintf("spot %.2f → flip %s%s", c.SpotUnderlying, flip, gap)
		switch {
		case c.ZeroGamma == nil || c.GapPct == nil:
			row.band, row.reason = bandUnranked, "no zero-crossing in sweep"
		case *c.GapPct > gammaGapYellow:
			row.band, row.reason = bandGreen, "spot >2% above flip"
		case *c.GapPct >= -gammaGapYellow:
			row.band, row.reason = bandYellow, "spot within ±2% of flip"
		default:
			row.band, row.reason = bandRed, "spot below flip"
		}
		return row
	}
	row.value = "—"
	row.stateNote = string(r.Status)
	return row
}

func rowBreadth(r rpc.RegimeBreadth) regimeRow {
	row := regimeRow{name: "SPX breadth", status: r.Status}
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		switch r.Status {
		case rpc.RegimeStatusUnavailable:
			row.stateNote = "unavailable"
			row.reason = "S5FI feed not entitled on retail IBKR"
		default:
			row.stateNote = string(r.Status)
		}
		return row
	}
	v := r.Envelope.Value
	row.value = fmt.Sprintf("%.1f%% above 50-DMA", v)
	// Renderer caveat: spec red band also requires "SPX within 3% of
	// 52w high" but we don't have SPX 52w-high context inside this row.
	// Conservative call: report red on raw breadth only; do not
	// downgrade to yellow. The spec is most concerned about the
	// breadth collapse itself; the SPX-at-highs modifier sharpens the
	// signal but doesn't invent it.
	switch {
	case v >= breadthGreen:
		row.band, row.reason = bandGreen, ">55%"
	case v >= breadthRed:
		row.band, row.reason = bandYellow, "40–55%"
	default:
		row.band, row.reason = bandRed, "<40%"
	}
	return row
}

// ----------------------------------------------------------------------------
// --explain block: dim the section header + indent each row's notes
// under its name. Used for full methodology disclosure when the user
// passes --explain. The notes content comes verbatim from the daemon,
// which already embeds the spec's threshold language.

func renderExplainBlock(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult) {
	type entry struct {
		name, notes string
		missing     []string
	}
	entries := []entry{
		{"VIX/VIX3M", r.VIXTermStructure.Notes, r.VIXTermStructure.FieldsMissing},
		{"HYG vs SPY", r.HYGSPYDivergence.Notes, r.HYGSPYDivergence.FieldsMissing},
		{"USD/JPY", r.USDJPY.Notes, r.USDJPY.FieldsMissing},
		{"SPY γ-zero", r.GammaZero.Notes, r.GammaZero.FieldsMissing},
		{"SPX breadth", r.Breadth.Notes, r.Breadth.FieldsMissing},
	}
	fmt.Fprintln(out, env.dim("  Spec thresholds + methodology (see "+r.SpecDoc+" for full disclosure):"))
	fmt.Fprintln(out)
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", env.bold(e.name))
		fmt.Fprintf(out, "  %s\n", env.dim(e.notes))
		if len(e.missing) > 0 {
			fmt.Fprintf(out, "  %s\n", env.dim("(missing: "+strings.Join(e.missing, ", ")+")"))
		}
		fmt.Fprintln(out)
	}
}

// ----------------------------------------------------------------------------
// Tiny helpers — local to this file, not promoted to cli.go because no
// other renderer needs them.

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func ifNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
