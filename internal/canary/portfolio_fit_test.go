package canary

import (
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/risk"
)

// A portfolio fit of "low" must be a measurement, never a default reached
// because the exposure-measuring signals were blocked or data-quality.
func TestCanaryPortfolioFitUnknownWhenExposureSignalsAreDataBlocked(t *testing.T) {
	p := CanaryPortfolioSummary{NetLiquidation: 100000}
	greeksBlind := []risk.Signal{{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality}}
	if got := canaryPortfolioFit(p, greeksBlind); got != canaryPortfolioFitUnknown {
		t.Fatalf("data-quality greeks signal must yield unknown fit, got %q", got)
	}
	blockedExposure := []risk.Signal{{ID: risk.SignalGrossExposureHigh, BlockedBy: []string{"positions_stale"}}}
	if got := canaryPortfolioFit(p, blockedExposure); got != canaryPortfolioFitUnknown {
		t.Fatalf("blocked exposure signal must yield unknown fit, got %q", got)
	}
	if got := canaryPortfolioFit(p, nil); got != canaryPortfolioFitLow {
		t.Fatalf("a measurable quiet book stays low, got %q", got)
	}
	measured := []risk.Signal{{ID: risk.SignalMarginCushionLow}}
	if got := canaryPortfolioFit(p, measured); got != canaryPortfolioFitMedium {
		t.Fatalf("measured medium signal must yield medium fit, got %q", got)
	}
}

func TestCanaryUnknownFitNeverClaimsLowExposure(t *testing.T) {
	r := CanaryResult{Action: canaryActionWatch, PortfolioFit: canaryPortfolioFitUnknown, MarketConfirmation: canaryMarketPartial}
	sum := canaryDecisionSummary(r)
	if strings.Contains(sum, "exposure is low") || strings.Contains(sum, "no reductions needed") {
		t.Fatalf("unknown fit must not claim low exposure: %q", sum)
	}
	if !strings.Contains(sum, "could not be measured") {
		t.Fatalf("unknown fit summary must disclose the blind spot: %q", sum)
	}
	if dir, sev := canaryDecisionState(canaryMarketPartial, canaryPortfolioFitUnknown, canaryInputDegraded, CanaryMarketSummary{}, nil); dir != risk.DirectionDefensive || sev != risk.SeverityWatch {
		t.Fatalf("partial market with unknown fit must stay defensive watch, got %v/%v", dir, sev)
	}
}
