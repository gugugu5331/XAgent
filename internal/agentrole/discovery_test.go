package agentrole

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"xagent/internal/redact"
)

func TestDiscoverFilesLoadsOnlyStableDirectLowercaseMarkdown(t *testing.T) {
	fileSystem := fstest.MapFS{
		"agents/zeta.md":         {Data: []byte("---\nname: zeta\ndescription: Zeta role\n---\nDo zeta work.\n")},
		"agents/alpha.md":        {Data: []byte("---\nname: alpha\ndescription: Alpha role\n---\nDo alpha work.\n")},
		"agents/UPPER.MD":        {Data: []byte("ignored")},
		"agents/.hidden.md":      {Data: []byte("ignored")},
		"agents/plain.txt":       {Data: []byte("ignored")},
		"agents/nested/child.md": {Data: []byte("ignored")},
	}
	candidates, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceProject,
		ID:     "project",
		FS:     fileSystem,
		Root:   "agents",
	}, DefaultLimits(), redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatalf("DiscoverFiles() error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].Name != "alpha" || candidates[1].Name != "zeta" {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].Origin.Text() != "agents/alpha.md" || candidates[1].Origin.Text() != "agents/zeta.md" {
		t.Fatalf("origins = %q, %q", candidates[0].Origin.Text(), candidates[1].Origin.Text())
	}
}

func TestSourceDirectoryMissingIsOptionalOnlyForUserAndProject(t *testing.T) {
	for _, source := range []Source{SourceUser, SourceProject} {
		candidates, err := DiscoverFiles(context.Background(), FileSource{
			Source: source, ID: string(source), FS: fstest.MapFS{}, Root: "missing",
		}, DefaultLimits(), redact.NewRuntimeRedactor())
		if err != nil || len(candidates) != 0 {
			t.Fatalf("optional %s source = %#v, %v", source, candidates, err)
		}
	}
	if _, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceBuiltin, ID: "builtin", FS: fstest.MapFS{}, Root: "missing",
	}, DefaultLimits(), redact.NewRuntimeRedactor()); err == nil {
		t.Fatal("missing builtin root was accepted")
	}
}

func TestSymlinkEntryIsInvalidAndRootSymlinkIsRejected(t *testing.T) {
	temporary := t.TempDir()
	realRoot := filepath.Join(temporary, "agents")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(temporary, "outside.md")
	if err := os.WriteFile(target, []byte("---\nname: outside\ndescription: Outside\n---\nDo work.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(realRoot, "linked.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	candidates, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceProject, ID: "project", FS: os.DirFS(temporary), Root: "agents",
	}, DefaultLimits(), redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatalf("entry symlink returned root error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Valid || len(candidates[0].Diagnostics) == 0 {
		t.Fatalf("symlink candidate = %#v", candidates)
	}

	if err := os.Symlink(realRoot, filepath.Join(temporary, "agents-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceProject, ID: "project", FS: os.DirFS(temporary), Root: "agents-link",
	}, DefaultLimits(), redact.NewRuntimeRedactor()); err == nil {
		t.Fatal("root symlink was accepted")
	}
	if err := os.Mkdir(filepath.Join(realRoot, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, filepath.Join(temporary, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceProject, ID: "project", FS: os.DirFS(temporary), Root: "parent-link/nested",
	}, DefaultLimits(), redact.NewRuntimeRedactor()); err == nil {
		t.Fatal("intermediate root symlink was accepted")
	}
}

func TestDiscoverFilesCountsEveryDirectoryEntryBeforeFiltering(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFiles = 2
	fileSystem := fstest.MapFS{
		"agents/.one":  {Data: []byte("ignored")},
		"agents/.two":  {Data: []byte("ignored")},
		"agents/ok.md": {Data: []byte("---\nname: ok\ndescription: Okay\n---\nDo work.\n")},
	}
	if _, err := DiscoverFiles(context.Background(), FileSource{
		Source: SourceProject, ID: "project", FS: fileSystem, Root: "agents",
	}, limits, redact.NewRuntimeRedactor()); err == nil {
		t.Fatal("directory entry limit was bypassed by ignored entries")
	}
}

func TestBuiltinSourceContainsExploreAndReview(t *testing.T) {
	candidates, err := DiscoverFiles(context.Background(), BuiltinSource(), DefaultLimits(), redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatalf("builtin discovery error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].Name != "explore" || candidates[1].Name != "review" {
		t.Fatalf("builtin candidates = %#v", candidates)
	}
	for _, candidate := range candidates {
		if !candidate.Valid {
			t.Fatalf("invalid builtin candidate = %#v", candidate)
		}
	}
}
