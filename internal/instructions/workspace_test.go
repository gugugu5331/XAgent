package instructions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/safefs"
)

func TestWorkspaceLoaderRejectsRelativeProjectRoot(t *testing.T) {
	if _, err := NewWorkspaceLoader("relative-project", "", testConfig()); err == nil {
		t.Fatal("relative project root was resolved through process cwd")
	}
}

func TestWorkspaceLoaderFreezesCanonicalRootAndIgnoresFieldMutation(t *testing.T) {
	parent := t.TempDir()
	projectRoot := filepath.Join(parent, "Project")
	otherRoot := filepath.Join(parent, "other")
	for _, root := range []string{projectRoot, otherRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeInstructionFile(t, filepath.Join(projectRoot, "MEWCODE.md"), "frozen project")
	writeInstructionFile(t, filepath.Join(otherRoot, "MEWCODE.md"), "mutated project")
	alias := filepath.Join(parent, "project-link")
	if err := os.Symlink(projectRoot, alias); err != nil {
		t.Fatal(err)
	}

	loader, err := NewWorkspaceLoader(alias, "", testConfig())
	if err != nil {
		t.Fatalf("NewWorkspaceLoader: %v", err)
	}
	loader.ProjectRoot = otherRoot
	loader.UserDir = otherRoot
	sources, items := loader.LoadSources(context.Background())
	if len(items) != 0 || len(sources) != 1 || sources[0].Content != "frozen project" {
		t.Fatalf("LoadSources = %#v diagnostics=%#v", sources, items)
	}
	if !filepath.IsAbs(sources[0].Path) || strings.HasPrefix(sources[0].Path, alias+string(filepath.Separator)) {
		t.Fatalf("source path is not canonical absolute: %q", sources[0].Path)
	}
}

func TestWorkspaceLoaderFailsClosedWhenFrozenRootIsReplaced(t *testing.T) {
	parent := t.TempDir()
	projectRoot := filepath.Join(parent, "project")
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, filepath.Join(projectRoot, "MEWCODE.md"), "same content")
	loader, err := NewWorkspaceLoader(projectRoot, "", testConfig())
	if err != nil {
		t.Fatalf("NewWorkspaceLoader: %v", err)
	}
	detached := filepath.Join(parent, "detached")
	if err := os.Rename(projectRoot, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, filepath.Join(projectRoot, "MEWCODE.md"), "same content")

	sources, items := loader.LoadSources(context.Background())
	if len(sources) != 0 {
		t.Fatalf("replaced root content was loaded: %#v", sources)
	}
	assertInstructionDiagnostic(t, items, "instructions_root_changed")
	for _, item := range items {
		if strings.Contains(item.Message, projectRoot) || strings.Contains(item.Path, projectRoot) {
			t.Fatalf("public diagnostic leaked root path: %#v", item)
		}
	}
}

func TestWorkspaceLoaderFailsClosedWhenRootAncestorBecomesSymlink(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, "container")
	projectRoot := filepath.Join(container, "project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, filepath.Join(projectRoot, "MEWCODE.md"), "same object")
	loader, err := NewWorkspaceLoader(projectRoot, "", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(parent, "detached")
	if err := os.Rename(container, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(detached, container); err != nil {
		t.Fatal(err)
	}
	sources, items := loader.LoadSources(context.Background())
	if len(sources) != 0 {
		t.Fatalf("symlink-rebound ancestor was accepted: %#v", sources)
	}
	assertInstructionDiagnostic(t, items, "instructions_root_changed")
}

func TestWorkspaceLoaderUsesOnlyExplicitFrozenUserRoot(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	writeInstructionFile(t, filepath.Join(userRoot, "MEWCODE.md"), "user instruction")
	cfg := testConfig()
	cfg.UserDir = userRoot

	withoutUser, err := NewWorkspaceLoader(projectRoot, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sources, items := withoutUser.LoadSources(context.Background())
	if len(items) != 0 || len(sources) != 0 {
		t.Fatalf("implicit user root was loaded: %#v diagnostics=%#v", sources, items)
	}
	withUser, err := NewWorkspaceLoader(projectRoot, userRoot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sources, items = withUser.LoadSources(context.Background())
	if len(items) != 0 || len(sources) != 1 || sources[0].Scope != ScopeUserDir || sources[0].Content != "user instruction" {
		t.Fatalf("explicit user root was not independently loaded: %#v diagnostics=%#v", sources, items)
	}
}

func TestWorkspaceLoaderFreezesNormalizedConfig(t *testing.T) {
	t.Run("project file mutation", func(t *testing.T) {
		root := t.TempDir()
		writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "frozen config")
		writeInstructionFile(t, filepath.Join(root, "OTHER.md"), "mutated config")
		loader, err := NewWorkspaceLoader(root, "", testConfig())
		if err != nil {
			t.Fatal(err)
		}
		loader.Config.ProjectFile = "OTHER.md"
		sources, items := loader.LoadSources(context.Background())
		if len(items) != 0 || len(sources) != 1 || sources[0].Content != "frozen config" {
			t.Fatalf("post-construction config mutation changed source: %#v diagnostics=%#v", sources, items)
		}
	})

	t.Run("budget mutation", func(t *testing.T) {
		root := t.TempDir()
		writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "content exceeds frozen budget")
		cfg := testConfig()
		cfg.MaxFileBytes = 4
		loader, err := NewWorkspaceLoader(root, "", cfg)
		if err != nil {
			t.Fatal(err)
		}
		loader.Config.MaxFileBytes = 1 << 20
		sources, items := loader.LoadSources(context.Background())
		if len(sources) != 0 {
			t.Fatalf("expanded post-construction budget loaded content: %#v", sources)
		}
		assertInstructionDiagnostic(t, items, "instructions_file_too_large")
	})
}

func TestWorkspaceCacheKeyBindsCanonicalAbsolutePathAndLiveIdentity(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	childRoot := filepath.Join(parent, "child")
	for _, root := range []string{mainRoot, childRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	}
	mainKey := instructionWorkspaceKey(t, mainRoot, ScopeProjectRoot)
	childKey := instructionWorkspaceKey(t, childRoot, ScopeProjectRoot)
	if mainKey == childKey {
		t.Fatal("different worktrees shared an instruction expansion key")
	}

	path := filepath.Join(mainRoot, "MEWCODE.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, path, "same content")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	replacedKey := instructionWorkspaceKey(t, mainRoot, ScopeProjectRoot)
	if mainKey == replacedKey {
		t.Fatal("same-content same-mtime inode replacement reused an instruction expansion key")
	}
}

func TestWorkspaceCacheScopeCannotPromoteUserExpansion(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	userKey := instructionWorkspaceKey(t, root, ScopeUserDir)
	projectKey := instructionWorkspaceKey(t, root, ScopeProjectRoot)
	if userKey == projectKey {
		t.Fatal("user instruction expansion was reusable at project scope")
	}
}

func TestWorkspaceCacheKeyBindsIncludeIdentityAndCanonicalAliases(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "WorkspaceCase")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "root\n@include include.md")
	includePath := filepath.Join(root, "include.md")
	writeInstructionFile(t, includePath, "same include")
	key := instructionWorkspaceIncludeKey(t, root)
	alias := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if aliasKey := instructionWorkspaceIncludeKey(t, alias); aliasKey != key {
		t.Fatal("equivalent symlink alias did not share the canonical workspace key")
	}
	caseAlias := filepath.Join(parent, strings.ToLower(filepath.Base(root)))
	if _, err := os.Stat(caseAlias); err == nil {
		if caseKey := instructionWorkspaceIncludeKey(t, caseAlias); caseKey != key {
			t.Fatal("equivalent case alias did not share the canonical workspace key")
		}
	}
	info, err := os.Stat(includePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(includePath); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, includePath, "same include")
	if err := os.Chtimes(includePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if replaced := instructionWorkspaceIncludeKey(t, root); replaced == key {
		t.Fatal("same-content same-mtime include replacement reused the expansion key")
	}
}

func TestWorkspaceLoaderOwnsAndUsesTaskLocalExpansionCache(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	for _, root := range []string{firstRoot, secondRoot} {
		writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "root\n@include include.md")
		writeInstructionFile(t, filepath.Join(root, "include.md"), "same include")
	}
	first, err := NewWorkspaceLoader(firstRoot, "", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWorkspaceLoader(secondRoot, "", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if first.workspace.cache == second.workspace.cache {
		t.Fatal("different workspace loaders shared a task-local expansion cache owner")
	}
	sections, items := first.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || !strings.Contains(sections[0].Content, "same include") {
		t.Fatalf("first load = %#v diagnostics=%#v", sections, items)
	}
	if got := len(first.workspace.cache.entries); got != 1 {
		t.Fatalf("task-local cache entries after first load = %d, want 1", got)
	}
	sections, items = first.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || len(first.workspace.cache.entries) != 1 {
		t.Fatalf("cached reload = %#v diagnostics=%#v entries=%d", sections, items, len(first.workspace.cache.entries))
	}
	if len(second.workspace.cache.entries) != 0 {
		t.Fatal("first workspace populated the second workspace cache")
	}
	writeInstructionFile(t, filepath.Join(firstRoot, "include.md"), "changed include")
	sections, items = first.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || !strings.Contains(sections[0].Content, "changed include") {
		t.Fatalf("changed dependency reused stale expansion: %#v diagnostics=%#v", sections, items)
	}
	if got := len(first.workspace.cache.entries); got != 2 {
		t.Fatalf("changed graph cache entries = %d, want distinct version", got)
	}
}

func TestWorkspaceExpansionCacheRejectsUnboundAndOversizedEntries(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	key, source, files := instructionWorkspaceGraph(t, root, ScopeProjectRoot)
	cache := &ExpansionCache{}
	if err := cache.Store(key, CachedExpansion{Content: "same content"}); err == nil {
		t.Fatal("cache accepted expansion without its bound graph")
	}
	if err := cache.Store(CacheKey{digest: [32]byte{1}}, CachedExpansion{
		Source: source, Content: "same content", Files: files,
	}); err == nil {
		t.Fatal("cache accepted a key that did not match its bound graph")
	}
	if err := cache.Store(key, CachedExpansion{
		Source: source, Content: strings.Repeat("x", maxCachedExpansionBytes+1), Files: files,
	}); err == nil {
		t.Fatal("cache accepted oversized expanded content")
	}
}

func TestWorkspaceCacheKeyDoesNotExposeCanonicalPath(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	key := instructionWorkspaceKey(t, root, ScopeProjectRoot)
	encoded, err := key.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || len(encoded) != 33 {
		t.Fatalf("opaque cache key leaked path material: %q", encoded)
	}
}

func TestWorkspaceCacheKeyRejectsMismatchedRootPathAndScopePriority(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	key, source, files := instructionWorkspaceGraph(t, root, ScopeProjectRoot)
	mismatched := source
	mismatched.RootPath = filepath.Join(root, "other.md")
	mismatchKey, err := NewCacheKey(mismatched, files, nil)
	if err != nil || mismatchKey == key {
		t.Fatalf("cache graph did not bind the alternate absolute namespace: key=%#v err=%v", mismatchKey, err)
	}
	wrongScope := source
	wrongScope.Scope = ScopeUserDir
	if _, err := NewCacheKey(wrongScope, files, nil); err == nil {
		t.Fatal("cache graph accepted project priority at user scope")
	}
}

func TestWorkspaceInstructionPathsAreBounded(t *testing.T) {
	if _, err := canonicalInstructionRelative(strings.Repeat("a", maxGraphPathBytes+1)); err == nil {
		t.Fatal("oversized instruction relative path was accepted")
	}
}

func instructionWorkspaceKey(t *testing.T, root string, scope Scope) CacheKey {
	t.Helper()
	key, _, _ := instructionWorkspaceGraph(t, root, scope)
	return key
}

func instructionWorkspaceGraph(t *testing.T, root string, scope Scope) (CacheKey, GraphSource, []FileVersion) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := safefs.Bootstrap(canonical, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	binding, err := opened.Root.Bind("MEWCODE.md")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := FileIdentityFromBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	version, err := NewFileVersion(filepath.Join(canonical, "MEWCODE.md"), identity, []byte("same content"))
	if err != nil {
		t.Fatal(err)
	}
	priority := PriorityProjectRoot
	if scope == ScopeUserDir {
		priority = PriorityUserDir
	}
	source := GraphSource{
		Name: "instruction", Scope: scope, Priority: priority,
		RootPath: filepath.Join(canonical, "MEWCODE.md"), Root: identity,
	}
	key, err := NewCacheKey(source, []FileVersion{version}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return key, source, []FileVersion{version}
}

func instructionWorkspaceIncludeKey(t *testing.T, root string) CacheKey {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err = canonicalInstructionExistingPath(canonical)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := safefs.Bootstrap(canonical, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	identities := make([]FileIdentity, 0, 2)
	versions := make([]FileVersion, 0, 2)
	for _, name := range []string{"MEWCODE.md", "include.md"} {
		binding, err := opened.Root.Bind(name)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := FileIdentityFromBinding(binding)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(canonical, name))
		if err != nil {
			t.Fatal(err)
		}
		version, err := NewFileVersion(filepath.Join(canonical, name), identity, content)
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
		versions = append(versions, version)
	}
	source := GraphSource{
		Name: "instruction", Scope: ScopeProjectRoot, Priority: PriorityProjectRoot,
		RootPath: filepath.Join(canonical, "MEWCODE.md"), Root: identities[0],
	}
	key, err := NewCacheKey(source, versions, []IncludeEdge{{From: identities[0], To: identities[1], Position: 0}})
	if err != nil {
		t.Fatal(err)
	}
	return key
}
