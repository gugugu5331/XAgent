package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/netpolicy"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestRequestMeasureBoundsAnthropicAndOpenAIJSON(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	request := requestOracleFixture(redactor, "custom/model:2026-08-03")
	measure, err := NewRequestBudgeter().MeasureRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("measure oracle request: %v", err)
	}
	for _, protocol := range []string{config.ProtocolAnthropic, config.ProtocolOpenAI} {
		protocol := protocol
		t.Run(protocol, func(t *testing.T) {
			body := captureProviderRequestBody(t, protocol, request)
			if !json.Valid(body) {
				t.Fatalf("%s recorder captured invalid JSON: %q", protocol, body)
			}
			if measure.Bytes < int64(len(body)) {
				t.Fatalf("%s request bound underestimated payload: measure=%#v serialized=%d", protocol, measure, len(body))
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode %s request: %v", protocol, err)
			}
			if decoded["model"] != request.Model {
				t.Fatalf("%s effective model = %#v, want %q", protocol, decoded["model"], request.Model)
			}
		})
	}
}

func TestRequestMeasureGoldenCoversEveryApprovedField(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	invalidUTF8 := string([]byte{0xff, 'x'})
	cases := []struct {
		name    string
		request provider.ChatRequest
		want    RequestMeasure
	}{
		{name: "empty", request: provider.ChatRequest{}, want: RequestMeasure{Bytes: 1623, PlanningTokens: 406}},
		{name: "ascii", request: provider.ChatRequest{Model: "abc", Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("ascii")}}}, want: RequestMeasure{Bytes: 1774, PlanningTokens: 444}},
		{name: "cjk", request: provider.ChatRequest{Model: "中", Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("中文")}}}, want: RequestMeasure{Bytes: 1780, PlanningTokens: 445}},
		{name: "emoji", request: provider.ChatRequest{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("🙂")}}}, want: RequestMeasure{Bytes: 1750, PlanningTokens: 438}},
		{name: "combining", request: provider.ChatRequest{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("e\u0301")}}}, want: RequestMeasure{Bytes: 1744, PlanningTokens: 436}},
		{name: "invalid_utf8", request: provider.ChatRequest{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact(invalidUTF8)}}}, want: RequestMeasure{Bytes: 1750, PlanningTokens: 438}},
		{name: "full_request", request: requestOracleFixture(redactor, "arbitrary-openai-compatible/model"), want: RequestMeasure{Bytes: 10673, PlanningTokens: 2669}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := NewRequestBudgeter().MeasureRequest(context.Background(), testCase.request)
			if err != nil {
				t.Fatalf("measure golden request: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("request measure = %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func TestArbitraryNonEmptyModelNamesDoNotChangeMeasurementRules(t *testing.T) {
	budgeter := NewRequestBudgeter()
	empty, err := budgeter.MeasureRequest(context.Background(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("measure empty model: %v", err)
	}
	models := []string{
		"abc",
		"中",
		"  vendor/custom-model:v9  ",
		"openai-compatible/model@edge+preview",
	}
	for _, model := range models {
		measured, measureErr := budgeter.MeasureRequest(context.Background(), provider.ChatRequest{Model: model})
		if measureErr != nil {
			t.Fatalf("measure arbitrary model %q: %v", model, measureErr)
		}
		wantDelta := int64(6 * len(strings.TrimSpace(model)))
		if measured.Bytes-empty.Bytes != wantDelta {
			t.Fatalf("model %q byte delta = %d, want raw-byte rule %d", model, measured.Bytes-empty.Bytes, wantDelta)
		}
	}
	ascii, _ := budgeter.MeasureRequest(context.Background(), provider.ChatRequest{Model: "abc"})
	cjk, _ := budgeter.MeasureRequest(context.Background(), provider.ChatRequest{Model: "中"})
	if ascii != cjk {
		t.Fatalf("equal raw-byte model names used different rules: ASCII=%#v CJK=%#v", ascii, cjk)
	}
}

func requestOracleFixture(redactor *redact.RuntimeRedactor, model string) provider.ChatRequest {
	artifactID := strings.Repeat("c", 64)
	result := `{"state":"completed","status":"success","summary":"safe result","preview":"CJK 中 emoji 🙂 combining é","artifact":{"id":"` + artifactID + `","bytes":8192,"created_at":"2026-08-03T00:00:00Z","available":true,"complete":true},"truncated":true,"truncation_reason":"inline_preview_limit"}`
	return provider.ChatRequest{
		Model: model,
		StableSystem: []provider.SystemBlock{
			{Name: "stable", Content: redactor.Redact("stable system ASCII"), Cacheable: true},
			{Name: "skill-sop", Content: redactor.Redact("Skill SOP：先检查 é 再执行 🙂"), Cacheable: true},
		},
		DynamicSystem: []provider.SystemBlock{{Name: "dynamic", Content: redactor.Redact("动态提醒")}},
		Messages: []provider.ModelMessage{
			{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("current input ASCII 中文 🙂 é")},
			{Role: provider.ModelMessageRoleAssistant, Content: redactor.Redact("selected history")},
			{Role: provider.ModelMessageRoleToolCall, ToolCallID: "call-1", ToolName: "Lookup", ArgumentsJSON: redactor.Redact(`{"query":"中文🙂é"}`)},
			{Role: provider.ModelMessageRoleToolResult, ToolCallID: "call-1", ToolName: "Lookup", Content: redactor.Redact(result), ToolResult: redactor.Redact(result), ToolResultStatus: string(tool.StatusSuccess)},
			{Role: provider.ModelMessageRoleContextSummary, Content: redactor.Redact("safe summary")},
			{Role: provider.ModelMessageRoleContextBoundary, Content: redactor.Redact("safe boundary")},
		},
		Thinking: config.ThinkingConfig{Enabled: true, Show: true, BudgetTokens: 4096},
		Tools: []provider.ToolDefinition{{
			Name: "Lookup", Description: "Lookup ASCII、中文、🙂、é",
			Schema: tool.Schema{Raw: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"中文🙂é"}},"required":["query"],"additionalProperties":false}`)},
		}},
		Cache: provider.CachePolicy{EnablePromptCache: true, SystemBreakpointName: "skill-sop", CacheTools: true},
	}
}

type providerRequestRecorder struct {
	mu      sync.Mutex
	body    []byte
	reads   int
	readErr error
}

func (r *providerRequestRecorder) record(request *http.Request) {
	data, err := io.ReadAll(io.LimitReader(request.Body, 8<<20))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	r.body = append([]byte(nil), data...)
	r.readErr = err
}

func (r *providerRequestRecorder) snapshot() ([]byte, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.body...), r.reads, r.readErr
}

func captureProviderRequestBody(t *testing.T, protocol string, request provider.ChatRequest) []byte {
	t.Helper()
	recorder := &providerRequestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		recorder.record(incoming)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if protocol == config.ProtocolAnthropic {
			_, _ = io.WriteString(writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	policy := netpolicy.NewPolicy()
	endpoint, err := policy.ValidateInitial(context.Background(), server.URL, netpolicy.PurposeProvider)
	if err != nil {
		t.Fatalf("validate local Provider recorder: %v", err)
	}
	client, err := netpolicy.NewClientFactory(policy).New(endpoint, netpolicy.ClientOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("create controlled Provider client: %v", err)
	}
	defer client.CloseIdleConnections()
	llm, err := provider.NewWithOptions(config.LLMConfig{
		Protocol: protocol, Model: "configured-fallback", APIKey: "sk-local-oracle", RequestTimeoutMS: 2_000,
	}, provider.ProviderOptions{Endpoint: endpoint, Client: client, RuntimeRedactor: redact.NewRuntimeRedactor()})
	if err != nil {
		t.Fatalf("construct %s Provider: %v", protocol, err)
	}
	stream, err := llm.StreamChat(context.Background(), request)
	if err != nil {
		t.Fatalf("start %s recorder request: %v", protocol, err)
	}
	for range stream.Events() {
	}
	closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := stream.Close(closeContext); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("close %s recorder stream: %v", protocol, err)
	}
	body, reads, readErr := recorder.snapshot()
	if readErr != nil || reads != 1 || len(body) == 0 {
		t.Fatalf("%s recorder state: reads=%d bytes=%d err=%v", protocol, reads, len(body), readErr)
	}
	return body
}
