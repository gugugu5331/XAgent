package tui

import (
	"strings"
	"testing"
	"time"
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

func TestTimerAndUsageRespectResetAndConfig(t *testing.T) {
	status := Status{
		Mode: "default", Provider: "fake", Model: "model",
		Duration: 1500 * time.Millisecond, InputTokens: 13, OutputTokens: 21,
		CacheCreationInputTokens: 8, CacheReadInputTokens: 5,
	}
	withoutTimer := status.View()
	if strings.Contains(withoutTimer, "耗时:") {
		t.Fatalf("explicitly disabled timer was rendered: %q", withoutTimer)
	}
	for _, want := range []string{"Tokens: 13 in / 21 out", "Cache: 8 create / 5 read"} {
		if !strings.Contains(withoutTimer, want) {
			t.Fatalf("disabled timer removed %q: %q", want, withoutTimer)
		}
	}

	status.ShowResponseTimer = true
	if withTimer := status.View(); !strings.Contains(withTimer, "耗时: 1.5s") {
		t.Fatalf("enabled timer was not rendered: %q", withTimer)
	}

	status.SetRegion(Region{Width: 200, Height: 1})
	status.ShowResponseTimer = false
	responsiveWithoutTimer := status.View()
	if strings.Contains(responsiveWithoutTimer, "耗时:") {
		t.Fatalf("responsive view rendered explicitly disabled timer: %q", responsiveWithoutTimer)
	}
	for _, want := range []string{"Tokens: 13 in / 21 out", "Cache: 8 create / 5 read"} {
		if !strings.Contains(responsiveWithoutTimer, want) {
			t.Fatalf("responsive disabled timer removed %q: %q", want, responsiveWithoutTimer)
		}
	}
	status.ShowResponseTimer = true
	if responsiveWithTimer := status.View(); !strings.Contains(responsiveWithTimer, "耗时: 1.5s") {
		t.Fatalf("responsive view did not render enabled timer: %q", responsiveWithTimer)
	}

	status.Streaming = true
	status.WaitingConfirmation = true
	status.AgentIteration = 3
	status.AgentMaxIterations = 5
	status.StopReason = "completed"
	status.StopMessage = "old stop"
	status.RequestModel = "old-model"
	status.Error = errStatusTest{}
	status.ResetRequest()
	if status.Duration != 0 || status.InputTokens != 0 || status.OutputTokens != 0 ||
		status.CacheCreationInputTokens != 0 || status.CacheReadInputTokens != 0 ||
		status.Streaming || status.WaitingConfirmation || status.AgentIteration != 0 || status.AgentMaxIterations != 0 ||
		status.StopReason != "" || status.StopMessage != "" || status.RequestModel != "" || status.Error != nil {
		t.Fatalf("request reset retained status values: %#v", status)
	}
	if !status.ShowResponseTimer {
		t.Fatal("request reset changed resolved timer config")
	}
}

type errStatusTest struct{}

func (errStatusTest) Error() string { return "boom" }
