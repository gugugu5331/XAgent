package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
)

func TestProviderRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	llm, err := New(config.LLMConfig{Protocol: config.ProtocolOpenAI, Model: "test", BaseURL: server.URL, APIKey: "sk-test-secret", RequestTimeoutMS: 10})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := llm.StreamChat(context.Background(), ChatRequest{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: "hi"}}})
	var gotErr error
	if err != nil {
		gotErr = err
	} else {
		for event := range stream {
			if event.Type == StreamEventError {
				gotErr = event.Err
			}
		}
	}
	if gotErr == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) && !strings.Contains(gotErr.Error(), "timeout") && !strings.Contains(gotErr.Error(), "Client.Timeout") {
		t.Fatalf("expected timeout-like error, got %v", gotErr)
	}
	if strings.Contains(gotErr.Error(), "sk-test-secret") {
		t.Fatalf("timeout error leaked api key: %v", gotErr)
	}
}
