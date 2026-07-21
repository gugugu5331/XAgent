package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type skillAppProvider struct {
	mu       sync.Mutex
	requests []provider.ChatRequest
	events   []provider.StreamEvent
}

func (p *skillAppProvider) Name() string { return "skill-test" }

func (p *skillAppProvider) StreamChat(_ context.Context, request provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	events := append([]provider.StreamEvent(nil), p.events...)
	p.mu.Unlock()
	if len(events) == 0 {
		events = []provider.StreamEvent{{Type: provider.StreamEventTextDelta, Delta: "completed"}, {Type: provider.StreamEventDone}}
	}
	stream := make(chan provider.StreamEvent, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func (p *skillAppProvider) Requests() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.ChatRequest(nil), p.requests...)
}

type appSkillSpec struct {
	file        string
	name        string
	description string
	mode        skill.Mode
	body        string
	tools       []string
	history     int
	model       string
}

func writeAppSkill(t *testing.T, root string, spec appSkillSpec) string {
	t.Helper()
	if spec.file == "" {
		spec.file = spec.name + ".md"
	}
	if spec.description == "" {
		spec.description = spec.name + " description"
	}
	var source strings.Builder
	fmt.Fprintf(&source, "---\nname: %s\ndescription: %s\nmode: %s\n", spec.name, spec.description, spec.mode)
	if len(spec.tools) > 0 {
		source.WriteString("allowed_tools:\n")
		for _, name := range spec.tools {
			fmt.Fprintf(&source, "  - %s\n", name)
		}
	}
	if spec.history != 0 {
		fmt.Fprintf(&source, "history: %d\n", spec.history)
	}
	if spec.model != "" {
		fmt.Fprintf(&source, "model: %s\n", spec.model)
	}
	fmt.Fprintf(&source, "---\n%s\n", spec.body)
	path := filepath.Join(root, spec.file)
	if err := os.WriteFile(path, []byte(source.String()), 0o600); err != nil {
		t.Fatalf("write Skill: %v", err)
	}
	return path
}

func newSkillAppModel(t *testing.T, skillRoot string, providerImpl provider.Provider) Model {
	t.Helper()
	projectRoot := t.TempDir()
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	manager, err := skill.NewManager(skill.ManagerOptions{
		Sources:         []skill.SourceFS{{Source: skill.SourceProject, Root: skillRoot}},
		ToolNames:       registry.Names(),
		ReservedCommand: appReservedCommands(),
		Redact:          func(value string) string { return value },
	})
	if err != nil {
		t.Fatalf("create Skill manager: %v", err)
	}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	executor := tool.NewExecutor(registry, projectRoot, time.Second, 1024)
	return New(Deps{
		Config: cfg, Provider: providerImpl, Store: &fakeRecoveringStore{}, Resources: fakeResources{},
		Registry: registry, Executor: executor, SkillManager: manager,
	})
}

func appReservedCommands() []string {
	definitions := command.MustNew(command.Builtins()...).Definitions()
	result := make([]string, 0, len(definitions)*2)
	for _, definition := range definitions {
		result = append(result, definition.Name)
		result = append(result, definition.Aliases...)
	}
	return result
}

func TestSkillActivityLifecycle(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{name: "commit", mode: skill.ModeShared, body: "commit SOP {{args}}"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)

	model = runCommandInput(t, model, "/commit first")
	if active := model.skillActivity.Snapshot().Active; len(active) != 1 || active[0].Name != "commit" {
		t.Fatalf("shared Skill did not activate: %#v", active)
	}
	model.startNewConversation()
	if active := model.skillActivity.Snapshot().Active; len(active) != 0 {
		t.Fatalf("new conversation inherited Skills: %#v", active)
	}

	model = runCommandInput(t, model, "/commit second")
	store := model.deps.Store.(*fakeRecoveringStore)
	store.conversation = conversation.NewConversation("other", time.Now())
	model.loadConversation("other")
	if active := model.skillActivity.Snapshot().Active; len(active) != 0 {
		t.Fatalf("loaded conversation inherited Skills: %#v", active)
	}
	if model.status.ActiveSkills != "" || model.status.RequestModel != "" {
		t.Fatalf("loaded conversation retained Skill status: %#v", model.status)
	}

	restarted := newSkillAppModel(t, root, providerImpl)
	if active := restarted.skillActivity.Snapshot().Active; len(active) != 0 {
		t.Fatalf("new App inherited Skills: %#v", active)
	}
}

func TestSkillCommandRefresh(t *testing.T) {
	root := t.TempDir()
	demoPath := writeAppSkill(t, root, appSkillSpec{name: "demo", mode: skill.ModeShared, body: "demo SOP"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)
	initialGeneration := model.skillGeneration

	writeAppSkill(t, root, appSkillSpec{name: "added", mode: skill.ModeShared, body: "added SOP with a different size"})
	model.input.SetValue("/add")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.input.Value() != "/added" || model.skillGeneration <= initialGeneration {
		t.Fatalf("added Skill was not refreshed: input=%q generation=%d", model.input.Value(), model.skillGeneration)
	}

	writeAppSkill(t, root, appSkillSpec{name: "entered", mode: skill.ModeShared, body: "ENTER REFRESH SOP {{args}}"})
	beforeEnterGeneration := model.skillGeneration
	model = runCommandInput(t, model, "/entered from-enter")
	requests := providerImpl.Requests()
	if model.skillGeneration <= beforeEnterGeneration || len(requests) != 1 || !requestContainsDynamic(requests[0], "ENTER REFRESH SOP from-enter") {
		t.Fatalf("Enter did not refresh and execute the added Skill: generation=%d requests=%#v", model.skillGeneration, requests)
	}

	if err := os.Remove(demoPath); err != nil {
		t.Fatal(err)
	}
	beforeRequestGeneration := model.skillGeneration
	model = runCommandInput(t, model, "ordinary request refresh")
	if got := model.commandRegistry.Complete("/demo"); len(got) != 0 {
		t.Fatalf("ordinary request refresh retained a deleted Skill: %#v", got)
	}
	requests = providerImpl.Requests()
	if model.skillGeneration <= beforeRequestGeneration || len(requests) != 2 || requestContainsDynamic(requests[1], "demo description") {
		t.Fatalf("ordinary request did not refresh the Skill catalog: generation=%d requests=%#v", model.skillGeneration, requests)
	}

	stableGeneration := model.skillGeneration
	writeAppSkill(t, root, appSkillSpec{file: "duplicate.md", name: "added", mode: skill.ModeShared, body: "conflicting body"})
	model.input.SetValue("/add")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.input.Value() != "/added" || model.skillGeneration != stableGeneration {
		t.Fatalf("failed refresh replaced the previous registry: input=%q generation=%d", model.input.Value(), model.skillGeneration)
	}
	if !strings.Contains(model.status.Notice, "上一版本") {
		t.Fatalf("failed refresh diagnostic was not visible: %q", model.status.Notice)
	}
}

func TestSkillHelpAndCompletion(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{name: "commit", description: "commit changes", mode: skill.ModeShared, body: "commit SOP"})
	writeAppSkill(t, root, appSkillSpec{name: "review", description: "review changes", mode: skill.ModeIsolated, history: 1, body: "review SOP"})
	writeAppSkill(t, root, appSkillSpec{name: "clear", description: "reserved conflict", mode: skill.ModeShared, body: "must not become a command"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)

	model = runCommandInput(t, model, "/help")
	for _, want := range []string{"/commit [Skill/shared]", "/review [Skill/isolated]"} {
		if !strings.Contains(model.status.Notice, want) {
			t.Fatalf("help omitted %q: %q", want, model.status.Notice)
		}
	}
	if !strings.Contains(model.status.Notice, "skill_command_reserved") {
		t.Fatalf("unchanged refresh dropped snapshot diagnostics: %q", model.status.Notice)
	}
	if got := model.commandRegistry.Complete("/clear"); len(got) != 1 || got[0].Badge != "" {
		t.Fatalf("reserved Skill replaced the base /clear command: %#v", got)
	}
	reservedActivity := skill.NewActivity()
	prepared, err := model.orchestrator.PrepareSkill(skill.Invocation{Name: "clear", Origin: skill.OriginAgentTool}, reservedActivity)
	if err != nil || prepared.Definition.Name != "clear" || len(reservedActivity.Snapshot().Active) != 1 {
		t.Fatalf("reserved Skill was not available to the Agent: prepared=%#v active=%#v err=%v", prepared, reservedActivity.Snapshot(), err)
	}
	reservedActivity.Clear()

	model.input.SetValue("/c")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if !model.commandMenu.Visible || !strings.Contains(model.commandMenu.View(), "/commit [Skill/shared]") {
		t.Fatalf("multi-match menu omitted Skill badge: %q", model.commandMenu.View())
	}
	model.commandMenu.Close()
	model.input.SetValue("/rev")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.input.Value() != "/review" {
		t.Fatalf("single Skill completion failed: %q", model.input.Value())
	}
	if len(providerImpl.Requests()) != 0 {
		t.Fatal("help or completion called the provider")
	}
}

func TestExecuteSharedSkillCommandAndPinnedSnapshot(t *testing.T) {
	root := t.TempDir()
	entry := writeAppSkill(t, root, appSkillSpec{
		name: "commit", mode: skill.ModeShared, model: "skill-model", body: "SHARED OLD SOP {{args}}",
	})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)

	model = runCommandInput(t, model, "/commit Target  One")
	requests := providerImpl.Requests()
	if len(requests) != 1 || requests[0].Model != "skill-model" || !requestContainsDynamic(requests[0], "SHARED OLD SOP Target  One") {
		t.Fatalf("shared Skill was not applied to the request: %#v", requests)
	}
	if len(model.conversation.Messages) < 2 || model.conversation.Messages[0].Content != "/commit Target  One" {
		t.Fatalf("raw Skill command was not persisted: %#v", model.conversation.Messages)
	}
	if strings.Contains(model.conversation.Messages[0].Content, "SHARED OLD SOP") {
		t.Fatal("SOP leaked into user history")
	}
	if model.status.ActiveSkills != "commit" || model.status.RequestModel != "" {
		t.Fatalf("shared Skill status was not restored after completion: %#v", model.status)
	}

	model = runCommandInput(t, model, "follow up")
	requests = providerImpl.Requests()
	if len(requests) != 2 || requests[1].Model != "skill-model" || !requestContainsDynamic(requests[1], "SHARED OLD SOP Target  One") {
		t.Fatalf("shared Skill did not persist: %#v", requests)
	}

	writeAppSkill(t, root, appSkillSpec{file: filepath.Base(entry), name: "commit", mode: skill.ModeShared, model: "skill-model", body: "SHARED UPDATED SOP {{args}} with more bytes"})
	model = runCommandInput(t, model, "after disk update")
	requests = providerImpl.Requests()
	if len(requests) != 3 || requests[2].Model != "skill-model" || !requestContainsDynamic(requests[2], "SHARED OLD SOP Target  One") || requestContainsDynamic(requests[2], "SHARED UPDATED SOP") {
		t.Fatalf("disk update rewrote an active snapshot: %#v", requests[2])
	}

	model = runCommandInput(t, model, "/commit New Args")
	requests = providerImpl.Requests()
	if len(requests) != 4 || requests[3].Model != "skill-model" || !requestContainsDynamic(requests[3], "SHARED UPDATED SOP New Args") {
		t.Fatalf("reactivation did not load the updated snapshot: %#v", requests[3])
	}
}

func TestExecuteIsolatedSkillCommandAndReviewShim(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{
		name: "review", mode: skill.ModeIsolated, history: 1, model: "review-model", body: "ISOLATED SOP {{args}}",
	})
	providerImpl := &skillAppProvider{events: []provider.StreamEvent{
		{Type: provider.StreamEventThinkingDelta, Delta: "temporary reasoning"},
		{Type: provider.StreamEventTextDelta, Delta: "review summary"},
		{Type: provider.StreamEventDone},
	}}
	model := newSkillAppModel(t, root, providerImpl)
	conversation.AppendUserMessage(model.conversation, "previous question")
	conversation.AppendAssistantMessage(model.conversation, "previous answer")
	model.messages.SetMessages(model.conversation.Messages)

	model = runCommandInput(t, model, "/review current changes")
	if got := model.conversation.Messages; len(got) != 4 || got[2].Content != "/review current changes" || got[3].Content != "review summary" {
		t.Fatalf("isolated Skill polluted main history: %#v", got)
	}
	if strings.Contains(model.messages.View(), "temporary reasoning") || !strings.Contains(model.messages.View(), "review summary") {
		t.Fatalf("transient trace was not cleaned: %q", model.messages.View())
	}
	if active := model.skillActivity.Snapshot().Active; len(active) != 0 {
		t.Fatalf("isolated Skill polluted main Activity: %#v", active)
	}
	requests := providerImpl.Requests()
	if len(requests) != 1 || requests[0].Model != "review-model" || !requestContainsDynamic(requests[0], "ISOLATED SOP current changes") {
		t.Fatalf("isolated request profile was wrong: %#v", requests)
	}
	if model.status.RequestModel != "" || model.status.ActiveSkills != "" {
		t.Fatalf("isolated status was not restored: %#v", model.status)
	}

	model = runCommandInput(t, model, "/rv shim args")
	if got := model.conversation.Messages; got[len(got)-2].Content != "/rv shim args" || got[len(got)-1].Content != "review summary" {
		t.Fatalf("/rv did not forward to review Skill: %#v", got)
	}
}

func TestClearSkillsKeepsConversation(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{name: "commit", mode: skill.ModeShared, model: "skill-model", body: "commit SOP"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)
	model = runCommandInput(t, model, "/commit")
	before := append([]conversation.Message(nil), model.conversation.Messages...)
	model.status.RequestModel = "skill-model"

	model = runCommandInput(t, model, "/clear")
	if !reflect.DeepEqual(model.conversation.Messages, before) {
		t.Fatalf("/clear changed conversation history: before=%#v after=%#v", before, model.conversation.Messages)
	}
	if active := model.skillActivity.Snapshot().Active; len(active) != 0 {
		t.Fatalf("/clear retained active Skills: %#v", active)
	}
	if model.status.ActiveSkills != "" || model.status.RequestModel != "" || model.messages.View() != "" {
		t.Fatalf("/clear did not reset Skill UI state: status=%#v messages=%q", model.status, model.messages.View())
	}

	model = runCommandInput(t, model, "ordinary request after clear")
	requests := providerImpl.Requests()
	if len(requests) != 2 || requests[0].Model != "skill-model" || requests[1].Model != "fake" {
		t.Fatalf("/clear did not restore the default request model: %#v", requests)
	}
	if requestContainsDynamic(requests[1], "commit SOP") {
		t.Fatalf("/clear left the shared SOP active in the next request: %#v", requests[1].DynamicSystem)
	}
}

func TestExecuteSharedSkillFailuresDoNotAppendUserMessages(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{name: "alpha", mode: skill.ModeShared, model: "model-a", body: "alpha SOP"})
	writeAppSkill(t, root, appSkillSpec{name: "beta", mode: skill.ModeShared, model: "model-b", body: "beta SOP"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)
	model = runCommandInput(t, model, "/alpha baseline")

	beforeMessages := append([]conversation.Message(nil), model.conversation.Messages...)
	beforeActivity := model.skillActivity.Snapshot()
	beforeRequests := len(providerImpl.Requests())
	oversized := strings.Repeat("x", skill.DefaultMaxArgsBytes+1)
	testCases := []struct {
		name      string
		skillName string
		args      string
		raw       string
	}{
		{name: "unknown Skill", skillName: "missing", raw: "/missing"},
		{name: "arguments over limit", skillName: "alpha", args: oversized, raw: "/alpha " + oversized},
		{name: "model conflict", skillName: "beta", raw: "/beta"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cmd, err := model.executeSkill(testCase.skillName, testCase.args, testCase.raw)
			if err == nil || cmd != nil {
				t.Fatalf("invalid Skill invocation unexpectedly started: cmd=%v err=%v", cmd != nil, err)
			}
			if !reflect.DeepEqual(model.conversation.Messages, beforeMessages) {
				t.Fatalf("failed Skill invocation appended a user message: before=%#v after=%#v", beforeMessages, model.conversation.Messages)
			}
			if got := model.skillActivity.Snapshot(); !reflect.DeepEqual(got, beforeActivity) {
				t.Fatalf("failed Skill invocation changed Activity: before=%#v after=%#v", beforeActivity, got)
			}
			if got := len(providerImpl.Requests()); got != beforeRequests {
				t.Fatalf("failed Skill invocation reached Provider: before=%d after=%d", beforeRequests, got)
			}
		})
	}
}

func TestSkillActivationConflictIsAtomic(t *testing.T) {
	root := t.TempDir()
	writeAppSkill(t, root, appSkillSpec{name: "alpha", mode: skill.ModeShared, model: "model-a", body: "alpha SOP"})
	writeAppSkill(t, root, appSkillSpec{name: "beta", mode: skill.ModeShared, model: "model-b", body: "beta SOP"})
	providerImpl := &skillAppProvider{}
	model := newSkillAppModel(t, root, providerImpl)
	model = runCommandInput(t, model, "/alpha")
	beforeMessages := append([]conversation.Message(nil), model.conversation.Messages...)

	model = runCommandInput(t, model, "/beta")
	active := model.skillActivity.Snapshot().Active
	if len(active) != 1 || active[0].Name != "alpha" || !reflect.DeepEqual(model.conversation.Messages, beforeMessages) {
		t.Fatalf("failed activation changed state: active=%#v messages=%#v", active, model.conversation.Messages)
	}
	if len(providerImpl.Requests()) != 1 || model.status.Error == nil {
		t.Fatalf("failed activation reached provider or hid error: requests=%d error=%v", len(providerImpl.Requests()), model.status.Error)
	}
}

func TestCloseCleansBuiltinSkillResources(t *testing.T) {
	projectRoot := t.TempDir()
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	manager, err := skill.NewManager(skill.ManagerOptions{
		Sources: []skill.SourceFS{skill.BuiltinSource()}, ToolNames: registry.Names(), ReservedCommand: appReservedCommands(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{
		Config: cfg, Provider: &skillAppProvider{}, Store: &fakeRecoveringStore{}, Resources: fakeResources{},
		Registry: registry, Executor: tool.NewExecutor(registry, projectRoot, time.Second, 1024), SkillManager: manager,
	})
	model = runCommandInput(t, model, "/commit")
	active := model.skillActivity.Snapshot().Active
	if len(active) != 1 {
		t.Fatalf("builtin Skill was not active: %#v", active)
	}
	materializedRoot := active[0].PackageRoot
	if _, err := os.Stat(materializedRoot); err != nil {
		t.Fatalf("builtin package was not materialized: %v", err)
	}
	model.close()
	if _, err := os.Stat(materializedRoot); !os.IsNotExist(err) {
		t.Fatalf("builtin package was not cleaned: %v", err)
	}
}

func TestClearCleansBuiltinSkillResources(t *testing.T) {
	projectRoot := t.TempDir()
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	manager, err := skill.NewManager(skill.ManagerOptions{
		Sources: []skill.SourceFS{skill.BuiltinSource()}, ToolNames: registry.Names(), ReservedCommand: appReservedCommands(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{
		Config: cfg, Provider: &skillAppProvider{}, Store: &fakeRecoveringStore{}, Resources: fakeResources{},
		Registry: registry, Executor: tool.NewExecutor(registry, projectRoot, time.Second, 1024), SkillManager: manager,
	})
	model = runCommandInput(t, model, "/commit")
	active := model.skillActivity.Snapshot().Active
	if len(active) != 1 {
		t.Fatalf("builtin Skill was not active: %#v", active)
	}
	materializedRoot := active[0].PackageRoot
	model = runCommandInput(t, model, "/clear")
	if len(model.skillActivity.Snapshot().Active) != 0 {
		t.Fatalf("clear retained active Skill: %#v", model.skillActivity.Snapshot())
	}
	if _, err := os.Stat(materializedRoot); !os.IsNotExist(err) {
		t.Fatalf("clear did not clean builtin package: %v", err)
	}
}
