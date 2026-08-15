package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

func assertLegacyProductionEntryUnchanged(t *testing.T) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal("parse production entry failed")
	}
	forbidden := map[string]struct{}{
		"assemblyFactories": {}, "assemblyRuntime": {}, "ownershipRegistry": {},
		"newOwnershipRegistry": {}, "buildCandidate": {},
	}
	var mainFunction, runArgsFunction *ast.FuncDecl
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			if _, blocked := forbidden[value.Name]; blocked {
				t.Fatalf("legacy production entry referenced candidate assembly identifier %q", value.Name)
			}
		case *ast.FuncDecl:
			switch value.Name.Name {
			case "main":
				mainFunction = value
			case "runArgs":
				runArgsFunction = value
			}
		}
		return true
	})
	if mainFunction == nil || runArgsFunction == nil {
		t.Fatal("legacy production entry functions are unavailable")
	}
	if countDirectCall(mainFunction.Body, "runCLI", func(call *ast.CallExpr) bool {
		return len(call.Args) == 4 && identifierName(call.Args[3]) == "runArgs"
	}) != 1 {
		t.Fatal("main no longer delegates exactly once to runCLI with runArgs")
	}
	if countDirectCall(runArgsFunction.Body, "buildAssemblyFromCLI", func(call *ast.CallExpr) bool {
		return len(call.Args) == 1 && identifierName(call.Args[0]) == "args"
	}) != 1 {
		t.Fatal("runArgs no longer uses the single Assembly build path")
	}
}

func TestCLIAssemblyOptionsUseExplicitSystemInputs(t *testing.T) {
	options, err := assemblyOptionsFromSystem([]string{"--config", "config.yaml"})
	if err != nil {
		t.Fatalf("assemblyOptionsFromSystem: %v", err)
	}
	for name, root := range map[string]string{
		"project":     options.Paths.ProjectRoot,
		"user config": options.Paths.UserConfigRoot,
		"user data":   options.Paths.UserDataRoot,
		"user cache":  options.Paths.UserCacheRoot,
	} {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			t.Fatalf("%s root is not absolute and canonical: %q", name, root)
		}
	}
	if options.Config.ProjectPath == "" || !filepath.IsAbs(options.Config.ProjectPath) ||
		filepath.Clean(options.Config.ProjectPath) != options.Config.ProjectPath {
		t.Fatalf("project configuration layer is not an absolute canonical path: %#v", options.Config)
	}
	if options.Config.UserPath != "" && (!filepath.IsAbs(options.Config.UserPath) || filepath.Clean(options.Config.UserPath) != options.Config.UserPath) {
		t.Fatalf("user configuration layer is not an absolute canonical path: %#v", options.Config)
	}
	if options.LookupEnv == nil || options.Stdin != os.Stdin || options.Stdout != os.Stdout || options.Stderr != os.Stderr || options.TrustedRoots != nil {
		t.Fatalf("CLI Assembly options did not retain explicit I/O, environment lookup, and system trust: %#v", options)
	}
}

func assertAssemblyCandidateHasNoOtherProductionReference(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal("read command package failed")
	}
	// cli.go contains only the pre-cutover Options→Build seam required by
	// T4.25h; it does not publish or run the candidate UI.
	allowed := map[string]struct{}{"assembly.go": {}, "cli.go": {}, "lifecycle.go": {}}
	forbidden := map[string]struct{}{
		"assemblyFactories":    {},
		"assemblyRuntime":      {},
		"ownershipRegistry":    {},
		"newOwnershipRegistry": {},
		"buildCandidate":       {},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal("parse command production file failed")
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok {
				if _, blocked := forbidden[identifier.Name]; blocked {
					t.Fatalf("%s referenced unpublished assembly candidate %q", name, identifier.Name)
				}
			}
			return true
		})
	}
}

func countDirectCall(body *ast.BlockStmt, name string, matches func(*ast.CallExpr) bool) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && identifierName(call.Fun) == name && matches(call) {
			count++
		}
		return true
	})
	return count
}

func identifierName(expression ast.Expr) string {
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func TestNewMemoryManagerHonorsEnabled(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	disabled := false
	disabledManager := newMemoryManager(t.TempDir(), config.MemoryConfig{Enabled: &disabled}, nil, runtimeRedactor)
	if disabledManager != nil {
		t.Fatal("memory.enabled=false constructed a production memory manager")
	}
	if manager := newSessionManager(nil, disabledManager, nil); manager.Memory != nil {
		t.Fatal("disabled memory became a non-nil typed interface in the session manager")
	}

	enabled := true
	enabledManager := newMemoryManager(t.TempDir(), config.MemoryConfig{Enabled: &enabled}, nil, runtimeRedactor)
	if enabledManager == nil {
		t.Fatal("memory.enabled=true did not construct a production memory manager")
	}
	if manager := newSessionManager(nil, enabledManager, nil); manager.Memory == nil {
		t.Fatal("enabled memory was not attached to the session manager")
	}
	if manager := newMemoryManager(t.TempDir(), config.MemoryConfig{}, nil, runtimeRedactor); manager == nil {
		t.Fatal("unset memory.enabled must preserve the enabled-by-default behavior")
	}
}

func TestConversationStoreOptionsCarryResolvedSessionLimits(t *testing.T) {
	projectRoot := t.TempDir()
	runtimeRedactor := redact.NewRuntimeRedactor()
	session := config.SessionConfig{
		Dir:             "sessions",
		MaxRecordBytes:  101,
		MaxSessionBytes: 202,
		MaxScanFiles:    303,
		MaxScanBytes:    404,
		RetentionDays:   30,
		GapReminderDays: 7,
	}

	options := conversationStoreOptions(session, projectRoot, runtimeRedactor)
	if options.DataDir != filepath.Join(projectRoot, "sessions") ||
		options.MaxRecordBytes != 101 || options.MaxSessionBytes != 202 ||
		options.MaxScanFiles != 303 || options.MaxScanBytes != 404 ||
		options.RetentionDays != 30 || options.GapReminderDays != 7 ||
		options.Redactor != runtimeRedactor {
		t.Fatalf("conversation store options lost resolved config: %#v", options)
	}
}

func TestStartupSkillAssemblyLoadsThreeTiers(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := filepath.Join(t.TempDir(), "skills")
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(userRoot, "commit.md"), "commit", "user commit", "shared", "Read")
	writeStartupSkill(t, filepath.Join(projectSkills, "commit.md"), "commit", "project commit", "shared", "Read")
	writeStartupSkill(t, filepath.Join(projectSkills, "clear.md"), "clear", "reserved skill", "shared", "Read")

	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	manager, err := newSkillManager(projectRoot, userRoot, registry, redact.Text)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.Catalog) < 4 {
		t.Fatalf("expected builtins plus project skill, got %#v", snapshot.Catalog)
	}
	commit, ok := manager.Resolve("commit")
	if !ok || commit.Source != skill.SourceProject || commit.Description != "project commit" {
		t.Fatalf("project override did not win: %#v ok=%v", commit, ok)
	}
	var clearFound bool
	for _, item := range snapshot.Catalog {
		if item.Name == "clear" {
			clearFound = true
			if item.SlashEnabled {
				t.Fatal("reserved command conflict unexpectedly received a slash command")
			}
		}
	}
	if !clearFound {
		t.Fatal("reserved-name skill should remain loadable in catalog")
	}
}

func TestStartupSkillAssemblyRejectsUnknownTool(t *testing.T) {
	projectRoot := t.TempDir()
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(projectSkills, "bad.md"), "bad", "bad tool", "shared", "MissingTool")
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if _, err := newSkillManager(projectRoot, filepath.Join(t.TempDir(), "missing"), registry, redact.Text); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool startup error, got %v", err)
	}
}

func TestStartupSkillAssemblyAcceptsRegisteredExtensionTool(t *testing.T) {
	projectRoot := t.TempDir()
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(projectSkills, "extended.md"), "extended", "extension tool", "shared", "mcp__demo__search")
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(startupTestTool{name: "mcp__demo__search"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if _, err := newSkillManager(projectRoot, filepath.Join(t.TempDir(), "missing"), registry, redact.Text); err != nil {
		t.Fatalf("registered extension tool was rejected: %v", err)
	}
}

func writeStartupSkill(t *testing.T, path string, name string, description string, mode string, allowedTool string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: " + description + "\nallowed_tools: [" + allowedTool + "]\nmode: " + mode + "\n---\nSOP {{args}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type startupTestTool struct{ name string }

func (t startupTestTool) Name() string      { return t.name }
func (startupTestTool) Description() string { return "test extension tool" }
func (startupTestTool) Schema() tool.Schema { return tool.ObjectSchema(nil, nil) }
func (startupTestTool) Risk() tool.Risk     { return tool.RiskSafe }
func (startupTestTool) Execute(_ context.Context, input tool.Input) tool.Result {
	return tool.Success(input, "ok", "ok", nil)
}
