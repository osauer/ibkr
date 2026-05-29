package cli

import "github.com/osauer/ibkr/internal/rpc"

// Preview* expose the user-facing text renderers to the cmd/_preview tool
// so it can demo the visual output with synthetic fixture data — useful
// for screenshots, social-preview images, and README updates without
// touching a live account.
//
// These are thin pass-throughs to the unexported renderers; they don't
// add behaviour and are not intended to be part of any stable public API.
// The Preview prefix and the placement in this file are the convention.
func PreviewRenderAccount(env *Env, a *rpc.AccountResult) {
	renderAccountText(env, a)
}

func PreviewRenderPositionsByUnderlying(env *Env, r *rpc.PositionsResult) {
	renderPositionsByUnderlying(env, r)
}

func PreviewRenderPositions(env *Env, r *rpc.PositionsResult) {
	renderPositionsText(env, r)
}

func PreviewRenderChainExpiries(env *Env, r *rpc.ChainExpiriesResult, withIV bool) {
	renderChainExpiriesText(env, r, withIV)
}

func PreviewRenderChainStrikes(env *Env, c *rpc.ChainResult) {
	renderChainText(env, c)
}

func PreviewRenderQuoteSnapshot(env *Env, qs []rpc.Quote) {
	renderQuoteSnapshotText(env, qs)
}

func PreviewRenderHistory(env *Env, r *rpc.HistoryDailyResult) {
	renderHistoryText(env, r)
}

func PreviewRenderScan(env *Env, r *rpc.ScanResult) {
	renderScanText(env, r)
}

func PreviewRenderSize(env *Env, r *SizeResult) {
	renderSizeText(env, r)
}

func PreviewRenderStatus(env *Env, h *rpc.HealthResult) {
	renderStatusText(env, h)
}

func PreviewRenderRegime(env *Env, r *rpc.RegimeSnapshotResult) {
	renderRegimeText(env, r)
}

func PreviewRenderCanary(env *Env, r *CanaryResult) {
	renderCanaryText(env, env.Stdout, r)
}
