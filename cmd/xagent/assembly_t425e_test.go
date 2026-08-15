package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/netpolicy"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
	"xagent/internal/tool"
)

// TestAssemblyInjectsNarrowOrchestratorDependencies is the T4.25e root
// acceptance test.  The orchestration candidate is intentionally inspected
// at both boundaries: the concrete assembly state may retain only the small
// service graph needed to construct the Orchestrator, and the constructor
// call must map that graph to OrchestratorOptions without forwarding the
// composition root, resolved Config, roots, capabilities, or network/store
// owners.  The latter checks are AST checks because the forbidden values are
// otherwise difficult to observe after construction (the Orchestrator keeps
// only its private fields).
func TestAssemblyInjectsNarrowOrchestratorDependencies(t *testing.T) {
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal("read assembly source: ", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "assembly.go", source, 0)
	if err != nil {
		t.Fatal("parse assembly source: ", err)
	}

	assertAssemblyOrchestrationConstructor(t, file)
	assertAssemblyOrchestrationStateShape(t)
	assertAssemblyContextPathBoundary(t)
	assertAssemblyPermissionBoundary(t)
	t.Run("runtime candidate uses the final shared services", func(t *testing.T) {
		// Reuse the production-stage fixture that already owns the local MCP,
		// Store, Capture and shutdown setup.  Its T4.25e assertions inspect the
		// constructed orchestration candidate before the UI stage can publish it.
		TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts(t)
	})
}

func assertAssemblyPermissionBoundary(t *testing.T) {
	t.Helper()
	first, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	second, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	newExecution := func() *assemblyExecution {
		health := permission.NewHealth(first)
		return &assemblyExecution{
			permissions: assemblyPermissions{
				health: health, authority: first,
				authorizer: &permission.Authorizer{Health: health, Issuer: first},
			},
			executor: &tool.Executor{TicketVerifier: first},
		}
	}
	if !validAssemblyOrchestrationPermissionBoundary(newExecution()) {
		t.Fatal("matching Assembly permission authority was rejected")
	}
	invalid := []*assemblyExecution{
		nil,
		func() *assemblyExecution { value := newExecution(); value.permissions.authorizer = nil; return value }(),
		func() *assemblyExecution { value := newExecution(); value.permissions.authority = nil; return value }(),
		func() *assemblyExecution {
			value := newExecution()
			value.permissions.authorizer.Issuer = second
			return value
		}(),
		func() *assemblyExecution {
			value := newExecution()
			value.executor.TicketVerifier = second
			return value
		}(),
		func() *assemblyExecution {
			value := newExecution()
			value.permissions.authorizer.Health = permission.NewHealth(first)
			return value
		}(),
	}
	for index, execution := range invalid {
		if validAssemblyOrchestrationPermissionBoundary(execution) {
			t.Fatalf("mismatched Assembly permission boundary %d was accepted", index)
		}
	}
}

func assertAssemblyContextPathBoundary(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	inside, err := assemblyContextPath(root, filepath.Join("nested", "..", "memory"), "test")
	if err != nil || inside != filepath.Join(root, "memory") {
		t.Fatalf("relative context path did not resolve beneath its explicit root: path=%q err=%v", inside, err)
	}
	for _, configured := range []string{
		"..",
		filepath.Join("..", "outside"),
		filepath.Join("nested", "..", "..", "outside"),
	} {
		if escaped, err := assemblyContextPath(root, configured, "test"); err == nil {
			t.Fatalf("relative context path escaped its explicit root: configured=%q resolved=%q", configured, escaped)
		}
	}
}

func assertAssemblyOrchestrationConstructor(t *testing.T, file *ast.File) {
	t.Helper()

	var stage *ast.FuncDecl
	constructorCalls := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "newAssemblyOrchestrationStage" {
			stage = function
		}
	}
	if stage == nil {
		t.Fatal("T4.25e orchestration stage constructor is missing")
	}
	if stage.Body == nil {
		t.Fatal("T4.25e orchestration stage has no implementation")
	}

	allowed := map[string]bool{
		"Provider": true, "Store": true, "Resources": true, "Thinking": true,
		"Registry": true, "Executor": true, "Authorizer": true, "ContextManager": true,
		"CleanupTimeout": true, "LifecycleDiagnostics": true,
		"ResultFactory": true, "SkillHistoryPolicy": true, "RequestBudgeter": true,
		"MaxRecordBytes": true, "MaxSessionBytes": true, "SessionContext": true,
		"Memory": true, "Diagnostics": true, "Agent": true, "SkillManager": true,
		"DefaultModel": true, "RuntimeRedactor": true, "Redact": true,
		"RedactionLookbehind": true, "Hooks": true,
	}
	sharedInstances := map[string]string{
		"Provider":             "state.adapters.provider",
		"Store":                "state.adapters.conversations",
		"Resources":            "contextServices.resources",
		"Registry":             "state.execution.registry",
		"Executor":             "state.execution.executor",
		"CleanupTimeout":       "state.configuration.cleanupTimeout",
		"LifecycleDiagnostics": "state.configuration.diagnostics",
		"Authorizer":           "state.execution.permissions.authorizer",
		"ContextManager":       "contextServices.contextManager",
		"ResultFactory":        "state.execution.resultFactory",
		"SkillHistoryPolicy":   "contextServices.skillHistoryPolicy",
		"RequestBudgeter":      "contextServices.requestBudgeter",
		"SessionContext":       "contextServices.sessionContext",
		"Memory":               "contextServices.memory",
		"Diagnostics":          "state.adapters.diagnostics",
		"SkillManager":         "state.execution.skills",
		"RuntimeRedactor":      "state.configuration.redactor",
		"Hooks":                "state.adapters.hooks",
	}
	forbidden := map[string]bool{
		"Assembly": true, "Config": true, "Capabilities": true,
		"Roots": true, "Root": true, "ArtifactStore": true,
		"NetworkPolicy": true, "NetworkClients": true, "ClientFactory": true,
		"ProtectionPlans": true, "StdioRoot": true, "Capture": true,
		"Writer": true, "Reader": true, "StoreOwner": true,
	}

	ast.Inspect(stage.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewWithOptions" || selectorExpressionPath(selector.X) != "orchestrator" {
			return true
		}
		constructorCalls++
		if len(call.Args) != 1 {
			t.Fatalf("orchestrator.NewWithOptions args = %d, want one options value", len(call.Args))
		}
		options, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("orchestrator.NewWithOptions did not receive an options literal")
		}
		for _, element := range options.Elts {
			key, ok := element.(*ast.KeyValueExpr)
			if !ok {
				t.Fatal("OrchestratorOptions contains an unkeyed field")
			}
			name := selectorExpressionPath(key.Key)
			if forbidden[name] {
				t.Fatalf("OrchestratorOptions forwarded forbidden assembly capability %q", name)
			}
			if !allowed[name] {
				t.Fatalf("OrchestratorOptions grew an unapproved dependency field %q", name)
			}
			if want, required := sharedInstances[name]; required {
				if got := selectorExpressionPath(key.Value); got != want {
					t.Fatalf("OrchestratorOptions.%s = %q, want shared instance %q", name, got, want)
				}
				delete(sharedInstances, name)
			}
		}
		return true
	})
	if constructorCalls != 1 {
		t.Fatalf("orchestrator.NewWithOptions calls in orchestration stage = %d, want one", constructorCalls)
	}
	if len(sharedInstances) != 0 {
		t.Fatalf("OrchestratorOptions omitted required shared services: %v", sharedInstances)
	}

	// The stage must construct its service graph before constructing the
	// Orchestrator.  A direct call to the constructor from another stage would
	// bypass this boundary and make the static check above meaningless.
	order := []string{"newAssemblyContextServices(", "orchestrator.NewWithOptions("}
	positions := make([]int, len(order))
	text := string(mustReadAssembly(t)) + "\n" + string(mustReadAssemblyFile(t, "assembly_context.go"))
	for index, marker := range order {
		positions[index] = strings.Index(text, marker)
		if positions[index] < 0 {
			t.Fatalf("assembly source is missing %q", marker)
		}
	}
	if positions[0] > positions[1] {
		t.Fatal("Orchestrator is constructed before Context/Session/Memory services")
	}
}

func assertAssemblyOrchestrationStateShape(t *testing.T) {
	t.Helper()
	typeOfState := reflect.TypeOf(assemblyBuildState{})
	field, ok := typeOfState.FieldByName("orchestration")
	if !ok {
		t.Fatal("assembly build state has no orchestration candidate")
	}
	if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Name() != "assemblyOrchestration" {
		t.Fatalf("assembly orchestration state field = %v, want *assemblyOrchestration", field.Type)
	}

	orchestrationType := field.Type.Elem()
	wantFields := map[string]bool{
		"orchestrator": true, "contextManager": true, "sessionContext": true,
		"memory": true, "resources": true, "requestBudgeter": true,
		"skillHistoryPolicy": true, "commandRegistry": true,
	}
	if orchestrationType.NumField() != len(wantFields) {
		t.Fatalf("assemblyOrchestration fields = %d, want exactly %d", orchestrationType.NumField(), len(wantFields))
	}
	forbidden := []reflect.Type{
		reflect.TypeOf(config.AppConfig{}),
		reflect.TypeOf(config.LoadedConfig{}),
		reflect.TypeOf(safefs.Capabilities{}),
		reflect.TypeOf(safefs.Capability{}),
		reflect.TypeOf(&safefs.Root{}),
		reflect.TypeOf((*artifact.Store)(nil)).Elem(),
		reflect.TypeOf((*netpolicy.HTTPPolicy)(nil)).Elem(),
		reflect.TypeOf((*netpolicy.ClientFactory)(nil)).Elem(),
		reflect.TypeOf((*proctree.Runner)(nil)).Elem(),
	}
	for index := 0; index < orchestrationType.NumField(); index++ {
		field := orchestrationType.Field(index)
		if !wantFields[field.Name] {
			t.Fatalf("assemblyOrchestration has unapproved field %s", field.Name)
		}
		for _, forbiddenType := range forbidden {
			if field.Type == forbiddenType {
				t.Fatalf("assemblyOrchestration retains forbidden capability field %s (%v)", field.Name, field.Type)
			}
		}
		name := strings.ToLower(field.Name)
		if strings.Contains(name, "config") || strings.Contains(name, "capabilit") ||
			strings.Contains(name, "root") || strings.Contains(name, "artifact") ||
			strings.Contains(name, "network") || strings.Contains(name, "capture") ||
			strings.Contains(name, "writer") || strings.Contains(name, "reader") {
			t.Fatalf("assemblyOrchestration field %s exposes a broad capability", field.Name)
		}
	}
}

func mustReadAssembly(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal("read assembly source: ", err)
	}
	return data
}

func mustReadAssemblyFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal("read assembly source ", name, ": ", err)
	}
	return data
}
