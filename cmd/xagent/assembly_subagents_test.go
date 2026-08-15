package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestAssemblySubagentSourcesUseOnlyExplicitRuntimeRoots(t *testing.T) {
	t.Parallel()

	paths := RuntimePaths{ProjectRoot: t.TempDir(), UserConfigRoot: t.TempDir()}
	sources, err := assemblySubagentRoleSources(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("role sources = %#v", sources)
	}
	want := []struct {
		source agentrole.Source
		id     string
		root   string
	}{
		{agentrole.SourceBuiltin, "builtin", "builtins"},
		{agentrole.SourceUser, "user", "agents"},
		{agentrole.SourceProject, "project", filepath.ToSlash(filepath.Join(".xagent", "agents"))},
	}
	for index := range want {
		if sources[index].Source != want[index].source || sources[index].ID != want[index].id ||
			sources[index].Root != want[index].root || sources[index].FS == nil {
			t.Fatalf("source %d = %#v, want %#v", index, sources[index], want[index])
		}
	}
}

func TestAssemblySubagentToolMetadataComesFromSealedRegistry(t *testing.T) {
	t.Parallel()

	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assemblySubagentToolMetadata(registry); err == nil {
		t.Fatal("unsealed registry exported role metadata")
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	metadata, err := assemblySubagentToolMetadata(registry)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]agentrole.ToolMetadata, len(metadata))
	for _, item := range metadata {
		byName[item.Name] = item
	}
	if !reflect.DeepEqual(byName["Read"], agentrole.ToolMetadata{
		Name: "Read", ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true,
	}) {
		t.Fatalf("Read metadata = %#v", byName["Read"])
	}
	if !reflect.DeepEqual(byName["Write"], agentrole.ToolMetadata{Name: "Write"}) {
		t.Fatalf("Write metadata = %#v", byName["Write"])
	}
}

func TestAssemblySubagentBuilderFailsClosedBeforeSealedRegistry(t *testing.T) {
	t.Parallel()

	graph, err := newAssemblySubagents(context.Background(), assemblySubagentRequest{
		Registry: tool.NewSafeCandidateRegistry(),
	})
	if err == nil || graph != nil {
		t.Fatalf("unsealed/incomplete subagent graph = %#v, err=%v", graph, err)
	}
}

func TestConfigExampleIncludesResolvableSubagentConfiguration(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "config.example.yaml")
	partial, err := config.DecodePartial(path)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Subagent.MaxConcurrent.Set || !partial.Subagent.RoleLimits.MaxFiles.Set ||
		!partial.Subagent.BackgroundTools.Set || !partial.LLM.ModelAliases.Haiku.Set {
		t.Fatalf("config example omitted subagent presence fields: %#v", partial.Subagent)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
