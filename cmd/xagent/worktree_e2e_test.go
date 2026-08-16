package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/sessionctx"
	"xagent/internal/subagent"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

func TestWorktreeE2EParallelSameRoleTasksRemainIsolatedWhenOneFails(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repository := worktreeE2ERepository(t, gitPath)
	mainFile := filepath.Join(repository, "same.txt")
	if err := os.WriteFile(mainFile, []byte("main-dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	statusBefore := worktreeE2EGit(t, gitPath, repository, "status", "--porcelain=v1", "--untracked-files=all")
	processCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	redactor := redact.NewRuntimeRedactor()
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &worktreeE2EToolRecorder{}
	writeTool := &worktreeE2EWriteTool{factory: resultFactory, recorder: recorder}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.RegisterWithOptions(writeTool, tool.RegistrationOptions{
		Workspace:       tool.WorkspacePolicy{Mode: tool.WorkspaceContextual, WriteContainment: true},
		WorkspaceBinder: writeTool,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	executor, err := tool.NewExecutorWithResultFactory(registry, repository, 2*time.Second, 8<<10, resultFactory)
	if err != nil {
		t.Fatal(err)
	}
	providerScript := newWorktreeE2EProvider(redactor)
	configValue := worktreeE2EConfig(t)
	graphOptions := assemblyWorktreeGraphFixture(t)
	graphOptions.ProjectRoot = repository
	graphOptions.SourceRegistry = registry
	graphOptions.ResultFactory = resultFactory
	graphOptions.RuntimeRedactor = redactor
	graphOptions.GitExecutable = gitPath
	graphOptions.GitRunner = nil
	graphOptions.Config = configValue.Subagent.Worktree.Clone()
	graphOptions.ExecutorTimeout = 2 * time.Second
	graphOptions.MaxOutputBytes = 8 << 10
	graph, err := newAssemblyWorktreeGraph(graphOptions)
	if err != nil {
		t.Fatal(err)
	}

	contextManager, err := contextmgr.New(nil, contextmgr.ManagerOptions{
		Context: configValue.Context, InlineOutputBytes: 8 << 10, RuntimeRedactor: redactor,
	})
	if err != nil {
		closeAssemblyWorktreeGraphTest(t, graph)
		t.Fatal(err)
	}
	contextPolicy, err := orchestrator.NewSkillHistoryPolicy(1<<30, 10_000_000, 1)
	if err != nil {
		closeAssemblyWorktreeGraphTest(t, graph)
		t.Fatal(err)
	}
	identity, err := worktree.NewRepositoryIdentity(repository, filepath.Join(repository, ".git"))
	if err != nil {
		closeAssemblyWorktreeGraphTest(t, graph)
		t.Fatal(err)
	}
	template := worktree.AcquireRequest{RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "subagent"}
	tasks, err := newAssemblySubagents(context.Background(), assemblySubagentRequest{
		Paths: RuntimePaths{
			ProjectRoot: repository, UserConfigRoot: canonicalAssemblyWorktreeTestDir(t, "user-config"),
			UserDataRoot: canonicalAssemblyWorktreeTestDir(t, "user-data"), UserCacheRoot: canonicalAssemblyWorktreeTestDir(t, "user-cache"),
		},
		Config: configValue, Provider: providerScript, Registry: registry, Executor: executor, ResultFactory: resultFactory,
		Authorizer:     &permission.Authorizer{Session: permission.NewSession(), Redact: redactor.Text},
		SessionContext: worktreeE2EStableContext{}, ContextManager: contextManager,
		RequestBudgeter: contextmgr.NewRequestBudgeter(), ContextPolicy: contextPolicy,
		Hooks: hook.Noop(), RuntimeRedactor: redactor, CleanupTimeout: 2 * time.Second,
		LifecycleDiagnostics: graphOptions.HookEngine.Diagnostics, Worktrees: graph, WorktreeTemplate: template,
	})
	if err != nil {
		closeAssemblyWorktreeGraphTest(t, graph)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tasks.shutdown(ctx); err != nil {
			t.Errorf("shutdown subagents: %v", err)
		}
		if err := tasks.closeResultInbox(ctx); err != nil {
			t.Errorf("close result inbox: %v", err)
		}
		closeAssemblyWorktreeGraphTest(t, graph)
	})

	parent := subagent.ParentRef{ConversationID: "worktree-e2e-parent"}
	submitContext, err := orchestrator.WithParentRuntimeSnapshot(context.Background(), parent, orchestrator.ParentRuntimeSnapshot{
		Model: "model-e2e", PermissionMode: permission.ModePermissive, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(submitContext, 15*time.Second)
	defer cancel()
	alpha, err := tasks.tasks.Submit(ctx, worktreeE2ESubmitInput(parent, "content-alpha"))
	if err != nil {
		t.Fatal(err)
	}
	beta, err := tasks.tasks.Submit(ctx, worktreeE2ESubmitInput(parent, "content-beta"))
	if err != nil {
		t.Fatal(err)
	}
	alphaCompletion, betaCompletion := worktreeE2EAwaitBoth(t, ctx, tasks.tasks, alpha.ID, beta.ID)

	if alphaCompletion.Status != subagent.StatusCompleted || betaCompletion.Status != subagent.StatusFailed {
		t.Fatalf("terminal statuses = alpha:%s beta:%s", alphaCompletion.Status, betaCompletion.Status)
	}
	for label, completion := range map[string]subagent.Completion{"alpha": alphaCompletion, "beta": betaCompletion} {
		if err := completion.Validate(); err != nil {
			t.Fatalf("%s completion is invalid: %v", label, err)
		}
		if completion.Workspace.Isolation != "worktree" || completion.Workspace.State != "retained" ||
			completion.Workspace.Cleanup != "retained" || !completion.Workspace.Dirty {
			t.Fatalf("%s settlement = %#v", label, completion.Workspace)
		}
	}
	if alphaCompletion.Workspace.WorkspaceID == betaCompletion.Workspace.WorkspaceID ||
		alphaCompletion.Workspace.Branch == betaCompletion.Workspace.Branch {
		t.Fatalf("parallel tasks shared identity: alpha=%#v beta=%#v", alphaCompletion.Workspace, betaCompletion.Workspace)
	}
	alphaLayout, err := worktree.ResolveManagedLayout(repository, alphaCompletion.Workspace.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	betaLayout, err := worktree.ResolveManagedLayout(repository, betaCompletion.Workspace.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if alphaLayout.WorkspaceRoot == betaLayout.WorkspaceRoot {
		t.Fatal("parallel tasks shared one worktree root")
	}
	baseOID := strings.TrimSpace(worktreeE2EGit(t, gitPath, repository, "rev-parse", "HEAD"))
	for label, candidate := range map[string]struct {
		completion subagent.Completion
		layout     worktree.ManagedLayout
	}{"alpha": {alphaCompletion, alphaLayout}, "beta": {betaCompletion, betaLayout}} {
		if candidate.completion.Workspace.BaseOID != baseOID || candidate.completion.Workspace.Branch != candidate.layout.Branch {
			t.Fatalf("%s Git identity = %#v layout=%#v HEAD=%q", label, candidate.completion.Workspace, candidate.layout, baseOID)
		}
		worktreeE2EGit(t, gitPath, repository, "show-ref", "--verify", "refs/heads/"+candidate.layout.Branch)
	}
	worktreeList := worktreeE2EGit(t, gitPath, repository, "worktree", "list", "--porcelain")
	for _, root := range []string{alphaLayout.WorkspaceRoot, betaLayout.WorkspaceRoot} {
		if !strings.Contains(worktreeList, "worktree "+root+"\n") {
			t.Fatalf("Git worktree registration omitted %q:\n%s", root, worktreeList)
		}
	}
	worktreeE2ERequireFile(t, filepath.Join(alphaLayout.WorkspaceRoot, "same.txt"), "content-alpha")
	worktreeE2ERequireFile(t, filepath.Join(betaLayout.WorkspaceRoot, "same.txt"), "content-beta")
	worktreeE2ERequireFile(t, mainFile, "main-dirty\n")
	if got := worktreeE2EGit(t, gitPath, repository, "status", "--porcelain=v1", "--untracked-files=all"); got != statusBefore {
		t.Fatalf("main worktree status changed:\n before %q\n after  %q", statusBefore, got)
	}
	if got, err := os.Getwd(); err != nil || got != processCWD {
		t.Fatalf("process cwd changed: got=%q want=%q err=%v", got, processCWD, err)
	}
	recorder.requireBindings(t, map[string]string{"content-alpha": alphaLayout.WorkspaceRoot, "content-beta": betaLayout.WorkspaceRoot})
	providerScript.requireTaskToolDefinitions(t)
	for label, completion := range map[string]subagent.Completion{"alpha": alphaCompletion, "beta": betaCompletion} {
		payload, err := json.Marshal(completion.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range []string{repository, alphaLayout.WorkspaceRoot, betaLayout.WorkspaceRoot} {
			if strings.Contains(string(payload), root) {
				t.Fatalf("%s workspace projection leaked absolute root %q: %s", label, root, payload)
			}
		}
	}
}

type worktreeE2EStableContext struct{}

func (worktreeE2EStableContext) PrepareStable(context.Context) sessionctx.PreparedContext {
	return sessionctx.PreparedContext{StableSections: []prompt.Section{{
		Name: "e2e", Priority: 1, Stable: true, Scope: prompt.ScopeGlobal, Content: "WORKTREE-E2E",
	}}}
}

type worktreeE2EToolRecorder struct {
	mu       sync.Mutex
	bindings map[string]string
	writes   map[string]string
}

func (recorder *worktreeE2EToolRecorder) bind(workspaceID, root string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.bindings == nil {
		recorder.bindings = make(map[string]string)
	}
	recorder.bindings[workspaceID] = root
}

func (recorder *worktreeE2EToolRecorder) write(content, root string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.writes == nil {
		recorder.writes = make(map[string]string)
	}
	recorder.writes[content] = root
}

func (recorder *worktreeE2EToolRecorder) requireBindings(t *testing.T, want map[string]string) {
	t.Helper()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.bindings) != 2 || len(recorder.writes) != 2 {
		t.Fatalf("tool bindings/writes = %#v / %#v", recorder.bindings, recorder.writes)
	}
	for content, root := range want {
		if recorder.writes[content] != root {
			t.Fatalf("tool write %q root=%q want=%q", content, recorder.writes[content], root)
		}
	}
}

type worktreeE2EWriteTool struct {
	factory   *tool.ResultFactory
	recorder  *worktreeE2EToolRecorder
	binding   tool.WorkspaceBinding
	workspace bool
}

func (*worktreeE2EWriteTool) Name() string        { return "Write" }
func (*worktreeE2EWriteTool) Description() string { return "write the bounded e2e fixture" }
func (*worktreeE2EWriteTool) Risk() tool.Risk     { return tool.RiskSafe }
func (*worktreeE2EWriteTool) Schema() tool.Schema {
	return tool.ObjectSchema([]string{"path", "content"}, map[string]tool.SchemaProperty{
		"path": tool.StringProperty("path"), "content": tool.StringProperty("content"),
	})
}
func (writer *worktreeE2EWriteTool) UsesSafeResultBoundary() bool {
	return writer != nil && writer.factory != nil
}
func (writer *worktreeE2EWriteTool) BindWorkspace(binding tool.WorkspaceBinding) (tool.Tool, error) {
	if writer == nil || !filepath.IsAbs(binding.Root) || !filepath.IsAbs(binding.ScratchRoot) {
		return nil, errors.New("invalid e2e workspace binding")
	}
	cloned := *writer
	cloned.binding = binding
	cloned.workspace = true
	writer.recorder.bind(binding.WorkspaceID, binding.Root)
	return &cloned, nil
}
func (writer *worktreeE2EWriteTool) Execute(_ context.Context, input tool.Input) tool.Result {
	path, _ := input.Arguments["path"].(string)
	content, _ := input.Arguments["content"].(string)
	if writer == nil || !writer.workspace || path != "same.txt" || (content != "content-alpha" && content != "content-beta") {
		result, _ := writer.factory.Build(tool.ResultFactoryInput{
			CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusError,
			Summary: "e2e write rejected", Error: &tool.Error{Code: tool.ErrInvalidArguments, Message: "e2e write rejected"},
		})
		return result
	}
	target := filepath.Join(writer.binding.Root, path)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		result, _ := writer.factory.Build(tool.ResultFactoryInput{
			CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusError,
			Summary: "e2e write failed", Error: &tool.Error{Code: tool.ErrCommandFailed, Message: "e2e write failed"},
		})
		return result
	}
	writer.recorder.write(content, writer.binding.Root)
	result, _ := writer.factory.Build(tool.ResultFactoryInput{
		CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "e2e write completed", Preview: "e2e write completed",
	})
	return result
}

type worktreeE2EProvider struct {
	redactor *redact.RuntimeRedactor
	mu       sync.Mutex
	calls    map[string]int
	first    map[string][]provider.ToolDefinition
	arrived  int
	ready    chan struct{}
}

func newWorktreeE2EProvider(redactor *redact.RuntimeRedactor) *worktreeE2EProvider {
	return &worktreeE2EProvider{redactor: redactor, calls: make(map[string]int), first: make(map[string][]provider.ToolDefinition), ready: make(chan struct{})}
}

func (*worktreeE2EProvider) Name() string { return "worktree-e2e" }

func (script *worktreeE2EProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	marker := worktreeE2EMarker(request)
	if marker == "" {
		return nil, errors.New("e2e task marker is unavailable")
	}
	script.mu.Lock()
	call := script.calls[marker]
	script.calls[marker] = call + 1
	if call == 0 {
		script.first[marker] = append([]provider.ToolDefinition(nil), request.Tools...)
		script.arrived++
		if script.arrived == 2 {
			close(script.ready)
		}
	}
	script.mu.Unlock()
	if call == 0 {
		select {
		case <-script.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		arguments := fmt.Sprintf(`{"path":"same.txt","content":%q}`, marker)
		return newWorktreeE2EStream(provider.StreamEvent{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{
			ID: "write-" + marker, Name: "Write", ArgumentsJSON: script.redactor.Redact(arguments),
		}}), nil
	}
	if marker == "content-beta" {
		return newWorktreeE2EStream(provider.StreamEvent{Type: provider.StreamEventError, Error: &diagnostics.SafeError{
			Code: "e2e_provider_failure", Source: "provider", Message: script.redactor.Redact("injected beta failure"), Recoverable: false,
		}}), nil
	}
	return newWorktreeE2EStream(
		provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: script.redactor.Redact("alpha complete")},
		provider.StreamEvent{Type: provider.StreamEventDone},
	), nil
}

func (script *worktreeE2EProvider) requireTaskToolDefinitions(t *testing.T) {
	t.Helper()
	script.mu.Lock()
	defer script.mu.Unlock()
	alpha, beta := script.first["content-alpha"], script.first["content-beta"]
	for _, definitions := range [][]provider.ToolDefinition{alpha, beta} {
		if len(definitions) != 1 || definitions[0].Name != "Write" {
			t.Fatalf("task tool definitions = %#v", definitions)
		}
	}
}

type worktreeE2EStream struct {
	events <-chan provider.StreamEvent
}

func newWorktreeE2EStream(events ...provider.StreamEvent) *worktreeE2EStream {
	stream := make(chan provider.StreamEvent, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	return &worktreeE2EStream{events: stream}
}

func (stream *worktreeE2EStream) Events() <-chan provider.StreamEvent { return stream.events }
func (*worktreeE2EStream) Close(context.Context) error                { return nil }

func worktreeE2EMarker(request provider.ChatRequest) string {
	for _, message := range request.Messages {
		for _, marker := range []string{"content-alpha", "content-beta"} {
			if strings.Contains(message.Content.Text(), marker) || strings.Contains(message.ArgumentsJSON.Text(), marker) {
				return marker
			}
		}
	}
	return ""
}

func worktreeE2ESubmitInput(parent subagent.ParentRef, marker string) subagent.SubmitInput {
	return subagent.SubmitInput{
		Task: "write " + marker + " to same.txt", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI, Parent: parent,
	}
}

func worktreeE2EAwaitBoth(t *testing.T, ctx context.Context, service subagent.Service, alpha, beta subagent.ID) (subagent.Completion, subagent.Completion) {
	t.Helper()
	type result struct {
		id         subagent.ID
		completion subagent.Completion
		err        error
	}
	results := make(chan result, 2)
	for _, id := range []subagent.ID{alpha, beta} {
		go func(taskID subagent.ID) {
			completion, err := service.Await(ctx, taskID)
			results <- result{id: taskID, completion: completion, err: err}
		}(id)
	}
	byID := make(map[subagent.ID]subagent.Completion, 2)
	for range 2 {
		select {
		case received := <-results:
			if received.err != nil {
				t.Fatalf("Await(%s): %v", received.id, received.err)
			}
			byID[received.id] = received.completion
		case <-ctx.Done():
			alphaDetail, alphaErr := service.Get(context.Background(), alpha)
			betaDetail, betaErr := service.Get(context.Background(), beta)
			t.Fatalf("timed out awaiting parallel tasks: %v alpha=%#v/%v beta=%#v/%v", context.Cause(ctx), alphaDetail.Task, alphaErr, betaDetail.Task, betaErr)
		}
	}
	return byID[alpha], byID[beta]
}

func worktreeE2EConfig(t *testing.T) config.AppConfig {
	t.Helper()
	worktreeConfig := assemblyWorktreeGraphFixture(t).Config
	limits := subagent.DefaultLimits()
	limits.MaxConcurrent = 2
	disabled := false
	return config.AppConfig{
		LLM:   config.LLMConfig{Protocol: config.ProtocolOpenAI, Model: "model-e2e"},
		Agent: config.AgentConfig{MaxIterations: 3, MaxUnknownToolCalls: 2},
		Tool:  config.ToolConfig{TimeoutMS: 2_000, MaxOutputBytes: 8 << 10, InlineOutputBytes: 8 << 10},
		Context: config.ContextConfig{
			Enabled: &disabled, ToolResultThresholdChars: 4 << 10, ToolResultsThresholdChars: 8 << 10,
			ModelWindowTokens: 100_000, AutoMarginTokens: 1_000, ManualMarginTokens: 100,
			RecentKeepTokens: 100, RecentKeepMessages: 10, SummaryFailureLimit: 2, PreviewChars: 256,
		},
		Memory: config.MemoryConfig{Enabled: &disabled},
		Subagent: config.SubagentConfig{
			RoleLimits: agentrole.DefaultLimits(), Limits: limits, Worktree: worktreeConfig,
		},
	}
}

func worktreeE2ERepository(t *testing.T, gitPath string) string {
	t.Helper()
	repository := canonicalAssemblyWorktreeTestDir(t, "repository")
	worktreeE2EGit(t, gitPath, repository, "init")
	worktreeE2EGit(t, gitPath, repository, "config", "user.name", "XAgent Worktree E2E")
	worktreeE2EGit(t, gitPath, repository, "config", "user.email", "xagent-worktree-e2e@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	roleRoot := filepath.Join(repository, ".xagent", "agents")
	if err := os.MkdirAll(roleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	role := "---\nname: reviewer\ndescription: Worktree E2E reviewer\nallowed_tools: [Write]\npermission_mode: permissive\nisolation: worktree\nmax_iterations: 3\n---\nWrite the requested fixture.\n"
	if err := os.WriteFile(filepath.Join(roleRoot, "reviewer.md"), []byte(role), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "same.txt"), []byte("committed-base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktreeE2EGit(t, gitPath, repository, "add", ".gitignore", ".xagent/agents/reviewer.md", "same.txt")
	worktreeE2EGit(t, gitPath, repository, "commit", "-m", "worktree e2e fixture")
	return repository
}

func worktreeE2EGit(t *testing.T, gitPath, directory string, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, gitPath, arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %q: %v\n%s", arguments, directory, err, output)
	}
	return string(output)
}

func worktreeE2ERequireFile(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != want {
		t.Fatalf("file %q = %q err=%v, want %q", path, contents, err, want)
	}
}

var (
	_ provider.Provider       = (*worktreeE2EProvider)(nil)
	_ provider.ChatStream     = (*worktreeE2EStream)(nil)
	_ tool.SafeResultProducer = (*worktreeE2EWriteTool)(nil)
	_ tool.WorkspaceBinder    = (*worktreeE2EWriteTool)(nil)
)
