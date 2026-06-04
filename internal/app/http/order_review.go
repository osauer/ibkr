package apphttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	nethttp "net/http"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/app/orderreview"
	"github.com/osauer/ibkr/internal/rpc"
)

type orderReviewPreviewRequest struct {
	Revision string               `json:"revision"`
	Rows     []orderReviewRowEdit `json:"rows"`
}

type orderReviewRowEdit struct {
	RowID    string `json:"row_id"`
	Included bool   `json:"included"`
	Quantity int    `json:"quantity"`
}

type orderReviewTransmitRow struct {
	RowID    string                `json:"row_id"`
	Quantity int                   `json:"quantity"`
	Result   *rpc.OrderPlaceResult `json:"result,omitempty"`
	Failure  string                `json:"failure,omitempty"`
}

type orderModifyPreviewRequest struct {
	Action     string              `json:"action,omitempty"`
	Contract   *rpc.ContractParams `json:"contract,omitempty"`
	Quantity   int                 `json:"quantity"`
	OrderType  string              `json:"order_type,omitempty"`
	LimitPrice *float64            `json:"limit_price,omitempty"`
	Strategy   string              `json:"strategy,omitempty"`
	TIF        string              `json:"tif,omitempty"`
	OutsideRTH *bool               `json:"outside_rth,omitempty"`
}

type orderModifyRequest struct {
	PreviewToken string `json:"preview_token"`
}

func (h *handler) handleCreateOrderReviewSet(w nethttp.ResponseWriter, r *nethttp.Request) {
	set, err := h.currentRiskPlanReviewSet(r.Context())
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if err := h.deps.Store.RecordOrderReviewSet(set); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, set)
}

func (h *handler) handleGetOrderReviewSet(w nethttp.ResponseWriter, r *nethttp.Request) {
	set, ok := h.deps.Store.OrderReviewSet(r.PathValue("id"))
	if !ok {
		writeError(w, nethttp.StatusNotFound, "order review set not found")
		return
	}
	writeJSON(w, set)
}

func (h *handler) handlePreviewOrderReviewSet(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderReviewPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	stored, ok := h.deps.Store.OrderReviewSet(r.PathValue("id"))
	if !ok {
		writeError(w, nethttp.StatusNotFound, "order review set not found")
		return
	}
	current, err := h.currentRiskPlanReviewSet(r.Context())
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if current.ID != stored.ID || current.Revision != stored.Revision || (req.Revision != "" && req.Revision != stored.Revision) {
		if err := h.deps.Store.RecordOrderReviewSet(current); err != nil {
			writeError(w, nethttp.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(nethttp.StatusConflict)
		writeJSON(w, map[string]any{
			"code":        "rebase_required",
			"message":     "current order proposal changed; preview the refreshed review set",
			"current_set": current,
			"changes":     classifyReviewSetChanges(stored, current),
		})
		return
	}
	preview, set, err := h.previewReviewSetRows(r.Context(), stored, req.Rows)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	set.LatestPreview = &preview
	set.Capabilities = preview.Capabilities
	set.UpdatedAt = preview.AsOf
	if err := h.deps.Store.RecordOrderReviewSet(set); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"set": set, "preview": preview})
}

func (h *handler) handleTransmitOrderReviewSet(w nethttp.ResponseWriter, r *nethttp.Request) {
	set, ok := h.deps.Store.OrderReviewSet(r.PathValue("id"))
	if !ok {
		writeError(w, nethttp.StatusNotFound, "order review set not found")
		return
	}
	preview := set.LatestPreview
	if preview == nil {
		writeError(w, nethttp.StatusBadRequest, "order review set has no preview")
		return
	}
	trading, err := h.deps.Daemon.TradingStatus(r.Context())
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	rows, err := validateTransmitPreview(set, *preview, *trading, time.Now().UTC())
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	out := make([]orderReviewTransmitRow, 0, len(rows))
	for _, row := range rows {
		token := strings.TrimSpace(row.Preview.PreviewToken)
		res, err := h.deps.Daemon.OrderPlace(r.Context(), rpc.OrderPlaceParams{PreviewToken: token, TimeoutMs: 10000})
		item := orderReviewTransmitRow{RowID: row.RowID, Quantity: row.Quantity}
		if err != nil {
			item.Failure = err.Error()
		} else {
			item.Result = res
		}
		out = append(out, item)
	}
	if orders, err := h.deps.Daemon.OrdersOpen(r.Context(), rpc.OrdersOpenParams{}); err == nil {
		writeJSON(w, map[string]any{"rows": out, "orders_open": orders, "as_of": time.Now().UTC()})
		return
	}
	writeJSON(w, map[string]any{"rows": out, "as_of": time.Now().UTC()})
}

func (h *handler) handleOrdersOpen(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OrdersOpen(r.Context(), rpc.OrdersOpenParams{Account: strings.TrimSpace(r.URL.Query().Get("account"))})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OrderStatus(r.Context(), rpc.OrderStatusParams{ID: r.PathValue("id")})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if !res.Found {
		writeError(w, nethttp.StatusNotFound, "order not found")
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderCancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OrderCancel(r.Context(), rpc.OrderCancelParams{ID: r.PathValue("id"), TimeoutMs: 10000})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderPreviewModify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderModifyPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	status, err := h.deps.Daemon.OrderStatus(r.Context(), rpc.OrderStatusParams{ID: r.PathValue("id")})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if !status.Found {
		writeError(w, nethttp.StatusNotFound, "order not found")
		return
	}
	params, err := modifyPreviewParamsFromRequest(status.Order, req)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Daemon.OrderPreview(r.Context(), params)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderModify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderModifyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.PreviewToken) == "" {
		writeError(w, nethttp.StatusBadRequest, "preview_token required")
		return
	}
	res, err := h.deps.Daemon.OrderModify(r.Context(), rpc.OrderModifyParams{
		ID:           r.PathValue("id"),
		PreviewToken: strings.TrimSpace(req.PreviewToken),
		TimeoutMs:    10000,
	})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) currentRiskPlanReviewSet(ctx context.Context) (orderreview.Set, error) {
	canary, err := h.deps.Daemon.Canary(ctx)
	if err != nil {
		return orderreview.Set{}, err
	}
	plan, err := h.deps.Daemon.RiskPlan(ctx, rpc.RiskPlanModeAuto, canary)
	if err != nil {
		return orderreview.Set{}, err
	}
	trading, err := h.deps.Daemon.TradingStatus(ctx)
	if err != nil {
		return orderreview.Set{}, err
	}
	return buildRiskPlanReviewSet(*plan, *trading, time.Now().UTC()), nil
}

func buildRiskPlanReviewSet(plan rpc.RiskPlanResult, trading rpc.TradingStatus, now time.Time) orderreview.Set {
	rows := make([]orderreview.Row, 0)
	for _, candidate := range plan.Candidates {
		for i, leg := range candidate.Legs {
			row := buildRiskPlanReviewRow(candidate, leg, i, trading)
			rows = append(rows, row)
		}
	}
	canaryFingerprint := plan.RefreshedCanaryFingerprint.Key
	if plan.TriggerCanaryFingerprint != nil && plan.TriggerCanaryFingerprint.Key != "" {
		canaryFingerprint = plan.TriggerCanaryFingerprint.Key
	}
	revision := reviewRevision(plan.PlanID, canaryFingerprint, rows, plan.SourceFingerprints)
	id := "ors_" + shortHash(plan.PlanID+"|"+canaryFingerprint+"|"+orderreview.SourceKindRiskPlan+"|"+orderreview.IntentMitigateRisk)
	return orderreview.Set{
		ID:                 id,
		Revision:           revision,
		SourceKind:         orderreview.SourceKindRiskPlan,
		Intent:             orderreview.IntentMitigateRisk,
		PlanID:             plan.PlanID,
		CanaryFingerprint:  canaryFingerprint,
		SourceFingerprints: plan.SourceFingerprints,
		Rows:               rows,
		Capabilities:       trading,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

func buildRiskPlanReviewRow(candidate rpc.RiskPlanCandidate, leg rpc.RiskPlanCandidateLeg, legIndex int, trading rpc.TradingStatus) orderreview.Row {
	blockers := append([]string(nil), candidate.BlockedBy...)
	blockers = append(blockers, leg.Warnings...)
	action := orderActionFromRiskLeg(leg.Action)
	if action == "" {
		blockers = append(blockers, "unsupported action "+leg.Action)
	}
	secType := strings.ToUpper(strings.TrimSpace(leg.Contract.SecType))
	if secType != "" && secType != "STK" {
		blockers = append(blockers, "broker WhatIf preview currently supports stock/ETF rows only")
	}
	if leg.EstimatedLimitPrice == nil && strings.EqualFold(leg.OrderType, rpc.OrderTypeLMT) {
		blockers = append(blockers, "limit price unavailable")
	}
	if candidate.Status != rpc.RiskPlanCandidatePreviewable {
		blockers = append(blockers, "risk-plan candidate is "+candidate.Status)
	}
	if !isCloseOrReduce(leg.PositionEffect) {
		blockers = append(blockers, "fast mitigation path only accepts close/reduce rows")
	}
	if !trading.CanPreview {
		blockers = append(blockers, "trading preview is not available")
	}
	maxQty := maxReviewQuantity(leg)
	qty := min(max(leg.Quantity, 0), maxQty)
	return orderreview.Row{
		RowID:            fmt.Sprintf("%s:%d", candidate.ID, legIndex+1),
		CandidateID:      candidate.ID,
		LegIndex:         legIndex,
		ProposedQuantity: max(leg.Quantity, 0),
		EditableQuantity: qty,
		MaxQuantity:      maxQty,
		Included:         len(blockers) == 0 && qty > 0,
		Action:           action,
		Contract:         leg.Contract,
		OrderType:        defaultString(leg.OrderType, rpc.OrderTypeLMT),
		LimitStrategy:    defaultString(leg.LimitStrategy, rpc.OrderStrategyPatientLimit),
		LimitPrice:       leg.EstimatedLimitPrice,
		TIF:              defaultString(leg.TIF, rpc.OrderTIFDay),
		OutsideRTH:       leg.OutsideRTH,
		Rationale:        strings.TrimSpace(candidate.Subject + ": " + candidate.Reason),
		RiskImpact: orderreview.RiskImpact{
			MarketValueBase:     leg.MarketValueBase,
			DollarDeltaBase:     leg.DollarDeltaBase,
			GrossExposurePctNLV: candidate.EstimatedReduction.GrossExposurePctNLV,
			NetDeltaPctNLV:      candidate.EstimatedReduction.NetDeltaPctNLV,
			GrossDeltaPctNLV:    candidate.EstimatedReduction.GrossDeltaPctNLV,
			RealizedPnLBase:     candidate.EstimatedReduction.RealizedPnLBase,
		},
		Status:         candidate.Status,
		HeldQuantity:   leg.HeldQuantity,
		PositionEffect: leg.PositionEffect,
		Blockers:       uniqueStrings(blockers),
		Warnings:       append([]string(nil), candidate.Warnings...),
	}
}

func (h *handler) previewReviewSetRows(ctx context.Context, set orderreview.Set, edits []orderReviewRowEdit) (orderreview.Preview, orderreview.Set, error) {
	editByID := map[string]orderReviewRowEdit{}
	for _, edit := range edits {
		if strings.TrimSpace(edit.RowID) == "" {
			return orderreview.Preview{}, set, fmt.Errorf("row_id required")
		}
		editByID[edit.RowID] = edit
	}
	if len(editByID) == 0 {
		for _, row := range set.Rows {
			editByID[row.RowID] = orderReviewRowEdit{RowID: row.RowID, Included: row.Included, Quantity: row.EditableQuantity}
		}
	}
	trading, err := h.deps.Daemon.TradingStatus(ctx)
	if err != nil {
		return orderreview.Preview{}, set, err
	}
	preview := orderreview.Preview{
		ID:           "orp_" + shortHash(fmt.Sprintf("%s|%s|%d", set.ID, set.Revision, time.Now().UnixNano())),
		SetID:        set.ID,
		SetRevision:  set.Revision,
		Capabilities: *trading,
		AsOf:         time.Now().UTC(),
	}
	selected := 0
	for i := range set.Rows {
		row := &set.Rows[i]
		edit, ok := editByID[row.RowID]
		if !ok {
			continue
		}
		qty := edit.Quantity
		if !edit.Included {
			qty = 0
		}
		rowResult := orderreview.PreviewRow{RowID: row.RowID, Included: edit.Included && qty > 0, Quantity: qty}
		rowBlockers := validateReviewRowEdit(*row, qty, edit.Included)
		if len(row.Blockers) > 0 && qty > 0 {
			rowBlockers = append(rowBlockers, row.Blockers...)
		}
		row.EditableQuantity = qty
		row.Included = rowResult.Included
		if qty <= 0 {
			preview.Rows = append(preview.Rows, rowResult)
			continue
		}
		selected++
		if len(rowBlockers) > 0 {
			rowResult.Blockers = uniqueStrings(rowBlockers)
			preview.Rows = append(preview.Rows, rowResult)
			continue
		}
		orderPreview, err := h.deps.Daemon.OrderPreview(ctx, rpc.OrderPreviewParams{
			Action:     row.Action,
			Contract:   row.Contract,
			Quantity:   qty,
			OrderType:  row.OrderType,
			LimitPrice: row.LimitPrice,
			Strategy:   row.LimitStrategy,
			TIF:        row.TIF,
			OutsideRTH: row.OutsideRTH,
			TimeoutMs:  5000,
		})
		if err != nil {
			rowResult.Failure = err.Error()
			rowResult.Blockers = []string{err.Error()}
			preview.Rows = append(preview.Rows, rowResult)
			continue
		}
		rowResult.Preview = orderPreview
		rowResult.Draft = &orderPreview.Draft
		rowResult.TokenMinted = orderPreview.TokenMinted
		rowResult.SubmitEligible = orderPreview.SubmitEligible
		rowResult.WhatIfStatus = orderPreview.WhatIf.Status
		rowResult.Warnings = dataWarningMessages(orderPreview.Warnings)
		preview.Rows = append(preview.Rows, rowResult)
	}
	preview.SubmitReady = selected > 0
	for _, row := range preview.Rows {
		if row.Included && !row.SubmitEligible {
			preview.SubmitReady = false
			break
		}
	}
	if selected == 0 {
		preview.Blockers = append(preview.Blockers, "select at least one order row to preview")
	}
	return preview, set, nil
}

func validateReviewRowEdit(row orderreview.Row, qty int, included bool) []string {
	if qty < 0 {
		return []string{"quantity cannot be negative"}
	}
	if !included || qty == 0 {
		return nil
	}
	blockers := []string{}
	if row.Action == "" {
		blockers = append(blockers, "order action unavailable")
	}
	if qty > row.MaxQuantity {
		blockers = append(blockers, fmt.Sprintf("quantity exceeds closable cap %d", row.MaxQuantity))
	}
	if qty > row.ProposedQuantity && !isCloseOrReduce(row.PositionEffect) {
		blockers = append(blockers, "quantity increase is only allowed for close/reduce rows")
	}
	if row.Contract.Symbol == "" {
		blockers = append(blockers, "contract symbol required")
	}
	return blockers
}

func validateTransmitPreview(set orderreview.Set, preview orderreview.Preview, trading rpc.TradingStatus, now time.Time) ([]orderreview.PreviewRow, error) {
	if !trading.CanTransmit {
		return nil, fmt.Errorf("trading.status can_transmit=false")
	}
	if preview.SetID != set.ID {
		return nil, fmt.Errorf("preview set_id does not match review set")
	}
	if preview.SetRevision != set.Revision {
		return nil, fmt.Errorf("preview revision does not match review set")
	}
	if !preview.SubmitReady {
		return nil, fmt.Errorf("preview is not submit-ready")
	}
	rows := make([]orderreview.PreviewRow, 0)
	for _, row := range preview.Rows {
		if !row.Included || row.Quantity <= 0 {
			continue
		}
		if !row.SubmitEligible {
			return nil, fmt.Errorf("row %s is not submit-eligible", row.RowID)
		}
		if row.Preview == nil {
			return nil, fmt.Errorf("row %s has no broker preview", row.RowID)
		}
		if strings.TrimSpace(row.Preview.PreviewToken) == "" {
			return nil, fmt.Errorf("row %s has no preview token", row.RowID)
		}
		if row.Preview.PreviewTokenExpiresAt.IsZero() || !now.Before(row.Preview.PreviewTokenExpiresAt) {
			return nil, fmt.Errorf("row %s preview token is expired", row.RowID)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("select at least one submit-eligible row")
	}
	return rows, nil
}

func modifyPreviewParamsFromRequest(order rpc.OrderView, req orderModifyPreviewRequest) (rpc.OrderPreviewParams, error) {
	if req.Quantity <= 0 {
		return rpc.OrderPreviewParams{}, fmt.Errorf("quantity must be positive")
	}
	contract := rpc.ContractParams{
		Symbol:  strings.TrimSpace(order.Symbol),
		SecType: strings.TrimSpace(order.SecType),
	}
	if req.Contract != nil {
		contract = *req.Contract
	}
	if strings.TrimSpace(contract.Symbol) == "" {
		return rpc.OrderPreviewParams{}, fmt.Errorf("contract symbol required")
	}
	if strings.TrimSpace(contract.SecType) == "" {
		contract.SecType = "STK"
	}
	if strings.TrimSpace(contract.Currency) == "" && strings.TrimSpace(contract.Market) == "" &&
		strings.TrimSpace(contract.Exchange) == "" && strings.TrimSpace(contract.PrimaryExch) == "" {
		contract.Currency = "USD"
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = order.Action
	}
	if strings.TrimSpace(action) == "" {
		return rpc.OrderPreviewParams{}, fmt.Errorf("action required")
	}
	orderType := defaultString(req.OrderType, order.OrderType)
	orderType = defaultString(orderType, rpc.OrderTypeLMT)
	tif := defaultString(req.TIF, order.TIF)
	tif = defaultString(tif, rpc.OrderTIFDay)
	limit := req.LimitPrice
	if limit == nil && order.LimitPrice > 0 {
		v := order.LimitPrice
		limit = &v
	}
	outsideRTH := false
	if req.OutsideRTH != nil {
		outsideRTH = *req.OutsideRTH
	}
	return rpc.OrderPreviewParams{
		Action:     action,
		Contract:   contract,
		Quantity:   req.Quantity,
		OrderType:  orderType,
		LimitPrice: limit,
		Strategy:   defaultString(req.Strategy, rpc.OrderStrategyPatientLimit),
		TIF:        tif,
		OutsideRTH: outsideRTH,
		ReplaceID:  order.OrderRef,
		TimeoutMs:  5000,
	}, nil
}

func classifyReviewSetChanges(oldSet, newSet orderreview.Set) []orderreview.RebaseChange {
	changes := []orderreview.RebaseChange{}
	oldRows := map[string]orderreview.Row{}
	for _, row := range oldSet.Rows {
		oldRows[row.RowID] = row
	}
	newRows := map[string]orderreview.Row{}
	for _, row := range newSet.Rows {
		newRows[row.RowID] = row
		if oldRow, ok := oldRows[row.RowID]; !ok {
			changes = append(changes, orderreview.RebaseChange{RowID: row.RowID, Code: "new_higher_priority_row", Message: "new proposal row is available"})
		} else if oldRow.ProposedQuantity != row.ProposedQuantity {
			changes = append(changes, orderreview.RebaseChange{RowID: row.RowID, Code: "adjusted_proposed_quantity", Message: "proposed quantity changed"})
		} else if len(oldRow.Blockers) == 0 && len(row.Blockers) > 0 {
			changes = append(changes, orderreview.RebaseChange{RowID: row.RowID, Code: "row_blocked", Message: "row is now blocked"})
		} else {
			changes = append(changes, orderreview.RebaseChange{RowID: row.RowID, Code: "unchanged_row", Message: "row still maps to current proposal"})
		}
	}
	for _, row := range oldSet.Rows {
		if _, ok := newRows[row.RowID]; !ok {
			changes = append(changes, orderreview.RebaseChange{RowID: row.RowID, Code: "row_gone", Message: "row no longer maps to current risk-plan proposal"})
		}
	}
	if oldSet.CanaryFingerprint != newSet.CanaryFingerprint || oldSet.Revision != newSet.Revision {
		changes = append(changes, orderreview.RebaseChange{Code: "source_changed", Message: "source fingerprint or proposal revision changed"})
	}
	return changes
}

func reviewRevision(planID, canaryFingerprint string, rows []orderreview.Row, fps rpc.CanarySourceFingerprints) string {
	raw, _ := json.Marshal(struct {
		PlanID            string                       `json:"plan_id"`
		CanaryFingerprint string                       `json:"canary_fingerprint"`
		Rows              []orderreview.Row            `json:"rows"`
		Source            rpc.CanarySourceFingerprints `json:"source"`
	}{planID, canaryFingerprint, rows, fps})
	return "rev_" + shortHash(string(raw))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func orderActionFromRiskLeg(action string) string {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "SELL", "SELL_TO_CLOSE":
		return rpc.OrderActionSell
	case "BUY", "BUY_TO_CLOSE":
		return rpc.OrderActionBuy
	default:
		return ""
	}
}

func maxReviewQuantity(leg rpc.RiskPlanCandidateLeg) int {
	held := int(math.Floor(math.Abs(leg.HeldQuantity)))
	if isCloseOrReduce(leg.PositionEffect) && held > 0 {
		return held
	}
	return max(leg.Quantity, 0)
}

func isCloseOrReduce(effect string) bool {
	switch strings.ToLower(strings.TrimSpace(effect)) {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return true
	default:
		return false
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func uniqueStrings(in []string) []string {
	out := []string{}
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || slices.Contains(out, item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func dataWarningMessages(warnings []rpc.DataWarning) []string {
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if warning.Message != "" {
			out = append(out, warning.Message)
		} else if warning.Code != "" {
			out = append(out, warning.Code)
		}
	}
	return out
}
