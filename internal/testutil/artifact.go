package testutil

import (
	"context"
	"errors"

	"xagent/internal/artifact"
)

// ArtifactSink is a narrow programmable artifact Begin capability for tests.
// It intentionally does not implement artifact.Store.
type ArtifactSink struct {
	BeginFunc func(context.Context, artifact.Metadata) (artifact.Writer, error)
}

func (s *ArtifactSink) Begin(ctx context.Context, metadata artifact.Metadata) (artifact.Writer, error) {
	if s == nil || s.BeginFunc == nil {
		return nil, errors.New("test artifact sink is unavailable")
	}
	return s.BeginFunc(ctx, metadata)
}

// ArtifactWriter is a programmable artifact.Writer with no filesystem path.
type ArtifactWriter struct {
	WriteFunc  func([]byte) (int, error)
	CommitFunc func(context.Context) (artifact.Ref, error)
	AbortFunc  func() error
}

func (w *ArtifactWriter) Write(payload []byte) (int, error) {
	if w == nil || w.WriteFunc == nil {
		return 0, errors.New("test artifact write is unavailable")
	}
	return w.WriteFunc(payload)
}

func (w *ArtifactWriter) Commit(ctx context.Context) (artifact.Ref, error) {
	if w == nil || w.CommitFunc == nil {
		return artifact.Ref{}, errors.New("test artifact commit is unavailable")
	}
	return w.CommitFunc(ctx)
}

func (w *ArtifactWriter) Abort() error {
	if w == nil || w.AbortFunc == nil {
		return errors.New("test artifact abort is unavailable")
	}
	return w.AbortFunc()
}
