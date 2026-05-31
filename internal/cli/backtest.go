package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

type CanaryBacktestObservation struct {
	Date          string                   `json:"date,omitempty"`
	AsOf          time.Time                `json:"as_of,omitzero"`
	Case          string                   `json:"case,omitempty"`
	MarketCluster string                   `json:"market_cluster,omitempty"`
	Account       rpc.AccountResult        `json:"account"`
	Positions     rpc.PositionsResult      `json:"positions"`
	Regime        rpc.RegimeSnapshotResult `json:"regime"`
	Target        CanaryBacktestTarget     `json:"target"`
	Notes         string                   `json:"notes,omitempty"`
}

type CanaryBacktestTarget struct {
	Stress            bool     `json:"stress"`
	Kind              string   `json:"kind,omitempty"`
	WindowDays        int      `json:"window_days,omitempty"`
	DaysToStress      *int     `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct *float64 `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct       *float64 `json:"vix_shock_pct,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

type CanaryBacktestResult struct {
	RunAt        time.Time                      `json:"run_at"`
	Policy       string                         `json:"policy"`
	Observations []CanaryBacktestRowResult      `json:"observations"`
	Metrics      CanaryBacktestMetrics          `json:"metrics"`
	Clusters     []CanaryBacktestClusterMetrics `json:"clusters,omitempty"`
	Findings     []string                       `json:"findings,omitempty"`
	NotAdvice    string                         `json:"not_advice"`
}

type CanaryBacktestRowResult struct {
	Date             string                `json:"date,omitempty"`
	Case             string                `json:"case,omitempty"`
	MarketCluster    string                `json:"market_cluster,omitempty"`
	TargetStress     bool                  `json:"target_stress"`
	TargetKind       string                `json:"target_kind,omitempty"`
	WindowDays       int                   `json:"window_days,omitempty"`
	DaysToStress     *int                  `json:"days_to_stress,omitempty"`
	Direction        risk.SignalDirection  `json:"direction,omitempty"`
	Severity         risk.SignalSeverity   `json:"severity"`
	PlannerMode      risk.PlannerMode      `json:"planner_mode,omitempty"`
	PlannerReadiness risk.PlannerReadiness `json:"planner_readiness,omitempty"`
	DataConfidence   string                `json:"data_confidence,omitempty"`
	SignalConfidence string                `json:"signal_confidence,omitempty"`
	PrimaryDrivers   []risk.SignalID       `json:"primary_drivers,omitempty"`
	DefensiveWatch   bool                  `json:"defensive_watch"`
	DefensiveAct     bool                  `json:"defensive_act"`
	DataQualityWatch bool                  `json:"data_quality_watch"`
	Blocked          bool                  `json:"blocked"`
	Canary           *rpc.CanaryResult     `json:"canary,omitempty"`
}

type CanaryBacktestMetrics struct {
	Observations        int      `json:"observations"`
	TargetStress        int      `json:"target_stress"`
	NonStress           int      `json:"non_stress"`
	DefensiveWatch      int      `json:"defensive_watch"`
	DefensiveAct        int      `json:"defensive_act"`
	DataQualityWatch    int      `json:"data_quality_watch"`
	Blocked             int      `json:"blocked"`
	WatchTruePositive   int      `json:"watch_true_positive"`
	WatchFalsePositive  int      `json:"watch_false_positive"`
	WatchMiss           int      `json:"watch_miss"`
	WatchPrecision      *float64 `json:"watch_precision,omitempty"`
	WatchRecall         *float64 `json:"watch_recall,omitempty"`
	WatchFalseAlarmRate *float64 `json:"watch_false_alarm_rate,omitempty"`
	WatchAvgLeadDays    *float64 `json:"watch_avg_lead_days,omitempty"`
	ActTruePositive     int      `json:"act_true_positive"`
	ActFalsePositive    int      `json:"act_false_positive"`
	ActMiss             int      `json:"act_miss"`
	ActPrecision        *float64 `json:"act_precision,omitempty"`
	ActRecall           *float64 `json:"act_recall,omitempty"`
	ActFalseAlarmRate   *float64 `json:"act_false_alarm_rate,omitempty"`
	ActAvgLeadDays      *float64 `json:"act_avg_lead_days,omitempty"`
}

type CanaryBacktestClusterMetrics struct {
	Name    string                `json:"name"`
	Metrics CanaryBacktestMetrics `json:"metrics"`
}

type canaryBacktestAccumulator struct {
	metrics        CanaryBacktestMetrics
	watchLeadDays  int
	watchLeadCount int
	actLeadDays    int
	actLeadCount   int
}

func runBacktest(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "backtest")
	inputPath := fs.String("input", "", "JSONL point-in-time observations")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 || rest[0] != "canary" {
		return fail(env, "backtest: usage: ibkr backtest canary --input PATH [--json]")
	}
	if strings.TrimSpace(*inputPath) == "" {
		return fail(env, "backtest: --input is required")
	}
	f, err := os.Open(*inputPath)
	if err != nil {
		return fail(env, "backtest: open %s: %v", *inputPath, err)
	}
	defer f.Close()

	observations, err := readCanaryBacktestObservations(f)
	if err != nil {
		return fail(env, "backtest: %v", err)
	}
	res := runCanaryBacktest(observations, time.Now())
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderCanaryBacktestText(env, env.Stdout, &res)
}

func readCanaryBacktestObservations(r io.Reader) ([]CanaryBacktestObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []CanaryBacktestObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row CanaryBacktestObservation
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if row.Date == "" && row.AsOf.IsZero() {
			return nil, fmt.Errorf("line %d: date or as_of is required", lineNo)
		}
		if row.Date != "" {
			if _, err := time.Parse("2006-01-02", row.Date); err != nil {
				return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
			}
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func runCanaryBacktest(observations []CanaryBacktestObservation, runAt time.Time) CanaryBacktestResult {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	res := CanaryBacktestResult{
		RunAt:     runAt,
		Policy:    canaryPolicy.Name,
		NotAdvice: "Backtest diagnostic only; not investment advice or a trade recommendation.",
	}
	total := &canaryBacktestAccumulator{}
	byCluster := map[string]*canaryBacktestAccumulator{}
	for _, obs := range observations {
		row := runCanaryBacktestObservation(obs)
		res.Observations = append(res.Observations, row)
		total.add(row)
		cluster := cleanBacktestCluster(row.MarketCluster)
		if byCluster[cluster] == nil {
			byCluster[cluster] = &canaryBacktestAccumulator{}
		}
		byCluster[cluster].add(row)
	}
	res.Metrics = total.result()
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		res.Clusters = append(res.Clusters, CanaryBacktestClusterMetrics{Name: name, Metrics: byCluster[name].result()})
	}
	res.Findings = canaryBacktestFindings(res)
	return res
}

func runCanaryBacktestObservation(obs CanaryBacktestObservation) CanaryBacktestRowResult {
	input, asOf := canaryBacktestInput(obs)
	canary := ComputeCanary(input)
	watch := canaryBacktestDefensiveAtLeast(canary, risk.SeverityWatch)
	act := canaryBacktestDefensiveAtLeast(canary, risk.SeverityAct)
	dataQuality := canary.Direction == risk.DirectionDataQuality && severityRankAtLeast(canary.Severity, risk.SeverityWatch)
	blocked := canary.PlannerReadiness == risk.PlannerReadinessBlocked
	return CanaryBacktestRowResult{
		Date:             backtestDateLabel(obs, asOf),
		Case:             obs.Case,
		MarketCluster:    cleanBacktestCluster(obs.MarketCluster),
		TargetStress:     obs.Target.Stress,
		TargetKind:       obs.Target.Kind,
		WindowDays:       obs.Target.WindowDays,
		DaysToStress:     obs.Target.DaysToStress,
		Direction:        canary.Direction,
		Severity:         canary.Severity,
		PlannerMode:      canary.PlannerModeHint,
		PlannerReadiness: canary.PlannerReadiness,
		DataConfidence:   canary.DataConfidence,
		SignalConfidence: canary.SignalConfidence,
		PrimaryDrivers:   canary.PrimaryDrivers,
		DefensiveWatch:   watch,
		DefensiveAct:     act,
		DataQualityWatch: dataQuality,
		Blocked:          blocked,
		Canary:           &canary,
	}
}

func canaryBacktestInput(obs CanaryBacktestObservation) (CanaryInput, time.Time) {
	asOf := canaryBacktestAsOf(obs)
	acct := obs.Account
	if acct.AsOf.IsZero() {
		acct.AsOf = asOf
	}
	pos := obs.Positions
	if pos.AsOf.IsZero() {
		pos.AsOf = asOf
	}
	regime := obs.Regime
	if regime.AsOf.IsZero() {
		regime.AsOf = asOf
	}
	backfillBacktestRegimeComposite(&regime)
	return CanaryInput{Account: acct, Positions: pos, Regime: regime, Now: asOf}, asOf
}

func canaryBacktestAsOf(obs CanaryBacktestObservation) time.Time {
	if !obs.AsOf.IsZero() {
		return obs.AsOf
	}
	if obs.Date != "" {
		if t, err := time.Parse("2006-01-02", obs.Date); err == nil {
			return t
		}
	}
	return time.Time{}
}

func backtestDateLabel(obs CanaryBacktestObservation, asOf time.Time) string {
	if obs.Date != "" {
		return obs.Date
	}
	if !asOf.IsZero() {
		return asOf.Format("2006-01-02")
	}
	return ""
}

func cleanBacktestCluster(cluster string) string {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return "unclassified"
	}
	return cluster
}

func backfillBacktestRegimeComposite(r *rpc.RegimeSnapshotResult) {
	if r == nil || r.Composite.ClusterRankedCount+r.Composite.ClusterUnrankedCount > 0 {
		return
	}
	r.Composite = rpc.RegimeComposite{}
	indicatorBands := []string{
		r.VIXTermStructure.Band,
		r.VolOfVol.Band,
		r.HYGSPYDivergence.Band,
		r.CreditSpreads.Band,
		r.FundingStress.Band,
		r.USDJPY.Band,
		r.GammaZero.Band,
		r.Breadth.Band,
	}
	for _, band := range indicatorBands {
		switch strings.ToLower(strings.TrimSpace(band)) {
		case "green":
			r.Composite.GreenCount++
			r.Composite.RankedCount++
		case "yellow":
			r.Composite.YellowCount++
			r.Composite.RankedCount++
		case "red":
			r.Composite.RedCount++
			r.Composite.RankedCount++
		default:
			r.Composite.UnrankedCount++
		}
	}
	clusterBands := []string{
		strongestBand([]string{r.VIXTermStructure.Band, r.VolOfVol.Band}),
		strongestBand([]string{r.HYGSPYDivergence.Band, r.CreditSpreads.Band}),
		strongestBand([]string{r.FundingStress.Band}),
		strongestBand([]string{r.USDJPY.Band}),
		strongestBand([]string{r.GammaZero.Band}),
		strongestBand([]string{r.Breadth.Band}),
	}
	for _, band := range clusterBands {
		switch band {
		case "green":
			r.Composite.ClusterGreenCount++
			r.Composite.ClusterRankedCount++
		case "yellow":
			r.Composite.ClusterYellowCount++
			r.Composite.ClusterRankedCount++
		case "red":
			r.Composite.ClusterRedCount++
			r.Composite.ClusterRankedCount++
		default:
			r.Composite.ClusterUnrankedCount++
		}
	}
	r.Composite.Verdict = backtestRegimeVerdict(r.Composite, len(clusterBands))
}

func backtestRegimeVerdict(c rpc.RegimeComposite, clusterCount int) string {
	switch {
	case c.ClusterRankedCount == 0:
		return "No ranked indicators - see rows below for state"
	case c.ClusterRankedCount < verdictFloor:
		return "Insufficient ranked indicators - see rows below for state"
	case c.ClusterRedCount == clusterCount:
		return "Full risk-off conditions"
	case c.ClusterRedCount >= 3:
		return "Broad stress regime"
	case c.ClusterRedCount >= 1:
		return "Stress signal present"
	case c.ClusterYellowCount >= 3:
		return "Elevated stress watch"
	default:
		return "Normal regime"
	}
}

func canaryBacktestDefensiveAtLeast(res CanaryResult, severity risk.SignalSeverity) bool {
	if !severityRankAtLeast(res.Severity, severity) {
		return false
	}
	return res.Direction == risk.DirectionDefensive || res.Direction == risk.DirectionMixed
}

func (a *canaryBacktestAccumulator) add(row CanaryBacktestRowResult) {
	a.metrics.Observations++
	if row.TargetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	if row.DefensiveWatch {
		a.metrics.DefensiveWatch++
	}
	if row.DefensiveAct {
		a.metrics.DefensiveAct++
	}
	if row.DataQualityWatch {
		a.metrics.DataQualityWatch++
	}
	if row.Blocked {
		a.metrics.Blocked++
	}
	a.addWatch(row)
	a.addAct(row)
}

func (a *canaryBacktestAccumulator) addWatch(row CanaryBacktestRowResult) {
	switch {
	case row.TargetStress && row.DefensiveWatch:
		a.metrics.WatchTruePositive++
		if row.DaysToStress != nil {
			a.watchLeadDays += *row.DaysToStress
			a.watchLeadCount++
		}
	case row.TargetStress:
		a.metrics.WatchMiss++
	case row.DefensiveWatch:
		a.metrics.WatchFalsePositive++
	}
}

func (a *canaryBacktestAccumulator) addAct(row CanaryBacktestRowResult) {
	switch {
	case row.TargetStress && row.DefensiveAct:
		a.metrics.ActTruePositive++
		if row.DaysToStress != nil {
			a.actLeadDays += *row.DaysToStress
			a.actLeadCount++
		}
	case row.TargetStress:
		a.metrics.ActMiss++
	case row.DefensiveAct:
		a.metrics.ActFalsePositive++
	}
}

func (a *canaryBacktestAccumulator) result() CanaryBacktestMetrics {
	m := a.metrics
	m.WatchPrecision = ratioPtr(m.WatchTruePositive, m.WatchTruePositive+m.WatchFalsePositive)
	m.WatchRecall = ratioPtr(m.WatchTruePositive, m.TargetStress)
	m.WatchFalseAlarmRate = ratioPtr(m.WatchFalsePositive, m.NonStress)
	m.WatchAvgLeadDays = avgPtr(a.watchLeadDays, a.watchLeadCount)
	m.ActPrecision = ratioPtr(m.ActTruePositive, m.ActTruePositive+m.ActFalsePositive)
	m.ActRecall = ratioPtr(m.ActTruePositive, m.TargetStress)
	m.ActFalseAlarmRate = ratioPtr(m.ActFalsePositive, m.NonStress)
	m.ActAvgLeadDays = avgPtr(a.actLeadDays, a.actLeadCount)
	return m
}

func ratioPtr(num, denom int) *float64 {
	if denom == 0 {
		return nil
	}
	v := float64(num) / float64(denom)
	return &v
}

func avgPtr(sum, count int) *float64 {
	if count == 0 {
		return nil
	}
	v := float64(sum) / float64(count)
	return &v
}

func canaryBacktestFindings(res CanaryBacktestResult) []string {
	m := res.Metrics
	if m.Observations == 0 {
		return []string{"No observations were loaded."}
	}
	var findings []string
	if m.TargetStress == 0 {
		findings = append(findings, "No target stress rows were present; add labelled forward windows before reading precision or recall.")
	} else if m.WatchMiss == 0 {
		findings = append(findings, "Watch-level defensive alerts caught every labelled stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Watch-level defensive alerts missed %d labelled stress row(s).", m.WatchMiss))
	}
	if m.WatchFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Watch-level defensive alerts fired on %d non-stress row(s); inspect cluster false positives before tightening policy.", m.WatchFalsePositive))
	}
	if m.ActMiss > 0 && m.TargetStress > 0 {
		findings = append(findings, fmt.Sprintf("Act-level alerts caught %d/%d stress rows; treat act recall as a severity filter, not the only success gate.", m.ActTruePositive, m.TargetStress))
	}
	if m.DataQualityWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d data-quality watch row(s) were tracked separately from defensive false positives.", m.DataQualityWatch))
	}
	for _, cluster := range res.Clusters {
		cm := cluster.Metrics
		if cm.WatchMiss > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch miss(es).", cluster.Name, cm.WatchMiss))
		}
		if cm.WatchFalsePositive > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch false positive(s).", cluster.Name, cm.WatchFalsePositive))
		}
	}
	return findings
}

func renderCanaryBacktestText(env *Env, out io.Writer, r *CanaryBacktestResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Backtest  ·  %d observations  ·  policy %s\n", r.Metrics.Observations, r.Policy)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-12s %d stress / %d non-stress\n", "Targets", r.Metrics.TargetStress, r.Metrics.NonStress)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Watch",
		formatBacktestRate(r.Metrics.WatchPrecision),
		formatBacktestRate(r.Metrics.WatchRecall),
		formatBacktestRate(r.Metrics.WatchFalseAlarmRate),
		formatBacktestNumber(r.Metrics.WatchAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Act",
		formatBacktestRate(r.Metrics.ActPrecision),
		formatBacktestRate(r.Metrics.ActRecall),
		formatBacktestRate(r.Metrics.ActFalseAlarmRate),
		formatBacktestNumber(r.Metrics.ActAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s %d data-quality watch · %d blocked planner row(s)\n", "Quality", r.Metrics.DataQualityWatch, r.Metrics.Blocked)
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %9s %9s %9s %9s",
			"CLUSTER", "OBS", "STRESS", "WATCH TP", "WATCH FP", "ACT TP", "BLOCKED")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %9d %9d %9d %9d\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.ActTruePositive,
				m.Blocked,
			)
		}
		fmt.Fprintln(out)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(out, "Findings")
		for _, finding := range r.Findings {
			fmt.Fprintf(out, "  - %s\n", finding)
		}
		fmt.Fprintln(out)
	}
	if r.NotAdvice != "" {
		fmt.Fprintln(out, env.dim("  "+r.NotAdvice))
		fmt.Fprintln(out)
	}
	return 0
}

func formatBacktestRate(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "--"
	}
	return fmt.Sprintf("%.0f%%", *v*100)
}

func formatBacktestNumber(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "--"
	}
	return fmt.Sprintf("%.1fd", *v)
}

func truncateVisible(s string, width int) string {
	if visibleLen(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:0]
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "."
}
