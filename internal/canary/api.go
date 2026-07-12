package canary

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	ActionWatch         = canaryActionWatch
	ActionDefend        = canaryActionDefend
	ActionRebalance     = canaryActionRebalance
	ActionDeploy        = canaryActionDeploy
	ActionConfirmInputs = canaryActionConfirmInputs

	InputOK       = canaryInputOK
	InputDegraded = canaryInputDegraded

	PortfolioFitLow     = canaryPortfolioFitLow
	PortfolioFitUnknown = canaryPortfolioFitUnknown
)

func PolicyName() string {
	return canaryPolicy.Name
}

func SummarizeMarket(r rpc.RegimeSnapshotResult, now time.Time) rpc.CanaryMarketSummary {
	return summarizeCanaryMarket(r, now)
}

func SeverityAtLeast(got, want risk.SignalSeverity) bool {
	return severityRankAtLeast(got, want)
}

func GammaDegraded(g rpc.RegimeGammaZero) bool {
	return canaryGammaDegraded(g)
}

func MarketEvidence(m rpc.CanaryMarketSummary) string {
	return canaryMarketEvidence(m)
}

func PortfolioEvidence(p rpc.CanaryPortfolioSummary) string {
	return canaryPortfolioEvidence(p)
}

func AmbiguityEvidence(m rpc.CanaryMarketSummary) string {
	return canaryAmbiguityEvidence(m)
}

func FormatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	return formatProtectionCoverageEvidence(c)
}

func AppendUniqueString(values []string, value string) []string {
	return appendUniqueString(values, value)
}
