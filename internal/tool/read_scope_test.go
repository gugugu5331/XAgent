package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/permission"
)

func TestReadScopeResolution(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "extra-alias")
	if err := os.Symlink(extra, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(project, "same.txt"), []byte("project-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "same.txt"), []byte("extra-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "extra-only.txt"), []byte("extra-only"), 0o600); err != nil {
		t.Fatal(err)
	}

	scope, err := NewReadScope(project, []string{extra, alias, project})
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.ExtraRoots) != 1 {
		t.Fatalf("roots were not canonicalized and deduplicated: %#v", scope)
	}
	ctx := WithReadScope(context.Background(), scope)
	scope.ExtraRoots[0] = project
	stored, ok := ReadScopeFromContext(ctx)
	if !ok || len(stored.ExtraRoots) != 1 || stored.ExtraRoots[0] == project {
		t.Fatalf("context did not keep a defensive copy: %#v", stored)
	}

	executor := newScopedExecutor(t, project)
	projectResult := executeScoped(t, executor, ctx, "Read", map[string]any{"path": "same.txt"})
	if projectResult.Status != StatusSuccess || projectResult.Content != "project-version" || projectResult.Data["path"] != "same.txt" {
		t.Fatalf("relative path did not prefer project root: %#v", projectResult)
	}
	extraResult := executeScoped(t, executor, ctx, "Read", map[string]any{"path": "extra-only.txt"})
	if extraResult.Status != StatusSuccess || extraResult.Content != "extra-only" {
		t.Fatalf("relative path did not fall back to extra root: %#v", extraResult)
	}
	display, _ := extraResult.Data["path"].(string)
	if !strings.HasPrefix(display, "skill:") || !strings.HasSuffix(display, "/extra-only.txt") || strings.Contains(display, filepath.Dir(extra)) {
		t.Fatalf("extra-root display path is unstable or leaks parent path: %q", display)
	}

	absoluteResult := executeScoped(t, executor, ctx, "Read", map[string]any{"path": filepath.Join(extra, "same.txt")})
	if absoluteResult.Status != StatusSuccess || absoluteResult.Content != "extra-version" {
		t.Fatalf("absolute extra-root read failed: %#v", absoluteResult)
	}
	outside := executeScoped(t, executor, ctx, "Read", map[string]any{"path": filepath.Join(t.TempDir(), "outside.txt")})
	if outside.Status != StatusError || outside.Error == nil || outside.Error.Code != ErrPathOutsideProject {
		t.Fatalf("outside absolute path was not rejected: %#v", outside)
	}
}

func TestReadToolExtraRoot(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	path := filepath.Join(extra, "reference.md")
	if err := os.WriteFile(path, []byte("reference canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	result := executeScoped(t, newScopedExecutor(t, project), WithReadScope(context.Background(), scope), "Read", map[string]any{"path": path})
	if result.Status != StatusSuccess || result.Content != "reference canary" || !strings.Contains(result.Summary, "skill:") {
		t.Fatalf("Read did not use extra root: %#v", result)
	}
}

func TestGlobToolExtraRoot(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(extra, "template.md"), []byte("template"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	result := executeScoped(t, newScopedExecutor(t, project), WithReadScope(context.Background(), scope), "Glob", map[string]any{"pattern": "*.md"})
	if result.Status != StatusSuccess || !strings.Contains(result.Content, "skill:") || !strings.Contains(result.Content, "template.md") {
		t.Fatalf("Glob did not search extra root: %#v", result)
	}
	invalid := executeScoped(t, newScopedExecutor(t, project), WithReadScope(context.Background(), scope), "Glob", map[string]any{"pattern": "["})
	if invalid.Status != StatusError || invalid.Error == nil || invalid.Error.Code != ErrInvalidArguments {
		t.Fatalf("invalid glob changed error classification: %#v", invalid)
	}
}

func TestGrepToolExtraRoot(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(extra, "example.md"), []byte("grep canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	result := executeScoped(t, newScopedExecutor(t, project), WithReadScope(context.Background(), scope), "Grep", map[string]any{"pattern": "grep canary", "path": extra})
	if result.Status != StatusSuccess || !strings.Contains(result.Content, "skill:") || !strings.Contains(result.Content, "example.md:1") {
		t.Fatalf("Grep did not search extra root: %#v", result)
	}
}

func TestReadGlobAndGrepExtraRoots(t *testing.T) {
	project := t.TempDir()
	extraOne := t.TempDir()
	extraTwo := t.TempDir()
	for path, content := range map[string]string{
		filepath.Join(project, "project.txt"): "project canary",
		filepath.Join(extraOne, "first.txt"):  "first skill canary",
		filepath.Join(extraTwo, "second.txt"): "second skill canary",
		filepath.Join(extraTwo, "ignored.go"): "second skill canary",
		filepath.Join(extraTwo, "nested.md"):  "markdown",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := NewReadScope(project, []string{extraOne, extraTwo})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadScope(context.Background(), scope)
	executor := newScopedExecutor(t, project)

	glob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "*.txt"})
	if glob.Status != StatusSuccess {
		t.Fatalf("glob failed: %#v", glob)
	}
	for _, want := range []string{"project.txt", "first.txt", "second.txt", "skill:"} {
		if !strings.Contains(glob.Content, want) {
			t.Fatalf("glob output missing %q: %q", want, glob.Content)
		}
	}
	lines := strings.Split(glob.Content, "\n")
	for index := 1; index < len(lines); index++ {
		if lines[index-1] > lines[index] {
			t.Fatalf("glob output is not stably sorted: %#v", lines)
		}
	}
	exactGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "project.txt"})
	if exactGlob.Status != StatusSuccess || exactGlob.Content != "project.txt" {
		t.Fatalf("exact project glob changed: %#v", exactGlob)
	}
	absoluteGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": filepath.Join(extraOne, "*.txt")})
	if absoluteGlob.Status != StatusSuccess || !strings.Contains(absoluteGlob.Content, "first.txt") || strings.Contains(absoluteGlob.Content, "project.txt") || strings.Contains(absoluteGlob.Content, "second.txt") {
		t.Fatalf("absolute extra-root glob escaped its selected root: %#v", absoluteGlob)
	}
	outsideGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": filepath.Join(t.TempDir(), "*.txt")})
	if outsideGlob.Status != StatusError || outsideGlob.Error == nil || outsideGlob.Error.Code != ErrPathOutsideProject {
		t.Fatalf("absolute outside glob was not rejected: %#v", outsideGlob)
	}

	grep := executeScoped(t, executor, ctx, "Grep", map[string]any{"pattern": "second skill canary", "path": extraTwo})
	if grep.Status != StatusSuccess || !strings.Contains(grep.Content, "skill:") || !strings.Contains(grep.Content, "second.txt:1") || !strings.Contains(grep.Content, "ignored.go:1") {
		t.Fatalf("grep extra root failed: %#v", grep)
	}
	projectGrep := executeScoped(t, executor, ctx, "Grep", map[string]any{"pattern": "project canary"})
	if projectGrep.Status != StatusSuccess || !strings.Contains(projectGrep.Content, "project.txt:1") {
		t.Fatalf("default grep root changed: %#v", projectGrep)
	}
}

func TestReadScopeRejectsSymlinkEscapesAndWriteExpansion(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("OUTSIDE_CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(extra, "inside.txt")
	if err := os.WriteFile(insideFile, []byte("inside canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	insideDir := filepath.Join(extra, "inside-dir")
	if err := os.Mkdir(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(insideDir, "nested.txt"), []byte("nested canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(extra, "secret-link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(extra, "outside-dir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(insideFile, filepath.Join(extra, "inside-link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(insideDir, filepath.Join(extra, "inside-dir-link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadScope(context.Background(), scope)
	executor := newScopedExecutor(t, project)

	readEscape := executeScoped(t, executor, ctx, "Read", map[string]any{"path": filepath.Join(extra, "secret-link.txt")})
	if readEscape.Status != StatusError || readEscape.Error == nil || readEscape.Error.Code != ErrPathOutsideProject {
		t.Fatalf("Read followed an escaping symlink: %#v", readEscape)
	}
	readInside := executeScoped(t, executor, ctx, "Read", map[string]any{"path": filepath.Join(extra, "inside-link.txt")})
	if readInside.Status != StatusSuccess || readInside.Content != "inside canary" {
		t.Fatalf("Read should allow an in-root symlink target: %#v", readInside)
	}
	glob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "*.txt"})
	if glob.Status != StatusSuccess || strings.Contains(glob.Content, "secret") || !strings.Contains(glob.Content, "inside.txt") {
		t.Fatalf("Glob exposed escaping symlink: %#v", glob)
	}
	nestedGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "*/*.txt"})
	if nestedGlob.Status != StatusSuccess || strings.Contains(nestedGlob.Content, "secret") || !strings.Contains(nestedGlob.Content, "inside-dir/nested.txt") {
		t.Fatalf("Glob followed an outside directory symlink or missed safe content: %#v", nestedGlob)
	}
	safeLinkGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "inside-dir-link/*.txt"})
	if safeLinkGlob.Status != StatusSuccess || !strings.Contains(safeLinkGlob.Content, "inside-dir/nested.txt") {
		t.Fatalf("Glob rejected a symlink whose target stays inside the root: %#v", safeLinkGlob)
	}
	escapeGlob := executeScoped(t, executor, ctx, "Glob", map[string]any{"pattern": "outside-dir/*.txt"})
	if escapeGlob.Status != StatusError || escapeGlob.Error == nil || escapeGlob.Error.Code != ErrPathOutsideProject {
		t.Fatalf("Glob traversed an explicit outside directory symlink: %#v", escapeGlob)
	}
	grep := executeScoped(t, executor, ctx, "Grep", map[string]any{"pattern": "OUTSIDE_CANARY", "path": extra})
	if grep.Status != StatusError || grep.Error == nil || grep.Error.Code != ErrNoResults || strings.Contains(grep.Content, "OUTSIDE_CANARY") {
		t.Fatalf("Grep exposed escaping symlink content: %#v", grep)
	}
	grepDirEscape := executeScoped(t, executor, ctx, "Grep", map[string]any{"pattern": "OUTSIDE_CANARY", "path": filepath.Join(extra, "outside-dir")})
	if grepDirEscape.Status != StatusError || grepDirEscape.Error == nil || grepDirEscape.Error.Code != ErrPathOutsideProject {
		t.Fatalf("Grep followed an escaping directory symlink: %#v", grepDirEscape)
	}

	writeTarget := filepath.Join(extra, "new.txt")
	write := executeScoped(t, executor, ctx, "Write", map[string]any{"path": writeTarget, "content": "must not write"})
	if write.Status != StatusError || write.Error == nil || write.Error.Code != ErrPathOutsideProject {
		t.Fatalf("extra read root expanded Write authority: %#v", write)
	}
	if _, err := os.Stat(writeTarget); !os.IsNotExist(err) {
		t.Fatalf("Write created an extra-root file: %v", err)
	}
	edit := executeScoped(t, executor, ctx, "Edit", map[string]any{"path": insideFile, "old_text": "inside", "new_text": "changed"})
	if edit.Status != StatusError || edit.Error == nil || edit.Error.Code != ErrPathOutsideProject {
		t.Fatalf("extra read root expanded Edit authority: %#v", edit)
	}
	data, err := os.ReadFile(insideFile)
	if err != nil || string(data) != "inside canary" {
		t.Fatalf("Edit changed extra-root file: data=%q err=%v", data, err)
	}
}

func TestReadScopeCannotReplaceConfiguredProjectRoot(t *testing.T) {
	project := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "file.txt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(other, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := executeScoped(t, newScopedExecutor(t, project), WithReadScope(context.Background(), scope), "Read", map[string]any{"path": "file.txt"})
	if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrPathOutsideProject {
		t.Fatalf("context replaced configured project root: %#v", result)
	}
}

func TestReadScopeAuthorizerTicketMatchesExecutor(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	path := filepath.Join(extra, "reference.md")
	if err := os.WriteFile(path, []byte("fingerprint canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "read-extra", Name: "Read", ArgumentsJSON: string(raw)}
	permissionCall := permission.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.ArgumentsJSON}
	normalized, err := permission.NormalizeCallWithReadRoots(permissionCall, project, scope.ExtraRoots)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(normalized.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := permission.NewCallIdentity(permission.CallIdentityInput{ToolName: call.Name, CanonicalArguments: canonical})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	decision := (&permission.Authorizer{Issuer: authority}).Decide(permissionCall, permission.Context{
		ProjectRoot: project,
		ReadRoots:   scope.ExtraRoots,
		Mode:        permission.ModeDefault,
		Identity:    identity,
	})
	if decision.Kind != permission.DecisionAllow || !decision.Ticket.Issued() {
		t.Fatalf("authorizer did not ticket extra-root Read: %#v", decision)
	}

	ctx := WithReadScope(context.Background(), scope)
	executor := newScopedExecutor(t, project)
	executor.TicketVerifier = authority
	result := executor.ExecuteAuthorized(ctx, call, decision.Ticket)
	if result.Status != StatusSuccess || result.Content != "fingerprint canary" {
		t.Fatalf("executor rejected the authorizer's extra-root ticket: %#v", result)
	}
}

func TestExtraReadRootsExpandReadToolsOnly(t *testing.T) {
	project := t.TempDir()
	extra := t.TempDir()
	path := filepath.Join(extra, "reference.txt")
	if err := os.WriteFile(path, []byte("scope canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := NewReadScope(project, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadScope(context.Background(), scope)
	executor := newScopedExecutor(t, project)

	readCases := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "Read", args: map[string]any{"path": path}, want: "scope canary"},
		{name: "Glob", args: map[string]any{"pattern": filepath.Join(extra, "*.txt")}, want: "reference.txt"},
		{name: "Grep", args: map[string]any{"pattern": "scope canary", "path": extra}, want: "reference.txt:1"},
	}
	for _, test := range readCases {
		t.Run(test.name, func(t *testing.T) {
			result := executeScoped(t, executor, ctx, test.name, test.args)
			if result.Status != StatusSuccess || !strings.Contains(result.Content, test.want) {
				t.Fatalf("%s could not access an extra read root: %#v", test.name, result)
			}
		})
	}

	writeTarget := filepath.Join(extra, "created.txt")
	writeCases := []struct {
		name string
		args map[string]any
	}{
		{name: "Write", args: map[string]any{"path": writeTarget, "content": "must not write"}},
		{name: "Edit", args: map[string]any{"path": path, "old_text": "scope", "new_text": "changed"}},
	}
	for _, test := range writeCases {
		t.Run(test.name, func(t *testing.T) {
			result := executeScoped(t, executor, ctx, test.name, test.args)
			if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrPathOutsideProject {
				t.Fatalf("extra read root expanded %s authority: %#v", test.name, result)
			}
		})
	}
	if _, err := os.Stat(writeTarget); !os.IsNotExist(err) {
		t.Fatalf("Write created a file in the extra root: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "scope canary" {
		t.Fatalf("Edit changed an extra-root file: %q", data)
	}

	bashCall := permission.Call{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"printf unsafe > ` + writeTarget + `"}`}
	withoutRoots, err := permission.NormalizeCallWithReadRoots(bashCall, project, nil)
	if err != nil {
		t.Fatal(err)
	}
	withRoots, err := permission.NormalizeCallWithReadRoots(bashCall, project, scope.ExtraRoots)
	if err != nil {
		t.Fatal(err)
	}
	if withRoots.FingerprintValue != withoutRoots.FingerprintValue || withRoots.RuleValue != withoutRoots.RuleValue {
		t.Fatalf("extra read roots changed Bash authorization identity: without=%#v with=%#v", withoutRoots, withRoots)
	}
	authorizer := &permission.Authorizer{}
	decision := authorizer.Decide(bashCall, permission.Context{ProjectRoot: project, ReadRoots: scope.ExtraRoots})
	if decision.Kind != permission.DecisionAsk {
		t.Fatalf("extra read root silently authorized Bash: %#v", decision)
	}
	if _, err := os.Stat(writeTarget); !os.IsNotExist(err) {
		t.Fatalf("Bash authorization check changed the extra root: %v", err)
	}
}

func TestPinnedReadScopeRejectsRootReplacedBySymlink(t *testing.T) {
	project := t.TempDir()
	parent := t.TempDir()
	packageRoot := filepath.Join(parent, "activated-skill")
	if err := os.Mkdir(packageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside canary"), 0o600); err != nil {
		t.Fatal(err)
	}

	activated, err := NewReadScope(project, []string{packageRoot})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := NewPinnedReadScope(project, activated.ExtraRoots)
	if err != nil {
		t.Fatalf("canonical root was not accepted at activation: %v", err)
	}
	if err := os.Remove(packageRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, packageRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if _, err := NewPinnedReadScope(project, pinned.ExtraRoots); err == nil {
		t.Fatal("pinned scope accepted a package root replaced by an outside symlink")
	}
	ctx := WithReadScope(context.Background(), pinned)
	result := executeScoped(t, newScopedExecutor(t, project), ctx, "Read", map[string]any{"path": filepath.Join(packageRoot, "secret.txt")})
	if result.Status != StatusError || result.Error == nil || strings.Contains(result.Content, "outside canary") {
		t.Fatalf("effective execution followed the replaced package root: %#v", result)
	}
}

func newScopedExecutor(t *testing.T, projectRoot string) *Executor {
	t.Helper()
	registry, err := NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(registry, projectRoot, time.Second, 32*1024)
}

func executeScoped(t *testing.T, executor *Executor, ctx context.Context, name string, args map[string]any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return executor.Execute(ctx, Call{ID: name + "-call", Name: name, ArgumentsJSON: string(raw)})
}
