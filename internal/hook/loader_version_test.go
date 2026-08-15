package hook

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHookVersionOneIsAccepted(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks: []\n")
	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Len() != 0 {
		t.Fatalf("empty version 1 document produced %d rules", snapshot.Len())
	}
}

func TestHookVersionMissingZeroNegativeTwoOverflowAndWrongTypeFailBeforeEffects(t *testing.T) {
	cases := []struct {
		name    string
		version string
	}{
		{name: "missing"},
		{name: "zero", version: "version: 0\n"},
		{name: "negative", version: "version: -1\n"},
		{name: "two", version: "version: 2\n"},
		{name: "integer overflow", version: "version: 999999999999999999999999999999999999\n"},
		{name: "quoted integer", version: "version: '1'\n"},
		{name: "string", version: "version: 'version-value-canary'\n"},
		{name: "boolean", version: "version: true\n"},
		{name: "float", version: "version: 1.0\n"},
		{name: "null", version: "version: null\n"},
		{name: "sequence", version: "version: [1]\n"},
		{name: "mapping", version: "version: {value: 1}\n"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			var lookups atomic.Int32
			input := item.version + "hooks:\n" +
				"- event: system_start\n" +
				"  action:\n" +
				"    type: command\n" +
				"    command: 'true'\n" +
				"    env: {HOOK_EFFECT: '${HOOK_VERSION_EFFECT_CANARY}'}\n"
			path := writeHookFile(t, home, ".config/xagent/hooks.yaml", input)

			projectConfig := filepath.Join(project, ".xagent")
			if _, err := os.Stat(projectConfig); !os.IsNotExist(err) {
				t.Fatalf("test precondition: project Hook directory exists: %v", err)
			}
			_, err := Load(LoadOptions{
				HomeDir: home, ProjectRoot: project,
				LookupEnv: func(string) (string, bool) {
					lookups.Add(1)
					return "effect-value", true
				},
			})
			loadErr := requireLoadError(t, err)
			if loadErr.Rule != 0 || loadErr.Path != "version" || loadErr.Message != hookSchemaVersionError {
				t.Fatalf("version error = %#v", loadErr)
			}
			if strings.Contains(err.Error(), "version-value-canary") || strings.Contains(err.Error(), "999999999999") {
				t.Fatal("version error echoed rejected scalar")
			}
			if lookups.Load() != 0 {
				t.Fatalf("invalid version reached rule compilation: lookups=%d", lookups.Load())
			}
			if _, err := os.Stat(projectConfig); !os.IsNotExist(err) {
				t.Fatalf("invalid version produced a filesystem effect: %v", err)
			}
			unchanged, readErr := os.ReadFile(path)
			if readErr != nil || string(unchanged) != input {
				t.Fatalf("invalid version rewrote its source: %v", readErr)
			}
		})
	}

	t.Run("project version fails before user rule compilation", func(t *testing.T) {
		home, project := t.TempDir(), t.TempDir()
		writeHookFile(t, home, ".config/xagent/hooks.yaml", "version: 1\nhooks:\n- event: system_start\n  action:\n    type: command\n    command: 'true'\n    env: {HOOK_EFFECT: '${HOOK_VERSION_EFFECT_CANARY}'}\n")
		projectPath := writeHookFile(t, project, ".xagent/hooks.yaml", "version: 2\nhooks: []\n")
		var lookups atomic.Int32
		_, err := Load(LoadOptions{
			HomeDir: home, ProjectRoot: project,
			LookupEnv: func(string) (string, bool) {
				lookups.Add(1)
				return "effect-value", true
			},
		})
		loadErr := requireLoadError(t, err)
		if loadErr.Source != projectPath || loadErr.Path != "version" || loadErr.Message != hookSchemaVersionError {
			t.Fatalf("project version error = %#v", loadErr)
		}
		if lookups.Load() != 0 {
			t.Fatalf("project version failure compiled an earlier user rule: lookups=%d", lookups.Load())
		}
	})
}
