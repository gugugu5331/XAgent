package artifact

import (
	"context"
	"io"
	"time"
)

// Ref is the only persistent identity exposed outside the artifact package.
// It deliberately contains no filesystem path or caller-provided label.
type Ref struct {
	ID        string    `json:"id"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
	Available bool      `json:"available"`
	Complete  bool      `json:"complete"`
}

// Metadata contains bounded classification data, never artifact payload or a
// source filesystem path.
type Metadata struct {
	MediaType string `json:"media_type,omitempty"`
}

type Writer interface {
	io.Writer
	Commit(ctx context.Context) (Ref, error)
	Abort() error
}

type CleanupResult struct {
	Removed        int   `json:"removed"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
	Failed         int   `json:"failed"`
}

type Store interface {
	Begin(ctx context.Context, metadata Metadata) (Writer, error)
	OpenForUser(ctx context.Context, id string) (io.ReadCloser, Ref, error)
	Cleanup(ctx context.Context) (CleanupResult, error)
	Close() error
}

type FileStoreOptions struct {
	Root          string
	WorkspaceRoot string
	MaxFileBytes  int64
	MaxTotalBytes int64
	Retention     time.Duration
}
