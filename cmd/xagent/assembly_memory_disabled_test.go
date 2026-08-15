package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/events"
	"xagent/internal/orchestrator"
	"xagent/internal/testutil"
)

func TestAssemblyDisabledMemoryOrdinaryCompletionDoesNotPanic(t *testing.T) {
	providerServer := testutil.NewFakeProviderServer(testutil.FakeProviderSuccess)
	t.Cleanup(providerServer.Close)

	runtime := buildDisabledMemoryAssemblyRuntime(t, providerServer.URL()+"/v1")
	conversation, err := runtime.ui.appServices.Conversations.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := runtime.ui.appServices.Orchestrator.SendRequest(
		context.Background(),
		conversation,
		orchestrator.RunRequest{UserText: "complete without memory", Mode: orchestrator.RunModeDefault},
	)
	if err != nil {
		t.Fatal(err)
	}

	completed := false
	for event := range stream {
		if event.Type == events.Error {
			t.Fatalf("ordinary request failed: %s", event.Text.Text())
		}
		completed = completed || event.Type == events.Done
	}
	if !completed {
		t.Fatal("ordinary request did not complete")
	}
}

func TestAssemblyDisabledMemoryAgentForkCompletionDoesNotPanic(t *testing.T) {
	providerServer, childSeen := newAssemblyForkProviderServer(t)
	t.Cleanup(providerServer.Close)

	runtime := buildDisabledMemoryAssemblyRuntime(t, providerServer.URL+"/v1")
	conversation, err := runtime.ui.appServices.Conversations.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := runtime.ui.appServices.Orchestrator.SendRequest(
		context.Background(),
		conversation,
		orchestrator.RunRequest{UserText: "delegate with fork", Mode: orchestrator.RunModeDefault},
	)
	if err != nil {
		t.Fatal(err)
	}

	completed := false
	for event := range stream {
		if event.Type == events.Error {
			t.Fatalf("Agent fork parent request failed: %s", event.Text.Text())
		}
		completed = completed || event.Type == events.Done
	}
	if !completed {
		t.Fatal("Agent fork parent request did not complete")
	}
	select {
	case <-childSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent fork child request was not observed")
	}
}

func newAssemblyForkProviderServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	childSeen := make(chan struct{})
	var childOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode fork provider request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		switch {
		case !assemblyRequestHasAgentTool(body):
			childOnce.Do(func() { close(childSeen) })
			writeAssemblyProviderSSE(writer, `{"choices":[{"delta":{"content":"fork child complete"}}]}`)
			writeAssemblyProviderSSE(writer, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		case assemblyRequestHasRole(body, "tool"):
			writeAssemblyProviderSSE(writer, `{"choices":[{"delta":{"content":"fork parent complete"}}]}`)
			writeAssemblyProviderSSE(writer, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		default:
			writeAssemblyProviderSSE(writer, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_agent_fork","type":"function","function":{"name":"Agent","arguments":"{\"task\":\"inspect the parent snapshot\",\"type\":\"fork\",\"placement\":\"foreground\"}"}}]},"finish_reason":"tool_calls"}]}`)
		}
		writeAssemblyProviderSSE(writer, "[DONE]")
	}))
	return server, childSeen
}

func assemblyRequestHasAgentTool(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	for _, item := range tools {
		toolDefinition, _ := item.(map[string]any)
		function, _ := toolDefinition["function"].(map[string]any)
		if name, _ := function["name"].(string); name == "Agent" {
			return true
		}
	}
	return false
}

func assemblyRequestHasRole(body map[string]any, role string) bool {
	messages, _ := body["messages"].([]any)
	for _, item := range messages {
		message, _ := item.(map[string]any)
		if current, _ := message["role"].(string); current == role {
			return true
		}
	}
	return false
}

func writeAssemblyProviderSSE(writer http.ResponseWriter, data string) {
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func buildDisabledMemoryAssemblyRuntime(t *testing.T, providerURL string) *Runtime {
	t.Helper()
	root := t.TempDir()
	paths := RuntimePaths{
		ProjectRoot:    filepath.Join(root, "project"),
		UserConfigRoot: filepath.Join(root, "config"),
		UserDataRoot:   filepath.Join(root, "data"),
		UserCacheRoot:  filepath.Join(root, "cache"),
	}
	for _, path := range []string{
		filepath.Join(paths.ProjectRoot, ".xagent", "skills"),
		filepath.Join(paths.UserConfigRoot, "skills"),
		paths.UserDataRoot,
		paths.UserCacheRoot,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	disabled := false
	assembled, err := defaultAssembly().Build(context.Background(), AssemblyOptions{
		Paths: paths,
		Config: ConfigInputs{Runtime: config.PartialAppConfig{
			LLM: config.PartialLLMConfig{
				Protocol: config.Optional[string]{Set: true, Value: config.ProtocolOpenAI},
				Model:    config.Optional[string]{Set: true, Value: "assembly-memory-test"},
				BaseURL:  config.Optional[string]{Set: true, Value: providerURL},
				APIKey:   config.Optional[string]{Set: true, Value: "assembly-memory-test-key"},
			},
			Memory: config.PartialMemoryConfig{Enabled: config.Optional[bool]{Set: true, Value: disabled}},
		}},
		LookupEnv: os.LookupEnv,
		Stdin:     strings.NewReader(""),
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("build disabled-memory assembly: %v", err)
	}
	t.Cleanup(func() {
		if err := assembled.Close(context.Background()); err != nil {
			t.Errorf("close disabled-memory assembly: %v", err)
		}
	})
	return assembled
}
