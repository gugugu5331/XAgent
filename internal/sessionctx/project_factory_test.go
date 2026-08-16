package sessionctx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/prompt"
)

func TestProjectFactoryBindsAbsoluteRootAndRebuildsProjectDependencies(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	userInstructions := &projectFactoryInstructionLoader{sections: []prompt.Section{
		{Name: "global", Content: "global shared", Stable: true, Scope: prompt.ScopeGlobal},
		{Name: "user", Content: "user shared", Stable: true, Scope: prompt.ScopeUser},
		{Name: "wrong-user", Content: "must not escape", Stable: true, Scope: prompt.ScopeProject},
		{Name: "unknown-user", Content: "unknown scope must not escape", Stable: true, Scope: prompt.Scope("future")},
	}}
	userMemory := &projectFactoryMemoryProvider{indices: map[memory.Scope]memory.Index{
		memory.ScopeUser: {Scope: memory.ScopeUser, Entries: []memory.IndexEntry{{ID: "user", Title: "user", Body: "shared user memory"}}},
	}}
	builder := &recordingProjectBuilder{}
	factory, err := NewProjectFactory(ProjectFactoryOptions{
		UserInstructions: userInstructions,
		UserMemory:       userMemory,
		Project:          builder,
	})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}

	first, err := factory.Bind(context.Background(), firstRoot)
	if err != nil {
		t.Fatalf("Bind(first): %v", err)
	}
	firstAgain, err := factory.Bind(context.Background(), firstRoot)
	if err != nil {
		t.Fatalf("Bind(first again): %v", err)
	}
	second, err := factory.Bind(context.Background(), secondRoot)
	if err != nil {
		t.Fatalf("Bind(second): %v", err)
	}

	wantRoots := []string{firstRoot, firstRoot, secondRoot}
	if !reflect.DeepEqual(builder.roots, wantRoots) {
		t.Fatalf("project build roots = %#v, want %#v", builder.roots, wantRoots)
	}
	if first == firstAgain || first.ProjectRoot() != firstRoot || second.ProjectRoot() != secondRoot {
		t.Fatalf("factory did not return distinct root-bound managers: first=%p again=%p second=%p", first, firstAgain, second)
	}
	if first.ProjectCacheKey() != firstRoot || second.ProjectCacheKey() != secondRoot || first.ProjectCacheKey() == second.ProjectCacheKey() {
		t.Fatalf("project cache keys are not isolated: first=%q second=%q", first.ProjectCacheKey(), second.ProjectCacheKey())
	}

	prepared := first.PrepareStable(context.Background())
	joined := joinSections(prepared.StableSections)
	for _, want := range []string{"global shared", "user shared", "project:" + firstRoot, "project memory:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("prepared sections %q do not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "must not escape") || strings.Contains(joined, "unknown scope must not escape") {
		t.Fatalf("user dependency leaked a rejected section: %q", joined)
	}
	if userInstructions.calls != 1 || !reflect.DeepEqual(userMemory.scopes, []memory.Scope{memory.ScopeUser}) {
		t.Fatalf("shared dependencies called incorrectly: instructions=%d memory=%#v", userInstructions.calls, userMemory.scopes)
	}
	if got := builder.providers[0].scopes; !reflect.DeepEqual(got, []memory.Scope{memory.ScopeProject}) {
		t.Fatalf("project memory scopes = %#v, want only project", got)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_scope_mismatch") {
		t.Fatalf("scope mismatch diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryRejectsRootReplacedByBuilder(t *testing.T) {
	root := t.TempDir()
	factory, err := NewProjectFactory(ProjectFactoryOptions{Project: projectBuilderFunc(func(_ context.Context, projectRoot string) (ProjectDependencies, error) {
		replaceProjectFactoryRoot(t, projectRoot)
		return ProjectDependencies{}, nil
	})})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	if _, err := factory.Bind(context.Background(), root); !errors.Is(err, ErrProjectFactoryInvalid) {
		t.Fatalf("Bind after builder root replacement = %v, want ErrProjectFactoryInvalid", err)
	}
}

func TestProjectFactoryDropsAllProjectContextWhenLoaderReplacesRoot(t *testing.T) {
	root := t.TempDir()
	user := &projectFactoryInstructionLoader{sections: []prompt.Section{{
		Name: "user", Content: "shared user survives", Stable: true, Scope: prompt.ScopeUser,
	}}}
	factory, err := NewProjectFactory(ProjectFactoryOptions{
		UserInstructions: user,
		Project: projectBuilderFunc(func(context.Context, string) (ProjectDependencies, error) {
			return ProjectDependencies{Instructions: replacingProjectLoader{replace: func() {
				replaceProjectFactoryRoot(t, root)
			}}}, nil
		}),
	})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	manager, err := factory.Bind(context.Background(), root)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared := manager.PrepareStable(context.Background())
	joined := joinSections(prepared.StableSections)
	if !strings.Contains(joined, "shared user survives") || strings.Contains(joined, "stale project loader") {
		t.Fatalf("loader identity race was not fail-closed: %q", joined)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_project_root_changed") {
		t.Fatalf("root identity diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryDropsEarlierProjectSectionsWhenMemoryReplacesRoot(t *testing.T) {
	root := t.TempDir()
	factory, err := NewProjectFactory(ProjectFactoryOptions{Project: projectBuilderFunc(func(context.Context, string) (ProjectDependencies, error) {
		return ProjectDependencies{
			Instructions: &projectFactoryInstructionLoader{sections: []prompt.Section{{
				Name: "project", Content: "earlier project instruction", Stable: true, Scope: prompt.ScopeProject,
			}}},
			Memory: &replacingProjectMemory{replace: func() { replaceProjectFactoryRoot(t, root) }},
		}, nil
	})})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	manager, err := factory.Bind(context.Background(), root)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared := manager.PrepareStable(context.Background())
	joined := joinSections(prepared.StableSections)
	if strings.Contains(joined, "earlier project instruction") || strings.Contains(joined, "stale project memory") {
		t.Fatalf("memory identity race did not roll back project context: %q", joined)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_project_root_changed") {
		t.Fatalf("root identity diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryRejectsMemoryIndexFromDifferentScope(t *testing.T) {
	root := t.TempDir()
	userMemory := &fixedProjectFactoryMemoryProvider{index: memory.Index{
		Scope:   memory.ScopeProject,
		Entries: []memory.IndexEntry{{ID: "project-secret", Title: "project", Body: "project content must not enter user scope"}},
	}}
	factory, err := NewProjectFactory(ProjectFactoryOptions{
		UserMemory: userMemory,
		Project: projectBuilderFunc(func(context.Context, string) (ProjectDependencies, error) {
			return ProjectDependencies{}, nil
		}),
	})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	manager, err := factory.Bind(context.Background(), root)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared := manager.PrepareStable(context.Background())
	if joined := joinSections(prepared.StableSections); strings.Contains(joined, "project content must not enter user scope") {
		t.Fatalf("mismatched project memory leaked into user scope: %q", joined)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_memory_scope_mismatch") {
		t.Fatalf("memory scope mismatch diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryContextRootReplacementDoesNotReportSuccess(t *testing.T) {
	root := t.TempDir()
	factory, err := NewProjectFactory(ProjectFactoryOptions{Project: projectBuilderFunc(func(context.Context, string) (ProjectDependencies, error) {
		return ProjectDependencies{
			Instructions: &projectFactoryInstructionLoader{sections: []prompt.Section{{
				Name: "project", Content: "project context must be rolled back", Stable: true, Scope: prompt.ScopeProject,
			}}},
			Context: replacingProjectContext{replace: func() { replaceProjectFactoryRoot(t, root) }},
		}, nil
	})})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	manager, err := factory.Bind(context.Background(), root)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared, err := manager.Prepare(context.Background(), conversation.NewConversation("root-replaced", zeroTime()), PrepareAuto)
	if !errors.Is(err, ErrProjectRootChanged) {
		t.Fatalf("Prepare error = %v, want ErrProjectRootChanged", err)
	}
	if prepared.MessagesChanged || prepared.ContextResult.Changed {
		t.Fatalf("root replacement was reported as successful context mutation: %#v", prepared)
	}
	if joined := joinSections(prepared.StableSections); strings.Contains(joined, "project context must be rolled back") {
		t.Fatalf("project stable context survived context-stage root replacement: %q", joined)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_project_root_changed") {
		t.Fatalf("root identity diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryRejectsInvalidRootsWithoutChangingProcessCWD(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	factory, err := NewProjectFactory(ProjectFactoryOptions{Project: &recordingProjectBuilder{}})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	before, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd before: %v", err)
	}
	for _, candidate := range []string{"relative", root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root), link, filepath.Join(root, "missing")} {
		if _, err := factory.Bind(context.Background(), candidate); !errors.Is(err, ErrProjectFactoryInvalid) {
			t.Fatalf("Bind(%q) error = %v, want ErrProjectFactoryInvalid", candidate, err)
		}
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd after: %v", err)
	}
	if after != before {
		t.Fatalf("process cwd changed: before=%q after=%q", before, after)
	}
}

func TestProjectFactoryFailsClosedWhenBoundRootIdentityChanges(t *testing.T) {
	root := t.TempDir()
	user := &projectFactoryInstructionLoader{sections: []prompt.Section{{
		Name: "user", Content: "shared user survives", Stable: true, Scope: prompt.ScopeUser,
	}}}
	builder := &recordingProjectBuilder{}
	factory, err := NewProjectFactory(ProjectFactoryOptions{UserInstructions: user, Project: builder})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	manager, err := factory.Bind(context.Background(), root)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("replace root: %v", err)
	}

	prepared := manager.PrepareStable(context.Background())
	joined := joinSections(prepared.StableSections)
	if !strings.Contains(joined, "shared user survives") {
		t.Fatalf("user context was discarded after project identity change: %q", joined)
	}
	if strings.Contains(joined, "project:"+root) {
		t.Fatalf("stale project context survived root replacement: %q", joined)
	}
	if !hasDiagnosticCode(prepared.Diagnostics, "sessionctx_project_root_changed") {
		t.Fatalf("root identity diagnostic missing: %#v", prepared.Diagnostics)
	}
}

func TestProjectFactoryValidatesBuilderAndBuildResult(t *testing.T) {
	if _, err := NewProjectFactory(ProjectFactoryOptions{}); !errors.Is(err, ErrProjectFactoryInvalid) {
		t.Fatalf("missing builder error = %v, want ErrProjectFactoryInvalid", err)
	}
	factory, err := NewProjectFactory(ProjectFactoryOptions{Project: projectBuilderFunc(func(context.Context, string) (ProjectDependencies, error) {
		return ProjectDependencies{}, errors.New("build failed")
	})})
	if err != nil {
		t.Fatalf("NewProjectFactory: %v", err)
	}
	if _, err := factory.Bind(context.Background(), t.TempDir()); !errors.Is(err, ErrProjectFactoryBuild) {
		t.Fatalf("builder failure = %v, want ErrProjectFactoryBuild", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.Bind(canceled, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Bind error = %v, want context.Canceled", err)
	}
}

type recordingProjectBuilder struct {
	roots     []string
	providers []*projectFactoryMemoryProvider
}

func (b *recordingProjectBuilder) BuildProject(_ context.Context, root string) (ProjectDependencies, error) {
	b.roots = append(b.roots, root)
	provider := &projectFactoryMemoryProvider{indices: map[memory.Scope]memory.Index{
		memory.ScopeProject: {Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{ID: root, Title: "project", Body: "project memory:" + root}}},
	}}
	b.providers = append(b.providers, provider)
	return ProjectDependencies{
		Instructions: &projectFactoryInstructionLoader{sections: []prompt.Section{
			{Name: "project", Content: "project:" + root, Stable: true, Scope: prompt.ScopeProject},
			{Name: "wrong-project", Content: "must not become user", Stable: true, Scope: prompt.ScopeUser},
		}},
		Memory:  provider,
		Context: &projectFactoryContextPreparer{},
	}, nil
}

type projectBuilderFunc func(context.Context, string) (ProjectDependencies, error)

func (f projectBuilderFunc) BuildProject(ctx context.Context, root string) (ProjectDependencies, error) {
	return f(ctx, root)
}

type projectFactoryInstructionLoader struct {
	sections []prompt.Section
	calls    int
}

func (l *projectFactoryInstructionLoader) Load(context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	l.calls++
	return append([]prompt.Section(nil), l.sections...), nil
}

type projectFactoryMemoryProvider struct {
	indices map[memory.Scope]memory.Index
	scopes  []memory.Scope
}

func (p *projectFactoryMemoryProvider) LoadIndex(scope memory.Scope) (memory.Index, error) {
	p.scopes = append(p.scopes, scope)
	return p.indices[scope], nil
}

func (*projectFactoryMemoryProvider) Diagnostics() []diagnostics.Diagnostic { return nil }

type projectFactoryContextPreparer struct{}

func (*projectFactoryContextPreparer) Prepare(context.Context, *conversation.Conversation, contextmgr.Mode) (contextmgr.Result, error) {
	return contextmgr.Result{}, nil
}

type replacingProjectLoader struct {
	replace func()
}

func (l replacingProjectLoader) Load(context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	l.replace()
	return []prompt.Section{{Name: "stale", Content: "stale project loader", Stable: true, Scope: prompt.ScopeProject}}, nil
}

type replacingProjectMemory struct {
	replace func()
}

func (p *replacingProjectMemory) LoadIndex(memory.Scope) (memory.Index, error) {
	p.replace()
	return memory.Index{Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{ID: "stale", Title: "stale", Body: "stale project memory"}}}, nil
}

func (*replacingProjectMemory) Diagnostics() []diagnostics.Diagnostic { return nil }

type fixedProjectFactoryMemoryProvider struct {
	index memory.Index
}

func (p *fixedProjectFactoryMemoryProvider) LoadIndex(memory.Scope) (memory.Index, error) {
	return p.index, nil
}

func (*fixedProjectFactoryMemoryProvider) Diagnostics() []diagnostics.Diagnostic { return nil }

type replacingProjectContext struct {
	replace func()
}

func (p replacingProjectContext) Prepare(context.Context, *conversation.Conversation, contextmgr.Mode) (contextmgr.Result, error) {
	p.replace()
	return contextmgr.Result{Changed: true}, nil
}

func replaceProjectFactoryRoot(t *testing.T, root string) {
	t.Helper()
	moved := root + "-replaced"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("replace root: %v", err)
	}
}

func hasDiagnosticCode(items []diagnostics.Diagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}
