package testutil

import (
	"context"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/netpolicy"
	"xagent/internal/provider"
	"xagent/internal/redact"
)

func TestFakeProviderServerStreamsOpenAIResponse(t *testing.T) {
	fake := NewFakeProviderServer(FakeProviderSuccess)
	defer fake.Close()
	llm := newFakeProviderClient(t, fake.URL())
	stream, err := llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []provider.ModelMessage{{
		Role: provider.ModelMessageRoleUser, Content: fakeSafeText("hi"),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range stream.Events() {
		if event.Type == provider.StreamEventTextDelta {
			text += event.Delta.Text()
		}
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	if text != "OK from fake provider" {
		t.Fatalf("text = %q", text)
	}
	requests := fake.Requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Authorization, "sk-test-secret") {
		t.Fatalf("request authorization not recorded: %#v", requests)
	}
}

func TestFakeProviderServerStreamsBashFailureToolCall(t *testing.T) {
	fake := NewFakeProviderServer(FakeProviderToolBashFail)
	defer fake.Close()
	llm := newFakeProviderClient(t, fake.URL())
	stream, err := llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []provider.ModelMessage{{
		Role: provider.ModelMessageRoleUser, Content: fakeSafeText("run failing bash"),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var sawTool bool
	for event := range stream.Events() {
		if event.Type == provider.StreamEventToolCall && event.ToolCall != nil && event.ToolCall.Name == "Bash" {
			sawTool = true
		}
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close tool stream: %v", err)
	}
	if !sawTool {
		t.Fatal("expected Bash tool call")
	}

	stream, err = llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []provider.ModelMessage{
		{Role: provider.ModelMessageRoleUser, Content: fakeSafeText("run failing bash")},
		{Role: provider.ModelMessageRoleToolResult, ToolCallID: "call_bash_fail", ToolName: "Bash", ToolResult: fakeSafeText(`{"status":"error"}`), ToolResultStatus: "error"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range stream.Events() {
		if event.Type == provider.StreamEventTextDelta {
			text += event.Delta.Text()
		}
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close final stream: %v", err)
	}
	if !strings.Contains(text, "Bash 失败摘要") {
		t.Fatalf("unexpected final text: %q", text)
	}
}

func newFakeProviderClient(t *testing.T, rawURL string) provider.Provider {
	t.Helper()
	policy := netpolicy.NewPolicy()
	endpoint, err := policy.ValidateInitial(context.Background(), rawURL, netpolicy.PurposeProvider)
	if err != nil {
		t.Fatalf("validate fake Provider endpoint: %v", err)
	}
	client, err := netpolicy.NewClientFactory(policy).New(endpoint, netpolicy.ClientOptions{})
	if err != nil {
		t.Fatalf("create guarded fake Provider client: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	llm, err := provider.NewWithOptions(config.LLMConfig{
		Protocol: config.ProtocolOpenAI, Model: "fake", BaseURL: rawURL,
		APIKey: "sk-test-secret", RequestTimeoutMS: 1000,
	}, provider.ProviderOptions{Endpoint: endpoint, Client: client, RuntimeRedactor: redact.NewRuntimeRedactor()})
	if err != nil {
		t.Fatalf("create fake Provider: %v", err)
	}
	return llm
}

func fakeSafeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}
