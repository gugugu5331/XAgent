package tool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
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

func TestCaptureTerminalFailuresPublishNoUsableRef(t *testing.T) {
	zeroWriteErr := errors.New("zero-byte staging write failed")
	zeroWriter := &captureTestWriter{writeErr: zeroWriteErr}
	zeroStore := &captureTestStore{next: zeroWriter}
	zeroCapture := newTestCapture(t, zeroStore, 8, 64)
	if written, writeErr := zeroCapture.Write([]byte("payload")); written != 0 || !errors.Is(writeErr, zeroWriteErr) {
		t.Fatalf("zero-byte write = %d, %v", written, writeErr)
	}
	zeroResult, zeroFinishErr := zeroCapture.Finish(context.Background())
	if !errors.Is(zeroFinishErr, zeroWriteErr) || !reflect.DeepEqual(zeroResult, CaptureResult{}) {
		t.Fatalf("zero-byte terminal result = %#v, %v", zeroResult, zeroFinishErr)
	}
	if zeroWriter.aborts != 1 || zeroWriter.commits != 0 {
		t.Fatalf("zero-byte failure finalization: aborts=%d commits=%d", zeroWriter.aborts, zeroWriter.commits)
	}

	commitErr := errors.New("staging commit failed")
	failedRef := &artifact.Ref{
		ID:        "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Bytes:     9,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: false,
		Complete:  false,
	}
	commitWriter := &captureTestWriter{commitErr: commitErr, commitRef: failedRef}
	commitStore := &captureTestStore{next: commitWriter}
	commitCapture := newTestCapture(t, commitStore, 8, 64)
	if written, writeErr := commitCapture.Write([]byte("123456789")); writeErr != nil || written != 9 {
		t.Fatalf("commit-failure staging write = %d, %v", written, writeErr)
	}
	commitResult, commitFinishErr := commitCapture.Finish(context.Background())
	if commitFinishErr == nil || !reflect.DeepEqual(commitResult, CaptureResult{}) {
		t.Fatalf("commit failure published metadata: %#v, %v", commitResult, commitFinishErr)
	}
	if commitWriter.commits != 1 || commitWriter.aborts != 0 {
		t.Fatalf("commit failure finalization: aborts=%d commits=%d", commitWriter.aborts, commitWriter.commits)
	}

	unavailableWriter := &captureTestWriter{commitRef: failedRef}
	unavailableStore := &captureTestStore{next: unavailableWriter}
	unavailableCapture := newTestCapture(t, unavailableStore, 8, 64)
	if written, writeErr := unavailableCapture.Write([]byte("123456789")); writeErr != nil || written != 9 {
		t.Fatalf("unavailable-ref staging write = %d, %v", written, writeErr)
	}
	unavailableResult, unavailableErr := unavailableCapture.Finish(context.Background())
	if unavailableErr == nil || !reflect.DeepEqual(unavailableResult, CaptureResult{}) {
		t.Fatalf("unavailable ref was published: %#v, %v", unavailableResult, unavailableErr)
	}
}

func TestCaptureAbortCommitAndUTF8ThresholdMatrix(t *testing.T) {
	expected := []string{
		"ascii_below_cap_abort",
		"ascii_at_cap_abort",
		"ascii_cap_plus_one_commit",
		"utf8_at_cap_abort",
		"utf8_over_cap_commit",
		"capture_hard_limit_incomplete_commit",
		"artifact_hard_limit_incomplete_commit",
		"write_failure_after_bytes_incomplete_commit",
		"canceled_after_bytes_incomplete_commit",
		"zero_byte_write_failure_no_ref",
		"commit_failure_no_ref",
	}
	seen := make(map[string]bool, len(expected))
	run := func(name string, test func(*testing.T)) {
		t.Run(name, func(t *testing.T) {
			seen[name] = true
			test(t)
		})
	}
	assertTerminal := func(t *testing.T, input string, wantCommit bool) {
		t.Helper()
		store := &captureTestStore{}
		capture := newTestCapture(t, store, 8, 64)
		if written, err := capture.Write([]byte(input)); err != nil || written != len(input) {
			t.Fatalf("capture write = %d, %v", written, err)
		}
		result, err := capture.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(result.Preview) || len(result.Preview) > 8 {
			t.Fatalf("invalid bounded preview %q", result.Preview)
		}
		if wantCommit {
			if store.last.commits != 1 || store.last.aborts != 0 || result.Artifact == nil || !result.Artifact.Complete || !result.Truncated || result.TruncationReason != CaptureTruncatedInline {
				t.Fatalf("complete commit result = %#v writer=%#v", result, store.last)
			}
		} else if store.last.aborts != 1 || store.last.commits != 0 || result.Artifact != nil || result.Truncated || result.TruncationReason != CaptureNotTruncated {
			t.Fatalf("inline abort result = %#v writer=%#v", result, store.last)
		}
	}

	run("ascii_below_cap_abort", func(t *testing.T) { assertTerminal(t, "1234567", false) })
	run("ascii_at_cap_abort", func(t *testing.T) { assertTerminal(t, "12345678", false) })
	run("ascii_cap_plus_one_commit", func(t *testing.T) { assertTerminal(t, "123456789", true) })
	run("utf8_at_cap_abort", func(t *testing.T) { assertTerminal(t, "你a🙂", false) })
	run("utf8_over_cap_commit", func(t *testing.T) { assertTerminal(t, "你ab🙂", true) })
	run("capture_hard_limit_incomplete_commit", func(t *testing.T) {
		store := &captureTestStore{}
		capture := newTestCapture(t, store, 4, 5)
		written, writeErr := capture.Write([]byte("123456789"))
		result, finishErr := capture.Finish(context.Background())
		var limitErr *budget.LimitError
		if written != 5 || !errors.As(writeErr, &limitErr) || !errors.As(finishErr, &limitErr) || result.Artifact == nil || result.Artifact.Complete || result.TruncationReason != CaptureTruncatedHardLimit || store.last.commits != 1 || store.last.aborts != 0 {
			t.Fatalf("hard-limit terminal = %#v write=%v finish=%v", result, writeErr, finishErr)
		}
	})
	run("artifact_hard_limit_incomplete_commit", func(t *testing.T) {
		artifactErr := &budget.LimitError{Scope: "artifact", Dimension: budget.Bytes, Limit: 3, Observed: 9}
		writer := &captureTestWriter{maxWrite: 3, writeErr: artifactErr}
		store := &captureTestStore{next: writer}
		capture := newTestCapture(t, store, 8, 64)
		_, writeErr := capture.Write([]byte("123456789"))
		result, finishErr := capture.Finish(context.Background())
		if !errors.Is(writeErr, artifactErr) || !errors.Is(finishErr, artifactErr) || result.Artifact == nil || result.Artifact.Complete || result.TruncationReason != CaptureTruncatedArtifact || writer.commits != 1 || writer.aborts != 0 {
			t.Fatalf("artifact-limit terminal = %#v write=%v finish=%v", result, writeErr, finishErr)
		}
	})
	run("write_failure_after_bytes_incomplete_commit", func(t *testing.T) {
		writeFailure := errors.New("partial write failed")
		writer := &captureTestWriter{maxWrite: 3, writeErr: writeFailure}
		capture := newTestCapture(t, &captureTestStore{next: writer}, 8, 64)
		_, writeErr := capture.Write([]byte("123456789"))
		result, finishErr := capture.Finish(context.Background())
		if !errors.Is(writeErr, writeFailure) || !errors.Is(finishErr, writeFailure) || result.Artifact == nil || result.Artifact.Complete || result.TruncationReason != CaptureTruncatedWriteFailure || writer.commits != 1 || writer.aborts != 0 {
			t.Fatalf("partial-write terminal = %#v write=%v finish=%v", result, writeErr, finishErr)
		}
	})
	run("canceled_after_bytes_incomplete_commit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		store := &captureTestStore{}
		capture := newTestCaptureWithContext(t, ctx, store, 8, 64)
		if _, err := capture.Write([]byte("abc")); err != nil {
			t.Fatal(err)
		}
		cancel()
		_, writeErr := capture.Write([]byte("later"))
		result, finishErr := capture.Finish(context.Background())
		if !errors.Is(writeErr, context.Canceled) || !errors.Is(finishErr, context.Canceled) || result.Artifact == nil || result.Artifact.Complete || result.TruncationReason != CaptureTruncatedCanceled || store.last.commits != 1 || store.last.aborts != 0 {
			t.Fatalf("cancel terminal = %#v write=%v finish=%v", result, writeErr, finishErr)
		}
	})
	run("zero_byte_write_failure_no_ref", func(t *testing.T) {
		writer := &captureTestWriter{writeErr: errors.New("zero write failed")}
		capture := newTestCapture(t, &captureTestStore{next: writer}, 8, 64)
		_, _ = capture.Write([]byte("payload"))
		result, finishErr := capture.Finish(context.Background())
		if finishErr == nil || !reflect.DeepEqual(result, CaptureResult{}) || writer.aborts != 1 || writer.commits != 0 {
			t.Fatalf("zero-write terminal = %#v err=%v", result, finishErr)
		}
	})
	run("commit_failure_no_ref", func(t *testing.T) {
		writer := &captureTestWriter{commitErr: errors.New("commit failed")}
		capture := newTestCapture(t, &captureTestStore{next: writer}, 8, 64)
		_, _ = capture.Write([]byte("123456789"))
		result, finishErr := capture.Finish(context.Background())
		if finishErr == nil || !reflect.DeepEqual(result, CaptureResult{}) || writer.commits != 1 || writer.aborts != 0 {
			t.Fatalf("commit failure terminal = %#v err=%v", result, finishErr)
		}
	})
	if len(seen) != len(expected) {
		t.Fatalf("seen %v, want %v", seen, expected)
	}
	for _, name := range expected {
		if !seen[name] {
			t.Fatalf("missing canonical subtest %q", name)
		}
	}
}

func newTestCapture(t *testing.T, store artifact.Store, inlineBytes, captureBytes int64) *Capture {
	return newTestCaptureWithContext(t, context.Background(), store, inlineBytes, captureBytes)
}

func newTestCaptureWithContext(t *testing.T, ctx context.Context, store artifact.Store, inlineBytes, captureBytes int64) *Capture {
	t.Helper()
	effective, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureBytes})
	if err != nil {
		t.Fatal(err)
	}
	counter, err := budget.NewCounter(effective, effective)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := NewCapture(ctx, CaptureOptions{
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
	next *captureTestWriter
}

func (s *captureTestStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	s.last = s.next
	if s.last == nil {
		s.last = &captureTestWriter{}
	}
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
	data       []byte
	writeCalls int
	commits    int
	aborts     int
	writeErr   error
	commitErr  error
	commitRef  *artifact.Ref
	abortErr   error
	maxWrite   int
}

func (w *captureTestWriter) Write(input []byte) (int, error) {
	w.writeCalls++
	if w.maxWrite > 0 && len(input) > w.maxWrite {
		w.data = append(w.data, input[:w.maxWrite]...)
		return w.maxWrite, w.writeErr
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.data = append(w.data, input...)
	return len(input), nil
}

func (w *captureTestWriter) Commit(context.Context) (artifact.Ref, error) {
	w.commits++
	if w.commitRef != nil {
		return *w.commitRef, w.commitErr
	}
	if w.commitErr != nil {
		return artifact.Ref{}, w.commitErr
	}
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
	return w.abortErr
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
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  true,
	}, nil
}

func (*discardCaptureWriter) Abort() error { return nil }
