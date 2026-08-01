package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/safefs"
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

func (r *faultingRead) Close() error { return nil }

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

func newWritableExecutor(t *testing.T, registry *Registry, root string, timeout time.Duration, outputBytes int) *Executor {
	t.Helper()
	opened := bootstrapWritableRoot(t, root)
	return NewExecutorWithWriteAccess(registry, root, timeout, outputBytes, opened.Root, opened.Capabilities.Ordinary())
}

func bootstrapWritableRoot(t *testing.T, root string) safefs.OpenResult {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, ".xagent"), 0o700); err != nil && !os.IsExist(err) {
		t.Fatal("create protected permission parent failed")
	}
	opened, err := safefs.Bootstrap(root, ProjectFilesystemPolicy())
	if err != nil {
		t.Fatal("bootstrap writable root failed")
	}
	t.Cleanup(func() {
		if err := opened.Root.Close(); err != nil {
			t.Error("close writable root failed")
		}
	})
	return opened
}

func TestWriteEditCannotMutateProtectedPermissionSlots(t *testing.T) {
	const (
		projectSlot = ".xagent/permissions.yaml"
		localSlot   = ".xagent/permissions.local.yaml"
	)

	t.Run("direct protected slots", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".xagent"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, slot := range []string{projectSlot, localSlot} {
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(slot)), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		opened := bootstrapWritableRoot(t, root)
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		for _, slot := range []string{projectSlot, localSlot} {
			assertWriteEditRejected(t, executor, slot, func(t *testing.T) {
				assertFileContent(t, filepath.Join(root, filepath.FromSlash(slot)), "old")
			})
		}
	})

	t.Run("deleted after bootstrap", func(t *testing.T) {
		root := permissionFixtureRoot(t, localSlot, "old")
		opened := bootstrapWritableRoot(t, root)
		path := filepath.Join(root, filepath.FromSlash(localSlot))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		assertWriteEditRejected(t, executor, localSlot, func(t *testing.T) { assertPathMissing(t, path) })
	})

	t.Run("replaced after bootstrap", func(t *testing.T) {
		root := permissionFixtureRoot(t, projectSlot, "old")
		opened := bootstrapWritableRoot(t, root)
		path := filepath.Join(root, filepath.FromSlash(projectSlot))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		assertWriteEditRejected(t, executor, projectSlot, func(t *testing.T) { assertFileContent(t, path, "replacement") })
	})

	t.Run("renamed over protected name", func(t *testing.T) {
		root := permissionFixtureRoot(t, localSlot, "old")
		source := filepath.Join(root, "rename-source.txt")
		if err := os.WriteFile(source, []byte("renamed"), 0o600); err != nil {
			t.Fatal(err)
		}
		opened := bootstrapWritableRoot(t, root)
		path := filepath.Join(root, filepath.FromSlash(localSlot))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(source, path); err != nil {
			t.Fatal(err)
		}
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		assertWriteEditRejected(t, executor, localSlot, func(t *testing.T) { assertFileContent(t, path, "renamed") })
	})

	t.Run("hard link alias", func(t *testing.T) {
		root := permissionFixtureRoot(t, projectSlot, "old")
		opened := bootstrapWritableRoot(t, root)
		protectedPath := filepath.Join(root, filepath.FromSlash(projectSlot))
		aliasPath := filepath.Join(root, "permission-hardlink.yaml")
		if err := os.Link(protectedPath, aliasPath); err != nil {
			t.Fatal(err)
		}
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		assertWriteEditRejected(t, executor, "permission-hardlink.yaml", func(t *testing.T) {
			assertFileContent(t, protectedPath, "old")
			assertFileContent(t, aliasPath, "old")
		})
	})

	t.Run("symbolic link alias", func(t *testing.T) {
		root := permissionFixtureRoot(t, localSlot, "old")
		opened := bootstrapWritableRoot(t, root)
		protectedPath := filepath.Join(root, filepath.FromSlash(localSlot))
		aliasPath := filepath.Join(root, "permission-symlink.yaml")
		if err := os.Symlink(filepath.FromSlash(localSlot), aliasPath); err != nil {
			t.Fatal(err)
		}
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		assertWriteEditRejected(t, executor, "permission-symlink.yaml", func(t *testing.T) {
			assertFileContent(t, protectedPath, "old")
		})
	})

	t.Run("protected slot absent at bootstrap", func(t *testing.T) {
		root := t.TempDir()
		opened := bootstrapWritableRoot(t, root)
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		for _, slot := range []string{projectSlot, localSlot} {
			path := filepath.Join(root, filepath.FromSlash(slot))
			assertWriteEditRejected(t, executor, slot, func(t *testing.T) { assertPathMissing(t, path) })
		}
	})

	t.Run("zero capability", func(t *testing.T) {
		root := permissionFixtureRoot(t, "ordinary.txt", "old")
		opened := bootstrapWritableRoot(t, root)
		executor := executorForOpenedRoot(t, root, opened, safefs.Capability{})
		path := filepath.Join(root, "ordinary.txt")
		assertWriteEditRejected(t, executor, "ordinary.txt", func(t *testing.T) { assertFileContent(t, path, "old") })
	})

	t.Run("capability not injected", func(t *testing.T) {
		root := permissionFixtureRoot(t, "ordinary.txt", "old")
		registry, err := NewRegistry(root)
		if err != nil {
			t.Fatal(err)
		}
		executor := NewExecutor(registry, root, time.Second, 4096)
		t.Cleanup(func() {
			if executor.root != nil {
				_ = executor.root.Close()
			}
		})
		path := filepath.Join(root, "ordinary.txt")
		assertWriteEditRejected(t, executor, "ordinary.txt", func(t *testing.T) { assertFileContent(t, path, "old") })
	})

	t.Run("cross root capability", func(t *testing.T) {
		firstRoot := t.TempDir()
		first := bootstrapWritableRoot(t, firstRoot)
		secondRoot := permissionFixtureRoot(t, "ordinary.txt", "old")
		second := bootstrapWritableRoot(t, secondRoot)
		executor := executorForOpenedRoot(t, secondRoot, second, first.Capabilities.Ordinary())
		path := filepath.Join(secondRoot, "ordinary.txt")
		assertWriteEditRejected(t, executor, "ordinary.txt", func(t *testing.T) { assertFileContent(t, path, "old") })
	})

	t.Run("ordinary target", func(t *testing.T) {
		root := t.TempDir()
		opened := bootstrapWritableRoot(t, root)
		executor := executorForOpenedRoot(t, root, opened, opened.Capabilities.Ordinary())
		write := executor.Execute(context.Background(), Call{ID: "ordinary-write", Name: "Write", ArgumentsJSON: `{"path":"ordinary.txt","content":"old"}`})
		if write.Status != StatusSuccess {
			t.Fatalf("ordinary Write failed: %#v", write)
		}
		edit := executor.Execute(context.Background(), Call{ID: "ordinary-edit", Name: "Edit", ArgumentsJSON: `{"path":"ordinary.txt","old_text":"old","new_text":"new"}`})
		if edit.Status != StatusSuccess {
			t.Fatalf("ordinary Edit failed: %#v", edit)
		}
		assertFileContent(t, filepath.Join(root, "ordinary.txt"), "new")
	})
}

func executorForOpenedRoot(t *testing.T, root string, opened safefs.OpenResult, capability safefs.Capability) *Executor {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutorWithWriteAccess(registry, root, time.Second, 4096, opened.Root, capability)
}

func permissionFixtureRoot(t *testing.T, relative, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".xagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertWriteEditRejected(t *testing.T, executor *Executor, relative string, assertUnchanged func(*testing.T)) {
	t.Helper()
	calls := []Call{
		{ID: "protected-write", Name: "Write", ArgumentsJSON: fmt.Sprintf(`{"path":%q,"content":"attacker"}`, relative)},
		{ID: "protected-edit", Name: "Edit", ArgumentsJSON: fmt.Sprintf(`{"path":%q,"old_text":"old","new_text":"attacker"}`, relative)},
	}
	for _, call := range calls {
		result := executor.Execute(context.Background(), call)
		if result.Status == StatusSuccess {
			t.Fatalf("%s unexpectedly mutated %q: %#v", call.Name, relative, result)
		}
		assertUnchanged(t)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file content = %q, want %q", data, want)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path was created or changed, stat error: %v", err)
	}
}

func TestGrepUsesSharedBudgetAndReportsScanErrors(t *testing.T) {
	t.Run("byte budget is shared across files", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"one.txt", "two.txt"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("hit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		executor := boundedGrepExecutor(t, root, grepLimits{bytes: 6, files: 10, directories: 10, lines: 10, outputBytes: 4096})
		result := executor.Execute(context.Background(), Call{ID: "shared-bytes", Name: "Grep", ArgumentsJSON: `{"pattern":"hit","path":"."}`})
		if result.Status != StatusSuccess || !result.Truncated || result.Data["bytes"] != int64(6) || result.Data["files"] != int64(2) || result.Data["count"] != 1 {
			t.Fatalf("Grep did not share the byte budget across files: %#v", result)
		}
		if result.Data["truncation_reason"] != string(budget.FilesScanMaxBytes) {
			t.Fatalf("wrong shared-byte truncation reason: %#v", result.Data)
		}
	})

	t.Run("file and directory budgets", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "root.txt"), []byte("hit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "child", "child.txt"), []byte("hit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedGrepExecutor(t, root, grepLimits{bytes: 64, files: 10, directories: 1, lines: 10, outputBytes: 4096})
		result := executor.Execute(context.Background(), Call{ID: "directory-budget", Name: "Grep", ArgumentsJSON: `{"pattern":"hit","path":"."}`})
		if !result.Truncated || result.Data["directories"] != int64(1) || result.Data["truncation_reason"] != string(budget.FilesScanMaxDirectories) {
			t.Fatalf("Grep did not enforce the shared directory budget: %#v", result)
		}

		flat := t.TempDir()
		for _, name := range []string{"a.txt", "b.txt"} {
			if err := os.WriteFile(filepath.Join(flat, name), []byte("hit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		executor = boundedGrepExecutor(t, flat, grepLimits{bytes: 64, files: 1, directories: 10, lines: 10, outputBytes: 4096})
		result = executor.Execute(context.Background(), Call{ID: "file-budget", Name: "Grep", ArgumentsJSON: `{"pattern":"hit","path":"."}`})
		if !result.Truncated || result.Data["files"] != int64(1) || result.Data["truncation_reason"] != string(budget.FilesScanMaxFiles) {
			t.Fatalf("Grep did not enforce the shared file budget: %#v", result)
		}
	})

	t.Run("line budget and long line", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("hit\nhit\nhit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := boundedGrepExecutor(t, root, grepLimits{bytes: 64, files: 10, directories: 10, lines: 2, outputBytes: 4096})
		result := executor.Execute(context.Background(), Call{ID: "line-budget", Name: "Grep", ArgumentsJSON: `{"pattern":"hit","path":"lines.txt"}`})
		if result.Status != StatusSuccess || !result.Truncated || result.Data["lines"] != int64(2) || result.Data["count"] != 2 || result.Data["truncation_reason"] != string(budget.FilesScanMaxLines) {
			t.Fatalf("Grep did not enforce the shared line budget: %#v", result)
		}

		longRoot := t.TempDir()
		content := strings.Repeat("x", 8192) + "\nhit\n"
		if err := os.WriteFile(filepath.Join(longRoot, "long.txt"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		executor = boundedGrepExecutor(t, longRoot, grepLimits{bytes: int64(len(content)), files: 10, directories: 10, lines: 10, outputBytes: 128})
		result = executor.Execute(context.Background(), Call{ID: "long-line", Name: "Grep", ArgumentsJSON: `{"pattern":"hit","path":"long.txt"}`})
		errorsValue, _ := result.Data["scan_errors"].([]string)
		if result.Status != StatusSuccess || result.Data["count"] != 1 || !containsString(errorsValue, "long.txt:1:line_too_long") || len(result.Content) > 128 {
			t.Fatalf("Grep did not safely report and continue after a long line: %#v", result)
		}
	})

	t.Run("partial read error is safe", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "fault.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		tool := &GrepTool{
			projectRoot: root,
			limits:      grepLimits{bytes: 64, files: 10, directories: 10, lines: 10, outputBytes: 4096},
			openFile: func(context.Context, *safefs.Root, string) (io.ReadCloser, error) {
				return &faultingRead{data: []byte("hit\n"), err: errors.New("/sensitive/raw/device/path")}, nil
			},
		}
		result := tool.Execute(context.Background(), Input{CallID: "fault", Name: "Grep", Arguments: map[string]any{"pattern": "hit", "path": "fault.txt"}})
		errorsValue, _ := result.Data["scan_errors"].([]string)
		visible := result.Summary + result.Content + strings.Join(errorsValue, " ")
		if result.Status != StatusSuccess || result.Data["count"] != 1 || !containsString(errorsValue, "fault.txt:read_failed") || strings.Contains(visible, "/sensitive/") {
			t.Fatalf("Grep did not return a safe bounded partial result: %#v", result)
		}
	})

	t.Run("canceled before traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := NewGrepTool(t.TempDir()).Execute(ctx, Input{CallID: "canceled", Name: "Grep", Arguments: map[string]any{"pattern": "hit"}})
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrTimeout || result.Content != "" {
			t.Fatalf("canceled Grep returned an unsafe result: %#v", result)
		}
	})

	t.Run("canceled while opening a file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "cancel.txt"), []byte("hit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		tool := &GrepTool{
			projectRoot: root,
			limits:      grepLimits{bytes: 64, files: 10, directories: 10, lines: 10, outputBytes: 4096},
			openFile: func(context.Context, *safefs.Root, string) (io.ReadCloser, error) {
				cancel()
				return nil, errors.New("open interrupted")
			},
		}
		result := tool.Execute(ctx, Input{CallID: "open-canceled", Name: "Grep", Arguments: map[string]any{"pattern": "hit", "path": "cancel.txt"}})
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrTimeout || result.Content != "" {
			t.Fatalf("Grep did not propagate cancellation from file open: %#v", result)
		}
	})
}

func boundedGrepExecutor(t *testing.T, root string, limits grepLimits) *Executor {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, root, time.Second, int(limits.outputBytes))
	executor.ScanMaxBytes = limits.bytes
	executor.ScanMaxFiles = limits.files
	executor.ScanMaxDirs = limits.directories
	executor.ScanMaxLines = limits.lines
	return executor
}

func TestGlobIsBoundedCancelableAndSymlinkSafe(t *testing.T) {
	t.Run("results are sorted", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"z.txt", "a.txt", "ignored.go"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		result := boundedGlobExecutor(t, root, globLimits{bytes: 256, files: 10, directories: 10, results: 10, outputBytes: 4096}).Execute(
			context.Background(), Call{ID: "sorted", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`},
		)
		if result.Status != StatusSuccess || result.Truncated || result.Content != "a.txt\nz.txt" || result.Data["count"] != 2 {
			t.Fatalf("Glob results were not deterministic: %#v", result)
		}
	})

	t.Run("result cap distinguishes cap and cap plus one", func(t *testing.T) {
		root := t.TempDir()
		for index := 0; index <= maxGlobResults; index++ {
			name := fmt.Sprintf("%03d.txt", index)
			if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		limits := globLimits{bytes: 4096, files: maxGlobResults + 1, directories: 10, results: maxGlobResults + 1, outputBytes: 4096}
		executor := boundedGlobExecutor(t, root, limits)
		result := executor.Execute(context.Background(), Call{ID: "cap-plus-one", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if !result.Truncated || result.Data["count"] != maxGlobResults || result.Data["truncation_reason"] != "glob.max_results" {
			t.Fatalf("Glob did not stop at cap+1: %#v", result)
		}

		if err := os.Remove(filepath.Join(root, fmt.Sprintf("%03d.txt", maxGlobResults))); err != nil {
			t.Fatal(err)
		}
		executor = boundedGlobExecutor(t, root, limits)
		result = executor.Execute(context.Background(), Call{ID: "exact-cap", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if result.Truncated || result.Data["count"] != maxGlobResults {
			t.Fatalf("Glob marked the exact result cap as truncated: %#v", result)
		}
	})

	t.Run("file budget is shared across roots", func(t *testing.T) {
		project := t.TempDir()
		extra := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, "project.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extra, "extra.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		scope, err := NewReadScope(project, []string{extra})
		if err != nil {
			t.Fatal(err)
		}
		executor := boundedGlobExecutor(t, project, globLimits{bytes: 256, files: 1, directories: 10, results: 10, outputBytes: 4096})
		result := executor.Execute(WithReadScope(context.Background(), scope), Call{ID: "shared-files", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if result.Status != StatusSuccess || !result.Truncated || result.Data["scanned_files"] != int64(1) || result.Data["count"] != 1 || result.Data["truncation_reason"] != string(budget.FilesScanMaxFiles) {
			t.Fatalf("Glob did not share the file budget across roots: %#v", result)
		}
	})

	t.Run("directory result and byte budgets", func(t *testing.T) {
		directoryRoot := t.TempDir()
		if err := os.Mkdir(filepath.Join(directoryRoot, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		executor := boundedGlobExecutor(t, directoryRoot, globLimits{bytes: 256, files: 10, directories: 1, results: 10, outputBytes: 4096})
		result := executor.Execute(context.Background(), Call{ID: "directory-budget", Name: "Glob", ArgumentsJSON: `{"pattern":"*/*.txt"}`})
		if !result.Truncated || result.Data["directories"] != int64(1) || result.Data["truncation_reason"] != string(budget.FilesScanMaxDirectories) {
			t.Fatalf("Glob did not enforce its directory budget: %#v", result)
		}

		resultRoot := t.TempDir()
		for _, name := range []string{"a.txt", "b.txt"} {
			if err := os.WriteFile(filepath.Join(resultRoot, name), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		executor = boundedGlobExecutor(t, resultRoot, globLimits{bytes: 256, files: 10, directories: 10, results: 1, outputBytes: 4096})
		result = executor.Execute(context.Background(), Call{ID: "result-budget", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if !result.Truncated || result.Data["results"] != int64(1) || result.Data["count"] != 1 || result.Data["truncation_reason"] != string(budget.FilesScanMaxLines) {
			t.Fatalf("Glob did not enforce its result budget: %#v", result)
		}

		byteRoot := t.TempDir()
		name := "long-name.txt"
		if err := os.WriteFile(filepath.Join(byteRoot, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		byteLimit := int64(len(name) - 1)
		executor = boundedGlobExecutor(t, byteRoot, globLimits{bytes: byteLimit, files: 10, directories: 10, results: 10, outputBytes: 4096})
		result = executor.Execute(context.Background(), Call{ID: "byte-budget", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if !result.Truncated || result.Data["bytes"] != byteLimit || result.Data["count"] != 0 || result.Data["truncation_reason"] != string(budget.FilesScanMaxBytes) {
			t.Fatalf("Glob did not enforce its cumulative byte budget: %#v", result)
		}

		outputRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(outputRoot, "output.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor = boundedGlobExecutor(t, outputRoot, globLimits{bytes: 256, files: 10, directories: 10, results: 10, outputBytes: 5})
		result = executor.Execute(context.Background(), Call{ID: "output-budget", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if !result.Truncated || result.Content != "" || result.Data["count"] != 0 || result.Data["truncation_reason"] != string(budget.ToolInlineOutputBytes) {
			t.Fatalf("Glob did not enforce its inline output budget while scanning: %#v", result)
		}
	})

	t.Run("cancellation stops traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := NewGlobTool(t.TempDir()).Execute(ctx, Input{CallID: "canceled", Name: "Glob", Arguments: map[string]any{"pattern": "*.txt"}})
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrTimeout || result.Content != "" {
			t.Fatalf("canceled Glob returned an unsafe result: %#v", result)
		}
	})

	t.Run("symlink escape is rejected", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "secret-link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "outside-dir")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		executor := boundedGlobExecutor(t, root, globLimits{bytes: 256, files: 10, directories: 10, results: 10, outputBytes: 4096})
		result := executor.Execute(context.Background(), Call{ID: "symlink-file", Name: "Glob", ArgumentsJSON: `{"pattern":"*.txt"}`})
		if result.Status != StatusSuccess || result.Content != "inside.txt" || strings.Contains(result.Content, "secret") {
			t.Fatalf("Glob exposed an escaping file symlink: %#v", result)
		}
		result = executor.Execute(context.Background(), Call{ID: "symlink-directory", Name: "Glob", ArgumentsJSON: `{"pattern":"outside-dir/*.txt"}`})
		if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrPathOutsideProject || strings.Contains(result.Content, "secret") {
			t.Fatalf("Glob followed an escaping directory symlink: %#v", result)
		}
	})
}

func boundedGlobExecutor(t *testing.T, root string, limits globLimits) *Executor {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, root, time.Second, int(limits.outputBytes))
	executor.ScanMaxBytes = limits.bytes
	executor.ScanMaxFiles = limits.files
	executor.ScanMaxDirs = limits.directories
	executor.ScanMaxLines = limits.results
	return executor
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
