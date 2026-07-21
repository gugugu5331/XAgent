package tui

import (
	"strings"
	"testing"
)

func TestStatusAlwaysShowsKnownModeFirst(t *testing.T) {
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "default", want: "[DEFAULT]"},
		{mode: "plan", want: "[PLAN]"},
		{mode: "PLAN", want: "[PLAN]"},
		{mode: "", want: "[DEFAULT]"},
		{mode: "unknown", want: "[DEFAULT]"},
	} {
		output := Status{Mode: test.mode, Provider: "fake", Model: "model"}.View()
		if !strings.HasPrefix(output, test.want) || !strings.Contains(output, "Provider: fake") || !strings.Contains(output, "Model: model") {
			t.Fatalf("mode %q rendered unexpectedly: %q", test.mode, output)
		}
	}
}

func TestStatusModeKeepsUsageMCPAndError(t *testing.T) {
	output := Status{Mode: "plan", Provider: "fake", Model: "model", InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4, MCP: "ready", Error: errStatusTest{}}.View()
	for _, want := range []string{"[PLAN]", "Tokens: 1 in / 2 out", "Cache: 3 create / 4 read", "MCP: ready", "错误: boom"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status missing %q: %q", want, output)
		}
	}
}

func TestStatusSkillAndRequestModel(t *testing.T) {
	running := Status{
		Mode: "default", Provider: "fake", Model: "default-model",
		ActiveSkills: "commit, test", RequestModel: "skill-model", Streaming: true,
	}.View()
	for _, want := range []string{"Model: default-model", "Skills: commit, test", "Request model: skill-model"} {
		if !strings.Contains(running, want) {
			t.Fatalf("running status missing %q: %q", want, running)
		}
	}

	completed := Status{Mode: "default", Provider: "fake", Model: "default-model", ActiveSkills: "commit, test"}.View()
	if strings.Contains(completed, "Request model:") || !strings.Contains(completed, "Model: default-model") {
		t.Fatalf("completed status did not restore default model: %q", completed)
	}
}

func TestStatusDefaultOutputUnchangedWithoutSkills(t *testing.T) {
	output := Status{Mode: "default", Provider: "fake", Model: "model"}.View()
	if output != "[DEFAULT] | Provider: fake | Model: model" {
		t.Fatalf("default status changed: %q", output)
	}
}

type errStatusTest struct{}

func (errStatusTest) Error() string { return "boom" }
