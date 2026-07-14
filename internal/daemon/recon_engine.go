package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/flexstmt"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Reconciliation engine (docs/design/post-trade-truth.md): matches broker
// statement flows against the declared capital-event ledger under the
// constitution's [recon] policy keys. Deterministic and regenerated from
// retained files on every call — the report id pins the exception set a
// reconcile signs off. Never-false-match: an ambiguous pairing is an
// exception, never a best-effort pick.

// reconFlow is one statement-side flow candidate after merge.
type reconFlow struct {
	id         string
	typ        string
	desc       string
	valueDate  time.Time
	amountBase float64
}

// buildReconReport regenerates the report. It is cheap (local files only)
// and side-effect free apart from reading journals.
func (s *Server) buildReconReport() *rpc.ReconResult {
	now := time.Now()
	res := &rpc.ReconResult{AsOf: now, Counts: map[string]int{}}
	configured, lastSuccess, lastAttempt, lastErr := s.flexFetchStatus()
	res.Fetch = rpc.ReconFetchStatus{Configured: configured, LastSuccess: lastSuccess, LastAttempt: lastAttempt, LastError: lastErr}

	var health []rpc.SourceHealth
	pol := s.riskPolicies.snapshot().policy
	rc := reconPolicyOf(pol)
	if rc == nil {
		res.Status = rpc.ReconStatusUnapproved
		res.Message = "recon.* policy keys are unapproved; write them in the risk policy before reconciliation can classify anything"
		health = append(health, rpc.SourceHealth{Source: "risk_policy", Status: "unapproved"})
		res.InputHealth = health
		return res
	}

	statements, problems, err := loadRetainedFlexStatements()
	switch {
	case err != nil:
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "cannot read retained statements: " + err.Error()
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable", Notes: []string{err.Error()}})
		return res
	case len(statements) == 0:
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "no retained Flex statements yet; enable [flex] and wait for the daily fetch, or check fetch.last_error"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable"})
		return res
	}
	res.Status = rpc.ReconStatusActive
	if len(problems) > 0 {
		res.Status = rpc.ReconStatusDegraded
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "degraded", Notes: problems})
	} else {
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "ok"})
	}

	// Merge: files arrive newest-first, so the first occurrence of a line
	// id wins (restatement supersede-by-id) and the first equity row per
	// report date wins.
	var (
		flows       []reconFlow
		exceptions  []rpc.ReconException
		seenLine    = map[string]bool{}
		equityByDay = map[string]flexstmt.EquityRow{}
	)
	for _, st := range statements {
		if st.WhenGenerated.After(res.StatementAsOf) {
			res.StatementAsOf = st.WhenGenerated
		}
		if res.CoverageFrom.IsZero() || st.FromDate.Before(res.CoverageFrom) {
			res.CoverageFrom = st.FromDate
		}
		if st.ToDate.After(res.CoverageTo) {
			res.CoverageTo = st.ToDate
		}
		for _, c := range st.Cash {
			if seenLine[c.ID] {
				continue
			}
			seenLine[c.ID] = true
			switch {
			case c.Category == flexstmt.CategoryFlow && c.AmountBase != nil:
				flows = append(flows, reconFlow{id: c.ID, typ: c.Type, desc: c.Description, valueDate: c.ValueDate, amountBase: *c.AmountBase})
			case c.Category == flexstmt.CategoryFlow: // flow with no usable base amount
				exceptions = append(exceptions, rpc.ReconException{LineID: c.ID, Category: rpc.ReconUncategorized, Type: c.Type,
					Description: c.Description, ValueDate: c.ValueDate, Note: "flow line without a usable base amount"})
			case c.Category == flexstmt.CategoryUncategorized:
				amt := c.AmountBase
				exceptions = append(exceptions, rpc.ReconException{LineID: c.ID, Category: rpc.ReconUncategorized, Type: c.Type,
					Description: c.Description, ValueDate: c.ValueDate, AmountBase: amt, Note: "unknown statement line type; classify or dismiss"})
			}
		}
		for _, t := range st.Transfers {
			if seenLine[t.ID] {
				continue
			}
			seenLine[t.ID] = true
			if t.AmountBase == nil {
				exceptions = append(exceptions, rpc.ReconException{LineID: t.ID, Category: rpc.ReconUncategorized, Type: "Transfer " + t.Direction,
					Description: t.Description, ValueDate: t.Date, Note: "transfer without a computable base value"})
				continue
			}
			amount := *t.AmountBase
			if t.Direction == "OUT" && amount > 0 {
				amount = -amount
			}
			flows = append(flows, reconFlow{id: t.ID, typ: "Transfer " + t.Direction, desc: t.Description, valueDate: t.Date, amountBase: amount})
		}
		for _, e := range st.Equity {
			key := e.ReportDate.Format("2006-01-02")
			if _, ok := equityByDay[key]; !ok {
				equityByDay[key] = e
			}
		}
	}

	events := replayCapitalFlowEvents()
	exceptions = append(exceptions, matchReconFlows(flows, events, rc)...)
	applyReconDismissals(exceptions)

	sort.Slice(exceptions, func(i, j int) bool { return exceptions[i].LineID < exceptions[j].LineID })
	for _, ex := range exceptions {
		res.Counts[ex.Category]++
		if !ex.Dismissed {
			res.Unresolved++
		}
	}
	res.Counts["matched"] = len(flows) - countFlowExceptions(exceptions)
	res.Exceptions = exceptions
	res.Equity = s.reconEquityCheck(equityByDay)
	res.ReportID = reconReportID(exceptions, res.CoverageFrom, res.CoverageTo, res.StatementAsOf, pol)
	res.InputHealth = health
	return res
}

func reconPolicyOf(c *risk.Constitution) *risk.ConstitutionRecon {
	if c == nil {
		return nil
	}
	r := c.Recon
	if r.AmountTolerancePct == nil || r.AmountToleranceMin == nil || r.DateWindowBusinessDays == nil || r.MaxReportAgeDays == nil {
		return nil
	}
	return &r
}

// matchReconFlows pairs statement flows with declared events. Bijective
// matches only: a flow or event with more than one candidate is ambiguous
// and surfaces as an exception (never-false-match).
func matchReconFlows(flows []reconFlow, events []capitalEventV1, rc *risk.ConstitutionRecon) []rpc.ReconException {
	tolerance := func(stmtAmount float64) float64 {
		return max(math.Abs(stmtAmount)**rc.AmountTolerancePct/100, *rc.AmountToleranceMin)
	}
	signed := func(ev capitalEventV1) float64 {
		if ev.Type == "withdrawal" {
			return -ev.AmountBase
		}
		return ev.AmountBase
	}
	amountOK := func(f reconFlow, ev capitalEventV1) bool {
		return math.Abs(f.amountBase-signed(ev)) <= tolerance(f.amountBase)
	}
	dateOK := func(f reconFlow, ev capitalEventV1, window int) bool {
		return businessDaysApart(f.valueDate, ev.EffectiveAt) <= window
	}

	flowCands := make([][]int, len(flows))
	eventCands := make([][]int, len(events))
	for fi, f := range flows {
		for ei, ev := range events {
			if amountOK(f, ev) && dateOK(f, ev, *rc.DateWindowBusinessDays) {
				flowCands[fi] = append(flowCands[fi], ei)
				eventCands[ei] = append(eventCands[ei], fi)
			}
		}
	}

	var out []rpc.ReconException
	flowDone := make([]bool, len(flows))
	eventDone := make([]bool, len(events))
	for fi, cands := range flowCands {
		switch {
		case len(cands) == 1 && len(eventCands[cands[0]]) == 1:
			flowDone[fi] = true
			eventDone[cands[0]] = true // matched
		case len(cands) > 1:
			f := flows[fi]
			amt := f.amountBase
			out = append(out, rpc.ReconException{LineID: f.id, Category: rpc.ReconAmbiguous, Type: f.typ, Description: f.desc,
				ValueDate: f.valueDate, AmountBase: &amt, Note: fmt.Sprintf("%d declared events qualify; tighten dates or amounts, never guessed", len(cands))})
			flowDone[fi] = true
		case len(cands) == 1: // its event serves multiple flows
			f := flows[fi]
			amt := f.amountBase
			out = append(out, rpc.ReconException{LineID: f.id, Category: rpc.ReconAmbiguous, Type: f.typ, Description: f.desc,
				ValueDate: f.valueDate, AmountBase: &amt, Note: "the qualifying declared event also matches another statement flow"})
			flowDone[fi] = true
		}
	}
	// Unmatched flows: probe for near-misses to name the exception well.
	for fi, f := range flows {
		if flowDone[fi] {
			continue
		}
		amt := f.amountBase
		ex := rpc.ReconException{LineID: f.id, Type: f.typ, Description: f.desc, ValueDate: f.valueDate, AmountBase: &amt}
		found := false
		for ei, ev := range events {
			if eventDone[ei] {
				continue
			}
			evAmt := signed(ev)
			// Plausibility bound for the amount_mismatch pairing: same
			// direction and within 10% of each other. Anything looser
			// glues unrelated flows together and hides a genuinely
			// missing line behind a nonsense pair. Categorization only —
			// both categories are exceptions the operator must resolve.
			sameSign := (f.amountBase >= 0) == (evAmt >= 0)
			rel := math.Abs(f.amountBase-evAmt) / max(math.Abs(f.amountBase), math.Abs(evAmt))
			switch {
			case dateOK(f, ev, *rc.DateWindowBusinessDays) && sameSign && rel <= 0.10:
				ex.Category = rpc.ReconAmountMismatch
				ex.EventAt = ev.EffectiveAt
				ex.EventAmountBase = &evAmt
				ex.Note = "dates align but amounts differ beyond tolerance"
			case amountOK(f, ev) && dateOK(f, ev, 3**rc.DateWindowBusinessDays):
				ex.Category = rpc.ReconDateMismatch
				ex.EventAt = ev.EffectiveAt
				ex.EventAmountBase = &evAmt
				ex.Note = "amounts align but the declared effective date is outside the window"
			default:
				continue
			}
			eventDone[ei] = true
			found = true
			break
		}
		if !found {
			ex.Category = rpc.ReconMissingFromLedger
			ex.Note = "statement flow with no declared capital event; declare it or dismiss with a reason"
		}
		out = append(out, ex)
	}
	for ei, ev := range events {
		if eventDone[ei] {
			continue
		}
		evAmt := signed(ev)
		out = append(out, rpc.ReconException{
			LineID:          "event-" + ev.At.UTC().Format("20060102-150405") + fmt.Sprintf("-%d", ei),
			Category:        rpc.ReconLedgerOnly,
			Type:            ev.Type,
			Description:     ev.Note,
			EventAt:         ev.EffectiveAt,
			EventAmountBase: &evAmt,
			Note:            "declared event with no statement flow; a loss dressed as a withdrawal would land here",
		})
	}
	return out
}

func countFlowExceptions(exceptions []rpc.ReconException) int {
	n := 0
	for _, ex := range exceptions {
		switch ex.Category {
		case rpc.ReconMissingFromLedger, rpc.ReconAmountMismatch, rpc.ReconDateMismatch, rpc.ReconAmbiguous:
			n++
		}
	}
	return n
}

// businessDaysApart counts weekdays strictly between the earlier and later
// timestamp's calendar days (same day = 0). Exchange holidays deliberately
// not modeled: a one-holiday slack belongs in the policy window.
func businessDaysApart(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	if da.After(db) {
		da, db = db, da
	}
	n := 0
	for d := da.AddDate(0, 0, 1); !d.After(db); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// replayCapitalFlowEvents returns the declared deposit/withdrawal events
// (journal order). Reconcile attestations are not flows.
func replayCapitalFlowEvents() []capitalEventV1 {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []capitalEventV1
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev capitalEventV1
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Version != 1 {
			continue
		}
		if ev.Type == "deposit" || ev.Type == "withdrawal" {
			out = append(out, ev)
		}
	}
	return out
}

// applyReconDismissals folds journaled human dismissals into the exception
// list. The journal is the source of truth; nothing else stores them.
func applyReconDismissals(exceptions []rpc.ReconException) {
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	reasons := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Kind   string `json:"kind"`
			LineID string `json:"line_id"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Kind == "recon_dismiss" && entry.LineID != "" {
			reasons[entry.LineID] = entry.Reason
		}
	}
	for i := range exceptions {
		if reason, ok := reasons[exceptions[i].LineID]; ok {
			exceptions[i].Dismissed = true
			exceptions[i].DismissReason = reason
		}
	}
}

func (s *Server) reconEquityCheck(equityByDay map[string]flexstmt.EquityRow) *rpc.ReconEquityCheck {
	var newest flexstmt.EquityRow
	for _, row := range equityByDay {
		if row.ReportDate.After(newest.ReportDate) {
			newest = row
		}
	}
	if newest.ReportDate.IsZero() {
		return nil
	}
	check := &rpc.ReconEquityCheck{StatementDate: newest.ReportDate, StatementTotalBase: newest.TotalBase}
	if s.riskCapital != nil {
		if equity, asOf := s.riskCapital.LastEquity(); equity > 0 {
			check.RuntimeEquityBase = &equity
			check.RuntimeAsOf = asOf
			if newest.TotalBase != 0 {
				div := (equity - newest.TotalBase) / math.Abs(newest.TotalBase) * 100
				check.DivergencePct = &div
			}
		}
	}
	return check
}

// reconReportID pins the exception set, coverage, statement freshness, and
// the policy identity that classified it.
func reconReportID(exceptions []rpc.ReconException, from, to, stmtAsOf time.Time, pol *risk.Constitution) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), stmtAsOf.UTC().Format(time.RFC3339))
	if pol != nil {
		fmt.Fprintf(h, "%s|", pol.FingerprintKey())
	}
	for _, ex := range exceptions {
		fmt.Fprintf(h, "%s|%s|%t\n", ex.LineID, ex.Category, ex.Dismissed)
	}
	return "recon-" + hex.EncodeToString(h.Sum(nil))[:16]
}
