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
	PortfolioPosture risk.PortfolioPosture `json:"portfolio_posture,omitempty"`
	Severity         risk.SignalSeverity   `json:"severity"`
	PlannerMode      risk.PlannerMode      `json:"planner_mode,omitempty"`
	PlannerReadiness risk.PlannerReadiness `json:"planner_readiness,omitempty"`
	DataConfidence   string                `json:"data_confidence,omitempty"`
	SignalConfidence string                `json:"signal_confidence,omitempty"`
	PrimaryDrivers   []risk.SignalID       `json:"primary_drivers,omitempty"`
	SignalWatch      bool                  `json:"signal_watch"`
	DefensiveWatch   bool                  `json:"defensive_watch"`
	DefensiveAct     bool                  `json:"defensive_act"`
	RebalanceWatch   bool                  `json:"rebalance_watch"`
	DataQualityWatch bool                  `json:"data_quality_watch"`
	Blocked          bool                  `json:"blocked"`
	Canary           *rpc.CanaryResult     `json:"canary,omitempty"`
}

type CanaryBacktestMetrics struct {
	Observations         int      `json:"observations"`
	TargetStress         int      `json:"target_stress"`
	NonStress            int      `json:"non_stress"`
	SignalWatch          int      `json:"signal_watch"`
	DefensiveWatch       int      `json:"defensive_watch"`
	DefensiveAct         int      `json:"defensive_act"`
	RebalanceWatch       int      `json:"rebalance_watch"`
	DataQualityWatch     int      `json:"data_quality_watch"`
	Blocked              int      `json:"blocked"`
	SignalTruePositive   int      `json:"signal_true_positive"`
	SignalFalsePositive  int      `json:"signal_false_positive"`
	SignalMiss           int      `json:"signal_miss"`
	SignalPrecision      *float64 `json:"signal_precision,omitempty"`
	SignalRecall         *float64 `json:"signal_recall,omitempty"`
	SignalFalseAlarmRate *float64 `json:"signal_false_alarm_rate,omitempty"`
	SignalAvgLeadDays    *float64 `json:"signal_avg_lead_days,omitempty"`
	WatchTruePositive    int      `json:"watch_true_positive"`
	WatchFalsePositive   int      `json:"watch_false_positive"`
	WatchMiss            int      `json:"watch_miss"`
	WatchPrecision       *float64 `json:"watch_precision,omitempty"`
	WatchRecall          *float64 `json:"watch_recall,omitempty"`
	WatchFalseAlarmRate  *float64 `json:"watch_false_alarm_rate,omitempty"`
	WatchAvgLeadDays     *float64 `json:"watch_avg_lead_days,omitempty"`
	ActTruePositive      int      `json:"act_true_positive"`
	ActFalsePositive     int      `json:"act_false_positive"`
	ActMiss              int      `json:"act_miss"`
	ActPrecision         *float64 `json:"act_precision,omitempty"`
	ActRecall            *float64 `json:"act_recall,omitempty"`
	ActFalseAlarmRate    *float64 `json:"act_false_alarm_rate,omitempty"`
	ActAvgLeadDays       *float64 `json:"act_avg_lead_days,omitempty"`
}

type CanaryBacktestClusterMetrics struct {
	Name    string                `json:"name"`
	Metrics CanaryBacktestMetrics `json:"metrics"`
}

type RegimeBacktestObservation struct {
	Date          string                   `json:"date,omitempty"`
	AsOf          time.Time                `json:"as_of,omitzero"`
	Case          string                   `json:"case,omitempty"`
	MarketCluster string                   `json:"market_cluster,omitempty"`
	Regime        rpc.RegimeSnapshotResult `json:"regime"`
	Target        RegimeBacktestTarget     `json:"target"`
	Notes         string                   `json:"notes,omitempty"`
}

type RegimeBacktestTarget struct {
	Stress            bool     `json:"stress"`
	Kind              string   `json:"kind,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	WindowDays        int      `json:"window_days,omitempty"`
	DaysToStress      *int     `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct *float64 `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct       *float64 `json:"vix_shock_pct,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

type RegimeBacktestResult struct {
	RunAt        time.Time                      `json:"run_at"`
	Policy       string                         `json:"policy"`
	Observations []RegimeBacktestRowResult      `json:"observations"`
	Metrics      RegimeBacktestMetrics          `json:"metrics"`
	Clusters     []RegimeBacktestClusterMetrics `json:"clusters,omitempty"`
	Findings     []string                       `json:"findings,omitempty"`
	NotAdvice    string                         `json:"not_advice"`
}

type RegimeBacktestRowResult struct {
	Date             string                    `json:"date,omitempty"`
	Case             string                    `json:"case,omitempty"`
	MarketCluster    string                    `json:"market_cluster,omitempty"`
	TargetStress     bool                      `json:"target_stress"`
	TargetKind       string                    `json:"target_kind,omitempty"`
	TargetScope      string                    `json:"target_scope,omitempty"`
	Scored           bool                      `json:"scored"`
	WindowDays       int                       `json:"window_days,omitempty"`
	DaysToStress     *int                      `json:"days_to_stress,omitempty"`
	Verdict          string                    `json:"verdict,omitempty"`
	RedClusters      int                       `json:"red_clusters"`
	YellowClusters   int                       `json:"yellow_clusters"`
	RankedClusters   int                       `json:"ranked_clusters"`
	UnrankedClusters int                       `json:"unranked_clusters"`
	RedClusterNames  []string                  `json:"red_cluster_names,omitempty"`
	StressWatch      bool                      `json:"stress_watch"`
	StressSignal     bool                      `json:"stress_signal"`
	DataQualityWatch bool                      `json:"data_quality_watch"`
	Regime           *rpc.RegimeSnapshotResult `json:"regime,omitempty"`
}

type RegimeBacktestMetrics struct {
	Observations         int      `json:"observations"`
	ScoredObservations   int      `json:"scored_observations"`
	OutOfScope           int      `json:"out_of_scope"`
	TargetStress         int      `json:"target_stress"`
	NonStress            int      `json:"non_stress"`
	StressWatch          int      `json:"stress_watch"`
	StressSignal         int      `json:"stress_signal"`
	DataQualityWatch     int      `json:"data_quality_watch"`
	WatchTruePositive    int      `json:"watch_true_positive"`
	WatchFalsePositive   int      `json:"watch_false_positive"`
	WatchMiss            int      `json:"watch_miss"`
	WatchPrecision       *float64 `json:"watch_precision,omitempty"`
	WatchRecall          *float64 `json:"watch_recall,omitempty"`
	WatchFalseAlarmRate  *float64 `json:"watch_false_alarm_rate,omitempty"`
	WatchAvgLeadDays     *float64 `json:"watch_avg_lead_days,omitempty"`
	StressTruePositive   int      `json:"stress_true_positive"`
	StressFalsePositive  int      `json:"stress_false_positive"`
	StressMiss           int      `json:"stress_miss"`
	StressPrecision      *float64 `json:"stress_precision,omitempty"`
	StressRecall         *float64 `json:"stress_recall,omitempty"`
	StressFalseAlarmRate *float64 `json:"stress_false_alarm_rate,omitempty"`
	StressAvgLeadDays    *float64 `json:"stress_avg_lead_days,omitempty"`
}

type RegimeBacktestClusterMetrics struct {
	Name    string                `json:"name"`
	Metrics RegimeBacktestMetrics `json:"metrics"`
}

type canaryBacktestAccumulator struct {
	metrics         CanaryBacktestMetrics
	signalLeadDays  int
	signalLeadCount int
	watchLeadDays   int
	watchLeadCount  int
	actLeadDays     int
	actLeadCount    int
}

type regimeBacktestAccumulator struct {
	metrics         RegimeBacktestMetrics
	watchLeadDays   int
	watchLeadCount  int
	stressLeadDays  int
	stressLeadCount int
}

func runBacktest(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "backtest")
	inputPath := fs.String("input", "", "JSONL point-in-time observations")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 || (rest[0] != "canary" && rest[0] != "regime") {
		return fail(env, "backtest: usage: ibkr backtest canary|regime --input PATH [--json]")
	}
	if strings.TrimSpace(*inputPath) == "" {
		return fail(env, "backtest: --input is required")
	}
	f, err := os.Open(*inputPath)
	if err != nil {
		return fail(env, "backtest: open %s: %v", *inputPath, err)
	}
	defer f.Close()

	if rest[0] == "regime" {
		observations, err := readRegimeBacktestObservations(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		res := runRegimeBacktest(observations, time.Now())
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderRegimeBacktestText(env, env.Stdout, &res)
	}

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

func readRegimeBacktestObservations(r io.Reader) ([]RegimeBacktestObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []RegimeBacktestObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row RegimeBacktestObservation
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

func runRegimeBacktest(observations []RegimeBacktestObservation, runAt time.Time) RegimeBacktestResult {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	res := RegimeBacktestResult{
		RunAt:     runAt,
		Policy:    "risk-regime-dashboard",
		NotAdvice: "Backtest diagnostic only; not investment advice or a trade recommendation.",
	}
	total := &regimeBacktestAccumulator{}
	byCluster := map[string]*regimeBacktestAccumulator{}
	for _, obs := range observations {
		row := runRegimeBacktestObservation(obs)
		res.Observations = append(res.Observations, row)
		total.add(row)
		cluster := cleanBacktestCluster(row.MarketCluster)
		if byCluster[cluster] == nil {
			byCluster[cluster] = &regimeBacktestAccumulator{}
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
		res.Clusters = append(res.Clusters, RegimeBacktestClusterMetrics{Name: name, Metrics: byCluster[name].result()})
	}
	res.Findings = regimeBacktestFindings(res)
	return res
}

func runCanaryBacktestObservation(obs CanaryBacktestObservation) CanaryBacktestRowResult {
	input, asOf := canaryBacktestInput(obs)
	canary := ComputeCanary(input)
	watch := canaryBacktestDefensiveAtLeast(canary, risk.SeverityWatch)
	act := canaryBacktestDefensiveAtLeast(canary, risk.SeverityAct)
	rebalance := canaryBacktestRebalanceAtLeast(canary, risk.SeverityWatch)
	dataQuality := canary.Direction == risk.DirectionDataQuality && severityRankAtLeast(canary.Severity, risk.SeverityWatch)
	blocked := canary.PlannerReadiness == risk.PlannerReadinessBlocked
	signalWatch := watch || rebalance
	return CanaryBacktestRowResult{
		Date:             backtestDateLabel(obs.Date, asOf),
		Case:             obs.Case,
		MarketCluster:    cleanBacktestCluster(obs.MarketCluster),
		TargetStress:     obs.Target.Stress,
		TargetKind:       obs.Target.Kind,
		WindowDays:       obs.Target.WindowDays,
		DaysToStress:     obs.Target.DaysToStress,
		Direction:        canary.Direction,
		PortfolioPosture: canary.PortfolioPosture,
		Severity:         canary.Severity,
		PlannerMode:      canary.PlannerModeHint,
		PlannerReadiness: canary.PlannerReadiness,
		DataConfidence:   canary.DataConfidence,
		SignalConfidence: canary.SignalConfidence,
		PrimaryDrivers:   canary.PrimaryDrivers,
		SignalWatch:      signalWatch,
		DefensiveWatch:   watch,
		DefensiveAct:     act,
		RebalanceWatch:   rebalance,
		DataQualityWatch: dataQuality,
		Blocked:          blocked,
		Canary:           &canary,
	}
}

func runRegimeBacktestObservation(obs RegimeBacktestObservation) RegimeBacktestRowResult {
	regime, asOf := regimeBacktestInput(obs)
	market := summarizeCanaryMarket(regime)
	stressWatch := regimeBacktestStressWatch(regime)
	stressSignal := regimeBacktestStressSignal(regime)
	dataQuality := regimeBacktestDataQualityWatch(regime)
	return RegimeBacktestRowResult{
		Date:             backtestDateLabel(obs.Date, asOf),
		Case:             obs.Case,
		MarketCluster:    cleanBacktestCluster(obs.MarketCluster),
		TargetStress:     obs.Target.Stress,
		TargetKind:       obs.Target.Kind,
		TargetScope:      cleanBacktestTargetScope(obs.Target.Scope),
		Scored:           regimeBacktestScoredScope(obs.Target.Scope),
		WindowDays:       obs.Target.WindowDays,
		DaysToStress:     obs.Target.DaysToStress,
		Verdict:          regime.Composite.Verdict,
		RedClusters:      regime.Composite.ClusterRedCount,
		YellowClusters:   regime.Composite.ClusterYellowCount,
		RankedClusters:   regime.Composite.ClusterRankedCount,
		UnrankedClusters: regime.Composite.ClusterUnrankedCount,
		RedClusterNames:  market.RedClusterNames,
		StressWatch:      stressWatch,
		StressSignal:     stressSignal,
		DataQualityWatch: dataQuality,
		Regime:           &regime,
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

func regimeBacktestInput(obs RegimeBacktestObservation) (rpc.RegimeSnapshotResult, time.Time) {
	asOf := regimeBacktestAsOf(obs)
	regime := obs.Regime
	if regime.AsOf.IsZero() {
		regime.AsOf = asOf
	}
	backfillBacktestRegimeComposite(&regime)
	if regime.Fingerprint.Key == "" {
		regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	}
	return regime, asOf
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

func regimeBacktestAsOf(obs RegimeBacktestObservation) time.Time {
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

func backtestDateLabel(date string, asOf time.Time) string {
	if date != "" {
		return date
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

func cleanBacktestTargetScope(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return "market"
	}
	return scope
}

func regimeBacktestScoredScope(scope string) bool {
	switch cleanBacktestTargetScope(scope) {
	case "market", "broad_market", "cross_asset":
		return true
	default:
		return false
	}
}

func regimeBacktestStressWatch(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	if c.ClusterRankedCount < verdictFloor {
		return false
	}
	return c.ClusterRedCount >= 1 || c.ClusterYellowCount >= 3
}

func regimeBacktestStressSignal(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	return c.ClusterRankedCount >= verdictFloor && c.ClusterRedCount >= 1
}

func regimeBacktestDataQualityWatch(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	if c.ClusterRankedCount < verdictFloor || c.ClusterUnrankedCount > 0 {
		return true
	}
	statuses := []string{
		r.VIXTermStructure.Status,
		r.VolOfVol.Status,
		r.HYGSPYDivergence.Status,
		r.CreditSpreads.Status,
		r.FundingStress.Status,
		r.USDJPY.Status,
		r.GammaZero.Status,
		r.Breadth.Status,
	}
	for _, status := range statuses {
		status = strings.ToLower(strings.TrimSpace(status))
		if status != "" && status != rpc.RegimeStatusOK {
			return true
		}
	}
	return canaryGammaDegraded(r.GammaZero) || len(r.WarningDetails) > 0
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

func canaryBacktestRebalanceAtLeast(res CanaryResult, severity risk.SignalSeverity) bool {
	if !severityRankAtLeast(res.Severity, severity) {
		return false
	}
	return res.Direction == risk.DirectionRebalance
}

func (a *canaryBacktestAccumulator) add(row CanaryBacktestRowResult) {
	a.metrics.Observations++
	if row.TargetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	if row.SignalWatch {
		a.metrics.SignalWatch++
	}
	if row.DefensiveWatch {
		a.metrics.DefensiveWatch++
	}
	if row.DefensiveAct {
		a.metrics.DefensiveAct++
	}
	if row.RebalanceWatch {
		a.metrics.RebalanceWatch++
	}
	if row.DataQualityWatch {
		a.metrics.DataQualityWatch++
	}
	if row.Blocked {
		a.metrics.Blocked++
	}
	a.addSignal(row)
	a.addWatch(row)
	a.addAct(row)
}

func (a *canaryBacktestAccumulator) addSignal(row CanaryBacktestRowResult) {
	switch {
	case row.TargetStress && row.SignalWatch:
		a.metrics.SignalTruePositive++
		if row.DaysToStress != nil {
			a.signalLeadDays += *row.DaysToStress
			a.signalLeadCount++
		}
	case row.TargetStress:
		a.metrics.SignalMiss++
	case row.SignalWatch:
		a.metrics.SignalFalsePositive++
	}
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
	m.SignalPrecision = ratioPtr(m.SignalTruePositive, m.SignalTruePositive+m.SignalFalsePositive)
	m.SignalRecall = ratioPtr(m.SignalTruePositive, m.TargetStress)
	m.SignalFalseAlarmRate = ratioPtr(m.SignalFalsePositive, m.NonStress)
	m.SignalAvgLeadDays = avgPtr(a.signalLeadDays, a.signalLeadCount)
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

func (a *regimeBacktestAccumulator) add(row RegimeBacktestRowResult) {
	a.metrics.Observations++
	if !row.Scored {
		a.metrics.OutOfScope++
		return
	}
	a.metrics.ScoredObservations++
	if row.TargetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	if row.StressWatch {
		a.metrics.StressWatch++
	}
	if row.StressSignal {
		a.metrics.StressSignal++
	}
	if row.DataQualityWatch {
		a.metrics.DataQualityWatch++
	}
	a.addWatch(row)
	a.addStress(row)
}

func (a *regimeBacktestAccumulator) addWatch(row RegimeBacktestRowResult) {
	switch {
	case row.TargetStress && row.StressWatch:
		a.metrics.WatchTruePositive++
		if row.DaysToStress != nil {
			a.watchLeadDays += *row.DaysToStress
			a.watchLeadCount++
		}
	case row.TargetStress:
		a.metrics.WatchMiss++
	case row.StressWatch:
		a.metrics.WatchFalsePositive++
	}
}

func (a *regimeBacktestAccumulator) addStress(row RegimeBacktestRowResult) {
	switch {
	case row.TargetStress && row.StressSignal:
		a.metrics.StressTruePositive++
		if row.DaysToStress != nil {
			a.stressLeadDays += *row.DaysToStress
			a.stressLeadCount++
		}
	case row.TargetStress:
		a.metrics.StressMiss++
	case row.StressSignal:
		a.metrics.StressFalsePositive++
	}
}

func (a *regimeBacktestAccumulator) result() RegimeBacktestMetrics {
	m := a.metrics
	m.WatchPrecision = ratioPtr(m.WatchTruePositive, m.WatchTruePositive+m.WatchFalsePositive)
	m.WatchRecall = ratioPtr(m.WatchTruePositive, m.TargetStress)
	m.WatchFalseAlarmRate = ratioPtr(m.WatchFalsePositive, m.NonStress)
	m.WatchAvgLeadDays = avgPtr(a.watchLeadDays, a.watchLeadCount)
	m.StressPrecision = ratioPtr(m.StressTruePositive, m.StressTruePositive+m.StressFalsePositive)
	m.StressRecall = ratioPtr(m.StressTruePositive, m.TargetStress)
	m.StressFalseAlarmRate = ratioPtr(m.StressFalsePositive, m.NonStress)
	m.StressAvgLeadDays = avgPtr(a.stressLeadDays, a.stressLeadCount)
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
	} else if m.SignalMiss == 0 {
		findings = append(findings, "Watch-level canary signals caught every labelled stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Watch-level canary signals missed %d labelled stress row(s).", m.SignalMiss))
	}
	if m.SignalFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Watch-level canary signals fired on %d non-stress row(s); inspect risk-budget false positives before tightening policy.", m.SignalFalsePositive))
	}
	if m.TargetStress == 0 {
		// Already covered above; keep the defensive-specific finding quiet when
		// there is no stress label base rate.
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
	if m.RebalanceWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d rebalance watch row(s) were tracked separately from defensive alerts.", m.RebalanceWatch))
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

func regimeBacktestFindings(res RegimeBacktestResult) []string {
	m := res.Metrics
	if m.Observations == 0 {
		return []string{"No observations were loaded."}
	}
	var findings []string
	if m.OutOfScope > 0 {
		findings = append(findings, fmt.Sprintf("%d out-of-scope row(s) were excluded from regime precision/recall.", m.OutOfScope))
	}
	if m.TargetStress == 0 {
		findings = append(findings, "No scored market-stress rows were present; add labelled forward windows before reading precision or recall.")
	} else if m.WatchMiss == 0 {
		findings = append(findings, "Regime watch caught every scored market-stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Regime watch missed %d scored market-stress row(s).", m.WatchMiss))
	}
	if m.WatchFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Regime watch fired on %d scored non-stress row(s); inspect yellow-cluster false positives before tuning.", m.WatchFalsePositive))
	}
	if m.TargetStress > 0 && m.StressMiss > 0 {
		findings = append(findings, fmt.Sprintf("Red-cluster stress signals caught %d/%d scored market-stress row(s).", m.StressTruePositive, m.TargetStress))
	}
	if m.StressFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Red-cluster stress signals fired on %d scored non-stress row(s).", m.StressFalsePositive))
	}
	if m.DataQualityWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d scored data-quality watch row(s) were tracked separately from stress false positives.", m.DataQualityWatch))
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
		"Signal",
		formatBacktestRate(r.Metrics.SignalPrecision),
		formatBacktestRate(r.Metrics.SignalRecall),
		formatBacktestRate(r.Metrics.SignalFalseAlarmRate),
		formatBacktestNumber(r.Metrics.SignalAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Defensive",
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
	fmt.Fprintf(out, "  %-12s %d rebalance watch row(s)\n", "Risk budget", r.Metrics.RebalanceWatch)
	fmt.Fprintf(out, "  %-12s %d data-quality watch · %d blocked planner row(s)\n", "Quality", r.Metrics.DataQualityWatch, r.Metrics.Blocked)
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %6s %6s %6s %6s %7s",
			"CLUSTER", "OBS", "STRESS", "DEF TP", "DEF FP", "REBAL", "ACT TP", "BLOCKED")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %6d %6d %6d %6d %7d\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.RebalanceWatch,
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

func renderRegimeBacktestText(env *Env, out io.Writer, r *RegimeBacktestResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Regime Backtest  ·  %d observations  ·  policy %s\n", r.Metrics.Observations, r.Policy)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-12s %d stress / %d non-stress / %d out-of-scope\n", "Targets", r.Metrics.TargetStress, r.Metrics.NonStress, r.Metrics.OutOfScope)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Watch",
		formatBacktestRate(r.Metrics.WatchPrecision),
		formatBacktestRate(r.Metrics.WatchRecall),
		formatBacktestRate(r.Metrics.WatchFalseAlarmRate),
		formatBacktestNumber(r.Metrics.WatchAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Stress",
		formatBacktestRate(r.Metrics.StressPrecision),
		formatBacktestRate(r.Metrics.StressRecall),
		formatBacktestRate(r.Metrics.StressFalseAlarmRate),
		formatBacktestNumber(r.Metrics.StressAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s %d data-quality watch row(s)\n", "Quality", r.Metrics.DataQualityWatch)
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %6s %6s %6s %6s %3s %3s",
			"CLUSTER", "OBS", "SCORED", "STRESS", "WAT TP", "WAT FP", "STR TP", "DQ", "OOS")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %6d %6d %6d %6d %3d %3d\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.ScoredObservations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.StressTruePositive,
				m.DataQualityWatch,
				m.OutOfScope,
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
