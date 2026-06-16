package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	proposalEventFileVersion = 1
	proposalOrderSource      = "trade_proposals"
)

type proposalEngine struct {
	mu      sync.Mutex
	server  *Server
	store   *proposalStore
	cadence time.Duration
	now     func() time.Time
	// scope resolves the connected broker session identity. Test seam;
	// nil falls back to server.currentBrokerStateScope.
	scope    func() brokerStateScope
	snapshot rpc.TradeProposalSnapshot
	// ignored is keyed by scopedIgnoreKey (account|mode|proposal key):
	// proposal keys hash contract identity only, so an unscoped set would
	// suppress the same contract across paper/live sessions.
	ignored map[string]struct{}
	// refreshFailStreak counts consecutive refreshes that ended on a
	// transient session blocker (see proposalRefreshTransient), with the
	// streak start time and the latest failure's blocker codes alongside.
	// Observability only — Run's backoff keeps its own counter — but
	// without it a preserved-snapshot outage is invisible: the failures
	// return err == nil and the served as_of silently freezes (observed
	// 2026-06-11: 44 minutes stale, zero log lines).
	refreshFailStreak int
	refreshFailSince  time.Time
	refreshFailCodes  []string
	// kick wakes Run for an immediate refresh (gateway reconnect). Lazily
	// created under mu by kickCh so tests constructing engines directly
	// need no extra setup. Buffered: senders never block.
	kick chan struct{}
}

type proposalStore struct {
	currentPath string
	eventsPath  string
	mu          sync.Mutex
}

type proposalEvent struct {
	Version            int                                 `json:"version"`
	At                 time.Time                           `json:"at"`
	Type               string                              `json:"type"`
	Key                string                              `json:"key,omitempty"`
	Revision           string                              `json:"revision,omitempty"`
	Bucket             string                              `json:"bucket,omitempty"`
	AccountID          string                              `json:"account_id,omitempty"`
	PolicyID           string                              `json:"policy_id,omitempty"`
	PolicyVersion      int                                 `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                     `json:"policy_fingerprint,omitzero"`
	PreviewTokenID     string                              `json:"preview_token_id,omitempty"`
	OrderRef           string                              `json:"order_ref,omitempty"`
	Message            string                              `json:"message,omitempty"`
	Reason             string                              `json:"reason,omitempty"`
	SourceFingerprints rpc.TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
}

func (s *Server) installProposalEngine() {
	current, err := defaultTradingStatePath("trade-proposals-current.json")
	if err != nil {
		s.warnf("trade proposals: resolve current path: %v", err)
		return
	}
	events, err := defaultTradingStatePath("trade-proposals.jsonl")
	if err != nil {
		s.warnf("trade proposals: resolve events path: %v", err)
		return
	}
	e := &proposalEngine{
		server:  s,
		store:   &proposalStore{currentPath: current, eventsPath: events},
		cadence: s.cfg.AutoTrade.WithDefaults().ProposalCadenceDuration(),
		now:     s.now,
		ignored: map[string]struct{}{},
	}
	if snap, err := e.store.LoadCurrent(); err == nil && snap.Kind != "" {
		if proposalSnapshotAdoptable(snap) {
			snap.LoadedFromState = true
			e.snapshot = snap
		} else {
			// Legacy/unscoped snapshot (e.g. account_id "All" from the
			// pre-v2 era): fail closed and let the first refresh
			// regenerate for the connected session.
			s.warnf("trade proposals: ignoring persisted snapshot without a concrete account/mode scope (account %q mode %q); regenerating on first refresh", snap.AccountID, snap.AccountMode)
		}
	}
	s.tradeProposals = e
}

// proposalRefreshRetryBase is the first quick-retry delay after a refresh
// that failed on a transient session condition (gateway still connecting,
// account/positions fetch failure, no concrete account identity yet). It
// doubles per consecutive transient failure and caps at the configured
// cadence. Without it the startup refresh races the gateway connect and
// the cached "ibkr connection unavailable" blocker is served for a full
// cadence (observed 2026-06-11: the SPA protection panel sat on the error
// for 15 minutes after every `ibkr restart`).
const proposalRefreshRetryBase = 30 * time.Second

// proposalRefreshBackoffCap bounds the sustained-failure retry interval
// independently of the cadence. With a 2m cadence, capping failure waits
// at the cadence would mean a blocked attempt (and its warn line) every
// 2 minutes for the whole length of a gateway outage; capping at 15m keeps
// outage logs as quiet as the old 15m-cadence behavior. Post-outage
// recovery latency does not ride on this cap — the gateway reconnect kicks
// the loop directly (see Kick).
const proposalRefreshBackoffCap = 15 * time.Minute

func (e *proposalEngine) Run(ctx context.Context) {
	if e == nil {
		return
	}
	failures := 0
	for {
		snap, err := e.Refresh(ctx, false)
		if err != nil || proposalRefreshTransient(snap) {
			failures++
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kickCh():
			// A fresh gateway handshake invalidates the escalated wait:
			// restart the quick-retry ladder so a transient failure on the
			// immediate post-reconnect refresh waits 30s, not the
			// accumulated outage backoff. The logging streak in
			// noteRefreshOutcome is deliberately untouched so the
			// "recovered after N blocked attempts" line still closes the
			// outage trail.
			failures = 0
		case <-time.After(proposalRefreshWait(e.cadence, failures)):
		}
	}
}

// Kick wakes Run for an immediate refresh, dropping the wake when one is
// already pending. Called from postConnectSetup after RequestAccountUpdates
// so a gateway reconnect refreshes the panel within seconds instead of
// waiting out a backed-off timer (observed 2026-06-12: gateway back at
// 10:53, panel stale until the 10:59:15 scheduled attempt).
func (e *proposalEngine) Kick() {
	if e == nil {
		return
	}
	select {
	case e.kickCh() <- struct{}{}:
	default:
	}
}

func (e *proposalEngine) kickCh() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kick == nil {
		e.kick = make(chan struct{}, 1)
	}
	return e.kick
}

// proposalRefreshWait returns the pause before the next automatic refresh:
// the full cadence after a clean refresh, an escalating 30s/1m/2m/…
// backoff while refreshes keep failing on transient session conditions,
// capped at proposalRefreshBackoffCap (or the cadence when that is longer,
// so slow-cadence overrides never retry faster on failure than on success).
// The wait <= 0 branch guards shift overflow on absurd streaks, mirroring
// gammaRetryBackoff.
func proposalRefreshWait(cadence time.Duration, failures int) time.Duration {
	if failures <= 0 || cadence <= proposalRefreshRetryBase {
		return cadence
	}
	ceil := max(cadence, proposalRefreshBackoffCap)
	wait := proposalRefreshRetryBase << (failures - 1)
	if wait <= 0 || wait > ceil {
		return ceil
	}
	return wait
}

// proposalPositionsUnprimed reports whether an empty positions list
// contradicts the account summary. A connected session serves an empty
// position cache (no error) until the account-updates portfolio burst
// lands; when the summary reports gross position value, the empty list is
// the unprimed stream, not a flat book — generating "no proposals" from
// it would replace a last-good snapshot with a silently wrong empty
// panel. Same heuristic as the connector's maybeResubscribeAccountUpdates
// self-heal. A genuinely flat account (gross position value 0) never
// trips this, so an emptied book still converges to an empty panel.
func proposalPositionsUnprimed(pos *rpc.PositionsResult, acct *rpc.AccountResult) bool {
	if pos == nil || acct == nil {
		return false
	}
	return len(pos.Stocks) == 0 && len(pos.Options) == 0 && acct.GrossPositionValue > 0
}

// proposalRefreshTransient reports whether the installed snapshot is
// blocked on a condition the next broker heartbeat can clear (connection
// not yet up, session identity not yet concrete, session switch still
// settling). Refresh failures that preserve a last-good snapshot return
// err == nil but still carry these blocker codes, so the codes are the
// signal, not the returned error. Persistent variants (a deliberately
// un-pinned data-only gateway stays account_identity_unscoped forever)
// converge to the cadence cap, where a scope-blocked refresh is one cheap
// no-broker-call pass.
func proposalRefreshTransient(snap rpc.TradeProposalSnapshot) bool {
	for _, b := range snap.Blockers {
		switch b.Code {
		case "account_identity_unscoped", "account_unavailable", "positions_unavailable", "positions_pending", "proposal_scope_mismatch":
			return true
		}
	}
	return false
}

func (e *proposalEngine) Snapshot(show bool) rpc.TradeProposalSnapshot {
	if e == nil {
		return emptyProposalSnapshot(time.Now().UTC())
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" {
		snap = emptyProposalSnapshot(e.clock())
	}
	// Serve guard: proposals are generated from one account/mode session
	// and must never surface under another (paper proposals shown on a
	// live session was the originating incident). Proposal-free shells
	// carry session-independent blockers and pass through unchanged.
	if len(snap.Proposals) > 0 {
		scope := e.currentScope()
		if blockers := proposalScopeBlockers(snap.AccountID, snap.AccountMode, scope); len(blockers) > 0 {
			shell := emptyProposalSnapshot(e.clock())
			if brokerScopeConcrete(scope) {
				shell.AccountID = scope.Account
				shell.AccountMode = scope.Mode
			}
			shell.Blockers = blockers
			return shell
		}
	}
	if show {
		e.appendShownEvents(snap)
	}
	return snap
}

// proposalRefreshWarnStreak is how many consecutive transient-failed
// refreshes stay quiet before the engine starts warning. The first
// refreshes after a daemon start routinely race the gateway connect and
// self-heal within the 30s/1m quick retries; warning from the third
// failure on keeps boot logs clean while a real outage surfaces within
// a few minutes.
const proposalRefreshWarnStreak = 3

func (e *proposalEngine) Refresh(ctx context.Context, show bool) (rpc.TradeProposalSnapshot, error) {
	snap, err := e.refresh(ctx, show)
	e.noteRefreshOutcome(snap, err)
	return snap, err
}

// noteRefreshOutcome advances the transient-failure streak after every
// refresh, regardless of caller, and emits the throttled log trail.
// Transient failures preserve the last-good snapshot and return err == nil
// — the blocker codes are the only signal — so this is where a stalled
// panel becomes diagnosable. Quiet below proposalRefreshWarnStreak, then
// one warn per failed attempt: Run's backoff paces those at 30s/1m/2m/…
// capped at the cadence, so a persistent outage logs once per escalation
// and then once per cadence, not once per poll. One info line closes the
// streak when a refresh finally lands.
func (e *proposalEngine) noteRefreshOutcome(snap rpc.TradeProposalSnapshot, err error) {
	failed := err != nil || proposalRefreshTransient(snap)
	now := e.clock()
	e.mu.Lock()
	if !failed {
		streak, since := e.refreshFailStreak, e.refreshFailSince
		e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes = 0, time.Time{}, nil
		e.mu.Unlock()
		if streak >= proposalRefreshWarnStreak && e.server != nil {
			e.server.infof("trade proposals: refresh recovered after %d blocked attempts over %s", streak, now.Sub(since).Round(time.Second))
		}
		return
	}
	e.refreshFailStreak++
	if e.refreshFailStreak == 1 {
		e.refreshFailSince = now
	}
	e.refreshFailCodes = proposalBlockerCodes(snap, err)
	streak, since, codes := e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes
	e.mu.Unlock()
	if streak < proposalRefreshWarnStreak || e.server == nil {
		return
	}
	e.server.warnf("trade proposals: refresh blocked %d consecutive times over %s (codes: %s); serving snapshot as_of %s (%s old)",
		streak, now.Sub(since).Round(time.Second), strings.Join(codes, ","),
		snap.AsOf.Format(time.RFC3339), now.Sub(snap.AsOf).Round(time.Second))
}

// proposalBlockerCodes flattens the installed snapshot's blocker codes for
// the refresh-streak trail; the raw fetch error stands in when a failure
// path produced no blockers.
func proposalBlockerCodes(snap rpc.TradeProposalSnapshot, err error) []string {
	var out []string
	for _, b := range snap.Blockers {
		if b.Code != "" && !slices.Contains(out, b.Code) {
			out = append(out, b.Code)
		}
	}
	if len(out) == 0 && err != nil {
		out = append(out, err.Error())
	}
	return out
}

// proposalRefreshHealth is the engine's refresh-streak view for the
// status.health proposals subsystem row.
type proposalRefreshHealth struct {
	Streak     int
	Since      time.Time
	Codes      []string
	ServedAsOf time.Time
}

// RefreshHealth reports the current transient-failure streak and the as_of
// of the snapshot being served. Zero streak means the last refresh
// installed cleanly.
func (e *proposalEngine) RefreshHealth() proposalRefreshHealth {
	if e == nil {
		return proposalRefreshHealth{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return proposalRefreshHealth{
		Streak:     e.refreshFailStreak,
		Since:      e.refreshFailSince,
		Codes:      append([]string(nil), e.refreshFailCodes...),
		ServedAsOf: e.snapshot.AsOf,
	}
}

func (e *proposalEngine) refresh(ctx context.Context, show bool) (rpc.TradeProposalSnapshot, error) {
	now := e.clock()
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	autoStatus := e.server.autoTradeStatus()
	if !cfg.ProposalsEnabledResolved() {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = autoStatus.Policy
		snap.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
		e.installSnapshot(snap, show)
		return snap, nil
	}
	policy, policyStatus := e.server.protectionPolicies.Active()
	if policyStatus.Status == rpc.ProtectionPolicyStatusDrift || policyStatus.Status == rpc.ProtectionPolicyStatusError {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = append([]rpc.TradingBlocker(nil), policyStatus.Blockers...)
		e.installSnapshot(snap, show)
		e.appendEvent(proposalEvent{At: now, Type: "policy-" + policyStatus.Status, PolicyID: policyStatus.PolicyID, PolicyVersion: policyStatus.PolicyVersion, PolicyFingerprint: policyStatus.Fingerprint, Message: policyStatus.Message})
		return snap, nil
	}
	// Bind the refresh to the connected session identity before touching
	// any account data. The aggregate "All" (or an empty / multi-account
	// managedAccounts list, or an unknown paper/live mode) is not an
	// account identity — proposals scoped to it would survive paper/live
	// session switches, which is exactly the leak this gate prevents.
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = []rpc.TradingBlocker{proposalScopeUnscopedBlocker(scope)}
		e.installSnapshot(snap, show)
		return snap, nil
	}
	acct, err := e.server.handleAccountSummary(ctx)
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "account_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		e.installSnapshot(snap, show)
		return snap, err
	}
	pos, err := e.server.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		e.installSnapshot(snap, show)
		return snap, err
	}
	if proposalPositionsUnprimed(pos, acct) {
		blockers := []rpc.TradingBlocker{{Code: "positions_pending", Message: "portfolio stream not yet primed; account summary reports open positions"}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		e.installSnapshot(snap, show)
		return snap, nil
	}
	accountFP := rpc.BuildAccountFingerprint(acct)
	positionsFP := rpc.BuildPositionsFingerprint(pos, acct.NetLiquidation)
	sources := rpc.TradeProposalSourceFingerprints{Account: &accountFP, Positions: &positionsFP}
	if fp, ok := e.regimeFingerprint(ctx); ok {
		sources.Regime = &fp
	}
	marketEvents := e.marketEventsSnapshot(ctx, pos)
	if marketEvents != nil {
		fp := marketEvents.Fingerprint
		if fp.Key == "" {
			fp = rpc.BuildMarketEventsFingerprint(marketEvents)
		}
		sources.MarketEvents = &fp
	}
	proposals := e.generate(policy, policyStatus, pos, sources, marketEvents, scope, now)
	slices.SortStableFunc(proposals, func(a, b rpc.TradeProposal) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	revision := proposalRevision(policyStatus.Fingerprint, sources, scope, proposals)
	for i := range proposals {
		proposals[i].Rank = i + 1
		proposals[i].Revision = revision
	}
	snap := rpc.TradeProposalSnapshot{
		Kind:               rpc.TradeProposalSnapshotKind,
		SchemaVersion:      rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:               now,
		Revision:           revision,
		AccountID:          scope.Account,
		AccountMode:        scope.Mode,
		PolicyID:           policy.PolicyID,
		PolicyVersion:      policy.PolicyVersion,
		PolicyFingerprint:  policyStatus.Fingerprint,
		PolicyStatus:       policyStatus,
		AutoTrade:          autoStatus,
		Trading:            autoStatus.Trading,
		SourceFingerprints: sources,
		MarketEvents:       marketEvents,
		Proposals:          proposals,
		Counts:             proposalCounts(proposals),
	}
	return e.installScoped(snap, scope, show), nil
}

// installScoped re-resolves the broker scope immediately before publishing a
// generated snapshot. The un-pinned gateway can reconnect to a different TWS
// session while Refresh fetches account/position data; installing that data
// under the scope resolved at refresh start would persist proposals labeled
// with one session's identity but built from another's positions. Fail
// closed with a proposal-free shell instead.
func (e *proposalEngine) installScoped(snap rpc.TradeProposalSnapshot, scope brokerStateScope, show bool) rpc.TradeProposalSnapshot {
	if current := e.currentScope(); !sameBrokerScope(current, scope) {
		shell := emptyProposalSnapshot(snap.AsOf)
		shell.AutoTrade = snap.AutoTrade
		shell.PolicyStatus = snap.PolicyStatus
		shell.Blockers = proposalScopeBlockers(scope.Account, scope.Mode, current)
		e.installSnapshot(shell, show)
		return shell
	}
	e.installSnapshot(snap, show)
	return snap
}

func (e *proposalEngine) generate(policy protectionPolicy, status rpc.ProtectionPolicyStatus, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, scope brokerStateScope, now time.Time) []rpc.TradeProposal {
	var out []rpc.TradeProposal
	if policy.Buckets.ThetaHygiene.Enabled {
		for _, row := range pos.Options {
			if p, ok := thetaProposal(policy, status, row, sources, now); ok {
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(scope, p.Key) {
					out = append(out, p)
				}
			}
		}
	}
	if policy.Buckets.RiskReduction.Enabled {
		for _, group := range pos.ByUnderlying {
			if p, ok := riskReductionProposal(policy, status, group, sources, now); ok {
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(scope, p.Key) {
					out = append(out, p)
				}
			}
		}
	}
	if policy.Buckets.TrailingStop.Enabled {
		stockEnabled := true
		if e != nil && e.server != nil {
			stockEnabled = e.server.stockProtectionEnabled()
		}
		if policy.Buckets.TrailingStop.StockETF.Enabled {
			for _, row := range pos.Stocks {
				if p, ok := trailingStopStockProposal(policy, status, row, sources, now, stockEnabled, e.resolveRowMinTick(row)); ok {
					applyMarketEventFlagsToProposal(&p, marketEvents)
					for _, b := range e.duplicateProtectiveBlockers(p) {
						proposalBlock(&p, b.Code, b.Message)
					}
					if !e.isIgnored(scope, p.Key) {
						out = append(out, p)
					}
				}
			}
		}
		if policy.Buckets.TrailingStop.Options.Enabled {
			multiLegBySymbol := multiLegOptionSymbols(pos.Options)
			for _, row := range pos.Options {
				if p, ok := trailingStopOptionProposal(policy, status, row, sources, now, multiLegBySymbol[strings.ToUpper(strings.TrimSpace(row.Symbol))], e.resolveRowMinTick(row)); ok {
					applyMarketEventFlagsToProposal(&p, marketEvents)
					if !e.isIgnored(scope, p.Key) {
						out = append(out, p)
					}
				}
			}
		}
	}
	return out
}

func (e *proposalEngine) marketEventsSnapshot(ctx context.Context, pos *rpc.PositionsResult) *rpc.MarketEventsResult {
	if e == nil || e.server == nil {
		return nil
	}
	symbols := marketEventSymbolsFromPositions(pos)
	if len(symbols) == 0 {
		return nil
	}
	eventsCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res := e.server.marketEventsForSymbols(eventsCtx, symbols)
	return &res
}

func (e *proposalEngine) regimeFingerprint(ctx context.Context) (rpc.Fingerprint, bool) {
	if e == nil || e.server == nil {
		return rpc.Fingerprint{}, false
	}
	regimeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	regime, err := e.server.handleRegimeSnapshot(regimeCtx, &rpc.Request{})
	if err != nil || regime == nil {
		return rpc.Fingerprint{}, false
	}
	fp := regime.Fingerprint
	if fp.Key == "" {
		fp = rpc.BuildRegimeFingerprint(regime)
	}
	return fp, fp.Key != ""
}

func thetaProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 || row.Theta == nil {
		return rpc.TradeProposal{}, false
	}
	dte, ok := optionDTE(row.Expiry, now)
	if !ok || dte > policy.Buckets.ThetaHygiene.MaxDTE {
		return rpc.TradeProposal{}, false
	}
	thetaPerDay := math.Abs(*row.Theta * row.Quantity * float64(max(row.Multiplier, 1)))
	if thetaPerDay < policy.Buckets.ThetaHygiene.MinAbsThetaPerDay {
		return rpc.TradeProposal{}, false
	}
	qty := int(math.Ceil(math.Abs(row.Quantity)))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketThetaHygiene, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("option expires in %d DTE with %.2f/day theta exposure", dte, thetaPerDay))
	p.ThetaPerDay = thetaPerDay
	p.Score = thetaPerDay + float64(max(policy.Buckets.ThetaHygiene.MaxDTE-dte, 0))
	p.Details = []string{fmt.Sprintf("dte=%d", dte)}
	if row.SpreadPct != nil && *row.SpreadPct > policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid {
		p.State = rpc.TradeProposalStateBlocked
		p.Blockers = []rpc.TradingBlocker{{Code: "wide_spread", Message: fmt.Sprintf("option spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid)}}
	}
	return p, true
}

func riskReductionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, group rpc.PositionGroup, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if group.GroupMarketValuePctNLV == nil || math.Abs(*group.GroupMarketValuePctNLV) <= policy.Buckets.RiskReduction.SingleNameTargetPctNLV {
		return rpc.TradeProposal{}, false
	}
	var row rpc.PositionView
	if group.Stock != nil && group.Stock.Quantity != 0 {
		row = *group.Stock
	} else {
		for _, opt := range group.Options {
			if opt.Quantity != 0 {
				row = opt
				break
			}
		}
	}
	if row.Symbol == "" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	if !proposalSupportedSecType(row.SecType) {
		return rpc.TradeProposal{}, false
	}
	pct := math.Abs(*group.GroupMarketValuePctNLV)
	excessPct := pct - policy.Buckets.RiskReduction.SingleNameTargetPctNLV
	excessNotional := math.Abs(groupMarketValueOrderValue(group)) * (excessPct / pct)
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	maxQty := int(math.Ceil(math.Abs(row.Quantity)))
	qty := maxQty
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark > 0 {
		mult := float64(max(row.Multiplier, 1))
		qty = int(math.Ceil(excessNotional / (mark * mult)))
		maxByNotional := int(math.Max(1, math.Floor(policy.Buckets.RiskReduction.MaxOrderNotional/(mark*mult))))
		qty = min(qty, maxByNotional)
	}
	qty = max(1, min(qty, maxQty))
	effect := rpc.OrderPositionEffectReduce
	if qty == maxQty {
		effect = rpc.OrderPositionEffectClose
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketRiskReduction, row, action, qty, effect, fmt.Sprintf("%s is %.1f%% of NLV, above %.1f%% target", group.Underlying, pct, policy.Buckets.RiskReduction.SingleNameTargetPctNLV))
	p.MarketValuePctNLV = cloneFloat64Ptr(group.GroupMarketValuePctNLV)
	p.RiskExcessNotional = excessNotional
	p.RiskExcessCurrency = p.Contract.Currency
	p.Score = pct
	return p, true
}

func trailingStopStockProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, stockProtectionEnabled bool, minTick float64) (rpc.TradeProposal, bool) {
	secType := strings.ToUpper(strings.TrimSpace(row.SecType))
	if secType != rpc.SecTypeStock && secType != "STK" && secType != "ETF" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.StockETF
	qty, fractionalRemainder := closeReduceQuantity(row.Quantity)
	if qty == 0 {
		return rpc.TradeProposal{}, false
	}
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	reference := trailingStopReferencePrice(row, action)
	if reference <= 0 && stockPositionLooksInactive(row) {
		return rpc.TradeProposal{}, false
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketTrailingStop, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("broker-side trailing stop at %.1f%% below/above the instrument price", cfg.DefaultPct))
	p.Contract.MinTick = minTick
	p.TIF = policy.Buckets.TrailingStop.effectiveTIF()
	if fractionalRemainder > 0 {
		p.Details = append(p.Details, fmt.Sprintf("fractional %.4g shares stay unprotected under the integer order path", fractionalRemainder))
	}
	applyTrailToProposal(&p, cfg.OrderType, cfg.DefaultPct, reference, action, cfg.LimitOffsetAbs)
	p.Score = math.Abs(row.MarketValue)
	p.Details = append(p.Details, fmt.Sprintf("trail=%.1f%%", cfg.DefaultPct))
	p.Details = append(p.Details, trailingStopTIFDetail(p.TIF, false))
	if !stockProtectionEnabled {
		proposalBlock(&p, "stock_protection_disabled", "stock/ETF protection is disabled in platform settings")
	}
	if reference <= 0 {
		proposalBlock(&p, "missing_reference_price", "stock/ETF trailing stop requires bid/ask or a positive portfolio mark")
	}
	if row.SpreadPct != nil && *row.SpreadPct > cfg.MaxSpreadPctOfMid {
		proposalBlock(&p, "wide_spread", fmt.Sprintf("stock/ETF spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, cfg.MaxSpreadPctOfMid))
	}
	return p, true
}

func trailingStopOptionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, multiLeg bool, minTick float64) (rpc.TradeProposal, bool) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.Options
	qty := int(math.Ceil(math.Abs(row.Quantity)))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	reference, spreadAbs, ok := optionTrailReference(row, action)
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketTrailingStop, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("broker-side option premium trailing stop at %.1f%%", cfg.DefaultPct))
	p.Contract.MinTick = minTick
	p.TIF = policy.Buckets.TrailingStop.effectiveTIF()
	applyTrailToProposal(&p, cfg.OrderType, cfg.DefaultPct, reference, action, cfg.LimitOffsetAbs)
	p.Score = math.Abs(row.MarketValue)
	p.Details = append(p.Details, fmt.Sprintf("premium trail=%.1f%%", cfg.DefaultPct))
	p.Details = append(p.Details, trailingStopTIFDetail(p.TIF, true))
	if row.Quantity < 0 && !cfg.AllowShortProfitTrail {
		proposalBlock(&p, "short_option_trail_disabled", "short-option trailing stops require explicit buy-to-close profit-trail policy")
	}
	if multiLeg {
		proposalBlock(&p, "multi_leg_option_trail_unsupported", "broker-side option trails are supported for single-leg option positions only in V1")
	}
	if !ok {
		proposalBlock(&p, "missing_option_bid_ask", "option trailing stop requires live two-sided option bid/ask")
	}
	if row.Stale {
		proposalBlock(&p, "stale_quote", "option trailing stop requires a fresh live option quote")
	}
	if row.SessionContext == nil || !row.SessionContext.IsOpen {
		proposalBlock(&p, "option_rth_closed", "option trailing stop proposals require the regular option session to be open")
	}
	if row.SpreadPct != nil && *row.SpreadPct > cfg.MaxSpreadPctOfMid {
		proposalBlock(&p, "wide_spread", fmt.Sprintf("option spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, cfg.MaxSpreadPctOfMid))
	}
	trailAbs := reference * cfg.DefaultPct / 100
	if reference > 0 && trailAbs < cfg.MinTrailAbs {
		proposalBlock(&p, "trail_too_small", fmt.Sprintf("option trail %.4f is below policy minimum %.4f", trailAbs, cfg.MinTrailAbs))
	}
	if reference > 0 && spreadAbs > 0 && trailAbs < cfg.SpreadMultiple*spreadAbs {
		proposalBlock(&p, "trail_inside_spread", fmt.Sprintf("option trail %.4f is below %.1fx spread %.4f", trailAbs, cfg.SpreadMultiple, spreadAbs))
	}
	return p, true
}

func multiLegOptionSymbols(rows []rpc.PositionView) map[string]bool {
	counts := make(map[string]int)
	for _, row := range rows {
		if row.Quantity == 0 {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if symbol == "" {
			continue
		}
		counts[symbol]++
	}
	out := make(map[string]bool)
	for symbol, count := range counts {
		if count > 1 {
			out[symbol] = true
		}
	}
	return out
}

func applyTrailToProposal(p *rpc.TradeProposal, orderType string, pct, reference float64, action string, limitOffset float64) {
	if p == nil {
		return
	}
	p.OrderType = strings.ToUpper(strings.TrimSpace(orderType))
	if p.OrderType == "" {
		p.OrderType = rpc.OrderTypeTRAIL
	}
	trail := &rpc.OrderTrailSpec{
		Basis:      rpc.OrderTrailBasisInstrumentPrice,
		OffsetType: rpc.OrderTrailOffsetPercent,
	}
	if reference > 0 {
		amount := ceilPriceToTick(reference*pct/100, trailMinimumTick(p.Contract, reference))
		trail.OffsetType = rpc.OrderTrailOffsetAmount
		trail.TrailingAmount = cloneFloat64Ptr(&amount)
		trail.InitialStopPrice = trailingStopInitialPriceForContract(action, reference, amount, p.Contract)
	} else {
		trail.TrailingPercent = cloneFloat64Ptr(&pct)
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && limitOffset > 0 {
		trail.LimitOffset = cloneFloat64Ptr(&limitOffset)
	}
	p.Trail = trail
}

func trailingStopReferencePrice(row rpc.PositionView, action string) float64 {
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		if row.Ask != nil && *row.Ask > 0 {
			return *row.Ask
		}
	} else if row.Bid != nil && *row.Bid > 0 {
		return *row.Bid
	}
	if row.QuotePrice != nil && *row.QuotePrice > 0 {
		return *row.QuotePrice
	}
	if row.Mark > 0 {
		return row.Mark
	}
	if row.ValuationMark > 0 {
		return row.ValuationMark
	}
	return 0
}

func stockPositionLooksInactive(row rpc.PositionView) bool {
	return row.Mark <= 0 &&
		row.ValuationMark <= 0 &&
		row.MarketValue == 0 &&
		(row.QuotePrice == nil || *row.QuotePrice <= 0) &&
		(row.Bid == nil || *row.Bid <= 0) &&
		(row.Ask == nil || *row.Ask <= 0)
}

func optionTrailReference(row rpc.PositionView, action string) (reference float64, spreadAbs float64, ok bool) {
	if row.OptionBid == nil || row.OptionAsk == nil || *row.OptionBid <= 0 || *row.OptionAsk <= *row.OptionBid {
		return 0, 0, false
	}
	spreadAbs = *row.OptionAsk - *row.OptionBid
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return *row.OptionAsk, spreadAbs, true
	}
	return *row.OptionBid, spreadAbs, true
}

func proposalBlock(p *rpc.TradeProposal, code, message string) {
	if p == nil {
		return
	}
	p.State = rpc.TradeProposalStateBlocked
	p.Blockers = appendTradingBlockerOnce(p.Blockers, rpc.TradingBlocker{Code: code, Message: message})
}

// positionWireSecType maps PositionView.SecType — the canonical AssetType
// enum carried on position rows ("STOCK", "OPTION", …; positionSecType is
// the forward mapping) — to the IBKR wire security type for broker contract
// fields. The enum forms are not valid on the wire: TWS rejects secType
// "STOCK" with error 321 "Please enter a valid security type".
func positionWireSecType(raw string) string {
	switch {
	case strings.EqualFold(raw, "OPTION") || strings.EqualFold(raw, "OPT"):
		return "OPT"
	case strings.EqualFold(raw, "ETF"):
		return "ETF"
	default:
		return "STK"
	}
}

func baseProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, bucket string, row rpc.PositionView, action string, qty int, effect string, reason string) rpc.TradeProposal {
	secType := positionWireSecType(row.SecType)
	contract := proposalContractFromPosition(row, secType)
	p := rpc.TradeProposal{Key: proposalKey(bucket, contract, action), State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: contract.Symbol, SecType: secType, Action: action, Quantity: qty, MaxQuantity: int(math.Ceil(math.Abs(row.Quantity))), PositionQuantity: row.Quantity, PositionEffect: effect, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now}
	if row.Mark > 0 {
		v := row.Mark
		p.LimitPrice = &v
		p.Notional = math.Abs(row.Mark) * float64(qty) * float64(max(row.Multiplier, 1))
	}
	return p
}

func proposalContractFromPosition(row rpc.PositionView, secType string) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        row.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:      secType,
		Exchange:     nonEmptyString(row.Exchange, "SMART"),
		Currency:     nonEmptyString(row.Currency, "USD"),
		LocalSymbol:  row.LocalSymbol,
		TradingClass: row.TradingClass,
		Expiry:       row.Expiry,
		Strike:       row.Strike,
		Right:        row.Right,
		Multiplier:   row.Multiplier,
	}
	if secType == "STK" || secType == "ETF" {
		// msgPortfolioValue stores the *primary* exchange under row.Exchange
		// (documented wire quirk); routing a protective order directly to it
		// forfeits SMART routing. Route SMART and keep the venue as
		// PrimaryExch — ConID anchors contract identity either way.
		primary := strings.ToUpper(strings.TrimSpace(row.Exchange))
		if primary != "" && primary != "SMART" {
			contract.PrimaryExch = primary
		}
		contract.Exchange = "SMART"
		if primary == "IBIS" {
			contract.Market = "de"
			contract.Currency = nonEmptyString(row.Currency, "EUR")
		}
	}
	return contract
}

func applyMarketEventFlagsToProposal(prop *rpc.TradeProposal, events *rpc.MarketEventsResult) {
	if prop == nil || events == nil {
		return
	}
	flags := proposalMarketEventFlags(*prop, events)
	if len(flags) == 0 {
		return
	}
	prop.MarketFlags = flags
	for _, flag := range flags {
		switch {
		case flag.ID == rpc.MarketEventHaltRegulatoryOrNews && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active halt")
		case flag.ID == rpc.MarketEventLULDRecent && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active LULD pause")
		}
	}
}

func proposalMarketEventFlags(prop rpc.TradeProposal, events *rpc.MarketEventsResult) []rpc.MarketEventFlag {
	if events == nil || events.BySymbol == nil {
		return nil
	}
	symbol := strings.ToUpper(strings.TrimSpace(prop.Symbol))
	if symbol == "" {
		return nil
	}
	out := []rpc.MarketEventFlag{}
	for _, flag := range events.BySymbol[symbol] {
		if !proposalMarketEventFlagApplies(prop, flag) {
			continue
		}
		out = append(out, flag)
	}
	slices.SortFunc(out, func(a, b rpc.MarketEventFlag) int {
		if c := cmpMarketEventSeverity(a.Severity, b.Severity); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func proposalMarketEventFlagApplies(prop rpc.TradeProposal, flag rpc.MarketEventFlag) bool {
	switch flag.ID {
	case rpc.MarketEventHaltRegulatoryOrNews, rpc.MarketEventLULDRecent:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	case rpc.MarketEventRegSHOThreshold:
		return proposalCloseReduceEffect(prop.PositionEffect)
	case rpc.MarketEventBorrowInventoryTight, rpc.MarketEventBorrowFeeExtreme:
		return prop.PositionQuantity < 0 &&
			strings.EqualFold(prop.Action, rpc.OrderActionBuy) &&
			proposalCloseReduceEffect(prop.PositionEffect)
	default:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	}
}

func marketEventBlockProposal(prop *rpc.TradeProposal, flag rpc.MarketEventFlag, reason string) {
	prop.State = rpc.TradeProposalStateBlocked
	code := "market_event_" + flag.ID
	message := fmt.Sprintf("%s is %s for %s", flag.Label, reason, flag.Symbol)
	if flag.Source != "" {
		message += " (" + flag.Source + ")"
	}
	prop.Blockers = appendTradingBlockerOnce(prop.Blockers, rpc.TradingBlocker{
		Code:    code,
		Message: message + "; refresh proposals after the market event clears.",
		Action:  "Wait for fresh tradability context before previewing or submitting this protection proposal.",
	})
}

func (e *proposalEngine) Preview(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposalPreviewResult, error) {
	prop, blockers, err := e.previewProposal(ctx, p)
	now := e.clock()
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, Preview: sanitizeProposalPreview(preview), Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.duplicateProtectiveBlockers(prop); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, Preview: sanitizeProposalPreview(preview), Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers()
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: false, Preview: sanitizeProposalPreview(preview), Blockers: blockers, AsOf: now}, nil
	}
	return rpc.TradeProposalPreviewResult{Accepted: true, Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: preview.SubmitEligible, Preview: sanitizeProposalPreview(preview), AsOf: now}, nil
}

func (e *proposalEngine) previewProposal(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	if p.FastPath {
		if prop, blockers, ok := e.fastPathPreviewProposal(p.Key, p.Revision); ok {
			return prop, blockers, nil
		}
	}
	return e.revalidatedProposal(ctx, p.Key, p.Revision)
}

func (e *proposalEngine) submitProposal(ctx context.Context, p rpc.TradeProposalSubmitParams, fastPathEnabled bool) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	if p.FastPath && fastPathEnabled {
		if prop, blockers, ok := e.fastPathSubmitProposal(p.Key, p.Revision); ok {
			return prop, blockers, nil
		}
	}
	return e.revalidatedProposal(ctx, p.Key, p.Revision)
}

func (e *proposalEngine) fastPathPreviewProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	return e.fastPathCachedProposal(key, revision)
}

func (e *proposalEngine) fastPathSubmitProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	return e.fastPathCachedProposal(key, revision)
}

func (e *proposalEngine) fastPathCachedProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, true
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" || snap.Revision == "" {
		return rpc.TradeProposal{}, nil, false
	}
	// The fast path serves the cached snapshot; cap its age so a daemon
	// restart (LoadedFromState) or a stalled refresh can never preview
	// against arbitrarily old trail numbers. Falling out of the fast path
	// lands on full revalidation, which is the safe slow path.
	maxAge := 2 * e.cadence
	if maxAge <= 0 {
		maxAge = 30 * time.Minute
	}
	if snap.LoadedFromState || e.clock().Sub(snap.AsOf) > maxAge {
		return rpc.TradeProposal{}, nil, false
	}
	// The fast path may only act on a cached snapshot generated under the
	// currently-connected account/mode session. Mismatch or an unscoped
	// session fails closed; proposal-free shells carry session-independent
	// blockers and are handled below.
	if len(snap.Proposals) > 0 {
		if blockers := proposalScopeBlockers(snap.AccountID, snap.AccountMode, e.currentScope()); len(blockers) > 0 {
			return rpc.TradeProposal{}, blockers, true
		}
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, true
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, true
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, true
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, true
	}
	for _, prop := range snap.Proposals {
		if prop.Key != key {
			continue
		}
		if prop.Bucket != rpc.TradeProposalBucketTrailingStop {
			return rpc.TradeProposal{}, nil, false
		}
		if len(snap.Blockers) > 0 {
			return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), true
		}
		return prop, prop.Blockers, true
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, true
}

func (e *proposalEngine) Submit(ctx context.Context, p rpc.TradeProposalSubmitParams) (rpc.TradeProposalSubmitResult, error) {
	now := e.clock()
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	prop, blockers, err := e.submitProposal(ctx, p, cfg.FastPathEnabledResolved())
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	if !cfg.FastPathEnabledResolved() || !p.FastPath {
		blockers := []rpc.TradingBlocker{{Code: "fast_path_disabled", Message: "proposal submit requires fast_path=true and [auto_trade].fast_path_enabled=true"}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.server.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal fast-path previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.duplicateProtectiveBlockers(prop); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers()
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	place, err := e.server.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: p.TimeoutMs, Origin: p.Origin})
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("submitted", prop, now, preview.PreviewTokenID, place.OrderRef, "proposal submitted through preview-backed fast path"))
	if e.server.proposalOutcomes != nil {
		if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeSubmitted(prop, preview, place, now)); err != nil {
			e.server.warnf("trade proposal outcomes: append submitted mark: %v", err)
		}
	}
	return rpc.TradeProposalSubmitResult{Accepted: place.Accepted, Proposal: prop, Preview: sanitizeProposalPreview(preview), Place: place, PreviewTokenID: preview.PreviewTokenID, OrderRef: place.OrderRef, Message: place.Message, AsOf: e.clock()}, nil
}

// resolveRowMinTick returns the broker-reported minimum increment for a held
// position's contract, fetched once per conID and cached for the daemon
// lifetime. Generation and preview must round trail prices on the same grid:
// the proposal-vs-preview drift gate compares them exactly — so the fetch
// uses the same contract shape proposals carry into previews, with
// row.SecType mapped to its wire code. Passing the row's enum form
// ("STOCK") verbatim made TWS reject the contract-details request with
// error 321 on every refresh: the failure is never cached, so each held
// stock row re-burned a request plus the full fetch timeout per cadence
// (observed 2026-06-11, five names × 15-minute proposal cadence).
func (e *proposalEngine) resolveRowMinTick(row rpc.PositionView) float64 {
	if e == nil || e.server == nil {
		return 0
	}
	contract := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	return e.server.resolveContractMinTick(context.Background(), contract, previewMinTickTimeout)
}

// closeReduceQuantity sizes a close/reduce order for a possibly fractional
// position: floor the magnitude (protect 10 of 10.5 shares) instead of
// ceiling it, which would classify as a flip and be blocked with errors that
// never mention fractions. The remainder is surfaced in proposal details.
func closeReduceQuantity(position float64) (int, float64) {
	abs := math.Abs(position)
	qty := int(math.Floor(abs + 1e-9))
	remainder := abs - float64(qty)
	if remainder < 1e-9 {
		remainder = 0
	}
	return qty, remainder
}

// duplicateProtectiveBlockers blocks a proposal when an open journaled order
// already works the same contract and side: submitting would double the stop,
// and a double fill flips the position. Inferred-expired and terminal rows do
// not count (see inferDayOrderExpiry), so a stale zombie cannot block
// re-protection forever.
func (e *proposalEngine) duplicateProtectiveBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	if e == nil || e.server == nil {
		return nil
	}
	views, _, err := e.server.loadOrderViews()
	if err != nil {
		// Journal unavailability blocks the write path through the trading
		// gate on its own; the duplicate check just can't help here.
		return nil
	}
	for _, v := range views {
		if !v.Open || !strings.EqualFold(v.Action, p.Action) {
			continue
		}
		if !orderViewMatchesProposalContract(v, p) {
			continue
		}
		return []rpc.TradingBlocker{{
			Code:    "existing_protective_order",
			Message: fmt.Sprintf("open order %s already works %s %s (%s)", v.OrderRef, p.Action, p.Symbol, nonEmptyString(v.OrderType, "order")),
			Action:  fmt.Sprintf("Keep the standing protection, or cancel it first with `ibkr order cancel %s` before submitting a replacement.", v.OrderRef),
		}}
	}
	return nil
}

func orderViewMatchesProposalContract(v rpc.OrderView, p rpc.TradeProposal) bool {
	if v.ConID != 0 && p.Contract.ConID != 0 {
		return v.ConID == p.Contract.ConID
	}
	return strings.EqualFold(v.Symbol, p.Symbol) && equivalentStockSecType(v.SecType, p.SecType)
}

func equivalentStockSecType(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "ETF" {
			return "STK"
		}
		return s
	}
	return norm(a) == norm(b)
}

func previewNotSubmitEligibleBlockers() []rpc.TradingBlocker {
	return []rpc.TradingBlocker{{
		Code:    "preview_not_submit_eligible",
		Message: "broker WhatIf did not make this proposal submit-eligible",
		Action:  "Resolve broker WhatIf availability and preview again before submitting a broker-managed stop.",
	}}
}

func (e *proposalEngine) Ignore(p rpc.TradeProposalIgnoreParams) rpc.TradeProposalIgnoreResult {
	now := e.clock()
	key := strings.TrimSpace(p.Key)
	scope := e.currentScope()
	e.mu.Lock()
	e.ignored[scopedIgnoreKey(scope, key)] = struct{}{}
	e.mu.Unlock()
	ev := proposalEvent{At: now, Type: "ignored", Key: key, Revision: strings.TrimSpace(p.Revision), Reason: strings.TrimSpace(p.Reason), Message: "proposal ignored"}
	if brokerScopeConcrete(scope) {
		ev.AccountID = scope.Account
	}
	e.appendEvent(ev)
	return rpc.TradeProposalIgnoreResult{Accepted: true, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "proposal ignored", AsOf: now}
}

func (e *proposalEngine) revalidatedProposal(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, nil
	}
	snap, err := e.Refresh(ctx, false)
	if err != nil && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, nil
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, nil
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, nil
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, nil
	}
	for _, prop := range snap.Proposals {
		if prop.Key == key {
			if len(snap.Blockers) > 0 {
				return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), nil
			}
			return prop, prop.Blockers, nil
		}
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, nil
}

func proposalOrderPreviewParams(prop rpc.TradeProposal, qty, timeoutMs int) rpc.OrderPreviewParams {
	orderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	strategy := rpc.OrderStrategyPatientLimit
	if orderType == rpc.OrderTypeTRAIL || orderType == rpc.OrderTypeTRAILLIMIT {
		strategy = rpc.OrderStrategyBrokerTrail
	}
	trail := cloneTrailSpec(prop.Trail)
	return rpc.OrderPreviewParams{Action: prop.Action, Contract: prop.Contract, Quantity: qty, OrderType: orderType, Trail: trail, Strategy: strategy, TIF: proposalTIF(prop), OutsideRTH: prop.OutsideRTH, TimeoutMs: timeoutMs, Source: proposalOrderSource}
}

// proposalTIF normalizes a proposal's TIF for preview params and the
// drift gate; proposals persisted before the field existed mean DAY.
func proposalTIF(prop rpc.TradeProposal) string {
	tif := strings.ToUpper(strings.TrimSpace(prop.TIF))
	if tif == "" {
		return rpc.OrderTIFDay
	}
	return tif
}

// trailingStopTIFDetail spells out the lifetime consequence of the bucket
// TIF on the proposal itself, where the operator reviews it.
func trailingStopTIFDetail(tif string, optionPremiumTrail bool) string {
	if strings.EqualFold(tif, rpc.OrderTIFGTC) {
		if optionPremiumTrail {
			return "tif=GTC: stop persists until filled or cancelled; theta decay alone walks the premium into the stop eventually"
		}
		return "tif=GTC: stop persists across sessions until filled or cancelled"
	}
	return "tif=DAY: stop expires at the session close and does not cover overnight gaps; set tif = \"GTC\" in [buckets.trailing_stop] to persist"
}

func selectedProposalQty(prop rpc.TradeProposal, requested int) int {
	if requested <= 0 {
		return prop.Quantity
	}
	return max(1, min(requested, prop.MaxQuantity))
}

func proposalPreviewSafetyBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if preview == nil {
		add("proposal_preview_missing", "proposal preview result is unavailable", "Refresh and preview the proposal again before submit.")
		return blockers
	}
	if !proposalCloseReduceEffect(prop.PositionEffect) {
		add("proposal_effect_not_close_reduce", fmt.Sprintf("proposal effect %q is not close/reduce", prop.PositionEffect), "Refresh proposals so the daemon can rebuild a close/reduce-only recommendation.")
	}
	if !proposalCloseReduceEffect(preview.Position.Effect) {
		add("preview_effect_not_close_reduce", fmt.Sprintf("preview effect %q is not close/reduce", preview.Position.Effect), "Refresh positions and preview again; proposal submit cannot open, increase, or flip exposure.")
	}
	if !proposalSupportedSecType(prop.SecType) || !proposalSupportedSecType(preview.Draft.Contract.SecType) {
		add("unsupported_security_type", "protection proposals support single-leg STK/ETF/OPT orders only", "Use a manual workflow for unsupported instruments.")
	}
	if !proposalSupportedOrderType(preview.Draft.OrderType) {
		add("unsupported_order_type", fmt.Sprintf("proposal order type %q is not supported", preview.Draft.OrderType), "Refresh proposals and preview a supported close/reduce order.")
	}
	previewTIF := strings.ToUpper(strings.TrimSpace(preview.Draft.TIF))
	switch {
	case previewTIF != rpc.OrderTIFDay && previewTIF != rpc.OrderTIFGTC:
		add("unsupported_tif", fmt.Sprintf("proposal time-in-force %q is not DAY or GTC", preview.Draft.TIF), "Refresh proposals and preview a supported time-in-force.")
	case previewTIF != proposalTIF(prop):
		add("tif_drift", fmt.Sprintf("preview time-in-force %q does not match proposal time-in-force %q", preview.Draft.TIF, proposalTIF(prop)), "Refresh proposals and preview again.")
	}
	if strings.EqualFold(preview.Draft.Contract.SecType, "OPT") && preview.Draft.OutsideRTH {
		add("option_outside_rth", "option protection proposals must not request outside_rth", "Refresh proposals and preview during the supported option session.")
	}
	if preview.Draft.Quantity <= 0 || preview.Draft.Quantity > prop.MaxQuantity {
		add("quantity_outside_position", fmt.Sprintf("proposal preview quantity %d exceeds close/reduce cap %d", preview.Draft.Quantity, prop.MaxQuantity), "Refresh positions and preview a quantity within the current position.")
	}
	if !strings.EqualFold(preview.Draft.Action, prop.Action) {
		add("action_drift", fmt.Sprintf("preview action %q does not match proposal action %q", preview.Draft.Action, prop.Action), "Refresh proposals and preview again.")
	}
	propOrderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if propOrderType == "" {
		propOrderType = rpc.OrderTypeLMT
	}
	if strings.ToUpper(strings.TrimSpace(preview.Draft.OrderType)) != propOrderType {
		add("order_type_drift", fmt.Sprintf("preview order type %q does not match proposal order type %q", preview.Draft.OrderType, prop.OrderType), "Refresh proposals and preview again.")
	}
	if isTrailOrderType(preview.Draft.OrderType) {
		switch {
		case prop.Trail == nil:
			add("proposal_trail_missing", "proposal is missing broker-side trail fields", "Refresh proposals and preview again.")
		case preview.Draft.Trail == nil:
			add("trail_missing", "proposal preview is missing broker-side trail fields", "Refresh proposals and preview again.")
		default:
			for _, blocker := range proposalTrailDriftBlockers(prop.Trail, preview.Draft.Trail) {
				add(blocker.Code, blocker.Message, blocker.Action)
			}
		}
	}
	if strings.TrimSpace(preview.Draft.Source) != proposalOrderSource {
		add("source_drift", "proposal preview source does not match the protection proposal engine", "Refresh proposals and preview again.")
	}
	return blockers
}

func proposalTrailDriftBlockers(proposal, preview *rpc.OrderTrailSpec) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  "Refresh proposals and preview again before submitting a broker-managed stop.",
		})
	}
	if !strings.EqualFold(strings.TrimSpace(proposal.OffsetType), strings.TrimSpace(preview.OffsetType)) {
		add("trail_offset_type_drift", fmt.Sprintf("preview trail offset type %q does not match proposal offset type %q", preview.OffsetType, proposal.OffsetType))
	}
	if !floatPtrEqual(proposal.TrailingPercent, preview.TrailingPercent) {
		add("trail_percent_drift", "preview trailing_percent does not match proposal trailing_percent")
	}
	if !floatPtrEqual(proposal.TrailingAmount, preview.TrailingAmount) {
		add("trail_amount_drift", "preview trailing_amount does not match proposal trailing_amount")
	}
	if !floatPtrEqual(proposal.LimitOffset, preview.LimitOffset) {
		add("trail_limit_offset_drift", "preview limit_offset does not match proposal limit_offset")
	}
	if !floatEqual(proposal.InitialStopPrice, preview.InitialStopPrice) {
		add("trail_initial_stop_drift", "preview initial_stop_price does not match proposal initial_stop_price")
	}
	return blockers
}

func floatPtrEqual(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return math.Abs(*a-*b) < 1e-9
	}
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func proposalSupportedOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT, rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func isTrailOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func cloneTrailSpec(in *rpc.OrderTrailSpec) *rpc.OrderTrailSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.TrailingPercent = cloneFloat64Ptr(in.TrailingPercent)
	out.TrailingAmount = cloneFloat64Ptr(in.TrailingAmount)
	out.LimitOffset = cloneFloat64Ptr(in.LimitOffset)
	return &out
}

func mergeTradingBlockers(first, second []rpc.TradingBlocker) []rpc.TradingBlocker {
	out := append([]rpc.TradingBlocker(nil), first...)
	for _, blocker := range second {
		out = appendTradingBlockerOnce(out, blocker)
	}
	return out
}

func proposalCloseReduceEffect(effect string) bool {
	switch effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return true
	default:
		return false
	}
}

func sanitizeProposalPreview(in *rpc.OrderPreviewResult) *rpc.TradeProposalOrderPreview {
	if in == nil {
		return nil
	}
	return &rpc.TradeProposalOrderPreview{PreviewTokenID: in.PreviewTokenID, PreviewTokenScope: in.PreviewTokenScope, PreviewTokenExpiresAt: in.PreviewTokenExpiresAt, TokenMinted: in.TokenMinted, SubmitEligible: in.SubmitEligible, Mode: in.Mode, Account: in.Account, Endpoint: in.Endpoint, ClientID: in.ClientID, Draft: in.Draft, Quote: in.Quote, Position: in.Position, Notional: in.Notional, MaxNotional: in.MaxNotional, WhatIf: in.WhatIf, Warnings: append([]rpc.DataWarning(nil), in.Warnings...), AsOf: in.AsOf}
}

func (e *proposalEngine) installSnapshot(snap rpc.TradeProposalSnapshot, show bool) {
	e.mu.Lock()
	prevRevision := e.snapshot.Revision
	prevMarkDate := e.snapshot.AsOf.Format(time.DateOnly)
	e.mu.Unlock()
	e.replaceSnapshot(snap)
	// "generated" events and daily outcome marks record new generation
	// work. At a 2m cadence most refreshes re-derive a revision-identical
	// proposal set; appending per-proposal duplicates would grow the
	// unbounded trade-proposals.jsonl ~7.5x faster and rescan the outcomes
	// file per proposal per cycle for nothing. Marks still pass on a date
	// change: their identity is daily (proposalOutcomeIdentity includes
	// MarkDate), so a revision frozen across midnight still owes the new
	// day's mark.
	newRevision := snap.Revision != prevRevision
	newMarkDate := snap.AsOf.Format(time.DateOnly) != prevMarkDate
	for _, prop := range snap.Proposals {
		if newRevision {
			e.appendEvent(proposalEventForProposal("generated", prop, snap.AsOf, "", "", "proposal generated"))
		}
		if (newRevision || newMarkDate) && e.server != nil && e.server.proposalOutcomes != nil {
			if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeMarked(prop, snap.AsOf)); err != nil {
				e.server.warnf("trade proposal outcomes: append daily mark: %v", err)
			}
		}
	}
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *proposalEngine) installPreservedSnapshot(snap rpc.TradeProposalSnapshot, show bool) {
	e.replaceSnapshot(snap)
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *proposalEngine) replaceSnapshot(snap rpc.TradeProposalSnapshot) {
	e.mu.Lock()
	e.snapshot = cloneProposalSnapshot(snap)
	e.mu.Unlock()
	if e.store == nil {
		return
	}
	// Only generated, concretely scoped snapshots survive a restart.
	// Error/unscoped shells (revision "empty") serve in-memory only:
	// the startup refresh runs before the gateway connects, and
	// persisting its shell overwrote the last good snapshot on every
	// start — installProposalEngine then warned "ignoring persisted
	// snapshot without a concrete account/mode scope" and warm-start
	// adoption never happened.
	if !proposalSnapshotPersistable(snap) {
		return
	}
	if err := e.store.SaveCurrent(snap); err != nil && e.server != nil {
		e.server.warnf("trade proposals: save current snapshot: %v", err)
	}
}

func (e *proposalEngine) preserveSnapshotOnRefreshFailure(scope brokerStateScope, autoStatus rpc.AutoTradeStatus, policyStatus rpc.ProtectionPolicyStatus, blockers []rpc.TradingBlocker, show bool) (rpc.TradeProposalSnapshot, bool) {
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if !proposalSnapshotUsable(snap) || !sameProposalPolicy(snap, policyStatus) {
		return rpc.TradeProposalSnapshot{}, false
	}
	// Preserving last-good proposals through a transient fetch failure is
	// only safe when they were generated for the same session: a paper
	// snapshot preserved through the reconnect blips of a paper→live
	// switch would resurface paper proposals under live.
	if !sameBrokerScope(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}, scope) {
		if e.server != nil {
			e.server.warnf("trade proposals: dropping preserved snapshot on refresh failure: snapshot scope %q/%q does not match connected session %q/%q", snap.AccountID, snap.AccountMode, scope.Account, scope.Mode)
		}
		return rpc.TradeProposalSnapshot{}, false
	}
	snap.AutoTrade = autoStatus
	snap.PolicyStatus = policyStatus
	snap.Trading = autoStatus.Trading
	merged := append([]rpc.TradingBlocker(nil), blockers...)
	for _, blocker := range snap.Blockers {
		merged = appendTradingBlockerOnce(merged, blocker)
	}
	snap.Blockers = merged
	e.installPreservedSnapshot(snap, show)
	return snap, true
}

func proposalSnapshotUsable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Kind == rpc.TradeProposalSnapshotKind && snap.Revision != "" && snap.Revision != "empty" && len(snap.Proposals) > 0
}

// proposalSnapshotPersistable reports whether snap is a generated,
// concretely scoped snapshot (including a legitimate zero-proposal
// generation) rather than a transient error/unscoped shell. Only these
// are written to disk; see replaceSnapshot.
func proposalSnapshotPersistable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Revision != "" && snap.Revision != "empty" &&
		brokerScopeConcrete(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode})
}

func sameProposalPolicy(snap rpc.TradeProposalSnapshot, status rpc.ProtectionPolicyStatus) bool {
	if snap.PolicyID != "" && status.PolicyID != "" && snap.PolicyID != status.PolicyID {
		return false
	}
	if snap.PolicyVersion != 0 && status.PolicyVersion != 0 && snap.PolicyVersion != status.PolicyVersion {
		return false
	}
	if snap.PolicyFingerprint.Key != "" && status.Fingerprint.Key != "" && snap.PolicyFingerprint.Key != status.Fingerprint.Key {
		return false
	}
	return true
}

func (e *proposalEngine) appendShownEvents(snap rpc.TradeProposalSnapshot) {
	for _, prop := range snap.Proposals {
		e.appendEvent(proposalEventForProposal("shown", prop, e.clock(), "", "", "proposal shown"))
	}
}

func (e *proposalEngine) appendBlocked(prop rpc.TradeProposal, key, revision string, blockers []rpc.TradingBlocker, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if len(blockers) > 0 {
		msg = blockers[0].Message
	}
	ev := proposalEventForProposal("blocked", prop, e.clock(), "", "", msg)
	if ev.Key == "" {
		ev.Key = strings.TrimSpace(key)
	}
	if ev.Revision == "" {
		ev.Revision = strings.TrimSpace(revision)
	}
	e.appendEvent(ev)
}

func proposalEventForProposal(eventType string, prop rpc.TradeProposal, at time.Time, tokenID, orderRef, msg string) proposalEvent {
	return proposalEvent{At: at, Type: eventType, Key: prop.Key, Revision: prop.Revision, Bucket: prop.Bucket, PolicyID: prop.PolicyID, PolicyVersion: prop.PolicyVersion, PolicyFingerprint: prop.PolicyFingerprint, PreviewTokenID: tokenID, OrderRef: orderRef, Message: msg, SourceFingerprints: prop.SourceFingerprints}
}

func (e *proposalEngine) appendEvent(ev proposalEvent) {
	if ev.AccountID == "" {
		e.mu.Lock()
		ev.AccountID = e.snapshot.AccountID
		e.mu.Unlock()
	}
	if err := e.store.AppendEvent(ev); err != nil {
		e.server.warnf("trade proposals: append event: %v", err)
	}
}

func (e *proposalEngine) isIgnored(scope brokerStateScope, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.ignored[scopedIgnoreKey(scope, key)]
	return ok
}

func scopedIgnoreKey(scope brokerStateScope, key string) string {
	return strings.ToUpper(strings.TrimSpace(scope.Account)) + "|" + strings.ToLower(strings.TrimSpace(scope.Mode)) + "|" + key
}

// currentScope resolves the connected broker session identity (account +
// paper/live mode) that scoped proposal state binds to.
func (e *proposalEngine) currentScope() brokerStateScope {
	if e == nil {
		return brokerStateScope{}
	}
	if e.scope != nil {
		return e.scope()
	}
	return e.server.currentBrokerStateScope()
}

// proposalSnapshotAdoptable reports whether a persisted snapshot may seed
// the in-memory state at startup. The gate is the scope being concrete,
// not the schema version string: legacy v1 snapshots have no account_mode
// and "All"-scoped snapshots have no concrete account, so both fail closed
// and the first refresh regenerates state for the connected session.
func proposalSnapshotAdoptable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Kind == rpc.TradeProposalSnapshotKind &&
		brokerScopeConcrete(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode})
}

// proposalScopeBlockers reports why a snapshot bound to snapAccount/snapMode
// must not be served or acted on under the current broker scope; nil means
// it matches.
func proposalScopeBlockers(snapAccount, snapMode string, scope brokerStateScope) []rpc.TradingBlocker {
	if !brokerScopeConcrete(scope) {
		return []rpc.TradingBlocker{proposalScopeUnscopedBlocker(scope)}
	}
	if !sameBrokerScope(brokerStateScope{Account: snapAccount, Mode: snapMode}, scope) {
		return []rpc.TradingBlocker{{
			Code:    "proposal_scope_mismatch",
			Message: fmt.Sprintf("proposal snapshot was generated for account %q mode %q but the connected session is account %q mode %q", snapAccount, snapMode, scope.Account, scope.Mode),
			Action:  "Refresh proposals to regenerate them for the connected session.",
		}}
	}
	return nil
}

func proposalScopeUnscopedBlocker(scope brokerStateScope) rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "account_identity_unscoped",
		Message: fmt.Sprintf("connected session has no concrete single-account identity (observed account %q mode %q); protection proposals are scoped per account and paper/live mode", scope.Account, scope.Mode),
		Action:  "Reconnect TWS/Gateway with a single concrete account, then refresh proposals.",
	}
}

func (e *proposalEngine) clock() time.Time {
	if e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func emptyProposalSnapshot(now time.Time) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: now, Revision: "empty", Proposals: []rpc.TradeProposal{}}
}

func proposalCounts(proposals []rpc.TradeProposal) rpc.TradeProposalCounts {
	var out rpc.TradeProposalCounts
	out.Total = len(proposals)
	for _, p := range proposals {
		if len(p.Blockers) == 0 {
			out.Actionable++
		}
		out.MarketFlags += len(p.MarketFlags)
		switch p.Bucket {
		case rpc.TradeProposalBucketThetaHygiene:
			out.ThetaHygiene++
			out.ThetaPerDay += p.ThetaPerDay
		case rpc.TradeProposalBucketRiskReduction:
			out.RiskReduction++
			out.RiskReductionExcessNotional += p.RiskExcessNotional
			out.RiskReductionExcessCurrency = mergedCurrency(out.RiskReductionExcessCurrency, p.RiskExcessCurrency)
		case rpc.TradeProposalBucketTrailingStop:
			out.TrailingStop++
		}
	}
	// A raw sum across different local currencies is meaningless. Rather
	// than serve EUR+USD arithmetic under a fake currency label (the SPA
	// used to coerce the "MIX" sentinel to USD), omit the aggregate until
	// a base-currency conversion exists; renderers show "--".
	if out.RiskReductionExcessCurrency == "MIX" {
		out.RiskReductionExcessNotional = 0
		out.RiskReductionExcessCurrency = ""
	}
	return out
}

func proposalRevision(policy rpc.Fingerprint, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, proposals []rpc.TradeProposal) string {
	stableSources := sources
	// Regime and market-event evidence are informative for ranking and blockers,
	// but their source-health fields can advance between list and preview. Keep
	// revision anchored to policy/account/positions so the one-confirm path does
	// not false-stale while refreshed proposals still carry live blockers.
	stableSources.Regime = nil
	stableSources.MarketEvents = nil
	// Account/mode enter the revision directly: the account and positions
	// fingerprints hash risk *buckets*, so two sessions with bucket-equal
	// exposure could otherwise collide on the same revision across a
	// paper/live switch.
	projection := struct {
		Policy   rpc.Fingerprint                     `json:"policy"`
		Account  string                              `json:"account"`
		Mode     string                              `json:"mode"`
		Sources  rpc.TradeProposalSourceFingerprints `json:"sources"`
		Proposal []string                            `json:"proposal"`
	}{Policy: policy, Account: strings.ToUpper(strings.TrimSpace(scope.Account)), Mode: strings.ToLower(strings.TrimSpace(scope.Mode)), Sources: stableSources}
	for _, p := range proposals {
		projection.Proposal = append(projection.Proposal, p.Key+":"+strconv.Itoa(p.Quantity)+":"+p.PositionEffect)
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func proposalKey(bucket string, contract rpc.ContractParams, action string) string {
	raw := strings.Join([]string{bucket, strings.ToUpper(contract.Symbol), strings.ToUpper(contract.SecType), strings.ToUpper(contract.LocalSymbol), contract.Expiry, strings.ToUpper(contract.Right), fmt.Sprintf("%.4f", contract.Strike), strings.ToUpper(action)}, "|")
	sum := sha256.Sum256([]byte(raw))
	return bucket + ":" + hex.EncodeToString(sum[:8])
}

func optionDTE(expiry string, now time.Time) (int, bool) {
	expiry = strings.TrimSpace(expiry)
	var t time.Time
	var err error
	switch len(expiry) {
	case len("20060102"):
		t, err = time.ParseInLocation("20060102", expiry, now.Location())
	case len("2006-01-02"):
		t, err = time.ParseInLocation("2006-01-02", expiry, now.Location())
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	exp := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
	return int(exp.Sub(today).Hours() / 24), true
}

func groupMarketValueOrderValue(g rpc.PositionGroup) float64 {
	if g.GroupMarketValue != 0 {
		return g.GroupMarketValue
	}
	if g.GroupMarketValueBase != nil {
		return *g.GroupMarketValueBase
	}
	return 0
}

func mergedCurrency(existing, next string) string {
	next = strings.ToUpper(strings.TrimSpace(next))
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	if existing == next {
		return existing
	}
	return "MIX"
}

func proposalSupportedSecType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF", "OPT", "OPTION":
		return true
	default:
		return false
	}
}

func cloneProposalSnapshot(in rpc.TradeProposalSnapshot) rpc.TradeProposalSnapshot {
	out := in
	out.Proposals = append([]rpc.TradeProposal(nil), in.Proposals...)
	for i := range out.Proposals {
		out.Proposals[i].Details = append([]string(nil), in.Proposals[i].Details...)
		out.Proposals[i].MarketFlags = append([]rpc.MarketEventFlag(nil), in.Proposals[i].MarketFlags...)
		out.Proposals[i].Blockers = append([]rpc.TradingBlocker(nil), in.Proposals[i].Blockers...)
	}
	out.Blockers = append([]rpc.TradingBlocker(nil), in.Blockers...)
	if in.MarketEvents != nil {
		events := *in.MarketEvents
		events.Flags = append([]rpc.MarketEventFlag(nil), in.MarketEvents.Flags...)
		events.SourceHealth = append([]rpc.SourceHealth(nil), in.MarketEvents.SourceHealth...)
		events.WarningDetails = append([]rpc.DataWarning(nil), in.MarketEvents.WarningDetails...)
		if in.MarketEvents.BySymbol != nil {
			events.BySymbol = make(map[string][]rpc.MarketEventFlag, len(in.MarketEvents.BySymbol))
			for sym, flags := range in.MarketEvents.BySymbol {
				events.BySymbol[sym] = append([]rpc.MarketEventFlag(nil), flags...)
			}
		}
		out.MarketEvents = &events
	}
	return out
}

func (s *proposalStore) SaveCurrent(snap rpc.TradeProposalSnapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateStateAtomic(s.currentPath, data)
}

func (s *proposalStore) LoadCurrent() (rpc.TradeProposalSnapshot, error) {
	data, err := os.ReadFile(s.currentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return rpc.TradeProposalSnapshot{}, nil
		}
		return rpc.TradeProposalSnapshot{}, err
	}
	var snap rpc.TradeProposalSnapshot
	err = json.Unmarshal(data, &snap)
	return snap, err
}

func (s *proposalStore) AppendEvent(ev proposalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Version == 0 {
		ev.Version = proposalEventFileVersion
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ensurePrivateStateDir(s.eventsPath); err != nil {
		return err
	}
	f, err := os.OpenFile(s.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func (s *proposalStore) FindSubmittedEvent(orderRef, tokenID string) (proposalEvent, bool, error) {
	if s == nil || s.eventsPath == "" {
		return proposalEvent{}, false, nil
	}
	orderRef = strings.TrimSpace(orderRef)
	tokenID = strings.TrimSpace(tokenID)
	if orderRef == "" && tokenID == "" {
		return proposalEvent{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return proposalEvent{}, false, nil
		}
		return proposalEvent{}, false, err
	}
	defer func() { _ = f.Close() }()
	var found proposalEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev proposalEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return proposalEvent{}, false, err
		}
		if ev.Type != "submitted" {
			continue
		}
		if orderRef != "" && ev.OrderRef == orderRef || tokenID != "" && ev.PreviewTokenID == tokenID {
			found = ev
		}
	}
	if err := sc.Err(); err != nil {
		return proposalEvent{}, false, err
	}
	if found.Type == "" {
		return proposalEvent{}, false, nil
	}
	return found, true, nil
}
