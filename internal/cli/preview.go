package cli

import "github.com/osauer/ibkr/internal/rpc"

// PreviewRenderAccount, PreviewRenderPositionsByUnderlying, and
// PreviewRenderChainExpiries expose the three signature text renderers
// to the cmd/_preview tool so it can demo the visual output with
// synthetic fixture data — useful for screenshots, social-preview
// images, and README updates without touching a live account.
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

func PreviewRenderChainExpiries(env *Env, r *rpc.ChainExpiriesResult, withIV bool) {
	renderChainExpiriesText(env, r, withIV)
}
