package hook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"xagent/internal/diagnostics"
)

func testLookup(value string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		return value, name == "BOUNDARY"
	}
}

func TestCommandConfigLimits(t *testing.T) {
	limits := DefaultLimits()
	utf8ValueAtLimit := strings.Repeat("界", (limits.EnvValueBytes-2)/3) + "ab"
	if len(utf8ValueAtLimit) != limits.EnvValueBytes {
		t.Fatalf("test value is %d bytes", len(utf8ValueAtLimit))
	}
	envAtLimit := make(map[string]string, limits.CommandEnvCount)
	for index := 0; index < limits.CommandEnvCount; index++ {
		envAtLimit["K"+strconv.Itoa(index)] = "value"
	}
	envOverLimit := make(map[string]string, len(envAtLimit)+1)
	for key, value := range envAtLimit {
		envOverLimit[key] = value
	}
	envOverLimit["OVER_LIMIT"] = "value"
	keyAtLimit := "A" + strings.Repeat("k", limits.EnvKeyBytes-1)
	keyOverLimit := keyAtLimit + "k"

	cases := []struct {
		name    string
		config  ActionConfig
		lookup  func(string) (string, bool)
		wantErr bool
	}{
		{name: "command limit", config: ActionConfig{Type: ActionCommand, Command: strings.Repeat("x", limits.CommandBytes)}},
		{name: "command limit plus one", config: ActionConfig{Type: ActionCommand, Command: strings.Repeat("x", limits.CommandBytes+1)}, wantErr: true},
		{name: "env count limit", config: ActionConfig{Type: ActionCommand, Command: "true", Env: envAtLimit}},
		{name: "env count limit plus one", config: ActionConfig{Type: ActionCommand, Command: "true", Env: envOverLimit}, wantErr: true},
		{name: "env key limit", config: ActionConfig{Type: ActionCommand, Command: "true", Env: map[string]string{keyAtLimit: "value"}}},
		{name: "env key limit plus one", config: ActionConfig{Type: ActionCommand, Command: "true", Env: map[string]string{keyOverLimit: "value"}}, wantErr: true},
		{name: "expanded UTF-8 env value limit", config: ActionConfig{Type: ActionCommand, Command: "true", Env: map[string]string{"VALUE": "${BOUNDARY}"}}, lookup: testLookup(utf8ValueAtLimit)},
		{name: "expanded UTF-8 env value limit plus one", config: ActionConfig{Type: ActionCommand, Command: "true", Env: map[string]string{"VALUE": "${BOUNDARY}"}}, lookup: testLookup(utf8ValueAtLimit + "x"), wantErr: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			lookup := item.lookup
			if lookup == nil {
				lookup = func(string) (string, bool) { return "", false }
			}
			_, err := compileAction(EventSystemStart, false, Duration{}, item.config, limits, lookup, nil)
			if (err != nil) != item.wantErr {
				t.Fatalf("compile error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestHTTPHeaderConfigLimits(t *testing.T) {
	limits := DefaultLimits()
	utf8ValueAtLimit := strings.Repeat("界", (limits.HTTPHeaderValueBytes-2)/3) + "ab"
	if len(utf8ValueAtLimit) != limits.HTTPHeaderValueBytes {
		t.Fatalf("test value is %d bytes", len(utf8ValueAtLimit))
	}
	headersAtLimit := make(map[string]string, limits.HTTPHeaderCount)
	for index := 0; index < limits.HTTPHeaderCount; index++ {
		headersAtLimit["X-Boundary-"+strconv.Itoa(index)] = "value"
	}
	headersOverLimit := make(map[string]string, len(headersAtLimit)+1)
	for key, value := range headersAtLimit {
		headersOverLimit[key] = value
	}
	headersOverLimit["X-Over-Limit"] = "value"
	nameAtLimit := "X" + strings.Repeat("A", limits.HTTPHeaderNameBytes-1)
	nameOverLimit := nameAtLimit + "A"

	cases := []struct {
		name    string
		headers map[string]string
		lookup  func(string) (string, bool)
		wantErr bool
	}{
		{name: "header count limit", headers: headersAtLimit},
		{name: "header count limit plus one", headers: headersOverLimit, wantErr: true},
		{name: "header name limit", headers: map[string]string{nameAtLimit: "value"}},
		{name: "header name limit plus one", headers: map[string]string{nameOverLimit: "value"}, wantErr: true},
		{name: "expanded UTF-8 header value limit", headers: map[string]string{"X-Value": "${BOUNDARY}"}, lookup: testLookup(utf8ValueAtLimit)},
		{name: "expanded UTF-8 header value limit plus one", headers: map[string]string{"X-Value": "${BOUNDARY}"}, lookup: testLookup(utf8ValueAtLimit + "x"), wantErr: true},
		{name: "case insensitive duplicate", headers: map[string]string{"x-boundary": "one", "X-Boundary": "two"}, wantErr: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			lookup := item.lookup
			if lookup == nil {
				lookup = func(string) (string, bool) { return "", false }
			}
			config := ActionConfig{Type: ActionHTTP, URL: "https://example.invalid", Headers: item.headers}
			_, err := compileAction(EventSystemStart, false, Duration{}, config, limits, lookup, nil)
			if (err != nil) != item.wantErr {
				t.Fatalf("compile error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestSubAgentLimits(t *testing.T) {
	limits := DefaultLimits()
	startupCases := []struct {
		name    string
		agent   string
		input   string
		wantErr bool
	}{
		{name: "agent limit", agent: strings.Repeat("a", limits.SubAgentNameBytes), input: "input"},
		{name: "agent limit plus one", agent: strings.Repeat("a", limits.SubAgentNameBytes+1), input: "input", wantErr: true},
		{name: "input template limit", agent: "agent", input: strings.Repeat("x", limits.SubAgentInputBytes)},
		{name: "input template limit plus one", agent: "agent", input: strings.Repeat("x", limits.SubAgentInputBytes+1), wantErr: true},
	}
	for _, item := range startupCases {
		t.Run(item.name, func(t *testing.T) {
			config := ActionConfig{Type: ActionSubAgent, Agent: item.agent, Input: item.input}
			_, err := compileAction(EventMessageBefore, false, Duration{}, config, limits, func(string) (string, bool) { return "", false }, nil)
			if (err != nil) != item.wantErr {
				t.Fatalf("compile error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}

	template, err := compileTemplate(EventMessageBefore, "{{message.content}}", limits.SubAgentInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name                 string
		contentBytes         int
		wantTemplateFailures int
		wantPlaceholderRuns  int
	}{
		{name: "rendered input limit", contentBytes: limits.SubAgentInputBytes, wantPlaceholderRuns: 1},
		{name: "rendered input limit plus one", contentBytes: limits.SubAgentInputBytes + 1, wantTemplateFailures: 2},
	} {
		t.Run(item.name, func(t *testing.T) {
			collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
			rule := Rule{
				Event:  EventMessageBefore,
				Source: Source{Path: "limits.yaml", Ordinal: 1, EffectiveOrdinal: 1},
				Once:   true,
				action: compiledAction{typeName: ActionSubAgent, agent: "agent", template: template},
			}
			engine, err := NewEngine(newSnapshot([]Rule{rule}), EngineOptions{ProjectRoot: t.TempDir(), LegacyDiagnostics: collector, Limits: limits})
			if err != nil {
				t.Fatal(err)
			}
			contextValue := engine.factory.executionBase(EventMessageBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
			contextValue.Message = &MessageContext{ID: "m", Role: MessageUser, Content: strings.Repeat("x", item.contentBytes)}
			event := engine.factory.freeze(contextValue)
			engine.dispatch(context.Background(), event, false)
			engine.dispatch(context.Background(), event, false)
			var templateFailures, placeholderRuns int
			for _, diagnostic := range collector.List() {
				switch diagnostic.Code {
				case DiagnosticTemplateFailed:
					templateFailures++
				case DiagnosticSubAgentNotImplemented:
					placeholderRuns++
				}
			}
			if templateFailures != item.wantTemplateFailures || placeholderRuns != item.wantPlaceholderRuns {
				t.Fatalf("template failures = %d, placeholder runs = %d", templateFailures, placeholderRuns)
			}
		})
	}
}

type limitCommandRecorder struct{ calls int }

func (r *limitCommandRecorder) Run(context.Context, CommandRequest) (CommandResult, error) {
	r.calls++
	return CommandResult{}, nil
}

type limitHTTPRecorder struct{ requests []HTTPRequest }

func (r *limitHTTPRecorder) Run(_ context.Context, request HTTPRequest) (HTTPResult, error) {
	r.requests = append(r.requests, request)
	return HTTPResult{Body: []byte("ok")}, nil
}

func TestOversizeEventActionRouting(t *testing.T) {
	limits := DefaultLimits()
	commandRecorder := &limitCommandRecorder{}
	httpRecorder := &limitHTTPRecorder{}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	parsedURL, err := url.Parse("https://example.invalid/hook")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(newSnapshot(nil), EngineOptions{
		ProjectRoot:       t.TempDir(),
		CommandRunner:     commandRecorder,
		HTTPRunner:        httpRecorder,
		LegacyDiagnostics: collector,
		Limits:            limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextValue := engine.factory.executionBase(EventMessageBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	contextValue.Message = &MessageContext{ID: "m", Role: MessageUser}
	baseline, err := json.Marshal(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	const canary = "oversize-event-payload-canary"
	contentBytes := limits.EventJSONBytes + 1 - len(baseline)
	if contentBytes < len(canary) {
		t.Fatalf("event baseline unexpectedly large: %d", len(baseline))
	}
	content := canary + strings.Repeat("x", contentBytes-len(canary))
	contextValue.Message.Content = content
	event := engine.factory.freeze(contextValue)
	if event.json != nil {
		t.Fatalf("oversize event retained %d payload bytes", len(event.json))
	}
	if _, ok := event.JSON(); ok {
		t.Fatal("oversize event exposed a JSON payload")
	}
	condition, err := compileCondition(EventMessageBefore, &ConditionGroup{All: []Predicate{{Field: "message.content", Match: MatchExact, Value: content}}})
	if err != nil {
		t.Fatal(err)
	}
	engine.rules = []Rule{
		{Event: EventMessageBefore, Source: Source{Path: "limits.yaml", Ordinal: 1, EffectiveOrdinal: 1}, condition: condition, action: compiledAction{typeName: ActionCommand, command: "true"}},
		{Event: EventMessageBefore, Source: Source{Path: "limits.yaml", Ordinal: 2, EffectiveOrdinal: 2}, condition: condition, action: compiledAction{typeName: ActionHTTP, http: &httpAction{url: cloneURL(parsedURL), method: http.MethodPost, headers: make(http.Header), sendEvent: true}}},
		{Event: EventMessageBefore, Source: Source{Path: "limits.yaml", Ordinal: 3, EffectiveOrdinal: 3}, condition: condition, action: compiledAction{typeName: ActionHTTP, http: &httpAction{url: cloneURL(parsedURL), method: http.MethodGet, headers: make(http.Header), sendEvent: false}}},
	}
	engine.dispatch(context.Background(), event, false)
	if commandRecorder.calls != 0 {
		t.Fatalf("oversize command payload started %d commands", commandRecorder.calls)
	}
	if len(httpRecorder.requests) != 1 || httpRecorder.requests[0].SendEvent || len(httpRecorder.requests[0].EventJSON) != 0 {
		t.Fatalf("HTTP requests = %#v", httpRecorder.requests)
	}
	var payloadFailures int
	for _, item := range collector.List() {
		if strings.Contains(item.Text(), canary) {
			t.Fatalf("payload leaked into diagnostic: %s", item.Text())
		}
		if item.Code == DiagnosticLimitExceeded {
			payloadFailures++
		}
	}
	if payloadFailures != 2 {
		t.Fatalf("payload failure diagnostics = %d", payloadFailures)
	}
}
