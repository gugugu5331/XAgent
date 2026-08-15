package command

import (
	"reflect"
	"strings"
	"testing"
)

func noopHandler(ExecutionContext, Invocation) error { return nil }

func TestRegistryNormalizesSortsAndCopiesDefinitions(t *testing.T) {
	definitions := []Definition{
		{Name: "/Zulu", Aliases: []string{"Z"}, Type: TypeLocal, Handler: noopHandler},
		{Name: "alpha", Aliases: []string{"A"}, Type: TypeUI, Handler: noopHandler},
	}
	registry, err := New(definitions...)
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Aliases[0] = "changed"
	visible := registry.Visible()
	if len(visible) != 2 || visible[0].Name != "alpha" || visible[1].Name != "zulu" || visible[1].Aliases[0] != "z" {
		t.Fatalf("unexpected definitions: %#v", visible)
	}
	visible[0].Aliases[0] = "mutated"
	if registry.Visible()[0].Aliases[0] != "a" {
		t.Fatal("Visible returned mutable registry aliases")
	}
}

func TestRegistryDefinitionsCopiesAllDefinitions(t *testing.T) {
	registry := MustNew(
		Definition{Name: "visible", Aliases: []string{"v"}, Type: TypeLocal, Handler: noopHandler},
		Definition{Name: "hidden", Aliases: []string{"x"}, Type: TypeLocal, Hidden: true, Handler: noopHandler},
	)
	definitions := registry.Definitions()
	if len(definitions) != 2 || definitions[0].Name != "hidden" || !definitions[0].Hidden {
		t.Fatalf("Definitions omitted hidden command: %#v", definitions)
	}
	definitions[0].Aliases[0] = "mutated"
	definitions[1].Name = "changed"
	again := registry.Definitions()
	if again[0].Aliases[0] != "x" || again[1].Name != "visible" {
		t.Fatalf("Definitions exposed mutable registry state: %#v", again)
	}
	if definitions := (*Registry)(nil).Definitions(); definitions != nil {
		t.Fatalf("nil Registry returned definitions: %#v", definitions)
	}
}

func TestRegistryRejectsConflicts(t *testing.T) {
	tests := []struct {
		name        string
		definitions []Definition
		conflict    string
	}{
		{name: "name name", conflict: "same", definitions: []Definition{{Name: "same", Type: TypeLocal, Handler: noopHandler}, {Name: "SAME", Type: TypeUI, Handler: noopHandler}}},
		{name: "name alias", conflict: "one", definitions: []Definition{{Name: "one", Type: TypeLocal, Handler: noopHandler}, {Name: "two", Aliases: []string{"ONE"}, Type: TypeLocal, Handler: noopHandler}}},
		{name: "alias alias", conflict: "x", definitions: []Definition{{Name: "one", Aliases: []string{"x"}, Type: TypeLocal, Handler: noopHandler}, {Name: "two", Aliases: []string{"X"}, Type: TypeLocal, Handler: noopHandler}}},
		{name: "inside definition", conflict: "dup", definitions: []Definition{{Name: "one", Aliases: []string{"dup", "DUP"}, Type: TypeLocal, Handler: noopHandler}}},
		{name: "shortcut shortcut", conflict: "n", definitions: []Definition{
			{Name: "one", Type: TypeUI, Intent: IntentNewConversation, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "n", Description: "one"}}, Handler: noopHandler},
			{Name: "two", Type: TypeUI, Intent: IntentShowSessions, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "N", Description: "two"}}, Handler: noopHandler},
		}},
		{name: "help metadata", conflict: "status", definitions: []Definition{
			{Name: "one", Type: TypeLocal, HelpEntries: []HelpEntry{{Kind: HelpEntryStatus, Name: "status", Description: "one"}}, Handler: noopHandler},
			{Name: "two", Type: TypeLocal, HelpEntries: []HelpEntry{{Kind: HelpEntryStatus, Name: "status", Description: "two"}}, Handler: noopHandler},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.definitions...)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.conflict) {
				t.Fatalf("expected conflict %q, got %v", test.conflict, err)
			}
		})
	}
}

func TestRegistryRejectsInvalidDefinitions(t *testing.T) {
	tests := []Definition{
		{Name: "", Type: TypeLocal, Handler: noopHandler},
		{Name: "two words", Type: TypeLocal, Handler: noopHandler},
		{Name: "name", Aliases: []string{""}, Type: TypeLocal, Handler: noopHandler},
		{Name: "name", Type: Type("bad"), Handler: noopHandler},
		{Name: "name", Type: TypeLocal},
		{Name: "name", Type: TypeLocal, HelpEntries: []HelpEntry{{Kind: HelpEntryKind("bad"), Name: "x", Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeLocal, HelpEntries: []HelpEntry{{Kind: HelpEntryPermissionMode, Name: "unknown", Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeLocal, HelpEntries: []HelpEntry{{Kind: HelpEntryStatus, Name: "status"}}, Handler: noopHandler},
		{Name: "name", Type: TypeLocal, Hidden: true, HelpEntries: []HelpEntry{{Kind: HelpEntryStatus, Name: "status", Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Hidden: true, Intent: IntentNewConversation, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "n", Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Intent: IntentKind("bad"), Handler: noopHandler},
		{Name: "name", Type: TypeUI, Shortcuts: []Shortcut{{Context: ShortcutContext("bad"), Key: "n", Intent: IntentNewConversation, Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "", Intent: IntentNewConversation, Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "n", Description: "bad"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Intent: IntentNewConversation, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "n"}}, Handler: noopHandler},
		{Name: "name", Type: TypeUI, Intent: IntentNewConversation, Shortcuts: []Shortcut{{Context: ShortcutSessions, Key: "n", Description: "one"}, {Context: ShortcutSessions, Key: "N", Description: "two"}}, Handler: noopHandler},
	}
	for index, definition := range tests {
		if _, err := New(definition); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", index)
		}
	}
}

func TestC9NavigationCommandsAndBindings(t *testing.T) {
	registry := MustNew(Builtins()...)
	controller := &c9IntentController{}

	wantCommands := map[string]IntentKind{
		"/new":      IntentNewConversation,
		"/sessions": IntentShowSessions,
		"/list":     IntentShowSessions,
	}
	for command, want := range wantCommands {
		got, ok := registry.IntentForCommand(command)
		if !ok || got != want {
			t.Fatalf("IntentForCommand(%q) = (%q, %t), want (%q, true)", command, got, ok, want)
		}
		result := registry.Dispatch(command, controller)
		if result.Err != nil || len(controller.intents) == 0 || controller.intents[len(controller.intents)-1] != want {
			t.Fatalf("Dispatch(%q) did not emit %q: result=%#v intents=%#v", command, want, result, controller.intents)
		}
	}

	wantBindings := map[struct {
		context ShortcutContext
		key     string
	}]IntentKind{
		{context: ShortcutChatIdle, key: "Esc"}:         IntentShowSessions,
		{context: ShortcutChatStreaming, key: "Esc"}:    IntentCancel,
		{context: ShortcutChatConfirmation, key: "Esc"}: IntentCancel,
		{context: ShortcutSessions, key: "Enter"}:       IntentOpenConversation,
		{context: ShortcutSessions, key: "n"}:           IntentNewConversation,
		{context: ShortcutSessions, key: "q"}:           IntentQuit,
	}
	for binding, want := range wantBindings {
		got, ok := registry.IntentForShortcut(binding.context, binding.key)
		if !ok || got != want {
			t.Fatalf("IntentForShortcut(%q, %q) = (%q, %t), want (%q, true)", binding.context, binding.key, got, ok, want)
		}
	}

	newIntent, _ := registry.IntentForCommand("/new")
	newKeyIntent, _ := registry.IntentForShortcut(ShortcutSessions, "n")
	sessionsIntent, _ := registry.IntentForCommand("/list")
	escapeIntent, _ := registry.IntentForShortcut(ShortcutChatIdle, "esc")
	if newIntent != newKeyIntent || sessionsIntent != escapeIntent {
		t.Fatalf("commands and shortcuts diverged: new=%q/n=%q sessions=%q/esc=%q", newIntent, newKeyIntent, sessionsIntent, escapeIntent)
	}

	bindings := registry.Bindings()
	if len(bindings) != 6 {
		t.Fatalf("Bindings count = %d, want 6: %#v", len(bindings), bindings)
	}
	if _, ok := registry.IntentForShortcut(ShortcutChatIdle, "enter"); ok {
		t.Fatal("shortcut leaked into the wrong context")
	}
	if _, ok := (*Registry)(nil).IntentForShortcut(ShortcutChatIdle, "esc"); ok || (*Registry)(nil).Bindings() != nil || (*Registry)(nil).HelpMetadata() != nil {
		t.Fatal("nil Registry exposed navigation metadata")
	}

	definitions := registry.Definitions()
	var newDefinition, sessionsDefinition, diagnosticsDefinition *Definition
	for index := range definitions {
		definition := &definitions[index]
		switch definition.Name {
		case "new":
			newDefinition = definition
		case "sessions":
			sessionsDefinition = definition
		case "diagnostics":
			diagnosticsDefinition = definition
		}
	}
	if newDefinition == nil || newDefinition.Hidden || newDefinition.Intent != IntentNewConversation || len(newDefinition.Shortcuts) != 1 {
		t.Fatalf("public /new metadata incomplete: %#v", newDefinition)
	}
	if sessionsDefinition == nil || sessionsDefinition.Hidden || !reflect.DeepEqual(sessionsDefinition.Aliases, []string{"list"}) || sessionsDefinition.Intent != IntentShowSessions || len(sessionsDefinition.Shortcuts) != 5 {
		t.Fatalf("public /sessions metadata incomplete: %#v", sessionsDefinition)
	}
	if diagnosticsDefinition == nil || !diagnosticsDefinition.Hidden || len(diagnosticsDefinition.HelpEntries) != 0 {
		t.Fatalf("legacy diagnostics visibility changed: %#v", diagnosticsDefinition)
	}

	help := registry.HelpMetadata()
	wantHelp := []HelpMetadata{
		{Kind: HelpEntryDiagnostics, Name: "status", Description: "显示有界、脱敏的诊断摘要", CanonicalName: "status"},
		{Kind: HelpEntryPermissionMode, Name: "default", Description: "只读操作自动允许，其他操作需要确认", CanonicalName: "permission"},
		{Kind: HelpEntryPermissionMode, Name: "permissive", Description: "除受保护命令外尽量自动允许", CanonicalName: "permission"},
		{Kind: HelpEntryPermissionMode, Name: "strict", Description: "写入与命令执行需要确认", CanonicalName: "permission"},
		{Kind: HelpEntryStatus, Name: "status", Description: "显示统一运行状态", CanonicalName: "status"},
	}
	if !reflect.DeepEqual(help, wantHelp) {
		t.Fatalf("HelpMetadata = %#v, want %#v", help, wantHelp)
	}
	help[0].Name = "mutated"
	bindings[0].Key = "mutated"
	if registry.HelpMetadata()[0].Name == "mutated" || registry.Bindings()[0].Key == "mutated" {
		t.Fatal("Registry exposed mutable help or binding metadata")
	}

	sessionsDefinition.Shortcuts[0].Key = "mutated"
	permissionIndex := definitionIndex(t, definitions, "permission")
	definitions[permissionIndex].HelpEntries[0].Name = "mutated"
	if got := registry.Definitions(); got[definitionIndex(t, got, "sessions")].Shortcuts[0].Key == "mutated" || got[definitionIndex(t, got, "permission")].HelpEntries[0].Name == "mutated" {
		t.Fatal("Definitions exposed mutable command metadata")
	}
}

func TestStatusIsPublicDiagnosticsEntry(t *testing.T) {
	registry := MustNew(Builtins()...)
	catalog := registry.HelpCatalog()
	var statusCommand bool
	for _, item := range catalog.Commands {
		if item.Name == "status" && reflect.DeepEqual(item.Aliases, []string{"st"}) && item.Usage == "/status" {
			statusCommand = true
		}
		if item.Name == "diagnostics" {
			t.Fatal("hidden diagnostics command entered public help catalog")
		}
	}
	if !statusCommand {
		t.Fatalf("public /status metadata missing: %#v", catalog.Commands)
	}

	wantKinds := map[HelpEntryKind]bool{HelpEntryStatus: false, HelpEntryDiagnostics: false}
	for _, entry := range catalog.Entries {
		if _, ok := wantKinds[entry.Kind]; ok && entry.Name == "status" && entry.CanonicalName == "status" {
			wantKinds[entry.Kind] = true
		}
		if entry.CanonicalName == "diagnostics" {
			t.Fatalf("legacy diagnostics owns public help metadata: %#v", entry)
		}
	}
	for kind, found := range wantKinds {
		if !found {
			t.Fatalf("/status missing %q help entry: %#v", kind, catalog.Entries)
		}
	}

	catalog.Commands[0].Name = "mutated"
	catalog.Bindings[0].Key = "mutated"
	catalog.Entries[0].Name = "mutated"
	again := registry.HelpCatalog()
	if again.Commands[0].Name == "mutated" || again.Bindings[0].Key == "mutated" || again.Entries[0].Name == "mutated" {
		t.Fatal("HelpCatalog exposed mutable registry state")
	}
}

type c9IntentController struct {
	testController
	intents []IntentKind
}

func (controller *c9IntentController) HandleIntent(intent IntentKind) error {
	controller.intents = append(controller.intents, intent)
	return nil
}

func definitionIndex(t *testing.T, definitions []Definition, name string) int {
	t.Helper()
	for index, definition := range definitions {
		if definition.Name == name {
			return index
		}
	}
	t.Fatalf("missing definition %q", name)
	return -1
}

func TestMustNewPanicsWithConflictDetails(t *testing.T) {
	defer func() {
		value := recover()
		if value == nil {
			t.Fatal("expected panic")
		}
		message := strings.ToLower(value.(error).Error())
		if !strings.Contains(message, "dup") || !strings.Contains(message, "one") || !strings.Contains(message, "two") {
			t.Fatalf("panic lacks conflict details: %q", message)
		}
	}()
	MustNew(
		Definition{Name: "one", Aliases: []string{"dup"}, Type: TypeLocal, Handler: noopHandler},
		Definition{Name: "two", Aliases: []string{"DUP"}, Type: TypeLocal, Handler: noopHandler},
	)
}
