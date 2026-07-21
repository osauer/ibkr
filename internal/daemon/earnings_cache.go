package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// earningsCache serves per-symbol next-earnings dates for the trading
// rulebook (rules 6-8). Fetches never run on the snapshot path. Provider
// attempts, their last-good values, and the aggregate resolution are committed
// atomically before readers can observe them. Ambiguity is always unknown,
// never a guessed date.

const (
	earningsStoreFilename    = "earnings-dates.json"
	earningsPersistVersion   = 2
	earningsLegacyVersion    = 1
	earningsFreshWindow      = 24 * time.Hour
	earningsTTL              = 45 * 24 * time.Hour
	earningsFetchTimeout     = 8 * time.Second
	earningsFailureRetry     = 15 * time.Minute
	earningsFetchConcurrency = 4
	earningsAuthorityScope   = "market/events/earnings"
	// Keep the established state kind: a v2 payload under the same key makes
	// older binaries reject the authority instead of silently reading a stale
	// sibling document.
	earningsStateKind = "earnings_dates.current.v1"

	// Legacy observations are immutable evidence from the former JSON cache.
	earningsObservationKind   = "earnings_dates.snapshot.v1"
	earningsObservationSource = "nasdaq.earnings_calendar"

	earningsProviderObservationKind = "earnings_dates.provider_outcome.v2"
	earningsNasdaqProvider          = "nasdaq"
	earningsWSHProvider             = "ibkr_wsh"
	earningsWSHObservationSource    = "ibkr.wsh_event_calendar"
)

const (
	earningsReasonConsensus        = "consensus"
	earningsReasonSingleSource     = "single_source"
	earningsReasonRetainedLastGood = "retained_last_good"
	earningsReasonConflicting      = "conflicting_sources"
	earningsReasonDateElapsed      = "date_elapsed"
)

type earningsEntry struct {
	// Date is the next earnings date, YYYY-MM-DD (ET calendar date).
	Date string `json:"date"`
	// TimeOfDay is "amc", "bmo", or "" when the provider did not say.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged unconfirmed dates.
	Estimated  bool      `json:"estimated,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// earningsPersistEnvelopeV1 is deliberately pinned for the unpublished JSON
// cutover importer. Never point it at the live v2 authority.
type earningsPersistEnvelopeV1 struct {
	Version int                      `json:"version"`
	Entries map[string]earningsEntry `json:"entries"`
}

type earningsPersistEnvelope struct {
	Version int                            `json:"version"`
	Symbols map[string]earningsSymbolState `json:"symbols"`
}

type earningsProviderAttempt struct {
	Status      string             `json:"status"`
	Entry       *earningsEntry     `json:"entry,omitempty"`
	AttemptedAt time.Time          `json:"attempted_at"`
	CompletedAt time.Time          `json:"completed_at"`
	NextAttempt *time.Time         `json:"next_attempt,omitempty"`
	LastFailure *rpc.SourceFailure `json:"last_failure,omitempty"`
}

type earningsProviderState struct {
	LastAttempt earningsProviderAttempt `json:"last_attempt"`
	LastGood    *earningsEntry          `json:"last_good,omitempty"`
}

// earningsResolution is persisted as the exact cross-provider decision made
// at commit time. Readers re-resolve against the current clock so TTL expiry
// cannot leave a formerly valid date usable forever.
type earningsResolution struct {
	Status string         `json:"status"`
	Reason string         `json:"reason,omitempty"`
	Entry  *earningsEntry `json:"entry,omitempty"`
	Stale  bool           `json:"stale,omitempty"`
}

type earningsSymbolState struct {
	Resolution earningsResolution               `json:"resolution"`
	Providers  map[string]earningsProviderState `json:"providers"`
	UpdatedAt  time.Time                        `json:"updated_at"`
}

// earningsResolutionView is the cache's typed rulebook integration surface.
// Provider data is already redacted and ordered deterministically.
type earningsResolutionView struct {
	Status    string
	Reason    string
	Entry     earningsEntry
	Stale     bool
	Providers []rpc.EarningsProviderInfo
}

type earningsProviderFetchResult struct {
	Status  string
	Entry   earningsEntry
	Failure *rpc.SourceFailure
}

// The error return is local-log-only. Implementations must put only stable,
// allowlisted data in Result; raw upstream text never enters persistence/RPC.
type earningsProviderFetcher func(context.Context, string) (earningsProviderFetchResult, error)

type earningsCache struct {
	mu       sync.Mutex
	symbols  map[string]earningsSymbolState
	inflight map[string]bool
	store    *earningsStore
	client   *http.Client
	logf     func(format string, args ...any)
	clock    func() time.Time
	// fetchURL is swappable for tests; %s receives the provider symbol.
	fetchURL string

	secondaryProvider string
	secondaryFetch    earningsProviderFetcher
}

func newEarningsCache(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheCold(dir, logf)
	if loaded, err := c.store.loadLegacy(c.clock()); err != nil {
		c.logf("earnings cache load failed (cold start): %v", err)
	} else if loaded != nil {
		c.symbols = migrateEarningsV1(loaded, c.clock())
	}
	return c
}

// newEarningsCacheCold installs the legacy codec without reading it. Server
// construction runs before the persistence lock, so production uses this and
// leaves legacy reads exclusively to the unpublished cutover importer.
func newEarningsCacheCold(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheMemory(logf)
	c.store = &earningsStore{dir: dir}
	return c
}

func newEarningsCacheMemory(logf func(string, ...any)) *earningsCache {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &earningsCache{
		symbols:  map[string]earningsSymbolState{},
		inflight: map[string]bool{},
		client:   &http.Client{Timeout: earningsFetchTimeout},
		logf:     logf,
		clock:    time.Now,
		fetchURL: "https://api.nasdaq.com/api/analyst/%s/earnings-date",
	}
}

// setSecondaryProvider installs the approved independent provider. Production
// uses ibkr_wsh; the narrow hook also permits deterministic provider-agreement
// tests without network or Gateway access.
func (c *earningsCache) setSecondaryProvider(provider string, fetch earningsProviderFetcher) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != earningsWSHProvider {
		return fmt.Errorf("unsupported secondary earnings provider %q", provider)
	}
	if fetch == nil {
		return errors.New("secondary earnings provider fetch is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secondaryProvider = provider
	c.secondaryFetch = fetch
	return nil
}

// UseCoreStore replaces any legacy JSON projection loaded by construction
// with the current daemon.db document. Missing state initializes a cold v2
// document. Failure is returned before the authority pointer or in-memory
// projection changes.
func (c *earningsCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("earnings cache: nil cache")
	}
	if store == nil {
		return errors.New("earnings cache: nil corestore")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = &earningsStore{}
	}
	loaded, err := c.store.useCoreStore(store, c.clock())
	if err != nil {
		return err
	}
	c.symbols = loaded
	c.inflight = map[string]bool{}
	return nil
}

// nasdaqSymbol maps IBKR symbols to the provider's spelling: share classes
// use dots there ("BRK B" -> "BRK.B"). Unmappable symbols return "".
func nasdaqSymbol(sym string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" || strings.ContainsAny(sym, "/\\?%") {
		return ""
	}
	return strings.ReplaceAll(sym, " ", ".")
}

// get returns the aggregate usable date and whether its evidence is degraded
// or stale. Conflicts and all typed unknown outcomes return ok=false.
func (c *earningsCache) get(sym string) (entry earningsEntry, stale, ok bool) {
	view, ok := c.resolution(sym)
	if !ok || view.Status != rpc.EarningsStatusDate {
		return earningsEntry{}, false, false
	}
	return view.Entry, view.Stale, true
}

func (c *earningsCache) resolution(sym string) (earningsResolutionView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sym = strings.ToUpper(strings.TrimSpace(sym))
	state, ok := c.symbols[sym]
	if !ok {
		return earningsResolutionView{}, false
	}
	resolved := resolveEarningsProviders(state.Providers, c.clock())
	view := earningsResolutionView{Status: resolved.Status, Reason: resolved.Reason, Stale: resolved.Stale}
	if resolved.Entry != nil {
		view.Entry = *resolved.Entry
	}
	view.Providers = projectEarningsProviders(state.Providers)
	return view, true
}

// kickRefresh asynchronously refreshes symbols with at least one due provider.
// The request context is detached because the snapshot that triggered this
// background work may finish before the bounded provider calls do.
func (c *earningsCache) kickRefresh(ctx context.Context, syms []string) {
	now := c.clock()
	var todo []string
	c.mu.Lock()
	for _, sym := range syms {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || c.inflight[sym] {
			continue
		}
		state := c.symbols[sym]
		if !c.anyProviderDueLocked(state, now) {
			continue
		}
		c.inflight[sym] = true
		todo = append(todo, sym)
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	go c.refresh(context.WithoutCancel(ctx), todo)
}

func (c *earningsCache) providerNamesLocked() []string {
	providers := []string{earningsNasdaqProvider}
	if c.secondaryFetch != nil && c.secondaryProvider != "" {
		providers = append(providers, c.secondaryProvider)
	}
	return providers
}

func (c *earningsCache) anyProviderDueLocked(state earningsSymbolState, now time.Time) bool {
	for _, provider := range c.providerNamesLocked() {
		if earningsProviderDue(state.Providers[provider], now) {
			return true
		}
	}
	return false
}

func earningsProviderDue(state earningsProviderState, now time.Time) bool {
	if state.LastAttempt.AttemptedAt.IsZero() {
		return true
	}
	if state.LastAttempt.NextAttempt == nil {
		return true
	}
	return !now.Before(*state.LastAttempt.NextAttempt)
}

func (c *earningsCache) refresh(ctx context.Context, syms []string) {
	sem := make(chan struct{}, earningsFetchConcurrency)
	var wg sync.WaitGroup
	for _, sym := range syms {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			c.refreshOne(ctx, sym)
		})
	}
	wg.Wait()
}

type earningsCompletedProvider struct {
	provider string
	attempt  earningsProviderAttempt
	localErr error
}

func (c *earningsCache) refreshOne(ctx context.Context, sym string) {
	c.mu.Lock()
	now := c.clock()
	state := cloneEarningsSymbolState(c.symbols[sym])
	providers := c.providerNamesLocked()
	secondaryProvider, secondaryFetch := c.secondaryProvider, c.secondaryFetch
	c.mu.Unlock()

	var completed []earningsCompletedProvider
	for _, provider := range providers {
		if !earningsProviderDue(state.Providers[provider], now) {
			continue
		}
		attemptedAt := c.clock()
		var result earningsProviderFetchResult
		var err error
		switch provider {
		case earningsNasdaqProvider:
			result, err = c.fetchNasdaqProvider(ctx, sym)
		case secondaryProvider:
			result, err = secondaryFetch(ctx, sym)
		default:
			result = transportFailureResult(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageAuthorityPersist, false, c.clock())
			err = fmt.Errorf("unsupported earnings provider %q", provider)
		}
		completedAt := c.clock()
		completed = append(completed, earningsCompletedProvider{
			provider: provider,
			attempt:  normalizeEarningsAttempt(provider, sym, result, attemptedAt, completedAt),
			localErr: err,
		})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	defer delete(c.inflight, sym)
	if len(completed) == 0 {
		return
	}

	// Merge into the newest committed symbol snapshot. Other symbols may have
	// committed while provider calls were in flight; the store revision is
	// global and is intentionally consumed only under this lock.
	candidate := cloneEarningsSymbols(c.symbols)
	symbolState := cloneEarningsSymbolState(candidate[sym])
	if symbolState.Providers == nil {
		symbolState.Providers = map[string]earningsProviderState{}
	}
	for _, item := range completed {
		providerState := symbolState.Providers[item.provider]
		providerState.LastAttempt = item.attempt
		if item.attempt.Status == rpc.EarningsStatusDate && item.attempt.Entry != nil {
			entry := *item.attempt.Entry
			providerState.LastGood = &entry
		}
		symbolState.Providers[item.provider] = providerState
		if item.localErr != nil {
			c.logf("earnings provider %s fetch %s failed: %v", item.provider, sym, item.localErr)
		}
	}
	decisionAt := c.clock()
	symbolState.Resolution = resolveEarningsProviders(symbolState.Providers, decisionAt)
	symbolState.UpdatedAt = decisionAt
	candidate[sym] = symbolState

	observations, err := earningsProviderObservations(sym, completed)
	if err != nil {
		c.logf("earnings provider %s outcome encode failed: %v", sym, err)
		return
	}
	if err := c.store.commit(context.WithoutCancel(ctx), candidate, observations, decisionAt); err != nil {
		// Publishing an uncommitted attempt would make a transient memory result
		// outrun restart authority. SQLite health reports the authority failure.
		c.logf("earnings provider %s authority commit failed: %v", sym, err)
		return
	}
	c.symbols = candidate
}

func normalizeEarningsAttempt(provider, symbol string, result earningsProviderFetchResult, attemptedAt, completedAt time.Time) earningsProviderAttempt {
	status := result.Status
	if !validEarningsProviderStatus(status) {
		status = rpc.EarningsStatusTransportFailure
		result.Entry = earningsEntry{}
		result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, Retryable: false}
	}
	if completedAt.Before(attemptedAt) {
		completedAt = attemptedAt
	}
	if status == rpc.EarningsStatusDate {
		if result.Entry.ObservedAt.IsZero() {
			result.Entry.ObservedAt = completedAt
		}
		if validateEarningsRowShape(symbol, result.Entry) != nil {
			status = rpc.EarningsStatusFormatChange
			result.Entry = earningsEntry{}
			result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: earningsProviderDecodeStage(provider), Retryable: false}
		} else if result.Entry.Date < earningsCalendarDate(completedAt) {
			status = rpc.EarningsStatusNoDatePublished
			result.Entry = earningsEntry{}
			result.Failure = nil
		}
	}
	if (status == rpc.EarningsStatusFormatChange || status == rpc.EarningsStatusTransportFailure) && result.Failure == nil {
		result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: earningsProviderDecodeStage(provider), Retryable: false}
	}
	attempt := earningsProviderAttempt{Status: status, AttemptedAt: attemptedAt, CompletedAt: completedAt}
	if status == rpc.EarningsStatusDate {
		entry := result.Entry
		attempt.Entry = &entry
	} else if result.Failure != nil {
		failure := *result.Failure
		failure.FailedAt = completedAt
		if !validEarningsSourceFailure(failure) {
			failure = rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, FailedAt: completedAt, Retryable: false}
		}
		attempt.LastFailure = &failure
	}
	next := earningsNextAttempt(status, attempt.LastFailure, completedAt)
	attempt.NextAttempt = &next
	return attempt
}

func earningsProviderDecodeStage(provider string) string {
	if provider == earningsWSHProvider {
		return rpc.SourceFailureStageWSHDecode
	}
	return rpc.SourceFailureStageNasdaqSchema
}

func earningsNextAttempt(status string, failure *rpc.SourceFailure, completedAt time.Time) time.Time {
	switch status {
	case rpc.EarningsStatusDate, rpc.EarningsStatusNoDatePublished:
		return completedAt.Add(earningsFreshWindow)
	case rpc.EarningsStatusUnsupportedSecurity:
		return completedAt.Add(earningsTTL)
	case rpc.EarningsStatusFormatChange:
		return completedAt.Add(earningsFreshWindow)
	case rpc.EarningsStatusTransportFailure:
		if failure != nil && !failure.Retryable {
			return completedAt.Add(earningsFreshWindow)
		}
		return completedAt.Add(earningsFailureRetry)
	default:
		return completedAt.Add(earningsFailureRetry)
	}
}

var earningsAnnouncementRe = regexp.MustCompile(`:\s*([A-Z][a-z]{2} \d{1,2}, \d{4})\s*$`)

type earningsProviderError struct {
	status  string
	failure *rpc.SourceFailure
	detail  error
}

func (e *earningsProviderError) Error() string {
	if e == nil || e.detail == nil {
		return "earnings provider outcome"
	}
	return e.detail.Error()
}

func providerOutcomeError(status, code, stage string, retryable bool, detail error) error {
	var failure *rpc.SourceFailure
	if code != "" {
		failure = &rpc.SourceFailure{Code: code, Stage: stage, Retryable: retryable}
	}
	return &earningsProviderError{status: status, failure: failure, detail: detail}
}

func (c *earningsCache) fetchNasdaqProvider(ctx context.Context, sym string) (earningsProviderFetchResult, error) {
	entry, err := c.fetchOne(ctx, sym)
	if err == nil {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusDate, Entry: entry}, nil
	}
	if providerErr, ok := errors.AsType[*earningsProviderError](err); ok {
		return earningsProviderFetchResult{Status: providerErr.status, Failure: providerErr.failure}, err
	}
	return transportFailureResult(rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, c.clock()), err
}

func transportFailureResult(code, stage string, retryable bool, at time.Time) earningsProviderFetchResult {
	return earningsProviderFetchResult{
		Status:  rpc.EarningsStatusTransportFailure,
		Failure: &rpc.SourceFailure{Code: code, Stage: stage, FailedAt: at, Retryable: retryable},
	}
}

// fetchOne preserves the original focused fetch seam while returning typed
// provider errors that refreshOne can safely project into persistence.
func (c *earningsCache) fetchOne(ctx context.Context, sym string) (earningsEntry, error) {
	providerSymbol := nasdaqSymbol(sym)
	if providerSymbol == "" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusUnsupportedSecurity, "", "", false, errors.New("symbol unsupported by Nasdaq earnings provider"))
	}
	ctx, cancel := context.WithTimeout(ctx, earningsFetchTimeout)
	defer cancel()
	url := fmt.Sprintf(c.fetchURL, providerSymbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqRequest, false, err)
	}
	// Nasdaq's CDN rejects bare Go clients; this allowlisted browser header set
	// is the endpoint contract established by the original provider spike.
	for k, v := range map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 ibkr-earnings/1.0",
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "en-US,en;q=0.9",
		"Origin":          "https://www.nasdaq.com",
		"Referer":         "https://www.nasdaq.com/",
	} {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusUnsupportedSecurity, "", "", false, fmt.Errorf("status %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageNasdaqRequest, retryable, fmt.Errorf("status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, err)
	}
	return parseNasdaqEarnings(body, c.clock())
}

// parseNasdaqEarnings extracts the announcement date, session half, and
// estimated flag. Missing/null/empty announcement is an explicit no-date
// publication; non-empty unrecognized content is a format change.
func parseNasdaqEarnings(body []byte, now time.Time) (earningsEntry, error) {
	var payload struct {
		Data *struct {
			Announcement json.RawMessage `json:"announcement"`
			ReportText   string          `json:"reportText"`
		} `json:"data"`
		Status struct {
			RCode int `json:"rCode"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqDecode, false, err)
	}
	if payload.Data == nil {
		if payload.Status.RCode == http.StatusBadRequest || payload.Status.RCode == http.StatusNotFound {
			return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusUnsupportedSecurity, "", "", false, errors.New("nasdaq reports unsupported security"))
		}
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload has no data object"))
	}
	announcementRaw := payload.Data.Announcement
	if len(announcementRaw) == 0 || string(announcementRaw) == "null" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusNoDatePublished, "", "", false, errors.New("nasdaq published no earnings date"))
	}
	var announcement string
	if err := json.Unmarshal(announcementRaw, &announcement); err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, err)
	}
	announcement = strings.TrimSpace(announcement)
	if announcement == "" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusNoDatePublished, "", "", false, errors.New("nasdaq published no earnings date"))
	}
	m := earningsAnnouncementRe.FindStringSubmatch(announcement)
	if m == nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement format changed"))
	}
	t, err := time.Parse("Jan 2, 2006", m[1])
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, err)
	}
	if t.Format(time.DateOnly) < earningsCalendarDate(now) {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusNoDatePublished, "", "", false, errors.New("nasdaq announcement date has elapsed"))
	}
	report := strings.ToLower(payload.Data.ReportText)
	entry := earningsEntry{Date: t.Format(time.DateOnly), ObservedAt: now}
	switch {
	case strings.Contains(report, "after market close"):
		entry.TimeOfDay = "amc"
	case strings.Contains(report, "before market open"):
		entry.TimeOfDay = "bmo"
	}
	if strings.Contains(report, "expected*") || strings.Contains(report, "expected *") || strings.Contains(announcement, "*") {
		entry.Estimated = true
	}
	return entry, nil
}

func earningsCalendarDate(now time.Time) string {
	loc, err := time.LoadLocation("America/New_York")
	if err == nil {
		now = now.In(loc)
	}
	return now.Format(time.DateOnly)
}

func resolveEarningsProviders(providers map[string]earningsProviderState, now time.Time) earningsResolution {
	type candidate struct {
		entry    earningsEntry
		retained bool
	}
	var candidates []candidate
	statuses := map[string]int{}
	for _, state := range providers {
		attempt := state.LastAttempt
		statuses[attempt.Status]++
		switch attempt.Status {
		case rpc.EarningsStatusDate:
			if attempt.Entry != nil && usableEarningsEntry(*attempt.Entry, now) {
				candidates = append(candidates, candidate{entry: *attempt.Entry})
			}
		case rpc.EarningsStatusTransportFailure:
			if state.LastGood != nil && usableEarningsEntry(*state.LastGood, now) {
				candidates = append(candidates, candidate{entry: *state.LastGood, retained: true})
			}
		}
	}
	if len(candidates) > 0 {
		chosen := candidates[0].entry
		stale := false
		for _, candidate := range candidates {
			if candidate.entry.Date != chosen.Date || incompatibleEarningsSessions(candidate.entry.TimeOfDay, chosen.TimeOfDay) {
				return earningsResolution{Status: rpc.EarningsStatusConflictingSources, Reason: earningsReasonConflicting}
			}
			if chosen.TimeOfDay == "" {
				chosen.TimeOfDay = candidate.entry.TimeOfDay
			}
			chosen.Estimated = chosen.Estimated || candidate.entry.Estimated
			if candidate.entry.ObservedAt.Before(chosen.ObservedAt) {
				chosen.ObservedAt = candidate.entry.ObservedAt
			}
			stale = stale || candidate.retained || now.Sub(candidate.entry.ObservedAt) > earningsFreshWindow
		}
		reason := earningsReasonSingleSource
		if len(candidates) > 1 {
			reason = earningsReasonConsensus
		} else if candidates[0].retained {
			reason = earningsReasonRetainedLastGood
		}
		return earningsResolution{Status: rpc.EarningsStatusDate, Reason: reason, Entry: &chosen, Stale: stale}
	}

	if statuses[rpc.EarningsStatusFormatChange] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusFormatChange, Reason: rpc.EarningsStatusFormatChange}
	}
	if statuses[rpc.EarningsStatusTransportFailure] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusTransportFailure, Reason: rpc.EarningsStatusTransportFailure}
	}
	if statuses[rpc.EarningsStatusUnsupportedSecurity] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusUnsupportedSecurity, Reason: rpc.EarningsStatusUnsupportedSecurity}
	}
	if statuses[rpc.EarningsStatusNoDatePublished] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished}
	}
	if statuses[rpc.EarningsStatusDate] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: earningsReasonDateElapsed}
	}
	return earningsResolution{Status: rpc.EarningsStatusTransportFailure, Reason: rpc.EarningsStatusTransportFailure}
}

func usableEarningsEntry(entry earningsEntry, now time.Time) bool {
	if entry.ObservedAt.IsZero() || now.Before(entry.ObservedAt) || now.Sub(entry.ObservedAt) > earningsTTL {
		return false
	}
	return entry.Date >= earningsCalendarDate(now)
}

func incompatibleEarningsSessions(a, b string) bool {
	return a != "" && b != "" && a != b
}

func projectEarningsProviders(providers map[string]earningsProviderState) []rpc.EarningsProviderInfo {
	names := make([]string, 0, len(providers))
	for provider := range providers {
		names = append(names, provider)
	}
	sort.Strings(names)
	out := make([]rpc.EarningsProviderInfo, 0, len(names))
	for _, provider := range names {
		state := providers[provider]
		attempt := state.LastAttempt
		info := rpc.EarningsProviderInfo{
			Provider: provider, Status: attempt.Status, AttemptedAt: attempt.AttemptedAt,
			NextAttempt: cloneTimePointer(attempt.NextAttempt), LastFailure: cloneEarningsSourceFailure(attempt.LastFailure),
		}
		if attempt.Entry != nil {
			info.Date = attempt.Entry.Date
			info.TimeOfDay = attempt.Entry.TimeOfDay
			info.Estimated = attempt.Entry.Estimated
			info.ObservedAt = attempt.Entry.ObservedAt
		}
		if state.LastGood != nil {
			info.LastGoodDate = state.LastGood.Date
		}
		out = append(out, info)
	}
	return out
}

func earningsProviderObservations(sym string, completed []earningsCompletedProvider) ([]corestore.ObservationInput, error) {
	observations := make([]corestore.ObservationInput, 0, len(completed))
	for _, item := range completed {
		payload, err := json.Marshal(struct {
			Version  int                     `json:"version"`
			Symbol   string                  `json:"symbol"`
			Provider string                  `json:"provider"`
			Attempt  earningsProviderAttempt `json:"attempt"`
		}{earningsPersistVersion, sym, item.provider, item.attempt})
		if err != nil {
			return nil, fmt.Errorf("encode %s earnings observation: %w", item.provider, err)
		}
		metadata, err := json.Marshal(struct {
			Version  int    `json:"version"`
			Provider string `json:"provider"`
			Status   string `json:"status"`
		}{earningsPersistVersion, item.provider, item.attempt.Status})
		if err != nil {
			return nil, fmt.Errorf("encode %s earnings metadata: %w", item.provider, err)
		}
		observations = append(observations, corestore.ObservationInput{
			ScopeKey: earningsAuthorityScope, Source: earningsObservationSourceForProvider(item.provider),
			Kind: earningsProviderObservationKind, ObservedAt: item.attempt.CompletedAt,
			ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	return observations, nil
}

func earningsObservationSourceForProvider(provider string) string {
	if provider == earningsWSHProvider {
		return earningsWSHObservationSource
	}
	return earningsObservationSource
}

func validEarningsProviderStatus(status string) bool {
	switch status {
	case rpc.EarningsStatusDate, rpc.EarningsStatusNoDatePublished,
		rpc.EarningsStatusUnsupportedSecurity, rpc.EarningsStatusFormatChange,
		rpc.EarningsStatusTransportFailure:
		return true
	default:
		return false
	}
}

func validEarningsSourceFailure(failure rpc.SourceFailure) bool {
	return rpc.ValidSourceFailure(&failure) && !failure.FailedAt.IsZero()
}

// earningsStore persists v2 state across restarts. The JSON save/load methods
// remain v1-only for the sealed legacy cutover path and isolated tests.
type earningsStore struct {
	dir       string
	authority *corestore.Store
	revision  int64
}

func (s *earningsStore) useCoreStore(store *corestore.Store, now time.Time) (map[string]earningsSymbolState, error) {
	if s == nil {
		return nil, errors.New("earnings store: nil store")
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil {
		return nil, fmt.Errorf("read earnings authority: %w", err)
	}
	loaded := map[string]earningsSymbolState{}
	if !ok {
		raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
		if err != nil {
			return nil, fmt.Errorf("encode initial earnings authority: %w", err)
		}
		doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize earnings authority: %w", err)
		}
	} else {
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(doc.JSON, &header); err != nil {
			return nil, fmt.Errorf("decode earnings authority header: %w", err)
		}
		switch header.Version {
		case earningsLegacyVersion:
			legacy, err := decodeEarningsEnvelopeV1(doc.JSON, now, true)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v1: %w", err)
			}
			loaded = migrateEarningsV1(legacy, now)
			raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
			if err != nil {
				return nil, fmt.Errorf("encode migrated earnings authority: %w", err)
			}
			doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
			if err != nil {
				return nil, fmt.Errorf("persist migrated earnings authority: %w", err)
			}
		case earningsPersistVersion:
			loaded, err = decodeEarningsEnvelopeV2(doc.JSON, now)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v2: %w", err)
			}
		default:
			return nil, fmt.Errorf("decode earnings authority: unsupported version %d", header.Version)
		}
	}
	s.authority = store
	s.revision = doc.Revision
	return loaded, nil
}

func (s *earningsStore) commit(ctx context.Context, symbols map[string]earningsSymbolState, observations []corestore.ObservationInput, now time.Time) error {
	if s == nil {
		return errors.New("earnings store: nil store")
	}
	if s.authority == nil {
		entries := resolvedEarningsEntries(symbols, now)
		return s.save(entries)
	}
	if err := validateEarningsSymbols(symbols, now); err != nil {
		return err
	}
	payload, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: symbols})
	if err != nil {
		return fmt.Errorf("encode earnings authority: %w", err)
	}
	update := corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
		ExpectedRevision: s.revision, JSON: payload,
	}
	var saved corestore.StateDocument
	if len(observations) == 0 {
		saved, err = s.authority.CompareAndSwapStateDocument(ctx, update)
	} else {
		saved, _, err = s.authority.CompareAndSwapStateDocumentWithObservations(ctx, update, observations)
	}
	if err != nil {
		return fmt.Errorf("commit earnings authority: %w", err)
	}
	s.revision = saved.Revision
	return nil
}

func (s *earningsStore) loadLegacy(now time.Time) (map[string]earningsEntry, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, earningsStoreFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read earnings cache: %w", err)
	}
	return decodeEarningsEnvelopeV1(data, now, false)
}

func decodeEarningsEnvelopeV1(data []byte, now time.Time, strict bool) (map[string]earningsEntry, error) {
	var env earningsPersistEnvelopeV1
	var err error
	if strict {
		err = decodeStrictMarketEventJSON(data, &env)
	} else {
		err = json.Unmarshal(data, &env)
	}
	if err != nil {
		return nil, fmt.Errorf("decode earnings cache: %w", err)
	}
	if env.Version != earningsLegacyVersion {
		if strict {
			return nil, fmt.Errorf("invalid earnings version %d", env.Version)
		}
		return nil, nil
	}
	if strict && env.Entries == nil {
		return nil, errors.New("earnings authority has no entries map")
	}
	entries := make(map[string]earningsEntry, len(env.Entries))
	for sym, entry := range env.Entries {
		if err := validateEarningsRow(sym, entry, now); err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		if now.Sub(entry.ObservedAt) <= earningsTTL {
			entries[strings.ToUpper(sym)] = entry
		}
	}
	return entries, nil
}

func decodeEarningsEnvelopeV2(data []byte, now time.Time) (map[string]earningsSymbolState, error) {
	var env earningsPersistEnvelope
	if err := decodeStrictMarketEventJSON(data, &env); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if env.Version != earningsPersistVersion {
		return nil, fmt.Errorf("invalid earnings version %d", env.Version)
	}
	if env.Symbols == nil {
		return nil, errors.New("earnings authority has no symbols map")
	}
	if err := validateEarningsSymbols(env.Symbols, now); err != nil {
		return nil, err
	}
	return cloneEarningsSymbols(env.Symbols), nil
}

func validateEarningsSymbols(symbols map[string]earningsSymbolState, now time.Time) error {
	for symbol, state := range symbols {
		canonical := strings.ToUpper(strings.TrimSpace(symbol))
		if canonical == "" || canonical != symbol || state.Providers == nil || state.UpdatedAt.IsZero() || now.Before(state.UpdatedAt) {
			return fmt.Errorf("invalid earnings symbol state %q", symbol)
		}
		if !validAggregateEarningsStatus(state.Resolution.Status) {
			return fmt.Errorf("invalid earnings resolution for %q", symbol)
		}
		recomputed := resolveEarningsProviders(state.Providers, state.UpdatedAt)
		if !sameEarningsResolution(state.Resolution, recomputed) {
			return fmt.Errorf("inconsistent earnings resolution for %q", symbol)
		}
		if state.Resolution.Status == rpc.EarningsStatusDate {
			if state.Resolution.Entry == nil || validateEarningsRowShape(symbol, *state.Resolution.Entry) != nil {
				return fmt.Errorf("invalid earnings resolution date for %q", symbol)
			}
		} else if state.Resolution.Entry != nil {
			return fmt.Errorf("unresolved earnings state carries a date for %q", symbol)
		}
		for provider, providerState := range state.Providers {
			if provider != earningsNasdaqProvider && provider != earningsWSHProvider {
				return fmt.Errorf("invalid earnings provider %q", provider)
			}
			if err := validateEarningsProviderState(symbol, provider, providerState, now); err != nil {
				return err
			}
			if state.UpdatedAt.Before(providerState.LastAttempt.CompletedAt) {
				return fmt.Errorf("earnings resolution predates %s attempt for %q", provider, symbol)
			}
		}
	}
	return nil
}

func validAggregateEarningsStatus(status string) bool {
	return validEarningsProviderStatus(status) || status == rpc.EarningsStatusConflictingSources
}

func sameEarningsResolution(a, b earningsResolution) bool {
	if a.Status != b.Status || a.Reason != b.Reason || a.Stale != b.Stale {
		return false
	}
	if a.Entry == nil || b.Entry == nil {
		return a.Entry == nil && b.Entry == nil
	}
	return *a.Entry == *b.Entry
}

func validateEarningsProviderState(symbol, provider string, state earningsProviderState, now time.Time) error {
	attempt := state.LastAttempt
	if !validEarningsProviderStatus(attempt.Status) || attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) || now.Before(attempt.CompletedAt) {
		return fmt.Errorf("invalid %s earnings attempt for %q", provider, symbol)
	}
	if attempt.NextAttempt == nil || attempt.NextAttempt.Before(attempt.CompletedAt) {
		return fmt.Errorf("invalid %s earnings retry for %q", provider, symbol)
	}
	if attempt.Status == rpc.EarningsStatusDate {
		if attempt.Entry == nil || validateEarningsRowShape(symbol, *attempt.Entry) != nil || attempt.LastFailure != nil {
			return fmt.Errorf("invalid %s earnings date for %q", provider, symbol)
		}
	} else if attempt.Entry != nil {
		return fmt.Errorf("unresolved %s earnings attempt carries a date for %q", provider, symbol)
	}
	if attempt.Status == rpc.EarningsStatusTransportFailure || attempt.Status == rpc.EarningsStatusFormatChange {
		if attempt.LastFailure == nil || !validEarningsSourceFailure(*attempt.LastFailure) || !attempt.LastFailure.FailedAt.Equal(attempt.CompletedAt) {
			return fmt.Errorf("invalid %s earnings failure for %q", provider, symbol)
		}
	} else if attempt.LastFailure != nil {
		return fmt.Errorf("semantic %s earnings outcome carries a failure for %q", provider, symbol)
	}
	if state.LastGood != nil {
		if err := validateEarningsRowShape(symbol, *state.LastGood); err != nil || now.Before(state.LastGood.ObservedAt) {
			return fmt.Errorf("invalid %s earnings last-good for %q", provider, symbol)
		}
	}
	return nil
}

func validateEarningsRow(sym string, entry earningsEntry, now time.Time) error {
	if err := validateEarningsRowShape(sym, entry); err != nil {
		return err
	}
	if now.Before(entry.ObservedAt) {
		return fmt.Errorf("invalid earnings row %q", sym)
	}
	return nil
}

func validateEarningsRowShape(sym string, entry earningsEntry) error {
	canonical := strings.ToUpper(strings.TrimSpace(sym))
	if canonical == "" || canonical != sym || nasdaqSymbol(canonical) == "" || entry.ObservedAt.IsZero() {
		return fmt.Errorf("invalid earnings row %q", sym)
	}
	parsed, err := time.Parse(time.DateOnly, entry.Date)
	if err != nil || parsed.Format(time.DateOnly) != entry.Date {
		return fmt.Errorf("invalid earnings date for %q", sym)
	}
	switch entry.TimeOfDay {
	case "", "amc", "bmo":
		return nil
	default:
		return fmt.Errorf("invalid earnings session for %q", sym)
	}
}

// save writes only the sealed v1 JSON format used by the cutover importer.
func (s *earningsStore) save(entries map[string]earningsEntry) error {
	if s.authority != nil {
		return errors.New("legacy earnings save unavailable after SQLite authority attachment")
	}
	env := earningsPersistEnvelopeV1{Version: earningsLegacyVersion, Entries: entries}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, earningsStoreFilename)
	tmp, err := os.CreateTemp(s.dir, earningsStoreFilename+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode %s: %w", earningsStoreFilename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", earningsStoreFilename, err)
	}
	return nil
}

func migrateEarningsV1(entries map[string]earningsEntry, now time.Time) map[string]earningsSymbolState {
	symbols := make(map[string]earningsSymbolState, len(entries))
	for symbol, entry := range entries {
		next := entry.ObservedAt.Add(earningsFreshWindow)
		entryCopy := entry
		provider := earningsProviderState{
			LastAttempt: earningsProviderAttempt{
				Status: rpc.EarningsStatusDate, Entry: &entryCopy,
				AttemptedAt: entry.ObservedAt, CompletedAt: entry.ObservedAt, NextAttempt: &next,
			},
			LastGood: &entryCopy,
		}
		providers := map[string]earningsProviderState{earningsNasdaqProvider: provider}
		symbols[symbol] = earningsSymbolState{
			Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now,
		}
	}
	return symbols
}

func resolvedEarningsEntries(symbols map[string]earningsSymbolState, now time.Time) map[string]earningsEntry {
	entries := map[string]earningsEntry{}
	for symbol, state := range symbols {
		resolution := resolveEarningsProviders(state.Providers, now)
		if resolution.Status == rpc.EarningsStatusDate && resolution.Entry != nil {
			entries[symbol] = *resolution.Entry
		}
	}
	return entries
}

func cloneEarningsSymbols(in map[string]earningsSymbolState) map[string]earningsSymbolState {
	out := make(map[string]earningsSymbolState, len(in))
	for symbol, state := range in {
		out[symbol] = cloneEarningsSymbolState(state)
	}
	return out
}

func cloneEarningsSymbolState(in earningsSymbolState) earningsSymbolState {
	out := in
	if in.Resolution.Entry != nil {
		entry := *in.Resolution.Entry
		out.Resolution.Entry = &entry
	}
	out.Providers = make(map[string]earningsProviderState, len(in.Providers))
	for provider, state := range in.Providers {
		copyState := state
		copyState.LastAttempt.Entry = cloneEarningsEntry(state.LastAttempt.Entry)
		copyState.LastAttempt.NextAttempt = cloneTimePointer(state.LastAttempt.NextAttempt)
		copyState.LastAttempt.LastFailure = cloneEarningsSourceFailure(state.LastAttempt.LastFailure)
		copyState.LastGood = cloneEarningsEntry(state.LastGood)
		out.Providers[provider] = copyState
	}
	return out
}

func cloneEarningsEntry(entry *earningsEntry) *earningsEntry {
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	return &copyEntry
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneEarningsSourceFailure(failure *rpc.SourceFailure) *rpc.SourceFailure {
	if failure == nil {
		return nil
	}
	copyFailure := *failure
	return &copyFailure
}
