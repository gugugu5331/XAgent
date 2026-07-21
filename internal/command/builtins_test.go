package command

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type testController struct {
	notices           []string
	errors            []error
	sent              []string
	cleared           int
	mode              Mode
	refreshed         int
	compactMessage    string
	compactErr        error
	session           SessionStatus
	memory            MemoryStatus
	permission        PermissionStatus
	runtime           RuntimeStatus
	usage             TokenUsage
	legacyMemoryArgs  []string
	legacyMemoryText  string
	legacyMemoryErr   error
	legacyMCPText     string
	legacyMCPErr      error
	legacyDiagnostics string
	legacyDiagErr     error
}

func (c *testController) DisplayNotice(text string)   { c.notices = append(c.notices, text) }
func (c *testController) DisplayError(err error)      { c.errors = append(c.errors, err) }
func (c *testController) SendUserMessage(text string) { c.sent = append(c.sent, text) }
func (c *testController) ClearMessages()              { c.cleared++ }
func (c *testController) SwitchMode(mode Mode)        { c.mode = mode }
func (c *testController) CurrentMode() Mode           { return c.mode }
func (c *testController) RefreshStatus()              { c.refreshed++ }
func (c *testController) CompactContext() (string, error) {
	return c.compactMessage, c.compactErr
}
func (c *testController) SessionStatus() SessionStatus       { return c.session }
func (c *testController) MemoryStatus() MemoryStatus         { return c.memory }
func (c *testController) PermissionStatus() PermissionStatus { return c.permission }
func (c *testController) RuntimeStatus() RuntimeStatus       { return c.runtime }
func (c *testController) TokenUsage() TokenUsage             { return c.usage }
func (c *testController) LegacyMemory(args string) (string, error) {
	c.legacyMemoryArgs = append(c.legacyMemoryArgs, args)
	return c.legacyMemoryText, c.legacyMemoryErr
}
func (c *testController) LegacyMCPStatus() (string, error) {
	return c.legacyMCPText, c.legacyMCPErr
}
func (c *testController) LegacyDiagnostics() (string, error) {
	return c.legacyDiagnostics, c.legacyDiagErr
}
func (c *testController) lastNotice() string {
	if len(c.notices) == 0 {
		return ""
	}
	return c.notices[len(c.notices)-1]
}
func (c *testController) lastError() string {
	if len(c.errors) == 0 || c.errors[len(c.errors)-1] == nil {
		return ""
	}
	return c.errors[len(c.errors)-1].Error()
}

func TestBuiltinsRegisterTenVisibleCommands(t *testing.T) {
	registry := MustNew(Builtins()...)
	visible := registry.Visible()
	if len(visible) != 10 {
		t.Fatalf("expected 10 visible commands, got %d: %#v", len(visible), visible)
	}
	expected := map[string]struct {
		aliases []string
		typeOf  Type
	}{
		"help": {aliases: []string{"h", "?"}, typeOf: TypeLocal}, "compact": {aliases: []string{"ctx"}, typeOf: TypeLocal},
		"clear": {aliases: []string{"cls"}, typeOf: TypeUI}, "plan": {aliases: []string{"p"}, typeOf: TypeUI},
		"do": {aliases: []string{"d"}, typeOf: TypeUI}, "session": {aliases: []string{"sess"}, typeOf: TypeLocal},
		"memory": {aliases: []string{"mem"}, typeOf: TypeLocal}, "permission": {aliases: []string{"perm"}, typeOf: TypeLocal},
		"status": {aliases: []string{"st"}, typeOf: TypeLocal}, "review": {aliases: []string{"rv"}, typeOf: TypePrompt},
	}
	seenTypes := map[Type]bool{}
	for _, definition := range visible {
		if definition.Name == "permissions" || definition.Name == "mcp" || definition.Name == "diagnostics" {
			t.Fatalf("hidden command is visible: %#v", definition)
		}
		if definition.Description == "" || definition.Usage == "" || definition.Handler == nil {
			t.Fatalf("incomplete metadata: %#v", definition)
		}
		want, ok := expected[definition.Name]
		if !ok || !reflect.DeepEqual(definition.Aliases, want.aliases) || definition.Type != want.typeOf || definition.Hidden {
			t.Fatalf("unexpected builtin metadata: %#v", definition)
		}
		seenTypes[definition.Type] = true
	}
	for _, typeOf := range []Type{TypeLocal, TypeUI, TypePrompt} {
		if !seenTypes[typeOf] {
			t.Fatalf("builtin types do not include %q", typeOf)
		}
	}
}

func TestBuiltinHelpShowsVisibleMetadataOnly(t *testing.T) {
	registry := MustNew(Builtins()...)
	for _, name := range []string{"/help", "/h", "/?"} {
		controller := &testController{}
		if result := registry.Dispatch(name, controller); result.Err != nil {
			t.Fatalf("%s failed: %v", name, result.Err)
		}
		text := controller.lastNotice()
		for _, definition := range registry.Visible() {
			for _, want := range append([]string{"/" + definition.Name, definition.Description, definition.Usage}, slashAliases(definition.Aliases)...) {
				if !strings.Contains(text, want) {
					t.Fatalf("help missing %q for /%s: %q", want, definition.Name, text)
				}
			}
			if definition.ArgHint != "" && !strings.Contains(text, definition.ArgHint) {
				t.Fatalf("help missing argument hint for /%s: %q", definition.Name, text)
			}
		}
		for _, hidden := range []string{"/permissions", "/mcp", "/diagnostics"} {
			if strings.Contains(text, hidden+" ") {
				t.Fatalf("help leaked %s: %q", hidden, text)
			}
		}
	}
}

func slashAliases(aliases []string) []string {
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		result = append(result, "/"+alias)
	}
	return result
}

func TestBuiltinUIAndReviewCommands(t *testing.T) {
	registry := MustNew(Builtins()...)
	controller := &testController{}
	registry.Dispatch("/clear", controller)
	if controller.cleared != 1 {
		t.Fatal("clear did not clear messages")
	}
	registry.Dispatch("/p", controller)
	if controller.mode != ModePlan || controller.refreshed != 1 {
		t.Fatalf("plan did not switch mode: %#v", controller)
	}
	registry.Dispatch("/d", controller)
	if controller.mode != ModeDefault || controller.refreshed != 2 {
		t.Fatalf("do did not restore mode: %#v", controller)
	}
	registry.Dispatch("/rv", controller)
	if len(controller.sent) != 1 || controller.sent[0] != ReviewPrompt || !strings.Contains(controller.sent[0], "不要修改文件") {
		t.Fatalf("review prompt mismatch: %#v", controller.sent)
	}
}

func TestBuiltinLocalStatusCommands(t *testing.T) {
	registry := MustNew(Builtins()...)
	controller := &testController{
		compactMessage: "上下文已压缩",
		session:        SessionStatus{ID: "session-1", MessageCount: 3, Mode: ModePlan, Streaming: true},
		memory: MemoryStatus{
			User: MemoryScopeStatus{Enabled: true, Count: 2}, Project: MemoryScopeStatus{Enabled: false, Count: 4}, Diagnostics: []string{"warning"},
		},
		permission: PermissionStatus{Mode: "default", SessionRules: 1, LocalRules: 2, ProjectRules: 3, UserRules: 4, LoadErrors: 5},
		runtime:    RuntimeStatus{Provider: "fake", Model: "model", Mode: ModePlan, Streaming: true, MCP: "1 ready", RecentError: "boom"},
		usage:      TokenUsage{Input: 11, Output: 22, CacheCreation: 33, CacheRead: 44},
	}
	tests := []struct {
		input string
		want  []string
	}{
		{input: "/ctx", want: []string{"上下文已压缩"}},
		{input: "/sess", want: []string{"session-1", "messages=3", "mode=plan", "streaming=true"}},
		{input: "/mem", want: []string{"user=on(2)", "project=off(4)", "warning"}},
		{input: "/perm", want: []string{"mode=default", "session=1", "load_errors=5"}},
		{input: "/st", want: []string{"provider=fake", "model=model", "mode=plan", "tokens=11_in/22_out", "cache=33_create/44_read", "recent_error=boom"}},
	}
	for _, test := range tests {
		controller.notices = nil
		result := registry.Dispatch(test.input, controller)
		if result.Err != nil {
			t.Fatalf("%s failed: %v", test.input, result.Err)
		}
		for _, want := range test.want {
			if !strings.Contains(controller.lastNotice(), want) {
				t.Fatalf("%s output missing %q: %q", test.input, want, controller.lastNotice())
			}
		}
	}
}

func TestBuiltinLegacyCommandsDelegate(t *testing.T) {
	registry := MustNew(Builtins()...)
	controller := &testController{legacyMemoryText: "memory ok", legacyMCPText: "mcp ok", legacyDiagnostics: "diagnostics ok", permission: PermissionStatus{Mode: "strict"}}
	tests := []struct {
		input string
		want  string
	}{
		{input: "/memory status", want: "memory ok"},
		{input: "/permissions status", want: "mode=strict"},
		{input: "/mcp status", want: "mcp ok"},
		{input: "/diagnostics", want: "diagnostics ok"},
	}
	for _, test := range tests {
		controller.notices = nil
		if result := registry.Dispatch(test.input, controller); result.Err != nil || !strings.Contains(controller.lastNotice(), test.want) {
			t.Fatalf("%s failed: result=%#v notice=%q", test.input, result, controller.lastNotice())
		}
	}
	if len(controller.legacyMemoryArgs) != 1 || controller.legacyMemoryArgs[0] != "status" {
		t.Fatalf("memory arguments not delegated: %#v", controller.legacyMemoryArgs)
	}
}

func TestBuiltinUsageErrorsNeverSendUserMessage(t *testing.T) {
	registry := MustNew(Builtins()...)
	for _, input := range []string{"/help extra", "/compact extra", "/clear extra", "/plan extra", "/do extra", "/session extra", "/permission extra", "/status extra", "/review extra", "/mcp nope", "/permissions nope", "/diagnostics extra"} {
		controller := &testController{}
		result := registry.Dispatch(input, controller)
		if result.Err == nil || !strings.Contains(result.Err.Error(), "用法:") || len(controller.sent) != 0 {
			t.Fatalf("expected usage error for %q: result=%#v controller=%#v", input, result, controller)
		}
	}
}

func TestBuiltinPropagatesControllerErrors(t *testing.T) {
	registry := MustNew(Builtins()...)
	controller := &testController{compactErr: errors.New("compact failed")}
	result := registry.Dispatch("/compact", controller)
	if result.Err == nil || controller.lastError() != "compact failed" {
		t.Fatalf("controller error was not reported: %#v %#v", result, controller)
	}
}
