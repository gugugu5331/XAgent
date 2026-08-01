package tool

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/budget"
)

type CaptureTruncationReason string

const (
	CaptureNotTruncated          CaptureTruncationReason = ""
	CaptureTruncatedInline       CaptureTruncationReason = "inline_preview_limit"
	CaptureTruncatedHardLimit    CaptureTruncationReason = "capture_hard_limit"
	CaptureTruncatedArtifact     CaptureTruncationReason = "artifact_hard_limit"
	CaptureTruncatedWriteFailure CaptureTruncationReason = "capture_write_failure"
	CaptureTruncatedCanceled     CaptureTruncationReason = "capture_canceled"
)

type CaptureOptions struct {
	Store       artifact.Store
	Counter     *budget.Counter
	InlineBytes int64
	Metadata    artifact.Metadata
}

// CaptureResult contains only a bounded UTF-8 preview and opaque artifact
// metadata. It never retains or exposes the complete output buffer.
type CaptureResult struct {
	Preview          string
	Artifact         *artifact.Ref
	CapturedBytes    int64
	Truncated        bool
	TruncationReason CaptureTruncationReason
}

// Capture is a bounded, concurrency-safe streaming writer. Every accepted
// byte is reserved from the operation counter before it is written to private
// artifact staging and copied, up to InlineBytes, into the preview.
type Capture struct {
	mu sync.Mutex

	ctx         context.Context
	counter     *budget.Counter
	writer      artifact.Writer
	inlineBytes int64
	preview     []byte
	captured    int64
	incomplete  bool
	reason      CaptureTruncationReason
	terminalErr error
	finished    bool
}

func NewCapture(ctx context.Context, options CaptureOptions) (*Capture, error) {
	if ctx == nil || options.Store == nil || options.Counter == nil {
		return nil, errors.New("capture dependencies are unavailable")
	}
	if options.InlineBytes <= 0 || options.InlineBytes > toolInlineOutputHardCap() {
		return nil, errors.New("capture inline preview limit is invalid")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	writer, err := options.Store.Begin(ctx, options.Metadata)
	if err != nil {
		return nil, errors.New("begin tool output artifact")
	}
	if writer == nil {
		return nil, errors.New("tool output artifact writer is unavailable")
	}
	initialCapacity := options.InlineBytes
	if initialCapacity > 4<<10 {
		initialCapacity = 4 << 10
	}
	return &Capture{
		ctx:         ctx,
		counter:     options.Counter,
		writer:      writer,
		inlineBytes: options.InlineBytes,
		preview:     make([]byte, 0, int(initialCapacity)),
	}, nil
}

func (c *Capture) Write(input []byte) (int, error) {
	if c == nil {
		return 0, errors.New("capture is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished {
		return 0, errors.New("capture is already finalized")
	}
	if c.terminalErr != nil {
		return 0, c.terminalErr
	}
	if err := c.ctx.Err(); err != nil {
		c.stop(err, CaptureTruncatedCanceled)
		return 0, err
	}
	if len(input) == 0 {
		return 0, nil
	}

	allowed, reserveErr := c.reserve(int64(len(input)))
	if allowed == 0 {
		return 0, c.terminalErr
	}
	written, writeErr := c.writer.Write(input[:int(allowed)])
	if written < 0 || int64(written) > allowed {
		c.stop(errors.New("artifact writer returned an invalid byte count"), CaptureTruncatedWriteFailure)
		return 0, c.terminalErr
	}
	c.retainPreview(input[:written])
	c.captured += int64(written)

	if writeErr != nil {
		reason := CaptureTruncatedWriteFailure
		var limitErr *budget.LimitError
		if errors.As(writeErr, &limitErr) {
			reason = CaptureTruncatedArtifact
		}
		c.stop(writeErr, reason)
		return written, writeErr
	}
	if int64(written) != allowed {
		c.stop(io.ErrShortWrite, CaptureTruncatedWriteFailure)
		return written, c.terminalErr
	}
	if reserveErr != nil {
		return written, reserveErr
	}
	return written, nil
}

// Finish finalizes staging exactly once. Inline-complete output is aborted;
// output above the inline threshold or stopped after accepting bytes is
// committed. A hard-limit error is returned together with the safe metadata.
func (c *Capture) Finish(ctx context.Context) (CaptureResult, error) {
	if c == nil {
		return CaptureResult{}, errors.New("capture is unavailable")
	}
	if ctx == nil {
		return CaptureResult{}, errors.New("capture finish context is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished {
		return CaptureResult{}, errors.New("capture is already finalized")
	}
	c.finished = true
	if err := ctx.Err(); err != nil {
		abortErr := c.writer.Abort()
		if abortErr != nil {
			return c.result(nil), errors.New("abort canceled tool output artifact")
		}
		return c.result(nil), err
	}

	shouldCommit := c.captured > c.inlineBytes || c.incomplete && c.captured > 0
	if !shouldCommit {
		if err := c.writer.Abort(); err != nil {
			return c.result(nil), errors.New("abort inline tool output artifact")
		}
		return c.result(nil), c.terminalErr
	}

	ref, err := c.writer.Commit(ctx)
	if err != nil {
		return c.result(nil), errors.New("commit tool output artifact")
	}
	if c.incomplete {
		ref.Complete = false
	}
	return c.result(&ref), c.terminalErr
}

func (c *Capture) reserve(requested int64) (int64, error) {
	if err := c.counter.Consume(budget.Bytes, requested); err == nil {
		return requested, nil
	} else {
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) {
			c.stop(err, CaptureTruncatedWriteFailure)
			return 0, err
		}
		remaining := c.counter.Remaining(budget.Bytes)
		if remaining > 0 {
			if consumeErr := c.counter.Consume(budget.Bytes, remaining); consumeErr != nil {
				c.stop(consumeErr, CaptureTruncatedWriteFailure)
				return 0, consumeErr
			}
		}
		mapped := &budget.LimitError{
			Scope:     string(budget.ToolCaptureBytes),
			Dimension: limitErr.Dimension,
			Limit:     limitErr.Limit,
			Observed:  limitErr.Observed,
		}
		c.stop(mapped, CaptureTruncatedHardLimit)
		return remaining, mapped
	}
}

func (c *Capture) retainPreview(accepted []byte) {
	remaining := c.inlineBytes - int64(len(c.preview))
	if remaining <= 0 || len(accepted) == 0 {
		return
	}
	if int64(len(accepted)) > remaining {
		accepted = accepted[:int(remaining)]
	}
	c.preview = append(c.preview, accepted...)
}

func (c *Capture) stop(err error, reason CaptureTruncationReason) {
	if c.terminalErr == nil {
		c.terminalErr = err
		c.reason = reason
	}
	c.incomplete = true
}

func (c *Capture) result(ref *artifact.Ref) CaptureResult {
	preview := boundedUTF8Preview(c.preview, int(c.inlineBytes))
	reason := c.reason
	truncated := c.incomplete || c.captured > c.inlineBytes
	if reason == CaptureNotTruncated && c.captured > c.inlineBytes {
		reason = CaptureTruncatedInline
	}
	return CaptureResult{
		Preview:          preview,
		Artifact:         cloneArtifactRef(ref),
		CapturedBytes:    c.captured,
		Truncated:        truncated,
		TruncationReason: reason,
	}
}

func boundedUTF8Preview(input []byte, maximum int) string {
	if maximum <= 0 || len(input) == 0 {
		return ""
	}
	if len(input) > maximum {
		input = input[:maximum]
	}
	var result strings.Builder
	result.Grow(len(input))
	for len(input) > 0 {
		r, size := utf8.DecodeRune(input)
		if r == utf8.RuneError && size == 1 {
			if result.Len()+utf8.RuneLen(utf8.RuneError) > maximum {
				break
			}
			result.WriteRune(utf8.RuneError)
			input = input[1:]
			continue
		}
		if result.Len()+size > maximum {
			break
		}
		result.Write(input[:size])
		input = input[size:]
	}
	return result.String()
}

func toolInlineOutputHardCap() int64 {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == budget.ToolInlineOutputBytes {
			return spec.HardCap
		}
	}
	return 0
}
