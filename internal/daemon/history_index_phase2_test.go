package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// testCanaryResult is a fully-populated canary snapshot for round-trip
// drift guards.
func testCanaryResult(key string) *rpc.CanaryResult {
	relevant := true
	return &rpc.CanaryResult{
		AsOf:                   time.Now(),
		Fingerprint:            rpc.Fingerprint{Version: "v1", Key: key},
		Action:                 "watch",
		Severity:               risk.SeverityWatch,
		Direction:              risk.DirectionDefensive,
		MarketConfirmation:     "partial",
		PortfolioFit:           "high",
		PortfolioAlertRelevant: &relevant,
		InputHealth:            "ok",
		Summary:                "round-trip canary summary",
		Market: rpc.CanaryMarketSummary{
			RegimePosture: rpc.RegimePosture{Stage: "early_warning", Tone: "watch"},
			RedClusters:   1,
		},
	}
}

// TestHistoryIndexCanaryRoundTrip is the canary writer→parser drift
// guard: a decision journaled by the REAL canaryDecisionJournal must come
// back from canary.history with the same fields.
func TestHistoryIndexCanaryRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	now := time.Now()
	s.journalCanaryDecision(testCanaryResult("sha256:canary-roundtrip"))

	var got rpc.CanaryHistoryResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleCanaryHistory(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return out.Count == 1, nil
	})
	e := got.Entries[0]
	if e.Fingerprint != "sha256:canary-roundtrip" || e.Action != "watch" || e.Severity != "watch" || e.Direction != "defensive" {
		t.Fatalf("decision fields did not round-trip: %+v", e)
	}
	if e.MarketStage != "early_warning" || e.InputHealth != "ok" || e.Summary != "round-trip canary summary" {
		t.Fatalf("evidence fields did not round-trip: %+v", e)
	}
	if e.PortfolioAlertRelevant == nil || !*e.PortfolioAlertRelevant {
		t.Fatalf("portfolio_alert_relevant did not round-trip: %+v", e.PortfolioAlertRelevant)
	}
	if e.SessionKey != nyTradingSessionKey(nyTime(now)) {
		t.Fatalf("session key = %q, want writer's", e.SessionKey)
	}
	if got.Index.IngestedBytes == 0 || got.Index.JournalBytes != got.Index.IngestedBytes {
		t.Fatalf("index health = %+v, want fully ingested", got.Index)
	}
}

// TestCanaryJournalDedupeHeartbeatAndGate pins the journal's dedupe,
// heartbeat, and runtime-disable semantics.
func TestCanaryJournalDedupeHeartbeatAndGate(t *testing.T) {
	s := newHistoryIndexServer(t)
	j := s.canaryDecisions
	res := testCanaryResult("sha256:dedupe")

	base := time.Now()
	if err := j.append(base, "", "", res); err != nil {
		t.Fatal(err)
	}
	if err := j.append(base.Add(time.Minute), "", "", res); err != nil {
		t.Fatal(err) // same fingerprint inside the heartbeat: deduped
	}
	if err := j.append(base.Add(canaryDecisionHeartbeat+time.Second), "", "", res); err != nil {
		t.Fatal(err) // heartbeat: journaled again
	}
	if err := j.append(base.Add(canaryDecisionHeartbeat+2*time.Second), "", "", testCanaryResult("sha256:changed")); err != nil {
		t.Fatal(err) // fingerprint change: journaled
	}
	data, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("journal lines = %d, want 3 (initial, heartbeat, change)", len(lines))
	}
	for _, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("journal line is not standalone JSON: %v", err)
		}
	}

	// Runtime disable: journalCanaryDecision must not append. Mutate the
	// installed store through its own lock (the maintenance loop reads it).
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		disabled := false
		next.Canary.Journal.Enabled = &disabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.journalCanaryDecision(testCanaryResult("sha256:while-disabled"))
	after, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(data) {
		t.Fatal("disabled canary journal still appended")
	}
}

// TestCanaryJournalLoopSkipsWhenDisconnected pins the cadence-loop gate:
// no gateway connector → no broker round-trips, no journal write.
func TestCanaryJournalLoopSkipsWhenDisconnected(t *testing.T) {
	s := newHistoryIndexServer(t)
	s.canaryJournalTick(context.Background())
	if _, err := os.Stat(s.canaryDecisions.path); !os.IsNotExist(err) {
		t.Fatalf("disconnected tick touched the journal (stat err %v)", err)
	}
}

// TestComposeBriefJournalsCanaryDecision proves the brief hook: rendering
// a brief journals the canary it computed.
func TestComposeBriefJournalsCanaryDecision(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	s.installCanaryDecisionJournal()
	if s.canaryDecisions == nil {
		t.Fatal("canary journal not installed")
	}
	_, _ = s.composeBrief(context.Background())
	data, err := os.ReadFile(s.canaryDecisions.path)
	if err != nil {
		t.Fatalf("brief did not journal a canary decision: %v", err)
	}
	var line canaryDecisionLine
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &line); err != nil {
		t.Fatalf("journaled line does not decode: %v", err)
	}
	if line.V != 1 || line.Summary == "" {
		t.Fatalf("journaled line incomplete: %+v", line)
	}
}

// TestHistoryIndexCapitalAndRiskPolicyRoundTrip drives the REAL capital
// and governance journal writers and proves the index parsers track them.
func TestHistoryIndexCapitalAndRiskPolicyRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	ev, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "deposit", AmountBase: 1234.5, Note: "round-trip"}, "human-tty")
	if err != nil {
		t.Fatal(err)
	}
	s.journalRiskPolicyTransition("absent", "active", nil)

	var got rpc.ReconEquityResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleReconEquity(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return len(out.Events) == 1, nil
	})
	e := got.Events[0]
	if e.Type != "deposit" || e.AmountBase != 1234.5 || e.Note != "round-trip" || e.Origin != "human-tty" {
		t.Fatalf("capital event did not round-trip: %+v", e)
	}
	if e.At.UnixMilli() != ev.At.UnixMilli() {
		t.Fatalf("at = %v, want %v", e.At, ev.At)
	}
	if len(got.Days) != 0 || got.Count != 0 {
		t.Fatalf("no statements retained, but days = %+v", got.Days)
	}

	// risk_policy_events row via direct read (no RPC surface for it).
	db, err := sql.Open("sqlite", "file:"+s.historyIndexOpts.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	waitForHistory(t, func() (bool, error) {
		var kind string
		err := db.QueryRow(`SELECT kind FROM risk_policy_events WHERE kind = 'policy_status'`).Scan(&kind)
		if err != nil {
			return false, nil //nolint:nilerr // row may not be ingested yet
		}
		return kind == "policy_status", nil
	})
}

// TestHistoryIndexProposalOutcomeRoundTrip drives the REAL AppendMark
// writer.
func TestHistoryIndexProposalOutcomeRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	mark := proposalOutcomeMark{
		State:             proposalOutcomeStateMarked,
		ProposalKey:       "pk-roundtrip",
		Symbol:            "TSYM",
		Quantity:          4,
		BaselinePrice:     10.5,
		MarkPrice:         9.75,
		PolicyFingerprint: rpc.Fingerprint{Version: "v1", Key: "sha256:prot-roundtrip"},
		BenchmarkSymbol:   "SPY",
	}
	if err := s.proposalOutcomes.AppendMark(mark); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+s.historyIndexOpts.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	waitForHistory(t, func() (bool, error) {
		var key, fingerprint string
		var markPrice float64
		err := db.QueryRow(`SELECT proposal_key, policy_fingerprint, mark_price FROM proposal_outcomes WHERE proposal_key = 'pk-roundtrip'`).
			Scan(&key, &fingerprint, &markPrice)
		if err != nil {
			return false, nil //nolint:nilerr // not ingested yet
		}
		if fingerprint != "sha256:prot-roundtrip" || markPrice != 9.75 {
			t.Fatalf("outcome fields did not round-trip: %q %v", fingerprint, markPrice)
		}
		return true, nil
	})
}

// TestHistoryIndexOrderJournalRoundTrip drives the REAL order journal
// writer (Append → onAppend kick) into order_events.
func TestHistoryIndexOrderJournalRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventPreviewed,
		OrderRef: "ord-rt", PreviewTokenID: "tok-rt", ReservedOrderID: 42,
		Account: "UTEST", Mode: "paper", SendState: orderSendStateReserved,
	}); err != nil {
		t.Fatal(err)
	}
	waitForHistory(t, func() (bool, error) {
		return s.historyIndex.Load().OrdersFresh(), nil
	})
	events, ok := s.indexedOrderEvents("test", nil, nil)
	if !ok || len(events) != 1 {
		t.Fatalf("indexed events = %d ok=%v, want 1/true", len(events), ok)
	}
	e := events[0]
	if e.OrderRef != "ord-rt" || e.PreviewTokenID != "tok-rt" || e.ReservedOrderID != 42 || e.Account != "UTEST" || e.Mode != "paper" {
		t.Fatalf("order event did not round-trip: %+v", e)
	}
}

// TestHistoryRotationSettingsAndMaintenanceGate covers the D11 settings:
// validation, null clears, snapshot round-trip, and the maintenance pass
// honoring the runtime disable.
func TestHistoryRotationSettingsAndMaintenanceGate(t *testing.T) {
	next := &platformSettingsData{Version: 1}
	if err := applySettingsKey(next, "history.rotation.keep_raw_months", json.RawMessage(`0`)); err == nil {
		t.Fatal("keep_raw_months 0 accepted; must be >= 1")
	}
	if err := applySettingsKey(next, "history.rotation.keep_raw_months", json.RawMessage(`3`)); err != nil {
		t.Fatal(err)
	}
	if next.History.Rotation.KeepRawMonths == nil || *next.History.Rotation.KeepRawMonths != 3 {
		t.Fatalf("keep_raw_months = %v, want 3", next.History.Rotation.KeepRawMonths)
	}
	if err := applySettingsKey(next, "history.rotation.keep_raw_months", json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	if next.History.Rotation.KeepRawMonths != nil {
		t.Fatal("null did not clear keep_raw_months")
	}
	if err := applySettingsKey(next, "canary.journal.enabled", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	if next.Canary.Journal.Enabled == nil || *next.Canary.Journal.Enabled {
		t.Fatal("canary.journal.enabled=false did not apply")
	}

	// Snapshot round-trip: defaults and overrides render with the
	// access/source contract.
	s := newHistoryIndexServer(t)
	out := s.platformSettingsSnapshot(nil)
	if !out.History.Rotation.Enabled.Value || out.History.Rotation.KeepRawMonths.Value != 2 {
		t.Fatalf("default history settings = %+v", out.History)
	}
	if !out.Canary.Journal.Enabled.Value {
		t.Fatalf("default canary settings = %+v", out.Canary)
	}
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		keep := 5
		disabled := false
		next.History.Rotation.KeepRawMonths = &keep
		next.History.Rotation.Enabled = &disabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out = s.platformSettingsSnapshot(nil)
	if out.History.Rotation.Enabled.Value || out.History.Rotation.KeepRawMonths.Value != 5 {
		t.Fatalf("overridden history settings = %+v", out.History)
	}

	// The maintenance pass honors the runtime disable.
	if s.historyMaintenancePass(context.Background()) {
		t.Fatal("maintenance pass ran while rotation is disabled")
	}
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		enabled := true
		next.History.Rotation.Enabled = &enabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !s.historyMaintenancePass(context.Background()) {
		t.Fatal("maintenance pass skipped while rotation is enabled")
	}
}

// TestPurgeEvidenceInvariant is D12.2: a journaled purge-shaped flow only
// ever grows the order journal, and its purge_id-carrying rows land in
// order_events untouched.
func TestPurgeEvidenceInvariant(t *testing.T) {
	s := newHistoryIndexServer(t)
	journalPath := s.orderJournal.Path
	sizeAt := func() int64 {
		st, err := os.Stat(journalPath)
		if err != nil {
			return 0
		}
		return st.Size()
	}
	var last int64
	appendAndCheck := func(ev orderJournalEvent) {
		t.Helper()
		if err := s.orderJournal.Append(ev); err != nil {
			t.Fatal(err)
		}
		if now := sizeAt(); now <= last {
			t.Fatalf("order journal shrank or stalled: %d -> %d", last, now)
		} else {
			last = now
		}
	}
	now := time.Now().UTC()
	appendAndCheck(orderJournalEvent{At: now, Type: orderJournalEventPreviewed, OrderRef: "purge-ord-1", PurgeID: "purge-20260720-1", Account: "UTEST", Mode: "paper"})
	appendAndCheck(orderJournalEvent{At: now.Add(time.Second), Type: orderJournalEventSendAttempted, OrderRef: "purge-ord-1", PurgeID: "purge-20260720-1", ReservedOrderID: 900, Account: "UTEST", Mode: "paper", SendState: orderSendStateSendAttempted})
	appendAndCheck(orderJournalEvent{At: now.Add(2 * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: "purge-ord-1", PurgeID: "purge-20260720-1", Status: "Filled", SendState: orderSendStateTerminal, Account: "UTEST", Mode: "paper"})

	waitForHistory(t, func() (bool, error) {
		return s.historyIndex.Load().OrdersFresh(), nil
	})
	events, ok := s.indexedOrderEvents("test", nil, nil)
	if !ok {
		t.Fatal("index not serving after purge-shaped appends")
	}
	purgeRows := 0
	for _, ev := range events {
		if ev.PurgeID == "purge-20260720-1" {
			purgeRows++
		}
	}
	if purgeRows != 3 {
		t.Fatalf("purge_id rows in order_events = %d, want 3", purgeRows)
	}
	if now := sizeAt(); now != last {
		t.Fatalf("ingest changed the journal: %d != %d", now, last)
	}
}
