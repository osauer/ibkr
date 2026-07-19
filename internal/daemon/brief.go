package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/canary"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	briefStateFile            = "brief-state.json"
	briefStateVersion         = 1
	briefFingerprintMax       = 256
	briefMoverLimit           = 3
	monthlyRenderReceiptLimit = 64
	monthlyRenderReceiptTTL   = 15 * time.Minute
)

type monthlyRenderReceipt struct {
	Month             string
	AuthorityIdentity string
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

type briefRuleState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type briefStampState struct {
	Fingerprint         string           `json:"fingerprint"`
	At                  time.Time        `json:"at"`
	RulebookFingerprint string           `json:"rulebook_fingerprint,omitempty"`
	Rules               []briefRuleState `json:"rules,omitempty"`
}

type briefStateFileV1 struct {
	Version int                        `json:"version"`
	Stamps  map[string]briefStampState `json:"stamps,omitempty"`
}

type briefStateStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	state  briefStateFileV1
}

func (s *Server) installBriefStateStore() {
	if s == nil {
		return
	}
	path, err := defaultTradingStatePath(briefStateFile)
	if err != nil {
		s.warnf("brief state: resolve state path: %v (stamp baselines will not survive restart)", err)
	}
	s.briefState = &briefStateStore{path: path}
}

func (st *briefStateStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	st.state = briefStateFileV1{Version: briefStateVersion, Stamps: map[string]briefStampState{}}
	if st.path == "" {
		return
	}
	data, err := osReadFile(st.path)
	if err != nil {
		return
	}
	var persisted briefStateFileV1
	if json.Unmarshal(data, &persisted) == nil && persisted.Version == briefStateVersion {
		st.state = persisted
		if st.state.Stamps == nil {
			st.state.Stamps = map[string]briefStampState{}
		}
	}
}

// osReadFile is a package seam kept variable-free in production; tests use
// ordinary XDG paths and do not need to replace it.
func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (st *briefStateStore) latestBaseline() (briefStampState, bool) {
	if st == nil {
		return briefStampState{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	var latest briefStampState
	for _, stamp := range st.state.Stamps {
		if stamp.At.After(latest.At) {
			latest = stamp
		}
	}
	return latest, !latest.At.IsZero()
}

func (st *briefStateStore) stamp(kind, fingerprint string, at time.Time, rules *rpc.RulesResult) error {
	if st == nil || st.path == "" {
		return fmt.Errorf("brief state persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	stamp := briefStampState{Fingerprint: fingerprint, At: at.UTC()}
	if rules != nil {
		if rules.PolicyFingerprint != nil {
			stamp.RulebookFingerprint = rules.PolicyFingerprint.Key
		}
		stamp.Rules = make([]briefRuleState, 0, len(rules.Rules))
		for _, row := range rules.Rules {
			stamp.Rules = append(stamp.Rules, briefRuleState{ID: row.ID, Status: row.Status})
		}
	}
	st.state.Stamps[kind] = stamp
	data, err := json.Marshal(st.state)
	if err != nil {
		return err
	}
	return writePrivateStateAtomic(st.path, data)
}

func (st *briefStateStore) stampedToday(kind string, now time.Time) (briefStampState, bool) {
	if st == nil {
		return briefStampState{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	stamp, ok := st.state.Stamps[kind]
	return stamp, ok && sameLocalDay(stamp.At, now)
}

func (s *Server) handleBriefSnapshot(ctx context.Context, req *rpc.Request) (*rpc.BriefResult, error) {
	if len(req.Params) > 0 {
		var p rpc.BriefSnapshotParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	res, _ := s.composeBrief(ctx)
	return res, nil
}

func (s *Server) handleBriefAck(ctx context.Context, req *rpc.Request) (*rpc.BriefAckResult, error) {
	var p rpc.BriefAckParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(p.Kind))
	if kind == rpc.BriefKindMonthly {
		return s.handleMonthlyBriefAck(ctx, p)
	}
	// Origin is checked before any composition or store access that could
	// lead to a write. Refused agent/empty requests journal nothing.
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	if kind != rpc.BriefKindMorning && kind != rpc.BriefKindEOD {
		return nil, errBadRequest("brief kind must be morning or eod")
	}
	fingerprint := strings.TrimSpace(p.BriefFingerprint)
	if fingerprint == "" || len(fingerprint) > briefFingerprintMax {
		return nil, errBadRequest(fmt.Sprintf("brief fingerprint must be non-empty and at most %d bytes", briefFingerprintMax))
	}
	now := s.briefNow()
	day := localDay(now)
	if rec, ok := artefactCompletedInPeriod(s.riskCapital.Artefacts(), kind, now); ok {
		return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: rec.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefFingerprint,
			Message: fmt.Sprintf("%s artefact already complete for %s", kind, day)}, nil
	}
	if stamp, ok := s.briefState.stampedToday(kind, now); ok {
		return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: stamp.At,
			AlreadyStamped: true, BriefFingerprint: stamp.Fingerprint,
			Message: fmt.Sprintf("%s brief already stamped for %s", kind, day)}, nil
	}

	// Re-evaluate only the rule set at stamp time for the durable delta
	// baseline. The already-rendered fingerprint is the attested content;
	// there is no second full brief fan-out on the write path.
	rules := s.evaluateRulesMode(ctx, false, false)
	mgr := s.riskPolicies.snapshot()
	rec, err := s.riskCapital.RecordArtefact(rpc.ArtefactParams{
		Artefact: kind, Origin: normalizedWriteOrigin(p.Origin), BriefFingerprint: fingerprint,
	}, mgr.policy)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if err := s.briefState.stamp(kind, fingerprint, rec.CompletedAt, rules); err != nil {
		return nil, fmt.Errorf("persist brief stamp baseline: %w", err)
	}
	return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: rec.CompletedAt,
		BriefFingerprint: fingerprint, Message: fmt.Sprintf("stamped %s artefact for %s", kind, day)}, nil
}

func (s *Server) handleMonthlyBriefAck(ctx context.Context, p rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Monthly completion is narrower than the legacy human-origin stamp: only
	// the authenticated paired foreground route may supply this origin.
	if strings.TrimSpace(p.Origin) != rpc.OrderOriginPairedDevice {
		return nil, errBadRequest("monthly brief completion requires human-paired-device foreground-render evidence; CLI and agent origins are refused")
	}
	fingerprint := strings.TrimSpace(p.BriefFingerprint)
	if fingerprint == "" || len(fingerprint) > briefFingerprintMax {
		return nil, errBadRequest(fmt.Sprintf("brief fingerprint must be non-empty and at most %d bytes", briefFingerprintMax))
	}
	if strings.TrimSpace(p.Evidence) != rpc.BriefAckEvidenceRender {
		return nil, errBadRequest("monthly brief completion requires render evidence")
	}
	if s == nil || s.riskPolicies == nil || s.nudges == nil {
		return nil, errBadRequest("monthly brief completion state is unavailable")
	}
	now := s.briefNow().UTC()
	authority := s.currentNudgeAuthority(now)
	policy := authority.policy
	if !authority.eligible {
		return nil, errBadRequest("monthly brief completion is unavailable until current active fully approved v4 authority is present")
	}
	report, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	day := nudgeLocalDay(policy.Cadence, now)
	evaluation, completion := s.governanceMonthlyPulseForWrite(authority, policy, report, now)
	if strings.TrimSpace(p.Month) != evaluation.Month || evaluation.Month == "" {
		return nil, errBadRequest("monthly brief completion month does not match the current policy month")
	}
	if evaluation.Status == risk.MonthlyPulseStatusCompleted && completion != nil {
		rec, ok := s.nudges.monthlyCompletionRecord(evaluation.Month, authority.policyIdentity)
		if !ok || rec.BriefIdentity != fingerprint {
			return nil, errBadRequest("monthly brief completion conflicts with the pinned rendered brief")
		}
		return &rpc.BriefAckResult{
			OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: completion.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefIdentity, Month: evaluation.Month,
			Evidence: rpc.BriefAckEvidenceRender, Message: "monthly foreground render already recorded",
		}, nil
	}
	if !policyPinsReady(authority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion is unavailable until readable matching policy pins are present")
	}
	if evaluation.Status != risk.MonthlyPulseStatusDue {
		return nil, errBadRequest("monthly brief is not currently due with readable matching policy pins")
	}
	authorityIdentity := monthlyAuthorityIdentity(authority, evaluation.Month, report, now)
	receipt, ok := s.monthlyRenderReceipt(fingerprint, evaluation.Month, authorityIdentity, now)
	if !ok {
		return nil, errBadRequest("monthly brief fingerprint has no current daemon render receipt; render the monthly brief again")
	}
	if s.monthlyAckBeforeWriteLock != nil {
		s.monthlyAckBeforeWriteLock()
	}
	s.nudgeWriteMu.Lock()
	defer s.nudgeWriteMu.Unlock()
	if s.nudgeBeforeCommit != nil {
		s.nudgeBeforeCommit("monthly")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalAuthority := s.currentNudgeAuthority(now)
	if !finalAuthority.eligible || !policyPinsReady(finalAuthority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	finalReport, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	finalEvaluation, finalCompletion := s.governanceMonthlyPulseForWrite(finalAuthority, finalAuthority.policy, finalReport, now)
	finalAuthorityIdentity := monthlyAuthorityIdentity(finalAuthority, finalEvaluation.Month, finalReport, now)
	if receipt.AuthorityIdentity != finalAuthorityIdentity {
		return nil, errBadRequest("monthly brief completion conflicts with current authority")
	}
	if finalCompletion != nil {
		if finalEvaluation.Month != evaluation.Month || finalAuthority.policyIdentity != authority.policyIdentity {
			return nil, errBadRequest("monthly brief completion authority changed before persistence")
		}
		rec, ok := s.nudges.monthlyCompletionRecord(finalEvaluation.Month, finalAuthority.policyIdentity)
		if !ok || rec.BriefIdentity != fingerprint || !rec.CompletedAt.Equal(finalCompletion.CompletedAt) {
			return nil, errBadRequest("monthly brief completion conflicts with the pinned rendered brief")
		}
		s.consumeMonthlyRenderReceipt(fingerprint, now)
		return &rpc.BriefAckResult{
			OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: rec.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefIdentity, Month: rec.Month,
			Evidence: rec.Evidence, Message: "monthly foreground render already recorded",
		}, nil
	}
	if finalEvaluation.Status != risk.MonthlyPulseStatusDue || finalEvaluation.Month != evaluation.Month {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	if s.nudgeAfterValidation != nil {
		s.nudgeAfterValidation("monthly")
	}
	// The test seam represents the last possible authority race before the
	// filesystem write. Revalidate again so receipt acceptance is still bound
	// to the exact current policy/pin/report generation at commit time.
	commitAuthority := s.currentNudgeAuthority(now)
	if !commitAuthority.eligible || !policyPinsReady(commitAuthority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	commitReport, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	commitEvaluation, commitCompletion := s.governanceMonthlyPulseForWrite(commitAuthority, commitAuthority.policy, commitReport, now)
	commitAuthorityIdentity := monthlyAuthorityIdentity(commitAuthority, commitEvaluation.Month, commitReport, now)
	if commitCompletion != nil || commitEvaluation.Status != risk.MonthlyPulseStatusDue ||
		commitEvaluation.Month != finalEvaluation.Month || commitAuthorityIdentity != finalAuthorityIdentity ||
		receipt.AuthorityIdentity != commitAuthorityIdentity {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	rec, already, err := s.nudges.recordMonthlyCompletion(commitEvaluation.Month, commitAuthority.policyIdentity, fingerprint, commitAuthorityIdentity, now)
	if err != nil {
		return nil, fmt.Errorf("persist monthly brief completion: %w", err)
	}
	if s.nudgeAfterPersist != nil {
		s.nudgeAfterPersist("monthly")
	}
	s.consumeMonthlyRenderReceipt(fingerprint, now)
	return &rpc.BriefAckResult{
		OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: rec.CompletedAt,
		AlreadyStamped: already, BriefFingerprint: rec.BriefIdentity, Month: rec.Month,
		Evidence: rec.Evidence, Message: "monthly paired-device foreground render recorded",
	}, nil
}

func (s *Server) issueMonthlyRenderReceipt(fingerprint, month, authorityIdentity string, now time.Time) {
	s.issueMonthlyRenderReceiptContext(context.Background(), fingerprint, month, authorityIdentity, now)
}

func (s *Server) issueMonthlyRenderReceiptContext(ctx context.Context, fingerprint, month, authorityIdentity string, now time.Time) bool {
	if s == nil || fingerprint == "" || month == "" || authorityIdentity == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	wasNil := s.monthlyRenderReceipts == nil
	before := make(map[string]monthlyRenderReceipt, len(s.monthlyRenderReceipts))
	maps.Copy(before, s.monthlyRenderReceipts)
	if s.monthlyRenderBeforePersist != nil {
		s.monthlyRenderBeforePersist()
	}
	if ctx.Err() != nil {
		return false
	}
	if s.monthlyRenderReceipts == nil {
		s.monthlyRenderReceipts = make(map[string]monthlyRenderReceipt)
	}
	s.pruneMonthlyRenderReceiptsLocked(now)
	for len(s.monthlyRenderReceipts) >= monthlyRenderReceiptLimit {
		oldestKey := ""
		var oldest time.Time
		for key, receipt := range s.monthlyRenderReceipts {
			if oldestKey == "" || receipt.IssuedAt.Before(oldest) {
				oldestKey, oldest = key, receipt.IssuedAt
			}
		}
		delete(s.monthlyRenderReceipts, oldestKey)
	}
	s.monthlyRenderReceipts[fingerprint] = monthlyRenderReceipt{
		Month: month, AuthorityIdentity: authorityIdentity,
		IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(monthlyRenderReceiptTTL),
	}
	if ctx.Err() != nil {
		if wasNil {
			s.monthlyRenderReceipts = nil
		} else {
			s.monthlyRenderReceipts = before
		}
		return false
	}
	return true
}

func (s *Server) monthlyRenderReceipt(fingerprint, month, authorityIdentity string, now time.Time) (monthlyRenderReceipt, bool) {
	if s == nil {
		return monthlyRenderReceipt{}, false
	}
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	s.pruneMonthlyRenderReceiptsLocked(now)
	receipt, ok := s.monthlyRenderReceipts[fingerprint]
	return receipt, ok && receipt.Month == month && receipt.AuthorityIdentity == authorityIdentity
}

func (s *Server) consumeMonthlyRenderReceipt(fingerprint string, now time.Time) {
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	s.pruneMonthlyRenderReceiptsLocked(now)
	delete(s.monthlyRenderReceipts, fingerprint)
}

func (s *Server) pruneMonthlyRenderReceiptsLocked(now time.Time) {
	for fingerprint, receipt := range s.monthlyRenderReceipts {
		if !now.Before(receipt.ExpiresAt) {
			delete(s.monthlyRenderReceipts, fingerprint)
		}
	}
}

func (s *Server) composeBrief(ctx context.Context) (*rpc.BriefResult, *rpc.RulesResult) {
	now := s.briefNow()
	res := &rpc.BriefResult{AsOf: now}

	acct, acctErr := s.buildAccountSummary(ctx, false)
	pos, posErr := s.handlePositionsList(ctx, &rpc.Request{})
	regime, regimeErr := s.briefRegimeSnapshot()
	breadth, breadthErr := s.buildBreadthSPX(&rpc.Request{}, false)
	gamma := s.briefGammaSnapshot()
	cal, calErr := s.handleMarketCalendar(&rpc.Request{Params: briefJSON(rpc.MarketCalendarParams{Market: "us", At: now, Days: 1})})

	var marketEvents *rpc.MarketEventsResult
	var marketEventsErr error
	if pos != nil {
		symbols := marketEventSymbolsFromPositions(pos)
		marketEvents, marketEventsErr = s.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: symbols})})
	} else {
		marketEventsErr = posErr
	}

	rules := s.evaluateRulesMode(ctx, false, false)
	renderAuthority := s.currentNudgeAuthority(now)
	policy := s.briefPolicyResultForAuthority(acct, acctErr, renderAuthority, now)
	constitution := renderAuthority.policy
	recon := s.buildReconReport()

	res.Market = composeBriefMarket(now, acct, pos, regime, breadth, gamma, marketEvents,
		acctErr, posErr, regimeErr, breadthErr, marketEventsErr)
	res.Calendar = composeBriefCalendar(cal, marketEvents, rules, calErr, marketEventsErr)
	res.Portfolio = s.composeBriefPortfolio(acct, pos, acctErr, posErr)
	res.RiskLimits = composeBriefRisk(policy, now)
	res.Process = s.composeBriefProcessForAuthority(policy, constitution, recon, rules, renderAuthority, now)
	res.StampTarget, res.StampTargetReason = s.briefStampTarget(policy, constitution, now)
	res.BriefFingerprint = briefContentFingerprint(res)
	// V4 monthly render evidence is bound to the current constitution even
	// when a policy-only revision happens not to alter a visible brief row.
	// V1-v3 retain their existing daily-stamp fingerprint byte behavior.
	if constitution != nil && constitution.PolicyVersion >= 4 {
		res.BriefFingerprint = opaqueIdentity("v4-brief", res.BriefFingerprint, renderAuthority.policyIdentity)
		if s.monthlyRenderBeforeIssue != nil {
			s.monthlyRenderBeforeIssue()
		}
		currentAuthority := s.currentNudgeAuthority(now)
		if nudgeAuthorityToken(renderAuthority) != nudgeAuthorityToken(currentAuthority) {
			return res, rules
		}
		monthly, _ := s.governanceMonthlyPulseForAuthority(renderAuthority, constitution, recon, now)
		if monthly.Status == risk.MonthlyPulseStatusBlocked {
			if recovery := s.governanceMonthlyPulseForRenderRecovery(renderAuthority, constitution, now); recovery.Status != "" {
				monthly = recovery
			}
		}
		if monthly.Status == risk.MonthlyPulseStatusDue {
			s.issueMonthlyRenderReceiptContext(ctx, res.BriefFingerprint, monthly.Month,
				monthlyAuthorityIdentity(renderAuthority, monthly.Month, recon, now), now)
		}
	}
	return res, rules
}

func (s *Server) briefNow() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func briefJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) briefGammaSnapshot() *rpc.GammaZeroSPXResult {
	if s == nil || s.zeroGamma == nil {
		return nil
	}
	env := s.zeroGamma.snapshotForScope(rpc.GammaZeroScopeCombined, nil, s.briefNow)
	return &env
}

func (s *Server) briefRegimeSnapshot() (*rpc.RegimeSnapshotResult, error) {
	if s == nil {
		return nil, fmt.Errorf("regime snapshot unavailable")
	}
	s.lastRegimeSnapshotMu.Lock()
	defer s.lastRegimeSnapshotMu.Unlock()
	if s.lastRegimeSnapshot == nil {
		return nil, fmt.Errorf("no daemon regime snapshot has completed yet")
	}
	copyResult := *s.lastRegimeSnapshot
	return &copyResult, nil
}

func (s *Server) briefPolicyResult(acct *rpc.AccountResult, acctErr error, now time.Time) *rpc.RiskPolicyResult {
	return s.briefPolicyResultForAuthority(acct, acctErr, s.currentNudgeAuthority(now), now)
}

func (s *Server) briefPolicyResultForAuthority(acct *rpc.AccountResult, acctErr error, authority nudgeAuthorityState, now time.Time) *rpc.RiskPolicyResult {
	value := authority.report
	res := &value
	res.AsOf = now
	res.Unapproved = append([]string(nil), authority.report.Unapproved...)
	res.Inventory = append([]rpc.PolicyPinStatus(nil), authority.report.Inventory...)
	if authority.policy == nil {
		res.Unapproved = (&risk.Constitution{}).UnapprovedKeys()
	}
	var obs *risk.CapitalObservation
	if acctErr == nil && acct != nil && acct.NetLiquidation > 0 &&
		(authority.policy == nil || authority.policy.Capital.BaseCurrency == "" || acct.BaseCurrency == "" || strings.EqualFold(authority.policy.Capital.BaseCurrency, acct.BaseCurrency)) {
		obs = &risk.CapitalObservation{EquityBase: acct.NetLiquidation, AsOf: acct.AsOf}
	}
	res.Capital = s.riskCapital.Report(authority.policy, obs)
	res.Limits = risk.ConstitutionLimits(authority.policy)
	res.Overrides = s.riskCapital.OverridesSnapshot()
	res.Cadence = s.riskCapital.Artefacts()
	return res
}

func composeBriefMarket(now time.Time, acct *rpc.AccountResult, pos *rpc.PositionsResult,
	regime *rpc.RegimeSnapshotResult, breadth *rpc.BreadthSPXResult, gamma *rpc.GammaZeroSPXResult,
	events *rpc.MarketEventsResult, acctErr, posErr, regimeErr, breadthErr, eventsErr error) rpc.BriefMarketSection {
	out := rpc.BriefMarketSection{}
	if regimeErr != nil || regime == nil {
		out.Regime.BriefRowState = briefUnavailable("regime snapshot unavailable: " + errText(regimeErr))
	} else {
		out.Regime.BriefRowState = briefOK("daemon regime lifecycle and composite verdict")
		out.Regime.Stage = regime.Posture.Stage
		if out.Regime.Stage == "" {
			out.Regime.Stage = regime.Lifecycle.Stage
		}
		out.Regime.Verdict = regime.Composite.Verdict
		if len(regime.WarningDetails) > 0 || out.Regime.Stage == "" || out.Regime.Verdict == "" {
			out.Regime.BriefRowState = briefDegraded("regime returned partial or unclassified evidence")
		}
	}
	if breadthErr != nil || breadth == nil {
		out.Breadth.BriefRowState = briefUnavailable("breadth snapshot unavailable: " + errText(breadthErr))
	} else if breadth.State != rpc.BreadthStateReady {
		out.Breadth.BriefRowState = briefDegraded("breadth source is " + string(breadth.State))
	} else {
		out.Breadth.BriefRowState = briefOK("S&P 500 constituent breadth")
		out.Breadth.PctAbove50DMA = new(breadth.PctAbove50DMA)
		out.Breadth.PctAbove200DMA = new(breadth.PctAbove200DMA)
		out.Breadth.NetNewHighsPct = new(breadth.NetNewHighsPct)
	}
	if breadth != nil {
		out.Breadth.AsOf, out.Breadth.DataType = breadth.AsOf, breadth.DataType
	}
	out.Gamma = composeBriefGamma(gamma)
	canaryInput := rpc.CanaryInput{Now: now}
	if acct != nil {
		canaryInput.Account = *acct
	}
	if pos != nil {
		canaryInput.Positions = *pos
	}
	if regime != nil {
		canaryInput.Regime = *regime
	}
	if events != nil {
		canaryInput.MarketEvents = *events
	}
	can := canary.ComputeCanary(canaryInput)
	out.Canary = rpc.BriefCanaryRow{
		BriefRowState: briefOK("pure canary composition over daemon snapshots"),
		Action:        can.Action, Severity: string(can.Severity), Summary: can.Summary,
	}
	if acctErr != nil || posErr != nil || regimeErr != nil || eventsErr != nil || can.InputHealth != "ok" {
		out.Canary.BriefRowState = briefDegraded("canary inputs are partial; unavailable sources remain explicit")
	}
	out.BriefRowState = briefSectionState("market", out.Regime.BriefRowState, out.Breadth.BriefRowState, out.Gamma.BriefRowState, out.Canary.BriefRowState)
	return out
}

func composeBriefGamma(env *rpc.GammaZeroSPXResult) rpc.BriefGammaRow {
	row := rpc.BriefGammaRow{BriefRowState: briefUnavailable("dealer gamma cache is unavailable")}
	if env == nil {
		return row
	}
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		row.BriefRowState = briefDegraded("dealer gamma source is " + env.Status)
		return row
	}
	computed := env.Result
	if spx := computed.PerIndex["SPX"]; spx != nil {
		computed = spx
	}
	row.BriefRowState = briefOK("SPX dealer zero-gamma versus spot")
	if computed.SpotUnderlying > 0 {
		row.Spot = new(computed.SpotUnderlying)
	}
	row.ZeroGamma, row.GapPct, row.GammaSign, row.AsOf = computed.ZeroGamma, computed.GapPct, computed.GammaSign, computed.AsOf
	if row.Spot == nil || (row.ZeroGamma == nil && row.GammaSign == "") {
		row.BriefRowState = briefDegraded("gamma result lacks a complete spot/zero-crossing classification")
	}
	if computed.Quality != nil && computed.Quality.Rankability != rpc.GammaRankabilityRankable {
		row.BriefRowState = briefDegraded("gamma is context-only: " + computed.Quality.RankabilityReason)
	}
	return row
}

func composeBriefCalendar(cal *rpc.MarketCalendarResult, events *rpc.MarketEventsResult, rules *rpc.RulesResult, calErr, eventsErr error) rpc.BriefCalendarSection {
	out := rpc.BriefCalendarSection{}
	if calErr != nil || cal == nil {
		out.Session.BriefRowState = briefUnavailable("market calendar unavailable: " + errText(calErr))
	} else {
		s := cal.Session
		out.Session = rpc.BriefSessionRow{BriefRowState: briefOK(nonEmptyString(s.Reason, "official session calendar")),
			Market: s.Market, State: s.State, IsOpen: s.IsOpen, Open: s.Open, Close: s.Close}
		if s.NextOpen != nil {
			out.Session.NextOpen = *s.NextOpen
		}
	}
	out.MarketEvents = briefMarketEventRows(events, rules, eventsErr)
	states := []rpc.BriefRowState{out.Session.BriefRowState}
	for _, row := range out.MarketEvents {
		states = append(states, row.BriefRowState)
	}
	out.BriefRowState = briefSectionState("calendar", states...)
	return out
}

func briefMarketEventRows(events *rpc.MarketEventsResult, rules *rpc.RulesResult, sourceErr error) []rpc.BriefMarketEventRow {
	kinds := []string{"earnings", "halt", "ssr", "borrow"}
	sets := map[string]map[string]struct{}{}
	for _, kind := range kinds {
		sets[kind] = map[string]struct{}{}
	}
	if rules != nil {
		for _, e := range rules.Earnings {
			if e.Date != "" && e.Source != "unknown" {
				sets["earnings"][strings.ToUpper(e.Symbol)] = struct{}{}
			}
		}
	}
	if events != nil {
		for _, flag := range events.Flags {
			id := strings.ToLower(flag.ID + " " + flag.Label)
			kind := ""
			switch {
			case strings.Contains(id, "halt") || strings.Contains(id, "luld"):
				kind = "halt"
			case strings.Contains(id, "reg_sho") || strings.Contains(id, "ssr"):
				kind = "ssr"
			case strings.Contains(id, "borrow"):
				kind = "borrow"
			}
			if kind != "" && flag.Status != rpc.MarketEventStatusInactive {
				sets[kind][strings.ToUpper(flag.Symbol)] = struct{}{}
			}
		}
	}
	rows := make([]rpc.BriefMarketEventRow, 0, len(kinds))
	degraded := sourceErr != nil || events == nil || briefSourceHealthDegraded(events.SourceHealth)
	for _, kind := range kinds {
		syms := mapKeysSorted(sets[kind])
		state := briefOK(fmt.Sprintf("%d held symbol(s) flagged", len(syms)))
		if degraded || (kind == "earnings" && rules == nil) {
			state = briefDegraded(fmt.Sprintf("%d known; one or more event sources are degraded", len(syms)))
		}
		rows = append(rows, rpc.BriefMarketEventRow{BriefRowState: state, Kind: kind, Count: len(syms), Symbols: syms})
	}
	return rows
}

func (s *Server) composeBriefPortfolio(acct *rpc.AccountResult, pos *rpc.PositionsResult, acctErr, posErr error) rpc.BriefPortfolioSection {
	out := rpc.BriefPortfolioSection{}
	if acctErr != nil || acct == nil {
		out.Account.BriefRowState = briefUnavailable("account summary unavailable: " + errText(acctErr))
	} else {
		out.Account = rpc.BriefAccountRow{BriefRowState: briefOK("account summary in base currency"),
			DailyPnLBase: acct.DailyPnL, BaseCurrency: acct.BaseCurrency, AsOf: acct.AsOf}
		if acct.NetLiquidation > 0 {
			out.Account.EquityBase = new(acct.NetLiquidation)
		}
		if out.Account.EquityBase == nil || acct.DailyPnL == nil {
			out.Account.BriefRowState = briefDegraded("daily P&L is unavailable; equity remains present")
			if out.Account.EquityBase == nil {
				out.Account.Detail = "account equity is unavailable; zero was not substituted"
			}
		}
	}
	if posErr != nil || pos == nil {
		out.Movers.BriefRowState = briefUnavailable("positions unavailable: " + errText(posErr))
		out.PremiumAtRisk.BriefRowState = briefUnavailable("positions unavailable")
		out.HedgeCost.BriefRowState = briefUnavailable("positions unavailable")
	} else {
		out.Movers = briefMovers(pos)
		out.PremiumAtRisk = briefPremiumAtRisk(pos, out.Account.BaseCurrency)
		out.HedgeCost = briefHedgeCost(pos, out.Account.BaseCurrency)
	}
	out.WorkingOrders = s.briefWorkingOrders()
	out.BriefRowState = briefSectionState("portfolio", out.Account.BriefRowState, out.Movers.BriefRowState,
		out.PremiumAtRisk.BriefRowState, out.HedgeCost.BriefRowState, out.WorkingOrders.BriefRowState)
	return out
}

func briefMovers(pos *rpc.PositionsResult) rpc.BriefMoversRow {
	row := rpc.BriefMoversRow{BriefRowState: briefOK("top positions by absolute daily P&L")}
	for _, p := range append(slices.Clone(pos.Stocks), pos.Options...) {
		if p.DailyPnLBase != nil {
			row.Rows = append(row.Rows, rpc.BriefMover{Symbol: strings.ToUpper(p.Symbol), DailyPnLBase: *p.DailyPnLBase})
		}
	}
	sort.SliceStable(row.Rows, func(i, j int) bool {
		return math.Abs(row.Rows[i].DailyPnLBase) > math.Abs(row.Rows[j].DailyPnLBase)
	})
	if len(row.Rows) > briefMoverLimit {
		row.Rows = row.Rows[:briefMoverLimit]
	}
	if len(row.Rows) == 0 {
		row.BriefRowState = briefDegraded("no position daily P&L values are available")
	}
	return row
}

func briefPremiumAtRisk(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("long-option market value in base currency"), BaseCurrency: base}
	var sum float64
	for _, p := range pos.Options {
		if p.Quantity <= 0 {
			continue
		}
		if p.MarketValueBase == nil {
			row.ExcludedLegs++
			continue
		}
		sum += *p.MarketValueBase
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d long option leg(s) excluded because base market value is unavailable", row.ExcludedLegs))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no long option positions")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func briefHedgeCost(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("daily theta of rulebook-classified hedge legs"), BaseCurrency: base}
	pol := risk.DefaultRulebookPolicy()
	var sum float64
	for _, p := range pos.Options {
		candidate := p.Quantity > 0 && strings.EqualFold(p.Right, "P") && pol.IsHedgeSymbol(p.Symbol)
		if !candidate {
			continue
		}
		leg := risk.LegInput{Right: p.Right, Quantity: p.Quantity, Delta: p.Delta, Underlying: p.Underlying, HedgeListed: true}
		if !risk.RulebookHedgeLeg(leg) || p.Theta == nil {
			row.ExcludedLegs++
			continue
		}
		value := *p.Theta * p.Quantity * float64(max(p.Multiplier, 1))
		if rate, ok := positionBaseRate(p, base); ok {
			value *= rate
		} else {
			row.ExcludedLegs++
			continue
		}
		sum += value
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d candidate hedge leg(s) excluded because classification Greeks/theta/FX are unavailable", row.ExcludedLegs))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no rulebook-classified hedge legs")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func (s *Server) briefWorkingOrders() rpc.BriefCountRow {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return rpc.BriefCountRow{BriefRowState: briefUnavailable("open-orders journal unavailable: " + err.Error())}
	}
	scope := s.currentBrokerStateScope()
	count := 0
	for _, view := range views {
		if view.Open && orderViewMatchesBrokerScope(view, scope) {
			count++
		}
	}
	return rpc.BriefCountRow{BriefRowState: briefOK("daemon open-orders journal view"), Count: &count}
}

func composeBriefRisk(policy *rpc.RiskPolicyResult, now time.Time) rpc.BriefRiskSection {
	out := rpc.BriefRiskSection{}
	if policy == nil || policy.Status == rpc.RiskPolicyStatusAbsent {
		state := briefUnavailable("risk constitution absent; capital controls are unapproved")
		out.Capital.BriefRowState, out.Latch.BriefRowState, out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState = state, state, state, state
		out.BriefRowState = state
		return out
	}
	c := policy.Capital
	out.Capital = rpc.BriefCapitalRow{BriefRowState: briefOK("constitution capital state"), Tier: c.Tier,
		Enforcement: c.Enforcement, ConsumedPct: c.ConsumedPct, DrawdownBase: c.DrawdownBase,
		AdjustedPeakBase: c.AdjustedPeakBase, BaseCurrency: c.BaseCurrency}
	if len(policy.Unapproved) > 0 || c.Tier == risk.CapitalTierUnapproved || c.ConsumedPct == nil {
		out.Capital.BriefRowState = briefDegraded("one or more capital inputs or policy decisions are unapproved")
	}
	out.Latch = rpc.BriefLatchRow{BriefRowState: briefOK("drawdown latch is not engaged"), Latched: c.BlockLatched, At: c.LatchedAt}
	if c.BlockLatched {
		age := max(int(now.Sub(c.LatchedAt).Hours()/24), 0)
		out.Latch.AgeDays = &age
		out.Latch.Detail = "drawdown latch remains engaged until a human reset"
	}
	out.Overrides.BriefRowState = briefOK("no active overrides")
	for _, o := range policy.Overrides {
		if o.Active && !now.After(o.ExpiresAt) {
			out.Overrides.Rows = append(out.Overrides.Rows, rpc.BriefOverride{Control: o.Control, ExpiresAt: o.ExpiresAt})
		}
	}
	if len(out.Overrides.Rows) > 0 {
		out.Overrides.Detail = fmt.Sprintf("%d active override(s)", len(out.Overrides.Rows))
	}
	out.PolicyDrift.BriefRowState = briefOK("all approval pins match")
	for _, pin := range policy.Inventory {
		if pin.Status != "match" {
			out.PolicyDrift.Rows = append(out.PolicyDrift.Rows, pin)
		}
	}
	if len(out.PolicyDrift.Rows) > 0 {
		out.PolicyDrift.BriefRowState = briefDegraded(fmt.Sprintf("%d sibling-policy approval pin(s) do not match", len(out.PolicyDrift.Rows)))
	}
	out.BriefRowState = briefSectionState("risk and limits", out.Capital.BriefRowState, out.Latch.BriefRowState,
		out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState)
	return out
}

func (s *Server) composeBriefProcess(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, recon *rpc.ReconResult, rules *rpc.RulesResult, now time.Time) rpc.BriefProcessSection {
	return s.composeBriefProcessForAuthority(policy, constitution, recon, rules, s.currentNudgeAuthority(now), now)
}

func (s *Server) composeBriefProcessForAuthority(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, recon *rpc.ReconResult, rules *rpc.RulesResult, authority nudgeAuthorityState, now time.Time) rpc.BriefProcessSection {
	out := rpc.BriefProcessSection{}
	if policy == nil {
		out.Reconcile.BriefRowState = briefUnavailable("risk policy unavailable")
	} else {
		capital := policy.Capital
		out.Reconcile = rpc.BriefReconcileRow{BriefRowState: briefOK("reconcile evidence and shared constitution clock"),
			LastReconciledAt: capital.LastReconciledAt, Source: capital.LastReconcileSource}
		clock := s.riskCapital.UnreconciledClock(constitution, now)
		if !clock.Approved {
			out.Reconcile.BriefRowState = briefDegraded("capital.max_unreconciled_days is unapproved")
		} else if capital.LastReconciledAt.IsZero() {
			out.Reconcile.BriefRowState = briefDegraded("no reconcile evidence has been recorded")
		} else {
			out.Reconcile.Deadline, out.Reconcile.DaysRemaining = clock.Deadline, clock.DaysRemaining
			if clock.Stale {
				out.Reconcile.BriefRowState = briefDegraded("reconcile evidence is past its declared horizon")
			}
		}
	}
	if recon == nil {
		out.AutoExtend.BriefRowState = briefUnavailable("reconciliation report unavailable")
		out.OneTap.BriefRowState = briefUnavailable("reconciliation report unavailable")
	} else {
		out.AutoExtend = rpc.BriefAutoExtendRow{BriefRowState: briefOK("no automatic extension recorded"),
			ReportID: recon.LastAutoExtendReportID, At: recon.LastAutoExtendedAt}
		if recon.LastAutoExtendReportID != "" {
			out.AutoExtend.Detail = "latest clean-report automatic extension"
		}
		_, blockers := s.reconcileReportAssessment(recon.ReportID)
		out.OneTap = rpc.BriefOneTapRow{BriefRowState: briefOK("current report is signable"), ReportID: recon.ReportID,
			Signable: len(blockers) == 0, Blockers: blockers}
		if len(blockers) > 0 {
			out.OneTap.BriefRowState = briefDegraded("current report is not signable")
		}
	}
	out.RulesDelta = s.briefRulesDelta(rules)
	out.Artefacts = composeBriefArtefacts(policy, constitution, now)
	if constitution != nil && constitution.PolicyVersion >= 4 {
		evaluation, completion := s.governanceMonthlyPulseForAuthority(authority, constitution, recon, now)
		if evaluation.Status == risk.MonthlyPulseStatusBlocked {
			if recovery := s.governanceMonthlyPulseForRenderRecovery(authority, constitution, now); recovery.Status != "" {
				evaluation = recovery
			}
		}
		out.MonthlyPulse = &rpc.BriefMonthlyPulseRow{
			Status: evaluation.Status, Month: evaluation.Month, DueAt: evaluation.DueAt,
		}
		if completion != nil && evaluation.Status == risk.MonthlyPulseStatusCompleted {
			out.MonthlyPulse.CompletedAt = completion.CompletedAt
		}
	}
	out.BriefRowState = briefProcessSectionState(out)
	return out
}

func (s *Server) briefMonthlyPulse(constitution *risk.Constitution, _ *rpc.RiskPolicyResult, report *rpc.ReconResult, now time.Time) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	return s.governanceMonthlyPulse(constitution, report, now)
}

func briefProcessSectionState(process rpc.BriefProcessSection) rpc.BriefRowState {
	rows := []rpc.BriefRowState{
		process.Reconcile.BriefRowState, process.AutoExtend.BriefRowState,
		process.OneTap.BriefRowState, process.RulesDelta.BriefRowState,
		process.Artefacts.BriefRowState,
	}
	if process.MonthlyPulse != nil {
		switch process.MonthlyPulse.Status {
		case rpc.BriefMonthlyPulseNotDue, rpc.BriefMonthlyPulseCompleted:
			rows = append(rows, briefOK("monthly pulse is current"))
		case rpc.BriefMonthlyPulseDue:
			rows = append(rows, briefDegraded("monthly pulse is due"))
		default:
			rows = append(rows, briefDegraded("monthly pulse is blocked by policy evidence"))
		}
	}
	return briefSectionState("process", rows...)
}

func policyPinsReady(inventory []rpc.PolicyPinStatus) bool {
	return policyPinsReadable(inventory, true)
}

func policyPinsReadable(inventory []rpc.PolicyPinStatus, requireMatch bool) bool {
	if len(inventory) != 3 {
		return false
	}
	want := map[string]bool{"rulebook": false, "protection": false, "canary": false}
	for _, pin := range inventory {
		statusReadable := pin.Status == "match" || (!requireMatch && pin.Status == "drift")
		if _, known := want[pin.Policy]; !known || want[pin.Policy] || !statusReadable || pin.PinnedID == "" || pin.PinnedVersion == "" || pin.LiveID == "" || pin.LiveVersion == "" {
			return false
		}
		want[pin.Policy] = true
	}
	return true
}

func (s *Server) briefRulesDelta(current *rpc.RulesResult) rpc.BriefRulesDeltaRow {
	row := rpc.BriefRulesDeltaRow{BriefRowState: briefOK("no rule status changes since the last stamped brief")}
	if current == nil {
		row.BriefRowState = briefUnavailable("rulebook snapshot unavailable")
		return row
	}
	if current.PolicyFingerprint != nil {
		row.CurrentFingerprint = current.PolicyFingerprint.Key
	}
	baseline, ok := s.briefState.latestBaseline()
	if !ok {
		row.BriefRowState = briefDegraded("no delta baseline yet")
		return row
	}
	row.BaselineAt, row.BaselineFingerprint = baseline.At, baseline.RulebookFingerprint
	row.RulebookFingerprintChanged = baseline.RulebookFingerprint != row.CurrentFingerprint
	base := make(map[string]string, len(baseline.Rules))
	for _, r := range baseline.Rules {
		base[r.ID] = r.Status
	}
	seen := map[string]bool{}
	for _, r := range current.Rules {
		seen[r.ID] = true
		old, exists := base[r.ID]
		switch {
		case !exists:
			row.Added = append(row.Added, r.ID)
		case old != r.Status:
			row.Transitions = append(row.Transitions, rpc.BriefRuleTransition{RuleID: r.ID, From: old, To: r.Status})
		}
	}
	for id := range base {
		if !seen[id] {
			row.Removed = append(row.Removed, id)
		}
	}
	slices.Sort(row.Added)
	slices.Sort(row.Removed)
	if row.RulebookFingerprintChanged || len(row.Transitions)+len(row.Added)+len(row.Removed) > 0 {
		row.BriefRowState = briefDegraded("rulebook changed since the last stamped brief")
	}
	return row
}

func composeBriefArtefacts(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) rpc.BriefArtefactsRow {
	row := rpc.BriefArtefactsRow{BriefRowState: briefOK("declared cadence completion; no overdue judgment")}
	classes := map[string]string{}
	if constitution != nil {
		classes[rpc.BriefKindMorning] = constitution.Cadence.Morning.Class
		classes[rpc.BriefKindEOD] = constitution.Cadence.EOD.Class
		classes["weekly"] = constitution.Cadence.Weekly.Class
	}
	for _, kind := range []string{rpc.BriefKindMorning, rpc.BriefKindEOD, "weekly"} {
		cadence := "daily"
		if kind == "weekly" {
			cadence = "weekly"
		}
		item := rpc.BriefArtefact{BriefRowState: briefUnavailable("artefact cadence is not declared"), Kind: kind, Cadence: cadence, Declared: classes[kind] != ""}
		if item.Declared {
			item.BriefRowState = briefOK("declared; completion state only, with no overdue judgment")
		}
		if rec, ok := artefactCompletedInPeriod(policyCadence(policy), kind, now); ok {
			item.Completed, item.CompletedAt = true, rec.CompletedAt
			item.Declared = rec.Class != ""
			item.BriefRowState = briefOK("completed in the current cadence period")
		}
		row.Rows = append(row.Rows, item)
	}
	declared := 0
	for _, item := range row.Rows {
		if item.Declared {
			declared++
		}
	}
	if declared == 0 {
		row.BriefRowState = briefUnavailable("cadence artefacts are unapproved or undeclared")
	} else if declared < len(row.Rows) {
		row.BriefRowState = briefDegraded("one or more cadence artefacts are unapproved or undeclared")
	}
	return row
}

func policyCadence(policy *rpc.RiskPolicyResult) []rpc.ArtefactRecord {
	if policy == nil {
		return nil
	}
	return policy.Cadence
}

func briefStampTarget(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) (string, string) {
	if policy == nil || constitution == nil {
		return "", "daily artefact cadence is unapproved"
	}
	declared := map[string]bool{
		rpc.BriefKindMorning: constitution.Cadence.Morning.Class != "",
		rpc.BriefKindEOD:     constitution.Cadence.EOD.Class != "",
	}
	for _, kind := range []string{rpc.BriefKindMorning, rpc.BriefKindEOD} {
		if !declared[kind] {
			return "", kind + " artefact cadence is unapproved"
		}
		if _, done := artefactCompletedInPeriod(policy.Cadence, kind, now); !done {
			return kind, ""
		}
	}
	return "", "both daily artefacts complete"
}

func (s *Server) briefStampTarget(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) (string, string) {
	if constitution != nil && constitution.PolicyVersion >= 4 {
		monthly, _ := s.briefMonthlyPulse(constitution, policy, s.buildReconReport(), now)
		switch monthly.Status {
		case risk.MonthlyPulseStatusDue:
			return rpc.BriefKindMonthly, ""
		case risk.MonthlyPulseStatusBlocked:
			return "", "monthly pulse is blocked by cadence or sibling-pin evidence"
		}
	}
	return briefStampTarget(policy, constitution, now)
}

func artefactCompletedInPeriod(records []rpc.ArtefactRecord, kind string, now time.Time) (rpc.ArtefactRecord, bool) {
	for _, rec := range records {
		if rec.Artefact != kind || rec.CompletedAt.IsZero() {
			continue
		}
		if kind == "weekly" {
			y1, w1 := rec.CompletedAt.In(time.Local).ISOWeek()
			y2, w2 := now.In(time.Local).ISOWeek()
			if y1 == y2 && w1 == w2 {
				return rec, true
			}
		} else if sameLocalDay(rec.CompletedAt, now) {
			return rec, true
		}
	}
	return rpc.ArtefactRecord{}, false
}

func sameLocalDay(a, b time.Time) bool { return localDay(a) == localDay(b) }
func localDay(t time.Time) string      { return t.In(time.Local).Format(time.DateOnly) }

func briefContentFingerprint(res *rpc.BriefResult) string {
	projection := struct {
		Market     rpc.BriefMarketSection
		Calendar   rpc.BriefCalendarSection
		Portfolio  rpc.BriefPortfolioSection
		RiskLimits rpc.BriefRiskSection
		Process    rpc.BriefProcessSection
	}{res.Market, res.Calendar, res.Portfolio, res.RiskLimits, res.Process}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func briefOK(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusOK, Detail: nonEmptyString(detail, "available")}
}
func briefDegraded(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusDegraded, Detail: nonEmptyString(detail, "degraded")}
}
func briefUnavailable(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusUnavailable, Detail: nonEmptyString(detail, "unavailable")}
}

func briefSectionState(name string, rows ...rpc.BriefRowState) rpc.BriefRowState {
	ok, unavailable := 0, 0
	for _, row := range rows {
		switch row.Status {
		case rpc.BriefStatusOK:
			ok++
		case rpc.BriefStatusUnavailable:
			unavailable++
		}
	}
	if len(rows) > 0 && unavailable == len(rows) {
		return briefUnavailable(name + " section unavailable")
	}
	if ok != len(rows) {
		return briefDegraded(name + " section contains disclosed degraded or unavailable rows")
	}
	return briefOK(name + " section complete")
}

func briefSourceHealthDegraded(rows []rpc.SourceHealth) bool {
	for _, row := range rows {
		if row.Status != "ok" {
			return true
		}
	}
	return false
}

func mapKeysSorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		if key != "" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}
