package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/provider"
)

func TestProjectIdentityChangesWhenFilesystemObjectIsReplaced(t *testing.T) {
	root := t.TempDir()
	first, err := NewProjectIdentity(root, "config-a")
	if err != nil {
		t.Fatalf("first identity: %v", err)
	}
	moved := root + "-old"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("replace root: %v", err)
	}
	second, err := NewProjectIdentity(root, "config-a")
	if err != nil {
		t.Fatalf("second identity: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("different filesystem objects shared project identity %q", first.ID)
	}
}

func TestProjectIdentityWorkspaceManagerSeparatesRootsAndConfig(t *testing.T) {
	firstRoot := projectIdentityCanonicalTempDir(t)
	secondRoot := projectIdentityCanonicalTempDir(t)
	sharedUserDir := t.TempDir()
	newManager := func(root, config string) *Manager {
		t.Helper()
		manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
			WorktreeRoot: root,
			ConfigDigest: config,
			ManagerOptions: ManagerOptions{
				UserDir: sharedUserDir, ProjectDir: filepath.Join(root, ".xagent", "memory"),
			},
		})
		if err != nil {
			t.Fatalf("NewWorkspaceManager(%q): %v", root, err)
		}
		t.Cleanup(func() { _ = manager.Close() })
		return manager
	}
	first := newManager(firstRoot, "config-a")
	firstAgain := newManager(firstRoot, "config-b")
	second := newManager(secondRoot, "config-a")
	if first.ProjectCacheNamespace() == second.ProjectCacheNamespace() || first.ProjectCacheNamespace() == firstAgain.ProjectCacheNamespace() {
		t.Fatalf("project cache namespaces collided: first=%q second=%q config=%q", first.ProjectCacheNamespace(), second.ProjectCacheNamespace(), firstAgain.ProjectCacheNamespace())
	}
	for managerIndex, manager := range []*Manager{first, second} {
		note := NewNote(NoteProjectKnowledge, ScopeProject, "same-size", strings.Repeat(string(rune('a'+managerIndex)), 16), "test", time.Unix(1, 0))
		if err := manager.SaveNote(note); err != nil {
			t.Fatalf("save manager %d: %v", managerIndex, err)
		}
		index, err := manager.LoadIndex(ScopeProject)
		if err != nil || len(index.Entries) != 1 || index.Entries[0].Body != note.Body {
			t.Fatalf("manager %d loaded another root: index=%#v err=%v", managerIndex, index, err)
		}
	}
	if status := first.Status(); status.UserDir != "" || status.ProjectDir != "" || status.ProjectIdentity != first.ProjectCacheNamespace() || len(status.ProjectIdentity) < 32 {
		t.Fatalf("workspace status exposed path or omitted stable identity: %#v", status)
	}
}

func TestProjectIdentityWorkspaceManagerRejectsNonCanonicalBinding(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	link := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	tests := []WorkspaceManagerOptions{
		{WorktreeRoot: "relative", ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")}},
		{WorktreeRoot: " " + root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")}},
		{WorktreeRoot: root + string(filepath.Separator) + ".", ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")}},
		{WorktreeRoot: link, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(link, ".xagent", "memory")}},
		{WorktreeRoot: root, ConfigDigest: "", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")}},
		{WorktreeRoot: root, ConfigDigest: " config ", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory") + " "}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: t.TempDir()}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: root}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".git", "memory")}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".control", "memory")}},
		{WorktreeRoot: root, ConfigDigest: "config", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "worktrees", "memory")}},
	}
	for index, options := range tests {
		if _, err := NewWorkspaceManager(options); !errors.Is(err, ErrWorkspaceManagerInvalid) {
			t.Fatalf("case %d error = %v, want ErrWorkspaceManagerInvalid", index, err)
		}
	}
}

func TestProjectIdentityWorkspaceManagerRejectsCaseAlias(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	alias := swapProjectIdentityPathCase(root)
	if alias == root {
		t.Skip("temporary directory has no cased component")
	}
	rootInfo, rootErr := os.Stat(root)
	aliasInfo, aliasErr := os.Stat(alias)
	if rootErr != nil || aliasErr != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem does not resolve case aliases")
	}
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: alias, ConfigDigest: "config-a",
		ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(alias, ".xagent", "memory")},
	})
	if manager != nil {
		_ = manager.Close()
	}
	if manager != nil || !errors.Is(err, ErrWorkspaceManagerInvalid) {
		t.Fatalf("NewWorkspaceManager accepted case alias %q for canonical root %q: manager=%T err=%v", alias, root, manager, err)
	}
}

func TestProjectIdentityWorkspaceCacheRejectsReplacedIndexObject(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	firstIndex := Index{Scope: ScopeProject, Entries: []IndexEntry{{ID: "entry-a", Title: "title", Body: "body-a"}}}
	secondIndex := Index{Scope: ScopeProject, Entries: []IndexEntry{{ID: "entry-b", Title: "title", Body: "body-b"}}}
	firstText := MarshalIndex(firstIndex, 200, 25*1024)
	secondText := MarshalIndex(secondIndex, 200, 25*1024)
	if len(firstText) != len(secondText) {
		t.Fatalf("test fixture sizes differ: first=%d second=%d", len(firstText), len(secondText))
	}
	indexPath := filepath.Join(projectDir, IndexFileName)
	if err := os.WriteFile(indexPath, []byte(firstText), 0o600); err != nil {
		t.Fatalf("write first index: %v", err)
	}
	firstInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat first index: %v", err)
	}
	loaded, err := manager.LoadIndex(ScopeProject)
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].ID != "entry-a" {
		t.Fatalf("load first index: index=%#v err=%v", loaded, err)
	}
	replacement := filepath.Join(projectDir, "replacement-index")
	if err := os.WriteFile(replacement, []byte(secondText), 0o600); err != nil {
		t.Fatalf("write replacement index: %v", err)
	}
	if err := os.Chtimes(replacement, firstInfo.ModTime(), firstInfo.ModTime()); err != nil {
		t.Fatalf("preserve replacement mtime: %v", err)
	}
	if err := os.Rename(replacement, indexPath); err != nil {
		t.Fatalf("replace index: %v", err)
	}
	secondInfo, err := os.Stat(indexPath)
	if err != nil || secondInfo.Size() != firstInfo.Size() || !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatalf("replacement metadata changed: first=%#v second=%#v err=%v", firstInfo, secondInfo, err)
	}
	loaded, err = manager.LoadIndex(ScopeProject)
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].ID != "entry-b" {
		t.Fatalf("workspace cache returned replaced index object: index=%#v err=%v", loaded, err)
	}
}

func TestProjectIdentityWorkspaceManagerFailsClosedAfterRootReplacement(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	userDir := t.TempDir()
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot:   root,
		ConfigDigest:   "config-a",
		ManagerOptions: ManagerOptions{UserDir: userDir, ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	projectNote := NewNote(NoteProjectKnowledge, ScopeProject, "project", "old project", "test", time.Unix(1, 0))
	if err := manager.SaveNote(projectNote); err != nil {
		t.Fatalf("prime project cache: %v", err)
	}
	if _, err := manager.LoadIndex(ScopeProject); err != nil {
		t.Fatalf("load primed project cache: %v", err)
	}
	moved := root + "-old"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("create replacement project dir: %v", err)
	}

	newProjectNote := NewNote(NoteProjectKnowledge, ScopeProject, "replacement", "must not be written", "test", time.Unix(2, 0))
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "load", run: func() error { _, err := manager.LoadIndex(ScopeProject); return err }},
		{name: "save", run: func() error { return manager.SaveNote(newProjectNote) }},
		{name: "rebuild", run: func() error { _, err := manager.RebuildIndex(ScopeProject); return err }},
		{name: "delete", run: func() error { return manager.DeleteNote(ScopeProject, projectNote.ID) }},
		{name: "update", run: func() error {
			return manager.applyDecision(UpdateInput{Scope: ScopeProject, Now: time.Unix(3, 0)}, UpdateDecision{
				Action: "add", Type: NoteProjectKnowledge, Title: "update", Body: "must not be updated",
			})
		}},
	}
	for _, operation := range operations {
		if err := operation.run(); !errors.Is(err, ErrProjectIdentityChanged) {
			t.Fatalf("%s error = %v, want ErrProjectIdentityChanged", operation.name, err)
		}
	}
	identityDiagnostics := 0
	for _, item := range manager.Diagnostics() {
		if item.Code == "memory_project_identity_changed" {
			identityDiagnostics++
		}
	}
	if identityDiagnostics != 1 {
		t.Fatalf("project identity diagnostics = %d, want one bounded diagnostic", identityDiagnostics)
	}
	if entries, err := os.ReadDir(projectDir); err != nil || len(entries) != 0 {
		t.Fatalf("replacement project directory was modified: entries=%#v err=%v", entries, err)
	}
	replacementRoot := root + "-replacement"
	if err := os.Rename(root, replacementRoot); err != nil {
		t.Fatalf("move replacement root: %v", err)
	}
	if err := os.Rename(moved, root); err != nil {
		t.Fatalf("restore original root: %v", err)
	}
	if _, err := manager.LoadIndex(ScopeProject); !errors.Is(err, ErrProjectIdentityChanged) {
		t.Fatalf("project binding recovered after identity failure: %v", err)
	}
	userNote := NewNote(NoteUserPreference, ScopeUser, "user", "shared user remains writable", "test", time.Unix(4, 0))
	if err := manager.SaveNote(userNote); err != nil {
		t.Fatalf("shared user memory was disabled by project replacement: %v", err)
	}
}

func TestProjectIdentityWorkspaceManagerRejectsLateSymlinkAncestor(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	external := t.TempDir()
	marker := filepath.Join(external, "marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.Rename(filepath.Join(root, ".xagent"), filepath.Join(root, ".xagent-old")); err != nil {
		t.Fatalf("move bound project ancestor: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, ".xagent")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	note := NewNote(NoteProjectKnowledge, ScopeProject, "late-link", "must stay inside worktree", "test", time.Unix(1, 0))
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error { return manager.SaveNote(note) }},
		{name: "rebuild", run: func() error { _, err := manager.RebuildIndex(ScopeProject); return err }},
		{name: "delete", run: func() error { return manager.DeleteNote(ScopeProject, note.ID) }},
	}
	for _, operation := range operations {
		if err := operation.run(); !errors.Is(err, ErrProjectDirectoryChanged) {
			t.Fatalf("%s error = %v, want ErrProjectDirectoryChanged", operation.name, err)
		}
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("read external target: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(marker) {
		t.Fatalf("workspace memory touched external symlink target: %#v", entries)
	}
	directoryDiagnostics := 0
	for _, item := range manager.Diagnostics() {
		if item.Code == "memory_project_directory_changed" {
			directoryDiagnostics++
		}
	}
	if directoryDiagnostics != 1 {
		t.Fatalf("project directory diagnostics = %d, want one", directoryDiagnostics)
	}
}

func TestProjectIdentityWorkspaceManagerRejectsInternalSymlinkRedirect(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := os.Rename(projectDir, filepath.Join(root, ".xagent", "memory-old")); err != nil {
		t.Fatalf("move bound project directory: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", ".git"), projectDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	note := NewNote(NoteProjectKnowledge, ScopeProject, "internal-link", "must not reach git", "test", time.Unix(1, 0))
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error { return manager.SaveNote(note) }},
		{name: "rebuild", run: func() error { _, err := manager.RebuildIndex(ScopeProject); return err }},
		{name: "delete", run: func() error { return manager.DeleteNote(ScopeProject, note.ID) }},
	}
	for _, operation := range operations {
		if err := operation.run(); !errors.Is(err, ErrProjectDirectoryChanged) {
			t.Fatalf("%s error = %v, want ErrProjectDirectoryChanged", operation.name, err)
		}
	}
	entries, err := os.ReadDir(gitDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace memory touched .git: entries=%#v err=%v", entries, err)
	}
}

func TestProjectIdentityWorkspaceAtomicWriteRejectsInternalSymlinkRedirect(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(root, ".xagent"), 0o700); err != nil {
		t.Fatalf("create xagent dir: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", ".git"), filepath.Join(root, ".xagent", "memory")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rootHandle.Close()
	if err := atomicWriteRoot(rootHandle, filepath.Join(".xagent", "memory", "escaped.md"), []byte("must not reach git"), 0o600); err == nil {
		t.Fatal("nested atomic write followed an internal symlink into .git")
	}
	entries, err := os.ReadDir(gitDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace atomic write touched .git: entries=%#v err=%v", entries, err)
	}
}

func TestProjectIdentityWorkspaceDiagnosticsDoNotExposeAbsoluteRoot(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.addDiagnostics([]diagnostics.Diagnostic{diagnostics.New("workspace_test", diagnostics.SeverityWarning, "safe").WithAttributes(map[string]string{"key": "original"})})
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, IndexFileName), []byte("bad index"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := manager.LoadIndex(ScopeProject); err != nil {
		t.Fatalf("LoadIndex rebuild: %v", err)
	}
	status := manager.Status()
	if status.ProjectDir != "" || status.ProjectIdentity == "" {
		t.Fatalf("workspace status leaked project path or identity missing: %#v", status)
	}
	for _, item := range status.Diagnostics {
		if strings.Contains(item.Text(), root) {
			t.Fatalf("workspace diagnostic exposed absolute root: %#v", item)
		}
	}
	status.Diagnostics[0].Attributes["key"] = "mutated"
	if got := manager.Diagnostics()[0].Attributes["key"]; got != "original" {
		t.Fatalf("workspace diagnostics were not defensively copied: %q", got)
	}
}

func TestProjectIdentityWorkspaceManagerCloseReleasesProjectRoot(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: filepath.Join(root, ".xagent", "memory")},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := manager.LoadIndex(ScopeProject); !errors.Is(err, ErrWorkspaceManagerClosed) {
		t.Fatalf("LoadIndex after Close = %v, want ErrWorkspaceManagerClosed", err)
	}
}

func TestProjectIdentityWorkspaceManagerCloseWaitsForInFlightAtomicWrite(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	projectDir := filepath.Join(root, ".xagent", "memory")
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "config-a", ManagerOptions: ManagerOptions{ProjectDir: projectDir},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var barrierOnce sync.Once
	manager.writer.workspaceTempCreated = func() {
		barrierOnce.Do(func() {
			close(entered)
			<-release
		})
	}
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- manager.SaveNote(NewNote(NoteProjectKnowledge, ScopeProject, "in-flight", "body", "test", time.Unix(1, 0)))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("atomic write did not reach temp-file barrier")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close returned before in-flight write settled: %v", closeErr)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := manager.project.projectRoot.Stat("."); err != nil {
		t.Fatalf("project root closed while write was in flight: %v", err)
	}
	close(release)
	if saveErr := <-saveDone; saveErr != nil {
		t.Fatalf("SaveNote after release: %v", saveErr)
	}
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatalf("Close after release: %v", closeErr)
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("in-flight Close left temporary file %q", entry.Name())
		}
	}
	if err := manager.SaveNote(NewNote(NoteProjectKnowledge, ScopeProject, "closed", "body", "test", time.Unix(2, 0))); !errors.Is(err, ErrWorkspaceManagerClosed) {
		t.Fatalf("SaveNote after Close = %v, want ErrWorkspaceManagerClosed", err)
	}
}

func TestWorkspaceManagerCloseDoesNotDeadlockOnAsyncProjectDecision(t *testing.T) {
	root := projectIdentityCanonicalTempDir(t)
	projectDir := filepath.Join(root, ".xagent", "memory")
	providerBarrier := &blockingWorkspaceMemoryProvider{
		entered: make(chan struct{}), release: make(chan struct{}),
		delegate: &fakeMemoryProvider{response: `{"action":"add","title":"late","type":"project_knowledge","body":"body"}`},
	}
	manager, err := NewWorkspaceManager(WorkspaceManagerOptions{
		WorktreeRoot: root, ConfigDigest: "async-close",
		ManagerOptions: ManagerOptions{
			ProjectDir: projectDir, Provider: providerBarrier, UpdateQueueSize: 1, UpdateConcurrency: 1,
			UpdateTimeoutMS: 1000, MaxCandidateBytes: 1024,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateAsync(UpdateInput{Scope: ScopeProject, Candidate: "remember this project decision", Now: time.Unix(1, 0)})
	select {
	case <-providerBarrier.entered:
	case <-time.After(time.Second):
		t.Fatal("async update did not reach provider barrier")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(300 * time.Millisecond):
		close(providerBarrier.release)
		t.Fatal("Close deadlocked behind provider work holding the project lifecycle lock")
	}
	close(providerBarrier.release)
	if !manager.WaitUpdates(time.Second) {
		t.Fatal("async update did not stop after workspace Close")
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "late.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("closed workspace memory accepted a late project write: %v", statErr)
	}
}

type blockingWorkspaceMemoryProvider struct {
	entered  chan struct{}
	release  chan struct{}
	delegate *fakeMemoryProvider
	once     sync.Once
}

func (p *blockingWorkspaceMemoryProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return p.delegate.StreamChat(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*blockingWorkspaceMemoryProvider) Name() string { return "blocking-workspace-memory" }

func projectIdentityCanonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks temp root: %v", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("Abs temp root: %v", err)
	}
	return filepath.Clean(root)
}

func swapProjectIdentityPathCase(value string) string {
	bytes := []byte(value)
	for index, current := range bytes {
		switch {
		case current >= 'a' && current <= 'z':
			bytes[index] = current - ('a' - 'A')
			return string(bytes)
		case current >= 'A' && current <= 'Z':
			bytes[index] = current + ('a' - 'A')
			return string(bytes)
		}
	}
	return value
}
