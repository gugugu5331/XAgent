package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

func TestWorkspaceFactoryFiltersCommandWithoutTrustedContainment(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	sink := workspaceFactoryDiagnostics(t)
	snapshot := newSnapshot([]Rule{commandRule(EventSystemStart, 1, "echo filtered", false, false, false)})
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: snapshot,
		Engine:   EngineOptions{Diagnostics: sink, Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	// Mutating the caller's copy after factory construction must not change the
	// command classification retained by the factory.
	snapshot.rules[0].action.decision = true
	runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	bound := runtime.(*workspaceHookRuntime)
	if len(bound.Engine.rules) != 0 {
		t.Fatalf("uncontained command rule remained registered: %#v", bound.Engine.rules)
	}
	if _, ok := bound.Engine.command.(disabledWorkspaceCommandRunner); !ok {
		t.Fatalf("factory reused NewEngine command default: %T", bound.Engine.command)
	}
	items := sink.Snapshot().Items()
	if len(items) != 1 || items[0].Diagnostic.Code != DiagnosticCommandContainmentFiltered ||
		strings.Contains(items[0].Diagnostic.Message.Text(), root) || strings.Contains(items[0].Diagnostic.Source, root) {
		t.Fatalf("filtered command diagnostic is missing or path-bearing: %#v", items)
	}
}

func TestCommandContainmentDecisionRuleFailsPrepareClosed(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: newSnapshot([]Rule{commandRule(EventToolBefore, 1, "decide", true, false, false)}),
		Engine:   EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	if runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working}); !errors.Is(err, ErrWorkspaceCommandContainmentRequired) || runtime != nil {
		t.Fatalf("Prepare decision command = runtime=%T err=%v", runtime, err)
	}
}

func TestWorkspaceFactoryBindsCanonicalRootEventAndCommandCWD(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	process := newStaticCommandProcess("", "", proctree.Result{})
	runner := &commandTestRunner{process: process}
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: newSnapshot([]Rule{commandRule(EventSystemStart, 1, "echo protected", false, false, false)}),
		Engine:   EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
		Runner:   runner,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working, Plans: &commandTestPlanFactory{}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	runtime.SystemStart(context.Background())
	runner.mu.Lock()
	request := runner.request
	runner.mu.Unlock()
	if request.WorkingDir != working || request.Mode != proctree.ProtectionRequired {
		t.Fatalf("command cwd/protection not bound: %#v", request)
	}
	var event EventContext
	if err := json.Unmarshal(process.stdinBytes(), &event); err != nil || event.Project.Root != root {
		t.Fatalf("command event root = %#v err=%v", event.Project, err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	bound := runtime.(*workspaceHookRuntime)
	if bound.identityRoot.Identity() != (safefs.Identity{}) {
		t.Fatal("runtime Close retained its owned identity root")
	}
	if working.Identity() == (safefs.Identity{}) {
		t.Fatal("runtime Close closed the borrowed working directory")
	}
}

func TestWorkspaceFactoryUsesExactTaskProtectionPlan(t *testing.T) {
	firstRoot, firstWorking := workspaceFactoryRoot(t)
	secondRoot, secondWorking := workspaceFactoryRoot(t)
	runner := &workspaceBorrowedRunner{}
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: newSnapshot([]Rule{commandRule(EventSystemStart, 1, "echo protected", false, false, false)}),
		Engine:   EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
		Runner:   runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPlans := &commandTestPlanFactory{}
	secondPlans := &commandTestPlanFactory{}
	first, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{
		ProjectRoot: firstRoot, WorkingDirectory: firstWorking, Plans: firstPlans,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	second, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{
		ProjectRoot: secondRoot, WorkingDirectory: secondWorking, Plans: secondPlans,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	firstCommand, firstOK := first.(*workspaceHookRuntime).Engine.command.(*ShellCommandRunner)
	secondCommand, secondOK := second.(*workspaceHookRuntime).Engine.command.(*ShellCommandRunner)
	if !firstOK || !secondOK || firstCommand.Plans != firstPlans || secondCommand.Plans != secondPlans ||
		firstCommand.Plans == secondCommand.Plans {
		t.Fatalf("task plans crossed runtimes: first=%T/%p second=%T/%p", firstCommand.Plans, firstCommand.Plans, secondCommand.Plans, secondCommand.Plans)
	}
	if firstCommand.WorkingDirectory != firstWorking || secondCommand.WorkingDirectory != secondWorking {
		t.Fatal("task working roots crossed hook runtimes")
	}
}

func TestWorkspaceFactoryRejectsTaskPlansWithoutTrustedRunner(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime, prepareErr := factory.Prepare(context.Background(), WorkspacePrepareRequest{
		ProjectRoot: root, WorkingDirectory: working, Plans: &commandTestPlanFactory{},
	}); runtime != nil || !errors.Is(prepareErr, ErrWorkspaceFactoryInvalid) {
		t.Fatalf("Prepare with untrusted task plans = runtime=%T err=%v", runtime, prepareErr)
	}
}

func TestWorkspaceFactoryPreservesNonCommandSnapshotAndNestedValues(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	parsed, err := url.Parse("https://example.com/hook")
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		commandRule(EventSystemStart, 1, "filtered", false, true, true),
		{Event: EventSystemStart, Source: Source{Path: "rules.yaml", Ordinal: 2, EffectiveOrdinal: 2}, Once: true, Async: true, Timeout: 7 * time.Second,
			action: compiledAction{typeName: ActionHTTP, timeout: 7 * time.Second, http: &httpAction{url: parsed, method: http.MethodPost, headers: http.Header{"X-Test": []string{"original"}}}}},
		{Event: EventSystemStart, Source: Source{Path: "rules.yaml", Ordinal: 3, EffectiveOrdinal: 3}, Timeout: 3 * time.Second,
			action: compiledAction{typeName: ActionPrompt, timeout: 3 * time.Second, scope: ScopeSession, template: &compiledTemplate{parts: []templatePart{{literal: "prompt-original"}}}}},
		{Event: EventSystemStart, Source: Source{Path: "rules.yaml", Ordinal: 4, EffectiveOrdinal: 4}, Timeout: 4 * time.Second,
			action: compiledAction{typeName: ActionSubAgent, timeout: 4 * time.Second, agent: "reviewer", template: &compiledTemplate{parts: []templatePart{{literal: "agent-original"}}}}},
	}
	rules[0].action.env = map[string]string{"SAFE": "original"}
	snapshot := newSnapshot(rules)
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: snapshot, Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	snapshot.rules[0].action.env["SAFE"] = "mutated"
	snapshot.rules[1].action.http.headers.Set("X-Test", "mutated")
	snapshot.rules[2].action.template.parts[0].literal = "mutated"
	runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	boundRules := runtime.(*workspaceHookRuntime).Engine.rules
	if len(boundRules) != 3 || boundRules[0].ActionType() != ActionHTTP || boundRules[1].ActionType() != ActionPrompt || boundRules[2].ActionType() != ActionSubAgent {
		t.Fatalf("non-command rule order changed: %#v", boundRules)
	}
	if !boundRules[0].Once || !boundRules[0].Async || boundRules[0].Timeout != 7*time.Second || boundRules[0].action.http.headers.Get("X-Test") != "original" ||
		boundRules[1].action.template.parts[0].literal != "prompt-original" || boundRules[2].action.template.parts[0].literal != "agent-original" {
		t.Fatalf("non-command rule content changed: %#v", boundRules)
	}
	if factory.snapshot.rules[0].action.env["SAFE"] != "original" {
		t.Fatalf("caller env mutation reached factory snapshot: %#v", factory.snapshot.rules[0].action.env)
	}
}

func TestWorkspaceFactoryCloseDoesNotCloseBorrowedRuntimes(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	runner := &workspaceBorrowedRunner{}
	plans := &workspaceBorrowedPlans{}
	httpRunner := &workspaceBorrowedHTTP{}
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor(), HTTPRunner: httpRunner},
		Runner: runner,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working, Plans: plans})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if runner.closed || plans.closed || httpRunner.closed || working.Identity() == (safefs.Identity{}) {
		t.Fatalf("borrowed runtime was closed: runner=%t plans=%t http=%t working=%v", runner.closed, plans.closed, httpRunner.closed, working.Identity())
	}
}

func TestWorkspaceFactoryPrepareFailureRollsBackOwnedIdentityRoot(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor(), CleanupTimeout: maxHookCleanupTimeout + time.Nanosecond},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	openIdentity := factory.openIdentity
	var owned *safefs.Root
	factory.openIdentity = func(path string, policy safefs.Policy) (safefs.OpenResult, error) {
		opened, openErr := openIdentity(path, policy)
		owned = opened.Root
		return opened, openErr
	}
	if runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working}); runtime != nil || err == nil {
		t.Fatalf("Prepare invalid Engine = runtime=%T err=%v", runtime, err)
	}
	if owned == nil || owned.Identity() != (safefs.Identity{}) || working.Identity() == (safefs.Identity{}) {
		t.Fatalf("Prepare rollback ownership: owned=%v working=%v", owned, working.Identity())
	}
}

func TestWorkspaceFactoryRejectsForeignOrNonCanonicalRoot(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	otherRoot, otherWorking := workspaceFactoryRoot(t)
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	tests := []WorkspacePrepareRequest{
		{ProjectRoot: "relative", WorkingDirectory: working},
		{ProjectRoot: root + string(filepath.Separator) + ".", WorkingDirectory: working},
		{ProjectRoot: link, WorkingDirectory: working},
		{ProjectRoot: root, WorkingDirectory: otherWorking},
		{ProjectRoot: otherRoot, WorkingDirectory: working},
	}
	for index, request := range tests {
		if runtime, err := factory.Prepare(context.Background(), request); !errors.Is(err, ErrWorkspaceFactoryInvalid) || runtime != nil {
			t.Fatalf("case %d Prepare = runtime=%T err=%v", index, runtime, err)
		}
	}
}

func TestWorkspaceFactoryRejectsCaseAlias(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	alias := swapWorkspaceFactoryCase(root)
	if alias == root {
		t.Skip("temporary directory has no cased component")
	}
	rootInfo, rootErr := os.Stat(root)
	aliasInfo, aliasErr := os.Stat(alias)
	if rootErr != nil || aliasErr != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem does not resolve case aliases")
	}
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	if runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: alias, WorkingDirectory: working}); runtime != nil || !errors.Is(err, ErrWorkspaceFactoryInvalid) {
		t.Fatalf("Prepare accepted case alias %q for canonical root %q: runtime=%T err=%v", alias, root, runtime, err)
	}
}

func TestCommandContainmentFailsClosedAfterRootIdentityChanges(t *testing.T) {
	root, working := workspaceFactoryRoot(t)
	runner := &commandTestRunner{process: newStaticCommandProcess("", "", proctree.Result{})}
	factory, err := NewWorkspaceFactory(WorkspaceFactoryOptions{
		Snapshot: newSnapshot([]Rule{
			commandRule(EventSystemStart, 1, "must-not-run", false, false, false),
			commandRule(EventToolBefore, 2, "must-deny", true, false, false),
		}),
		Engine: EngineOptions{Diagnostics: workspaceFactoryDiagnostics(t), Redactor: redact.NewRuntimeRedactor()},
		Runner: runner,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceFactory: %v", err)
	}
	runtime, err := factory.Prepare(context.Background(), WorkspacePrepareRequest{ProjectRoot: root, WorkingDirectory: working, Plans: &commandTestPlanFactory{}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatalf("rename workspace root: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("replace workspace root: %v", err)
	}
	runtime.SystemStart(context.Background())
	decision := runtime.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}, NewToolInput("c", "Read", map[string]any{}))
	if !decision.IsDeny() || !strings.Contains(decision.Reason(), "workspace hook") {
		t.Fatalf("identity change did not fail decision closed: %#v", decision)
	}
	runner.mu.Lock()
	request := runner.request
	runner.mu.Unlock()
	if request.Executable != "" {
		t.Fatalf("command started after root replacement: %#v", request)
	}
}

func workspaceFactoryRoot(t *testing.T) (string, *safefs.Root) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root = filepath.Clean(root)
	opened, err := safefs.Bootstrap(root, safefs.Policy{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	return root, opened.Root
}

func workspaceFactoryDiagnostics(t *testing.T) *diagnostics.Sink {
	t.Helper()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor: redact.NewRuntimeRedactor(), MaxItems: 8, MaxItemBytes: 512, MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatalf("NewBoundedSink: %v", err)
	}
	return sink
}

func swapWorkspaceFactoryCase(value string) string {
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

type workspaceBorrowedRunner struct{ closed bool }

func (*workspaceBorrowedRunner) Start(context.Context, proctree.Request) (proctree.Process, error) {
	return nil, fmt.Errorf("unused")
}
func (r *workspaceBorrowedRunner) Close() error { r.closed = true; return nil }

type workspaceBorrowedPlans struct{ closed bool }

func (*workspaceBorrowedPlans) Create(context.Context) (proctree.ProtectionPlan, error) {
	return proctree.ProtectionPlan{}, fmt.Errorf("unused")
}
func (p *workspaceBorrowedPlans) Close() error { p.closed = true; return nil }

type workspaceBorrowedHTTP struct{ closed bool }

func (*workspaceBorrowedHTTP) Run(context.Context, HTTPRequest) (HTTPResult, error) {
	return HTTPResult{}, fmt.Errorf("unused")
}
func (h *workspaceBorrowedHTTP) CloseIdleConnections() { h.closed = true }
