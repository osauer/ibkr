package cli

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

// SizeInput is the validated input to ComputeSize. Fields mirror the CLI
// flags one-for-one so the pure function is testable without the runner.
//
// Target is optional. When set, ComputeSize derives the R-multiple
// (reward-to-risk ratio = profit-to-stop distance / entry-to-stop
// distance) and the breakeven win rate. Long trades require target >
// entry; short trades require target < entry.
type SizeInput struct {
	Symbol      string
	Side        string  // "long" | "short"
	Entry       float64 // quote currency
	Stop        float64 // quote currency
	Target      float64 // optional take-profit; 0 disables the R block
	RiskPct     float64 // percent of NLV; (0, 100]
	Lot         int     // round shares down to this multiple; >= 1
	FX          float64 // quote-currency units per 1 base-currency unit; > 0
	NLV         float64 // base currency
	BuyingPower float64 // base currency (0 disables BP check)
	Currency    string  // base currency code, surfaced in output only
}

// SizeResult is the wire shape of `ibkr size --json` and the input to the
// text renderer. Keep this struct stable — Claude consumes it.
//
// Target / R / RewardQuote / RewardBase / BreakevenWinRate are populated
// only when the input carries a non-zero Target. R is the reward-to-risk
// ratio (|target − entry| / |entry − stop|), the canonical "is this trade
// worth taking" filter; ≥ 2R is the common discretionary threshold.
// BreakevenWinRate is 1 / (1 + R) — at this win rate the strategy breaks
// even, so any sustained edge above it is profitable.
type SizeResult struct {
	Symbol           string   `json:"symbol"`
	Side             string   `json:"side"`
	Entry            float64  `json:"entry"`
	Stop             float64  `json:"stop"`
	Target           *float64 `json:"target,omitempty"`
	RiskPct          float64  `json:"risk_pct"`
	Lot              int      `json:"lot"`
	FX               float64  `json:"fx"`
	NLV              float64  `json:"nlv"`
	BaseCurrency     string   `json:"base_currency,omitempty"`
	RiskBase         float64  `json:"risk_base"`  // NLV * pct/100
	RiskQuote        float64  `json:"risk_quote"` // RiskBase * fx
	PerShareRisk     float64  `json:"per_share_risk"`
	Shares           int      `json:"shares"`
	Notional         float64  `json:"notional"`    // shares * entry
	MaxLoss          float64  `json:"max_loss"`    // shares * per_share_risk (quote ccy)
	R                *float64 `json:"r,omitempty"` // (|target-entry|) / (|entry-stop|)
	RewardQuote      *float64 `json:"reward_quote,omitempty"`
	RewardBase       *float64 `json:"reward_base,omitempty"`
	BreakevenWinRate *float64 `json:"breakeven_win_rate,omitempty"` // 1 / (1+R), fraction
	Status           string   `json:"status"`                       // "ok" | "tight_risk" | "exceeds_buying_power"
}

// ComputeSize is the pure sizing function. All validation lives here so the
// runner stays a thin wiring layer; tests cover the math and the validations
// without spinning up a daemon. Exported because the MCP server reuses it for
// the ibkr_size tool — the daemon has no size RPC; sizing is account-snapshot
// + local arithmetic.
func ComputeSize(in SizeInput) (SizeResult, error) {
	side, err := validateSizePlan(in, true)
	if err != nil {
		return SizeResult{}, err
	}

	perShare := math.Abs(in.Entry - in.Stop)
	if perShare == 0 {
		// Defensive — the side-vs-stop checks in validateSizePlan should make this unreachable.
		return SizeResult{}, fmt.Errorf("per-share risk is zero (entry == stop)")
	}

	riskBase := in.NLV * in.RiskPct / 100
	riskQuote := riskBase * in.FX
	rawShares := riskQuote / perShare
	shares := int(math.Floor(rawShares/float64(in.Lot))) * in.Lot

	res := SizeResult{
		Symbol:       strings.ToUpper(in.Symbol),
		Side:         side,
		Entry:        in.Entry,
		Stop:         in.Stop,
		RiskPct:      in.RiskPct,
		Lot:          in.Lot,
		FX:           in.FX,
		NLV:          in.NLV,
		BaseCurrency: in.Currency,
		RiskBase:     riskBase,
		RiskQuote:    riskQuote,
		PerShareRisk: perShare,
		Shares:       shares,
		Notional:     float64(shares) * in.Entry,
		MaxLoss:      float64(shares) * perShare,
		Status:       "ok",
	}

	if in.Target != 0 {
		tgt := in.Target
		res.Target = &tgt
		perShareReward := math.Abs(in.Target - in.Entry)
		r := perShareReward / perShare
		res.R = &r
		rewardQuote := float64(shares) * perShareReward
		rewardBase := rewardQuote / in.FX
		res.RewardQuote = &rewardQuote
		res.RewardBase = &rewardBase
		be := 1.0 / (1.0 + r)
		res.BreakevenWinRate = &be
	}

	if shares == 0 {
		res.Status = "tight_risk"
		return res, nil
	}
	if in.BuyingPower > 0 && res.Notional > in.BuyingPower*in.FX {
		res.Status = "exceeds_buying_power"
	}
	return res, nil
}

func validateSizePlan(in SizeInput, requireAccount bool) (string, error) {
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side == "" {
		side = "long"
	}
	if side != "long" && side != "short" {
		return "", fmt.Errorf("side must be long or short (got %q)", in.Side)
	}
	if in.Symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}
	if in.Entry <= 0 {
		return "", fmt.Errorf("entry must be > 0 (got %v)", in.Entry)
	}
	if in.Stop <= 0 {
		return "", fmt.Errorf("stop must be > 0 (got %v)", in.Stop)
	}
	if side == "long" && in.Stop >= in.Entry {
		return "", fmt.Errorf("long trade requires stop (%v) < entry (%v)", in.Stop, in.Entry)
	}
	if side == "short" && in.Stop <= in.Entry {
		return "", fmt.Errorf("short trade requires stop (%v) > entry (%v)", in.Stop, in.Entry)
	}
	if in.Target != 0 {
		if in.Target < 0 {
			return "", fmt.Errorf("target must be > 0 (got %v)", in.Target)
		}
		if side == "long" && in.Target <= in.Entry {
			return "", fmt.Errorf("long trade requires target (%v) > entry (%v)", in.Target, in.Entry)
		}
		if side == "short" && in.Target >= in.Entry {
			return "", fmt.Errorf("short trade requires target (%v) < entry (%v)", in.Target, in.Entry)
		}
	}
	if in.RiskPct <= 0 || in.RiskPct > 100 {
		return "", fmt.Errorf("risk-pct must be in (0, 100] (got %v)", in.RiskPct)
	}
	if in.Lot < 1 {
		return "", fmt.Errorf("lot must be >= 1 (got %v)", in.Lot)
	}
	if in.FX <= 0 {
		return "", fmt.Errorf("fx must be > 0 (got %v)", in.FX)
	}
	if requireAccount && in.NLV <= 0 {
		return "", fmt.Errorf("nlv must be > 0 (got %v) — is the gateway connected?", in.NLV)
	}
	return side, nil
}

func runSize(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "size")
	symbol := fs.String("symbol", "", "underlying symbol (required)")
	entry := fs.Float64("entry", 0, "planned entry price per share (required)")
	stop := fs.Float64("stop", 0, "planned stop price per share (required)")
	target := fs.Float64("target", 0, "optional take-profit price; enables R-multiple and breakeven win-rate output")
	riskPct := fs.Float64("risk-pct", 1.0, "percent of NLV to risk on this trade")
	side := fs.String("side", "long", "trade direction: long | short")
	lot := fs.Int("lot", 1, "round shares down to this multiple")
	fx := fs.Float64("fx", 1.0, "quote-currency units per 1 base-currency unit (e.g. 1.085 for EUR→USD)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	plan := SizeInput{
		Symbol:  *symbol,
		Side:    *side,
		Entry:   *entry,
		Stop:    *stop,
		Target:  *target,
		RiskPct: *riskPct,
		Lot:     *lot,
		FX:      *fx,
	}
	if _, err := validateSizePlan(plan, false); err != nil {
		return fail(env, "size: %v", err)
	}

	var acct rpc.AccountResult
	if err := env.Conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return fail(env, "size: %v", err)
	}

	in := plan
	in.NLV = acct.NetLiquidation
	in.BuyingPower = acct.BuyingPower
	in.Currency = acct.BaseCurrency
	res, err := ComputeSize(in)
	if err != nil {
		return fail(env, "size: %v", err)
	}

	if *jsonOut {
		return printJSON(env, res)
	}
	return renderSizeText(env, &res)
}

func renderSizeText(env *Env, r *SizeResult) int {
	out := env.Stdout
	ccyBase := nonEmpty(r.BaseCurrency, "USD")
	fmt.Fprintln(out)
	header := fmt.Sprintf("Size  %s · %s · entry %.2f · stop %.2f", r.Symbol, r.Side, r.Entry, r.Stop)
	if r.Target != nil {
		header += fmt.Sprintf(" · target %.2f", *r.Target)
	}
	fmt.Fprintln(out, header)
	fmt.Fprintln(out)

	// labelWidth covers the widest label ("Risk in quote ccy" = 17,
	// "Max gain at target" = 18, "Breakeven win rate" = 18); valueWidth
	// fits a 7-digit notional with grouping ("$ 9,999,999.99" = 14).
	const (
		labelWidth = 20
		valueWidth = 14
	)
	label := func(s string) string {
		return fmt.Sprintf("  %-*s", labelWidth, s)
	}
	// moneyBase stamps the base-currency symbol; right for NLV, RiskBase,
	// RewardBase. moneyQuote is for values denominated in the trade's
	// quote currency (RiskQuote, PerShareRisk, Notional, MaxLoss,
	// RewardQuote): when FX = 1 the quote currency IS the base currency so
	// the symbol is correct; when FX ≠ 1 the symbol would lie (the value
	// is in some other currency the daemon doesn't carry on SizeResult),
	// so we drop it and let the "Risk in quote ccy (fx …)" header do the
	// labelling.
	moneyBase := func(v float64) string {
		return env.formatMoneyNegCcyRight(v, ccyBase, valueWidth)
	}
	moneyQuote := func(v float64) string {
		if r.FX == 1.0 {
			return moneyBase(v)
		}
		return padLeftVisible(formatMoneyBare(v), valueWidth)
	}

	fmt.Fprintf(out, "%s  %s  (%s)\n", label("Net liquidation"), moneyBase(r.NLV), ccyBase)
	fmt.Fprintf(out, "%s  %s  (%.2f%% of NLV)\n", label("Risk budget"), moneyBase(r.RiskBase), r.RiskPct)
	if r.FX != 1.0 {
		fmt.Fprintf(out, "%s  %s  (fx %.4f)\n", label("Risk in quote ccy"), moneyQuote(r.RiskQuote), r.FX)
	}
	fmt.Fprintf(out, "%s  %s\n", label("Per-share risk"), moneyQuote(r.PerShareRisk))
	fmt.Fprintln(out)

	// Shares is the hero — the sizing tool's whole purpose is to tell the
	// user how many to buy. Bold it; the lot suffix stays plain so the
	// hero doesn't get a competing visual sibling on the same line.
	shares := padLeftVisible(fmt.Sprintf("%d", r.Shares), valueWidth)
	fmt.Fprintf(out, "%s  %s  (lot %d)\n", label("Shares"), env.bold(shares), r.Lot)
	fmt.Fprintf(out, "%s  %s\n", label("Notional"), moneyQuote(r.Notional))
	fmt.Fprintf(out, "%s  %s\n", label("Max loss at stop"), moneyQuote(r.MaxLoss))

	// Reward / R block: only when --target was supplied. R-multiple is the
	// canonical "is this trade worth taking" filter — ≥2R is the common
	// discretionary threshold. Breakeven win rate is the dual reading:
	// what hit-rate the strategy needs to be flat at this R.
	if r.R != nil && r.RewardQuote != nil && r.BreakevenWinRate != nil {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s  %s\n", label("Max gain at target"), moneyQuote(*r.RewardQuote))
		if r.FX != 1.0 && r.RewardBase != nil {
			fmt.Fprintf(out, "%s  %s  (in %s)\n", label("Max gain in base ccy"), moneyBase(*r.RewardBase), ccyBase)
		}
		rStr := padLeftVisible(fmt.Sprintf("%.2fR", *r.R), valueWidth)
		fmt.Fprintf(out, "%s  %s\n", label("Reward:risk"), rStr)
		beStr := padLeftVisible(fmt.Sprintf("%.1f%%", *r.BreakevenWinRate*100), valueWidth)
		fmt.Fprintf(out, "%s  %s\n", label("Breakeven win rate"), beStr)
	}

	if r.Status != "ok" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.yellow(fmt.Sprintf("  ⚠ status: %s", r.Status)))
		switch r.Status {
		case "tight_risk":
			fmt.Fprintln(out, "    risk budget < per-share risk × lot — widen the stop, raise --risk-pct, or lower --lot.")
		case "exceeds_buying_power":
			fmt.Fprintln(out, "    notional exceeds available buying power; trim --risk-pct or revisit the entry.")
		}
	}
	fmt.Fprintln(out)
	return 0
}
