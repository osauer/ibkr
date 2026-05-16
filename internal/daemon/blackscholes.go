package daemon

import "math"

// bsGamma computes the Black-Scholes gamma of a European option leg.
//
// Gamma is the second derivative of option price with respect to the
// underlying spot — i.e., how fast delta changes as spot moves. It's
// identical for calls and puts at the same strike/expiry/vol because
// put-call parity makes their first derivatives differ only by a
// constant. For dealer-gamma aggregation we compute one γ per (spot,
// strike, vol, t) tuple and let the caller assign the sign per the
// dealer-positioning convention.
//
// Formula: γ = φ(d1) / (S · σ · √t)
//
// where d1 = ( ln(S/K) + (r − q + σ²/2) · t ) / ( σ · √t )
// and φ is the standard normal probability density.
//
// Inputs:
//
//	spot   — current price of the underlying (S)
//	strike — option strike (K)
//	t      — time to expiry in years; must be > 0
//	vol    — implied volatility as a decimal (0.20 == 20 %); must be > 0
//	r      — risk-free rate (decimal); typically 0 for the zero-gamma
//	         aggregation since rate sensitivity over the [0.85, 1.15]
//	         sweep is negligible relative to vol-skew effects
//	q      — continuous dividend yield (decimal); 0 for SPX index options
//
// Returns 0 for degenerate inputs (t <= 0, vol <= 0, spot <= 0) — the
// aggregator treats degenerate legs as zero-gamma rather than panicking
// on a NaN propagation. The caller is responsible for filtering legs
// with missing IV before they reach this function.
func bsGamma(spot, strike, t, vol, r, q float64) float64 {
	if spot <= 0 || strike <= 0 || t <= 0 || vol <= 0 {
		return 0
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(spot/strike) + (r-q+0.5*vol*vol)*t) / (vol * sqrtT)
	// Standard-normal pdf: φ(x) = exp(-x²/2) / √(2π).
	pdf := math.Exp(-0.5*d1*d1) / math.Sqrt(2*math.Pi)
	return pdf / (spot * vol * sqrtT)
}

// dealerGEX returns the dollar gamma per 1 % move attributable to a
// single option leg, signed under the Perfiliev convention: positive
// for calls, negative for puts. The aggregate over a chain is the
// dealer-positioning estimate at the given spot; a sign flip across
// adjacent spot scenarios is the "zero-gamma" crossing.
//
// Formula: GEX = sign · γ · OI · multiplier · spot² · 0.01
//
// Notes:
//   - multiplier is 100 for SPX index options (and most US-listed
//     equity options); the caller passes it explicitly to avoid
//     hardcoding the SPX assumption into the math.
//   - The 0.01 scales gamma per $1 of underlying into gamma per 1 %
//     of underlying — standard practitioner units that match every
//     published GEX number you can cross-check against.
//   - Returns 0 when γ degenerates (see bsGamma) so missing-data legs
//     don't poison the sum.
func dealerGEX(gamma, openInt float64, multiplier int, spot float64, isCall bool) float64 {
	if openInt == 0 || multiplier == 0 || spot <= 0 {
		return 0
	}
	contrib := gamma * openInt * float64(multiplier) * spot * spot * 0.01
	if isCall {
		return contrib
	}
	return -contrib
}

// absGEX returns the sign-agnostic magnitude contribution of a single
// option leg — same formula as dealerGEX but always positive. Used for
// the "where is dealer hedging concentrated" view, which is robust to
// the dealer-positioning assumption (since covered-call ETF flow,
// autocallable hedging, and structured products can invert the naive
// sign).
func absGEX(gamma, openInt float64, multiplier int, spot float64) float64 {
	if openInt == 0 || multiplier == 0 || spot <= 0 {
		return 0
	}
	return math.Abs(gamma) * openInt * float64(multiplier) * spot * spot * 0.01
}
