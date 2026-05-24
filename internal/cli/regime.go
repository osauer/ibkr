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
	moveYellow         = 100.0
	moveRed            = 130.0
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
	explain := fs.Bool("explain", false, "show spec thresholds, streaks, and quality under each row")
	watch := fs.Bool("watch", false, "re-poll continuously; in-place redraw on a TTY")
	rate := fs.Duration("rate", 5*time.Minute, "poll interval for --watch (default 5m)")
	logPath := fs.String("log", "", "append snapshot to a JSONL file at <path> (one line per call)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "regime: takes no positional args (got %v)", fs.Args())
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
		return renderRegimeTextTo(env, out, &res, *explain)
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
	indicators := "VIX VIX3M VVIX MOVE HYG SPY OAS funding USD.JPY gamma breadth"
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
// rule; one-line indicator rows; optional --explain footer with
// the spec's prose per row. Pass explain=true for the verbose mode.
func renderRegimeTextTo(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, explain bool) int {
	now := r.AsOf
	rows := []regimeRow{
		rowVIXTerm(now, r.VIXTermStructure),
		rowVolOfVol(now, r.VolOfVol),
		rowRatesVol(now, r.RatesVol),
		rowHYGSPY(now, r.HYGSPYDivergence),
		rowCreditSpreads(now, r.CreditSpreads),
		rowFundingStress(now, r.FundingStress),
		rowUSDJPY(now, r.USDJPY),
		rowGamma(now, r.GammaZero),
		rowBreadth(now, r.Breadth),
	}
	c := tallyComposite(rows)

	// Shared hero: title + timestamp share one line; SPY+VIX anchor
	// sits below indented; the regime summary line is optional (empty
	// when no ranked indicators have landed, but the verdict + count
	// summary below still surface).
	renderCommandHero(out,
		"Risk Regime",
		r.AsOf.Format("2006-01-02 15:04 MST"),
		renderRegimeHeadline(env, r),
		"")

	// Hero summary: a color-coded regime label plus a plain-English
	// evidence balance. The table below remains the audit trail; this
	// line is the three-second read.
	fmt.Fprintf(out, "  %s %s\n", env.bold(colorRegimeLabel(env, c, c.verdict())), env.dim("· "+c.evidence()))
	if indicatorEvidence := c.indicatorEvidence(); indicatorEvidence != c.evidence() {
		fmt.Fprintf(out, "  %s\n", env.dim("Indicators: "+indicatorEvidence))
	}
	fmt.Fprintf(out, "  Punch line: %s\n", regimePunchLine(rows))
	fmt.Fprintln(out)

	// Header row + horizontal rule give the reader a key for the
	// columns. Dim-colored so the band rows stay visually dominant. The
	// rule width matches the widest row layout (glyph + name + value +
	// band + note), recomputed once here so it tracks layout changes.
	fmt.Fprintln(out, renderRegimeHeader(env))
	for _, row := range rows {
		fmt.Fprintln(out, renderRow(env, row))
	}

	fmt.Fprintln(out)
	if explain {
		renderExplainBlock(env, out, r)
		return 0
	}
	fmt.Fprintln(out, env.dim("  Pass --explain for per-indicator spec thresholds, streaks, and quality notes."))
	return 0
}

// renderRegimeHeader returns the dim column-header line displayed above
// the indicator rows. Aligned with renderRow's column widths so labels
// land directly over their cells. Reads as a key for the reader without
// needing to consult docs.
func renderRegimeHeader(env *Env) string {
	const (
		nameW  = 17
		valueW = 30
		bandW  = 7
	)
	// "  " = 2-space indent + " " = where the glyph sits + 2 spaces.
	header := fmt.Sprintf("     %s  %s  %s  %s",
		padRightVisible("INDICATOR", nameW),
		padRightVisible("VALUE", valueW),
		padRightVisible("BAND", bandW),
		"NOTE")
	return env.dim(header)
}

// renderRow lays out one indicator line: 2-space indent, glyph, indicator
// name (left-padded to 17 — fits the combined-scope "γ-zero (SPY+SPX)"
// label), value cell (left-padded to 30), band word (color, padded to 7
// visible cells), reason (dim parenthetical), optional streak marker
// ("day 3"), optional quality/stale suffix. The band-word color is
// applied AFTER padding so column alignment stays correct under ANSI
// escapes — same trick as the account renderer's padLeftVisible.
func renderRow(env *Env, r regimeRow) string {
	const (
		nameW  = 17
		valueW = 30
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
	// In default view, NOTE = the reason string verbatim (no surrounding
	// parens, no quality clock). The reader sees a clean column with
	// short interpretive text per indicator. Quality / streak / stale
	// markers are promoted to --explain since they're noise on the
	// default scan and useful only when auditing one indicator's
	// provenance.
	note := env.dim(r.reason)
	return fmt.Sprintf("  %s  %s  %s  %s  %s",
		r.glyph(env), padRightVisible(r.name, nameW), padRightVisible(value, valueW), bandCell, note)
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
		return "No ranked indicators — see rows below for state"
	case c.clusterRanked < verdictFloor:
		return "Insufficient signal — too few indicators ranked"
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
		parts = append(parts, fmt.Sprintf("%d green %s", c.clusterGreen, plural(c.clusterGreen, "cluster", "clusters")))
	}
	if c.clusterYellow > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow %s", c.clusterYellow, plural(c.clusterYellow, "cluster", "clusters")))
	}
	if c.clusterRed > 0 {
		parts = append(parts, fmt.Sprintf("%d red %s", c.clusterRed, plural(c.clusterRed, "cluster", "clusters")))
	}
	if c.clusterUnranked > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked %s", c.clusterUnranked, plural(c.clusterUnranked, "cluster", "clusters")))
	}
	if len(parts) == 0 {
		return "0 ranked clusters"
	}
	return strings.Join(parts, " / ")
}

func (c regimeComposite) indicatorEvidence() string {
	var parts []string
	if c.green > 0 {
		parts = append(parts, fmt.Sprintf("%d green", c.green))
	}
	if c.yellow > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow", c.yellow))
	}
	if c.red > 0 {
		parts = append(parts, fmt.Sprintf("%d red", c.red))
	}
	if c.unranked > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked", c.unranked))
	}
	if len(parts) == 0 {
		return "0 ranked"
	}
	return strings.Join(parts, " / ")
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

func regimePunchLine(rows []regimeRow) string {
	groups := map[string][]string{}
	for _, row := range rows {
		key := row.band.labelKey(row.status)
		groups[key] = append(groups[key], regimeEvidenceName(row.name))
	}
	var parts []string
	for _, spec := range []struct {
		key  string
		word string
	}{
		{"red", "stressed"},
		{"yellow", "mixed"},
		{"green", "constructive"},
		{"computing", "computing"},
		{"unavailable", "unavailable"},
		{"unranked", "unranked"},
	} {
		names := groups[spec.key]
		if len(names) == 0 {
			continue
		}
		verb := "is"
		if len(names) > 1 {
			verb = "are"
		}
		parts = append(parts, fmt.Sprintf("%s %s %s", joinHumanList(names), verb, spec.word))
	}
	if len(parts) == 0 {
		return "No regime indicators produced a rankable reading."
	}
	return strings.Join(parts, "; ") + "."
}

func (b regimeBand) labelKey(status string) string {
	switch b {
	case bandGreen:
		return "green"
	case bandYellow:
		return "yellow"
	case bandRed:
		return "red"
	}
	switch status {
	case rpc.RegimeStatusComputing:
		return "computing"
	case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		return "unavailable"
	default:
		return "unranked"
	}
}

func regimeEvidenceName(name string) string {
	switch {
	case strings.HasPrefix(name, "VIX/"):
		return "volatility term structure"
	case name == "VVIX":
		return "vol-of-vol"
	case name == "MOVE":
		return "rates volatility"
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

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
	row := regimeRow{name: "VIX/VIX3M", cluster: "equity_vol", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Ratio == nil {
		row.value = "—"
		row.stateNote = "ratio unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "VIX/VIX3M tick missing")
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

func rowVolOfVol(now time.Time, r rpc.RegimeVolOfVol) regimeRow {
	row := regimeRow{name: "VVIX", cluster: "equity_vol", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.Last == nil {
		row.value = "—"
		row.stateNote = "VVIX unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "Cboe VVIX file unavailable")
		return row
	}
	row.value = fmt.Sprintf("%.1f", *r.Last)
	if r.Change20D != nil {
		row.value += fmt.Sprintf("  %+.1f%%/20d", *r.Change20D)
	}
	row.quality = qualityTag(now, r.ValueQuality)
	switch {
	case *r.Last < vvixYellow:
		row.band, row.reason = bandGreen, "<90 vol-of-vol"
	case *r.Last < vvixRed:
		row.band, row.reason = bandYellow, "90–110"
	default:
		row.band, row.reason = bandRed, ">110 vol-of-vol shock"
	}
	return row
}

func rowRatesVol(now time.Time, r rpc.RegimeRatesVol) regimeRow {
	row := regimeRow{name: "MOVE", cluster: "rates_vol", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.Last == nil {
		row.value = "—"
		row.stateNote = "MOVE unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "ICE/MOVE tick missing")
		return row
	}
	row.value = fmt.Sprintf("%.1f", *r.Last)
	if r.ChangePct != nil {
		row.value += fmt.Sprintf("  %+.1f%%", *r.ChangePct)
	}
	row.quality = qualityTag(now, r.ValueQuality)
	switch {
	case *r.Last < moveYellow:
		row.band, row.reason = bandGreen, "<100 rates vol"
	case *r.Last < moveRed:
		row.band, row.reason = bandYellow, "100–130"
	default:
		row.band, row.reason = bandRed, ">130 rates-vol stress"
	}
	return row
}

func rowHYGSPY(now time.Time, r rpc.RegimeHYGSPYDivergence) regimeRow {
	row := regimeRow{name: "HYG vs SPY", cluster: "credit", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError {
		row.value = "—"
		row.stateNote = "HYG/SPY unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "credit proxy tick missing")
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
	// Banding. HYG below 50dma while SPY is near highs is the credit-
	// equity divergence this row exists to catch. Streaks carry the
	// "is this sustained?" context; the band itself should not hide the
	// current divergence.
	switch {
	case r.HYG50DMA == nil:
		row.band, row.reason = bandUnranked, "50dma missing — cannot band"
	case *r.HYGPrice >= *r.HYG50DMA:
		row.band, row.reason = bandGreen, "HYG ≥ 50dma"
	case r.SPY52WHigh != nil && r.SPYPrice != nil && *r.SPYPrice >= hygSpyNearHighProx**r.SPY52WHigh:
		row.band, row.reason = bandRed, "HYG < 50dma · SPY near highs"
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

func rowCreditSpreads(now time.Time, r rpc.RegimeCreditSpreads) regimeRow {
	row := regimeRow{name: "HY/IG OAS", cluster: "credit", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.HYOAS == nil {
		row.value = "—"
		row.stateNote = "OAS unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "FRED OAS series unavailable")
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
		row.band, row.reason = bandRed, "HY OAS stress"
	case *r.HYOAS >= hyOASYellow || (r.HY20DChange != nil && *r.HY20DChange >= hyOASWidenYellow):
		row.band, row.reason = bandYellow, "HY OAS elevated/widening"
	default:
		row.band, row.reason = bandGreen, "HY OAS <4.0"
	}
	return row
}

func rowFundingStress(now time.Time, r rpc.RegimeFundingStress) regimeRow {
	row := regimeRow{name: "funding spread", cluster: "funding", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.SpreadBps == nil {
		row.value = "—"
		row.stateNote = "funding unavailable"
		row.reason = shortUnavailableReason(r.ErrorMessage, "FRED funding series unavailable")
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
	row.value = fmt.Sprintf("%.0fbp  CP %s / TB %s", *r.SpreadBps, cp, tb)
	row.quality = qualityTag(now, r.CP3MQuality, r.TBill3MQuality, r.SpreadQuality)
	switch {
	case *r.SpreadBps < fundingYellowBps:
		row.band, row.reason = bandGreen, "<25bp"
	case *r.SpreadBps < fundingRedBps:
		row.band, row.reason = bandYellow, "25–75bp"
	default:
		row.band, row.reason = bandRed, ">75bp funding stress"
	}
	return row
}

func rowUSDJPY(now time.Time, r rpc.RegimeUSDJPY) regimeRow {
	row := regimeRow{name: "USD/JPY", cluster: "fx_carry", status: r.Status, streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		row.value = "—"
		row.stateNote = "no FX tick"
		row.reason = shortUnavailableReason(r.ErrorMessage, "check IDEALPRO entitlement")
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
	row := regimeRow{name: gammaRowLabel(r), cluster: "dealer_gamma", status: r.Status, streak: streakMarker(r.Streak)}
	switch r.Status {
	case rpc.RegimeStatusComputing:
		row.value = ""
		eta := r.Envelope.EtaSeconds
		note := fmt.Sprintf("computing  %ds ETA", eta)
		if r.Envelope.Progress > 0 {
			note += fmt.Sprintf(" · %d%%", r.Envelope.Progress)
		}
		// If the in-flight compute is a retry of a recent failure,
		// surface the prior error context so the user sees what's
		// being re-attempted instead of a clean "first call of the
		// NY session" message that hides the previous abort.
		if r.Envelope.RetryOfErrorAt != nil && r.Envelope.RetryOfErrorSummary != "" {
			row.reason = fmt.Sprintf("retry of %q at %s",
				r.Envelope.RetryOfErrorSummary,
				r.Envelope.RetryOfErrorAt.Local().Format("15:04:05"))
		} else {
			row.reason = "first call of the NY session; re-poll for result"
		}
		row.stateNote = note
		return row
	case rpc.RegimeStatusError:
		row.value = ""
		row.stateNote = ifNonEmpty(r.Envelope.Error, "compute failed")
		row.reason = "next regime call after 60 s will retry"
		return row
	case rpc.RegimeStatusUnavailable:
		row.value = ""
		row.stateNote = "unavailable"
		if r.Envelope.Status == rpc.GammaZeroStatusCold {
			row.reason = "no cached gamma snapshot"
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
		// Gamma's two scalars are always modelled (zero_gamma via the
		// BS sweep) or derived (|Γ|·OI sum from observed OI+IV); the
		// row will carry "· modelled" regardless of ranking.
		row.quality = qualityTag(now, r.ZeroGammaQuality, r.GammaTotalAbsQuality)
		if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
			row.value = formatRegimeAgreement(c)
			if c.GammaTotalAbs > 0 {
				row.value += fmt.Sprintf("  |Γ|·OI %.1fbn", c.GammaTotalAbs/1e9)
			}
			row.band = gammaCombinedRegimeBand(c)
			switch row.band {
			case bandGreen:
				row.reason = "SPY/SPX both stabilizing"
			case bandRed:
				if c.RegimeAgreement == "agree:short-gamma" {
					row.reason = "SPY/SPX both amplifying"
				} else if c.RegimeAgreement == "disagree" {
					row.reason = "SPY/SPX disagree; dominant gamma exposure is amplifying"
				} else {
					row.reason = "dominant gamma exposure is amplifying"
				}
			case bandYellow:
				if c.RegimeAgreement == "disagree" {
					row.reason = "SPY/SPX gamma regimes disagree"
				} else {
					row.reason = "mixed per-index gamma bands"
				}
			default:
				row.reason = "no usable per-index gamma profile"
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
		// statement the renderer should surface. Magnitude (|Γ|·OI) is
		// the convention-free co-primary; rendered inline only when it
		// landed non-zero so the no-aggregator-data case (zero from
		// either an empty profile or a v2 daemon) doesn't paint a
		// misleading "$0.0bn" in the value cell.
		mag := ""
		if c.GammaTotalAbs > 0 {
			mag = fmt.Sprintf("  |Γ|·OI %.1fbn", c.GammaTotalAbs/1e9)
		}
		spotPrefix := fmt.Sprintf("spot %.2f · ", c.SpotUnderlying)
		switch c.GammaSign {
		case "positive":
			row.value = fmt.Sprintf("%slong-γ%s", spotPrefix, mag)
			row.band = bandGreen
			row.reason = "dealer long-γ · stabilizing"
		case "negative":
			row.value = fmt.Sprintf("%sshort-γ%s", spotPrefix, mag)
			row.band = bandRed
			row.reason = "dealer short-γ · amplifying"
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

func gammaCombinedRegimeBand(c *rpc.GammaZeroComputed) regimeBand {
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

func rowBreadth(now time.Time, r rpc.RegimeBreadth) regimeRow {
	row := regimeRow{name: "SPX breadth", cluster: "breadth", status: r.Status, streak: streakMarker(r.Streak)}
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
	v50 := r.Envelope.PctAbove50DMA
	v200 := r.Envelope.PctAbove200DMA
	// Compact two-number value cell: "47%50d · 62%200d  +1.4% nh".
	// "nh" stands for "net new highs". The annotation reads cramped at
	// first glance but matches the rest of the rows' single-row layout
	// — fuller breakdown lives in `ibkr breadth`.
	row.value = fmt.Sprintf("%.0f%%50d · %.0f%%200d", v50, v200)
	if r.NewHighsToday > 0 || r.NewLowsToday > 0 {
		row.value += fmt.Sprintf("  %+.1f%% nh", r.NetNewHighsPct)
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
		row.band, row.reason = bandGreen, ">55% (50d)"
	case v50 >= breadthRed:
		row.band, row.reason = bandYellow, "40–55% (50d)"
	default:
		row.band, row.reason = bandRed, "<40% (50d)"
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
		{"VVIX", r.VolOfVol.Notes, nil, []qField{
			{"VVIX", r.VolOfVol.ValueQuality},
		}},
		{"MOVE", r.RatesVol.Notes, nil, []qField{
			{"MOVE", r.RatesVol.ValueQuality},
		}},
		{"HYG vs SPY", r.HYGSPYDivergence.Notes, r.HYGSPYDivergence.FieldsMissing, []qField{
			{"HYG", r.HYGSPYDivergence.HYGQuality},
			{"HYG_50DMA", r.HYGSPYDivergence.HYG50DMAQuality},
			{"SPY", r.HYGSPYDivergence.SPYQuality},
			{"SPY_52w_high", r.HYGSPYDivergence.SPY52WHighQuality},
		}},
		{"HY/IG OAS", r.CreditSpreads.Notes, r.CreditSpreads.FieldsMissing, []qField{
			{"HY_OAS", r.CreditSpreads.HYOASQuality},
			{"IG_OAS", r.CreditSpreads.IGOASQuality},
			{"HY-IG_spread", r.CreditSpreads.SpreadQuality},
		}},
		{"funding spread", r.FundingStress.Notes, r.FundingStress.FieldsMissing, []qField{
			{"CP_3m", r.FundingStress.CP3MQuality},
			{"TBill_3m", r.FundingStress.TBill3MQuality},
			{"CP-TBill", r.FundingStress.SpreadQuality},
		}},
		{"USD/JPY", r.USDJPY.Notes, r.USDJPY.FieldsMissing, []qField{
			{"Last", r.USDJPY.LastQuality},
			{"Close_7d_ago", r.USDJPY.Close7DAgoQuality},
		}},
		{gammaRowLabel(r.GammaZero), r.GammaZero.Notes, r.GammaZero.FieldsMissing, []qField{
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
				switch {
				case age >= 48*time.Hour:
					ageStr = fmt.Sprintf(" · age %dd", int(age.Hours()/24))
				case age >= time.Hour:
					ageStr = fmt.Sprintf(" · age %dh", int(age.Hours()))
				default:
					ageStr = fmt.Sprintf(" · age %ds", int(age.Seconds()))
				}
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
		if e.name == gammaRowLabel(r.GammaZero) {
			if res := r.GammaZero.Envelope.Result; res != nil && res.DerivedIVLegs > 0 {
				fmt.Fprintf(out, "    %s\n", env.dim(fmt.Sprintf(
					"compute used %d/%d legs with BS-IV from option quote/close fallback (model engine idle)",
					res.DerivedIVLegs, res.LegCount)))
			}
		}
		fmt.Fprintf(out, "  %s\n", env.dim(e.notes))
		if e.name == gammaRowLabel(r.GammaZero) {
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

func shortUnavailableReason(message, fallback string) string {
	if message == "" {
		return fallback
	}
	switch {
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
