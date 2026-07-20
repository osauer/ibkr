package alerts

import (
	"errors"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// AlertPresentation is the complete operator-safe copy surface derived from a
// source-neutral candidate. Producer evidence and private identities never
// enter this value; detailed evidence remains on authenticated daemon views.
type AlertPresentation struct {
	Title       string
	Body        string
	Destination string
}

// PresentAlertCandidate turns only validated enums into display copy. It does
// not decide whether a condition belongs in the dashboard, inbox, digest, or
// Web Push: the daemon candidate and app delivery policy retain those roles.
func PresentAlertCandidate(candidate rpc.AlertCandidate) (AlertPresentation, error) {
	if err := rpc.ValidateAlertCandidate(candidate); err != nil {
		return AlertPresentation{}, fmt.Errorf("invalid alert candidate: %w", err)
	}
	topic, ok := alertKindTopic(candidate.Kind)
	if !ok {
		return AlertPresentation{}, errors.New("unsupported alert kind")
	}
	destination, ok := alertDestination(candidate.Destination)
	if !ok {
		return AlertPresentation{}, errors.New("unsupported alert destination")
	}

	presentation := AlertPresentation{Destination: destination}
	switch candidate.State {
	case rpc.AlertEpisodeRecovered:
		presentation.Title = topic + " recovered"
		presentation.Body = "The daemon observed recovery with current evidence. Open " + destination + " for the current status."
	case rpc.AlertEpisodeEscalated:
		presentation.Title = topic + " escalated"
		presentation.Body = alertSeveritySentence(candidate.Severity, true) + " Open " + destination + " for the current classified condition."
	case rpc.AlertEpisodeOpen:
		presentation.Title = topic + " needs attention"
		presentation.Body = alertSeveritySentence(candidate.Severity, false) + " Open " + destination + " for the current classified condition."
	default:
		return AlertPresentation{}, errors.New("unsupported alert state")
	}
	return presentation, nil
}

// AlertPushPayload is the sole source-neutral lock-screen constructor. A
// record/inbox/digest/unapproved candidate and a recovered occurrence are not
// page payloads, even if a caller reaches this function by mistake. Transport
// eligibility is rechecked separately against durable app state.
func AlertPushPayload(candidate rpc.AlertCandidate, displayID string) (push.Payload, error) {
	if candidate.DeliveryPreference != rpc.AlertDeliveryPage {
		return push.Payload{}, errors.New("alert candidate is not page-class")
	}
	if candidate.State == rpc.AlertEpisodeRecovered {
		return push.Payload{}, errors.New("recovered alert candidate cannot page")
	}
	if !validAlertDisplayID(displayID) {
		return push.Payload{}, errors.New("invalid alert display id")
	}
	presentation, err := PresentAlertCandidate(candidate)
	if err != nil {
		return push.Payload{}, err
	}
	return push.Payload{
		Title: presentation.Title, Body: presentation.Body, Severity: string(candidate.Severity),
		Kind: string(candidate.Kind), Destination: string(candidate.Destination), DisplayID: displayID,
	}, nil
}

func validAlertDisplayID(displayID string) bool {
	if len(displayID) != len("alert-")+16 || !strings.HasPrefix(displayID, "alert-") {
		return false
	}
	for _, char := range displayID[len("alert-"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func alertKindTopic(kind rpc.AlertKind) (string, bool) {
	switch kind {
	case rpc.AlertKindMarketState:
		return "Market regime", true
	case rpc.AlertKindPortfolioRisk:
		return "Portfolio risk", true
	case rpc.AlertKindMarginSafety:
		return "Margin safety", true
	case rpc.AlertKindDrawdown:
		return "Drawdown", true
	case rpc.AlertKindProtectionGap:
		return "Protection gap", true
	case rpc.AlertKindOrderIntegrity:
		return "Order integrity", true
	case rpc.AlertKindReconciliationException:
		return "Reconciliation exception", true
	case rpc.AlertKindGovernance:
		return "Risk process", true
	case rpc.AlertKindPolicyDrift:
		return "Risk policy", true
	case rpc.AlertKindDataHealth:
		return "Trading data health", true
	case rpc.AlertKindDeliveryHealth:
		return "Alert delivery", true
	default:
		return "", false
	}
}

func alertDestination(destination rpc.AlertDestination) (string, bool) {
	switch destination {
	case rpc.AlertDestinationMonitor:
		return "Monitor", true
	case rpc.AlertDestinationAlerts:
		return "Alerts", true
	case rpc.AlertDestinationBrief:
		return "Brief", true
	default:
		return "", false
	}
}

func alertSeveritySentence(severity rpc.AlertSeverity, escalated bool) string {
	if escalated {
		switch severity {
		case rpc.AlertSeverityObserve:
			return "The daemon recorded a new escalation."
		case rpc.AlertSeverityWatch:
			return "The daemon raised this condition to watch."
		case rpc.AlertSeverityAct:
			return "The daemon raised this condition to action grade."
		case rpc.AlertSeverityUrgent:
			return "The daemon raised this condition to urgent."
		}
	}
	switch severity {
	case rpc.AlertSeverityObserve:
		return "The daemon recorded this condition for review."
	case rpc.AlertSeverityWatch:
		return "The daemon classified this condition at watch."
	case rpc.AlertSeverityAct:
		return "The daemon classified this condition at action grade."
	case rpc.AlertSeverityUrgent:
		return "The daemon classified this condition as urgent."
	default:
		return "The daemon recorded this condition."
	}
}
