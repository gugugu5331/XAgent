package main

import (
	"os"
	"strings"
	"testing"

	"xagent/internal/config"
)

// TestCleanupTimeoutAcceptsOneAndTwoThousandRejectsZeroNegativeAndTwoThousandOne
// freezes the presence-aware public lifecycle boundary. Zero is invalid when
// explicitly supplied; an omitted value is resolved by config, not by a leaf
// owner.
func TestCleanupTimeoutAcceptsOneAndTwoThousandRejectsZeroNegativeAndTwoThousandOne(t *testing.T) {
	for _, milliseconds := range []int64{1, 2_000} {
		loaded, err := resolveT427CleanupTimeout(milliseconds)
		if err != nil {
			t.Fatalf("cleanup_timeout_ms=%d rejected: %v", milliseconds, err)
		}
		if got := loaded.Config.Lifecycle.CleanupTimeoutMS; got != milliseconds {
			t.Fatalf("cleanup_timeout_ms=%d resolved as %d", milliseconds, got)
		}
	}
	for _, milliseconds := range []int64{0, -1, 2_001} {
		if loaded, err := resolveT427CleanupTimeout(milliseconds); err == nil {
			t.Fatalf("cleanup_timeout_ms=%d accepted as %#v", milliseconds, loaded.Config.Lifecycle)
		}
	}
}

func resolveT427CleanupTimeout(milliseconds int64) (config.LoadedConfig, error) {
	return config.ResolveConfig(config.MergeResult{Value: config.PartialAppConfig{
		Lifecycle: config.PartialLifecycleConfig{
			CleanupTimeoutMS: config.Optional[int64]{Set: true, Value: milliseconds},
		},
	}}, config.LoadOptions{})
}

// TestEveryPublishedConfigKeyReachesOwningOptions is an Assembly-level owner
// audit. It intentionally works from the production AST/text boundary rather
// than leaf defaults: public values must be taken from the final resolved
// config and supplied to the constructor that owns them.
func TestEveryPublishedConfigKeyReachesOwningOptions(t *testing.T) {
	source := t427AssemblySource(t)
	for owner, fields := range map[string][]string{
		"artifact store": {
			"Root:          artifactRoot",
			"MaxFileBytes:  resolved.Artifact.MaxFileBytes",
			"MaxTotalBytes: resolved.Artifact.MaxTotalBytes",
			"Retention:     time.Duration(resolved.Artifact.RetentionDays) * 24 * time.Hour",
		},
		"process runner": {
			"proctree.NewRunner(proctree.Options{",
			"CleanupTimeout: state.configuration.cleanupTimeout",
			"Diagnostics:    state.configuration.diagnostics",
		},
		"MCP manager": {
			"mcpclient.NewManager(resolved.MCP, mcpclient.ManagerOptions{",
			"MaxTools:          int(resolved.MCP.MaxTools)",
			"MaxPages:          int(resolved.MCP.MaxPages)",
			"MaxResponseBytes:  resolved.MCP.MaxResponseBytes",
			"MaxProtocolErrors: resolved.MCP.MaxProtocolErrors",
			"DefaultTimeout:    time.Duration(resolved.MCP.DefaultTimeoutMS) * time.Millisecond",
			"CleanupTimeout:    cleanupTimeout",
			"Diagnostics:       state.configuration.diagnostics",
		},
		"provider": {
			"provider.NewWithOptions(resolved.LLM, provider.ProviderOptions{",
			"Timeout:      time.Duration(resolved.LLM.RequestTimeoutMS) * time.Millisecond",
			"CleanupTimeout:  cleanupTimeout",
			"Diagnostics:     state.configuration.diagnostics",
		},
	} {
		for _, field := range fields {
			if !t427ContainsMarker(source, field) {
				t.Fatalf("%s no longer receives its resolved public configuration: missing %q", owner, field)
			}
		}
	}
	if got := strings.Count(source, "diagnostics.NewBoundedSink("); got != 1 {
		t.Fatalf("Assembly creates %d bounded diagnostics sinks, want one resolved owner", got)
	}
	// The legacy Collector remains the event/status projection consumed by the
	// existing App and Hook compatibility surfaces. It is not a C7 lifecycle
	// sink and must not be confused with, or substituted for, the one
	// diagnostics.BoundedSink created by the Config stage.
	if got := strings.Count(source, "diagnostics.NewCollector("); got != 1 {
		t.Fatalf("Assembly creates %d legacy diagnostics collectors, want one compatibility projection", got)
	}
	hookOptions := t427OwnerOptions(t, source, "Hook Engine", "hook.NewEngine(hookSnapshot, hook.EngineOptions{", "})")
	if !t427ContainsMarker(hookOptions, "Diagnostics: state.configuration.diagnostics") ||
		!t427ContainsMarker(hookOptions, "LegacyDiagnostics: diagnosticCollector") {
		t.Fatal("Hook Engine no longer keeps the bounded C7 sink distinct from the legacy diagnostics projection")
	}
}

// TestResolvedConfigReachesOwners checks the C7 fan-out which cannot be
// inferred from leaf package tests: the one resolved timeout and one bounded
// sink must reach every Assembly-owned lifecycle participant. T4.25f permits
// Assembly to retain App RuntimeOptions in the unpublished UI candidate, but
// deliberately forbids constructing or publishing the App itself.
func TestResolvedConfigReachesOwners(t *testing.T) {
	source := t427AssemblySource(t)
	for _, owner := range []struct {
		name    string
		start   string
		end     string
		markers []string
	}{
		{
			name: "Process", start: "proctree.NewRunner(proctree.Options{", end: "})",
			markers: []string{
				"CleanupTimeout: state.configuration.cleanupTimeout",
				"Diagnostics:    state.configuration.diagnostics",
			},
		},
		{
			name: "Provider/ChatStream", start: "provider.NewWithOptions(resolved.LLM, provider.ProviderOptions{", end: "})",
			markers: []string{
				"CleanupTimeout:  cleanupTimeout",
				"Diagnostics:     state.configuration.diagnostics",
			},
		},
		{
			name: "Hook Engine", start: "hook.NewEngine(hookSnapshot, hook.EngineOptions{", end: "})",
			markers: []string{
				"Diagnostics: state.configuration.diagnostics",
				"CleanupTimeout: cleanupTimeout",
			},
		},
		{
			name: "MCP Manager", start: "mcpclient.NewManager(resolved.MCP, mcpclient.ManagerOptions{", end: "}, mcpclient.ManagerDependencies{",
			markers: []string{
				"CleanupTimeout:    cleanupTimeout",
				"Diagnostics:       state.configuration.diagnostics",
			},
		},
		{
			name: "MCP dependencies", start: "mcpclient.ManagerDependencies{", end: "})",
			markers: []string{
				"Diagnostics:       state.configuration.diagnostics",
			},
		},
		{
			name: "Orchestrator", start: "orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{", end: "})",
			markers: []string{
				"CleanupTimeout:       state.configuration.cleanupTimeout",
				"LifecycleDiagnostics: state.configuration.diagnostics",
			},
		},
		{
			name: "unpublished App RuntimeOptions", start: "runtimeOptions = app.RuntimeOptions{", end: "}",
			markers: []string{
				"CleanupTimeout: state.configuration.cleanupTimeout",
				"Diagnostics:    state.configuration.diagnostics",
			},
		},
	} {
		options := t427OwnerOptions(t, source, owner.name, owner.start, owner.end)
		for _, marker := range owner.markers {
			if !t427ContainsMarker(options, marker) {
				t.Fatalf("resolved C7 timeout/bounded sink does not reach %s options: missing %q", owner.name, marker)
			}
		}
	}
	for _, forbidden := range []string{"app.New("} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("Assembly retains a legacy App constructor: found %q", forbidden)
		}
	}
	if strings.Contains(source, "cleanupTimeout = 5 * time.Second") {
		t.Fatal("Assembly lifecycle owner retains an unapproved cleanup timeout fallback")
	}
}

func t427OwnerOptions(t *testing.T, source, owner, start, end string) string {
	t.Helper()
	startIndex := strings.Index(source, start)
	if startIndex < 0 {
		t.Fatalf("%s options constructor is missing: %q", owner, start)
	}
	tail := source[startIndex:]
	endIndex := strings.Index(tail, end)
	if endIndex < 0 {
		t.Fatalf("%s options constructor has no closing marker %q", owner, end)
	}
	return tail[:endIndex+len(end)]
}

func t427ContainsMarker(source, marker string) bool {
	return strings.Contains(strings.Join(strings.Fields(source), " "), strings.Join(strings.Fields(marker), " "))
}

func t427AssemblySource(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatalf("read assembly source: %v", err)
	}
	return string(source)
}
