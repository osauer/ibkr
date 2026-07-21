package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

const opportunityOutcomeFormulaCloseToClose = "close_to_close_excess_v1"

const opportunityScoreTargetPolicyNetExcessPositive = "net-excess-positive"

type opportunityScoreOptions struct {
	TargetPolicy string
}

// OpportunityPriceBarRow is one dated market-data bar used to score forward
// opportunity outcomes and mark-to-market simulations.
type OpportunityPriceBarRow struct {
	Symbol        string  `json:"symbol"`
	Date          string  `json:"date"`
	Open          float64 `json:"open,omitempty"`
	High          float64 `json:"high,omitempty"`
	Low           float64 `json:"low,omitempty"`
	Close         float64 `json:"close"`
	AdjustedClose float64 `json:"adjusted_close,omitempty"`
	Volume        int64   `json:"volume,omitempty"`
	Source        string  `json:"source,omitempty"`
}

type opportunityPriceBarLedger struct {
	BySymbol         map[string][]OpportunityPriceBarRow
	Source           string
	Checksum         string
	SourceQuality    string
	SourceWarnings   []string
	BarSources       []string
	ManifestPath     string
	ManifestChecksum string
	SourceProvider   string
	SourceMethod     string
	SourceCreatedAt  string
	SourceCommand    string
}

func readOpportunityPriceBarLedgerFromFile(barsPath string) (opportunityPriceBarLedger, error) {
	return readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, "")
}

func readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath string) (opportunityPriceBarLedger, error) {
	checksum, err := sha256FileHex(barsPath)
	if err != nil {
		return opportunityPriceBarLedger{}, err
	}
	f, err := os.Open(barsPath)
	if err != nil {
		return opportunityPriceBarLedger{}, err
	}
	defer f.Close()
	bars, err := readOpportunityPriceBars(f)
	if err != nil {
		return opportunityPriceBarLedger{}, err
	}
	sourceQuality, sourceWarnings, sources := opportunityPriceBarLedgerSourceQuality(bars)
	ledger := opportunityPriceBarLedger{
		BySymbol:       bars,
		Source:         barsPath,
		Checksum:       "sha256:" + checksum,
		SourceQuality:  sourceQuality,
		SourceWarnings: sourceWarnings,
		BarSources:     sources,
	}
	if strings.TrimSpace(manifestPath) != "" {
		manifest, manifestChecksum, err := readOpportunityPriceBarManifestFromFile(manifestPath)
		if err != nil {
			return opportunityPriceBarLedger{}, err
		}
		if err := validateOpportunityPriceBarManifest(manifest, ledger); err != nil {
			return opportunityPriceBarLedger{}, fmt.Errorf("bars manifest %s: %w", manifestPath, err)
		}
		ledger.ManifestPath = manifestPath
		ledger.ManifestChecksum = manifestChecksum
		ledger.SourceProvider = strings.TrimSpace(manifest.Provider)
		ledger.SourceMethod = strings.TrimSpace(manifest.Method)
		ledger.SourceCreatedAt = manifest.CreatedAt.Format(time.RFC3339)
		ledger.SourceCommand = strings.TrimSpace(manifest.Command)
	}
	if ledger.SourceQuality == "ok" && strings.TrimSpace(ledger.ManifestChecksum) == "" {
		ledger.SourceQuality = "unattested_source"
		ledger.SourceWarnings = append(ledger.SourceWarnings, fmt.Sprintf("trusted bar source requires --bars-manifest with schema %s before alpha claims", opportunityPriceBarManifestSchemaV1))
	}
	return ledger, nil
}

const opportunityPriceBarManifestSchemaV1 = "opportunity_price_bars_manifest_v1"

type opportunityPriceBarManifest struct {
	SchemaVersion    string                              `json:"schema_version"`
	SourceID         string                              `json:"source_id"`
	ExporterID       string                              `json:"exporter_id"`
	ExporterVersion  string                              `json:"exporter_version"`
	Provider         string                              `json:"provider"`
	Method           string                              `json:"method"`
	WhatToShow       string                              `json:"what_to_show,omitempty"`
	PriceBasis       string                              `json:"price_basis"`
	BarSize          string                              `json:"bar_size"`
	AdjustmentPolicy string                              `json:"adjustment_policy"`
	CreatedAt        time.Time                           `json:"created_at"`
	Command          string                              `json:"command"`
	BarFile          string                              `json:"bar_file"`
	BarsSHA256       string                              `json:"bars_sha256"`
	RowCount         int                                 `json:"row_count"`
	Symbols          []opportunityPriceBarManifestSymbol `json:"symbols"`
}

type opportunityPriceBarManifestSymbol struct {
	Symbol    string `json:"symbol"`
	FirstDate string `json:"first_date"`
	LastDate  string `json:"last_date"`
	Bars      int    `json:"bars"`
}

func readOpportunityPriceBarManifestFromFile(path string) (opportunityPriceBarManifest, string, error) {
	checksum, err := sha256FileHex(path)
	if err != nil {
		return opportunityPriceBarManifest{}, "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return opportunityPriceBarManifest{}, "", err
	}
	defer f.Close()
	var manifest opportunityPriceBarManifest
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return opportunityPriceBarManifest{}, "", err
	}
	return manifest, "sha256:" + checksum, nil
}

func validateOpportunityPriceBarManifest(manifest opportunityPriceBarManifest, ledger opportunityPriceBarLedger) error {
	if strings.TrimSpace(manifest.SchemaVersion) != opportunityPriceBarManifestSchemaV1 {
		return fmt.Errorf("schema_version %q does not match %s", manifest.SchemaVersion, opportunityPriceBarManifestSchemaV1)
	}
	source := strings.ToLower(strings.TrimSpace(manifest.SourceID))
	if opportunityPriceBarSourceClass(source) != "trusted" {
		return fmt.Errorf("source_id %q is not a trusted repo-owned exporter ID", manifest.SourceID)
	}
	if strings.TrimSpace(manifest.ExporterID) == "" {
		return fmt.Errorf("exporter_id is required")
	}
	if strings.ToLower(strings.TrimSpace(manifest.ExporterID)) != source {
		return fmt.Errorf("exporter_id %q does not match source_id %q", manifest.ExporterID, manifest.SourceID)
	}
	if strings.TrimSpace(manifest.ExporterVersion) == "" {
		return fmt.Errorf("exporter_version is required")
	}
	if strings.TrimSpace(manifest.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(manifest.Method) == "" {
		return fmt.Errorf("method is required")
	}
	if strings.TrimSpace(manifest.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if strings.TrimSpace(manifest.BarSize) != "1 day" {
		return fmt.Errorf("bar_size %q does not match 1 day", manifest.BarSize)
	}
	if strings.TrimSpace(manifest.AdjustmentPolicy) == "" {
		return fmt.Errorf("adjustment_policy is required")
	}
	if source == opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV && strings.TrimSpace(manifest.WhatToShow) != "ADJUSTED_LAST" {
		return fmt.Errorf("what_to_show %q does not match ADJUSTED_LAST for %s", manifest.WhatToShow, opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV)
	}
	if strings.TrimSpace(manifest.BarFile) == "" {
		return fmt.Errorf("bar_file is required")
	}
	if manifest.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	if strings.TrimSpace(manifest.PriceBasis) != "adjusted_close" {
		return fmt.Errorf("price_basis %q does not match adjusted_close", manifest.PriceBasis)
	}
	if strings.TrimSpace(manifest.BarsSHA256) != strings.TrimSpace(ledger.Checksum) {
		return fmt.Errorf("bars_sha256 %q does not match bars ledger %q", manifest.BarsSHA256, ledger.Checksum)
	}
	if got, want := opportunityPriceBarRowCount(ledger.BySymbol), manifest.RowCount; want != got {
		return fmt.Errorf("row_count %d does not match bars ledger %d", want, got)
	}
	for symbol, rows := range ledger.BySymbol {
		for _, row := range rows {
			if strings.ToLower(strings.TrimSpace(row.Source)) != source {
				return fmt.Errorf("bar source for %s %s is %q, expected manifest source_id %q", symbol, row.Date, row.Source, manifest.SourceID)
			}
			if row.AdjustedClose <= 0 {
				return fmt.Errorf("adjusted_close is required for alpha-grade source %s %s", symbol, row.Date)
			}
		}
	}
	expected := opportunityPriceBarManifestSymbolMap(opportunityPriceBarManifestSymbolsFromBars(ledger.BySymbol))
	got := opportunityPriceBarManifestSymbolMap(manifest.Symbols)
	if len(got) != len(expected) {
		return fmt.Errorf("symbols cover %d symbol(s), expected %d", len(got), len(expected))
	}
	for symbol, want := range expected {
		have, ok := got[symbol]
		if !ok {
			return fmt.Errorf("symbols missing %s", symbol)
		}
		if have.FirstDate != want.FirstDate || have.LastDate != want.LastDate || have.Bars != want.Bars {
			return fmt.Errorf("symbols[%s] = %s..%s %d bars, expected %s..%s %d bars", symbol, have.FirstDate, have.LastDate, have.Bars, want.FirstDate, want.LastDate, want.Bars)
		}
	}
	return nil
}

func opportunityPriceBarRowCount(bars map[string][]OpportunityPriceBarRow) int {
	count := 0
	for _, rows := range bars {
		count += len(rows)
	}
	return count
}

func opportunityPriceBarManifestSymbolsFromBars(bars map[string][]OpportunityPriceBarRow) []opportunityPriceBarManifestSymbol {
	symbols := make([]string, 0, len(bars))
	for symbol := range bars {
		symbols = append(symbols, symbol)
	}
	slices.Sort(symbols)
	out := make([]opportunityPriceBarManifestSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		rows := bars[symbol]
		if len(rows) == 0 {
			continue
		}
		out = append(out, opportunityPriceBarManifestSymbol{
			Symbol:    symbol,
			FirstDate: rows[0].Date,
			LastDate:  rows[len(rows)-1].Date,
			Bars:      len(rows),
		})
	}
	return out
}

func opportunityPriceBarManifestSymbolMap(symbols []opportunityPriceBarManifestSymbol) map[string]opportunityPriceBarManifestSymbol {
	out := make(map[string]opportunityPriceBarManifestSymbol, len(symbols))
	for _, symbol := range symbols {
		clean := strings.ToUpper(strings.TrimSpace(symbol.Symbol))
		if clean == "" {
			continue
		}
		symbol.Symbol = clean
		out[clean] = symbol
	}
	return out
}

func readOpportunityPriceBars(r io.Reader) (map[string][]OpportunityPriceBarRow, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := map[string][]OpportunityPriceBarRow{}
	seen := map[string]int{}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row OpportunityPriceBarRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if symbol == "" {
			return nil, fmt.Errorf("line %d: symbol is required", lineNo)
		}
		if _, err := parseOpportunityDate(row.Date); err != nil {
			return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
		}
		if opportunityBarClose(row) <= 0 {
			return nil, fmt.Errorf("line %d: close or adjusted_close must be positive", lineNo)
		}
		row.Symbol = symbol
		key := symbol + "\x00" + row.Date
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf("line %d: duplicate price bar for %s on %s; first seen on line %d", lineNo, symbol, row.Date, prev)
		}
		seen[key] = lineNo
		out[symbol] = append(out[symbol], row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for symbol := range out {
		sort.Slice(out[symbol], func(i, j int) bool {
			return out[symbol][i].Date < out[symbol][j].Date
		})
	}
	return out, nil
}

func scoreOpportunityPointInTimeRows(rows []OpportunityPointInTimeRow, ledger opportunityPriceBarLedger) ([]OpportunityPointInTimeRow, error) {
	return scoreOpportunityPointInTimeRowsWithOptions(rows, ledger, opportunityScoreOptions{})
}

func scoreOpportunityPointInTimeRowsWithOptions(rows []OpportunityPointInTimeRow, ledger opportunityPriceBarLedger, opts opportunityScoreOptions) ([]OpportunityPointInTimeRow, error) {
	if len(ledger.BySymbol) == 0 {
		return nil, fmt.Errorf("bars ledger is empty")
	}
	if strings.TrimSpace(ledger.Checksum) == "" {
		return nil, fmt.Errorf("bars ledger checksum is required")
	}
	policy := cleanOpportunityScoreTargetPolicy(opts.TargetPolicy)
	if policy == "unknown" {
		return nil, fmt.Errorf("unsupported target policy %q; supported values: %s", strings.TrimSpace(opts.TargetPolicy), opportunityScoreTargetPolicyNetExcessPositive)
	}
	out := make([]OpportunityPointInTimeRow, 0, len(rows))
	for i, row := range rows {
		scored, err := scoreOpportunityPointInTimeRowWithOptions(row, ledger, opportunityScoreOptions{TargetPolicy: policy})
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, scored)
	}
	return out, nil
}

func scoreOpportunityPointInTimeRowWithOptions(row OpportunityPointInTimeRow, ledger opportunityPriceBarLedger, opts opportunityScoreOptions) (OpportunityPointInTimeRow, error) {
	policy := cleanOpportunityScoreTargetPolicy(opts.TargetPolicy)
	if policy == "unknown" {
		return row, fmt.Errorf("unsupported target policy %q; supported values: %s", strings.TrimSpace(opts.TargetPolicy), opportunityScoreTargetPolicyNetExcessPositive)
	}
	if policy == "" {
		if strings.TrimSpace(row.Target.Kind) == "" {
			return row, fmt.Errorf("target.kind is required before score-opportunity")
		}
		if strings.TrimSpace(row.Target.Source) == "" || strings.TrimSpace(row.Target.Method) == "" {
			return row, fmt.Errorf("target.source and target.method are required before score-opportunity")
		}
	}
	if err := validateOpportunityPointInTimeRowScorable(row); err != nil {
		return row, err
	}
	instrument := strings.ToUpper(strings.TrimSpace(row.Trade.Instrument))
	if instrument == "" {
		instrument = strings.ToUpper(strings.TrimSpace(row.Features.Instrument))
	}
	if instrument == "" {
		return row, fmt.Errorf("trade.instrument or features.instrument is required")
	}
	benchmark := strings.ToUpper(strings.TrimSpace(row.Trade.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
		row.Trade.Benchmark = benchmark
	}
	entry, exit, err := opportunityScoreWindow(row, ledger.BySymbol[instrument])
	if err != nil {
		return row, err
	}
	benchEntry, benchExit, err := opportunityScoreBenchmarkWindow(entry.Date, exit.Date, ledger.BySymbol[benchmark])
	if err != nil {
		return row, fmt.Errorf("benchmark %s: %w", benchmark, err)
	}
	entryClose := opportunityBarClose(entry)
	exitClose := opportunityBarClose(exit)
	benchEntryClose := opportunityBarClose(benchEntry)
	benchExitClose := opportunityBarClose(benchExit)
	forward := opportunityPctReturn(entryClose, exitClose)
	bench := opportunityPctReturn(benchEntryClose, benchExitClose)
	excess := forward - bench
	adverse, favorable := opportunityExcursions(ledger.BySymbol[instrument], entry.Date, exit.Date, entryClose)
	source := opportunityBarSource(ledger.Source, instrument)
	benchSource := opportunityBarSource(ledger.Source, benchmark)
	row.LabelStatus = "scored"
	row.Outcome.EntryDate = entry.Date
	row.Outcome.ExitDate = exit.Date
	row.Outcome.EntryPrice = &entryClose
	row.Outcome.ExitPrice = &exitClose
	row.Outcome.PriceSource = source
	row.Outcome.BenchmarkSource = benchSource
	row.Outcome.Formula = opportunityOutcomeFormulaCloseToClose
	row.Outcome.PriceBasis = "adjusted_close"
	row.Outcome.SourceChecksum = ledger.Checksum
	row.Outcome.BenchmarkSourceChecksum = ledger.Checksum
	row.Outcome.ForwardReturnPct = roundOpportunityPct(forward)
	row.Outcome.BenchmarkReturnPct = roundOpportunityPct(bench)
	row.Outcome.ExcessReturnPct = roundOpportunityPct(excess)
	row.Outcome.MaxAdverseExcursionPct = roundOpportunityPct(adverse)
	row.Outcome.MaxFavorableExcursionPct = roundOpportunityPct(favorable)
	if policy != "" {
		if err := applyOpportunityScoreTargetPolicy(&row, policy, ledger); err != nil {
			return row, err
		}
	}
	return row, nil
}

func cleanOpportunityScoreTargetPolicy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
	case opportunityScoreTargetPolicyNetExcessPositive, "net_excess_positive":
		return opportunityScoreTargetPolicyNetExcessPositive
	default:
		return "unknown"
	}
}

func applyOpportunityScoreTargetPolicy(row *OpportunityPointInTimeRow, policy string, ledger opportunityPriceBarLedger) error {
	switch policy {
	case opportunityScoreTargetPolicyNetExcessPositive:
		costPct := 0.0
		if row.Trade.RoundTripCostBps != nil {
			if *row.Trade.RoundTripCostBps < 0 {
				return fmt.Errorf("trade.round_trip_cost_bps must be non-negative")
			}
			costPct = *row.Trade.RoundTripCostBps / 100
		}
		netExcess := row.Outcome.ExcessReturnPct - costPct
		row.Target = OpportunityBacktestTarget{
			Opportunity: netExcess > 0,
			Kind:        "net_excess_positive",
			Scope:       "instrument_vs_benchmark",
			Source:      opportunityNonEmptyString(ledger.ManifestChecksum, ledger.Checksum),
			Method:      "net_excess_after_round_trip_cost_v1",
			Notes:       fmt.Sprintf("mechanical label from scored adjusted bars: excess_return_pct %.1f minus round_trip_cost_pct %.1f", row.Outcome.ExcessReturnPct, costPct),
		}
		return nil
	default:
		return fmt.Errorf("unsupported target policy %q", policy)
	}
}

func validateOpportunityPointInTimeRowScorable(row OpportunityPointInTimeRow) error {
	if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, row.Features); err != nil {
		return err
	}
	status := strings.TrimSpace(row.LabelStatus)
	switch status {
	case "scored":
		return nil
	case "unscored_forward_window_pending":
		if !opportunityOutcomeEmpty(row.Outcome) {
			return fmt.Errorf("outcome fields must be empty while label_status is unscored_forward_window_pending")
		}
		blockers := opportunityCaptureLiveContextBlockers(row.Features)
		if len(blockers) > 0 {
			instrument := cleanOpportunityBacktestInstrument(opportunityNonEmptyString(row.Trade.Instrument, row.Features.Instrument))
			return fmt.Errorf("capture row failed --require-live for %s: %s", instrument, strings.Join(blockers, ","))
		}
		return nil
	default:
		return fmt.Errorf("label_status must be unscored_forward_window_pending or scored before score-opportunity")
	}
}

func opportunityOutcomeEmpty(outcome OpportunityBacktestOutcome) bool {
	return strings.TrimSpace(outcome.EntryDate) == "" &&
		strings.TrimSpace(outcome.ExitDate) == "" &&
		outcome.EntryPrice == nil &&
		outcome.ExitPrice == nil &&
		strings.TrimSpace(outcome.PriceSource) == "" &&
		strings.TrimSpace(outcome.BenchmarkSource) == "" &&
		strings.TrimSpace(outcome.Formula) == "" &&
		strings.TrimSpace(outcome.PriceBasis) == "" &&
		strings.TrimSpace(outcome.SourceChecksum) == "" &&
		strings.TrimSpace(outcome.BenchmarkSourceChecksum) == "" &&
		outcome.ForwardReturnPct == 0 &&
		outcome.BenchmarkReturnPct == 0 &&
		outcome.ExcessReturnPct == 0 &&
		outcome.MaxAdverseExcursionPct == 0 &&
		outcome.MaxFavorableExcursionPct == 0
}

func opportunityScoreWindow(row OpportunityPointInTimeRow, bars []OpportunityPriceBarRow) (OpportunityPriceBarRow, OpportunityPriceBarRow, error) {
	if len(bars) == 0 {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no price bars for %s", strings.TrimSpace(row.Trade.Instrument))
	}
	entryRule := cleanOpportunityEntryRule(row.Trade.EntryRule)
	if err := validateOpportunityEntryRuleSupported(row.Trade.EntryRule); err != nil {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, err
	}
	entryDate := strings.TrimSpace(row.Outcome.EntryDate)
	var entry OpportunityPriceBarRow
	var ok bool
	if entryRule == "next_close" {
		decision, err := opportunityDecisionDateWithSession(row.Date, row.AsOf, row.Features.SessionContext)
		if err != nil {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, err
		}
		entryDate = decision.Format("2006-01-02")
		entry, ok = firstOpportunityBarAfter(bars, entryDate)
		if !ok {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no next-close entry bar after %s", entryDate)
		}
		exitDate, err := opportunityScoreExitDate(entry.Date, row.Trade.HorizonDays)
		if err != nil {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, err
		}
		exit, ok := firstOpportunityBarOnOrAfter(bars, exitDate)
		if !ok {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no exit bar on or after %s", exitDate)
		}
		return entry, exit, nil
	} else {
		if entryDate == "" {
			entryDate = strings.TrimSpace(row.Date)
		}
		if entryDate == "" && !row.AsOf.IsZero() {
			entryDate = row.AsOf.Format("2006-01-02")
		}
		if entryDate == "" {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("date, as_of, or outcome.entry_date is required")
		}
		entry, ok = firstOpportunityBarOnOrAfter(bars, entryDate)
		if !ok {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no entry bar on or after %s", entryDate)
		}
	}
	exitDate := strings.TrimSpace(row.Outcome.ExitDate)
	if exitDate == "" {
		horizon := row.Trade.HorizonDays
		if horizon <= 0 {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("outcome.exit_date or trade.horizon_days is required")
		}
		entryTime, err := parseOpportunityDate(entry.Date)
		if err != nil {
			return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, err
		}
		exitDate = entryTime.AddDate(0, 0, horizon).Format("2006-01-02")
	}
	exit, ok := firstOpportunityBarOnOrAfter(bars, exitDate)
	if !ok {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no exit bar on or after %s", exitDate)
	}
	return entry, exit, nil
}

func opportunityScoreExitDate(entryDate string, horizonDays int) (string, error) {
	if horizonDays <= 0 {
		return "", fmt.Errorf("trade.horizon_days must be positive")
	}
	entryTime, err := parseOpportunityDate(entryDate)
	if err != nil {
		return "", err
	}
	return entryTime.AddDate(0, 0, horizonDays).Format("2006-01-02"), nil
}

func cleanOpportunityEntryRule(rule string) string {
	rule = strings.ToLower(strings.TrimSpace(rule))
	switch rule {
	case "", "next_close", "next-close":
		return "next_close"
	default:
		return rule
	}
}

func validateOpportunityEntryRuleSupported(rule string) error {
	clean := cleanOpportunityEntryRule(rule)
	switch clean {
	case "next_close":
		return nil
	default:
		return fmt.Errorf("unsupported trade.entry_rule %q; supported values: next_close", strings.TrimSpace(rule))
	}
}

func opportunityScoreBenchmarkWindow(entryDate, exitDate string, bars []OpportunityPriceBarRow) (OpportunityPriceBarRow, OpportunityPriceBarRow, error) {
	if len(bars) == 0 {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no benchmark bars")
	}
	entry, ok := firstOpportunityBarOnOrAfter(bars, entryDate)
	if !ok {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no entry bar on or after %s", entryDate)
	}
	exit, ok := firstOpportunityBarOnOrAfter(bars, exitDate)
	if !ok {
		return OpportunityPriceBarRow{}, OpportunityPriceBarRow{}, fmt.Errorf("no exit bar on or after %s", exitDate)
	}
	return entry, exit, nil
}

func firstOpportunityBarOnOrAfter(bars []OpportunityPriceBarRow, date string) (OpportunityPriceBarRow, bool) {
	for _, bar := range bars {
		if bar.Date >= date {
			return bar, true
		}
	}
	return OpportunityPriceBarRow{}, false
}

func firstOpportunityBarAfter(bars []OpportunityPriceBarRow, date string) (OpportunityPriceBarRow, bool) {
	for _, bar := range bars {
		if bar.Date > date {
			return bar, true
		}
	}
	return OpportunityPriceBarRow{}, false
}

func opportunityExcursions(bars []OpportunityPriceBarRow, entryDate, exitDate string, entryClose float64) (float64, float64) {
	adverse := math.Inf(1)
	favorable := math.Inf(-1)
	for _, bar := range bars {
		if bar.Date < entryDate || bar.Date > exitDate {
			continue
		}
		ret := opportunityPctReturn(entryClose, opportunityBarClose(bar))
		if ret < adverse {
			adverse = ret
		}
		if ret > favorable {
			favorable = ret
		}
	}
	if math.IsInf(adverse, 1) {
		adverse = 0
	}
	if math.IsInf(favorable, -1) {
		favorable = 0
	}
	return adverse, favorable
}

func opportunityPctReturn(start, end float64) float64 {
	if start == 0 {
		return math.NaN()
	}
	return (end - start) / start * 100
}

func opportunityBarClose(row OpportunityPriceBarRow) float64 {
	if row.AdjustedClose > 0 {
		return row.AdjustedClose
	}
	return row.Close
}

func opportunityBarSource(source, symbol string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "bars"
	}
	return source + "#" + strings.ToUpper(strings.TrimSpace(symbol))
}

func opportunityPriceBarLedgerSourceQuality(bars map[string][]OpportunityPriceBarRow) (string, []string, []string) {
	total := 0
	missing := 0
	fixture := 0
	untrusted := 0
	sourcesSet := map[string]struct{}{}
	for _, rows := range bars {
		for _, row := range rows {
			total++
			source := strings.TrimSpace(row.Source)
			if source != "" {
				sourcesSet[source] = struct{}{}
			}
			switch opportunityPriceBarSourceClass(source) {
			case "missing":
				missing++
			case "fixture":
				fixture++
			case "untrusted":
				untrusted++
			}
		}
	}
	sources := make([]string, 0, len(sourcesSet))
	for source := range sourcesSet {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	var warnings []string
	switch {
	case total == 0:
		return "missing_source", []string{"bars ledger is empty"}, sources
	case missing > 0:
		warnings = append(warnings, fmt.Sprintf("%d/%d price bar(s) are missing source", missing, total))
		return "missing_source", warnings, sources
	case fixture > 0:
		warnings = append(warnings, fmt.Sprintf("%d/%d price bar(s) use fixture/test source", fixture, total))
		return "fixture_source", warnings, sources
	case untrusted > 0:
		warnings = append(warnings, fmt.Sprintf("%d/%d price bar(s) use an unrecognized source; use a repo-owned exporter ID such as %s before alpha claims", untrusted, total, opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV))
		return "untrusted_source", warnings, sources
	default:
		return "ok", nil, sources
	}
}

const opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV = "ibkr_hmds_adjusted_ohlcv_v1"

func opportunityPriceBarSourceClass(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return "missing"
	}
	for _, token := range []string{"fixture", "test", "manual", "synthetic"} {
		if strings.Contains(source, token) {
			return "fixture"
		}
	}
	switch source {
	case opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		"interactive_brokers_hmds_adjusted_ohlcv_v1",
		"polygon_adjusted_ohlcv_v1",
		"tiingo_adjusted_ohlcv_v1",
		"nasdaq_data_link_adjusted_ohlcv_v1",
		"stooq_adjusted_ohlcv_v1":
		return "trusted"
	}
	return "untrusted"
}

func roundOpportunityPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*10) / 10
}

func parseOpportunityDate(date string) (time.Time, error) {
	return time.Parse("2006-01-02", strings.TrimSpace(date))
}

func sha256FileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
