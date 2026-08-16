package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"xagent/internal/app"
	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/mcpclient"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/safefs"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

func TestAssemblyPipelineHasSingleOwnershipRegistry(t *testing.T) {
	t.Run("six fixed stages share one sealed registry before runtime publication", func(t *testing.T) {
		var stages []assemblyStage
		var closeOrder []assemblyStage
		factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
			if ctx == nil || state == nil || register == nil {
				t.Fatal("assembly stage received an incomplete build scope")
			}
			stages = append(stages, stage)
			return register(func(context.Context) error {
				closeOrder = append(closeOrder, stage)
				return nil
			})
		})

		built, err := (assembly{factories: factories}).buildCandidate(context.Background())
		if err != nil || built == nil {
			t.Fatal("complete assembly pipeline did not publish one runtime")
		}
		wantStages := []assemblyStage{
			assemblyStageConfig,
			assemblyStageSecurity,
			assemblyStageExecution,
			assemblyStageAdapters,
			assemblyStageOrchestration,
			assemblyStageUI,
		}
		if !reflect.DeepEqual(stages, wantStages) {
			t.Fatalf("assembly stage order = %v", stages)
		}
		if built.owners == nil || built.owners.registrationCount() != assemblyStageCount {
			t.Fatal("runtime did not retain one complete ownership registry")
		}
		built.owners.mu.Lock()
		registeredStages := make([]assemblyStage, 0, len(built.owners.records))
		for _, record := range built.owners.records {
			registeredStages = append(registeredStages, record.stage)
		}
		built.owners.mu.Unlock()
		if !reflect.DeepEqual(registeredStages, wantStages) {
			t.Fatalf("assembly owner stage order = %v, want %v", registeredStages, wantStages)
		}
		for index, complete := range built.completed {
			if !complete {
				t.Fatalf("assembly stage %d was not complete at publication", index)
			}
		}
		if len(closeOrder) != 0 {
			t.Fatal("assembly build closed an owner before runtime publication")
		}
		if err := built.owners.register(assemblyStageUI, func(context.Context) error { return nil }); err == nil {
			t.Fatal("published runtime retained a mutable ownership registry")
		}
		if err := built.owners.closeAll(); err != nil {
			t.Fatal("close complete assembly runtime failed")
		}
		if err := built.owners.closeAll(); err != nil {
			t.Fatal("repeated close changed complete assembly result")
		}
		wantCloseOrder := []assemblyStage{
			assemblyStageUI,
			assemblyStageOrchestration,
			assemblyStageAdapters,
			assemblyStageExecution,
			assemblyStageSecurity,
			assemblyStageConfig,
		}
		if !reflect.DeepEqual(closeOrder, wantCloseOrder) {
			t.Fatalf("assembly close order = %v, want %v", closeOrder, wantCloseOrder)
		}
	})

	t.Run("failure and cancellation never publish a partial runtime", func(t *testing.T) {
		failureCanary := "private-stage-failure"
		var called []assemblyStage
		var failedRegister assemblyOwnerRegistrar
		factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			called = append(called, stage)
			failedRegister = register
			if stage == assemblyStageExecution {
				return errors.New(failureCanary)
			}
			return register(func(context.Context) error { return nil })
		})
		built, err := (assembly{factories: factories}).buildCandidate(context.Background())
		if err == nil || built != nil || strings.Contains(err.Error(), failureCanary) {
			t.Fatal("failed assembly stage published a runtime or leaked its error")
		}
		if !reflect.DeepEqual(called, []assemblyStage{assemblyStageConfig, assemblyStageSecurity, assemblyStageExecution}) {
			t.Fatalf("assembly continued after failure: %v", called)
		}
		if failedRegister == nil || failedRegister(func(context.Context) error { return nil }) == nil {
			t.Fatal("failed assembly left its stage registration scope active")
		}

		ctx, cancel := context.WithCancel(context.Background())
		called = nil
		cancelFactories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			called = append(called, stage)
			if err := register(func(context.Context) error { return nil }); err != nil {
				return err
			}
			cancel()
			return nil
		})
		built, err = (assembly{factories: cancelFactories}).buildCandidate(ctx)
		if err == nil || built != nil || !reflect.DeepEqual(called, []assemblyStage{assemblyStageConfig}) {
			t.Fatal("canceled assembly entered a later stage or published a runtime")
		}
	})

	t.Run("cancellation inside final ui stage prevents publication", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var called []assemblyStage
		var finalRegister assemblyOwnerRegistrar
		factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			called = append(called, stage)
			if err := register(func(context.Context) error { return nil }); err != nil {
				return err
			}
			if stage == assemblyStageUI {
				finalRegister = register
				cancel()
			}
			return nil
		})
		built, err := (assembly{factories: factories}).buildCandidate(ctx)
		if err == nil || built != nil || len(called) != assemblyStageCount {
			t.Fatal("final-stage cancellation published a runtime")
		}
		if finalRegister == nil || finalRegister(func(context.Context) error { return nil }) == nil {
			t.Fatal("final-stage registration scope remained active")
		}
	})

	t.Run("escaped stage registrar cannot satisfy a later stage", func(t *testing.T) {
		var escaped assemblyOwnerRegistrar
		var called []assemblyStage
		factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			called = append(called, stage)
			if stage == assemblyStageConfig {
				escaped = register
				return register(func(context.Context) error { return nil })
			}
			if stage == assemblyStageSecurity {
				if err := escaped(func(context.Context) error { return nil }); err == nil {
					t.Fatal("completed stage accepted a late owner")
				}
				return nil
			}
			return register(func(context.Context) error { return nil })
		})
		built, err := (assembly{factories: factories}).buildCandidate(context.Background())
		if err == nil || built != nil {
			t.Fatal("late owner registration did not fail the candidate build")
		}
		if !reflect.DeepEqual(called, []assemblyStage{assemblyStageConfig, assemblyStageSecurity}) {
			t.Fatalf("assembly continued after escaped registration: %v", called)
		}
	})

	t.Run("incomplete pipeline fails before builder effects", func(t *testing.T) {
		calls := 0
		factories := assemblyFactoriesForTest(func(_ int, _ assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			calls++
			return register(func(context.Context) error { return nil })
		})
		factories.ui = nil
		built, err := (assembly{factories: factories}).buildCandidate(context.Background())
		if err == nil || built != nil || calls != 0 {
			t.Fatal("incomplete pipeline entered a builder or published a runtime")
		}
	})

	t.Run("candidate remains outside the legacy production entry", func(t *testing.T) {
		assertLegacyProductionEntryUnchanged(t)
		assertAssemblyCandidateHasNoOtherProductionReference(t)
		assertAssemblyUIHasNoOtherProductionReference(t)
	})

	t.Run("ownership registry cannot resolve domain services", func(t *testing.T) {
		assertOwnershipRegistryCapabilityShape(t)
		assertAssemblyBuilderCapabilityShape(t)
	})
}

func TestAssemblyPreservesAppAndTUIBoundaries(t *testing.T) {
	t.Run("fixed UI stage exposes only the approved narrow graph", func(t *testing.T) {
		assertAssemblyUITypeShape(t)
		assertAssemblyUIConstructorShape(t)

		store := &assemblyUITestConversationStore{}
		artifacts := &assemblyUITestArtifactStore{}
		orchestratorValue := orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{})
		state := &assemblyBuildState{
			security:      &assemblySecurity{artifacts: artifacts},
			adapters:      &assemblyAdapters{conversations: store, hooks: &hook.Engine{}},
			orchestration: &assemblyOrchestration{orchestrator: orchestratorValue},
		}
		registered := 0
		var close ownerClose
		stage := newAssemblyUIStage()
		if err := stage(context.Background(), state, func(owner ownerClose) error {
			registered++
			close = owner
			return nil
		}); err != nil {
			t.Fatalf("fixed UI stage failed: %v", err)
		}
		if state.ui == nil || registered != 1 || close == nil {
			t.Fatalf("UI candidate/owner = %#v/%d/%v, want candidate and one owner", state.ui, registered, close != nil)
		}
		services := state.ui.appServices
		assertAssemblyAppBoundaryInstances(t, services, store, orchestratorValue, artifacts, state.adapters.hooks)
		if state.ui.viewModel.Screen() != tui.ScreenList || state.ui.viewModel.Lines() != nil {
			t.Fatalf("initial TUI snapshot is not a safe empty ViewModel: %#v", state.ui.viewModel)
		}
		if state.ui.intentSink == nil {
			t.Fatal("TUI candidate did not receive an intent sink")
		}
		intentCanary := "private-unpublished-input"
		intent := tui.NewIntent(tui.IntentSubmit, intentCanary, "opaque-session")
		if err := state.ui.intentSink.HandleIntent(intent); err == nil || strings.Contains(err.Error(), intentCanary) ||
			strings.Contains(err.Error(), "Store") || strings.Contains(err.Error(), "Orchestrator") {
			t.Fatalf("unpublished intent sink did not fail closed safely: %v", err)
		}
		assertAssemblyCapabilityFreeType(t, reflect.TypeOf(state.ui.viewModel), map[reflect.Type]bool{})
	})

	t.Run("missing or canceled UI stage never leaves a candidate", func(t *testing.T) {
		missing := &assemblyBuildState{}
		if err := newAssemblyUIStage()(context.Background(), missing, func(ownerClose) error { return nil }); err == nil || missing.ui != nil {
			t.Fatalf("missing UI dependencies produced a candidate: err=%v ui=%#v", err, missing.ui)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		canceled := &assemblyBuildState{
			security:      &assemblySecurity{artifacts: &assemblyUITestArtifactStore{}},
			adapters:      &assemblyAdapters{conversations: &assemblyUITestConversationStore{}, hooks: &hook.Engine{}},
			orchestration: &assemblyOrchestration{orchestrator: orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{})},
		}
		registered := 0
		if err := newAssemblyUIStage()(ctx, canceled, func(ownerClose) error { registered++; return nil }); err == nil || canceled.ui != nil || registered != 0 {
			t.Fatalf("canceled UI stage left state/owner: err=%v ui=%#v owners=%d", err, canceled.ui, registered)
		}

		invalid := *newAssemblyTestUI()
		invalid.viewModel = tui.ViewModel{}
		if validAssemblyUI(&invalid) {
			t.Fatal("UI candidate accepted a ViewModel without a valid screen")
		}
		invalid = *newAssemblyTestUI()
		var typedNilSink *assemblyUITestIntentSink
		invalid.intentSink = typedNilSink
		if validAssemblyUI(&invalid) {
			t.Fatal("UI candidate accepted a typed-nil intent sink")
		}

		broadStore := &assemblyUITestConversationStore{}
		broadArtifacts := &assemblyUITestArtifactStore{}
		broadOrchestrator := orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{})
		broadHooks := &hook.Engine{}
		if validAssemblyAppServices(app.AppServices{
			Conversations: broadStore,
			Orchestrator:  broadOrchestrator,
			Artifacts:     broadArtifacts,
			Hooks:         broadHooks,
		}) {
			t.Fatal("App boundary accepted broad concrete services without narrow adapters")
		}
		var typedNilStore *assemblyUITestConversationStore
		invalid = *newAssemblyTestUI()
		invalid.appServices.Conversations = assemblyConversationAccess{service: typedNilStore}
		if validAssemblyUI(&invalid) {
			t.Fatal("UI candidate accepted a narrow adapter containing a typed-nil service")
		}
		invalid = *newAssemblyTestUI()
		var recursive assemblyConversationAccess
		recursive.service = &recursive
		invalid.appServices.Conversations = recursive
		if validAssemblyUI(&invalid) {
			t.Fatal("UI candidate accepted a recursively wrapped App service")
		}
	})
}

func TestAssemblyCandidateIsNotPublishedEarly(t *testing.T) {
	var finalState *assemblyBuildState
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		finalState = state
		return register(func(context.Context) error { return nil })
	})
	factories.ui = func(_ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if err := register(func(context.Context) error { return nil }); err != nil {
			return err
		}
		return errors.New("ui candidate construction failed")
	}
	if runtime, err := (assembly{factories: factories}).buildCandidate(context.Background()); err == nil || runtime != nil || finalState == nil || finalState.ui != nil {
		t.Fatalf("failed UI stage published a runtime/candidate: runtime=%#v err=%v state=%#v", runtime, err, finalState)
	}

	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal(err)
	}
	assertAssemblyHasNoProductionUICall(t, source)
}

func assertAssemblyUITypeShape(t *testing.T) {
	t.Helper()
	typeOfUI := reflect.TypeOf(assemblyUI{})
	want := map[string]reflect.Type{
		"appServices":    reflect.TypeOf(app.AppServices{}),
		"runtimeOptions": reflect.TypeOf(app.RuntimeOptions{}),
		"viewModel":      reflect.TypeOf(tui.ViewModel{}),
		"intentSink":     reflect.TypeOf((*assemblyTUIIntentSink)(nil)).Elem(),
		"model":          reflect.TypeOf((*app.Model)(nil)),
	}
	if typeOfUI.NumField() != len(want) {
		t.Fatalf("assemblyUI fields = %d, want %d", typeOfUI.NumField(), len(want))
	}
	for name, wantType := range want {
		field, ok := typeOfUI.FieldByName(name)
		if !ok || field.Type != wantType {
			t.Fatalf("assemblyUI.%s = %v, want %v", name, field.Type, wantType)
		}
	}
	sinkType := reflect.TypeOf((*assemblyTUIIntentSink)(nil)).Elem()
	if sinkType.NumMethod() != 1 {
		t.Fatal("intent sink boundary grew beyond one value-only method")
	}
	method := sinkType.Method(0)
	if method.Name != "HandleIntent" || method.Type.NumIn() != 1 || method.Type.In(0) != reflect.TypeOf(tui.Intent{}) ||
		method.Type.NumOut() != 1 || method.Type.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Fatalf("intent sink method = %v, want HandleIntent(tui.Intent) error", method.Type)
	}
	servicesType := reflect.TypeOf(app.AppServices{})
	if servicesType.NumField() != 4 {
		t.Fatalf("AppServices fields = %d, want four narrow capabilities", servicesType.NumField())
	}
	for _, fieldName := range []string{"Conversations", "Orchestrator", "Artifacts", "Hooks"} {
		field, _ := servicesType.FieldByName(fieldName)
		if field.Type.Kind() != reflect.Interface {
			t.Fatalf("AppServices.%s is not an interface: %v", fieldName, field.Type)
		}
	}
}

func assertAssemblyAppBoundaryInstances(
	t *testing.T,
	services app.AppServices,
	conversations app.ConversationAccess,
	orchestration app.Orchestration,
	artifacts app.ArtifactUserReader,
	hooks app.HookLifecycle,
) {
	t.Helper()
	conversationAccess, conversationOK := services.Conversations.(assemblyConversationAccess)
	orchestrationAccess, orchestrationOK := services.Orchestrator.(assemblyOrchestrationAccess)
	artifactReader, artifactOK := services.Artifacts.(assemblyArtifactUserReader)
	hookLifecycle, hookOK := services.Hooks.(assemblyHookLifecycle)
	if !conversationOK || conversationAccess.service != conversations ||
		!orchestrationOK || orchestrationAccess.service != orchestration ||
		!artifactOK || artifactReader.service != artifacts ||
		!hookOK || hookLifecycle.service != hooks {
		t.Fatal("App boundary adapters did not retain the final shared services")
	}
	if _, ok := services.Conversations.(conversation.Store); ok {
		t.Fatal("App recovered the broad Conversation Store through its narrow boundary")
	}
	if _, ok := services.Orchestrator.(*orchestrator.Orchestrator); ok {
		t.Fatal("App recovered the concrete Orchestrator through its narrow boundary")
	}
	if _, ok := services.Artifacts.(artifact.Store); ok {
		t.Fatal("App recovered the broad Artifact Store through its narrow boundary")
	}
	if _, ok := services.Hooks.(hook.Runtime); ok {
		t.Fatal("App recovered the broad Hook Runtime through its narrow boundary")
	}
	for _, boundary := range []struct {
		name     string
		value    any
		contract reflect.Type
	}{
		{name: "conversations", value: services.Conversations, contract: reflect.TypeOf((*app.ConversationAccess)(nil)).Elem()},
		{name: "orchestration", value: services.Orchestrator, contract: reflect.TypeOf((*app.Orchestration)(nil)).Elem()},
		{name: "artifacts", value: services.Artifacts, contract: reflect.TypeOf((*app.ArtifactUserReader)(nil)).Elem()},
		{name: "hooks", value: services.Hooks, contract: reflect.TypeOf((*app.HookLifecycle)(nil)).Elem()},
	} {
		if methods := reflect.TypeOf(boundary.value).NumMethod(); methods != boundary.contract.NumMethod() {
			t.Fatalf("App %s boundary exposes %d methods, want exactly %d", boundary.name, methods, boundary.contract.NumMethod())
		}
	}
}

func assertAssemblyUIConstructorShape(t *testing.T) {
	t.Helper()
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "assembly.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	imports := assemblyImportAliases(t, file)
	var stage *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "newAssemblyUIStage" {
			stage = function
			break
		}
	}
	if stage == nil || stage.Body == nil {
		t.Fatal("fixed UI stage constructor is missing")
	}
	appServicesLiterals, viewModelCalls := 0, 0
	ast.Inspect(stage.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			if selector, ok := typed.Fun.(*ast.SelectorExpr); ok {
				x := selectorExpressionPath(selector.X)
				switch importPath := imports[x]; {
				case importPath == "xagent/internal/app" && selector.Sel.Name == "New":
					t.Fatalf("UI stage calls legacy app.New")
				case importPath == "xagent/internal/tui" && (selector.Sel.Name == "Run" || selector.Sel.Name == "NewProgram"):
					t.Fatalf("UI stage starts the TUI before cutover: %s", selector.Sel.Name)
				case importPath == "xagent/internal/tui" && selector.Sel.Name == "NewStateViewModel":
					viewModelCalls++
				}
			}
		case *ast.CompositeLit:
			selector, ok := typed.Type.(*ast.SelectorExpr)
			if ok && imports[selectorExpressionPath(selector.X)] == "xagent/internal/app" && selector.Sel.Name == "AppServices" {
				appServicesLiterals++
			}
		}
		return true
	})
	if appServicesLiterals != 1 {
		t.Fatalf("UI stage constructs %d AppServices values, want one", appServicesLiterals)
	}
	if viewModelCalls != 1 {
		t.Fatalf("UI stage constructs %d ViewModels, want one", viewModelCalls)
	}
}

func assertAssemblyHasNoProductionUICall(t *testing.T, source []byte) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "assembly.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	imports := assemblyImportAliases(t, file)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		importPath := imports[selectorExpressionPath(selector.X)]
		forbidden := (importPath == "xagent/internal/app" && selector.Sel.Name == "New") ||
			(importPath == "xagent/internal/tui" && (selector.Sel.Name == "Run" || selector.Sel.Name == "NewProgram")) ||
			(importPath == "github.com/charmbracelet/bubbletea" && selector.Sel.Name == "NewProgram")
		if forbidden {
			t.Fatalf("candidate Assembly starts a production UI through %s.%s", importPath, selector.Sel.Name)
		}
		return true
	})
}

func assertAssemblyUIHasNoOtherProductionReference(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal("read command package failed")
	}
	allowed := map[string]struct{}{"assembly.go": {}, "lifecycle.go": {}}
	forbidden := map[string]struct{}{
		"assemblyUI":                  {},
		"assemblyTUIIntentSink":       {},
		"unpublishedIntentSink":       {},
		"newAssemblyUIStage":          {},
		"validAssemblyUI":             {},
		"validAssemblyAppServices":    {},
		"assemblyConversationAccess":  {},
		"assemblyOrchestrationAccess": {},
		"assemblyArtifactUserReader":  {},
		"assemblyHookLifecycle":       {},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read command production file %s: %v", name, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			t.Fatalf("parse command production file %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok {
				if _, blocked := forbidden[identifier.Name]; blocked {
					t.Fatalf("%s referenced unpublished App/TUI candidate %q", name, identifier.Name)
				}
			}
			return true
		})
	}
}

func assemblyImportAliases(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	aliases := make(map[string]string, len(file.Imports))
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		alias := importPath
		if separator := strings.LastIndexByte(alias, '/'); separator >= 0 {
			alias = alias[separator+1:]
		}
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		if alias == "." && (importPath == "xagent/internal/app" || importPath == "xagent/internal/tui" || importPath == "github.com/charmbracelet/bubbletea") {
			t.Fatalf("assembly uses an unauditable dot import for %s", importPath)
		}
		aliases[alias] = importPath
	}
	return aliases
}

func assertAssemblyCapabilityFreeType(t *testing.T, typeOf reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if typeOf == nil || seen[typeOf] {
		return
	}
	seen[typeOf] = true
	switch typeOf.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Uintptr:
		t.Fatalf("TUI ViewModel contains capability-bearing type %v", typeOf)
	case reflect.Slice, reflect.Array:
		assertAssemblyCapabilityFreeType(t, typeOf.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < typeOf.NumField(); index++ {
			assertAssemblyCapabilityFreeType(t, typeOf.Field(index).Type, seen)
		}
	}
}

type assemblyUITestConversationStore struct{}

func (*assemblyUITestConversationStore) Create(context.Context) (*conversation.Conversation, error) {
	return nil, nil
}
func (*assemblyUITestConversationStore) List(context.Context) (conversation.ListResult, error) {
	return conversation.ListResult{}, nil
}
func (*assemblyUITestConversationStore) Load(context.Context, string) (conversation.LoadResult, error) {
	return conversation.LoadResult{}, nil
}
func (*assemblyUITestConversationStore) Save(context.Context, *conversation.Conversation) (conversation.SaveResult, error) {
	return conversation.SaveResult{}, nil
}
func (*assemblyUITestConversationStore) Maintain(context.Context) (conversation.MaintenanceResult, error) {
	return conversation.MaintenanceResult{}, nil
}

type assemblyUITestArtifactStore struct{}

func (*assemblyUITestArtifactStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return nil, nil
}
func (*assemblyUITestArtifactStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, nil
}
func (*assemblyUITestArtifactStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}
func (*assemblyUITestArtifactStore) Close() error { return nil }

type assemblyUITestIntentSink struct{}

func (*assemblyUITestIntentSink) HandleIntent(tui.Intent) error { return nil }

func assemblyFactoriesForTest(build func(int, assemblyStage, context.Context, *assemblyBuildState, assemblyOwnerRegistrar) error) assemblyFactories {
	builder := func(index int, stage assemblyStage) assemblyStageBuilder {
		return func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
			return build(index, stage, ctx, state, register)
		}
	}
	uiBuilder := builder(5, assemblyStageUI)
	// Existing pre-UI pipeline tests use generic stage builders.  Give those
	// fixtures a minimal candidate marker so buildCandidate can enforce the
	// same completeness invariant as the fixed UI stage without making the
	// generic test factory a second production construction path.
	uiBuilder = func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if err := builder(5, assemblyStageUI)(ctx, state, register); err != nil {
			return err
		}
		if state != nil && state.ui == nil {
			state.ui = newAssemblyTestUI()
		}
		return nil
	}
	return assemblyFactories{
		config:        builder(0, assemblyStageConfig),
		security:      builder(1, assemblyStageSecurity),
		execution:     builder(2, assemblyStageExecution),
		adapters:      builder(3, assemblyStageAdapters),
		orchestration: builder(4, assemblyStageOrchestration),
		ui:            uiBuilder,
	}
}

func newAssemblyTestUI() *assemblyUI {
	conversations := &assemblyUITestConversationStore{}
	orchestration := orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{})
	artifacts := &assemblyUITestArtifactStore{}
	hooks := &hook.Engine{}
	return &assemblyUI{
		appServices: app.AppServices{
			Conversations: assemblyConversationAccess{service: conversations},
			Orchestrator:  assemblyOrchestrationAccess{service: orchestration},
			Artifacts:     assemblyArtifactUserReader{service: artifacts},
			Hooks:         assemblyHookLifecycle{service: hooks},
		},
		viewModel:  tui.NewStateViewModel(tui.ViewModelSpec{Screen: tui.ScreenList}),
		intentSink: unpublishedIntentSink{},
	}
}

func TestAssemblyRegistersSecretsBeforeExternalEffects(t *testing.T) {
	winner := "assembly-effective-secret-canary"
	loser := "assembly-overridden-secret-canary"
	t.Setenv("XAGENT_ASSEMBLY_WINNER", winner)
	t.Setenv("XAGENT_ASSEMBLY_LOSER", loser)
	userPath := writeAssemblyConfig(t, "user.yaml", `
llm:
  api_key: ${XAGENT_ASSEMBLY_LOSER}
`)
	projectPath := writeAssemblyConfig(t, "project.yaml", `
llm:
  api_key: ${XAGENT_ASSEMBLY_WINNER}
`)

	externalEffects := 0
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if stage == assemblyStageConfig {
			t.Fatal("generic config factory ran instead of the fixed config stage")
		}
		if stage == assemblyStageSecurity {
			externalEffects++
			if state.configuration == nil || state.configuration.redactor == nil || state.configuration.diagnostics == nil {
				t.Fatal("external stage observed incomplete configuration owners")
			}
			if state.configuration.loaded.Config.LLM.APIKey != winner {
				t.Fatal("external stage observed an unexpanded or overridden secret")
			}
			if strings.Contains(state.configuration.redactor.Text(winner), winner) {
				t.Fatal("effective secret was not registered before the first external stage")
			}
			if state.configuration.redactor.Text(loser) != loser {
				t.Fatal("overridden secret was expanded or registered")
			}
			state.configuration.diagnostics.Add(diagnostics.SanitizeInput{
				Code: "assembly-config-secret",
				Err:  errors.New(winner),
			})
			items := state.configuration.diagnostics.Snapshot().Items()
			if len(items) != 1 || strings.Contains(items[0].Diagnostic.Message.Text(), winner) {
				t.Fatal("diagnostics did not use the registered runtime redactor")
			}
		}
		return register(func(context.Context) error { return nil })
	})
	factories.config = newAssemblyConfigStage(assemblyConfigRequest{
		userPath:    userPath,
		projectPath: projectPath,
	})

	built, err := (assembly{factories: factories}).buildCandidate(context.Background())
	if err != nil || built == nil || externalEffects != 1 {
		t.Fatal("configuration owners were not complete before external assembly effects")
	}
}

func TestDiagnosticsUsesFinalResolvedLimitsOnce(t *testing.T) {
	userPath := writeAssemblyConfig(t, "user.yaml", `
diagnostics:
  max_items: 4
  max_item_bytes: 128
  max_total_bytes: 512
`)
	projectPath := writeAssemblyConfig(t, "project.yaml", `
diagnostics:
  max_items: 3
`)
	runtimeLayer := config.PartialAppConfig{Diagnostics: config.PartialDiagnosticsConfig{
		MaxItems: config.Optional[int64]{Set: true, Value: 1},
	}}

	var sink *diagnostics.Sink
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if stage == assemblyStageConfig {
			t.Fatal("generic config factory ran instead of the fixed config stage")
		}
		if stage == assemblyStageSecurity {
			if state.configuration == nil || state.configuration.loaded.Config.Diagnostics.MaxItems != 1 {
				t.Fatal("diagnostics owner did not receive the final runtime-layer limit")
			}
			sink = state.configuration.diagnostics
		}
		return register(func(context.Context) error { return nil })
	})
	factories.config = newAssemblyConfigStage(assemblyConfigRequest{
		userPath:    userPath,
		projectPath: projectPath,
		runtime:     runtimeLayer,
	})

	if built, err := (assembly{factories: factories}).buildCandidate(context.Background()); err != nil || built == nil || sink == nil {
		t.Fatal("assembly did not publish the resolved diagnostics owner")
	}
	sink.Add(diagnostics.SanitizeInput{Code: "first", Err: errors.New("safe")})
	sink.Add(diagnostics.SanitizeInput{Code: "second", Err: errors.New("safe")})
	if snapshot := sink.Snapshot(); len(snapshot.Items()) != 1 || snapshot.Dropped() != 1 {
		t.Fatal("diagnostics sink did not enforce the final resolved item limit")
	}
	assertAssemblyConfigurationOwnerConstruction(t)
}

func TestInvalidConfigHasNoExternalSideEffect(t *testing.T) {
	missingVariable := "XAGENT_T425A_MISSING_SECRET"
	t.Setenv(missingVariable, "temporary-value")
	if err := os.Unsetenv(missingVariable); err != nil {
		t.Fatal("unset missing-variable fixture failed")
	}
	testCases := []struct {
		name    string
		content string
	}{
		{name: "unknown_field", content: "unknown_private_field: invalid-config-secret-canary"},
		{name: "missing_effective_environment", content: `
llm:
  api_key: ${XAGENT_T425A_MISSING_SECRET}
`},
		{name: "numeric_hard_cap", content: `
llm:
  api_key: invalid-config-secret-canary
diagnostics:
  max_items: 0
		`},
		{name: "diagnostics_combination", content: `
diagnostics:
  max_item_bytes: 2
  max_total_bytes: 1
`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			projectPath := writeAssemblyConfig(t, "private-config-path-canary.yaml", testCase.content)
			request := assemblyConfigRequest{projectPath: projectPath}
			directRegistry := newOwnershipRegistry()
			directScope := newAssemblyOwnerScope(assemblyStageConfig, directRegistry)
			state := &assemblyBuildState{}
			directErr := newAssemblyConfigStage(request)(context.Background(), state, directScope.registrar())
			directScope.finish()
			if directErr == nil || state.configuration != nil ||
				strings.Contains(directErr.Error(), "invalid-config-secret-canary") ||
				strings.Contains(directErr.Error(), projectPath) ||
				strings.Contains(directErr.Error(), filepath.Base(projectPath)) {
				t.Fatal("invalid configuration did not return a fixed safe assembly error")
			}

			externalEffects := 0
			factories := assemblyFactoriesForTest(func(_ int, _ assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
				externalEffects++
				return register(func(context.Context) error { return nil })
			})
			factories.config = newAssemblyConfigStage(request)
			built, err := (assembly{factories: factories}).buildCandidate(context.Background())
			if err == nil || built != nil || externalEffects != 0 ||
				strings.Contains(err.Error(), "invalid-config-secret-canary") ||
				strings.Contains(err.Error(), projectPath) {
				t.Fatal("invalid configuration reached an external stage or leaked private input")
			}
		})
	}
}

func assertAssemblyConfigurationOwnerConstruction(t *testing.T) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "assembly.go", nil, 0)
	if err != nil {
		t.Fatal("parse assembly configuration stage failed")
	}
	redactorCalls := 0
	diagnosticsCalls := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		packageName, _ := selector.X.(*ast.Ident)
		if packageName == nil {
			return true
		}
		switch packageName.Name + "." + selector.Sel.Name {
		case "redact.NewRuntimeRedactor":
			redactorCalls++
		case "diagnostics.NewBoundedSink":
			diagnosticsCalls++
		}
		return true
	})
	if redactorCalls != 1 || diagnosticsCalls != 1 {
		t.Fatalf("candidate configuration owners constructed redactor=%d diagnostics=%d times", redactorCalls, diagnosticsCalls)
	}
}

func TestAssemblyKeepsRootCapabilitiesPrivate(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "project")
	legacyRoot := filepath.Join(t.TempDir(), "legacy")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	for _, path := range []string{filepath.Join(projectRoot, ".xagent"), legacyRoot, cacheRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("create assembly security root failed")
		}
	}

	var security *assemblySecurity
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		switch stage {
		case assemblyStageConfig, assemblyStageSecurity:
			t.Fatal("generic factory ran instead of a fixed assembly stage")
		case assemblyStageExecution:
			security = state.security
			if security == nil || security.roots.project == nil || security.roots.legacy == nil ||
				security.artifacts == nil || security.networkPolicy == nil || security.networkClients == nil ||
				security.processes == nil || security.protectionPlans == nil {
				t.Fatal("execution stage observed an incomplete security foundation")
			}
			if security.roots.project.Identity() == (safefs.Identity{}) || security.roots.legacy.Identity() == (safefs.Identity{}) {
				t.Fatal("security foundation did not retain opened root identities")
			}
			if reflect.DeepEqual(security.capabilities.project, safefs.Capabilities{}) ||
				reflect.DeepEqual(security.capabilities.legacy, safefs.Capabilities{}) {
				t.Fatal("assembly discarded a root capability container")
			}
		}
		return register(func(context.Context) error { return nil })
	})
	factories.config = newAssemblyConfigStage(assemblyConfigRequest{})
	factories.security = newAssemblySecurityStage(assemblySecurityRequest{paths: RuntimePaths{
		ProjectRoot:   projectRoot,
		UserDataRoot:  legacyRoot,
		UserCacheRoot: cacheRoot,
	}})

	built, err := (assembly{factories: factories}).buildCandidate(context.Background())
	if err != nil || built == nil || security == nil {
		t.Fatal("assembly did not publish a complete security foundation")
	}
	capabilityType := reflect.TypeOf(safefs.Capabilities{})
	for _, owner := range []any{security.artifacts, security.networkPolicy, security.networkClients, security.processes, security.protectionPlans, *built} {
		typeOfOwner := reflect.TypeOf(owner)
		if typeOfOwner == capabilityType || typeContainsDirectField(typeOfOwner, capabilityType) {
			t.Fatal("assembly leaked the capability container into a leaf owner or runtime")
		}
	}
	if err := built.owners.closeAll(); err != nil {
		t.Fatal("close assembly security foundation failed")
	}
	if security.roots.project.Identity() != (safefs.Identity{}) || security.roots.legacy.Identity() != (safefs.Identity{}) {
		t.Fatal("assembly registry did not own both root lifecycles")
	}
	if writer, err := security.artifacts.Begin(context.Background(), artifact.Metadata{}); err == nil {
		if writer != nil {
			_ = writer.Abort()
		}
		t.Fatal("assembly registry did not close the artifact store")
	}
	assertAssemblySecurityOwnerConstruction(t)
}

func TestAssemblyDistributesCapabilitiesWithoutEscalation(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "project")
	userConfigRoot := filepath.Join(t.TempDir(), "config")
	legacyRoot := filepath.Join(t.TempDir(), "legacy")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	for _, path := range []string{
		filepath.Join(projectRoot, ".xagent", "skills"),
		filepath.Join(userConfigRoot, "skills"),
		legacyRoot,
		cacheRoot,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("create assembly execution fixture failed")
		}
	}
	if err := os.WriteFile(filepath.Join(userConfigRoot, "permissions.yaml"), []byte("version: 1\nrules:\n  - tool: Write\n    pattern: user-rule.txt\n    match_type: exact\n    effect: deny\n"), 0o600); err != nil {
		t.Fatal("create isolated user permission fixture failed")
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	paths := RuntimePaths{
		ProjectRoot:    projectRoot,
		UserConfigRoot: userConfigRoot,
		UserDataRoot:   legacyRoot,
		UserCacheRoot:  cacheRoot,
	}
	runtimeConfig := config.PartialAppConfig{Tool: config.PartialToolConfig{
		InlineOutputBytes: config.Optional[int64]{Set: true, Value: 4},
		CaptureBytes:      config.Optional[int64]{Set: true, Value: 8},
	}}

	var execution *assemblyExecution
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		switch stage {
		case assemblyStageConfig, assemblyStageSecurity, assemblyStageExecution:
			t.Fatal("generic factory ran instead of a fixed assembly stage")
		case assemblyStageAdapters:
			execution = state.execution
		}
		return register(func(context.Context) error { return nil })
	})
	factories.config = newAssemblyConfigStage(assemblyConfigRequest{runtime: runtimeConfig})
	factories.security = newAssemblySecurityStage(assemblySecurityRequest{paths: paths})
	factories.execution = newAssemblyExecutionStage(assemblyExecutionRequest{paths: paths})

	built, err := (assembly{factories: factories}).buildCandidate(context.Background())
	if err != nil || built == nil || execution == nil {
		t.Fatal("assembly did not publish a complete execution foundation")
	}
	t.Cleanup(func() { _ = built.owners.closeAll() })

	if execution.permissions.authorizer == nil || execution.permissions.health == nil ||
		execution.permissions.authority == nil || execution.registry == nil || execution.executor == nil ||
		execution.instructions == nil || execution.skills == nil || execution.resultFactory == nil || execution.capture == nil {
		t.Fatal("execution foundation omitted a required local service")
	}
	if execution.permissions.authorizer.Health != execution.permissions.health ||
		execution.permissions.authorizer.Issuer != execution.permissions.authority ||
		execution.executor.TicketVerifier != execution.permissions.authority ||
		!reflect.DeepEqual(execution.permissions.authorizer.Writer, execution.permissions.writer) {
		t.Fatal("permission issuer, verifier, health, or protected writer was duplicated")
	}
	if len(execution.permissions.authorizer.User.Rules) != 1 ||
		execution.permissions.authorizer.User.Rules[0].Pattern != "user-rule.txt" {
		t.Fatal("permission service did not load rules from the explicit user config root")
	}
	if len(execution.permissions.authorizer.LoadErrors) != 0 {
		t.Fatal("permission authorizer retained loader errors outside the shared health state")
	}
	wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", tool.LoadSkillToolName, tool.AgentToolName}
	if !reflect.DeepEqual(execution.registry.Names(), wantTools) || execution.executor.Registry != execution.registry {
		t.Fatal("safe registry and executor did not share the fixed built-in tool set")
	}
	if execution.instructions.Loader.ProjectRoot != projectRoot ||
		execution.instructions.Loader.UserDir != userConfigRoot ||
		execution.instructions.Loader.Config.MaxFileBytes <= 0 {
		t.Fatal("instruction service did not receive the final roots and limits")
	}
	if execution.skills.Snapshot().Generation == 0 {
		t.Fatal("skill service did not publish an initial immutable snapshot")
	}

	first, err := execution.capture(context.Background(), artifact.Metadata{MediaType: "text/plain"})
	if err != nil || first == nil {
		t.Fatal("first operation capture was not created")
	}
	second, err := execution.capture(context.Background(), artifact.Metadata{MediaType: "text/plain"})
	if err != nil || second == nil || second == first {
		t.Fatal("capture closure reused an operation-local capture")
	}
	for index, capture := range []*tool.Capture{first, second} {
		written, writeErr := capture.Write([]byte("123456789"))
		if written != 8 || writeErr == nil {
			t.Fatalf("capture %d did not receive an independent eight-byte budget", index)
		}
	}
	firstResult, firstErr := first.Finish(context.Background())
	secondResult, secondErr := second.Finish(context.Background())
	if firstErr == nil || secondErr == nil || firstResult.Artifact == nil || secondResult.Artifact == nil ||
		firstResult.Artifact.ID == secondResult.Artifact.ID || firstResult.CapturedBytes != 8 || secondResult.CapturedBytes != 8 ||
		len(firstResult.Preview) != 4 || len(secondResult.Preview) != 4 {
		t.Fatal("capture closure reused a counter/writer or ignored final inline/capture limits")
	}

	ordinary := tool.Call{ID: "ordinary", Name: "Write", ArgumentsJSON: `{"path":"ordinary.txt","content":"ordinary"}`}
	ordinaryResult := executeAssemblyAuthorizedCall(t, execution, ordinary)
	if ordinaryResult.UserView().Status != tool.StatusSuccess {
		t.Fatal("ordinary capability did not reach the Write execution boundary")
	}
	protected := tool.Call{ID: "protected", Name: "Write", ArgumentsJSON: `{"path":".xagent/permissions.local.yaml","content":"forged"}`}
	protectedResult := executeAssemblyAuthorizedCall(t, execution, protected)
	if protectedResult.UserView().Status == tool.StatusSuccess {
		t.Fatal("ordinary Write escalated into the protected permission slot")
	}
	if err := execution.permissions.writer.WriteLocal(permission.Rule{
		Tool: "Write", Pattern: "approved.txt", MatchType: string(permission.MatchExact), Effect: string(permission.EffectAllow),
	}); err != nil {
		t.Fatal("protected permission writer did not receive its dedicated capability")
	}
	permissionData, err := os.ReadFile(filepath.Join(projectRoot, ".xagent", "permissions.local.yaml"))
	if err != nil || strings.Contains(string(permissionData), "forged") || !strings.Contains(string(permissionData), "approved.txt") {
		t.Fatal("protected permission file was not exclusively published by permission Writer")
	}

	assertAssemblyExecutionOwnerConstruction(t)
}

func TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts(t *testing.T) {
	mcpServer := assemblyMCPFixture(t)
	defer mcpServer.Close()
	projectRoot := filepath.Join(t.TempDir(), "project")
	hookHome := filepath.Join(t.TempDir(), "home")
	userConfigRoot := filepath.Join(hookHome, ".config", "xagent")
	legacyRoot := filepath.Join(t.TempDir(), "legacy")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	for _, path := range []string{
		filepath.Join(projectRoot, ".xagent", "skills"),
		filepath.Join(projectRoot, "sessions-final"),
		filepath.Join(userConfigRoot, "skills"),
		legacyRoot,
		cacheRoot,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("create assembly adapter fixture failed")
		}
	}
	legacySessionID := "assembly-legacy-import"
	legacyPayload := []byte(`{"private":"assembly legacy payload"}`)
	legacyExternalPath := filepath.Join(legacyRoot, "context_blobs", legacySessionID, "result.json")
	if err := os.MkdirAll(filepath.Dir(legacyExternalPath), 0o700); err != nil {
		t.Fatal("create assembly legacy artifact parent failed")
	}
	if err := os.WriteFile(legacyExternalPath, legacyPayload, 0o600); err != nil {
		t.Fatal("write assembly legacy artifact failed")
	}
	legacyRecord, err := json.Marshal(map[string]any{
		"id": legacySessionID, "title": "legacy assembly",
		"created_at": "2026-08-09T10:00:00Z", "updated_at": "2026-08-09T10:00:03Z",
		"messages": []any{
			map[string]any{"role": "tool_call", "content": "Read({})", "created_at": "2026-08-09T10:00:01Z", "tool_call_id": "call-1", "tool_name": "Read", "raw_tool_arguments": `{}`},
			map[string]any{"role": "tool_result", "content": "safe preview", "created_at": "2026-08-09T10:00:02Z", "tool_call_id": "call-1", "tool_name": "Read", "tool_result_content": "safe preview", "tool_result_status": "success", "externalized": true, "external_path": legacyExternalPath, "external_bytes": len(legacyPayload), "external_preview": "safe preview"},
		},
	})
	if err != nil {
		t.Fatal("marshal assembly legacy conversation failed")
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "sessions-final", legacySessionID+".json"), legacyRecord, 0o600); err != nil {
		t.Fatal("write assembly legacy conversation failed")
	}
	paths := RuntimePaths{
		ProjectRoot:    projectRoot,
		UserConfigRoot: userConfigRoot,
		UserDataRoot:   legacyRoot,
		UserCacheRoot:  cacheRoot,
	}

	var adapters *assemblyAdapters
	var execution *assemblyExecution
	var orchestration *assemblyOrchestration
	var security *assemblySecurity
	factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		switch stage {
		case assemblyStageConfig, assemblyStageSecurity, assemblyStageExecution, assemblyStageAdapters:
			if stage == assemblyStageSecurity {
				security = state.security
			} else {
				t.Fatal("generic factory ran instead of a fixed assembly stage")
			}
		case assemblyStageOrchestration:
			adapters = state.adapters
			execution = state.execution
		}
		return register(func(context.Context) error { return nil })
	})
	factories.config = newAssemblyConfigStage(assemblyConfigRequest{runtime: config.PartialAppConfig{
		LLM: config.PartialLLMConfig{
			Protocol: config.Optional[string]{Set: true, Value: config.ProtocolOpenAI},
			Model:    config.Optional[string]{Set: true, Value: "assembly-adapter-model"},
			BaseURL:  config.Optional[string]{Set: true, Value: "http://127.0.0.1:1/v1"},
		},
		Tool: config.PartialToolConfig{
			InlineOutputBytes: config.Optional[int64]{Set: true, Value: 8},
			CaptureBytes:      config.Optional[int64]{Set: true, Value: 64},
		},
		MCP: config.PartialMCPConfig{
			DefaultTimeoutMS:  config.Optional[int64]{Set: true, Value: 3500},
			MaxResponseBytes:  config.Optional[int64]{Set: true, Value: 2 * 1024 * 1024},
			MaxTools:          config.Optional[int64]{Set: true, Value: 3},
			MaxPages:          config.Optional[int64]{Set: true, Value: 2},
			MaxProtocolErrors: config.Optional[int64]{Set: true, Value: 2},
			Servers: map[string]config.PartialMCPServerConfig{
				"assembly-fixture": {
					Type: config.Optional[string]{Set: true, Value: config.MCPTransportHTTP},
					URL:  config.Optional[string]{Set: true, Value: mcpServer.URL},
				},
			},
		},
		Session: config.PartialSessionConfig{
			Dir:             config.Optional[string]{Set: true, Value: "sessions-final"},
			MaxRecordBytes:  config.Optional[int64]{Set: true, Value: 131072},
			MaxSessionBytes: config.Optional[int64]{Set: true, Value: 1048576},
			MaxScanFiles:    config.Optional[int64]{Set: true, Value: 17},
			MaxScanBytes:    config.Optional[int64]{Set: true, Value: 2097152},
			RetentionDays:   config.Optional[int64]{Set: true, Value: 11},
			GapReminderDays: config.Optional[int64]{Set: true, Value: 4},
		},
	}})
	securityStage := newAssemblySecurityStage(assemblySecurityRequest{paths: paths})
	factories.security = func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if err := securityStage(ctx, state, register); err != nil {
			return err
		}
		security = state.security
		return nil
	}
	factories.execution = newAssemblyExecutionStage(assemblyExecutionRequest{paths: paths})
	adapterStage := newAssemblyAdaptersStage(assemblyAdaptersRequest{
		paths:       paths,
		hookHomeDir: hookHome,
		lookupEnv:   os.LookupEnv,
		environment: []string{"PATH=" + os.Getenv("PATH")},
	})
	factories.adapters = func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		err := adapterStage(ctx, state, register)
		if err != nil {
			t.Fatalf("fixed adapter stage failed: %v", err)
		}
		return nil
	}
	orchestrationStage := newAssemblyOrchestrationStage(assemblyExecutionRequest{paths: paths})
	factories.orchestration = func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if err := orchestrationStage(ctx, state, register); err != nil {
			return err
		}
		if state.orchestration == nil {
			return errors.New("orchestration stage returned without state")
		}
		adapters = state.adapters
		execution = state.execution
		orchestration = state.orchestration
		return nil
	}
	factories.ui = newAssemblyUIStage()

	built, err := (assembly{factories: factories}).buildCandidate(context.Background())
	if err != nil || built == nil || adapters == nil || orchestration == nil {
		t.Fatalf("assembly did not publish a complete adapter/orchestration foundation: %v", err)
	}
	t.Cleanup(func() { _ = built.owners.closeAll() })
	if built.ui == nil || !validAssemblyUI(built.ui) {
		t.Fatal("UI stage did not retain a complete candidate-only graph")
	}
	if security == nil {
		t.Fatal("UI stage did not retain the final Security services")
	}
	assertAssemblyAppBoundaryInstances(t, built.ui.appServices, adapters.conversations, orchestration.orchestrator, security.artifacts, adapters.hooks)
	if adapters.provider == nil || adapters.hooks == nil || adapters.hookResults == nil || adapters.mcp == nil ||
		adapters.conversations == nil || adapters.diagnostics == nil {
		t.Fatal("adapter foundation omitted a final provider, Hook, Hook result adapter, MCP, store, or diagnostic service")
	}
	hookResult, err := adapters.hookResults.Build(tool.ResultFactoryInput{
		CallID: "hook-denied", Name: "Write", State: tool.Rejected, Status: tool.StatusDenied,
		Summary: "denied by Hook", Error: &tool.Error{Code: tool.ErrHookDenied, Message: "denied by Hook", Recoverable: true},
	})
	if err != nil || hookResult.UserView().Status != tool.StatusDenied {
		t.Fatal("synthetic Hook result adapter did not use the final ResultFactory")
	}
	if adapters.provider.Name() == "" || adapters.mcp.Snapshot().State != mcpclient.ManagerStateRunning {
		t.Fatal("provider or MCP manager was not constructed through its final runtime contract")
	}
	if orchestration.orchestrator == nil || orchestration.contextManager == nil || orchestration.sessionContext == nil ||
		orchestration.resources == nil || orchestration.commandRegistry == nil || orchestration.requestBudgeter.Validate() != nil {
		t.Fatal("orchestration stage omitted a final narrow service graph")
	}
	if orchestration.sessionContext.Context != orchestration.contextManager {
		t.Fatal("SessionContext did not use the final ContextManager instance")
	}
	if _, err := orchestration.orchestrator.BuildExecutionProfile(orchestrator.RunModeDefault, nil, 0); err != nil {
		t.Fatalf("final Orchestrator could not build a safe execution profile: %v", err)
	}
	mcpTools := adapters.mcp.Tools()
	if len(mcpTools) != 1 || mcpTools[0].Name() != "mcp__assembly-fixture__echo" {
		t.Fatalf("final MCP manager did not publish fixture tool: %#v", mcpTools)
	}
	first := executeAssemblyAuthorizedCall(t, execution, tool.Call{
		ID: "mcp-first", Name: mcpTools[0].Name(), ArgumentsJSON: `{"message":"first-mcp-canary"}`,
	})
	second := executeAssemblyAuthorizedCall(t, execution, tool.Call{
		ID: "mcp-second", Name: mcpTools[0].Name(), ArgumentsJSON: `{"message":"second-mcp-canary"}`,
	})
	firstMeta, secondMeta := first.OutputMeta(), second.OutputMeta()
	if first.Status != tool.StatusSuccess || second.Status != tool.StatusSuccess || firstMeta.CapturedBytes <= 8 ||
		secondMeta.CapturedBytes <= 8 || first.UserView().Preview.Text() != "first-mc" || second.UserView().Preview.Text() != "second-m" ||
		firstMeta.Artifact == nil || secondMeta.Artifact == nil || firstMeta.Artifact.ID == secondMeta.Artifact.ID {
		t.Fatalf("MCP result did not use independent bounded captures: first=%#v second=%#v", first, second)
	}
	conversationValue, err := adapters.conversations.Create(context.Background())
	if err != nil || conversationValue == nil {
		t.Fatal("final conversation store could not create a conversation")
	}
	if _, err := adapters.conversations.Save(context.Background(), conversationValue); err != nil {
		t.Fatal("final conversation store could not publish a v2 snapshot")
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "sessions-final", "v2", conversationValue.ID+".jsonl")); err != nil {
		t.Fatal("Conversation Store did not use the final resolved session directory")
	}
	legacyLoaded, err := adapters.conversations.Load(context.Background(), legacySessionID)
	if err != nil || !legacyLoaded.Available || legacyLoaded.Conversation == nil || len(legacyLoaded.Conversation.Messages) != 2 ||
		legacyLoaded.Conversation.Messages[1].Tool == nil || legacyLoaded.Conversation.Messages[1].Tool.Artifact == nil ||
		legacyLoaded.Conversation.Messages[1].Tool.Artifact.Bytes != int64(len(legacyPayload)) {
		t.Fatalf("final conversation store did not import legacy artifact through the narrow importer: %#v err=%v", legacyLoaded, err)
	}
	if sourceAfter, readErr := os.ReadFile(legacyExternalPath); readErr != nil || string(sourceAfter) != string(legacyPayload) {
		t.Fatal("legacy migration modified its source artifact")
	}

	built.owners.mu.Lock()
	adapterOwners := 0
	for _, record := range built.owners.records {
		if record.stage == assemblyStageAdapters {
			adapterOwners++
		}
	}
	built.owners.mu.Unlock()
	if adapterOwners != 4 {
		t.Fatalf("adapter stage registered %d owners, want provider client, Hook HTTP, Hook Engine, and MCP", adapterOwners)
	}
	if err := built.owners.closeAll(); err != nil {
		t.Fatal("close assembly adapter foundation failed")
	}
	if adapters.mcp.Snapshot().State != mcpclient.ManagerStateClosed {
		t.Fatal("assembly registry did not close the MCP manager")
	}
	assertAssemblyAdapterOwnerConstruction(t)
}

// TestAssemblyHasSingleOwnerForEveryPolicyClient is the T4.25g root gate. The
// concrete clients are intentionally private to Provider, the Hook HTTP
// runner, and the MCP HTTP transport, so this test combines the real local
// assembly fixture above with source-level ownership checks at the composition
// boundary.  The lower-level packages separately prove each owner's close
// operation; this gate proves Assembly wires exactly one close node for each
// owner and never falls back to a process-global HTTP client.
func TestAssemblyHasSingleOwnerForEveryPolicyClient(t *testing.T) {
	TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts(t)
	assertAssemblyNetworkClientOwnership(t)
}

func assertAssemblyNetworkClientOwnership(t *testing.T) {
	t.Helper()
	source := string(mustReadAssembly(t))
	markers := map[string]int{
		"netpolicy.NewPolicy(":                  1,
		"netpolicy.NewClientFactory(":           1,
		"state.security.networkClients.New(":    1,
		"providerClient.CloseIdleConnections()": 1,
		"hookHTTP.CloseIdleConnections()":       1,
		"mcpManager.Close(cleanupCtx)":          1,
		"register(closeProviderClient)":         1,
		"register(closeHookHTTP)":               1,
		"register(closeHookEngine)":             1,
		"register(closeMCP)":                    1,
		"http.DefaultClient":                    0,
		"http.DefaultTransport":                 0,
	}
	for marker, want := range markers {
		if got := strings.Count(source, marker); got != want {
			t.Fatalf("assembly network ownership marker %q count = %d, want %d", marker, got, want)
		}
	}

	// The only direct network client created by the Assembly adapters is the
	// Provider client. Hook and MCP receive the shared policy/factory and create
	// their own clients inside their respective owners.
	assertAssemblyAdapterDependencyFlow(t)
	assertAssemblyAdapterOwnerConstruction(t)

	for _, path := range []string{
		filepath.Join("..", "..", "internal", "provider", "openai.go"),
		filepath.Join("..", "..", "internal", "provider", "anthropic.go"),
		filepath.Join("..", "..", "internal", "hook", "http.go"),
		filepath.Join("..", "..", "internal", "mcpclient", "transport", "http", "transport.go"),
	} {
		component, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read network owner component %s: %v", path, err)
		}
		if strings.Contains(string(component), "http.DefaultClient") {
			t.Fatalf("network owner component %s falls back to http.DefaultClient", path)
		}
	}
}

func assemblyMCPFixture(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Fatalf("decode assembly MCP request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case "initialize":
			writeAssemblyMCPResult(t, writer, message.ID, map[string]any{
				"protocolVersion": protocol.SupportedProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "assembly-fixture"},
			})
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeAssemblyMCPResult(t, writer, message.ID, map[string]any{"tools": []any{
				map[string]any{"name": "echo", "description": "assembly fixture", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			var params protocol.CallToolRequest
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatalf("decode assembly MCP call: %v", err)
			}
			messageText, _ := params.Arguments["message"].(string)
			writeAssemblyMCPResult(t, writer, message.ID, map[string]any{"content": []any{
				map[string]any{"type": "text", "text": messageText},
			}})
		default:
			t.Fatalf("unexpected assembly MCP method %q", message.Method)
		}
	}))
}

func writeAssemblyMCPResult(t *testing.T, writer http.ResponseWriter, id any, result any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Fatalf("write assembly MCP response: %v", err)
	}
}

func executeAssemblyAuthorizedCall(t *testing.T, execution *assemblyExecution, call tool.Call) tool.Result {
	t.Helper()
	validated, err := execution.executor.PrepareCall(context.Background(), call)
	if err != nil {
		t.Fatal("prepare assembly tool call failed")
	}
	identity, err := execution.executor.CallIdentity(validated)
	if err != nil {
		t.Fatal("build assembly tool identity failed")
	}
	ticket, err := execution.permissions.authority.Issue(call.ID, identity)
	if err != nil {
		t.Fatal("issue assembly execution ticket failed")
	}
	return execution.executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
}

func assertAssemblyExecutionOwnerConstruction(t *testing.T) {
	t.Helper()
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal("read assembly execution stage failed")
	}
	checks := map[string]int{
		"tool.NewResultFactory(":                           1,
		"tool.NewSafeCandidateRegistry(":                   1,
		"tool.NewCapture(":                                 1,
		"budget.NewCounter(":                               1,
		"capabilities.project.Ordinary()":                  1,
		"capabilities.project.Protected()":                 1,
		"tool.NewExecutorWithWriteAccessAndResultFactory(": 1,
	}
	for marker, want := range checks {
		if got := strings.Count(string(source), marker); got != want {
			t.Fatalf("assembly construction marker %q count = %d, want %d", marker, got, want)
		}
	}
	if !strings.Contains(string(source), "Store:       artifactStore") ||
		strings.Contains(string(source), "Store:       state.security.artifacts") {
		t.Fatal("capture closure retained the assembly build state instead of the narrow artifact store")
	}
	capabilitiesType := reflect.TypeOf(safefs.Capabilities{})
	capabilityType := reflect.TypeOf(safefs.Capability{})
	if typeContainsDirectField(reflect.TypeOf(assemblyExecution{}), capabilitiesType) ||
		typeContainsDirectField(reflect.TypeOf(assemblyExecution{}), capabilityType) {
		t.Fatal("execution state retained a writable capability container or bearer")
	}
}

func assertAssemblyAdapterOwnerConstruction(t *testing.T) {
	t.Helper()
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal("read assembly adapter stage failed")
	}
	checks := map[string]int{
		"provider.NewWithOptions(":                         1,
		"hook.Load(":                                       1,
		"hook.NewEngine(":                                  1,
		"hook.NewSyntheticResultAdapter(":                  1,
		"mcpclient.NewManager(":                            1,
		"conversation.NewLegacyArtifactImporter(":          1,
		"conversation.NewJSONLStore(":                      1,
		"netpolicy.NewPolicy(":                             1,
		"netpolicy.NewClientFactory(":                      1,
		"Capture:           state.execution.capture":       1,
		"ResultFactory:     state.execution.resultFactory": 1,
	}
	for marker, want := range checks {
		if got := strings.Count(string(source), marker); got != want {
			t.Fatalf("assembly adapter marker %q count = %d, want %d", marker, got, want)
		}
	}
	assertAssemblyAdapterDependencyFlow(t)
	for _, forbidden := range []string{
		"provider.NewOpenAI(",
		"provider.NewAnthropic(",
		"mcpclient.NewLegacyRemoteToolAdapter(",
		"http.DefaultClient",
		"http.DefaultTransport",
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("assembly adapter stage retained forbidden fallback %q", forbidden)
		}
	}
	adapterType := reflect.TypeOf(assemblyAdapters{})
	for _, forbiddenType := range []reflect.Type{
		reflect.TypeOf((*artifact.Store)(nil)).Elem(),
		reflect.TypeOf((*conversation.LegacyArtifactImporter)(nil)).Elem(),
		reflect.TypeOf(&safefs.Root{}),
		reflect.TypeOf(safefs.Capabilities{}),
		reflect.TypeOf(safefs.Capability{}),
	} {
		if typeContainsDirectField(adapterType, forbiddenType) {
			t.Fatalf("adapter state retained forbidden capability %v", forbiddenType)
		}
	}
}

func assertAssemblyAdapterDependencyFlow(t *testing.T) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "assembly.go", nil, 0)
	if err != nil {
		t.Fatal("parse assembly adapter stage failed")
	}
	found := make(map[string]int)
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		packageName, _ := selector.X.(*ast.Ident)
		if packageName == nil {
			return true
		}
		name := packageName.Name + "." + selector.Sel.Name
		switch name {
		case "provider.NewWithOptions":
			found[name]++
			if len(call.Args) != 2 || selectorExpressionPath(call.Args[0]) != "resolved.LLM" ||
				compositeFieldPath(call.Args[1], "Endpoint") != "providerEndpoint" ||
				compositeFieldPath(call.Args[1], "Client") != "providerClient" ||
				compositeFieldPath(call.Args[1], "RuntimeRedactor") != "redactor" {
				t.Fatal("Provider did not receive only the validated endpoint, controlled client, and shared redactor")
			}
		case "hook.NewSyntheticResultAdapter":
			found[name]++
			if len(call.Args) != 1 || selectorExpressionPath(call.Args[0]) != "state.execution.resultFactory" {
				t.Fatal("synthetic Hook adapter did not receive the shared ResultFactory directly")
			}
		case "conversation.NewLegacyArtifactImporter":
			found[name]++
			if len(call.Args) != 3 || selectorExpressionPath(call.Args[0]) != "state.security.roots.legacy" ||
				selectorExpressionPath(call.Args[1]) != "paths.UserDataRoot" || selectorExpressionPath(call.Args[2]) != "legacySink" {
				t.Fatal("legacy importer input capability flow changed")
			}
		case "conversation.NewJSONLStore":
			found[name]++
			if len(call.Args) != 1 || compositeFieldPath(call.Args[0], "LegacyArtifactImporter") != "legacyImporter" ||
				compositeFieldPath(call.Args[0], "Redactor") != "redactor" {
				t.Fatal("Conversation Store did not receive the narrow importer and shared redactor")
			}
		case "mcpclient.NewManager":
			found[name]++
			if len(call.Args) != 3 || selectorExpressionPath(call.Args[0]) != "resolved.MCP" ||
				compositeFieldPath(call.Args[2], "RuntimeRedactor") != "redactor" ||
				compositeFieldPath(call.Args[2], "ResultFactory") != "state.execution.resultFactory" ||
				compositeFieldPath(call.Args[2], "Capture") != "state.execution.capture" ||
				compositeFieldPath(call.Args[2], "HTTPClientFactory") != "state.security.networkClients" ||
				compositeFieldPath(call.Args[2], "StdioPlanFactory") != "state.security.protectionPlans" ||
				compositeFieldPath(call.Args[2], "StdioRoot") != "state.security.roots.project" {
				t.Fatal("MCP Manager final dependency flow changed")
			}
		}
		return true
	})
	for _, name := range []string{
		"provider.NewWithOptions", "hook.NewSyntheticResultAdapter", "conversation.NewLegacyArtifactImporter",
		"conversation.NewJSONLStore", "mcpclient.NewManager",
	} {
		if found[name] != 1 {
			t.Fatalf("assembly adapter constructor %s count = %d, want 1", name, found[name])
		}
	}
}

func compositeFieldPath(expression ast.Expr, fieldName string) string {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, element := range literal.Elts {
		keyed, ok := element.(*ast.KeyValueExpr)
		if !ok || selectorExpressionPath(keyed.Key) != fieldName {
			continue
		}
		return selectorExpressionPath(keyed.Value)
	}
	return ""
}

func assertAssemblySecurityOwnerConstruction(t *testing.T) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "assembly.go", nil, 0)
	if err != nil {
		t.Fatal("parse assembly security stage failed")
	}
	fileStoreCalls := 0
	protectionFactoryCalls := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		packageName, _ := selector.X.(*ast.Ident)
		if packageName == nil {
			return true
		}
		switch packageName.Name + "." + selector.Sel.Name {
		case "artifact.NewFileStore":
			fileStoreCalls++
		case "proctree.NewProtectionPlanFactory":
			protectionFactoryCalls++
			if len(call.Args) != 2 {
				t.Fatal("protection plan factory input shape changed")
			}
			roots, ok := call.Args[0].(*ast.CompositeLit)
			if !ok || len(roots.Elts) != 1 || selectorExpressionPath(roots.Elts[0]) != "project.Root" {
				t.Fatal("protection plan exposed a non-project assembly root")
			}
		}
		return true
	})
	if fileStoreCalls != 1 || protectionFactoryCalls != 1 {
		t.Fatalf("security owners constructed artifact=%d protection=%d times", fileStoreCalls, protectionFactoryCalls)
	}
}

func selectorExpressionPath(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		prefix := selectorExpressionPath(value.X)
		if prefix == "" {
			return ""
		}
		return prefix + "." + value.Sel.Name
	default:
		return ""
	}
}

func typeContainsDirectField(owner reflect.Type, target reflect.Type) bool {
	if owner == nil {
		return false
	}
	if owner.Kind() == reflect.Pointer {
		owner = owner.Elem()
	}
	if owner.Kind() != reflect.Struct {
		return false
	}
	for index := 0; index < owner.NumField(); index++ {
		if owner.Field(index).Type == target {
			return true
		}
	}
	return false
}

func writeAssemblyConfig(t *testing.T, name string, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal("write assembly config fixture failed")
	}
	return path
}

func TestArtifactRootFollowsC8(t *testing.T) {
	t.Run("configured root survives strict decode merge and resolve", func(t *testing.T) {
		userRoot := filepath.Join(t.TempDir(), "user-artifacts")
		projectRoot := filepath.Join(t.TempDir(), "project-artifacts")
		runtimeRoot := filepath.Join(t.TempDir(), "runtime-artifacts")
		user := decodeArtifactRootFixture(t, userRoot)
		project := decodeArtifactRootFixture(t, projectRoot)
		runtimeLayer := config.PartialAppConfig{Artifact: config.PartialArtifactConfig{
			Root: config.Optional[string]{Set: true, Value: runtimeRoot},
		}}
		merged, err := config.MergeLayers(
			config.ConfigLayer{Source: config.SourceRuntime, Value: runtimeLayer},
			config.ConfigLayer{Source: config.SourceUser, Value: user},
			config.ConfigLayer{Source: config.SourceProject, Value: project},
		)
		if err != nil {
			t.Fatal("merge artifact root layers failed")
		}
		loaded, err := config.ResolveConfig(merged, config.LoadOptions{})
		if err != nil {
			t.Fatal("resolve configured artifact root failed")
		}
		if loaded.Config.Artifact.Root != runtimeRoot || loaded.Provenance["artifact.root"] != config.SourceRuntime {
			t.Fatal("artifact root did not follow fixed config precedence")
		}
		empty := config.MergeResult{Value: config.PartialAppConfig{Artifact: config.PartialArtifactConfig{
			Root: config.Optional[string]{Set: true, Value: ""},
		}}}
		if _, err := config.ResolveConfig(empty, config.LoadOptions{}); err == nil {
			t.Fatal("explicit empty artifact root became the default root")
		}
	})

	t.Run("default root uses stable opaque project identity", func(t *testing.T) {
		projectPath := filepath.Join(t.TempDir(), "plaintext-project-path-canary")
		if err := os.Mkdir(projectPath, 0o700); err != nil {
			t.Fatal("create named artifact project failed")
		}
		project := openArtifactProjectPath(t, projectPath)
		cacheRoot := t.TempDir()
		paths := RuntimePaths{ProjectRoot: projectPath, UserCacheRoot: cacheRoot}
		resolved, err := resolveArtifactRoot(paths, project, "")
		if err != nil {
			t.Fatal("resolve default artifact root failed")
		}
		identity, err := project.Identity().MarshalBinary()
		if err != nil {
			t.Fatal("marshal project identity failed")
		}
		digest := sha256.Sum256(identity)
		projectID := hex.EncodeToString(digest[:])
		canonicalCache, err := filepath.EvalSymlinks(cacheRoot)
		if err != nil {
			t.Fatal("canonicalize cache fixture failed")
		}
		want := filepath.Join(canonicalCache, "xagent", "artifacts", projectID)
		if resolved != want || filepath.Base(resolved) != projectID || len(projectID) != 64 {
			t.Fatalf("default artifact root = %q, want opaque stable path", resolved)
		}
		if strings.Contains(resolved, projectPath) || strings.Contains(projectID, filepath.Base(projectPath)) {
			t.Fatal("default artifact root disclosed the project path")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Fatal("default artifact root was not prepared privately")
		}

		reopened, err := safefs.Bootstrap(projectPath, safefs.Policy{})
		if err != nil {
			t.Fatal("reopen project root failed")
		}
		t.Cleanup(func() { _ = reopened.Root.Close() })
		repeated, err := resolveArtifactRoot(paths, reopened.Root, "")
		if err != nil || repeated != resolved {
			t.Fatal("same project identity produced a different artifact root")
		}

		otherPath, other := openArtifactProject(t)
		otherResolved, err := resolveArtifactRoot(RuntimePaths{ProjectRoot: otherPath, UserCacheRoot: cacheRoot}, other, "")
		if err != nil || otherResolved == resolved {
			t.Fatal("different project identities shared one default artifact root")
		}
	})

	t.Run("explicit root is private and cannot resolve into workspace", func(t *testing.T) {
		projectPath, project := openArtifactProject(t)
		paths := RuntimePaths{ProjectRoot: projectPath, UserCacheRoot: t.TempDir()}
		explicit := filepath.Join(t.TempDir(), "configured-artifacts")
		resolved, err := resolveArtifactRoot(paths, project, explicit)
		canonicalParent, canonicalErr := filepath.EvalSymlinks(filepath.Dir(explicit))
		wantExplicit := filepath.Join(canonicalParent, filepath.Base(explicit))
		if err != nil || canonicalErr != nil || resolved != wantExplicit {
			t.Fatal("safe explicit artifact root was rejected")
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Fatal("explicit artifact root was not prepared privately")
		}

		rejectedRoots := []string{
			"relative-artifacts",
			projectPath,
			filepath.Join(projectPath, "artifacts"),
			filepath.Dir(projectPath),
		}
		if runtime.GOOS != "windows" {
			publicRoot := filepath.Join(t.TempDir(), "public-artifacts")
			if err := os.Mkdir(publicRoot, 0o700); err != nil {
				t.Fatal("create public artifact fixture failed")
			}
			if err := os.Chmod(publicRoot, 0o755); err != nil {
				t.Fatal("make artifact fixture public failed")
			}
			rejectedRoots = append(rejectedRoots, publicRoot)
		}
		for _, rejected := range rejectedRoots {
			if root, err := resolveArtifactRoot(paths, project, rejected); err == nil || root != "" {
				t.Fatalf("unsafe explicit artifact root was accepted: %q", rejected)
			}
		}

		alias := filepath.Join(t.TempDir(), "workspace-alias")
		if err := os.Symlink(projectPath, alias); err == nil {
			if root, err := resolveArtifactRoot(paths, project, filepath.Join(alias, "artifacts")); err == nil || root != "" {
				t.Fatal("artifact root followed a link into the workspace")
			}
		}
	})

	t.Run("unavailable cache fails without workspace fallback", func(t *testing.T) {
		projectPath, project := openArtifactProject(t)
		cacheFile := filepath.Join(t.TempDir(), "not-a-cache-directory")
		if err := os.WriteFile(cacheFile, []byte("blocked"), 0o600); err != nil {
			t.Fatal("create unavailable cache fixture failed")
		}
		resolved, err := resolveArtifactRoot(RuntimePaths{ProjectRoot: projectPath, UserCacheRoot: cacheFile}, project, "")
		if err == nil || resolved != "" {
			t.Fatal("unavailable cache root did not fail closed")
		}
		if _, err := os.Stat(filepath.Join(projectPath, ".xagent", "artifacts")); !os.IsNotExist(err) {
			t.Fatal("artifact root fell back into the workspace")
		}
	})

	t.Run("project path and opened root must have the same identity", func(t *testing.T) {
		declaredPath, declaredRoot := openArtifactProject(t)
		actualPath, actualRoot := openArtifactProject(t)
		insideActual := filepath.Join(actualPath, "artifacts")
		resolved, err := resolveArtifactRoot(
			RuntimePaths{ProjectRoot: declaredPath, UserCacheRoot: t.TempDir()},
			actualRoot,
			insideActual,
		)
		if err == nil || resolved != "" {
			t.Fatal("mismatched project path and opened root were accepted")
		}
		if strings.Contains(err.Error(), declaredPath) || strings.Contains(err.Error(), actualPath) {
			t.Fatal("project identity rejection exposed a private path")
		}
		if _, statErr := os.Stat(insideActual); !os.IsNotExist(statErr) {
			t.Fatal("project identity mismatch created an artifact directory")
		}
		_ = declaredRoot
	})
}

func decodeArtifactRootFixture(t *testing.T, root string) config.PartialAppConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("artifact:\n  root: "+root+"\n"), 0o600); err != nil {
		t.Fatal("write artifact root config fixture failed")
	}
	partial, err := config.DecodePartial(path)
	if err != nil {
		t.Fatal("strict decode rejected artifact root")
	}
	return partial
}

func openArtifactProject(t *testing.T) (string, *safefs.Root) {
	t.Helper()
	path := t.TempDir()
	return path, openArtifactProjectPath(t, path)
}

func openArtifactProjectPath(t *testing.T, path string) *safefs.Root {
	t.Helper()
	opened, err := safefs.Bootstrap(path, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap artifact project failed")
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	return opened.Root
}
