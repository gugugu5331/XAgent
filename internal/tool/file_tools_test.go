package tool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/budget"
)

func TestReadCancellationLongLineAndBudget(t *testing.T) {
	t.Run("canceled before open", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "unread.txt"), []byte("must not be returned"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := NewReadTool(root).Execute(ctx, Input{
			Name:      "Read",
			CallID:    "canceled",
			Arguments: map[string]any{"path": "unread.txt"},
		})
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrTimeout || result.Content != "" {
			t.Fatalf("canceled Read returned an unsafe result: %#v", result)
		}
	})

	t.Run("long line stops at output budget", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(strings.Repeat("x", 2<<20)), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedReadExecutor(t, root, 1<<20, 10, 64)
		result := executor.Execute(context.Background(), Call{ID: "long", Name: "Read", ArgumentsJSON: `{"path":"long.txt"}`})
		if result.Status != StatusSuccess || !result.Truncated || len(result.Content) > 64 || result.Content != strings.Repeat("x", 64) {
			t.Fatalf("long line was not bounded from the first chunk: %#v", result)
		}
		if result.Data["truncation_reason"] != string(budget.ToolInlineOutputBytes) || result.Data["lines"] != int64(1) {
			t.Fatalf("long-line budget metadata is incorrect: %#v", result.Data)
		}
	})

	t.Run("single file byte budget", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("0123456789abcdef"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedReadExecutor(t, root, 8, 10, 64)
		result := executor.Execute(context.Background(), Call{ID: "file-budget", Name: "Read", ArgumentsJSON: `{"path":"large.txt"}`})
		if result.Status != StatusSuccess || !result.Truncated || result.Content != "01234567" || result.Data["bytes"] != int64(8) {
			t.Fatalf("single-file byte budget was not enforced: %#v", result)
		}
		if result.Data["truncation_reason"] != string(budget.FilesReadMaxBytes) {
			t.Fatalf("wrong single-file truncation reason: %#v", result.Data)
		}
	})

	t.Run("line budget", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("a\nb\nc\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedReadExecutor(t, root, 64, 2, 64)
		result := executor.Execute(context.Background(), Call{ID: "line-budget", Name: "Read", ArgumentsJSON: `{"path":"lines.txt"}`})
		if result.Status != StatusSuccess || !result.Truncated || result.Content != "a\nb\n" || result.Data["lines"] != int64(2) {
			t.Fatalf("line budget was not enforced: %#v", result)
		}
		if result.Data["truncation_reason"] != string(budget.FilesScanMaxLines) {
			t.Fatalf("wrong line truncation reason: %#v", result.Data)
		}
	})

	t.Run("exact budgets complete", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "exact.txt"), []byte("a\nb"), 0o600); err != nil {
			t.Fatal(err)
		}
		read := &ReadTool{projectRoot: root, limits: readLimits{fileBytes: 3, lines: 2, outputBytes: 3}}
		result := read.Execute(context.Background(), Input{CallID: "exact-budget", Name: "Read", Arguments: map[string]any{"path": "exact.txt"}})
		if result.Status != StatusSuccess || result.Truncated || result.Content != "a\nb" || result.Data["bytes"] != int64(3) || result.Data["lines"] != int64(2) {
			t.Fatalf("an exact-budget file was incorrectly truncated: %#v", result)
		}
	})

	t.Run("safe open error", func(t *testing.T) {
		root := t.TempDir()
		secretName := "private-credential-canary.txt"
		executor := boundedReadExecutor(t, root, 64, 10, 64)
		result := executor.Execute(context.Background(), Call{ID: "missing", Name: "Read", ArgumentsJSON: `{"path":"` + secretName + `"}`})
		visible := result.Summary + result.Content
		if result.Error != nil {
			visible += result.Error.Message
		}
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrNotFound || strings.Contains(visible, secretName) || strings.Contains(visible, root) {
			t.Fatalf("open error leaked a path or changed classification: %#v", result)
		}
	})

	t.Run("safe partial read error", func(t *testing.T) {
		reader := &faultingRead{data: []byte("bounded prefix"), err: errors.New("/sensitive/host/path: device failure")}
		outcome, err := readBounded(context.Background(), reader, readLimits{fileBytes: 64, lines: 10, outputBytes: 64})
		if err == nil || err.Error() != "read handle failed" || string(outcome.content) != "bounded prefix" || !outcome.truncated {
			t.Fatalf("underlying error was not converted to a bounded safe partial result: outcome=%#v err=%v", outcome, err)
		}
	})

	t.Run("small file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("first\nsecond"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedReadExecutor(t, root, 64, 10, 64)
		result := executor.Execute(context.Background(), Call{ID: "small", Name: "Read", ArgumentsJSON: `{"path":"small.txt"}`})
		if result.Status != StatusSuccess || result.Truncated || result.Content != "first\nsecond" || result.Data["bytes"] != int64(12) || result.Data["lines"] != int64(2) {
			t.Fatalf("small bounded Read changed behavior: %#v", result)
		}
	})
}

type faultingRead struct {
	data []byte
	err  error
}

func (r *faultingRead) Read(destination []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err == nil {
			return 0, io.EOF
		}
		err := r.err
		r.err = nil
		return 0, err
	}
	count := copy(destination, r.data)
	r.data = r.data[count:]
	return count, nil
}

func boundedReadExecutor(t *testing.T, root string, fileBytes, lines int64, outputBytes int) *Executor {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, root, time.Second, outputBytes)
	executor.ReadMaxBytes = fileBytes
	executor.ReadMaxLines = lines
	return executor
}
