package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveProjectPathRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	_, err := ResolveProjectPath(root, "../outside.txt")
	if err == nil || !strings.Contains(err.Error(), ErrPathOutsideProject) {
		t.Fatalf("expected outside project error, got %v", err)
	}
}

func TestRegistryRejectsDuplicateTool(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewReadTool(root)); err == nil {
		t.Fatal("expected duplicate tool registration to fail")
	}
}

func TestReadAndWriteTools(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)

	write := executor.Execute(context.Background(), Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"hello"}`})
	if write.Status != StatusSuccess {
		t.Fatalf("write failed: %#v", write)
	}

	read := executor.Execute(context.Background(), Call{ID: "2", Name: "Read", ArgumentsJSON: `{"path":"a.txt"}`})
	if read.Status != StatusSuccess || read.Content != "hello" {
		t.Fatalf("read mismatch: %#v", read)
	}
}

func TestEditToolMatchCounts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one two one"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)

	missing := executor.Execute(context.Background(), Call{ID: "1", Name: "Edit", ArgumentsJSON: `{"path":"file.txt","old_text":"three","new_text":"x"}`})
	if missing.Status != StatusError || missing.Error.Code != ErrNotFound {
		t.Fatalf("expected not_found, got %#v", missing)
	}

	multiple := executor.Execute(context.Background(), Call{ID: "2", Name: "Edit", ArgumentsJSON: `{"path":"file.txt","old_text":"one","new_text":"x"}`})
	if multiple.Status != StatusError || multiple.Error.Code != ErrMultipleMatches {
		t.Fatalf("expected multiple_matches, got %#v", multiple)
	}

	unique := executor.Execute(context.Background(), Call{ID: "3", Name: "Edit", ArgumentsJSON: `{"path":"file.txt","old_text":"two","new_text":"2"}`})
	if unique.Status != StatusSuccess {
		t.Fatalf("expected success, got %#v", unique)
	}
}

func TestBashToolFailureAndTimeout(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, 100*time.Millisecond, 1024)

	failure := executor.Execute(context.Background(), Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"exit 7"}`})
	if failure.Status != StatusError || failure.Error.Code != ErrCommandFailed {
		t.Fatalf("expected command_failed, got %#v", failure)
	}

	timeout := executor.Execute(context.Background(), Call{ID: "2", Name: "Bash", ArgumentsJSON: `{"command":"sleep 1"}`})
	if timeout.Status != StatusTimeout {
		t.Fatalf("expected timeout, got %#v", timeout)
	}
}

func TestGlobAndGrep(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file.go"), []byte("package main\nfunc hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)

	glob := executor.Execute(context.Background(), Call{ID: "1", Name: "Glob", ArgumentsJSON: `{"pattern":"dir/*.go"}`})
	if glob.Status != StatusSuccess || !strings.Contains(glob.Content, "dir/file.go") {
		t.Fatalf("glob mismatch: %#v", glob)
	}

	grep := executor.Execute(context.Background(), Call{ID: "2", Name: "Grep", ArgumentsJSON: `{"pattern":"hello","path":"dir"}`})
	if grep.Status != StatusSuccess || !strings.Contains(grep.Content, "hello") {
		t.Fatalf("grep mismatch: %#v", grep)
	}

	noResults := executor.Execute(context.Background(), Call{ID: "3", Name: "Grep", ArgumentsJSON: `{"pattern":"missing","path":"dir"}`})
	if noResults.Status != StatusError || noResults.Error.Code != ErrNoResults {
		t.Fatalf("expected no_results, got %#v", noResults)
	}
}

func TestExecutorTruncatesOutput(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 10)
	result := executor.Execute(context.Background(), Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"long.txt","content":"abcdefghijklmnopqrstuvwxyz"}`})
	if result.Status != StatusSuccess {
		t.Fatalf("write failed: %#v", result)
	}
	read := executor.Execute(context.Background(), Call{ID: "2", Name: "Read", ArgumentsJSON: `{"path":"long.txt"}`})
	if !read.Truncated {
		t.Fatalf("expected truncated read, got %#v", read)
	}
}
