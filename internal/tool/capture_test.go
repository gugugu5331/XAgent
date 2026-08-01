package tool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/budget"
)

func TestCaptureStreamsToBoundedPreviewAndArtifact(t *testing.T) {
	t.Run("inline output aborts staging", func(t *testing.T) {
		store := &captureTestStore{}
		capture := newTestCapture(t, store, 16, 64)
		input := []byte("hello, 世界")
		if written, err := capture.Write(input); err != nil || written != len(input) {
			t.Fatalf("write inline output = %d, %v", written, err)
		}
		result, err := capture.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		writer := store.last
		if result.Preview != string(input) || result.Artifact != nil || result.CapturedBytes != int64(len(input)) {
			t.Fatalf("unexpected inline result: %#v", result)
		}
		if writer == nil || !bytes.Equal(writer.data, input) || writer.aborts != 1 || writer.commits != 0 {
			t.Fatalf("first bytes were not staged then aborted: %#v", writer)
		}
	})

	t.Run("output above inline threshold commits artifact", func(t *testing.T) {
		store := &captureTestStore{}
		capture := newTestCapture(t, store, 4, 64)
		input := []byte("ab世界-tail")
		for _, chunk := range [][]byte{input[:3], input[3:7], input[7:]} {
			if written, err := capture.Write(chunk); err != nil || written != len(chunk) {
				t.Fatalf("stream chunk = %d, %v", written, err)
			}
		}
		result, err := capture.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		writer := store.last
		if !utf8.ValidString(result.Preview) || len(result.Preview) > 4 {
			t.Fatalf("preview is not bounded UTF-8: %q", result.Preview)
		}
		if result.Artifact == nil || !result.Artifact.Complete || result.Artifact.Bytes != int64(len(input)) {
			t.Fatalf("complete artifact metadata missing: %#v", result.Artifact)
		}
		if !result.Truncated || result.TruncationReason != CaptureTruncatedInline {
			t.Fatalf("inline truncation was not reported: %#v", result)
		}
		if writer == nil || !bytes.Equal(writer.data, input) || writer.commits != 1 || writer.aborts != 0 {
			t.Fatalf("stream was not committed exactly once: %#v", writer)
		}
	})

	t.Run("hard limit stops producer and marks artifact incomplete", func(t *testing.T) {
		store := &captureTestStore{}
		capture := newTestCapture(t, store, 4, 10)
		input := bytes.Repeat([]byte("x"), 32)
		written, writeErr := capture.Write(input)
		var limitErr *budget.LimitError
		if written != 10 || !errors.As(writeErr, &limitErr) || limitErr.Scope != string(budget.ToolCaptureBytes) {
			t.Fatalf("hard-limit write = %d, %v", written, writeErr)
		}
		if written, err := capture.Write([]byte("later")); written != 0 || !errors.As(err, &limitErr) {
			t.Fatalf("producer continued after hard limit: %d, %v", written, err)
		}
		result, finishErr := capture.Finish(context.Background())
		if !errors.As(finishErr, &limitErr) || result.Artifact == nil || result.Artifact.Complete {
			t.Fatalf("hard-limit finish = %#v, %v", result, finishErr)
		}
		if result.CapturedBytes != 10 || result.TruncationReason != CaptureTruncatedHardLimit || len(store.last.data) != 10 {
			t.Fatalf("hard limit did not preserve exactly the accepted bytes: %#v", result)
		}
		if store.last.commits != 1 || store.last.aborts != 0 {
			t.Fatal("hard-limited staging was not committed exactly once")
		}
	})

	t.Run("allocation bytes stay bounded by preview", func(t *testing.T) {
		small := captureAllocatedBytesPerOperation(t, 4<<10)
		large := captureAllocatedBytesPerOperation(t, 4<<20)
		t.Logf("allocated bytes/op: 4KiB input=%d, 4MiB input=%d", small, large)
		const tolerance = int64(16 << 10)
		if large > small+tolerance {
			t.Fatalf("capture allocations grew with full input: small=%d large=%d", small, large)
		}
	})
}

func newTestCapture(t *testing.T, store artifact.Store, inlineBytes, captureBytes int64) *Capture {
	t.Helper()
	effective, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureBytes})
	if err != nil {
		t.Fatal(err)
	}
	counter, err := budget.NewCounter(effective, effective)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := NewCapture(context.Background(), CaptureOptions{
		Store:       store,
		Counter:     counter,
		InlineBytes: inlineBytes,
		Metadata:    artifact.Metadata{MediaType: "text/plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return capture
}

func captureAllocatedBytesPerOperation(t *testing.T, inputBytes int) int64 {
	t.Helper()
	input := bytes.Repeat([]byte("z"), inputBytes)
	result := testing.Benchmark(func(benchmark *testing.B) {
		for index := 0; index < benchmark.N; index++ {
			capture := newBenchmarkCapture(benchmark, int64(inputBytes))
			if written, err := capture.Write(input); err != nil || written != len(input) {
				benchmark.Fatalf("capture write = %d, %v", written, err)
			}
			if _, err := capture.Finish(context.Background()); err != nil {
				benchmark.Fatal(err)
			}
		}
	})
	return result.AllocedBytesPerOp()
}

func newBenchmarkCapture(t testing.TB, captureBytes int64) *Capture {
	t.Helper()
	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureBytes})
	if err != nil {
		t.Fatal(err)
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := NewCapture(context.Background(), CaptureOptions{
		Store:       discardCaptureStore{},
		Counter:     counter,
		InlineBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	return capture
}

type captureTestStore struct {
	last *captureTestWriter
}

func (s *captureTestStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	s.last = &captureTestWriter{}
	return s.last, nil
}

func (*captureTestStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}

func (*captureTestStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}

func (*captureTestStore) Close() error { return nil }

type captureTestWriter struct {
	data    []byte
	commits int
	aborts  int
}

func (w *captureTestWriter) Write(input []byte) (int, error) {
	w.data = append(w.data, input...)
	return len(input), nil
}

func (w *captureTestWriter) Commit(context.Context) (artifact.Ref, error) {
	w.commits++
	return artifact.Ref{
		ID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Bytes:     int64(len(w.data)),
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  true,
	}, nil
}

func (w *captureTestWriter) Abort() error {
	w.aborts++
	return nil
}

type discardCaptureStore struct{}

func (discardCaptureStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return &discardCaptureWriter{}, nil
}

func (discardCaptureStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}

func (discardCaptureStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}

func (discardCaptureStore) Close() error { return nil }

type discardCaptureWriter struct {
	bytes int64
}

func (w *discardCaptureWriter) Write(input []byte) (int, error) {
	w.bytes += int64(len(input))
	return len(input), nil
}

func (w *discardCaptureWriter) Commit(context.Context) (artifact.Ref, error) {
	return artifact.Ref{
		ID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Bytes:     w.bytes,
		Available: true,
		Complete:  true,
	}, nil
}

func (*discardCaptureWriter) Abort() error { return nil }
