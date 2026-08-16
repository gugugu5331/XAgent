package worktree

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type recordingGitRunner struct {
	commands []GitCommand
	outputs  []GitOutput
	errors   []error
}

func (r *recordingGitRunner) Run(_ context.Context, command GitCommand) (GitOutput, error) {
	r.commands = append(r.commands, command.Clone())
	var output GitOutput
	if len(r.outputs) > 0 {
		output, r.outputs = r.outputs[0], r.outputs[1:]
	}
	var err error
	if len(r.errors) > 0 {
		err, r.errors = r.errors[0], r.errors[1:]
	}
	return output, err
}

func newRecordingGitClient(runner *recordingGitRunner) *GitClient {
	return NewGitClient("git", runner, time.Second, 4096)
}

func TestGitCommandIsStructuredAndBounded(t *testing.T) {
	command, err := NewGitCommand("git", "/repo", time.Second, 1024, "rev-parse", "--verify", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if command.Executable != "git" || command.CWD != "/repo" || !reflect.DeepEqual(command.Args, []string{"rev-parse", "--verify", "HEAD"}) {
		t.Fatalf("命令未使用结构化 argv：%#v", command)
	}
	command.Args[0] = "changed"
	if command.Clone().Args[0] != "changed" {
		t.Fatal("Clone 基本行为错误")
	}
}

func TestForbiddenGitCommandsFailClosed(t *testing.T) {
	tests := [][]string{
		{"worktree", "remove", "--force", "/tmp/w"},
		{"worktree", "repair"},
		{"worktree", "prune"},
		{"reset", "--hard"},
		{"clean", "-fd"},
		{"branch", "-D", "x"},
		{"push", "origin", "--delete", "x"},
	}
	for _, args := range tests {
		if _, err := NewGitCommand("git", "/repo", time.Second, 1024, args...); !errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("禁用 argv 未被拒绝：%q (%v)", args, err)
		}
	}
}

func TestGitCommandRejectsUnapprovedShapes(t *testing.T) {
	tests := [][]string{
		{"rev-parse", "--git-dir"},
		{"status", "--short"},
		{"config", "--global", "core.hooksPath", "x"},
		{"config", "extensions.worktreeConfig", "true", "extra"},
		{"config", "--unset", "core.hooksPath"},
		{"config", "--worktree", "--unset", "extensions.worktreeConfig"},
		{"update-ref", "refs/heads/x", strings.Repeat("a", 40)},
		{"worktree", "add", "--detach", "/tmp/w", strings.Repeat("a", 40)},
	}
	for _, args := range tests {
		if _, err := NewGitCommand("git", "/repo", time.Second, 1024, args...); !errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("非 allowlist 形状未被拒绝：%q (%v)", args, err)
		}
	}
}

func TestGitClientAppliesCombinedOutputLimit(t *testing.T) {
	runner := &recordingGitRunner{outputs: []GitOutput{{Stdout: []byte("123456"), Stderr: []byte("abcdef")}}}
	client := NewGitClient("git", runner, time.Second, 10)
	if _, err := client.ConfigGet(context.Background(), "/repo", "core.hooksPath"); !errors.Is(err, ErrGitOutputLimit) {
		t.Fatalf("stdout/stderr 合计超过限制必须拒绝：%v", err)
	}
}

func TestGitReaderUsesExactArgv(t *testing.T) {
	runner := &recordingGitRunner{outputs: []GitOutput{
		{Stdout: []byte(strings.Repeat("a", 40) + "\n")},
		{Stdout: []byte("worktree /repo\x00HEAD " + strings.Repeat("a", 40) + "\x00branch refs/heads/main\x00\x00")},
		{Stdout: []byte("1 .M N... 100644 100644 100644 " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " file.go\x00")},
		{Stdout: []byte("refs/heads/main\n")},
		{Stdout: []byte("refs/heads/main\t" + strings.Repeat("a", 40) + "\trefs/remotes/origin/main\n")},
		{},
		{Stdout: []byte("true\n")},
	}}
	client := newRecordingGitClient(runner)
	ctx := context.Background()
	if _, err := client.ResolveHEAD(ctx, "/repo"); err != nil {
		t.Fatal(err)
	}
	if list, err := client.WorktreeList(ctx, "/repo"); err != nil || len(list) != 1 || list[0].Path != "/repo" {
		t.Fatalf("worktree parse: %#v %v", list, err)
	}
	if status, err := client.Status(ctx, "/repo"); err != nil || !status.Dirty || len(status.Entries) != 1 {
		t.Fatalf("status parse: %#v %v", status, err)
	}
	if ref, err := client.SymbolicRef(ctx, "/repo"); err != nil || ref != "refs/heads/main" {
		t.Fatalf("symbolic ref: %q %v", ref, err)
	}
	if refs, err := client.ForEachRef(ctx, "/repo", "refs/heads/x"); err != nil || len(refs) != 1 || refs[0].Upstream != "refs/remotes/origin/main" {
		t.Fatalf("refs: %#v %v", refs, err)
	}
	if ok, err := client.MergeBaseIsAncestor(ctx, "/repo", "a", "b"); err != nil || !ok {
		t.Fatalf("ancestor: %v %v", ok, err)
	}
	if value, err := client.ConfigGet(ctx, "/repo", "extensions.worktreeConfig"); err != nil || value != "true" {
		t.Fatalf("config: %q %v", value, err)
	}

	want := [][]string{
		{"rev-parse", "--verify", "HEAD"},
		{"worktree", "list", "--porcelain", "-z"},
		{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching"},
		{"symbolic-ref", "-q", "HEAD"},
		{"for-each-ref", "--format=%(refname)%09%(objectname)%09%(upstream)", "refs/heads/x"},
		{"merge-base", "--is-ancestor", "a", "b"},
		{"config", "--get", "extensions.worktreeConfig"},
	}
	for i := range want {
		if !reflect.DeepEqual(runner.commands[i].Args, want[i]) {
			t.Fatalf("argv[%d] = %#v, want %#v", i, runner.commands[i].Args, want[i])
		}
	}
}

func TestParseRefsPreservesEmptyUpstreamAndRejectsMalformedRecords(t *testing.T) {
	oid := strings.Repeat("a", 40)
	refs, err := parseRefs([]byte("refs/heads/x\t" + oid + "\t\n"))
	if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/x" || refs[0].ObjectName != oid || refs[0].Upstream != "" {
		t.Fatalf("empty upstream record rejected: refs=%#v err=%v", refs, err)
	}
	for _, malformed := range [][]byte{
		[]byte("refs/heads/x\t" + oid + "\n"),
		[]byte("refs/heads/x\t" + oid + "\t\textra\n"),
		[]byte("refs/heads/x\t" + oid + "\t\n\n"),
		[]byte("refs/heads/x\t" + oid + "\trefs/remotes/origin/x\x00\n"),
		[]byte("refs/heads/x\t" + oid + "\trefs/remotes/origin/x\rjunk\n"),
	} {
		if refs, err := parseRefs(malformed); err == nil || refs != nil {
			t.Fatalf("malformed ref record accepted: %q %#v", malformed, refs)
		}
	}
}

func TestGitReaderHandlesPredicateExitOne(t *testing.T) {
	runner := &recordingGitRunner{errors: []error{&GitCommandError{ExitCode: 1}, &GitCommandError{ExitCode: 1}}}
	client := newRecordingGitClient(runner)
	ignored, err := client.CheckIgnore(context.Background(), "/repo", ".xagent/worktrees")
	if err != nil || ignored {
		t.Fatalf("check-ignore exit 1 应表示 false：%v %v", ignored, err)
	}
	ancestor, err := client.MergeBaseIsAncestor(context.Background(), "/repo", "a", "b")
	if err != nil || ancestor {
		t.Fatalf("merge-base exit 1 应表示 false：%v %v", ancestor, err)
	}
}

func TestWorktreeConfigGetUsesExactArgv(t *testing.T) {
	runner := &recordingGitRunner{outputs: []GitOutput{{Stdout: []byte(".githooks\n")}}}
	client := newRecordingGitClient(runner)
	value, err := client.WorktreeConfigGet(context.Background(), "/repo/worktree", "core.hooksPath")
	if err != nil || value != ".githooks" {
		t.Fatalf("worktree config value = %q, %v", value, err)
	}
	want := []string{"config", "--worktree", "--get", "core.hooksPath"}
	if len(runner.commands) != 1 || !reflect.DeepEqual(runner.commands[0].Args, want) || runner.commands[0].CWD != "/repo/worktree" {
		t.Fatalf("worktree config argv = %#v, want %#v", runner.commands, want)
	}
}

func TestWorktreeConfigGetMapsExitOneToNotFound(t *testing.T) {
	runner := &recordingGitRunner{errors: []error{&GitCommandError{ExitCode: 1}}}
	client := newRecordingGitClient(runner)
	if _, err := client.WorktreeConfigGet(context.Background(), "/repo/worktree", "core.hooksPath"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset worktree config error = %v, want ErrNotFound", err)
	}
}

func TestWorktreeConfigGetRejectsUntrustedShapeBeforeGit(t *testing.T) {
	runner := &recordingGitRunner{}
	client := newRecordingGitClient(runner)
	if _, err := client.WorktreeConfigGet(context.Background(), "/repo/worktree", "user.email"); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("unapproved worktree key error = %v", err)
	}
	if _, err := client.WorktreeConfigGet(context.Background(), "relative/worktree", "core.hooksPath"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("relative worktree cwd error = %v", err)
	}
	if _, err := client.WorktreeConfigGet(context.Background(), "/repo/worktree/../other", "core.hooksPath"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("non-canonical worktree cwd error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatal("invalid worktree config reads must not execute Git")
	}
}

func TestGitMutatorUsesExactSafeArgv(t *testing.T) {
	runner := &recordingGitRunner{}
	client := newRecordingGitClient(runner)
	ctx := context.Background()
	request := AddWorktreeRequest{RepositoryRoot: "/repo", Directory: "/repo/.xagent/worktrees/tasks/01/id", Branch: "xagent/worktree/id", BaseOID: strings.Repeat("a", 40)}
	if err := client.AddWorktree(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := client.EnableWorktreeConfig(ctx, request.RepositoryRoot); err != nil {
		t.Fatal(err)
	}
	if err := client.SetWorktreeConfig(ctx, request.Directory, "core.hooksPath", ".githooks"); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveWorktree(ctx, request.RepositoryRoot, request.Directory); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRefCAS(ctx, request.RepositoryRoot, "refs/heads/xagent/worktree/id", request.BaseOID); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"worktree", "add", "-b", request.Branch, request.Directory, request.BaseOID},
		{"config", "extensions.worktreeConfig", "true"},
		{"config", "--worktree", "core.hooksPath", ".githooks"},
		{"worktree", "remove", request.Directory},
		{"update-ref", "-d", "refs/heads/xagent/worktree/id", request.BaseOID},
	}
	for i := range want {
		if !reflect.DeepEqual(runner.commands[i].Args, want[i]) {
			t.Fatalf("argv[%d] = %#v, want %#v", i, runner.commands[i].Args, want[i])
		}
	}
}

func TestGitMutatorRejectsInvalidRefAndOIDBeforeExecution(t *testing.T) {
	runner := &recordingGitRunner{}
	client := newRecordingGitClient(runner)
	if err := client.EnableWorktreeConfig(context.Background(), "relative/repo"); err == nil {
		t.Fatal("仓库级配置必须绑定绝对 cwd")
	}
	if err := client.SetWorktreeConfig(context.Background(), "/repo", "user.email", "attacker@example.test"); err == nil {
		t.Fatal("Worktree config 只能修改明确允许的 key")
	}
	if err := client.DeleteRefCAS(context.Background(), "/repo", "refs/remotes/origin/main", strings.Repeat("a", 40)); err == nil {
		t.Fatal("不得删除远端跟踪引用")
	}
	if err := client.DeleteRefCAS(context.Background(), "/repo", "refs/heads/x", "not-an-oid"); err == nil {
		t.Fatal("必须校验 expected OID")
	}
	if len(runner.commands) != 0 {
		t.Fatal("非法请求不得执行 Git")
	}
}

func TestGitMutatorRestoresConfigWithExactSafeArgv(t *testing.T) {
	runner := &recordingGitRunner{}
	client := newRecordingGitClient(runner)
	ctx := context.Background()

	if err := client.RestoreWorktreeConfigExtension(ctx, "/repo", "false", true); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreWorktreeHooksPath(ctx, "/repo/worktree", ".previous-hooks", true); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreWorktreeConfigExtension(ctx, "/repo", "ignored\x00value", false); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreWorktreeHooksPath(ctx, "/repo/worktree", "ignored\x00value", false); err != nil {
		t.Fatal(err)
	}

	want := [][]string{
		{"config", "extensions.worktreeConfig", "false"},
		{"config", "--worktree", "core.hooksPath", ".previous-hooks"},
		{"config", "--unset", "extensions.worktreeConfig"},
		{"config", "--worktree", "--unset", "core.hooksPath"},
	}
	if len(runner.commands) != len(want) {
		t.Fatalf("commands = %d, want %d", len(runner.commands), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(runner.commands[i].Args, want[i]) {
			t.Fatalf("argv[%d] = %#v, want %#v", i, runner.commands[i].Args, want[i])
		}
	}
}

func TestGitMutatorRestoreUnsetExitFiveIsIdempotent(t *testing.T) {
	runner := &recordingGitRunner{errors: []error{
		&GitCommandError{ExitCode: 5},
		&GitCommandError{ExitCode: 5},
	}}
	client := newRecordingGitClient(runner)
	if err := client.RestoreWorktreeConfigExtension(context.Background(), "/repo", "", false); err != nil {
		t.Fatalf("extension unset exit 5 must be idempotent: %v", err)
	}
	if err := client.RestoreWorktreeHooksPath(context.Background(), "/repo/worktree", "", false); err != nil {
		t.Fatalf("hooks unset exit 5 must be idempotent: %v", err)
	}
}

func TestGitMutatorRestoreRejectsUntrustedPreviousValues(t *testing.T) {
	runner := &recordingGitRunner{}
	client := newRecordingGitClient(runner)
	ctx := context.Background()

	for _, value := range []string{"", "TRUE", "yes", "1", "false\n"} {
		if err := client.RestoreWorktreeConfigExtension(ctx, "/repo", value, true); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("non-canonical Git bool %q was not rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "bad\x00path", "bad\rpath", "bad\npath", strings.Repeat("x", maxMetadataPath+1)} {
		if err := client.RestoreWorktreeHooksPath(ctx, "/repo/worktree", value, true); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("unsafe hooksPath %q was not rejected: %v", value, err)
		}
	}
	if err := client.RestoreWorktreeConfigExtension(ctx, "relative/repo", "true", true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("relative repository root was not rejected: %v", err)
	}
	if err := client.RestoreWorktreeHooksPath(ctx, "relative/worktree", ".hooks", true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("relative worktree root was not rejected: %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatal("invalid restore requests must not execute Git")
	}
}
