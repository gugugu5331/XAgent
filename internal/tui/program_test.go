package tui

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/redact"
)

type finalModel struct{ value string }

func (m finalModel) Init() tea.Cmd                       { return nil }
func (m finalModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m finalModel) View() string                        { return m.value }

type recordingProgram struct {
	model tea.Model
	err   error
}

func (p recordingProgram) Run() (tea.Model, error) { return p.model, p.err }

type resizeTraceModel struct {
	events []string
}

func (m resizeTraceModel) Init() tea.Cmd { return nil }
func (m resizeTraceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.events = append(m.events, fmt.Sprintf("size:%dx%d", value.Width, value.Height))
	case tea.KeyMsg:
		m.events = append(m.events, "key:"+value.String())
	default:
		m.events = append(m.events, fmt.Sprintf("%T", msg))
	}
	return m, nil
}
func (m resizeTraceModel) View() string { return strings.Join(m.events, "|") }

func TestRunReturnsFinalModel(t *testing.T) {
	want := finalModel{value: "final"}

	t.Run("normal", func(t *testing.T) {
		got, err := runProgram(recordingProgram{model: want})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("final model = %#v, want %#v", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		wantErr := errors.New("renderer failed")
		got, err := runProgram(recordingProgram{model: want, err: wantErr})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if got != want {
			t.Fatalf("final model = %#v, want %#v", got, want)
		}
	})
}

func TestWindowSizePrecedesInputDispatch(t *testing.T) {
	final := finalModel{value: "unwrapped"}
	if got := unwrapResponsiveModel(newResponsiveModel(final)); got != final {
		t.Fatalf("responsive runner hid final application model: %#v", got)
	}

	var model tea.Model = newResponsiveModel(resizeTraceModel{})
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := model.View(); got != "key:enter" {
		t.Fatalf("program forged a size before the terminal reported one: %q", got)
	}

	model, _ = model.Update(tea.WindowSizeMsg{Width: 51, Height: 13})
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	want := "key:enter|size:51x13|size:51x13|key:tab|size:51x13"
	if got := model.View(); got != want {
		t.Fatalf("real size did not bracket input dispatch:\n got: %q\nwant: %q", got, want)
	}

	model, _ = model.Update(tea.WindowSizeMsg{Width: 137, Height: 41})
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.View()
	if !strings.HasSuffix(got, "size:137x41|size:137x41|key:esc|size:137x41") || strings.Contains(got, "80x24") {
		t.Fatalf("program did not retain the latest real terminal size: %q", got)
	}
}

func TestNoHookVisibleCompatibility(t *testing.T) {
	want := finalModel{value: "conversation\n[DEFAULT] provider=fake model=fake\ninput"}
	var baseline string
	for _, name := range []string{"nil", "noop", "empty-engine"} {
		got, err := runProgram(recordingProgram{model: want})
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		visible := got.View()
		if visible != want.View() {
			t.Fatalf("%s final view = %q, want %q", name, visible, want.View())
		}
		if baseline == "" {
			baseline = visible
		} else if visible != baseline {
			t.Fatalf("%s changed final TUI view: legacy=%q actual=%q", name, baseline, visible)
		}
	}
}

func TestTUIHasNoDomainServiceDependency(t *testing.T) {
	assertTUIProductionImportsAreCapabilityFree(t)

	secret := "t43-view-canary"
	lines := []redact.SafeText{redact.NewRuntimeRedactor().Redact("visible " + secret)}
	view := NewViewModel(7, ScreenChat, lines)
	lines[0] = redact.SafeText{}

	first := view.Lines()
	if len(first) != 1 || !strings.Contains(first[0].Text(), secret) {
		t.Fatalf("ViewModel retained caller-owned slice: %#v", first)
	}
	first[0] = redact.SafeText{}
	second := view.Lines()
	if len(second) != 1 || !strings.Contains(second[0].Text(), secret) {
		t.Fatalf("ViewModel exposed mutable backing storage: %#v", second)
	}
	if view.Generation() != 7 || view.Screen() != ScreenChat {
		t.Fatalf("ViewModel identity = (%d, %q), want (7, %q)", view.Generation(), view.Screen(), ScreenChat)
	}

	intent := NewIntent(IntentSubmit, "user input", "session-id")
	if intent.Kind() != IntentSubmit || intent.Value() != "user input" || intent.TargetID() != "session-id" {
		t.Fatalf("Intent projection changed: kind=%q value=%q target=%q", intent.Kind(), intent.Value(), intent.TargetID())
	}

	for _, value := range []any{view, intent} {
		assertCapabilityFreeValue(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}
}

func TestViewModelIsSafeAndImmutable(t *testing.T) {
	assertTUIProductionImportsAreCapabilityFree(t)

	redactor := redact.NewRuntimeRedactor()
	visible := redactor.Redact("safe view data")
	lines := []redact.SafeText{visible}
	skills := []string{"review"}
	messages := []redact.SafeText{visible}
	transientIDs := []string{"transient-12"}
	entries := []SessionListEntrySpec{{
		ID:                 "session-12",
		Title:              visible,
		UpdatedAtUnixMilli: 1_775_212_800_000,
		MessageCount:       3,
		Available:          true,
		Selectable:         true,
		RecoveryStatus:     "partial",
		RecoveryNotice:     visible,
	}}

	view := NewStateViewModel(ViewModelSpec{
		Generation:      12,
		RuntimeSequence: 15,
		Screen:          ScreenChat,
		Lines:           lines,
		Conversation: ConversationViewSpec{
			ActiveID: "session-12", Mode: "plan", Skills: skills,
			Input: visible, Messages: messages, Notice: visible,
		},
		Request: RequestViewSpec{
			Duration: 2_500_000_000, InputTokens: 13, OutputTokens: 8,
			CacheCreated: 5, CacheRead: 3, StopReason: "end_turn",
			LastError: SafeErrorViewSpec{
				Present: true, Code: "safe_error", Source: "app", Message: visible, Recoverable: true,
			},
			Confirmation: ConfirmationViewSpec{
				Present: true, CallID: "call-12", Name: "Bash", Prompt: visible,
				Risk: "high", PermissionMode: "default", ScopePreview: visible,
				Warning: visible, RevokeHint: visible, AllowPermanent: true,
			},
			TransientIDs: transientIDs,
		},
		Sessions: SessionListViewSpec{
			Entries: entries, Truncated: true, ScannedFiles: 4, ScannedBytes: 2048, Notice: visible,
		},
	})

	lines[0] = redact.SafeText{}
	skills[0] = "mutated"
	messages[0] = redact.SafeText{}
	transientIDs[0] = "mutated"
	entries[0].ID = "mutated"
	entries[0].Title = redact.SafeText{}

	conversation := view.Conversation()
	request := view.Request()
	sessions := view.Sessions()
	if view.Generation() != 12 || view.RuntimeSequence() != 15 || view.Screen() != ScreenChat ||
		view.Lines()[0].Text() != visible.Text() || conversation.Skills()[0] != "review" ||
		conversation.Messages()[0].Text() != visible.Text() || request.TransientIDs()[0] != "transient-12" ||
		sessions.Entries()[0].ID() != "session-12" || sessions.Entries()[0].Title().Text() != visible.Text() {
		t.Fatalf("ViewModel retained caller-owned construction data: %#v", view)
	}

	returnedLines := view.Lines()
	returnedSkills := conversation.Skills()
	returnedMessages := conversation.Messages()
	returnedTransientIDs := request.TransientIDs()
	returnedEntries := sessions.Entries()
	returnedLines[0] = redact.SafeText{}
	returnedSkills[0] = "mutated"
	returnedMessages[0] = redact.SafeText{}
	returnedTransientIDs[0] = "mutated"
	returnedEntries[0] = SessionListEntry{}
	if view.Lines()[0].Text() != visible.Text() || conversation.Skills()[0] != "review" ||
		conversation.Messages()[0].Text() != visible.Text() || request.TransientIDs()[0] != "transient-12" ||
		sessions.Entries()[0].ID() != "session-12" {
		t.Fatal("ViewModel exposed renderer-mutable backing storage")
	}

	detachedConversation := view.Conversation()
	detachedRequest := view.Request()
	detachedSessions := view.Sessions()
	detachedConversation.skills[0] = "mutated"
	detachedConversation.messages[0] = redact.SafeText{}
	detachedRequest.transientIDs[0] = "mutated"
	detachedSessions.entries[0] = SessionListEntry{}
	if view.Conversation().Skills()[0] != "review" || view.Conversation().Messages()[0].Text() != visible.Text() ||
		view.Request().TransientIDs()[0] != "transient-12" || view.Sessions().Entries()[0].ID() != "session-12" {
		t.Fatal("ViewModel exposed slice storage through a nested view snapshot")
	}

	lastError, hasError := request.LastError()
	confirmation, hasConfirmation := request.Confirmation()
	if !hasError || lastError.Code() != "safe_error" || lastError.Message().Text() != visible.Text() ||
		!hasConfirmation || confirmation.CallID() != "call-12" || confirmation.Prompt().Text() != visible.Text() {
		t.Fatalf("ViewModel lost safe optional values: error=%#v confirmation=%#v", lastError, confirmation)
	}

	for _, value := range []any{
		view, conversation, request, lastError, confirmation, sessions, sessions.Entries()[0],
	} {
		assertCapabilityFreeValue(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}

	nonNilEmpty := NewStateViewModel(ViewModelSpec{
		Lines:        []redact.SafeText{},
		Conversation: ConversationViewSpec{Skills: []string{}, Messages: []redact.SafeText{}},
		Request:      RequestViewSpec{TransientIDs: []string{}},
		Sessions:     SessionListViewSpec{Entries: []SessionListEntrySpec{}},
	})
	if nonNilEmpty.Lines() == nil || nonNilEmpty.Conversation().Skills() == nil ||
		nonNilEmpty.Conversation().Messages() == nil || nonNilEmpty.Request().TransientIDs() == nil ||
		nonNilEmpty.Sessions().Entries() == nil {
		t.Fatal("ViewModel collapsed present empty slices into absent nil slices")
	}
	nilSlices := NewStateViewModel(ViewModelSpec{})
	if nilSlices.Lines() != nil || nilSlices.Conversation().Skills() != nil ||
		nilSlices.Conversation().Messages() != nil || nilSlices.Request().TransientIDs() != nil ||
		nilSlices.Sessions().Entries() != nil {
		t.Fatal("ViewModel changed absent nil slices into present empty slices")
	}
}

func assertTUIProductionImportsAreCapabilityFree(t *testing.T) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate TUI package")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(currentFile), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbiddenImports := map[string]bool{
		"xagent/internal/artifact":     true,
		"xagent/internal/config":       true,
		"xagent/internal/mcpclient":    true,
		"xagent/internal/orchestrator": true,
		"xagent/internal/provider":     true,
	}
	forbiddenSelectors := map[string]map[string]bool{
		"xagent/internal/conversation": {
			"Store": true, "Conversation": true, "ListResult": true, "LoadResult": true,
		},
		"xagent/internal/tool": {
			"Registry": true, "Executor": true, "Tool": true,
		},
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), parseErr)
		}
		aliases := make(map[string]string)
		for _, imported := range parsed.Imports {
			importPath := strings.Trim(imported.Path.Value, "\"")
			if forbiddenImports[importPath] {
				t.Fatalf("%s imports forbidden domain service %q", filepath.Base(path), importPath)
			}
			if _, checked := forbiddenSelectors[importPath]; !checked {
				continue
			}
			alias := filepath.Base(importPath)
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			if alias == "." {
				t.Fatalf("%s dot-imports boundary package %q", filepath.Base(path), importPath)
			}
			aliases[alias] = importPath
		}

		parsed, parseErr = parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s selectors: %v", filepath.Base(path), parseErr)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			identifier, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier {
				return true
			}
			importPath, knownAlias := aliases[identifier.Name]
			if knownAlias && forbiddenSelectors[importPath][selector.Sel.Name] {
				t.Errorf("%s accesses forbidden capability %s.%s", filepath.Base(path), identifier.Name, selector.Sel.Name)
			}
			return true
		})
	}
}

func assertCapabilityFreeValue(t *testing.T, valueType reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[valueType] {
		return
	}
	seen[valueType] = true
	if valueType.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if field.PkgPath == "" {
			t.Fatalf("%v.%s is exported and mutable", valueType, field.Name)
		}
		fieldType := field.Type
		if fieldType == reflect.TypeOf((*error)(nil)).Elem() ||
			fieldType.Kind() == reflect.Interface ||
			fieldType.Kind() == reflect.Pointer ||
			fieldType.Kind() == reflect.Map ||
			fieldType.Kind() == reflect.Func ||
			fieldType.Kind() == reflect.Chan ||
			fieldType.Kind() == reflect.UnsafePointer ||
			fieldType.Kind() == reflect.Uintptr ||
			(fieldType.Kind() == reflect.Slice && fieldType.Elem().Kind() == reflect.Uint8) {
			t.Fatalf("%v.%s exposes unsafe or capability-bearing type %v", valueType, field.Name, fieldType)
		}
		packagePath := fieldType.PkgPath()
		for _, forbidden := range []string{
			"/artifact", "/config", "/conversation", "/diagnostics", "/events", "/mcpclient", "/orchestrator", "/provider", "/tool",
		} {
			if strings.Contains(packagePath, forbidden) {
				t.Fatalf("%v.%s depends on forbidden domain package %q", valueType, field.Name, packagePath)
			}
		}
		if fieldType.Kind() == reflect.Slice || fieldType.Kind() == reflect.Array {
			assertCapabilityFreeValue(t, fieldType.Elem(), seen)
		} else {
			assertCapabilityFreeValue(t, fieldType, seen)
		}
	}
}
