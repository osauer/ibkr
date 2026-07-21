package rpc

import "github.com/osauer/ibkr/v2/internal/risk"

// Alert candidate wire types are aliases of the pure risk contract. Keeping a
// single concrete definition makes conversion loss impossible and ensures RPC
// JSON validation cannot drift into a second policy evaluator.
type (
	AlertSource             = risk.AlertSource
	AlertKind               = risk.AlertKind
	AlertEpisodeState       = risk.AlertEpisodeState
	AlertSeverity           = risk.AlertSeverity
	AlertDeliveryPreference = risk.AlertDeliveryPreference
	AlertEvidenceHealth     = risk.AlertEvidenceHealth
	AlertDestination        = risk.AlertDestination
	AlertCoverageState      = risk.AlertCoverageState
	AlertCoverageFreshness  = risk.AlertCoverageFreshness
	AlertSnapshotState      = risk.AlertSnapshotState
	AlertCandidate          = risk.AlertCandidate
	AlertCoverage           = risk.AlertCoverage
	AlertCandidateSnapshot  = risk.AlertCandidateSnapshot
)

const (
	AlertCandidateSnapshotVersion = risk.AlertCandidateSnapshotVersion

	AlertSourceCanary         = risk.AlertSourceCanary
	AlertSourceRegime         = risk.AlertSourceRegime
	AlertSourceRulebook       = risk.AlertSourceRulebook
	AlertSourceRiskPolicy     = risk.AlertSourceRiskPolicy
	AlertSourceProtection     = risk.AlertSourceProtection
	AlertSourceOrderIntegrity = risk.AlertSourceOrderIntegrity
	AlertSourceReconciliation = risk.AlertSourceReconciliation
	AlertSourceGovernance     = risk.AlertSourceGovernance
	AlertSourceDataHealth     = risk.AlertSourceDataHealth
	AlertSourceDelivery       = risk.AlertSourceDelivery

	AlertKindMarketState             = risk.AlertKindMarketState
	AlertKindPortfolioRisk           = risk.AlertKindPortfolioRisk
	AlertKindMarginSafety            = risk.AlertKindMarginSafety
	AlertKindDrawdown                = risk.AlertKindDrawdown
	AlertKindProtectionGap           = risk.AlertKindProtectionGap
	AlertKindOrderIntegrity          = risk.AlertKindOrderIntegrity
	AlertKindReconciliationException = risk.AlertKindReconciliationException
	AlertKindGovernance              = risk.AlertKindGovernance
	AlertKindPolicyDrift             = risk.AlertKindPolicyDrift
	AlertKindDataHealth              = risk.AlertKindDataHealth
	AlertKindDeliveryHealth          = risk.AlertKindDeliveryHealth

	AlertEpisodeOpen      = risk.AlertEpisodeOpen
	AlertEpisodeEscalated = risk.AlertEpisodeEscalated
	AlertEpisodeRecovered = risk.AlertEpisodeRecovered

	AlertSeverityObserve = risk.AlertSeverityObserve
	AlertSeverityWatch   = risk.AlertSeverityWatch
	AlertSeverityAct     = risk.AlertSeverityAct
	AlertSeverityUrgent  = risk.AlertSeverityUrgent

	AlertDeliveryUnapproved = risk.AlertDeliveryUnapproved
	AlertDeliveryRecordOnly = risk.AlertDeliveryRecordOnly
	AlertDeliveryInbox      = risk.AlertDeliveryInbox
	AlertDeliveryDigest     = risk.AlertDeliveryDigest
	AlertDeliveryPage       = risk.AlertDeliveryPage

	AlertEvidenceCurrent     = risk.AlertEvidenceCurrent
	AlertEvidencePartial     = risk.AlertEvidencePartial
	AlertEvidenceStale       = risk.AlertEvidenceStale
	AlertEvidenceUnavailable = risk.AlertEvidenceUnavailable
	AlertEvidenceError       = risk.AlertEvidenceError

	AlertDestinationMonitor = risk.AlertDestinationMonitor
	AlertDestinationAlerts  = risk.AlertDestinationAlerts
	AlertDestinationBrief   = risk.AlertDestinationBrief

	AlertCoverageComplete    = risk.AlertCoverageComplete
	AlertCoveragePartial     = risk.AlertCoveragePartial
	AlertCoverageUnavailable = risk.AlertCoverageUnavailable

	AlertCoverageCurrent = risk.AlertCoverageCurrent
	AlertCoverageStale   = risk.AlertCoverageStale
	AlertCoverageUnknown = risk.AlertCoverageUnknown

	AlertSnapshotClear   = risk.AlertSnapshotClear
	AlertSnapshotActive  = risk.AlertSnapshotActive
	AlertSnapshotUnknown = risk.AlertSnapshotUnknown
)

// BuildAlertEpisodeKey delegates opaque identity construction to the pure
// contract; RPC adapters never reinterpret its semantic inputs.
func BuildAlertEpisodeKey(source AlertSource, kind AlertKind, identityParts ...string) (string, error) {
	return risk.BuildAlertEpisodeKey(source, kind, identityParts...)
}

// BuildAlertOccurrenceKey delegates daemon-authored opening, reopen, and
// qualifying-escalation identity to the pure contract. Apps consume the opaque
// result; they do not mint it or decide when it rotates.
func BuildAlertOccurrenceKey(episodeKey string, identityParts ...string) (string, error) {
	return risk.BuildAlertOccurrenceKey(episodeKey, identityParts...)
}

// BuildAlertAuthorityScope returns the opaque account/mode authority carried
// by private candidate snapshots. Raw account and mode values do not cross the
// RPC boundary.
func BuildAlertAuthorityScope(account, mode string) (string, error) {
	return risk.BuildAlertAuthorityScope(account, mode)
}

func ValidateAlertAuthorityScope(value string) error {
	return risk.ValidateAlertAuthorityScope(value)
}

func ValidateAlertCandidate(candidate AlertCandidate) error {
	return candidate.Validate()
}

func ValidateAlertCandidateSnapshot(snapshot AlertCandidateSnapshot) error {
	return snapshot.Validate()
}
