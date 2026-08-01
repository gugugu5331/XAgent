package tool

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/permission"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

func TestExecutionStateAndPolicyAreIndependent(t *testing.T) {
	if _, coupled := reflect.TypeOf(ExecutionPolicy{}).FieldByName("Risk"); coupled {
		t.Fatal("ExecutionPolicy coupled scheduling to risk classification")
	}
	if (ExecutionPolicy{}).AllowsConcurrentExecution() {
		t.Fatal("a safe risk classification would not make a zero scheduling policy concurrent")
	}
	if !(ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}).AllowsConcurrentExecution() {
		t.Fatal("a dangerous risk classification would not override an explicitly concurrent-safe policy")
	}

	for _, test := range []struct {
		name   string
		policy ExecutionPolicy
		want   bool
	}{
		{name: "neither", policy: ExecutionPolicy{}},
		{name: "read only", policy: ExecutionPolicy{ReadOnly: true}},
		{name: "concurrent safe", policy: ExecutionPolicy{ConcurrentSafe: true}},
		{name: "both", policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.policy.AllowsConcurrentExecution(); got != test.want {
				t.Fatalf("AllowsConcurrentExecution() = %v, want %v", got, test.want)
			}
		})
	}

	states := []ExecutionState{Prepared, Rejected, CancelledBeforeStart, Running, Completed, CancelledAfterStart}
	seen := make(map[ExecutionState]bool, len(states))
	for _, state := range states {
		if !state.Valid() || seen[state] {
			t.Fatalf("execution state is invalid or duplicated: %q", state)
		}
		seen[state] = true
	}
	if len(seen) != 6 {
		t.Fatalf("execution state count = %d, want 6", len(seen))
	}
	for _, state := range []ExecutionState{Prepared, Running, CancelledBeforeStart} {
		if state.CanProduceResult() {
			t.Fatalf("state %q may not produce a result", state)
		}
	}
	for _, state := range []ExecutionState{Rejected, Completed, CancelledAfterStart} {
		if !state.CanProduceResult() {
			t.Fatalf("state %q must be able to produce a result", state)
		}
	}

	resultType := reflect.TypeOf(Result{})
	for index := 0; index < resultType.NumField(); index++ {
		name := strings.ToLower(resultType.Field(index).Name)
		if strings.Contains(name, "stdout") || strings.Contains(name, "stderr") || strings.HasPrefix(name, "raw") {
			t.Fatalf("Result exposes raw output field %q", resultType.Field(index).Name)
		}
	}
}

func TestResultFactoryProducesThreeSafeViews(t *testing.T) {
	const (
		canary      = "result-runtime-canary-8f21"
		privatePath = "/private/artifacts/raw-output.txt"
		artifactID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	runtimeRedactor.RegisterSecret(privatePath)
	factory, err := NewResultFactory(runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	ref := &artifact.Ref{
		ID:        artifactID,
		Bytes:     8192,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  false,
	}
	result, err := factory.Build(ResultFactoryInput{
		CallID:           "call-17",
		Name:             "Bash",
		State:            Completed,
		Status:           StatusError,
		Summary:          "command failed: " + canary,
		Preview:          "bounded preview " + canary + " staged at " + privatePath,
		Artifact:         ref,
		Truncated:        true,
		TruncationReason: "capture limit reached after " + canary,
		Error:            &Error{Code: ErrCommandFailed, Message: "failure: " + canary, Recoverable: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The result owns an immutable copy of artifact metadata.
	ref.ID = strings.Repeat("f", len(artifactID))
	model := result.ModelContent().Text()
	user := result.UserView()
	persisted := result.PersistedContent().Text()
	meta := result.OutputMeta()
	userText := strings.Join([]string{
		user.Summary.Text(),
		user.Preview.Text(),
		user.TruncationReason.Text(),
	}, "\n")
	for name, view := range map[string]string{
		"model":     model,
		"user":      userText,
		"persisted": persisted,
	} {
		if strings.Contains(view, canary) || strings.Contains(view, privatePath) {
			t.Fatalf("%s view leaked raw output: %q", name, view)
		}
		if !strings.Contains(view, "[redacted]") {
			t.Fatalf("%s view did not retain a visible redaction marker: %q", name, view)
		}
	}
	if user.Artifact == nil || user.Artifact.ID != artifactID || user.Artifact.Bytes != 8192 {
		t.Fatalf("user view did not expose the opaque artifact ref: %#v", user.Artifact)
	}
	if meta.Artifact == nil || meta.Artifact.ID != artifactID || !meta.Truncated {
		t.Fatalf("output metadata did not preserve the opaque artifact ref: %#v", meta)
	}
	if !strings.Contains(model, artifactID) || !strings.Contains(persisted, artifactID) {
		t.Fatal("safe model and persisted views must expose the opaque artifact ref")
	}
	if strings.Contains(persisted, "bounded preview") {
		t.Fatal("persisted content retained the model/user preview")
	}
	if strings.Contains(result.Content, canary) || strings.Contains(result.Error.Message, canary) {
		t.Fatal("legacy compatibility fields retained unredacted data")
	}

	if _, err := factory.Build(ResultFactoryInput{State: CancelledBeforeStart}); err == nil {
		t.Fatal("a call cancelled before start must not produce a Result")
	}
}

func TestRegistryValidatesBeforeAuthorizationAndKeepsPolicyLocal(t *testing.T) {
	rawSchema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1}},"required":["name"],"additionalProperties":false}`)
	registered := &registryBoundaryTool{
		name:        "Remote",
		description: "registration snapshot",
		schema:      Schema{Raw: rawSchema},
		risk:        RiskDangerous,
	}
	registry := newEmptyRegistry()
	if err := registry.RegisterWithOptions(registered, RegistrationOptions{
		Policy:            ExecutionPolicy{},
		RemoteAnnotations: json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true,"destructiveHint":false}`),
	}); err != nil {
		t.Fatal(err)
	}

	registered.description = "mutated after registration"
	registered.risk = RiskSafe
	rawSchema[0] = '['
	toolView, ok := registry.Get("Remote")
	if !ok || toolView.Description() != "registration snapshot" || toolView.Risk() != RiskDangerous {
		t.Fatalf("registry returned mutable tool metadata: %#v", toolView)
	}
	firstSchema := toolView.Schema()
	firstSchema.Raw[0] = '['
	secondSchema := toolView.Schema()
	if len(secondSchema.Raw) == 0 || secondSchema.Raw[0] != '{' {
		t.Fatal("registry schema snapshot aliases caller memory")
	}
	descriptor, ok := registry.Descriptor("Remote")
	if !ok || descriptor.Policy.ReadOnly || descriptor.Policy.ConcurrentSafe {
		t.Fatalf("remote hints loosened local policy: %#v", descriptor.Policy)
	}
	descriptor.Description = "caller mutation"
	descriptor.Schema.Raw[0] = '['
	again, _ := registry.Descriptor("Remote")
	if again.Description != "registration snapshot" || again.Schema.Raw[0] != '{' {
		t.Fatal("descriptor mutation changed the registry snapshot")
	}

	if _, err := registry.ValidateCall(Call{Name: "Remote", ArgumentsJSON: `{}`}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing required argument passed schema validation: %v", err)
	}
	if _, err := registry.ValidateCall(Call{Name: "Remote", ArgumentsJSON: `{"name":"ok","extra":true}`}); err == nil || !strings.Contains(err.Error(), "not an allowed property") {
		t.Fatalf("unknown argument passed raw schema validation: %v", err)
	}
	validated, err := registry.ValidateCall(Call{Name: "Remote", ArgumentsJSON: `{"name":"ok"}`})
	if err != nil || validated.ExecutionPolicy().ReadOnly || validated.ExecutionPolicy().ConcurrentSafe {
		t.Fatalf("valid raw-schema call or local policy failed: %#v, %v", validated, err)
	}

	strict := &registryBoundaryTool{name: "LocallyReadOnly", schema: ObjectSchema(nil, nil), risk: RiskSafe}
	if err := registry.RegisterWithOptions(strict, RegistrationOptions{
		Policy:            ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true},
		RemoteAnnotations: json.RawMessage(`{"readOnlyHint":false}`),
	}); err != nil {
		t.Fatal(err)
	}
	strictDescriptor, _ := registry.Descriptor("LocallyReadOnly")
	if strictDescriptor.Policy.ReadOnly || strictDescriptor.Policy.ConcurrentSafe {
		t.Fatal("restrictive remote annotation did not tighten local policy")
	}

	builtins, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builtins.ValidateCallWithContext(Call{Name: "Write", ArgumentsJSON: `{"content":"x"}`}, ValidationContext{}); err == nil || !strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "opened root") {
		t.Fatalf("resource binding ran before schema validation: %v", err)
	}
}

func TestCanonicalArgumentsAndBindingsAreDeterministic(t *testing.T) {
	projectRoot := t.TempDir()
	opened, err := safefs.Bootstrap(projectRoot, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Root.Close(); err != nil {
			t.Errorf("close validation root: %v", err)
		}
	}()

	registry := newEmptyRegistry()
	writeSchema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`)
	if err := registry.Register(&registryBoundaryTool{name: "Write", schema: Schema{Raw: writeSchema}, risk: RiskDangerous}); err != nil {
		t.Fatal(err)
	}
	validation := ValidationContext{Root: opened.Root, ProjectRoot: projectRoot}
	first, err := registry.ValidateCallWithContext(Call{ID: "first", Name: "Write", ArgumentsJSON: ` { "path" : "target.txt", "content" : "x" } `}, validation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.ValidateCallWithContext(Call{ID: "second", Name: "Write", ArgumentsJSON: `{"content":"x","path":"target.txt"}`}, validation)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(first.CanonicalArguments()), `{"content":"x","path":"target.txt"}`; got != want || string(second.CanonicalArguments()) != want {
		t.Fatalf("canonical arguments are not stable: %q / %q", got, second.CanonicalArguments())
	}
	firstBindings := first.ResourceBindings()
	secondBindings := second.ResourceBindings()
	if len(firstBindings) != 1 || len(secondBindings) != 1 {
		t.Fatalf("file call did not bind one resource: %d / %d", len(firstBindings), len(secondBindings))
	}
	firstBinding, err := firstBindings[0].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	secondBinding, err := secondBindings[0].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstBinding, secondBinding) {
		t.Fatal("equivalent calls produced different resource bindings")
	}
	firstIdentity, err := permission.NewCallIdentity(first.IdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := permission.NewCallIdentity(second.IdentityInput())
	if err != nil || firstIdentity != secondIdentity {
		t.Fatal("equivalent calls produced different authorization identities")
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "target.txt"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	rebound, err := registry.ValidateCallWithContext(Call{Name: "Write", ArgumentsJSON: `{"path":"target.txt","content":"x"}`}, validation)
	if err != nil {
		t.Fatal(err)
	}
	reboundIdentity, err := permission.NewCallIdentity(rebound.IdentityInput())
	if err != nil || reboundIdentity == firstIdentity {
		t.Fatal("resource replacement did not change the bound call identity")
	}

	targetDigest := sha256.Sum256([]byte("final-mcp-server-config"))
	mcpSchema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"},"label":{"type":"string"}},"required":["count","label"],"additionalProperties":false}`)
	if err := registry.RegisterWithOptions(&registryBoundaryTool{name: "mcp__server__remote", schema: Schema{Raw: mcpSchema}, risk: RiskDangerous}, RegistrationOptions{
		TargetDigest:      &targetDigest,
		RemoteAnnotations: json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	mcpFirst, err := registry.ValidateCall(Call{Name: "mcp__server__remote", ArgumentsJSON: `{"label":"same","count":2}`})
	if err != nil {
		t.Fatal(err)
	}
	mcpSecond, err := registry.ValidateCall(Call{Name: "mcp__server__remote", ArgumentsJSON: `{"count":2,"label":"same"}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(mcpFirst.CanonicalArguments()) != string(mcpSecond.CanonicalArguments()) {
		t.Fatal("MCP canonical arguments depend on input key order")
	}
	if digest := mcpFirst.TargetDigest(); digest == nil || *digest != targetDigest {
		t.Fatalf("MCP target digest was not frozen into validation: %#v", digest)
	}
	mcpIdentity, err := permission.NewCallIdentity(mcpFirst.IdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	mcpIdentityAgain, err := permission.NewCallIdentity(mcpSecond.IdentityInput())
	if err != nil || mcpIdentity != mcpIdentityAgain {
		t.Fatal("equivalent MCP calls produced different identities")
	}
}

func TestExecutorConsumesTicketAtStartBoundary(t *testing.T) {
	registry := newEmptyRegistry()
	recorder := &startBoundaryTool{name: "Recorder"}
	if err := registry.Register(recorder); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, t.TempDir(), time.Second, 1024)
	call := Call{ID: "start-once", Name: "Recorder", ArgumentsJSON: `{}`}
	validated, err := executor.PrepareCall(context.Background(), call)
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

	if result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket); result.Status != StatusSuccess {
		t.Fatalf("authorized call did not start: %#v", result)
	}
	if result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket); result.Status != StatusDenied {
		t.Fatalf("consumed ticket was reusable: %#v", result)
	}
	if calls := recorder.calls.Load(); calls != 1 {
		t.Fatalf("tool crossed the start boundary %d times", calls)
	}
}

func TestResourceReplacementInvalidatesTicket(t *testing.T) {
	projectRoot := t.TempDir()
	registry, err := NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, projectRoot, time.Second, 1024)
	call := Call{ID: "replace", Name: "Write", ArgumentsJSON: `{"path":"target.txt","content":"authorized"}`}
	validated, err := executor.PrepareCall(context.Background(), call)
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
	if err := os.WriteFile(filepath.Join(projectRoot, "target.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if result.Status != StatusDenied {
		t.Fatalf("stale resource ticket was accepted: %#v", result)
	}
	content, err := os.ReadFile(filepath.Join(projectRoot, "target.txt"))
	if err != nil || string(content) != "replacement" {
		t.Fatalf("stale ticket changed replacement: %q, %v", content, err)
	}
}

func TestCancelBeforeStartHasNoResultOrSideEffect(t *testing.T) {
	registry := newEmptyRegistry()
	recorder := &startBoundaryTool{name: "Cancelable"}
	if err := registry.Register(recorder); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, t.TempDir(), time.Second, 1024)
	call := Call{ID: "cancel-before-start", Name: "Cancelable", ArgumentsJSON: `{}`}
	validated, err := executor.PrepareCall(context.Background(), call)
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
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if result := executor.ExecuteValidatedAuthorized(canceled, validated, ticket); !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("cancellation before start produced a result: %#v", result)
	}
	if calls := recorder.calls.Load(); calls != 0 {
		t.Fatalf("cancellation before start executed tool %d times", calls)
	}
	if result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket); result.Status != StatusSuccess {
		t.Fatalf("pre-start cancellation consumed the ticket: %#v", result)
	}
}

type registryBoundaryTool struct {
	name        string
	description string
	schema      Schema
	risk        Risk
}

func (t *registryBoundaryTool) Name() string { return t.name }

func (t *registryBoundaryTool) Description() string { return t.description }

func (t *registryBoundaryTool) Schema() Schema { return t.schema }

func (t *registryBoundaryTool) Risk() Risk { return t.risk }

func (*registryBoundaryTool) Execute(context.Context, Input) Result { return Result{} }

type startBoundaryTool struct {
	name  string
	calls atomic.Int32
}

func (t *startBoundaryTool) Name() string { return t.name }

func (*startBoundaryTool) Description() string { return "start boundary fixture" }

func (*startBoundaryTool) Schema() Schema { return ObjectSchema(nil, nil) }

func (*startBoundaryTool) Risk() Risk { return RiskDangerous }

func (t *startBoundaryTool) Execute(_ context.Context, input Input) Result {
	t.calls.Add(1)
	return Success(input, "started", "", nil)
}
