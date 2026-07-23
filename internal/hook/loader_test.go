package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func writeHookFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDiscovery(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil || snapshot.Len() != 0 {
		t.Fatalf("missing files: %v, %d", err, snapshot.Len())
	}
	userPath := writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n  - event: session_start\n    action:\n      type: prompt\n      content: hello\n      scope: session\n")
	projectPath := writeHookFile(t, project, ".xagent/hooks.yaml", "version: 1\nhooks:\n  - event: system_start\n    action:\n      type: command\n      command: 'true'\n")
	snapshot, err = Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	rules := snapshot.Rules()
	if len(rules) != 2 {
		t.Fatalf("rules = %d", len(rules))
	}
	if rules[0].Source.Path != userPath || rules[1].Source.Path != projectPath || rules[0].Source.EffectiveOrdinal != 1 || rules[1].Source.EffectiveOrdinal != 2 {
		t.Fatalf("unstable merge: %#v", rules)
	}
	if rules[1].Timeout != 30_000_000_000 || rules[0].action.scope != ScopeSession {
		t.Fatal("defaults not compiled")
	}
}

func TestLoadYAMLStructure(t *testing.T) {
	cases := map[string]string{
		"duplicate":        "version: 1\nversion: 1\nhooks: []\n",
		"unknown":          "version: 1\nhooks: []\nextra: true\n",
		"second document":  "version: 1\nhooks: []\n---\nversion: 1\nhooks: []\n",
		"alias":            "version: 1\nhooks: &a []\n",
		"unknown event":    "version: 1\nhooks:\n- event: future\n  action: {type: command, command: 'true'}\n",
		"mixed condition":  "version: 1\nhooks:\n- event: tool_before\n  if: {all: [{field: tool.name, match: exact, value: Bash}], any: [{field: tool.name, match: exact, value: Bash}]}\n  action: {type: command, command: 'true'}\n",
		"action union":     "version: 1\nhooks:\n- event: turn_start\n  action: {type: prompt, content: hi, decision: false}\n",
		"bad async":        "version: 1\nhooks:\n- event: tool_before\n  async: true\n  action: {type: command, command: 'true'}\n",
		"bad prompt scope": "version: 1\nhooks:\n- event: session_start\n  action: {type: prompt, content: hi}\n",
		"bad field":        "version: 1\nhooks:\n- event: turn_start\n  if: {all: [{field: turn.status, match: exact, value: completed}]}\n  action: {type: command, command: 'true'}\n",
		"bad regex":        "version: 1\nhooks:\n- event: tool_before\n  if: {all: [{field: tool.name, match: regex, value: '['}]}\n  action: {type: command, command: 'true'}\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			writeHookFile(t, home, ".config/xagent/hooks.yaml", input)
			if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestConfigPresence(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: system_start\n  timeout: 0s\n  action: {type: command, command: 'true'}\n")
	if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project}); err == nil {
		t.Fatal("explicit zero timeout accepted")
	}
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: system_start\n  action: {type: http, url: 'https://example.invalid', method: ''}\n")
	if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project}); err == nil {
		t.Fatal("explicit empty HTTP method accepted")
	}
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: turn_start\n  action: {type: prompt, content: 'prompt', scope: ''}\n")
	if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project}); err == nil {
		t.Fatal("explicit empty Prompt scope accepted")
	}
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: system_start\n  action: {type: http, url: 'https://example.invalid', send_event: false}\n- event: turn_start\n  action: {type: prompt, content: 'prompt'}\n")
	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.rules) != 2 || snapshot.rules[0].action.http.method != "POST" || snapshot.rules[0].action.http.sendEvent || snapshot.rules[1].action.scope != ScopeTurn {
		t.Fatalf("omitted defaults or explicit false lost: %#v", snapshot.rules)
	}
}

func TestEnvironmentExpansion(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	runtimeRedactor := redact.NewRuntimeRedactor()
	const secret = "environment-component-canary-12345"
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: 'true'\n    env: {AUTHORIZATION: 'Bearer ${API_TOKEN}', DISPLAY: 'prefix-${API_TOKEN}', LITERAL: '$${API_TOKEN}'}\n")
	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Redactor: runtimeRedactor, LookupEnv: func(name string) (string, bool) { return secret, name == "API_TOKEN" }})
	if err != nil {
		t.Fatal(err)
	}
	env := snapshot.rules[0].action.env
	if env["AUTHORIZATION"] != "Bearer "+secret || env["DISPLAY"] != "prefix-"+secret || env["LITERAL"] != "${API_TOKEN}" {
		t.Fatalf("env = %#v", env)
	}
	if got := runtimeRedactor.Text(secret); got != "[redacted]" {
		t.Fatalf("bare expansion was not registered: %q", got)
	}
	if got := runtimeRedactor.Text("Bearer " + secret); got != "Bearer [redacted]" {
		t.Fatalf("registered composite instead of replacement component: %q", got)
	}
	if got := runtimeRedactor.Text("prefix-" + secret); got != "prefix-[redacted]" {
		t.Fatalf("sensitive reference component was not registered independently: %q", got)
	}
	if got := runtimeRedactor.MaxSecretBytes(); got != len(secret) {
		t.Fatalf("maximum registered secret bytes = %d, want %d", got, len(secret))
	}
}

func TestLoadErrorLocationAndRedaction(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	secret := "secret-canary-value"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(secret)
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: future\n  action: {type: command, command: '"+secret+"'}\n")
	_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Redactor: runtimeRedactor})
	if err == nil {
		t.Fatal("expected error")
	}
	message := err.Error()
	if !strings.Contains(message, "hooks[0]") || !strings.Contains(message, ":3:") || strings.Contains(message, secret) {
		t.Fatalf("unsafe/unlocated error: %s", message)
	}
}

func TestLoadErrorsDoNotEchoConfiguredValues(t *testing.T) {
	cases := []struct {
		name     string
		canary   string
		input    string
		category string
	}{
		{
			name:     "regex",
			canary:   "regex-value-canary-924713",
			input:    "version: 1\nhooks:\n- event: tool_before\n  if: {all: [{field: tool.name, match: regex, value: '(?P<regex-value-canary-924713>'}]}\n  action: {type: command, command: 'true'}\n",
			category: "invalid regex",
		},
		{
			name:     "duration",
			canary:   "duration-value-canary-924713",
			input:    "version: 1\nhooks:\n- event: system_start\n  timeout: duration-value-canary-924713\n  action: {type: command, command: 'true'}\n",
			category: "invalid duration",
		},
		{
			name:     "prompt template",
			canary:   "prompt-value-canary-924713",
			input:    "version: 1\nhooks:\n- event: message_before\n  action: {type: prompt, content: '{{message.prompt-value-canary-924713}}', scope: turn}\n",
			category: "invalid template field",
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			path := writeHookFile(t, home, ".config/xagent/hooks.yaml", item.input)
			_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
			if err == nil {
				t.Fatal("expected error")
			}
			message := err.Error()
			if !strings.Contains(message, path) || !strings.Contains(message, "rule 1") || !strings.Contains(message, "hooks[0]") || !strings.Contains(message, item.category) {
				t.Fatalf("error lacks safe location/category: %s", message)
			}
			if strings.Contains(message, item.canary) {
				t.Fatalf("error echoed configured value: %s", message)
			}
		})
	}
}

func TestLoadYAMLSyntaxErrorHasSafeLocation(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	path := writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n  - event: syntax-canary\n    action: [\n")
	_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err == nil {
		t.Fatal("expected error")
	}
	var loadErr *LoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("error type = %T", err)
	}
	if loadErr.Source != path || loadErr.Line <= 0 || loadErr.Column <= 0 || loadErr.Path != "$" || loadErr.Message != "invalid YAML structure" {
		t.Fatalf("syntax location = %#v", loadErr)
	}
	if strings.Contains(err.Error(), "syntax-canary") {
		t.Fatalf("syntax error echoed source value: %s", err)
	}
}

func TestLoadRawNodeErrorUsesOffendingNodeAndPath(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	input := "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: 'true'\n    command: raw-value-canary\n"
	path := writeHookFile(t, home, ".config/xagent/hooks.yaml", input)
	_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	loadErr := requireLoadError(t, err)
	if loadErr.Source != path || loadErr.Rule != 1 || loadErr.Path != "hooks[0].action.command" || loadErr.Line != lineOf(input, "command: raw-value-canary") || loadErr.Column != 5 || loadErr.Message != "duplicate mapping key" {
		t.Fatalf("raw location = %#v", loadErr)
	}
	if strings.Contains(err.Error(), "raw-value-canary") {
		t.Fatalf("raw error echoed source value: %s", err)
	}
}

func TestLoadMappingErrorsUseLeafPaths(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantPath   string
		lineNeedle string
		wantColumn int
	}{
		{
			name:       "unknown action field",
			input:      "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: 'true'\n    unexpected_field: mapping-value-canary\n",
			wantPath:   "hooks[0].action.unexpected_field",
			lineNeedle: "unexpected_field:",
			wantColumn: 5,
		},
		{
			name:       "missing action type",
			input:      "version: 1\nhooks:\n- event: system_start\n  action:\n    command: mapping-value-canary\n",
			wantPath:   "hooks[0].action.type",
			lineNeedle: "command:",
			wantColumn: 5,
		},
		{
			name:       "unknown predicate field",
			input:      "version: 1\nhooks:\n- event: tool_before\n  if:\n    all:\n    - field: tool.name\n      match: exact\n      value: Bash\n      extra: mapping-value-canary\n  action: {type: command, command: 'true'}\n",
			wantPath:   "hooks[0].if.all[0].extra",
			lineNeedle: "extra:",
			wantColumn: 7,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			writeHookFile(t, home, ".config/xagent/hooks.yaml", item.input)
			_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
			loadErr := requireLoadError(t, err)
			if loadErr.Rule != 1 || loadErr.Path != item.wantPath || loadErr.Line != lineOf(item.input, item.lineNeedle) || loadErr.Column != item.wantColumn {
				t.Fatalf("mapping location = %#v", loadErr)
			}
			if strings.Contains(err.Error(), "mapping-value-canary") {
				t.Fatalf("mapping error echoed source value: %s", err)
			}
		})
	}
}

func TestLoadCompileErrorsUseSafeLeafLocations(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		canary     string
		wantPath   string
		lineNeedle string
		category   string
	}{
		{
			name:       "event",
			canary:     "future-event-canary",
			input:      "version: 1\nhooks:\n- event: future-event-canary\n  action: {type: command, command: 'true'}\n",
			wantPath:   "hooks[0].event",
			lineNeedle: "event:",
			category:   "unknown event",
		},
		{
			name:       "second predicate regex",
			canary:     "regex-value-canary",
			input:      "version: 1\nhooks:\n- event: tool_before\n  if:\n    all:\n    - field: tool.name\n      match: exact\n      value: Bash\n    - field: tool.name\n      match: regex\n      value: 'regex-value-canary['\n  action: {type: command, command: 'true'}\n",
			wantPath:   "hooks[0].if.all[1].value",
			lineNeedle: "value: 'regex-value-canary['",
			category:   "invalid regex",
		},
		{
			name:       "condition field",
			canary:     "private-field-canary",
			input:      "version: 1\nhooks:\n- event: turn_start\n  if:\n    any:\n    - field: private-field-canary\n      match: exact\n      value: ok\n  action: {type: command, command: 'true'}\n",
			wantPath:   "hooks[0].if.any[0].field",
			lineNeedle: "field:",
			category:   "field is unavailable for event",
		},
		{
			name:       "prompt template",
			canary:     "prompt-value-canary",
			input:      "version: 1\nhooks:\n- event: message_before\n  action:\n    type: prompt\n    content: '{{message.prompt-value-canary}}'\n    scope: turn\n",
			wantPath:   "hooks[0].action.content",
			lineNeedle: "content:",
			category:   "invalid template field",
		},
		{
			name:       "command",
			canary:     "command-value-canary",
			input:      "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: '{{command-value-canary}}'\n",
			wantPath:   "hooks[0].action.command",
			lineNeedle: "command:",
			category:   "event placeholders are not allowed in command",
		},
		{
			name:       "environment",
			canary:     "ENV_VALUE_CANARY",
			input:      "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: 'true'\n    env:\n      TOKEN: '${ENV_VALUE_CANARY}'\n",
			wantPath:   "hooks[0].action.env",
			lineNeedle: "TOKEN:",
			category:   "undefined environment reference",
		},
		{
			name:       "header",
			canary:     "header-value-canary",
			input:      "version: 1\nhooks:\n- event: system_start\n  action:\n    type: http\n    url: https://example.invalid\n    headers:\n      X-Test: \"header-value-canary\\n\"\n",
			wantPath:   "hooks[0].action.headers",
			lineNeedle: "X-Test:",
			category:   "invalid header value",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			writeHookFile(t, home, ".config/xagent/hooks.yaml", item.input)
			_, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
			loadErr := requireLoadError(t, err)
			if loadErr.Rule != 1 || loadErr.Path != item.wantPath || loadErr.Line != lineOf(item.input, item.lineNeedle) || loadErr.Column <= 0 || loadErr.Message != item.category {
				t.Fatalf("compile location = %#v", loadErr)
			}
			if strings.Contains(err.Error(), item.canary) {
				t.Fatalf("compile error echoed configured value: %s", err)
			}
		})
	}
}

func requireLoadError(t *testing.T, err error) *LoadError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var loadErr *LoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("error type = %T: %v", err, err)
	}
	return loadErr
}

func lineOf(content, needle string) int {
	index := strings.Index(content, needle)
	if index < 0 {
		return 0
	}
	return 1 + strings.Count(content[:index], "\n")
}
