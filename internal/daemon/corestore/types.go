package corestore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRevisionConflict       = errors.New("corestore: revision conflict")
	ErrPreviewTokenConsumed   = errors.New("corestore: preview token already consumed")
	ErrBrokerScopeCollision   = errors.New("corestore: broker scope collision")
	ErrAuthorityMismatch      = errors.New("corestore: authority mismatch")
	ErrRollback               = errors.New("corestore: authority rollback detected")
	ErrBlocked                = errors.New("corestore: health is blocked")
	ErrOrderIDFloor           = errors.New("corestore: reserved order id does not advance global floor")
	ErrCheckpointBusy         = errors.New("corestore: WAL checkpoint is busy")
	ErrLegacyImportConflict   = errors.New("corestore: legacy authority was already imported from a different source")
	ErrFreshAuthorityConflict = errors.New("corestore: fresh trading authority requires empty order and purge state")
	ErrProjectionConflict     = errors.New("corestore: immutable projection conflict")
)

// Options configures the authoritative store. Path is required; the daemon
// integration decides where daemon.db lives.
type Options struct {
	Path        string
	BusyTimeout time.Duration
	// MigrationBackupPath must name a verified, reopenable backup at the
	// exact current write head before an existing older schema may upgrade.
	MigrationBackupPath string
	// MinimumHead, when non-nil, prevents opening an older copy of the same
	// authority. It is intended for restore/backup selection boundaries.
	MinimumHead *AuthorityHead
	// CommitObserver runs synchronously after every successful durable
	// mutation while the store's write lock is still held. Production uses it
	// to persist an external monotonic head; an observer failure is returned
	// to the caller and latches the store unhealthy without rolling back the
	// already-committed SQLite transaction.
	CommitObserver func(AuthorityHead) error
}

// AuthorityHead is the rollback-detection identity and monotonic write head
// carried inside every database and backup.
type AuthorityHead struct {
	AuthorityEpoch   string
	HeadGeneration   int64
	LastEventSeq     int64
	SignerGeneration int64
}

// Health is a process-lifetime latch. Critical mutation failures caused by a
// full, busy, corrupt, or I/O-failing SQLite store transition Ready to false;
// only closing and explicitly reopening the store can reset it.
type Health struct {
	Ready     bool
	Code      string
	BlockedAt time.Time
}

type StateDocument struct {
	ScopeKey  string
	Kind      string
	Revision  int64
	JSON      []byte
	UpdatedAt time.Time
}

type StateDocumentCAS struct {
	ScopeKey         string
	Kind             string
	ExpectedRevision int64
	JSON             []byte
	// UpdatedAtNotBefore is an optional atomic commit-clock floor. The store
	// compares it with the exact timestamp it will persist inside the same
	// critical mutation, before touching the document or authority head. It is
	// zero for ordinary callers.
	UpdatedAtNotBefore time.Time
}

type RevisionConflictError struct {
	Expected int64
	Actual   int64
	Exists   bool
}

func (e *RevisionConflictError) Error() string {
	if !e.Exists {
		return fmt.Sprintf("%v: expected revision %d, document does not exist", ErrRevisionConflict, e.Expected)
	}
	return fmt.Sprintf("%v: expected revision %d, actual revision %d", ErrRevisionConflict, e.Expected, e.Actual)
}

func (e *RevisionConflictError) Is(target error) bool { return target == ErrRevisionConflict }

// ObservationInput stores Payload byte-for-byte. ContentType and MetadataJSON
// describe it without interpreting untrusted source content as authority.
type ObservationInput struct {
	ScopeKey         string
	Source           string
	Kind             string
	ObservedAt       time.Time
	ContentType      string
	Payload          []byte
	MetadataJSON     []byte
	DecisionEligible bool
}

type ObservationReceipt struct {
	ID            int64
	PayloadSHA256 [sha256.Size]byte
	RecordedAt    time.Time
}

type Observation struct {
	ID            int64
	ScopeKey      string
	Source        string
	Kind          string
	ObservedAt    time.Time
	RecordedAt    time.Time
	ContentType   string
	Payload       []byte
	PayloadSHA256 [sha256.Size]byte
	MetadataJSON  []byte
	// DecisionEligible is a typed authority boundary. Imported legacy
	// observations are false and must never seed current runtime state.
	DecisionEligible bool
}

type ObservationQuery struct {
	ScopeKey           string
	Source             string
	Kind               string
	FromObservedAtMS   int64
	ToObservedAtMS     int64
	AfterObservationID int64
	DecisionEligible   *bool
	Limit              int
}

// BrokerScope binds an authority namespace to all broker identity pins. A
// ScopeKey can never be rebound, and one binding cannot be aliased by a second
// ScopeKey.
type BrokerScope struct {
	ScopeKey string
	Endpoint string
	ClientID int
	Account  string
	Mode     string
}

type PreviewTokenDigest [sha256.Size]byte

// HashPreviewTokenID hashes the canonical preview-token identifier. Callers
// must not pass the raw signed token; legacy state stores only this identifier.
func HashPreviewTokenID(previewTokenID string) PreviewTokenDigest {
	return sha256.Sum256([]byte(previewTokenID))
}

type ActionKind string

const (
	ActionPlace        ActionKind = "place"
	ActionModify       ActionKind = "modify"
	ActionCancel       ActionKind = "cancel"
	ActionPurge        ActionKind = "purge"
	ActionRestore      ActionKind = "restore"
	ActionExercise     ActionKind = "exercise"
	ActionSmokeCleanup ActionKind = "smoke_cleanup"
)

type TransmitOrigin string

const (
	OriginAgentCLI TransmitOrigin = "agent_gated_cli"
	OriginHumanCLI TransmitOrigin = "human_cli"
	OriginDaemon   TransmitOrigin = "daemon_internal"
)

type OrderEventRecord struct {
	EventSeq        int64
	Scope           BrokerScope
	EventKey        string
	AtMS            int64
	Type            string
	Action          ActionKind
	Origin          TransmitOrigin
	OrderRef        string
	PreviewTokenID  string
	ReservedOrderID int64
	PermID          int64
	Status          string
	RawJSON         []byte
}

type OrderQuery struct {
	ScopeKey        string
	FromAtMS        int64
	ToAtMS          int64
	AfterEventSeq   int64
	OrderRef        string
	ReservedOrderID *int64
	PermID          *int64
	PreviewTokenID  string
	Limit           int
}

// PreTransmitRequest is committed before a caller may transmit. Success is
// evidence of durable staging, not broker-submit authority.
type PreTransmitRequest struct {
	Scope                 BrokerScope
	TokenDigest           PreviewTokenDigest
	AuthorityEpoch        string
	SignerGeneration      int64
	RequestedOrderIDFloor int64
	ReservedOrderID       int64
	Action                ActionKind
	Origin                TransmitOrigin
	Events                []OrderEventRecord
}

type LifecycleCommit struct {
	Scope  BrokerScope
	Events []OrderEventRecord
	State  *StateDocumentCAS
}

type LifecycleResult struct {
	EventSeqs []int64
	State     *StateDocument
	Head      AuthorityHead
}

type LegacyConsumedToken struct {
	Scope          BrokerScope
	PreviewTokenID string
	ConsumedAt     time.Time
}

type LegacyOrderFloor struct {
	Scope BrokerScope
	Floor int64
}

type LegacyOrderImport struct {
	SourceFingerprint string
	GlobalFloor       int64
	ScopedFloors      []LegacyOrderFloor
	ConsumedTokens    []LegacyConsumedToken
	Events            []OrderEventRecord
}

type LegacyOrderImportResult struct {
	Imported  bool
	EventSeqs []int64
	Head      AuthorityHead
}

type PreTransmitResult struct {
	EffectiveOrderIDFloor int64
	EventSeqs             []int64
	Head                  AuthorityHead
}

type IntegrityReport struct {
	QuickCheckResults    []string
	ForeignKeyViolations []ForeignKeyViolation
}

func (r IntegrityReport) OK() bool {
	return len(r.QuickCheckResults) == 1 && r.QuickCheckResults[0] == "ok" && len(r.ForeignKeyViolations) == 0
}

type ForeignKeyViolation struct {
	Table       string
	RowID       *int64
	ParentTable string
	ForeignKey  int64
}

type BackupInfo struct {
	Path      string
	Head      AuthorityHead
	Integrity IntegrityReport
}

type StatementFileRecord struct {
	ScopeKey             string
	FileKey              string
	SizeBytes            int64
	SHA256               [sha256.Size]byte
	Status               string
	StatementGeneratedAt *time.Time
	IngestedAt           *time.Time
	UpdatedAt            time.Time
}

type StatementEquityDayRecord struct {
	ID                  int64
	ScopeKey            string
	AccountKey          string
	Day                 string
	EquityBaseText      string
	StatementFileKey    string
	StatementFileSHA256 [sha256.Size]byte
	GeneratedAt         time.Time
	RawJSON             []byte
}

type CheckpointResult struct {
	Busy               int
	LogFrames          int
	CheckpointedFrames int
}

type EventInput struct {
	ScopeKey    string
	EventKey    string
	Type        string
	Action      string
	Origin      string
	OccurredAt  time.Time
	PayloadJSON []byte
	Projection  EventProjection
}

type EventReceipt struct {
	EventSeq   int64
	RecordedAt time.Time
	Head       AuthorityHead
}

type EventRecord struct {
	EventSeq    int64
	ScopeKey    string
	EventKey    string
	Type        string
	Action      string
	Origin      string
	OccurredAt  time.Time
	RecordedAt  time.Time
	PayloadJSON []byte
}

type EventQuery struct {
	ScopeKey      string
	Type          string
	FromAtMS      int64
	ToAtMS        int64
	AfterEventSeq int64
	Limit         int
}

// EventProjection is a typed tagged union; zero values append only the
// canonical event_log row. At most one member may be non-nil.
type EventProjection struct {
	RegimeDecision   *RegimeDecisionProjection
	RuleTransition   *RuleTransitionProjection
	CanaryTransition *CanaryTransitionProjection
	CapitalEvent     *CapitalEventProjection
	RiskPolicyEvent  *RiskPolicyEventProjection
	ProposalOutcome  *ProposalOutcomeProjection
}

type RegimeDecisionProjection struct {
	DecisionKey string
	Stage       string
	Severity    string
	Readiness   string
	Confidence  string
	Verdict     string
	Fingerprint string
	Indicators  []RegimeIndicatorProjection
}

type RegimeIndicatorProjection struct {
	Indicator       string
	Status          string
	Band            string
	Value           *float64
	Depth           *float64
	StreakSessions  *int64
	Freshness       string
	Eligible        *bool
	Latched         bool
	ThresholdsLabel string
}

type RuleTransitionProjection struct {
	RuleID, Status, PreviousStatus, PolicyID, PolicyFingerprint string
	PolicyVersion                                               *int64
}
type CanaryTransitionProjection struct {
	Action, Severity, Direction, MarketStage, InputHealth string
	PortfolioAlertRelevant                                *bool
}
type CapitalEventProjection struct{ Kind, AmountBaseText, EffectiveAt, ReportID string }
type RiskPolicyEventProjection struct {
	Kind, PolicyID, PolicyFingerprint string
	PolicyVersion                     *int64
}
type ProposalOutcomeProjection struct{ ProposalKey, Revision, Bucket, Symbol, SecType, Action, State string }
