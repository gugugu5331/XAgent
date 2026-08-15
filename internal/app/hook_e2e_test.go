//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	agentevents "xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

// These tests deliberately drive the App update loop rather than calling Hook
// actions directly. Everything outside the process boundary is deterministic:
// Provider responses are scripted, command execution is injected, and HTTP is
// restricted to an httptest loopback listener.

func TestHookE2EDriverSmoke(t *testing.T) {
	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: `version: 1
hooks: []
`,
		Scripts: [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: appSafeText("smoke complete")},
			{Type: provider.StreamEventDone},
		}},
	})
	driver := hookE2EDriver{model: &fixture.model}
	conversationRef := fixture.model.conversation
	driver.submit(t, "smoke request", hookE2EConfirmNone)

	if driver.countEvent(agentevents.Done) != 1 || driver.countEvent(agentevents.Error) != 0 {
		t.Fatalf("smoke terminal events = %#v", driver.events)
	}
	if conversationRef == nil || len(conversationRef.Messages) != 2 || conversationRef.Messages[0].Content.Text() != "smoke request" || conversationRef.Messages[1].Content.Text() != "smoke complete" {
		t.Fatalf("smoke conversation = %#v", conversationRef)
	}
	requests := fixture.provider.Requests()
	if len(requests) != 1 || len(requests[0].System) != 0 || requests[0].Observer != nil {
		t.Fatalf("empty Hook config did not use the legacy request path: %#v", requests)
	}
	if fixture.collector.Count() != 0 {
		t.Fatalf("empty Hook config emitted diagnostics: %#v", fixture.collector.List())
	}
	fixture.close(t)
}

func TestHookE2ELifecyclePrompt(t *testing.T) {
	runner := &hookE2ECommandRunner{}
	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: `version: 1
hooks:
  - event: system_start
    action: {type: command, command: record}
  - event: session_start
    action: {type: command, command: record}
  - event: session_start
    action: {type: prompt, content: E2E_SESSION_PROMPT, scope: session}
  - event: turn_start
    action: {type: command, command: record}
  - event: turn_start
    action: {type: prompt, content: E2E_NEXT_PROMPT, scope: next}
  - event: turn_start
    action: {type: prompt, content: E2E_TURN_PROMPT, scope: turn}
  - event: message_before
    action: {type: command, command: record}
  - event: message_after
    action: {type: command, command: record}
  - event: turn_end
    action: {type: command, command: record}
  - event: session_end
    action: {type: command, command: record}
  - event: system_stop
    action: {type: command, command: record}
`,
		CommandRunner: runner,
		Scripts: [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{ID: "read-lifecycle", Name: "Read", ArgumentsJSON: appSafeText(`{"path":"seed.txt"}`)}}},
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("lifecycle complete")}, {Type: provider.StreamEventDone}},
		},
	})
	if err := os.WriteFile(filepath.Join(fixture.projectRoot, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := hookE2EDriver{model: &fixture.model}
	conversationRef := fixture.model.conversation
	driver.submit(t, "exercise lifecycle", hookE2EConfirmNone)

	requests := fixture.provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider iterations = %d, want 2", len(requests))
	}
	for index, request := range requests {
		wantHooks := "E2E_SESSION_PROMPT,E2E_NEXT_PROMPT,E2E_TURN_PROMPT"
		if index == 1 {
			wantHooks = "E2E_SESSION_PROMPT,E2E_TURN_PROMPT"
		}
		if gotHooks := strings.Join(hookE2EHookBlockContents(request), ","); gotHooks != wantHooks {
			t.Fatalf("iteration %d Hook blocks = %q, want %q", index+1, gotHooks, wantHooks)
		}
		firstHook := hookE2EFirstHookBlock(request)
		if firstHook <= 0 {
			t.Fatalf("Hook blocks were not placed after fixed safety blocks: %#v", request.System)
		}
	}

	fixture.close(t)
	contexts := hookE2EDecodeCommandEvents(t, runner.Requests())
	wantOrder := []hook.Event{
		hook.EventSystemStart,
		hook.EventSessionStart,
		hook.EventTurnStart,
		hook.EventMessageBefore,
		hook.EventMessageAfter,
		hook.EventMessageBefore,
		hook.EventMessageAfter,
		hook.EventTurnEnd,
		hook.EventSessionEnd,
		hook.EventSystemStop,
	}
	if got := hookE2EEventNames(contexts); !hookE2EEventSequenceEqual(got, wantOrder) {
		t.Fatalf("lifecycle event order:\n got: %v\nwant: %v", got, wantOrder)
	}
	if contexts[3].Message == nil || contexts[4].Message == nil || contexts[3].Message.ID != contexts[4].Message.ID || contexts[3].Message.Role != hook.MessageUser {
		t.Fatalf("user message identity was not preserved: before=%#v after=%#v", contexts[3].Message, contexts[4].Message)
	}
	if contexts[5].Message == nil || contexts[6].Message == nil || contexts[5].Message.ID != contexts[6].Message.ID || contexts[5].Message.Role != hook.MessageAssistant {
		t.Fatalf("assistant message identity was not preserved: before=%#v after=%#v", contexts[5].Message, contexts[6].Message)
	}
	if contexts[7].Turn == nil || contexts[7].Turn.Status != hook.TurnCompleted {
		t.Fatalf("turn did not complete before terminal/close: %#v", contexts[7].Turn)
	}
	if conversationRef == nil || len(conversationRef.Messages) != 4 {
		t.Fatalf("two-iteration conversation was not persisted: %#v", conversationRef)
	}

	ref := hook.ExecutionRef{
		SessionID:   contexts[2].Session.ID,
		ExecutionID: contexts[2].Execution.ID,
		TurnID:      contexts[2].Turn.ID,
		Kind:        contexts[2].Execution.Kind,
		Mode:        contexts[2].Execution.Mode,
	}
	lease, err := fixture.engine.AcquirePrompts(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if blocks := lease.Blocks(); len(blocks) != 0 {
		t.Fatalf("turn/session close retained prompt blocks: %#v", blocks)
	}
	lease.Release()
}

func TestHookE2EActionTransport(t *testing.T) {
	var transportMu sync.Mutex
	transportOrder := []string{}
	appendTransport := func(value string) {
		transportMu.Lock()
		transportOrder = append(transportOrder, value)
		transportMu.Unlock()
	}
	runner := &hookE2ECommandRunner{run: func(_ context.Context, request hook.CommandRequest) (hook.CommandResult, error) {
		appendTransport("command")
		return hook.CommandResult{Stdout: []byte("COMMAND_PRIVATE_OUTPUT")}, nil
	}}
	type capturedHTTP struct {
		method string
		header string
		body   []byte
	}
	var httpMu sync.Mutex
	var captured []capturedHTTP
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read loopback body: %v", err)
		}
		httpMu.Lock()
		captured = append(captured, capturedHTTP{method: request.Method, header: request.Header.Get("X-E2E"), body: append([]byte(nil), body...)})
		httpMu.Unlock()
		appendTransport("http")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("HTTP_PRIVATE_RESPONSE"))
	}))
	defer server.Close()

	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: fmt.Sprintf(`version: 1
hooks:
  - event: tool_before
    action:
      type: command
      command: capture-event
      env:
        E2E_MARK: stable
  - event: tool_before
    action:
      type: http
      url: %q
      method: POST
      headers:
        X-E2E: transport
`, server.URL),
		CommandRunner: runner,
		Scripts: [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{
				ID:            "typed-transport",
				Name:          "Read",
				ArgumentsJSON: appSafeText(`{"path":"payload.txt","text":"alpha","number":9007199254740993,"flag":true,"object":{"key":"value"},"array":[1,"two",false]}`),
			}}},
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("transport complete")}, {Type: provider.StreamEventDone}},
		},
	})
	if err := os.WriteFile(filepath.Join(fixture.projectRoot, "payload.txt"), []byte("read payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := hookE2EDriver{model: &fixture.model}
	conversationRef := fixture.model.conversation
	driver.submit(t, "transport request", hookE2EConfirmNone)

	commandRequests := runner.Requests()
	if len(commandRequests) != 1 || commandRequests[0].ProjectRoot != fixture.projectRoot || commandRequests[0].Environment["E2E_MARK"] != "stable" {
		t.Fatalf("command transport request = %#v", commandRequests)
	}
	httpMu.Lock()
	httpCapture := append([]capturedHTTP(nil), captured...)
	httpMu.Unlock()
	if len(httpCapture) != 1 || httpCapture[0].method != http.MethodPost || httpCapture[0].header != "transport" {
		t.Fatalf("HTTP transport request = %#v", httpCapture)
	}
	if !bytes.Equal(commandRequests[0].EventJSON, httpCapture[0].body) {
		t.Fatalf("command and HTTP received different EventContext:\ncommand=%s\nhttp=%s", commandRequests[0].EventJSON, httpCapture[0].body)
	}
	transportMu.Lock()
	order := append([]string(nil), transportOrder...)
	transportMu.Unlock()
	if strings.Join(order, ",") != "command,http" {
		t.Fatalf("rule transport order = %v", order)
	}

	payload := hookE2EDecodeObject(t, commandRequests[0].EventJSON)
	if payload["event"] != string(hook.EventToolBefore) {
		t.Fatalf("unexpected transported event: %#v", payload["event"])
	}
	toolPayload, ok := payload["tool"].(map[string]any)
	if !ok {
		t.Fatalf("missing tool payload: %#v", payload)
	}
	arguments, ok := toolPayload["arguments"].(map[string]any)
	if !ok || arguments["number"] != json.Number("9007199254740993") || arguments["flag"] != true {
		t.Fatalf("typed tool arguments were not preserved: %#v", arguments)
	}
	if _, ok := arguments["object"].(map[string]any); !ok {
		t.Fatalf("object argument lost its type: %#v", arguments["object"])
	}
	if values, ok := arguments["array"].([]any); !ok || len(values) != 3 || values[0] != json.Number("1") || values[2] != false {
		t.Fatalf("array argument lost its type: %#v", arguments["array"])
	}
	conversationJSON, err := json.Marshal(conversationRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"COMMAND_PRIVATE_OUTPUT", "HTTP_PRIVATE_RESPONSE"} {
		if bytes.Contains(conversationJSON, []byte(private)) || strings.Contains(fixture.model.View(), private) {
			t.Fatalf("Hook private transport output leaked into conversation/UI: %s", private)
		}
	}
	fixture.close(t)
}

func TestHookE2EToolDecision(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		decision         string
		confirmation     hookE2EConfirmation
		wantConfirmation int
		wantExecuted     bool
	}{
		{name: "allow continues ordinary confirmation", decision: `{"decision":"allow"}`, confirmation: hookE2EConfirmAllow, wantConfirmation: 1, wantExecuted: true},
		{name: "deny short circuits confirmation and handler", decision: `{"decision":"deny","reason":"blocked by deterministic E2E policy"}`, confirmation: hookE2EConfirmDeny, wantConfirmation: 0, wantExecuted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &hookE2ECommandRunner{run: func(_ context.Context, request hook.CommandRequest) (hook.CommandResult, error) {
				if request.Command == "decide" {
					return hook.CommandResult{Stdout: []byte(testCase.decision)}, nil
				}
				return hook.CommandResult{}, nil
			}}
			fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
				YAML: `version: 1
hooks:
  - event: tool_before
    action:
      type: command
      command: decide
      decision: true
  - event: tool_after
    action:
      type: command
      command: record-after
`,
				PermissionMode: "default",
				CommandRunner:  runner,
				Scripts: [][]provider.StreamEvent{
					{{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{ID: "write-decision", Name: "Write", ArgumentsJSON: appSafeText(`{"path":"decision.txt","content":"executed"}`)}}},
					{{Type: provider.StreamEventTextDelta, Delta: appSafeText("decision complete")}, {Type: provider.StreamEventDone}},
				},
			})
			driver := hookE2EDriver{model: &fixture.model}
			conversationRef := fixture.model.conversation
			driver.submit(t, "decision request", testCase.confirmation)

			if driver.confirmations != testCase.wantConfirmation {
				t.Fatalf("confirmation count = %d, want %d; events=%#v", driver.confirmations, testCase.wantConfirmation, driver.events)
			}
			calls := runner.Requests()
			commands := make([]string, 0, len(calls))
			for _, call := range calls {
				commands = append(commands, call.Command)
			}
			if testCase.wantExecuted {
				if strings.Join(commands, ",") != "decide,record-after" {
					t.Fatalf("allow action chain = %v", commands)
				}
				content, err := os.ReadFile(filepath.Join(fixture.projectRoot, "decision.txt"))
				if err != nil || string(content) != "executed" {
					t.Fatalf("allowed handler result: content=%q err=%v", content, err)
				}
			} else {
				if strings.Join(commands, ",") != "decide" {
					t.Fatalf("deny reached tool_after: %v", commands)
				}
				if _, err := os.Stat(filepath.Join(fixture.projectRoot, "decision.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("denied handler touched target: %v", err)
				}
				var denied *conversation.Message
				for index := range conversationRef.Messages {
					message := &conversationRef.Messages[index]
					if message.Role == conversation.RoleToolResult && message.Tool != nil && message.Tool.CallID == "write-decision" {
						denied = message
						break
					}
				}
				if denied == nil || denied.Tool == nil || denied.Tool.Error == nil || denied.Tool.Error.Code != tool.ErrHookDenied || denied.Tool.Status != tool.StatusDenied || !strings.Contains(denied.Content.Text(), "blocked by deterministic E2E policy") || !denied.Tool.Error.Recoverable {
					t.Fatalf("hook_denied result did not flow back to the model: %#v", denied)
				}
			}
			if driver.countEvent(agentevents.Done) != 1 || driver.countEvent(agentevents.Error) != 0 {
				t.Fatalf("decision turn did not complete: %#v", driver.events)
			}
			fixture.close(t)
		})
	}
}

func TestHookE2EAsyncOnce(t *testing.T) {
	runner := newHookE2EBarrierRunner()
	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: `version: 1
hooks:
  - event: turn_start
    once: true
    async: true
    timeout: 10s
    action:
      type: command
      command: async-once
`,
		CommandRunner: runner,
	})

	const concurrentTurns = 24
	start := make(chan struct{})
	var group sync.WaitGroup
	errorsSeen := make(chan error, concurrentTurns)
	group.Add(concurrentTurns)
	for index := 0; index < concurrentTurns; index++ {
		index := index
		go func() {
			defer group.Done()
			<-start
			conv := conversation.NewConversation(fmt.Sprintf("async-%d", index), time.Unix(10, 0))
			stream, err := fixture.model.orchestrator.SendRequest(context.Background(), conv, orchestratorRunRequest("async once"))
			if err != nil {
				errorsSeen <- err
				return
			}
			doneCount := 0
			for event := range stream {
				if event.Type == agentevents.Error {
					errorsSeen <- event.Err
					return
				}
				if event.Type == agentevents.Done {
					doneCount++
				}
			}
			if doneCount != 1 {
				errorsSeen <- fmt.Errorf("terminal Done events = %d, want 1", doneCount)
			}
		}()
	}
	close(start)
	waitHookE2EChannel(t, runner.started, "async once action start")
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent turn: %v", err)
		}
	}
	if got := runner.Count(); got != 1 {
		t.Fatalf("async once side effects = %d, want 1", got)
	}

	if err := fixture.model.Close(context.Background()); err != nil {
		t.Fatalf("App.Close: %v", err)
	}
	select {
	case <-runner.finished:
		t.Fatal("App.Close incorrectly shut down the process-owned Hook worker")
	default:
	}
	shutdownResult := make(chan error, 1)
	shutdownCalled := make(chan struct{})
	go func() {
		close(shutdownCalled)
		shutdownResult <- fixture.engine.Shutdown(context.Background())
	}()
	waitHookE2EChannel(t, shutdownCalled, "Hook.Shutdown call")
	select {
	case err := <-shutdownResult:
		close(runner.release)
		waitHookE2EChannel(t, runner.finished, "accepted async action cleanup")
		t.Fatalf("Hook.Shutdown returned before accepted work finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(runner.release)
	waitHookE2EChannel(t, runner.finished, "accepted async action finish")
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Hook.Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hook.Shutdown did not join accepted work")
	}
	if got := runner.Count(); got != 1 {
		t.Fatalf("shutdown duplicated async once side effect: %d", got)
	}
	if err := fixture.engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Hook.Shutdown: %v", err)
	}
}

func TestHookE2EIsolatedSkill(t *testing.T) {
	runner := &hookE2ECommandRunner{}
	skillDir := t.TempDir()
	writeHookE2ESkill(t, skillDir, "review", skill.ModeIsolated, "ISOLATED SOP {{args}}")
	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: `version: 1
hooks:
  - event: turn_start
    action: {type: command, command: lifecycle}
  - event: turn_start
    action: {type: prompt, content: "EXEC={{execution.kind}}", scope: turn}
  - event: message_before
    action: {type: command, command: lifecycle}
  - event: message_after
    action: {type: command, command: lifecycle}
  - event: turn_end
    action: {type: command, command: lifecycle}
`,
		CommandRunner: runner,
		SkillDir:      skillDir,
		Scripts: [][]provider.StreamEvent{
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("main answer")}, {Type: provider.StreamEventDone}},
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("isolated summary")}, {Type: provider.StreamEventDone}},
		},
	})
	driver := hookE2EDriver{model: &fixture.model}
	conversationRef := fixture.model.conversation
	driver.submit(t, "main request", hookE2EConfirmNone)
	driver.submit(t, "/review inspect target", hookE2EConfirmNone)

	requests := fixture.provider.Requests()
	if len(requests) != 2 || !hookE2ERequestContains(requests[0], "EXEC=main") || hookE2ERequestContains(requests[0], "EXEC=isolated_skill") || !hookE2ERequestContains(requests[1], "EXEC=isolated_skill") || hookE2ERequestContains(requests[1], "EXEC=main") {
		t.Fatalf("main/isolated prompt state crossed execution boundaries: %#v", requests)
	}
	if conversationRef == nil || len(conversationRef.Messages) != 4 {
		t.Fatalf("isolated summary bridge changed main history shape: %#v", conversationRef)
	}
	wantMain := []struct {
		role    conversation.MessageRole
		content string
	}{
		{conversation.RoleUser, "main request"},
		{conversation.RoleAssistant, "main answer"},
		{conversation.RoleUser, "/review inspect target"},
		{conversation.RoleAssistant, "isolated summary"},
	}
	for index, want := range wantMain {
		message := conversationRef.Messages[index]
		if message.Role != want.role || message.Content.Text() != want.content {
			t.Fatalf("main history[%d] = %#v, want %#v", index, message, want)
		}
	}

	contexts := hookE2EDecodeCommandEvents(t, runner.Requests())
	turnKinds := []hook.ExecutionKind{}
	messageBefore := map[hook.ExecutionKind]int{}
	messageAfter := map[hook.ExecutionKind]int{}
	turnEnds := map[hook.ExecutionKind]int{}
	for _, event := range contexts {
		if event.Execution == nil {
			continue
		}
		switch event.Event {
		case hook.EventTurnStart:
			turnKinds = append(turnKinds, event.Execution.Kind)
		case hook.EventMessageBefore:
			messageBefore[event.Execution.Kind]++
		case hook.EventMessageAfter:
			messageAfter[event.Execution.Kind]++
		case hook.EventTurnEnd:
			turnEnds[event.Execution.Kind]++
		}
	}
	if len(turnKinds) != 2 || turnKinds[0] != hook.ExecutionMain || turnKinds[1] != hook.ExecutionIsolatedSkill {
		t.Fatalf("turn execution kinds = %v", turnKinds)
	}
	for _, kind := range []hook.ExecutionKind{hook.ExecutionMain, hook.ExecutionIsolatedSkill} {
		if messageBefore[kind] != 2 || messageAfter[kind] != 2 || turnEnds[kind] != 1 {
			t.Fatalf("%s lifecycle counts: before=%d after=%d end=%d", kind, messageBefore[kind], messageAfter[kind], turnEnds[kind])
		}
	}
	fixture.close(t)
}

func TestHookE2EFailureDiagnostics(t *testing.T) {
	const privateFailure = "HOOK_PRIVATE_TIMEOUT_CANARY"
	var attempts atomic.Int64
	runner := &hookE2ECommandRunner{run: func(ctx context.Context, _ hook.CommandRequest) (hook.CommandResult, error) {
		if attempts.Add(1) > 1 {
			return hook.CommandResult{}, nil
		}
		<-ctx.Done()
		return hook.CommandResult{}, errors.New(privateFailure)
	}}
	fixture := newHookE2EFixture(t, hookE2EFixtureOptions{
		YAML: `version: 1
hooks:
  - event: turn_start
    once: true
    timeout: 10ms
    action:
      type: command
      command: controlled-timeout
`,
		CommandRunner: runner,
		Scripts: [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{ID: "failure-read", Name: "Read", ArgumentsJSON: appSafeText(`{"path":"failure-seed.txt"}`)}}},
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("agent survived timeout")}, {Type: provider.StreamEventDone}},
			{{Type: provider.StreamEventTextDelta, Delta: appSafeText("agent remained isolated")}, {Type: provider.StreamEventDone}},
		},
	})
	if err := os.WriteFile(filepath.Join(fixture.projectRoot, "failure-seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := hookE2EDriver{model: &fixture.model}
	conversationRef := fixture.model.conversation
	driver.submit(t, "trigger controlled failure", hookE2EConfirmNone)

	if driver.countEvent(agentevents.Done) != 1 || driver.countEvent(agentevents.Error) != 0 || fixture.model.status.Error != nil {
		t.Fatalf("Hook timeout did not fail open: events=%#v status=%v", driver.events, fixture.model.status.Error)
	}
	items := fixture.collector.List()
	if len(items) != 1 || items[0].Code != hook.DiagnosticActionTimeout || strings.Contains(items[0].Text(), privateFailure) {
		t.Fatalf("timeout diagnostic was missing or unsafe: %#v", items)
	}
	beforeDiagnosticsView := fixture.model.View()
	conversationJSON, err := json.Marshal(conversationRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{hook.DiagnosticActionTimeout, privateFailure} {
		if strings.Contains(beforeDiagnosticsView, forbidden) || bytes.Contains(conversationJSON, []byte(forbidden)) {
			t.Fatalf("diagnostic leaked before /diagnostics: %q", forbidden)
		}
	}

	driver.submit(t, "/diagnostics", hookE2EConfirmNone)
	if !strings.Contains(fixture.model.status.Notice, hook.DiagnosticActionTimeout) || strings.Contains(fixture.model.status.Notice, privateFailure) {
		t.Fatalf("/diagnostics visibility was not local and safe: %q", fixture.model.status.Notice)
	}
	driver.submit(t, "continue after viewing diagnostics", hookE2EConfirmNone)
	if driver.countEvent(agentevents.Done) != 2 || driver.countEvent(agentevents.Error) != 0 {
		t.Fatalf("post-diagnostics turn did not complete: %#v", driver.events)
	}
	requests := fixture.provider.Requests()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(requests))
	}
	for index, request := range requests {
		for _, forbidden := range []string{hook.DiagnosticActionTimeout, privateFailure} {
			if hookE2ERequestContains(request, forbidden) {
				t.Fatalf("diagnostic leaked into Provider request %d: %q", index+1, forbidden)
			}
		}
	}
	conversationJSON, err = json.Marshal(conversationRef)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(conversationJSON, []byte(hook.DiagnosticActionTimeout)) || bytes.Contains(conversationJSON, []byte(privateFailure)) {
		t.Fatalf("diagnostic leaked into Conversation: %s", conversationJSON)
	}
	if len(runner.Requests()) != 2 {
		t.Fatalf("failed once action was not retried exactly once: %d attempts", len(runner.Requests()))
	}
	fixture.close(t)
}

type hookE2EFixtureOptions struct {
	YAML           string
	Scripts        [][]provider.StreamEvent
	CommandRunner  hook.CommandRunner
	HTTPRunner     hook.HTTPRunner
	PermissionMode string
	SkillDir       string
}

type hookE2EFixture struct {
	model       Model
	engine      *hook.Engine
	provider    *hookE2EProvider
	collector   *diagnostics.Collector
	projectRoot string
	closed      atomic.Bool
}

func newHookE2EFixture(t *testing.T, options hookE2EFixtureOptions) *hookE2EFixture {
	t.Helper()
	projectRoot := t.TempDir()
	homeDir := t.TempDir()
	hookDir := filepath.Join(projectRoot, ".xagent")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte(options.YAML), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeRedactor := redact.NewRuntimeRedactor()
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
	snapshot, err := hook.Load(hook.LoadOptions{
		HomeDir: homeDir, ProjectRoot: projectRoot, Redactor: runtimeRedactor,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("load Hook fixture: %v", err)
	}
	var idSequence atomic.Uint64
	engine, err := hook.NewEngine(snapshot, hook.EngineOptions{
		ProjectRoot:       projectRoot,
		LegacyDiagnostics: collector,
		Redactor:          runtimeRedactor,
		CommandRunner:     options.CommandRunner,
		HTTPRunner:        options.HTTPRunner,
		Clock:             func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		IDSource:          func() string { return "e2e-" + strconv.FormatUint(idSequence.Add(1), 10) },
		AsyncWorkers:      2,
		AsyncQueue:        32,
		ShutdownGrace:     500 * time.Millisecond,
		ShutdownJoinGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct Hook engine: %v", err)
	}
	providerImpl := &hookE2EProvider{scripts: hookE2ECloneScripts(options.Scripts)}
	store := &hookE2EStore{}
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	var skillManager *skill.Manager
	if options.SkillDir != "" {
		if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
			t.Fatal(err)
		}
		skillManager, err = skill.NewManager(skill.ManagerOptions{
			Sources:         []skill.SourceFS{{Source: skill.SourceProject, Root: options.SkillDir}},
			ToolNames:       registry.Names(),
			ReservedCommand: hookE2EReservedCommands(),
			Redact:          runtimeRedactor.Text,
		})
		if err != nil {
			t.Fatalf("construct Skill manager: %v", err)
		}
	}
	permissionMode := options.PermissionMode
	if permissionMode == "" {
		permissionMode = "permissive"
	}
	cfg := &config.AppConfig{
		LLM:        config.LLMConfig{Model: "e2e-model", RequestTimeoutMS: 2_000},
		UI:         config.UIConfig{StartMode: config.StartModeNew},
		Permission: config.PermissionConfig{Mode: permissionMode},
		Agent:      config.AgentConfig{MaxIterations: 4, MaxUnknownToolCalls: 2},
	}
	if err := os.Mkdir(filepath.Join(projectRoot, ".xagent"), 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	openedProject, err := safefs.Bootstrap(projectRoot, tool.ProjectFilesystemPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := openedProject.Root.Close(); err != nil {
			t.Error(err)
		}
	})
	executor := tool.NewExecutorWithWriteAccess(registry, projectRoot, time.Second, 64*1024, openedProject.Root, openedProject.Capabilities.Ordinary())
	model := New(Deps{
		Config: cfg, Provider: providerImpl, Store: store, Resources: hookE2EResources{},
		Registry: registry, Executor: executor, Diagnostics: collector, SkillManager: skillManager,
		Redact: runtimeRedactor.Text, RedactionLookbehind: runtimeRedactor.MaxSecretBytes(), Hooks: engine,
	})
	fixture := &hookE2EFixture{model: model, engine: engine, provider: providerImpl, collector: collector, projectRoot: projectRoot}
	t.Cleanup(func() {
		appCtx, cancelApp := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelApp()
		if err := fixture.model.Close(appCtx); err != nil {
			t.Errorf("cleanup App.Close: %v", err)
		}
		hookCtx, cancelHook := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelHook()
		if err := fixture.engine.Shutdown(hookCtx); err != nil {
			t.Errorf("cleanup Hook.Shutdown: %v", err)
		}
	})
	return fixture
}

func (f *hookE2EFixture) close(t *testing.T) {
	t.Helper()
	if !f.closed.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.model.Close(ctx); err != nil {
		t.Fatalf("App.Close: %v", err)
	}
	if err := f.engine.Shutdown(ctx); err != nil {
		t.Fatalf("Hook.Shutdown: %v", err)
	}
}

type hookE2EConfirmation uint8

const (
	hookE2EConfirmNone hookE2EConfirmation = iota
	hookE2EConfirmAllow
	hookE2EConfirmDeny
)

type hookE2EDriver struct {
	model         *Model
	events        []agentevents.Event
	confirmations int
}

func (d *hookE2EDriver) submit(t *testing.T, input string, confirmation hookE2EConfirmation) {
	t.Helper()
	if d.model == nil {
		t.Fatal("nil E2E model")
	}
	d.model.input.SetValue(input)
	updated, cmd := d.model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	*d.model = updated.(Model)
	for steps := 0; ; steps++ {
		if steps > 256 {
			t.Fatal("E2E driver exceeded 256 update steps")
		}
		if d.model.confirmation != nil {
			d.confirmations++
			key := "n"
			if confirmation == hookE2EConfirmAllow {
				key = "y"
			}
			confirmationModel, confirmationCmd := d.model.Update(keyMsg(key))
			*d.model = confirmationModel.(Model)
			if confirmationCmd != nil {
				t.Fatal("confirmation unexpectedly replaced the active stream listener")
			}
		}
		if cmd == nil {
			break
		}
		message := hookE2ERunCommand(t, d.model, cmd)
		if delivered, ok := message.(eventMsg); ok {
			d.events = append(d.events, delivered.event)
		}
		updated, cmd = d.model.Update(message)
		*d.model = updated.(Model)
	}
	if d.model.streaming {
		t.Fatalf("input %q left the App streaming", input)
	}
}

func (d *hookE2EDriver) countEvent(eventType agentevents.Type) int {
	count := 0
	for _, event := range d.events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func hookE2ERunCommand(t *testing.T, model *Model, cmd tea.Cmd) tea.Msg {
	t.Helper()
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case message := <-result:
		return message
	case <-time.After(2 * time.Second):
		if model != nil && model.lifecycle != nil {
			model.lifecycle.cancel()
		}
		t.Fatal("timed out waiting for App command")
		return nil
	}
}

type hookE2EProvider struct {
	mu       sync.Mutex
	scripts  [][]provider.StreamEvent
	requests []provider.ChatRequest
	next     int
	tracker  appTestStreamTracker
}

func (*hookE2EProvider) Name() string { return "hook-e2e-scripted" }

func (p *hookE2EProvider) StreamChat(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, hookE2ECloneRequest(request))
	index := p.next
	p.next++
	events := []provider.StreamEvent{{Type: provider.StreamEventTextDelta, Delta: appSafeText("script default")}, {Type: provider.StreamEventDone}}
	if index < len(p.scripts) {
		events = append([]provider.StreamEvent(nil), p.scripts[index]...)
	}
	p.mu.Unlock()
	if request.Observer != nil {
		request.Observer.MarkSent()
		request.Observer.Finish(true)
	}
	return newAppTestChatStream(events, &p.tracker), nil
}

func (p *hookE2EProvider) Requests() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]provider.ChatRequest, len(p.requests))
	for index, request := range p.requests {
		result[index] = hookE2ECloneRequest(request)
	}
	return result
}

func hookE2ECloneRequest(request provider.ChatRequest) provider.ChatRequest {
	request.System = append([]provider.SystemBlock(nil), request.System...)
	request.StableSystem = append([]provider.SystemBlock(nil), request.StableSystem...)
	request.DynamicSystem = append([]provider.SystemBlock(nil), request.DynamicSystem...)
	request.Messages = append([]provider.ModelMessage(nil), request.Messages...)
	request.Tools = append([]provider.ToolDefinition(nil), request.Tools...)
	return request
}

func hookE2ECloneScripts(scripts [][]provider.StreamEvent) [][]provider.StreamEvent {
	result := make([][]provider.StreamEvent, len(scripts))
	for index := range scripts {
		result[index] = append([]provider.StreamEvent(nil), scripts[index]...)
	}
	return result
}

type hookE2ECommandRunner struct {
	mu       sync.Mutex
	requests []hook.CommandRequest
	run      func(context.Context, hook.CommandRequest) (hook.CommandResult, error)
}

func (r *hookE2ECommandRunner) Run(ctx context.Context, request hook.CommandRequest) (hook.CommandResult, error) {
	request.EventJSON = append([]byte(nil), request.EventJSON...)
	request.Environment = hookE2ECloneStringMap(request.Environment)
	r.mu.Lock()
	r.requests = append(r.requests, request)
	run := r.run
	r.mu.Unlock()
	if run != nil {
		return run(ctx, request)
	}
	return hook.CommandResult{}, nil
}

func (r *hookE2ECommandRunner) Requests() []hook.CommandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]hook.CommandRequest, len(r.requests))
	for index, request := range r.requests {
		request.EventJSON = append([]byte(nil), request.EventJSON...)
		request.Environment = hookE2ECloneStringMap(request.Environment)
		result[index] = request
	}
	return result
}

type hookE2EBarrierRunner struct {
	count    atomic.Int64
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	start    sync.Once
	finish   sync.Once
}

func newHookE2EBarrierRunner() *hookE2EBarrierRunner {
	return &hookE2EBarrierRunner{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
}

func (r *hookE2EBarrierRunner) Run(ctx context.Context, _ hook.CommandRequest) (hook.CommandResult, error) {
	r.count.Add(1)
	r.start.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		r.finish.Do(func() { close(r.finished) })
		return hook.CommandResult{}, ctx.Err()
	case <-r.release:
		r.finish.Do(func() { close(r.finished) })
		return hook.CommandResult{}, nil
	}
}

func (r *hookE2EBarrierRunner) Count() int { return int(r.count.Load()) }

type hookE2EStore struct {
	mu      sync.Mutex
	created int
	active  *conversation.Conversation
}

func (s *hookE2EStore) List(context.Context) (conversation.ListResult, error) {
	return conversation.ListResult{}, nil
}
func (s *hookE2EStore) Load(_ context.Context, id string) (conversation.LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.ID != id {
		return conversation.LoadResult{}, fmt.Errorf("conversation %s not found", id)
	}
	return conversation.LoadResult{Conversation: s.active, Available: true, Recovery: conversation.RecoveryReport{Status: conversation.RecoveryClean}}, nil
}
func (s *hookE2EStore) Save(_ context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	s.mu.Lock()
	s.active = value
	s.mu.Unlock()
	return conversation.SaveResult{Kind: conversation.SaveNoop}, nil
}
func (s *hookE2EStore) Create(context.Context) (*conversation.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	s.active = conversation.NewConversation(fmt.Sprintf("e2e-session-%d", s.created), time.Unix(1_700_000_000, 0))
	return s.active, nil
}
func (s *hookE2EStore) Maintain(context.Context) (conversation.MaintenanceResult, error) {
	return conversation.MaintenanceResult{}, nil
}

type hookE2EResources struct{}

func (hookE2EResources) SystemPrompt() string      { return "E2E fixed safety prompt" }
func (hookE2EResources) UILabel(key string) string { return key }

func hookE2EDecodeCommandEvents(t *testing.T, requests []hook.CommandRequest) []hook.EventContext {
	t.Helper()
	result := make([]hook.EventContext, 0, len(requests))
	for _, request := range requests {
		var event hook.EventContext
		if err := json.Unmarshal(request.EventJSON, &event); err != nil {
			t.Fatalf("decode Hook EventContext: %v\npayload=%s", err, request.EventJSON)
		}
		result = append(result, event)
	}
	return result
}

func hookE2EEventNames(events []hook.EventContext) []hook.Event {
	result := make([]hook.Event, 0, len(events))
	for _, event := range events {
		result = append(result, event.Event)
	}
	return result
}

func hookE2EEventSequenceEqual(left, right []hook.Event) bool {
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

func hookE2EDecodeObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return result
}

func hookE2ERequestContains(request provider.ChatRequest, value string) bool {
	for _, blocks := range [][]provider.SystemBlock{request.System, request.StableSystem, request.DynamicSystem} {
		for _, block := range blocks {
			if strings.Contains(block.Content.Text(), value) {
				return true
			}
		}
	}
	for _, message := range request.Messages {
		if strings.Contains(message.Content.Text(), value) || strings.Contains(message.ArgumentsJSON.Text(), value) ||
			strings.Contains(message.ToolResult.Text(), value) || strings.Contains(message.ToolResultStatus, value) {
			return true
		}
	}
	return false
}

func hookE2EFirstHookBlock(request provider.ChatRequest) int {
	for index, block := range request.System {
		if strings.HasPrefix(block.Name, "hook:") {
			return index
		}
	}
	return -1
}

func hookE2EHookBlockContents(request provider.ChatRequest) []string {
	result := []string{}
	for _, block := range request.System {
		if strings.HasPrefix(block.Name, "hook:") {
			result = append(result, block.Content.Text())
		}
	}
	return result
}

func hookE2ECloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func hookE2EReservedCommands() []string {
	definitions := command.MustNew(command.Builtins()...).Definitions()
	result := make([]string, 0, len(definitions)*2)
	for _, definition := range definitions {
		result = append(result, definition.Name)
		result = append(result, definition.Aliases...)
	}
	return result
}

func writeHookE2ESkill(t *testing.T, root string, name string, mode skill.Mode, body string) {
	t.Helper()
	content := fmt.Sprintf("---\nname: %s\ndescription: deterministic E2E Skill\nmode: %s\nhistory: 0\n---\n%s\n", name, mode, body)
	if err := os.WriteFile(filepath.Join(root, name+".md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitHookE2EChannel(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

// Keep the async test independent of unexported orchestrator request fields
// while still driving the public Agent-loop entry point through the App-owned
// orchestrator.
func orchestratorRunRequest(text string) orchestrator.RunRequest {
	return orchestrator.RunRequest{UserText: text, Mode: orchestrator.RunModeDefault}
}
