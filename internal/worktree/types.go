package worktree

import (
	"context"
	"time"
)

const (
	RecordSchemaVersion    = 1
	ManifestSchemaVersion  = 1
	TombstoneSchemaVersion = 1
)

type State string

const (
	StateCreating        State = "creating"
	StateInitializing    State = "initializing"
	StateReady           State = "ready"
	StateActive          State = "active"
	StateSettling        State = "settling"
	StateRetained        State = "retained"
	StateDeleting        State = "deleting"
	StateDeleted         State = "deleted"
	StatePartial         State = "partial"
	StateManualAttention State = "manual_attention"
)

func (s State) Valid() bool {
	switch s {
	case StateCreating, StateInitializing, StateReady, StateActive, StateSettling,
		StateRetained, StateDeleting, StateDeleted, StatePartial, StateManualAttention:
		return true
	default:
		return false
	}
}

func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	allowed := map[State]map[State]bool{
		StateCreating:        {StateInitializing: true, StateDeleting: true, StatePartial: true, StateManualAttention: true},
		StateInitializing:    {StateReady: true, StateDeleting: true, StatePartial: true, StateManualAttention: true},
		StateReady:           {StateActive: true, StateSettling: true, StateDeleting: true, StatePartial: true, StateManualAttention: true},
		StateActive:          {StateSettling: true, StatePartial: true, StateManualAttention: true},
		StateSettling:        {StateRetained: true, StateDeleting: true, StateDeleted: true, StatePartial: true, StateManualAttention: true},
		StateRetained:        {StateActive: true, StateSettling: true, StateDeleting: true, StateManualAttention: true},
		StateDeleting:        {StateDeleted: true, StatePartial: true, StateManualAttention: true},
		StatePartial:         {StateSettling: true, StateManualAttention: true},
		StateManualAttention: {},
		StateDeleted:         {},
	}
	return allowed[from][to]
}

type Record struct {
	SchemaVersion      int                `json:"schema_version"`
	Revision           uint64             `json:"revision"`
	WorkspaceID        string             `json:"workspace_id"`
	OwnerID            string             `json:"owner_id"`
	RepositoryIdentity RepositoryIdentity `json:"repository_identity"`
	LogicalName        string             `json:"logical_name"`
	Directory          string             `json:"directory"`
	Branch             string             `json:"branch"`
	BaseOID            string             `json:"base_oid"`
	HeadOID            string             `json:"head_oid,omitempty"`
	State              State              `json:"state"`
	Manifest           ManifestRef        `json:"manifest,omitempty"`
	Lease              LeaseRecord        `json:"lease,omitempty"`
	Settlement         SettlementRecord   `json:"settlement,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	ExpiresAt          time.Time          `json:"expires_at,omitempty"`
	IntegrityDigest    string             `json:"integrity_digest"`
}

func (r Record) Clone() Record { return r }

type Lease struct {
	WorkspaceID string    `json:"workspace_id"`
	OwnerID     string    `json:"owner_id"`
	Root        string    `json:"-"`
	Branch      string    `json:"branch"`
	BaseOID     string    `json:"base_oid"`
	AcquiredAt  time.Time `json:"acquired_at"`
}

type LeaseRecord struct {
	OwnerID    string    `json:"owner_id,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	AcquiredAt time.Time `json:"acquired_at,omitempty"`
	Heartbeat  time.Time `json:"heartbeat,omitempty"`
}

type SettlementState string

const (
	SettlementDeleted         SettlementState = "deleted"
	SettlementRetained        SettlementState = "retained"
	SettlementPartial         SettlementState = "partial"
	SettlementManualAttention SettlementState = "manual_attention"
)

type Settlement struct {
	State      SettlementState
	Dirty      bool
	Unpushed   bool
	ReasonCode string
}

type SettlementRecord struct {
	State       SettlementState `json:"state,omitempty"`
	Dirty       bool            `json:"dirty,omitempty"`
	Unpushed    bool            `json:"unpushed,omitempty"`
	ReasonCode  string          `json:"reason_code,omitempty"`
	CompletedAt time.Time       `json:"completed_at,omitempty"`
}

type ManifestRef struct {
	Path   string `json:"path,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type Tombstone struct {
	SchemaVersion  int       `json:"schema_version"`
	WorkspaceID    string    `json:"workspace_id"`
	DeletedAt      time.Time `json:"deleted_at"`
	RecordRevision uint64    `json:"record_revision"`
	Digest         string    `json:"digest"`
}

type ListQuery struct {
	States []State
	Limit  int
}

type Store interface {
	Create(context.Context, Record) error
	Load(context.Context, string) (Record, error)
	CompareAndSwap(context.Context, Record, uint64) error
	WriteTombstone(context.Context, Tombstone) error
	List(context.Context, ListQuery) ([]Record, error)
}
