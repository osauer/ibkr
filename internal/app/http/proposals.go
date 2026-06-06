package apphttp

import (
	"encoding/json"
	nethttp "net/http"

	"github.com/osauer/ibkr/internal/rpc"
)

func (h *handler) handleProposalsSnapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.TradeProposalsSnapshot(r.Context(), rpc.TradeProposalSnapshotParams{Show: r.URL.Query().Get("show") == "1"})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsRefresh(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.TradeProposalsRefresh(r.Context(), rpc.TradeProposalRefreshParams{Show: true})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsPreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalPreviewParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsPreview(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsSubmit(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalSubmitParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsSubmit(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsIgnore(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalIgnoreParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsIgnore(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}
