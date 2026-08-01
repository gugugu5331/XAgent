package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeRejectsUnknownNullAndDuplicate(t *testing.T) {
	testCases := []struct {
		name    string
		content string
		path    string
		problem string
	}{
		{name: "unknown", content: "agent:\n  unexpected: 1\n", path: "agent.unexpected", problem: string(decodeUnknownField)},
		{name: "null", content: "llm:\n  model: null\n", path: "llm.model", problem: string(decodeNull)},
		{name: "duplicate", content: "agent:\n  max_iterations: 1\n  max_iterations: 2\n", path: "agent.max_iterations", problem: string(decodeDuplicate)},
		{name: "wrong type", content: "agent:\n  max_iterations: false\n", path: "agent.max_iterations", problem: string(decodeWrongType)},
		{name: "server unknown", content: "mcp:\n  servers:\n    local:\n      surprise: true\n", path: "mcp.servers.local.surprise", problem: string(decodeUnknownField)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			name := writePartialConfig(t, testCase.content)
			_, err := DecodePartial(name)
			if err == nil || !strings.Contains(err.Error(), testCase.path) || !strings.Contains(err.Error(), testCase.problem) {
				t.Fatal("strict partial decode accepted invalid configuration")
			}
		})
	}
}

func TestDecodeErrorDoesNotLeakValue(t *testing.T) {
	const sensitiveValue = "decode-sensitive-value-9d73"
	testCases := []string{
		"agent:\n  max_iterations: " + sensitiveValue + "\n",
		"llm:\n  api_key: [" + sensitiveValue + "]\n",
		"mcp:\n  servers:\n    local:\n      timeout_ms: " + sensitiveValue + "\n",
	}
	for _, content := range testCases {
		_, err := DecodePartial(writePartialConfig(t, content))
		if err == nil || strings.Contains(err.Error(), sensitiveValue) {
			t.Fatal("decode error was absent or leaked a configuration scalar")
		}
	}
}

func TestDecodePartialPreservesExplicitValues(t *testing.T) {
	partial, err := DecodePartial(writePartialConfig(t, "ui:\n  show_response_timer: false\nmcp:\n  servers:\n    local:\n      timeout_ms: 0\n"))
	if err != nil {
		t.Fatal("decode valid partial configuration failed")
	}
	if !partial.UI.ShowResponseTimer.Set || partial.UI.ShowResponseTimer.Value {
		t.Fatal("decode lost explicit false")
	}
	server := partial.MCP.Servers["local"]
	if !server.TimeoutMS.Set || server.TimeoutMS.Value != 0 {
		t.Fatal("decode lost explicit MCP timeout zero")
	}
}

func writePartialConfig(t *testing.T, content string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal("write partial config fixture failed")
	}
	return name
}
