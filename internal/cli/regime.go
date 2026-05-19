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
	// quality is the row's compact provenance tag, e.g. "· est 18s" or
	// "· modelled". Empty string when every value on the row came from
	// a firm-live tick — the default case stays unannotated to keep the
	// rendering uncluttered. Each row builder computes this from the
	// rpc.Quality pointers attached to the values it consumed.
	quality string
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
	now := r.AsOf
	rows := []regimeRow{
		rowVIXTerm(now, r.VIXTermStructure),
		rowHYGSPY(now, r.HYGSPYDivergence),
		rowUSDJPY(now, r.USDJPY),
		rowGamma(now, r.GammaZero),
		rowBreadth(now, r.Breadth),
	}
	c := tallyComposite(rows)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Risk Regime  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(out)

	// SPY + VIX headline: the two anchors every other indicator below
	// is interpreted against. SPY change is colored on sign (green up /
	// red down); VIX is colored inverted (red on up = vol expanding =
	// risk-off, green on down). Either half is dropped when its primary
	// price didn't land — the headline never invents numbers.
	if line := renderRegimeHeadline(env, r); line != "" {
		fmt.Fprintf(out, "  %s\n", line)
		fmt.Fprintln(out)
	}

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
	// Compose the suffix: quality tag (from Quality envelopes) takes
	// precedence over the legacy "· stale tick" indicator. The two
	// surface the same idea via different layers — the tag is more
	// specific (firm vs estimate vs modelled), so prefer it when the
	// daemon populated Quality.
	suffix := ""
	switch {
	case r.quality != "":
		suffix = "  " + env.dim(r.quality)
	case r.status == rpc.RegimeStatusStale:
		suffix = "  " + env.dim("· stale tick")
	}
	return fmt.Sprintf("  %s  %s  %s  %s%s%s",
		r.glyph(env), padRightVisible(r.name, nameW), padRightVisible(value, valueW), bandCell, reason, suffix)
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
			return 4
		case q.FreshnessClass == rpc.FreshnessDerived || q.Confidence == rpc.ConfidenceEstimate:
			return 3
		case q.FreshnessClass == rpc.FreshnessFrozen:
			return 2
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
		4: {"· modelled", 5 * time.Minute, "%s %dm old", func(d time.Duration) int { return int(d.Minutes()) }},
		3: {"· est", 5 * time.Second, "%s %ds", func(d time.Duration) int { return int(d.Seconds()) }},
		2: {"· frozen", 5 * time.Second, "%s %ds", func(d time.Duration) int { return int(d.Seconds()) }},
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

// renderRegimeHeadline returns the SPY + VIX summary line shown above
// the indicator rows: spot price + day's dollar / percent change for
// SPY, spot + percent change for VIX (which has no shares so dollar
// change is meaningless). Each side renders only when its primary
// price arrived; either half can be missing without dropping the line.
// Returns "" when neither half has data.
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
	row := regimeRow{name: "VIX/VIX3M", status: r.Status}
	if r.Status == rpc.RegimeStatusError || r.Ratio == nil {
		row.value = "—"
		row.stateNote = ifNonEmpty(r.ErrorMessage, "ratio unavailable")
		return row
	}
	row.value = fmt.Sprintf("%.3f  (%.2f / %.2f)", *r.Ratio, deref(r.VIX), deref(r.VIX3M))
	row.quality = qualityTag(now, r.VIXQuality, r.VIX3MQuality)
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

func rowHYGSPY(now time.Time, r rpc.RegimeHYGSPYDivergence) regimeRow {
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
	row.quality = qualityTag(now, r.HYGQuality, r.HYG50DMAQuality, r.SPYQuality, r.SPY52WHighQuality)
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

func rowUSDJPY(now time.Time, r rpc.RegimeUSDJPY) regimeRow {
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
	row.quality = qualityTag(now, r.LastQuality, r.Close7DAgoQuality)
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

func rowGamma(now time.Time, r rpc.RegimeGammaZero) regimeRow {
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
		row.reason = "first call of the NY session; re-poll for result"
		return row
	case rpc.RegimeStatusError:
		row.value = ""
		row.stateNote = ifNonEmpty(r.Envelope.Error, "compute failed")
		row.reason = "next regime call after 60 s will retry"
		return row
	case rpc.RegimeStatusOK:
		c := r.Envelope.Result
		if c == nil {
			row.value = "—"
			row.stateNote = "envelope missing payload"
			return row
		}
		// Gamma's two scalars are always modelled (zero_gamma via the
		// BS sweep) or derived (|Γ|·OI sum from observed OI+IV); the
		// row will carry "· modelled" regardless of ranking.
		row.quality = qualityTag(now, r.ZeroGammaQuality, r.GammaTotalAbsQuality)
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
			switch {
			case *c.GapPct > gammaGapYellow:
				row.band, row.reason = bandGreen, "spot >2% above γ-zero"
			case *c.GapPct >= -gammaGapYellow:
				row.band, row.reason = bandYellow, "spot within ±2% of γ-zero"
			default:
				row.band, row.reason = bandRed, "spot below γ-zero"
			}
			return row
		}
		// No crossing. The compute's GammaSign tells us which side of
		// zero the whole swept profile landed on — that IS the regime
		// statement the renderer should surface.
		gabsBn := c.GammaTotalAbs / 1e9
		switch c.GammaSign {
		case "positive":
			row.value = fmt.Sprintf("spot %.2f · long-γ  |Γ|·OI %.1fbn",
				c.SpotUnderlying, gabsBn)
			row.band = bandGreen
			row.reason = "dealer long-γ across ±15% sweep — stabilizing regime, γ-zero is well below spot"
		case "negative":
			row.value = fmt.Sprintf("spot %.2f · short-γ  |Γ|·OI %.1fbn",
				c.SpotUnderlying, gabsBn)
			row.band = bandRed
			row.reason = "dealer short-γ across ±15% sweep — amplifying regime, γ-zero is well above spot"
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

func rowBreadth(now time.Time, r rpc.RegimeBreadth) regimeRow {
	row := regimeRow{name: "SPX breadth", status: r.Status}
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		switch r.Status {
		case rpc.RegimeStatusUnavailable:
			row.stateNote = "unavailable"
			row.reason = "breadth engine offline (no cached snapshot)"
		case rpc.RegimeStatusComputing:
			row.stateNote = "computing"
			// ~60 min is the IBKR-pacing-limited cold-start cost
			// (60 historical-data requests per 10-min sliding window
			// × 503 names ≈ 85 min in the worst case; observed ~60).
			// Mention --foreground so the user knows how to keep the
			// daemon alive long enough to finish.
			row.reason = "cold-start refresh (~60 min, IBKR-paced); use 'ibkr daemon --foreground'"
		default:
			row.stateNote = string(r.Status)
		}
		return row
	}
	v := r.Envelope.Value
	row.value = fmt.Sprintf("%.1f%% above 50-DMA", v)
	row.quality = qualityTag(now, r.ValueQuality)
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
	type qField struct {
		name string
		q    *rpc.Quality
	}
	type entry struct {
		name, notes string
		missing     []string
		quals       []qField
	}
	entries := []entry{
		{"VIX/VIX3M", r.VIXTermStructure.Notes, r.VIXTermStructure.FieldsMissing, []qField{
			{"VIX", r.VIXTermStructure.VIXQuality},
			{"VIX3M", r.VIXTermStructure.VIX3MQuality},
		}},
		{"HYG vs SPY", r.HYGSPYDivergence.Notes, r.HYGSPYDivergence.FieldsMissing, []qField{
			{"HYG", r.HYGSPYDivergence.HYGQuality},
			{"HYG_50DMA", r.HYGSPYDivergence.HYG50DMAQuality},
			{"SPY", r.HYGSPYDivergence.SPYQuality},
			{"SPY_52w_high", r.HYGSPYDivergence.SPY52WHighQuality},
		}},
		{"USD/JPY", r.USDJPY.Notes, r.USDJPY.FieldsMissing, []qField{
			{"Last", r.USDJPY.LastQuality},
			{"Close_7d_ago", r.USDJPY.Close7DAgoQuality},
		}},
		{"SPY γ-zero", r.GammaZero.Notes, r.GammaZero.FieldsMissing, []qField{
			{"Zero_gamma", r.GammaZero.ZeroGammaQuality},
			{"|Gamma|.OI_sum", r.GammaZero.GammaTotalAbsQuality},
		}},
		{"SPX breadth", r.Breadth.Notes, r.Breadth.FieldsMissing, []qField{
			{"Value", r.Breadth.ValueQuality},
		}},
	}
	fmt.Fprintln(out, env.dim("  Spec thresholds + methodology (see "+r.SpecDoc+" for full disclosure):"))
	fmt.Fprintln(out)
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", env.bold(e.name))
		// Per-scalar provenance block. Each field shows freshness +
		// confidence + age + source so the reader can audit any number
		// they're about to act on. Nil-Quality fields are silently
		// skipped — keeps legacy daemons / fixtures working.
		for _, qf := range e.quals {
			if qf.q == nil {
				continue
			}
			age := r.AsOf.Sub(qf.q.AsOf)
			ageStr := ""
			if age > time.Second {
				ageStr = fmt.Sprintf(" · age %ds", int(age.Seconds()))
			}
			src := ""
			if qf.q.Source != "" {
				src = " · " + qf.q.Source
			}
			fmt.Fprintf(out, "    %s\n", env.dim(fmt.Sprintf("%-15s %s %s%s%s",
				qf.name, qf.q.Confidence, qf.q.FreshnessClass, ageStr, src)))
		}
		// The gamma row gets two extra surfaces specific to its
		// modelled-output nature: a BS-IV-derived disclosure when the
		// fallback fired, and a plain-English read of what γ-zero means.
		if e.name == "SPY γ-zero" {
			if res := r.GammaZero.Envelope.Result; res != nil && res.DerivedIVLegs > 0 {
				fmt.Fprintf(out, "    %s\n", env.dim(fmt.Sprintf(
					"compute used %d/%d legs with BS-IV from prior-session last price (model engine idle off-hours)",
					res.DerivedIVLegs, res.LegCount)))
			}
		}
		fmt.Fprintf(out, "  %s\n", env.dim(e.notes))
		if e.name == "SPY γ-zero" {
			fmt.Fprintf(out, "  %s\n", env.dim("The γ-zero level is where dealer net gamma crosses zero. Above γ-zero, dealer hedging dampens moves (stabilizing regime). Below, dealer hedging amplifies moves (volatile regime). Within ±2% the regime can flip on a single session. When the sweep shows no crossing inside ±15%, the row bands on the signed profile: long-γ (well above γ-zero, stable) or short-γ (well below, amplifying)."))
		}
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
