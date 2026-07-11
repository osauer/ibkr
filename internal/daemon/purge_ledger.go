package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	purgeLedgerKind          = "ibkr.purge_ledger"
	purgeLedgerSchemaVersion = "purge-ledger-v2"

	purgeLedgerStatusActive   = "active"
	purgeLedgerStatusRestored = "restored"

	purgeRestoreSource = "purge_restore"
)

type purgeLedgerStore struct {
	Path string
	mu   sync.Mutex
	now  func() time.Time
}

type purgeLedgerFile struct {
	Kind          string           `json:"kind"`
	SchemaVersion string           `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Rows          []purgeLedgerRow `json:"rows"`
}

type purgeLedgerRow struct {
	LegID               string                          `json:"leg_id"`
	PurgeID             string                          `json:"purge_id,omitempty"`
	Symbol              string                          `json:"symbol"`
	SecType             string                          `json:"sec_type"`
	Contract            rpc.ContractParams              `json:"contract"`
	Account             string                          `json:"account,omitempty"`
	Mode                string                          `json:"mode,omitempty"`
	Currency            string                          `json:"currency,omitempty"`
	OriginalSide        string                          `json:"original_side"`
	OriginalQuantity    float64                         `json:"original_quantity"`
	PurgeAction         string                          `json:"purge_action"`
	RestoreAction       string                          `json:"restore_action"`
	Multiplier          int                             `json:"multiplier"`
	PurgedQuantity      float64                         `json:"purged_quantity"`
	RestoredQuantity    float64                         `json:"restored_quantity"`
	RemainingQuantity   float64                         `json:"remaining_quantity"`
	PurgeValue          float64                         `json:"purge_value,omitempty"`
	RestoreValue        float64                         `json:"restore_value,omitempty"`
	ShadowPnL           float64                         `json:"shadow_pnl,omitempty"`
	Status              string                          `json:"status"`
	LastPurgeOrderRef   string                          `json:"last_purge_order_ref,omitempty"`
	LastRestoreOrderRef string                          `json:"last_restore_order_ref,omitempty"`
	CreatedAt           time.Time                       `json:"created_at"`
	UpdatedAt           time.Time                       `json:"updated_at"`
	Warnings            []string                        `json:"warnings,omitempty"`
	OrderFills          map[string]purgeLedgerOrderFill `json:"order_fills,omitempty"`
}

type purgeLedgerOrderFill struct {
	Source       string  `json:"source"`
	OrderRef     string  `json:"order_ref"`
	Filled       float64 `json:"filled"`
	AvgFillPrice float64 `json:"avg_fill_price,omitempty"`
}

func defaultPurgeLedgerPath() (string, error) {
	return defaultTradingStatePath("purge-ledger.json")
}

func newPurgeLedgerStore(path string, now func() time.Time) *purgeLedgerStore {
	return &purgeLedgerStore{Path: path, now: now}
}

func (s *purgeLedgerStore) Snapshot(scope brokerStateScope, purgeID string) ([]rpc.PurgeLedgerRow, rpc.PurgeLedgerTotals, error) {
	if s == nil {
		return nil, rpc.PurgeLedgerTotals{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger, err := s.loadLocked()
	if err != nil {
		return nil, rpc.PurgeLedgerTotals{}, err
	}
	rows := make([]rpc.PurgeLedgerRow, 0, len(ledger.Rows))
	for _, row := range ledger.Rows {
		if !purgeLedgerRowMatchesBrokerScope(row, scope) {
			continue
		}
		if purgeID != "" && !strings.EqualFold(row.PurgeID, purgeID) {
			continue
		}
		rows = append(rows, purgeLedgerRowToRPC(row))
	}
	sortPurgeLedgerRows(rows)
	return rows, purgeLedgerTotals(rows), nil
}

func (s *purgeLedgerStore) AllRows() ([]purgeLedgerRow, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return slices.Clone(ledger.Rows), nil
}

func (s *purgeLedgerStore) ApplyOrderFill(ev orderJournalEvent) error {
	if s == nil {
		return nil
	}
	if ev.Filled <= 0 || ev.OrderRef == "" {
		return nil
	}
	switch ev.Source {
	case purgeExecuteSource, purgeRestoreSource:
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger, err := s.loadLocked()
	if err != nil {
		return err
	}
	now := s.currentTime()
	changed := applyPurgeLedgerFill(&ledger, ev, now)
	if !changed {
		return nil
	}
	ledger.UpdatedAt = now
	return s.saveLocked(ledger)
}

func applyPurgeLedgerFill(ledger *purgeLedgerFile, ev orderJournalEvent, now time.Time) bool {
	if ledger == nil {
		return false
	}
	legID := strings.TrimSpace(ev.LegID)
	if legID == "" {
		legID = purgeLegIDForContract(contractParamsFromJournalEvent(ev))
	}
	idx := slices.IndexFunc(ledger.Rows, func(row purgeLedgerRow) bool {
		return row.LegID == legID &&
			strings.EqualFold(row.Account, ev.Account) &&
			strings.EqualFold(row.Mode, ev.Mode)
	})
	if idx < 0 {
		if ev.Source != purgeExecuteSource {
			return false
		}
		ledger.Rows = append(ledger.Rows, purgeLedgerRowFromPurgeFill(ev, legID, now))
		idx = len(ledger.Rows) - 1
	}
	row := &ledger.Rows[idx]
	if row.OrderFills == nil {
		row.OrderFills = map[string]purgeLedgerOrderFill{}
	}
	prior := row.OrderFills[ev.OrderRef]
	if ev.Filled <= prior.Filled+1e-9 {
		return false
	}
	delta := ev.Filled - prior.Filled
	price := purgeLedgerDeltaPrice(ev, prior, delta)
	if price <= 0 {
		row.Warnings = appendPurgeLedgerUnique(row.Warnings, "fill callback did not include a usable fill price")
	}
	row.OrderFills[ev.OrderRef] = purgeLedgerOrderFill{
		Source:       ev.Source,
		OrderRef:     ev.OrderRef,
		Filled:       ev.Filled,
		AvgFillPrice: purgeLedgerEventAvgPrice(ev, price),
	}
	switch ev.Source {
	case purgeExecuteSource:
		applyPurgeFillToLedgerRow(row, ev, delta, price, now)
	case purgeRestoreSource:
		applyRestoreFillToLedgerRow(row, ev, delta, price, now)
	}
	normalizePurgeLedgerRow(row)
	return true
}

func purgeLedgerRowFromPurgeFill(ev orderJournalEvent, legID string, now time.Time) purgeLedgerRow {
	contract := contractParamsFromJournalEvent(ev)
	originalSide := purgeOriginalSideLong
	restoreAction := rpc.OrderActionBuy
	originalQty := math.Abs(ev.Quantity)
	if strings.EqualFold(ev.Action, rpc.OrderActionBuy) {
		originalSide = purgeOriginalSideShort
		restoreAction = rpc.OrderActionSell
		originalQty = -math.Abs(ev.Quantity)
	}
	return purgeLedgerRow{
		LegID:             legID,
		PurgeID:           ev.PurgeID,
		Symbol:            contract.Symbol,
		SecType:           contract.SecType,
		Contract:          contract,
		Account:           ev.Account,
		Mode:              ev.Mode,
		Currency:          contract.Currency,
		OriginalSide:      originalSide,
		OriginalQuantity:  originalQty,
		PurgeAction:       strings.ToUpper(strings.TrimSpace(ev.Action)),
		RestoreAction:     restoreAction,
		Multiplier:        contractMultiplier(contract),
		Status:            purgeLedgerStatusActive,
		CreatedAt:         now,
		UpdatedAt:         now,
		LastPurgeOrderRef: ev.OrderRef,
	}
}

func applyPurgeFillToLedgerRow(row *purgeLedgerRow, ev orderJournalEvent, delta, price float64, now time.Time) {
	if ev.PurgeID != "" {
		row.PurgeID = ev.PurgeID
	}
	row.LastPurgeOrderRef = ev.OrderRef
	row.PurgedQuantity += delta
	if price > 0 {
		row.PurgeValue += delta * price * float64(max(row.Multiplier, 1))
	}
	row.UpdatedAt = now
}

func applyRestoreFillToLedgerRow(row *purgeLedgerRow, ev orderJournalEvent, delta, price float64, now time.Time) {
	row.LastRestoreOrderRef = ev.OrderRef
	applied := min(delta, max(row.PurgedQuantity-row.RestoredQuantity, 0))
	row.RestoredQuantity += applied
	if price > 0 {
		row.RestoreValue += applied * price * float64(max(row.Multiplier, 1))
	}
	row.UpdatedAt = now
}

func normalizePurgeLedgerRow(row *purgeLedgerRow) {
	if row == nil {
		return
	}
	if row.Multiplier <= 0 {
		row.Multiplier = contractMultiplier(row.Contract)
	}
	row.RemainingQuantity = max(row.PurgedQuantity-row.RestoredQuantity, 0)
	if row.RemainingQuantity <= 1e-9 {
		row.RemainingQuantity = 0
		row.Status = purgeLedgerStatusRestored
	} else {
		row.Status = purgeLedgerStatusActive
	}
	row.ShadowPnL = purgeLedgerShadowPnL(*row)
}

func purgeLedgerShadowPnL(row purgeLedgerRow) float64 {
	if row.PurgedQuantity <= 0 || row.RestoredQuantity <= 0 || row.PurgeValue <= 0 || row.RestoreValue <= 0 {
		return 0
	}
	multiplier := float64(max(row.Multiplier, 1))
	purgeAvg := row.PurgeValue / (row.PurgedQuantity * multiplier)
	restoreAvg := row.RestoreValue / (row.RestoredQuantity * multiplier)
	sign := 1.0
	if row.OriginalQuantity < 0 || strings.EqualFold(row.OriginalSide, purgeOriginalSideShort) {
		sign = -1
	}
	return (purgeAvg - restoreAvg) * row.RestoredQuantity * multiplier * sign
}

func purgeLedgerDeltaPrice(ev orderJournalEvent, prior purgeLedgerOrderFill, delta float64) float64 {
	if delta <= 0 {
		return 0
	}
	if ev.AvgFillPrice > 0 {
		priorValue := prior.Filled * prior.AvgFillPrice
		nextValue := ev.Filled * ev.AvgFillPrice
		if nextValue > priorValue {
			return (nextValue - priorValue) / delta
		}
		return ev.AvgFillPrice
	}
	if ev.LastFillPrice > 0 {
		return ev.LastFillPrice
	}
	return 0
}

func purgeLedgerEventAvgPrice(ev orderJournalEvent, fallback float64) float64 {
	if ev.AvgFillPrice > 0 {
		return ev.AvgFillPrice
	}
	return fallback
}

func contractParamsFromJournalEvent(ev orderJournalEvent) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        ev.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(ev.Symbol)),
		SecType:      strings.ToUpper(strings.TrimSpace(ev.SecType)),
		Exchange:     strings.TrimSpace(ev.Exchange),
		PrimaryExch:  strings.TrimSpace(ev.PrimaryExch),
		Currency:     strings.ToUpper(strings.TrimSpace(ev.Currency)),
		LocalSymbol:  strings.TrimSpace(ev.LocalSymbol),
		TradingClass: strings.TrimSpace(ev.TradingClass),
		Expiry:       strings.TrimSpace(ev.Expiry),
		Strike:       ev.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(ev.Right)),
		Multiplier:   ev.Multiplier,
	}
	if contract.SecType == "" {
		contract.SecType = "STK"
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if contract.Exchange == "" {
		contract.Exchange = "SMART"
	}
	if contract.Multiplier == 0 {
		contract.Multiplier = contractMultiplier(contract)
	}
	return contract
}

func purgeLedgerRowToRPC(row purgeLedgerRow) rpc.PurgeLedgerRow {
	normalizePurgeLedgerRow(&row)
	multiplier := float64(max(row.Multiplier, 1))
	var purgeAvg, restoreAvg float64
	if row.PurgedQuantity > 0 && row.PurgeValue > 0 {
		purgeAvg = row.PurgeValue / (row.PurgedQuantity * multiplier)
	}
	if row.RestoredQuantity > 0 && row.RestoreValue > 0 {
		restoreAvg = row.RestoreValue / (row.RestoredQuantity * multiplier)
	}
	return rpc.PurgeLedgerRow{
		LegID:               row.LegID,
		PurgeID:             row.PurgeID,
		Symbol:              row.Symbol,
		SecType:             row.SecType,
		Contract:            row.Contract,
		Account:             row.Account,
		Mode:                row.Mode,
		Currency:            row.Currency,
		OriginalSide:        row.OriginalSide,
		OriginalQuantity:    row.OriginalQuantity,
		PurgeAction:         row.PurgeAction,
		RestoreAction:       row.RestoreAction,
		Multiplier:          row.Multiplier,
		PurgedQuantity:      row.PurgedQuantity,
		RestoredQuantity:    row.RestoredQuantity,
		RemainingQuantity:   row.RemainingQuantity,
		PurgeAvgPrice:       purgeAvg,
		RestoreAvgPrice:     restoreAvg,
		PurgeValue:          row.PurgeValue,
		RestoreValue:        row.RestoreValue,
		ShadowPnL:           row.ShadowPnL,
		Status:              row.Status,
		LastPurgeOrderRef:   row.LastPurgeOrderRef,
		LastRestoreOrderRef: row.LastRestoreOrderRef,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		Warnings:            slices.Clone(row.Warnings),
	}
}

func purgeLedgerTotals(rows []rpc.PurgeLedgerRow) rpc.PurgeLedgerTotals {
	var totals rpc.PurgeLedgerTotals
	for _, row := range rows {
		if row.RemainingQuantity > 0 {
			totals.ActiveRows++
		} else {
			totals.RestoredRows++
		}
		totals.PurgedQuantity += row.PurgedQuantity
		totals.RestoredQuantity += row.RestoredQuantity
		totals.RemainingQuantity += row.RemainingQuantity
		totals.PurgeValue += row.PurgeValue
		totals.RestoreValue += row.RestoreValue
		totals.ShadowPnL += row.ShadowPnL
	}
	return totals
}

func sortPurgeLedgerRows(rows []rpc.PurgeLedgerRow) {
	slices.SortStableFunc(rows, func(a, b rpc.PurgeLedgerRow) int {
		if a.RemainingQuantity > 0 && b.RemainingQuantity <= 0 {
			return -1
		}
		if a.RemainingQuantity <= 0 && b.RemainingQuantity > 0 {
			return 1
		}
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.SecType, b.Contract.SecType); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.Expiry, b.Contract.Expiry); c != 0 {
			return c
		}
		if a.Contract.Strike < b.Contract.Strike {
			return -1
		}
		if a.Contract.Strike > b.Contract.Strike {
			return 1
		}
		if c := strings.Compare(a.Contract.Right, b.Contract.Right); c != 0 {
			return c
		}
		return strings.Compare(a.LegID, b.LegID)
	})
}

func (s *purgeLedgerStore) loadLocked() (purgeLedgerFile, error) {
	if s == nil || s.Path == "" {
		return purgeLedgerFile{}, fmt.Errorf("purge ledger path is empty")
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return purgeLedgerFile{
				Kind:          purgeLedgerKind,
				SchemaVersion: purgeLedgerSchemaVersion,
				UpdatedAt:     s.currentTime(),
			}, nil
		}
		return purgeLedgerFile{}, fmt.Errorf("read purge ledger: %w", err)
	}
	var ledger purgeLedgerFile
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return purgeLedgerFile{}, fmt.Errorf("decode purge ledger: %w", err)
	}
	if ledger.Kind != purgeLedgerKind || ledger.SchemaVersion != purgeLedgerSchemaVersion {
		if ledger.Kind == purgeLedgerKind {
			_ = os.Remove(s.Path)
			return purgeLedgerFile{
				Kind:          purgeLedgerKind,
				SchemaVersion: purgeLedgerSchemaVersion,
				UpdatedAt:     s.currentTime(),
			}, nil
		}
		return purgeLedgerFile{}, fmt.Errorf("purge ledger is %q/%q, want %q/%q", ledger.Kind, ledger.SchemaVersion, purgeLedgerKind, purgeLedgerSchemaVersion)
	}
	return ledger, nil
}

func (s *purgeLedgerStore) saveLocked(ledger purgeLedgerFile) error {
	ledger.Kind = purgeLedgerKind
	ledger.SchemaVersion = purgeLedgerSchemaVersion
	if ledger.UpdatedAt.IsZero() {
		ledger.UpdatedAt = s.currentTime()
	}
	raw, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal purge ledger: %w", err)
	}
	raw = append(raw, '\n')
	return writePrivateStateAtomic(s.Path, raw)
}

func (s *purgeLedgerStore) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func appendPurgeLedgerUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
