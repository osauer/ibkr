package apphttp

import (
	"errors"
	nethttp "net/http"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Alert Inbox v2 constants identify the additive browser schema and its
// explicitly non-authoritative shadow delivery posture.
const (
	AlertInboxV2SchemaVersion = "alert-inbox-v2"
	AlertInboxV2Authority     = "shadow"
	alertInboxV2PollInterval  = 250 * time.Millisecond
)

// AlertInboxV2DTO is the additive, public shadow projection of the app-owned
// alert delivery ledger. Keep this mapping explicit: the store view also
// carries internal delivery-policy evidence that must never become HTTP or SSE
// data merely because a field is added to an internal type.
type AlertInboxV2DTO struct {
	SchemaVersion  string                        `json:"schema_version"`
	Authority      string                        `json:"authority"`
	Initialized    bool                          `json:"initialized"`
	Generation     uint64                        `json:"generation"`
	AsOf           *time.Time                    `json:"as_of"`
	CurrentState   *rpc.AlertSnapshotState       `json:"current_state"`
	Coverage       *AlertInboxV2CoverageDTO      `json:"coverage"`
	Occurrences    []AlertInboxV2OccurrenceDTO   `json:"occurrences"`
	Attention      AlertInboxV2AttentionDTO      `json:"attention"`
	DeliveryHealth AlertInboxV2DeliveryHealthDTO `json:"delivery_health"`
}

// AlertInboxV2CoverageDTO exposes producer coverage and evidence freshness
// without internal source watermarks or authority scope.
type AlertInboxV2CoverageDTO struct {
	State           rpc.AlertCoverageState     `json:"state"`
	Freshness       rpc.AlertCoverageFreshness `json:"freshness"`
	AsOf            time.Time                  `json:"as_of"`
	ExpectedSources []rpc.AlertSource          `json:"expected_sources"`
	CoveredSources  []rpc.AlertSource          `json:"covered_sources"`
}

// AlertInboxV2OccurrenceDTO is a redacted browser occurrence. DisplayID is the
// only public identity; producer keys and transport evidence remain private.
type AlertInboxV2OccurrenceDTO struct {
	DisplayID          string                      `json:"display_id"`
	Source             rpc.AlertSource             `json:"source"`
	Kind               rpc.AlertKind               `json:"kind"`
	State              rpc.AlertEpisodeState       `json:"state"`
	Severity           rpc.AlertSeverity           `json:"severity"`
	DeliveryPreference rpc.AlertDeliveryPreference `json:"delivery_preference"`
	EvidenceHealth     rpc.AlertEvidenceHealth     `json:"evidence_health"`
	Destination        rpc.AlertDestination        `json:"destination"`
	EvidenceAsOf       time.Time                   `json:"evidence_as_of"`
	StateChangedAt     time.Time                   `json:"state_changed_at"`
	FirstSeenAt        time.Time                   `json:"first_seen_at"`
	LastSeenAt         time.Time                   `json:"last_seen_at"`
	EndedAt            *time.Time                  `json:"ended_at"`
	EndReason          *string                     `json:"end_reason"`
	AttentionSeq       uint64                      `json:"attention_seq"`
}

// AlertInboxV2AttentionRefDTO identifies one redacted unread occurrence.
type AlertInboxV2AttentionRefDTO struct {
	DisplayID string          `json:"display_id"`
	Source    rpc.AlertSource `json:"source"`
	Kind      rpc.AlertKind   `json:"kind"`
}

// AlertInboxV2AttentionDTO exposes the durable render/read cursor and its
// redacted unread references; it is not proof of human attention.
type AlertInboxV2AttentionDTO struct {
	UnreadCount    int                           `json:"unread_count"`
	HighWaterSeq   uint64                        `json:"high_water_seq"`
	ReadThroughSeq uint64                        `json:"read_through_seq"`
	UnreadRefs     []AlertInboxV2AttentionRefDTO `json:"unread_refs"`
}

// AlertInboxV2DeliveryHealthDTO exposes classified app transport health while
// omitting targets, attempts, receipts, and raw transport errors.
type AlertInboxV2DeliveryHealthDTO struct {
	State     string     `json:"state"`
	Class     string     `json:"class"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func (h *handler) alertInboxV2DTO() AlertInboxV2DTO {
	return newAlertInboxV2DTO(h.deps.Store.AlertDelivery(time.Now().UTC()))
}

func newAlertInboxV2DTO(view state.AlertDeliveryView) AlertInboxV2DTO {
	quarantined := view.DeliveryHealth.State == state.AlertDeliveryHealthUnavailable &&
		view.DeliveryHealth.Class == state.AlertDeliveryHealthClassInvalidPersistedState
	deliveryHealth := AlertInboxV2DeliveryHealthDTO{
		State: state.AlertDeliveryHealthShadow, Class: state.AlertDeliveryHealthClassShadow,
	}
	if view.DeliveryHealth.State != "" {
		deliveryHealth = AlertInboxV2DeliveryHealthDTO{
			State: view.DeliveryHealth.State, Class: view.DeliveryHealth.Class,
			UpdatedAt: alertInboxV2Time(view.DeliveryHealth.UpdatedAt),
		}
	}
	dto := AlertInboxV2DTO{
		SchemaVersion:  AlertInboxV2SchemaVersion,
		Authority:      AlertInboxV2Authority,
		Initialized:    view.Initialized && !quarantined,
		Generation:     view.Generation,
		Occurrences:    make([]AlertInboxV2OccurrenceDTO, 0, len(view.Occurrences)),
		Attention:      newAlertInboxV2AttentionDTO(view.Attention),
		DeliveryHealth: deliveryHealth,
	}
	if !dto.Initialized {
		return dto
	}

	dto.AsOf = alertInboxV2Time(view.AsOf)
	currentState := view.CurrentState
	dto.CurrentState = &currentState
	dto.Coverage = &AlertInboxV2CoverageDTO{
		State: view.Coverage.State, Freshness: view.Coverage.Freshness, AsOf: view.Coverage.AsOf.UTC(),
		ExpectedSources: append([]rpc.AlertSource{}, view.Coverage.ExpectedSources...),
		CoveredSources:  append([]rpc.AlertSource{}, view.Coverage.CoveredSources...),
	}
	for _, occurrence := range view.Occurrences {
		item := AlertInboxV2OccurrenceDTO{
			DisplayID: occurrence.DisplayID, Source: occurrence.Source, Kind: occurrence.Kind,
			State: occurrence.State, Severity: occurrence.Severity, DeliveryPreference: occurrence.DeliveryPreference,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf.UTC(), StateChangedAt: occurrence.StateChangedAt.UTC(),
			FirstSeenAt: occurrence.FirstSeenAt.UTC(), LastSeenAt: occurrence.LastSeenAt.UTC(),
			EndedAt: alertInboxV2Time(occurrence.EndedAt), AttentionSeq: occurrence.AttentionSeq,
		}
		if occurrence.EndReason != "" {
			reason := occurrence.EndReason
			item.EndReason = &reason
		}
		dto.Occurrences = append(dto.Occurrences, item)
	}
	dto.DeliveryHealth = AlertInboxV2DeliveryHealthDTO{
		State: view.DeliveryHealth.State, Class: view.DeliveryHealth.Class,
		UpdatedAt: alertInboxV2Time(view.DeliveryHealth.UpdatedAt),
	}
	return dto
}

func alertInboxV2Time(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func (h *handler) handleAlertInboxV2(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.alertInboxV2DTO())
}

func (h *handler) handleAlertInboxV2Attention(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.alertInboxV2DTO().Attention)
}

func (h *handler) handleAlertInboxV2AttentionRead(w nethttp.ResponseWriter, r *nethttp.Request) {
	throughSeq, err := decodeAttentionReadRequest(r)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid alert inbox v2 attention read request")
		return
	}
	current := h.alertInboxV2DTO()
	if !current.Initialized && current.DeliveryHealth.State == state.AlertDeliveryHealthUnavailable &&
		current.DeliveryHealth.Class == state.AlertDeliveryHealthClassInvalidPersistedState {
		writeError(w, nethttp.StatusConflict, "alert inbox v2 state is unavailable")
		return
	}
	_, err = h.deps.Store.MarkAlertDeliveryAttentionRead(throughSeq)
	if err != nil {
		if errors.Is(err, state.ErrAlertDeliveryAttentionRead) {
			writeError(w, nethttp.StatusConflict, err.Error())
			return
		}
		writeError(w, nethttp.StatusInternalServerError, "persist alert inbox v2 attention read cursor")
		return
	}
	writeJSON(w, h.alertInboxV2DTO())
}

func newAlertInboxV2AttentionDTO(attention state.AlertDeliveryAttention) AlertInboxV2AttentionDTO {
	dto := AlertInboxV2AttentionDTO{
		UnreadCount: attention.UnreadCount, HighWaterSeq: attention.HighWaterSeq,
		ReadThroughSeq: attention.ReadThroughSeq,
		UnreadRefs:     make([]AlertInboxV2AttentionRefDTO, 0, len(attention.UnreadRefs)),
	}
	for _, ref := range attention.UnreadRefs {
		dto.UnreadRefs = append(dto.UnreadRefs, AlertInboxV2AttentionRefDTO{
			DisplayID: ref.DisplayID, Source: ref.Source, Kind: ref.Kind,
		})
	}
	return dto
}

type alertInboxV2StreamCursor struct {
	Authority   string
	Initialized bool
	Generation  uint64
}

func newAlertInboxV2StreamCursor(dto AlertInboxV2DTO) alertInboxV2StreamCursor {
	return alertInboxV2StreamCursor{Authority: dto.Authority, Initialized: dto.Initialized, Generation: dto.Generation}
}
