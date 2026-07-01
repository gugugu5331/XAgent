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

func TestRelativeToRootUsesResolvedRootSymlink(t *testing.T) {
	realRoot := t.TempDir()
	linkParent := t.TempDir()
	linkRoot := filepath.Join(linkParent, "root-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	file := filepath.Join(realRoot, "file.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveProjectPath(linkRoot, "file.txt")
	if err != nil {
		t.Fatalf("resolve via root symlink failed: %v", err)
	}
	realFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != realFile {
		t.Fatalf("expected resolved real file %q, got %q", realFile, resolved)
	}
	if rel := RelativeToRoot(linkRoot, resolved); rel != "file.txt" {
		t.Fatalf("expected relative path through resolved root, got %q", rel)
	}
}

func TestSymlinkEscapesAreRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(insideFile, []byte("inside content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "secret-link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-dir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)

	read := executor.Execute(context.Background(), Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"secret-link.txt"}`})
	if read.Status != StatusError || read.Error.Code != ErrPathOutsideProject {
		t.Fatalf("expected read to reject outside symlink, got %#v", read)
	}

	edit := executor.Execute(context.Background(), Call{ID: "edit", Name: "Edit", ArgumentsJSON: `{"path":"secret-link.txt","old_text":"outside","new_text":"changed"}`})
	if edit.Status != StatusError || edit.Error.Code != ErrPathOutsideProject {
		t.Fatalf("expected edit to reject outside symlink, got %#v", edit)
	}
	outsideData, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideData) != "outside secret" {
		t.Fatalf("outside file was modified through symlink: %q", outsideData)
	}

	grep := executor.Execute(context.Background(), Call{ID: "grep", Name: "Grep", ArgumentsJSON: `{"pattern":"outside secret","path":"."}`})
	if grep.Status != StatusError || grep.Error.Code != ErrNoResults || strings.Contains(grep.Content, "outside secret") {
		t.Fatalf("expected grep to skip outside symlink content, got %#v", grep)
	}

	write := executor.Execute(context.Background(), Call{ID: "write", Name: "Write", ArgumentsJSON: `{"path":"outside-dir/new/file.txt","content":"escaped"}`})
	if write.Status != StatusError || write.Error.Code != ErrPathOutsideProject {
		t.Fatalf("expected write to reject outside symlink directory, got %#v", write)
	}
	if _, err := os.Stat(filepath.Join(outside, "new", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("write created file outside project, stat err: %v", err)
	}

	glob := executor.Execute(context.Background(), Call{ID: "glob", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
	if glob.Status != StatusSuccess {
		t.Fatalf("glob failed: %#v", glob)
	}
	if strings.Contains(glob.Content, "secret-link.txt") || strings.Contains(glob.Content, "secret.txt") {
		t.Fatalf("glob returned escaping symlink path: %#v", glob)
	}
	if !strings.Contains(glob.Content, "inside.txt") {
		t.Fatalf("glob should still return safe in-project file: %#v", glob)
	}
}

func TestReadOnlyRegistryContainsOnlyReadTools(t *testing.T) {
	registry, err := NewReadOnlyRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Read", "Glob", "Grep"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected read-only registry to contain %s", name)
		}
	}
	for _, name := range []string{"Write", "Edit", "Bash"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("read-only registry should not contain %s", name)
		}
	}
	if len(registry.AnthropicDefinitions()) != 3 || len(registry.OpenAIDefinitions()) != 3 {
		t.Fatalf("expected three read-only tool definitions")
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

func TestToolDescriptionsReinforcePromptRules(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][]string{
		"Read":  {"dedicated tool", "editing", "project"},
		"Write": {"Read", "project"},
		"Edit":  {"Read", "project"},
		"Bash":  {"Prefer dedicated tools", "not sandboxed", "cautiously"},
		"Glob":  {"dedicated tool", "project"},
		"Grep":  {"dedicated tool", "editing", "project"},
	}
	for name, wants := range checks {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		description := tool.Description()
		for _, want := range wants {
			if !strings.Contains(description, want) {
				t.Fatalf("%s description missing %q: %s", name, want, description)
			}
		}
		for _, forbidden := range []string{"bypass confirmation", "skip confirmation", "destructive"} {
			if strings.Contains(description, forbidden) {
				t.Fatalf("%s description contains unsafe phrase %q: %s", name, forbidden, description)
			}
		}
	}
}
