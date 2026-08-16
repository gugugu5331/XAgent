package workspace

import (
	"context"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/permission"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestToolBindingUsesTaskRegistryRootAndExecutor(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	tracker := &workspaceToolTracker{}
	mainRoot := t.TempDir()
	source := tool.NewSafeCandidateRegistry()
	contextual := &workspaceToolProbe{name: "Contextual", root: mainRoot, tracker: tracker}
	independent := &workspaceToolProbe{name: "Independent", root: mainRoot, tracker: tracker}
	fixed := &workspaceToolProbe{name: "WorktreeManagerCapability", root: mainRoot, tracker: tracker}
	unknown := &workspaceToolProbe{name: "Unknown", root: mainRoot, tracker: tracker}
	registerWorkspaceToolProbe(t, source, contextual, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, contextual)
	registerWorkspaceToolProbe(t, source, independent, tool.WorkspacePolicy{Mode: tool.WorkspaceIndependent}, nil)
	registerWorkspaceToolProbe(t, source, fixed, tool.WorkspacePolicy{Mode: tool.WorkspaceFixed}, nil)
	registerWorkspaceToolProbe(t, source, unknown, tool.WorkspacePolicy{}, nil)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	mainExecutor, err := tool.NewExecutorWithResultFactory(source, mainRoot, time.Second, 4096, resultFactory)
	if err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	request, scope := newFactoryBindFixture(t)
	request.Role = &agentrole.Definition{Metadata: agentrole.Metadata{ToolAllow: []string{"Contextual"}}}
	runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	defer runtime.Close(context.Background())

	if runtime.Registry() == source {
		t.Fatal("task runtime reused the source registry")
	}
	if runtime.ToolExecutor() == mainExecutor || runtime.ToolExecutor().ProjectRoot != request.Root || runtime.ToolExecutor().Registry != runtime.Registry() {
		t.Fatal("task ToolExecutor reused or retained the main root executor")
	}
	if runtime.Executor() == nil || runtime.Capabilities() == nil {
		t.Fatal("task scoped executor or capability switch is missing")
	}
	if got := runtime.Capabilities().Current().Names; !sameStrings(got, []string{"Contextual"}) {
		t.Fatalf("role-filtered capability names = %v, want Contextual", got)
	}
	if got := runtime.Registry().Names(); !sameStrings(got, []string{"Contextual", "Independent"}) {
		t.Fatalf("task registry names = %v", got)
	}
	report := runtime.ToolBindingReport()
	if report.Filtered["WorktreeManagerCapability"] != tool.WorkspaceFilterFixed || report.Filtered["Unknown"] != tool.WorkspaceFilterUnknown {
		t.Fatalf("workspace filtering report = %#v", report.Filtered)
	}
	report.Filtered["Contextual"] = tool.WorkspaceFilterUnknown
	if _, mutated := runtime.ToolBindingReport().Filtered["Contextual"]; mutated {
		t.Fatal("caller mutated Runtime's workspace binding report")
	}
	if tracker.boundRoot() != request.Root {
		t.Fatalf("contextual binder root = %q, want %q", tracker.boundRoot(), request.Root)
	}

	call := tool.Call{ID: "task-call", Name: "Contextual", ArgumentsJSON: `{}`}
	validated, ticket := prepareWorkspaceCall(t, runtime.ToolExecutor(), scope, call)
	runtime.Executor().ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if got := tracker.executionRoots(); !sameStrings(got, []string{request.Root}) {
		t.Fatalf("task execution roots = %v, want task root", got)
	}

	// A validated call is an execution capability input, not just parsed JSON.
	// The task executor must reject provenance from the main-root executor.
	mainValidated, mainTicket := prepareWorkspaceCall(t, mainExecutor, scope, tool.Call{ID: "main-call", Name: "Contextual", ArgumentsJSON: `{}`})
	runtime.Executor().ExecuteValidatedAuthorized(context.Background(), mainValidated, mainTicket)
	if got := tracker.executionRoots(); !sameStrings(got, []string{request.Root}) {
		t.Fatalf("main validated call crossed task lineage: executions=%v", got)
	}
}

func TestToolBindingBashRequiresTrustedContainment(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		containment bool
		wantBash    bool
	}{
		{name: "untrusted", containment: false, wantBash: false},
		{name: "trusted", containment: true, wantBash: true},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
			if err != nil {
				t.Fatal(err)
			}
			tracker := &workspaceToolTracker{}
			source := tool.NewSafeCandidateRegistry()
			bash := &workspaceToolProbe{name: "Bash", root: t.TempDir(), tracker: tracker}
			registerWorkspaceToolProbe(t, source, bash, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual, WriteContainment: testCase.containment}, bash)
			if err := source.Seal(); err != nil {
				t.Fatal(err)
			}
			factory := newWorkspaceFactoryForTest(t, source, resultFactory)
			request, scope := newFactoryBindFixture(t)
			runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close(context.Background())
			_, gotBash := runtime.Registry().Get("Bash")
			if gotBash != testCase.wantBash {
				t.Fatalf("Bash visible = %v, want %v; report=%#v", gotBash, testCase.wantBash, runtime.ToolBindingReport().Filtered)
			}
			if !testCase.wantBash && runtime.ToolBindingReport().Filtered["Bash"] != tool.WorkspaceFilterNoWriteContainment {
				t.Fatal("untrusted Bash did not receive the stable containment filter")
			}
		})
	}
}

type workspaceToolTracker struct {
	mu         sync.Mutex
	bound      string
	executions []string
}

func (tracker *workspaceToolTracker) recordBinding(root string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.bound = root
}

func (tracker *workspaceToolTracker) recordExecution(root string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.executions = append(tracker.executions, root)
}

func (tracker *workspaceToolTracker) boundRoot() string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.bound
}

func (tracker *workspaceToolTracker) executionRoots() []string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return append([]string(nil), tracker.executions...)
}

type workspaceToolProbe struct {
	name    string
	root    string
	tracker *workspaceToolTracker
}

func (probe *workspaceToolProbe) Name() string           { return probe.name }
func (*workspaceToolProbe) Description() string          { return "workspace task binding probe" }
func (*workspaceToolProbe) Schema() tool.Schema          { return tool.ObjectSchema(nil, nil) }
func (*workspaceToolProbe) Risk() tool.Risk              { return tool.RiskSafe }
func (*workspaceToolProbe) UsesSafeResultBoundary() bool { return true }
func (probe *workspaceToolProbe) Execute(context.Context, tool.Input) tool.Result {
	probe.tracker.recordExecution(probe.root)
	return tool.Result{}
}

func (probe *workspaceToolProbe) BindWorkspace(binding tool.WorkspaceBinding) (tool.Tool, error) {
	probe.tracker.recordBinding(binding.Root)
	return &workspaceToolProbe{name: probe.name, root: binding.Root, tracker: probe.tracker}, nil
}

func registerWorkspaceToolProbe(t *testing.T, registry *tool.Registry, probe tool.Tool, policy tool.WorkspacePolicy, binder tool.WorkspaceBinder) {
	t.Helper()
	if err := registry.RegisterWithOptions(probe, tool.RegistrationOptions{Workspace: policy, WorkspaceBinder: binder}); err != nil {
		t.Fatal(err)
	}
}

func prepareWorkspaceCall(t *testing.T, executor *tool.Executor, scope permission.TaskScope, call tool.Call) (tool.ValidatedCall, permission.ExecutionTicket) {
	t.Helper()
	validated, err := executor.PrepareCall(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := executor.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := scope.Issuer.Issue(call.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return validated, ticket
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
