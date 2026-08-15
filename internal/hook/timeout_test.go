package hook

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHookTimeoutAbsentUsesActionDefaults(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeHookFile(t, home, ".config/xagent/hooks.yaml", `version: 1
hooks:
  - event: system_start
    action: {type: command, command: "true"}
  - event: system_start
    action: {type: http, url: "https://example.invalid"}
  - event: system_start
    action: {type: subagent, agent: "reviewer", input: "review"}
`)

	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	rules := snapshot.Rules()
	want := []struct {
		action  ActionType
		timeout time.Duration
	}{
		{action: ActionCommand, timeout: 30 * time.Second},
		{action: ActionHTTP, timeout: 10 * time.Second},
		{action: ActionSubAgent, timeout: 30 * time.Second},
	}
	if len(rules) != len(want) {
		t.Fatalf("rules = %d, want %d", len(rules), len(want))
	}
	for index, expected := range want {
		if rules[index].ActionType() != expected.action || rules[index].Timeout != expected.timeout {
			t.Errorf("rule %d = (%s, %s), want (%s, %s)", index, rules[index].ActionType(), rules[index].Timeout, expected.action, expected.timeout)
		}
	}
}

func TestHookTimeoutAcceptsOneMillisecondAndTenMinutes(t *testing.T) {
	actions := []struct {
		name string
		yaml string
	}{
		{name: "command", yaml: `action: {type: command, command: "true"}`},
		{name: "http", yaml: `action: {type: http, url: "https://example.invalid"}`},
		{name: "subagent", yaml: `action: {type: subagent, agent: "reviewer", input: "review"}`},
	}
	boundaries := []struct {
		raw  string
		want time.Duration
	}{
		{raw: "1ms", want: time.Millisecond},
		{raw: "10m", want: 10 * time.Minute},
	}

	for _, action := range actions {
		for _, boundary := range boundaries {
			t.Run(action.name+"/"+boundary.raw, func(t *testing.T) {
				home, project := t.TempDir(), t.TempDir()
				input := fmt.Sprintf("version: 1\nhooks:\n- event: system_start\n  timeout: %s\n  %s\n", boundary.raw, action.yaml)
				writeHookFile(t, home, ".config/xagent/hooks.yaml", input)

				snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
				if err != nil {
					t.Fatal(err)
				}
				rules := snapshot.Rules()
				if len(rules) != 1 || rules[0].Timeout != boundary.want {
					t.Fatalf("compiled timeout = %v, want %s", rules, boundary.want)
				}
			})
		}
	}
}

func TestHookTimeoutRejectsBelowMinAboveCapZeroNegativeOverflowAndPrompt(t *testing.T) {
	cases := []struct {
		name        string
		timeout     string
		action      string
		wantMessage string
	}{
		{name: "below minimum", timeout: "999us", action: `action: {type: command, command: "true"}`, wantMessage: "timeout must be between 1ms and 10m"},
		{name: "above cap", timeout: "10m1ns", action: `action: {type: http, url: "https://example.invalid"}`, wantMessage: "timeout must be between 1ms and 10m"},
		{name: "zero", timeout: "0s", action: `action: {type: subagent, agent: "reviewer", input: "review"}`, wantMessage: "timeout must be between 1ms and 10m"},
		{name: "negative", timeout: "-1ms", action: `action: {type: command, command: "true"}`, wantMessage: "timeout must be between 1ms and 10m"},
		{name: "parse duration overflow", timeout: "2562047h47m16.854775808s", action: `action: {type: command, command: "true"}`, wantMessage: "invalid duration"},
		{name: "prompt", timeout: "1s", action: `action: {type: prompt, content: "review"}`, wantMessage: "timeout is not valid for prompt"},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			input := fmt.Sprintf("version: 1\nhooks:\n- event: system_start\n  timeout: %s\n  %s\n", item.timeout, item.action)
			path := writeHookFile(t, home, ".config/xagent/hooks.yaml", input)

			_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
			if err == nil {
				t.Fatal("invalid timeout accepted")
			}
			var loadErr *LoadError
			if !errors.As(err, &loadErr) {
				t.Fatalf("error type = %T, want *LoadError", err)
			}
			if loadErr.Source != path || loadErr.Rule != 1 || loadErr.Path != "hooks[0].timeout" || loadErr.Message != item.wantMessage {
				t.Fatalf("timeout error = %#v", loadErr)
			}
			if strings.Contains(err.Error(), item.timeout) {
				t.Fatalf("error echoed configured timeout: %s", err)
			}
		})
	}
}
