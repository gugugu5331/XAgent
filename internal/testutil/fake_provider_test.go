package testutil

import (
	"context"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
)

func TestFakeProviderServerStreamsOpenAIResponse(t *testing.T) {
	fake := NewFakeProviderServer(FakeProviderSuccess)
	defer fake.Close()
	llm, err := provider.New(config.LLMConfig{Protocol: config.ProtocolOpenAI, Model: "fake", BaseURL: fake.URL(), APIKey: "sk-test-secret", RequestTimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range stream {
		if event.Type == provider.StreamEventTextDelta {
			text += event.Delta
		}
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
	llm, err := provider.New(config.LLMConfig{Protocol: config.ProtocolOpenAI, Model: "fake", BaseURL: fake.URL(), APIKey: "sk-test-secret", RequestTimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: "run failing bash"}}})
	if err != nil {
		t.Fatal(err)
	}
	var sawTool bool
	for event := range stream {
		if event.Type == provider.StreamEventToolCall && len(event.ToolCalls) == 1 && event.ToolCalls[0].Name == "Bash" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("expected Bash tool call")
	}

	stream, err = llm.StreamChat(context.Background(), provider.ChatRequest{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: "run failing bash"},
		{Role: conversation.RoleToolResult, ToolCallID: "call_bash_fail", ToolResultContent: `{"status":"error"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range stream {
		if event.Type == provider.StreamEventTextDelta {
			text += event.Delta
		}
	}
	if !strings.Contains(text, "Bash 失败摘要") {
		t.Fatalf("unexpected final text: %q", text)
	}
}
