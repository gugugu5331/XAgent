package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xagent/internal/permission"
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

func TestExecutorUsesConfiguredLimits(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, 10*time.Millisecond, 12)

	if got := executor.Execute(context.Background(), Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"sleep 1"}`}); got.Status != StatusTimeout {
		t.Fatalf("expected configured timeout, got %#v", got)
	}
	if got := executor.Execute(context.Background(), Call{ID: "2", Name: "Write", ArgumentsJSON: `{"path":"long.txt","content":"abcdefghijklmnopqrstuvwxyz"}`}); got.Status != StatusSuccess {
		t.Fatalf("write failed: %#v", got)
	}
	read := executor.Execute(context.Background(), Call{ID: "3", Name: "Read", ArgumentsJSON: `{"path":"long.txt"}`})
	if !read.Truncated || !strings.Contains(read.Summary, "截断") {
		t.Fatalf("expected configured max output truncation, got %#v", read)
	}
}

func TestBashToolFailureContentIncludesRedactedStdoutAndStderr(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 4096)

	result := executor.Execute(context.Background(), Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"printf 'api_key=secret-key'; printf 'Authorization: Bearer abc123' >&2; exit 7"}`})
	if result.Status != StatusError || result.Error.Code != ErrCommandFailed {
		t.Fatalf("expected command_failed, got %#v", result)
	}
	if !strings.Contains(result.Content, "exit_code: 7") || !strings.Contains(result.Content, "stdout:") || !strings.Contains(result.Content, "stderr:") {
		t.Fatalf("failure content missing stdout/stderr summary: %q", result.Content)
	}
	if strings.Contains(result.Content, "secret-key") || strings.Contains(result.Content, "abc123") {
		t.Fatalf("failure content leaked secret: %q", result.Content)
	}
	if stdout, _ := result.Data["stdout"].(string); strings.Contains(stdout, "secret-key") {
		t.Fatalf("stdout data leaked secret: %q", stdout)
	}
	if stderr, _ := result.Data["stderr"].(string); strings.Contains(stderr, "abc123") {
		t.Fatalf("stderr data leaked secret: %q", stderr)
	}
}

func TestBashToolSuccessRedactsStdout(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 4096)

	result := executor.Execute(context.Background(), Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"printf 'api_key=secret-key'"}`})
	if result.Status != StatusSuccess {
		t.Fatalf("expected success, got %#v", result)
	}
	if strings.Contains(result.Content, "secret-key") {
		t.Fatalf("success content leaked secret: %q", result.Content)
	}
}

func TestExecutorTruncatesUTF8Safely(t *testing.T) {
	value, truncated := truncateString("你好世界", 5)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(value) {
		t.Fatalf("truncated string is invalid utf8: %q", value)
	}
}

func TestExecutorBoundsEveryModelVisibleResultField(t *testing.T) {
	const limit = 64
	executor := &Executor{MaxOutputBytes: limit}
	result := executor.truncate(Result{
		Summary: strings.Repeat("摘", 100),
		Content: strings.Repeat("内", 100),
		Data: map[string]any{
			"stdout":  strings.Repeat("出", 100),
			"stderr":  strings.Repeat("错", 100),
			"content": strings.Repeat("数", 100),
		},
		Error: &Error{Code: ErrCommandFailed, Message: strings.Repeat("误", 100)},
	})
	if !result.Truncated || !strings.Contains(result.Summary, "截断") {
		t.Fatalf("bounded result did not report truncation: %#v", result)
	}
	for name, value := range map[string]string{
		"summary": result.Summary,
		"content": result.Content,
		"error":   result.Error.Message,
	} {
		if len(value) > limit || !utf8.ValidString(value) {
			t.Fatalf("%s is not safely bounded: bytes=%d value=%q", name, len(value), value)
		}
	}
	for _, key := range []string{"stdout", "stderr", "content"} {
		value, _ := result.Data[key].(string)
		if len(value) > limit/2 || !utf8.ValidString(value) {
			t.Fatalf("data.%s is not safely bounded: bytes=%d value=%q", key, len(value), value)
		}
	}
}

func TestExecuteAuthorizedRejectsMismatchedTicket(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)
	call := Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"hello"}`}
	other := Call{ID: call.ID, Name: call.Name, ArgumentsJSON: `{"path":"other.txt","content":"hello"}`}
	ticket := mustExecutorTicket(t, context.Background(), executor, other)
	result := executor.ExecuteAuthorized(context.Background(), call, ticket)
	if result.Status != StatusDenied || result.Error == nil || result.Error.Code != ErrPermissionDenied {
		t.Fatalf("expected permission denied, got %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("mismatched grant executed write, stat err: %v", err)
	}
}

func TestExecuteAuthorizedAllowsMatchingTicket(t *testing.T) {
	root := t.TempDir()
	registry, _ := NewRegistry(root)
	executor := NewExecutor(registry, root, time.Second, 1024)
	call := Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"hello"}`}
	ticket := mustExecutorTicket(t, context.Background(), executor, call)
	result := executor.ExecuteAuthorized(context.Background(), call, ticket)
	if result.Status != StatusSuccess {
		t.Fatalf("expected authorized write success, got %#v", result)
	}
}

func TestRegistryValidateCall(t *testing.T) {
	registry := &Registry{tools: map[string]Tool{}}
	recorder := &recordingTool{name: "Recorder"}
	if err := registry.Register(recorder); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.ValidateCall(Call{Name: "Missing", ArgumentsJSON: `not json`}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown tool was not rejected before JSON parsing: %v", err)
	}
	for _, raw := range []string{"", " \n\t "} {
		validated, err := registry.ValidateCall(Call{ID: "blank", Name: "Recorder", ArgumentsJSON: raw})
		if err != nil {
			t.Fatalf("blank arguments failed: %v", err)
		}
		if validated.Tool != recorder || len(validated.Arguments) != 0 {
			t.Fatalf("blank arguments were not normalized to {}: %#v", validated)
		}
	}

	validated, err := registry.ValidateCall(Call{ID: "numbers", Name: "Recorder", ArgumentsJSON: `{"large":9007199254740993,"decimal":1.2300,"nested":{"items":[2]}}`})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := validated.Arguments["large"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("large number lost precision/type: %#v", validated.Arguments["large"])
	}
	if got, ok := validated.Arguments["decimal"].(json.Number); !ok || got.String() != "1.2300" {
		t.Fatalf("decimal lost lexical precision/type: %#v", validated.Arguments["decimal"])
	}

	for _, raw := range []string{
		`[]`, `"scalar"`, `1`, `true`, `null`, `{broken`, `{} {}`, `{} trailing`, `{"one":1}\n{"two":2}`,
	} {
		if _, err := registry.ValidateCall(Call{Name: "Recorder", ArgumentsJSON: raw}); err == nil {
			t.Fatalf("ValidateCall accepted non-object/trailing input %q", raw)
		}
	}
}

func TestExecuteValidatedAuthorizedPreservesArgumentsAndTicket(t *testing.T) {
	registry := &Registry{tools: map[string]Tool{}}
	recorder := &recordingTool{name: "Recorder", mutate: true}
	if err := registry.Register(recorder); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, t.TempDir(), time.Second, 1024)
	call := Call{ID: "call-1", Name: "Recorder", ArgumentsJSON: `{"large":9007199254740993,"decimal":1.25}`}
	validated, err := registry.ValidateCall(call)
	if err != nil {
		t.Fatal(err)
	}
	ticket := mustExecutorTicket(t, context.Background(), executor, call)
	result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if result.Status != StatusSuccess {
		t.Fatalf("validated execution failed: %#v", result)
	}
	if validated.Arguments["observed"] == true {
		t.Fatalf("tool mutated the authorization snapshot: %#v", validated.Arguments)
	}
	if _, ok := recorder.last.Arguments["large"].(json.Number); !ok {
		t.Fatalf("tool received lossy number: %#v", recorder.last.Arguments["large"])
	}

	for name, badCall := range map[string]Call{
		"tool":      {ID: call.ID, Name: "Other", ArgumentsJSON: call.ArgumentsJSON},
		"call id":   {ID: "other-call", Name: call.Name, ArgumentsJSON: call.ArgumentsJSON},
		"arguments": {ID: call.ID, Name: call.Name, ArgumentsJSON: `{"large":1,"decimal":1.25}`},
	} {
		t.Run(name, func(t *testing.T) {
			badTicket := mustRawTicket(t, executor, badCall)
			result := executor.ExecuteValidatedAuthorized(context.Background(), validated, badTicket)
			if result.Status != StatusDenied || result.Error == nil || result.Error.Code != ErrPermissionDenied {
				t.Fatalf("mismatched ticket was not rejected: %#v", result)
			}
		})
	}
	compatibilityTicket := mustExecutorTicket(t, context.Background(), executor, call)
	compatibility := executor.ExecuteAuthorized(context.Background(), call, compatibilityTicket)
	if compatibility.Status != StatusSuccess {
		t.Fatalf("legacy ExecuteAuthorized wrapper changed: %#v", compatibility)
	}
}

func mustExecutorTicket(t *testing.T, ctx context.Context, executor *Executor, call Call) permission.ExecutionTicket {
	t.Helper()
	validated, err := executor.PrepareCall(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := executor.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	executor.TicketVerifier = authority
	ticket, err := authority.Issue(call.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func mustRawTicket(t *testing.T, executor *Executor, call Call) permission.ExecutionTicket {
	t.Helper()
	arguments := map[string]any{}
	if err := json.Unmarshal([]byte(call.ArgumentsJSON), &arguments); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := permission.NewCallIdentity(permission.CallIdentityInput{ToolName: call.Name, CanonicalArguments: canonical})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	executor.TicketVerifier = authority
	ticket, err := authority.Issue(call.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestHookDeniedError(t *testing.T) {
	denied := Result{
		CallID:  "call-1",
		Name:    "Bash",
		Status:  StatusDenied,
		Content: "blocked by trusted project policy",
		Error:   &Error{Code: ErrHookDenied, Message: "blocked by trusted project policy", Recoverable: true},
	}
	encoded, err := json.Marshal(denied)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"status":"denied"`) || !strings.Contains(string(encoded), `"code":"hook_denied"`) || !strings.Contains(string(encoded), `"recoverable":true`) {
		t.Fatalf("hook deny serialization is incomplete: %s", encoded)
	}
	if !strings.Contains(string(encoded), "blocked by trusted project policy") {
		t.Fatalf("model-visible safe reason is missing: %s", encoded)
	}
	if ErrHookDenied == ErrPermissionDenied {
		t.Fatalf("hook and permission denial codes are conflated: %q", ErrHookDenied)
	}
}

func TestSchemaRawJSONIsPreserved(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"items":{"type":"array","items":{"type":"number"}},"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}},"additionalProperties":false}`)
	data, err := json.Marshal(Schema{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(raw) {
		t.Fatalf("raw schema changed:\n%s", data)
	}
}

func TestProviderDefinitionsUseRawSchema(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"integer"}}},"required":["items"],"additionalProperties":false}`)
	registry := &Registry{tools: map[string]Tool{}}
	if err := registry.Register(fakeTool{schema: Schema{Raw: raw}}); err != nil {
		t.Fatal(err)
	}

	anthropic, err := json.Marshal(registry.AnthropicDefinitions()[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if string(anthropic) != string(raw) {
		t.Fatalf("anthropic input_schema changed:\n%s", anthropic)
	}

	openai, err := json.Marshal(registry.OpenAIDefinitions()[0].Function.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if string(openai) != string(raw) {
		t.Fatalf("openai parameters changed:\n%s", openai)
	}
}

type fakeTool struct {
	schema Schema
}

func (f fakeTool) Name() string                          { return "Fake" }
func (f fakeTool) Description() string                   { return "fake tool" }
func (f fakeTool) Schema() Schema                        { return f.schema }
func (f fakeTool) Risk() Risk                            { return RiskSafe }
func (f fakeTool) Execute(context.Context, Input) Result { return Result{} }

type recordingTool struct {
	name   string
	mutate bool
	last   Input
}

func (r *recordingTool) Name() string        { return r.name }
func (r *recordingTool) Description() string { return "records input" }
func (r *recordingTool) Schema() Schema      { return Schema{Type: "object"} }
func (r *recordingTool) Risk() Risk          { return RiskDangerous }
func (r *recordingTool) Execute(_ context.Context, input Input) Result {
	r.last = input
	if r.mutate {
		input.Arguments["observed"] = true
	}
	return Success(input, "recorded", "recorded", nil)
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
