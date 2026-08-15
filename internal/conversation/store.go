package conversation

import (
	"context"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

// StateDigest is the canonical digest identity of persisted v2 state.
type StateDigest string

type RecoveryStatus string

const (
	RecoveryClean       RecoveryStatus = "clean"
	RecoveryPartial     RecoveryStatus = "partial"
	RecoveryPlaceholder RecoveryStatus = "placeholder"
)

type RecoveryReport struct {
	Status            RecoveryStatus
	LastValidRevision uint64
	SkippedRecords    int
	Diagnostics       []diagnostics.Diagnostic
}

type ConversationSummary struct {
	ID           string
	Title        redact.SafeText
	UpdatedAt    time.Time
	MessageCount int
}

type ListEntry struct {
	Summary   ConversationSummary
	Available bool
	Recovery  RecoveryReport
}

type PersistedState struct {
	Revision       uint64
	MessageCount   int
	Digest         StateDigest
	MessagesDigest StateDigest
}

type ListResult struct {
	Entries      []ListEntry
	Truncated    bool
	ScannedFiles int
	ScannedBytes int64
	Diagnostics  []diagnostics.Diagnostic
}

type LoadResult struct {
	Conversation *Conversation
	Available    bool
	Persisted    PersistedState
	Recovery     RecoveryReport
}

type SaveKind string

const (
	SaveNoop     SaveKind = "noop"
	SaveBatch    SaveKind = "batch"
	SaveSnapshot SaveKind = "snapshot"
)

type SaveResult struct {
	Kind      SaveKind
	Persisted PersistedState
}

type MaintenanceResult struct {
	ScannedFiles int
	ScannedBytes int64
	Deleted      int
	Skipped      int
	Truncated    bool
	Diagnostics  []diagnostics.Diagnostic
}

type Store interface {
	Create(ctx context.Context) (*Conversation, error)
	List(ctx context.Context) (ListResult, error)
	Load(ctx context.Context, id string) (LoadResult, error)
	Save(ctx context.Context, conversation *Conversation) (SaveResult, error)
	Maintain(ctx context.Context) (MaintenanceResult, error)
}
