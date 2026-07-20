package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
)

const marketEventStateVersion = 1

const (
	marketEventRegSHOScope                    = "market/events/reg-sho"
	marketEventRegSHOStateKind                = "reg_sho.current.v1"
	marketEventRegSHOObservationKind          = "reg_sho.snapshot.v1"
	marketEventRegSHOSource                   = "nasdaq.reg_sho_threshold"
	marketEventHaltsScope                     = "market/events/halts"
	marketEventHaltsStateKind                 = "trading_halts.current.v1"
	marketEventHaltsObservationKind           = "trading_halts.snapshot.v1"
	marketEventHaltsSource                    = "nasdaq.trade_halts"
	marketEventBorrowFeesScope                = "market/events/borrow-fees"
	marketEventBorrowFeesStateKind            = "borrow_fees.current.v1"
	marketEventBorrowFeesObservationKind      = "borrow_fees.snapshot.v1"
	marketEventBorrowFeesSource               = "ibkr.short_stock_availability"
	marketEventBorrowInventoryScope           = "market/events/borrow-inventory"
	marketEventBorrowInventoryStateKind       = "borrow_inventory.current.v1"
	marketEventBorrowInventoryObservationKind = "borrow_inventory.snapshot.v1"
	marketEventBorrowInventorySource          = "ibkr.tws.generic_tick_236"
)

type marketEventRegSHOState struct {
	Version int                    `json:"version"`
	Entry   marketEventRegSHOEntry `json:"entry"`
}

type marketEventHaltsState struct {
	Version int                   `json:"version"`
	Entry   marketEventHaltsEntry `json:"entry"`
}

type marketEventBorrowFeesState struct {
	Version int                       `json:"version"`
	Entry   marketEventBorrowFeeEntry `json:"entry"`
}

type marketEventBorrowInventoryState struct {
	Version    int                                         `json:"version"`
	ObservedAt time.Time                                   `json:"observed_at"`
	Records    map[string]marketEventBorrowInventoryRecord `json:"records"`
}

type marketEventBorrowInventoryRecord struct {
	Symbol          string    `json:"symbol"`
	ShortableShares int64     `json:"shortable_shares"`
	AsOf            time.Time `json:"as_of"`
	DataType        string    `json:"data_type,omitempty"`
	Delayed         bool      `json:"delayed,omitempty"`
}

// UseCoreStore atomically swaps the cache projection to daemon.db. Every
// existing document is decoded and validated before any in-memory value or
// authority pointer changes. Retry/backoff and negative-absence state are
// intentionally reset: they are ephemeral control state, not observations.
func (c *marketEventCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("market events: nil cache")
	}
	if store == nil {
		return errors.New("market events: nil corestore")
	}
	regSHO, err := loadMarketEventRegSHO(store)
	if err != nil {
		return err
	}
	halts, err := loadMarketEventHalts(store)
	if err != nil {
		return err
	}
	borrowFees, err := loadMarketEventBorrowFees(store)
	if err != nil {
		return err
	}
	if err := validateStoredBorrowInventory(store); err != nil {
		return err
	}
	c.mu.Lock()
	c.authority = store
	c.regSHO = regSHO
	c.halts = halts
	c.borrowFees = borrowFees
	c.shortableAbsent = nil
	c.regSHOFailedAt = time.Time{}
	c.haltsFailedAt = time.Time{}
	c.borrowFeesFailedAt = time.Time{}
	c.mu.Unlock()
	return nil
}

func loadMarketEventRegSHO(store *corestore.Store) (marketEventRegSHOEntry, error) {
	raw, ok, err := loadMarketState(store, marketEventRegSHOScope, marketEventRegSHOStateKind)
	if err != nil || !ok {
		return marketEventRegSHOEntry{}, err
	}
	var state marketEventRegSHOState
	if err := json.Unmarshal(raw, &state); err != nil {
		return marketEventRegSHOEntry{}, fmt.Errorf("decode Reg SHO authority: %w", err)
	}
	if state.Version != marketEventStateVersion {
		return marketEventRegSHOEntry{}, fmt.Errorf("invalid Reg SHO authority version %d", state.Version)
	}
	if err := validateRegSHOEntry(state.Entry); err != nil {
		return marketEventRegSHOEntry{}, fmt.Errorf("validate Reg SHO authority: %w", err)
	}
	return cloneRegSHOEntry(state.Entry), nil
}

func loadMarketEventHalts(store *corestore.Store) (marketEventHaltsEntry, error) {
	raw, ok, err := loadMarketState(store, marketEventHaltsScope, marketEventHaltsStateKind)
	if err != nil || !ok {
		return marketEventHaltsEntry{}, err
	}
	var state marketEventHaltsState
	if err := json.Unmarshal(raw, &state); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("decode trading-halts authority: %w", err)
	}
	if state.Version != marketEventStateVersion {
		return marketEventHaltsEntry{}, fmt.Errorf("invalid trading-halts authority version %d", state.Version)
	}
	if err := validateHaltsEntry(state.Entry); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("validate trading-halts authority: %w", err)
	}
	return cloneHaltsEntry(state.Entry), nil
}

func loadMarketEventBorrowFees(store *corestore.Store) (marketEventBorrowFeeEntry, error) {
	raw, ok, err := loadMarketState(store, marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
	if err != nil || !ok {
		return marketEventBorrowFeeEntry{}, err
	}
	var state marketEventBorrowFeesState
	if err := json.Unmarshal(raw, &state); err != nil {
		return marketEventBorrowFeeEntry{}, fmt.Errorf("decode borrow-fees authority: %w", err)
	}
	if state.Version != marketEventStateVersion {
		return marketEventBorrowFeeEntry{}, fmt.Errorf("invalid borrow-fees authority version %d", state.Version)
	}
	if err := validateBorrowFeeEntry(state.Entry); err != nil {
		return marketEventBorrowFeeEntry{}, fmt.Errorf("validate borrow-fees authority: %w", err)
	}
	return cloneBorrowFeeEntry(state.Entry), nil
}

func validateStoredBorrowInventory(store *corestore.Store) error {
	raw, ok, err := loadMarketState(store, marketEventBorrowInventoryScope, marketEventBorrowInventoryStateKind)
	if err != nil || !ok {
		return err
	}
	var state marketEventBorrowInventoryState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode borrow-inventory authority: %w", err)
	}
	if err := validateBorrowInventoryState(state); err != nil {
		return fmt.Errorf("validate borrow-inventory authority: %w", err)
	}
	return nil
}

func (c *marketEventCache) persistRegSHO(ctx context.Context, entry marketEventRegSHOEntry) error {
	if c.authority == nil {
		return nil
	}
	if err := validateRegSHOEntry(entry); err != nil {
		return err
	}
	state := marketEventRegSHOState{Version: marketEventStateVersion, Entry: cloneRegSHOEntry(entry)}
	return saveMarketEventState(ctx, c.authority, marketEventRegSHOScope, marketEventRegSHOStateKind, marketEventRegSHOSource, marketEventRegSHOObservationKind, entry.FetchedAt, state, len(entry.Symbols), entry.SourceURL)
}

func (c *marketEventCache) persistHalts(ctx context.Context, entry marketEventHaltsEntry) error {
	if c.authority == nil {
		return nil
	}
	if err := validateHaltsEntry(entry); err != nil {
		return err
	}
	entry = normalizeHaltsEntry(entry)
	state := marketEventHaltsState{Version: marketEventStateVersion, Entry: entry}
	return saveMarketEventState(ctx, c.authority, marketEventHaltsScope, marketEventHaltsStateKind, marketEventHaltsSource, marketEventHaltsObservationKind, entry.FetchedAt, state, len(entry.Records), entry.SourceURL)
}

func (c *marketEventCache) persistBorrowFees(ctx context.Context, entry marketEventBorrowFeeEntry) error {
	if c.authority == nil {
		return nil
	}
	if err := validateBorrowFeeEntry(entry); err != nil {
		return err
	}
	state := marketEventBorrowFeesState{Version: marketEventStateVersion, Entry: cloneBorrowFeeEntry(entry)}
	return saveMarketEventState(ctx, c.authority, marketEventBorrowFeesScope, marketEventBorrowFeesStateKind, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind, entry.FetchedAt, state, len(entry.Symbols), entry.SourceURL)
}

func (c *marketEventCache) persistBorrowInventory(ctx context.Context, observedAt time.Time, records map[string]marketEventBorrowInventoryRecord) error {
	if c.authority == nil || len(records) == 0 {
		return nil
	}
	state := marketEventBorrowInventoryState{Version: marketEventStateVersion, ObservedAt: observedAt, Records: records}
	if err := validateBorrowInventoryState(state); err != nil {
		return err
	}
	return saveMarketEventState(ctx, c.authority, marketEventBorrowInventoryScope, marketEventBorrowInventoryStateKind, marketEventBorrowInventorySource, marketEventBorrowInventoryObservationKind, observedAt, state, len(records), "IBKR generic tick 236")
}

func saveMarketEventState(ctx context.Context, store *corestore.Store, scope, stateKind, source, observationKind string, observedAt time.Time, value any, records int, sourceURL string) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(struct {
		Version   int    `json:"version"`
		Records   int    `json:"records"`
		SourceURL string `json:"source_url,omitempty"`
	}{marketEventStateVersion, records, sourceURL})
	if err != nil {
		return err
	}
	return saveMarketStateContext(ctx, store, scope, stateKind, corestore.ObservationInput{
		ScopeKey: scope, Source: source, Kind: observationKind,
		ObservedAt: observedAt, ContentType: "application/json",
		Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
	})
}

func validateRegSHOEntry(entry marketEventRegSHOEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || entry.Symbols == nil {
		return errors.New("invalid Reg SHO envelope")
	}
	for key, row := range entry.Symbols {
		if key == "" || key != normSym(key) || row.Symbol != key {
			return fmt.Errorf("invalid Reg SHO row %q", key)
		}
	}
	return nil
}

func validateHaltsEntry(entry marketEventHaltsEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || entry.Records == nil {
		return errors.New("invalid trading-halts envelope")
	}
	for i, row := range entry.Records {
		if row.Symbol == "" || row.Symbol != normSym(row.Symbol) || row.HaltedAt.IsZero() || strings.TrimSpace(row.ReasonCode) == "" {
			return fmt.Errorf("invalid trading-halts row %d", i)
		}
	}
	return nil
}

func validateBorrowFeeEntry(entry marketEventBorrowFeeEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || entry.Symbols == nil {
		return errors.New("invalid borrow-fees envelope")
	}
	for key, row := range entry.Symbols {
		if key == "" || key != normSym(key) || row.Symbol != key || row.Available < 0 {
			return fmt.Errorf("invalid borrow-fees row %q", key)
		}
	}
	return nil
}

func validateBorrowInventoryState(state marketEventBorrowInventoryState) error {
	if state.Version != marketEventStateVersion || state.ObservedAt.IsZero() || len(state.Records) == 0 {
		return errors.New("invalid borrow-inventory envelope")
	}
	for key, row := range state.Records {
		if key == "" || key != normSym(key) || row.Symbol != key || row.ShortableShares < 0 || row.AsOf.IsZero() {
			return fmt.Errorf("invalid borrow-inventory row %q", key)
		}
	}
	return nil
}

func normalizeHaltsEntry(entry marketEventHaltsEntry) marketEventHaltsEntry {
	entry = cloneHaltsEntry(entry)
	slices.SortFunc(entry.Records, func(a, b marketEventHaltRecord) int {
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := a.HaltedAt.Compare(b.HaltedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ReasonCode, b.ReasonCode)
	})
	return entry
}
