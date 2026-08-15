package provider

import (
	"context"
	"errors"
	"sync"
	"time"

	"xagent/internal/diagnostics"
)

const maxChatStreamCleanupTimeout = 2 * time.Second

var errChatStreamCleanupTimeout = errors.New("provider stream cleanup exceeded its hard deadline")

type ChatStream interface {
	Events() <-chan StreamEvent
	Close(ctx context.Context) error
}

type ChatStreamOptions struct {
	CleanupTimeout time.Duration
	Diagnostics    diagnostics.BoundedSink
}

type streamCleanup func(context.Context) error
type streamForceClose func()

type chatStream struct {
	events       chan StreamEvent
	closing      chan struct{}
	timeout      time.Duration
	diagnostics  diagnostics.BoundedSink
	cleanupOwner streamCleanup
	forceOwner   streamForceClose

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

var _ ChatStream = (*chatStream)(nil)

func newChatStream(events chan StreamEvent, options ChatStreamOptions, cleanup streamCleanup, forceClose streamForceClose) (*chatStream, error) {
	if events == nil || cleanup == nil {
		return nil, errors.New("provider stream lifecycle options are invalid")
	}
	timeout := options.CleanupTimeout
	if timeout < 0 {
		return nil, errors.New("provider stream cleanup timeout is invalid")
	}
	if timeout == 0 {
		timeout = maxChatStreamCleanupTimeout
	}
	if timeout > maxChatStreamCleanupTimeout {
		return nil, errors.New("provider stream cleanup timeout exceeds hard limit")
	}
	return &chatStream{
		events:       events,
		closing:      make(chan struct{}),
		timeout:      timeout,
		diagnostics:  options.Diagnostics,
		cleanupOwner: cleanup,
		forceOwner:   forceClose,
		closeDone:    make(chan struct{}),
	}, nil
}

func (s *chatStream) emit(ctx context.Context, event StreamEvent) bool {
	if s == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.closing:
		return false
	default:
	}
	select {
	case s.events <- event:
		return true
	case <-s.closing:
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *chatStream) Events() <-chan StreamEvent {
	if s == nil {
		return nil
	}
	return s.events
}

func (s *chatStream) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeOnce.Do(func() {
		close(s.closing)
		go s.cleanup()
	})
	select {
	case <-s.closeDone:
		return s.closeErr
	default:
	}
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		select {
		case <-s.closeDone:
			return s.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (s *chatStream) cleanup() {
	defer close(s.closeDone)
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- s.cleanupOwner(ctx)
	}()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.closeErr = err
			return
		}
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.closeErr = ctx.Err()
			return
		}
	}

	if s.forceOwner != nil {
		s.forceOwner()
	}
	s.closeErr = errChatStreamCleanupTimeout
	if s.diagnostics != nil {
		s.diagnostics.Add(diagnostics.SanitizeInput{
			Code:     "provider_stream_cleanup_timeout",
			Source:   "provider",
			Severity: diagnostics.SeverityError,
			Err:      errChatStreamCleanupTimeout,
		})
	}
}
