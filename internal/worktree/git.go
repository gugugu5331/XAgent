package worktree

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultGitOutputBytes = 1 << 20

var (
	gitOIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	gitRefPattern = regexp.MustCompile(`^refs/[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	gitKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*(?:\.[A-Za-z][A-Za-z0-9-]*)+$`)
)

type GitCommand struct {
	Executable     string
	Args           []string
	CWD            string
	Timeout        time.Duration
	MaxOutputBytes int
}

func (c GitCommand) Clone() GitCommand {
	clone := c
	clone.Args = append([]string(nil), c.Args...)
	return clone
}

type GitOutput struct {
	Stdout []byte
	Stderr []byte
}

type GitCommandRunner interface {
	Run(context.Context, GitCommand) (GitOutput, error)
}

type GitCommandError struct {
	ExitCode int
	cause    error
}

func (e *GitCommandError) Error() string { return "git command failed" }
func (e *GitCommandError) Unwrap() error { return e.cause }

func NewGitCommand(executable, cwd string, timeout time.Duration, maxOutputBytes int, args ...string) (GitCommand, error) {
	if executable == "" || filepath.Base(executable) != "git" || !filepath.IsAbs(cwd) || timeout <= 0 || maxOutputBytes <= 0 {
		return GitCommand{}, ErrForbiddenGitCommand
	}
	clonedArgs := append([]string(nil), args...)
	if err := validateGitArgv(clonedArgs); err != nil {
		return GitCommand{}, err
	}
	return GitCommand{Executable: executable, Args: clonedArgs, CWD: filepath.Clean(cwd), Timeout: timeout, MaxOutputBytes: maxOutputBytes}, nil
}

type GitClient struct {
	executable     string
	runner         GitCommandRunner
	timeout        time.Duration
	maxOutputBytes int
}

func NewGitClient(executable string, runner GitCommandRunner, timeout time.Duration, maxOutputBytes int) *GitClient {
	if executable == "" {
		executable = "git"
	}
	if runner == nil {
		runner = execGitRunner{}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultGitOutputBytes
	}
	return &GitClient{executable: executable, runner: runner, timeout: timeout, maxOutputBytes: maxOutputBytes}
}

type GitReader interface {
	ResolveHEAD(context.Context, string) (string, error)
	WorktreeList(context.Context, string) ([]WorktreeInfo, error)
	Status(context.Context, string) (StatusSnapshot, error)
	SymbolicRef(context.Context, string) (string, error)
	ForEachRef(context.Context, string, string) ([]RefInfo, error)
	MergeBaseIsAncestor(context.Context, string, string, string) (bool, error)
	CheckIgnore(context.Context, string, string) (bool, error)
	ConfigGet(context.Context, string, string) (string, error)
	WorktreeConfigGet(context.Context, string, string) (string, error)
}

type GitMutator interface {
	AddWorktree(context.Context, AddWorktreeRequest) error
	EnableWorktreeConfig(context.Context, string) error
	SetWorktreeConfig(context.Context, string, string, string) error
	RestoreWorktreeConfigExtension(context.Context, string, string, bool) error
	RestoreWorktreeHooksPath(context.Context, string, string, bool) error
	RemoveWorktree(context.Context, string, string) error
	DeleteRefCAS(context.Context, string, string, string) error
}

type WorktreeInfo struct {
	Path       string
	HEAD       string
	Branch     string
	Bare       bool
	Detached   bool
	Locked     bool
	LockReason string
	Prunable   bool
}

type StatusEntry struct {
	Code string
	Path string
}

type StatusSnapshot struct {
	Dirty   bool
	Entries []StatusEntry
}

type RefInfo struct {
	Name       string
	ObjectName string
	Upstream   string
}

type AddWorktreeRequest struct {
	RepositoryRoot string
	Directory      string
	Branch         string
	BaseOID        string
}

func (c *GitClient) ResolveHEAD(ctx context.Context, cwd string) (string, error) {
	output, err := c.run(ctx, cwd, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(output.Stdout))
	if !validOID(oid) {
		return "", ErrInvalidMetadata
	}
	return oid, nil
}

func (c *GitClient) WorktreeList(ctx context.Context, cwd string) ([]WorktreeInfo, error) {
	output, err := c.run(ctx, cwd, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(output.Stdout)
}

func (c *GitClient) Status(ctx context.Context, cwd string) (StatusSnapshot, error) {
	output, err := c.run(ctx, cwd, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return StatusSnapshot{}, err
	}
	return parseStatus(output.Stdout), nil
}

func (c *GitClient) SymbolicRef(ctx context.Context, cwd string) (string, error) {
	output, err := c.run(ctx, cwd, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(string(output.Stdout))
	if !validRef(ref) {
		return "", ErrInvalidMetadata
	}
	return ref, nil
}

func (c *GitClient) ForEachRef(ctx context.Context, cwd, pattern string) ([]RefInfo, error) {
	if !validRefPattern(pattern) {
		return nil, ErrInvalidMetadata
	}
	output, err := c.run(ctx, cwd, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(upstream)", pattern)
	if err != nil {
		return nil, err
	}
	return parseRefs(output.Stdout)
}

func (c *GitClient) MergeBaseIsAncestor(ctx context.Context, cwd, ancestor, descendant string) (bool, error) {
	if !validRevision(ancestor) || !validRevision(descendant) {
		return false, ErrInvalidMetadata
	}
	_, err := c.run(ctx, cwd, "merge-base", "--is-ancestor", ancestor, descendant)
	if commandExitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

func (c *GitClient) CheckIgnore(ctx context.Context, cwd, path string) (bool, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return false, ErrUnsafePath
	}
	_, err := c.run(ctx, cwd, "check-ignore", "-q", "--", path)
	if commandExitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

func (c *GitClient) ConfigGet(ctx context.Context, cwd, key string) (string, error) {
	if !gitKeyPattern.MatchString(key) {
		return "", ErrInvalidMetadata
	}
	output, err := c.run(ctx, cwd, "config", "--get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output.Stdout)), nil
}

func (c *GitClient) WorktreeConfigGet(ctx context.Context, worktreeRoot, key string) (string, error) {
	if worktreeRoot == "" || !filepath.IsAbs(worktreeRoot) || filepath.Clean(worktreeRoot) != worktreeRoot {
		return "", ErrUnsafePath
	}
	if key != "core.hooksPath" {
		return "", ErrInvalidMetadata
	}
	output, err := c.run(ctx, worktreeRoot, "config", "--worktree", "--get", "core.hooksPath")
	if commandExitCode(err) == 1 {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output.Stdout)), nil
}

func (c *GitClient) AddWorktree(ctx context.Context, request AddWorktreeRequest) error {
	if !filepath.IsAbs(request.RepositoryRoot) || !filepath.IsAbs(request.Directory) || !validTemporaryBranch(request.Branch) || !validOID(request.BaseOID) {
		return ErrInvalidMetadata
	}
	_, err := c.run(ctx, request.RepositoryRoot, "worktree", "add", "-b", request.Branch, filepath.Clean(request.Directory), request.BaseOID)
	return err
}

// EnableWorktreeConfig 只启用 Git 官方的 worktreeConfig 扩展，不接受调用方提供 key/value。
func (c *GitClient) EnableWorktreeConfig(ctx context.Context, repositoryRoot string) error {
	if !filepath.IsAbs(repositoryRoot) {
		return ErrUnsafePath
	}
	_, err := c.run(ctx, repositoryRoot, "config", "extensions.worktreeConfig", "true")
	return err
}

func (c *GitClient) SetWorktreeConfig(ctx context.Context, cwd, key, value string) error {
	if key != "core.hooksPath" || !validHooksPathValue(value) {
		return ErrInvalidMetadata
	}
	_, err := c.run(ctx, cwd, "config", "--worktree", key, value)
	return err
}

func (c *GitClient) RestoreWorktreeConfigExtension(ctx context.Context, repositoryRoot, previousValue string, wasSet bool) error {
	if !filepath.IsAbs(repositoryRoot) {
		return ErrUnsafePath
	}
	if wasSet {
		if previousValue != "true" && previousValue != "false" {
			return ErrInvalidMetadata
		}
		_, err := c.run(ctx, repositoryRoot, "config", "extensions.worktreeConfig", previousValue)
		return err
	}
	_, err := c.run(ctx, repositoryRoot, "config", "--unset", "extensions.worktreeConfig")
	if commandExitCode(err) == 5 {
		return nil
	}
	return err
}

func (c *GitClient) RestoreWorktreeHooksPath(ctx context.Context, worktreeRoot, previousValue string, wasSet bool) error {
	if !filepath.IsAbs(worktreeRoot) {
		return ErrUnsafePath
	}
	if wasSet {
		if !validHooksPathValue(previousValue) {
			return ErrInvalidMetadata
		}
		_, err := c.run(ctx, worktreeRoot, "config", "--worktree", "core.hooksPath", previousValue)
		return err
	}
	_, err := c.run(ctx, worktreeRoot, "config", "--worktree", "--unset", "core.hooksPath")
	if commandExitCode(err) == 5 {
		return nil
	}
	return err
}

func validHooksPathValue(value string) bool {
	return value != "" && len(value) <= maxMetadataPath && !strings.ContainsAny(value, "\x00\r\n")
}

func (c *GitClient) RemoveWorktree(ctx context.Context, repositoryRoot, directory string) error {
	if !filepath.IsAbs(repositoryRoot) || !filepath.IsAbs(directory) {
		return ErrUnsafePath
	}
	_, err := c.run(ctx, repositoryRoot, "worktree", "remove", filepath.Clean(directory))
	return err
}

func (c *GitClient) DeleteRefCAS(ctx context.Context, repositoryRoot, ref, expectedOID string) error {
	if !filepath.IsAbs(repositoryRoot) || !strings.HasPrefix(ref, "refs/heads/xagent/worktree/") || !validRef(ref) || !validOID(expectedOID) {
		return ErrInvalidMetadata
	}
	_, err := c.run(ctx, repositoryRoot, "update-ref", "-d", ref, expectedOID)
	return err
}

func (c *GitClient) run(ctx context.Context, cwd string, args ...string) (GitOutput, error) {
	command, err := NewGitCommand(c.executable, cwd, c.timeout, c.maxOutputBytes, args...)
	if err != nil {
		return GitOutput{}, err
	}
	output, err := c.runner.Run(ctx, command)
	if len(output.Stdout) > c.maxOutputBytes || len(output.Stderr) > c.maxOutputBytes || len(output.Stdout)+len(output.Stderr) > c.maxOutputBytes {
		return GitOutput{}, ErrGitOutputLimit
	}
	return output, err
}

type execGitRunner struct{}

func (execGitRunner) Run(ctx context.Context, command GitCommand) (GitOutput, error) {
	commandCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	process := exec.CommandContext(commandCtx, command.Executable, command.Args...)
	process.Dir = command.CWD
	stdout := &boundedBuffer{limit: command.MaxOutputBytes}
	stderr := &boundedBuffer{limit: command.MaxOutputBytes}
	process.Stdout = stdout
	process.Stderr = stderr
	err := process.Run()
	if stdout.exceeded || stderr.exceeded {
		return GitOutput{}, ErrGitOutputLimit
	}
	output := GitOutput{Stdout: append([]byte(nil), stdout.Bytes()...), Stderr: append([]byte(nil), stderr.Bytes()...)}
	if err == nil {
		return output, nil
	}
	if commandCtx.Err() != nil {
		return GitOutput{}, commandCtx.Err()
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return output, &GitCommandError{ExitCode: exitCode, cause: err}
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.exceeded {
		return len(data), nil
	}
	remaining := b.limit - b.Len()
	if remaining < len(data) {
		b.exceeded = true
		if remaining > 0 {
			_, _ = b.Buffer.Write(data[:remaining])
		}
		return len(data), nil
	}
	return b.Buffer.Write(data)
}

func validateGitArgv(args []string) error {
	if len(args) == 0 {
		return ErrForbiddenGitCommand
	}
	for _, arg := range args {
		if arg == "" || strings.ContainsRune(arg, 0) || arg == "--force" || arg == "-f" || arg == "--hard" || arg == "-D" || arg == "--delete" {
			return ErrForbiddenGitCommand
		}
	}
	switch args[0] {
	case "rev-parse":
		if len(args) == 3 && args[1] == "--verify" && args[2] == "HEAD" {
			return nil
		}
	case "check-ignore":
		if len(args) == 4 && args[1] == "-q" && args[2] == "--" {
			return nil
		}
	case "status":
		if len(args) == 5 && args[1] == "--porcelain=v2" && args[2] == "-z" && args[3] == "--untracked-files=all" && args[4] == "--ignored=matching" {
			return nil
		}
	case "symbolic-ref":
		if len(args) == 3 && args[1] == "-q" && args[2] == "HEAD" {
			return nil
		}
	case "for-each-ref":
		if len(args) == 3 && args[1] == "--format=%(refname)%09%(objectname)%09%(upstream)" {
			return nil
		}
	case "merge-base":
		if len(args) == 4 && args[1] == "--is-ancestor" {
			return nil
		}
	case "config":
		if len(args) == 3 && args[1] == "--get" {
			return nil
		}
		if len(args) == 3 && args[1] == "extensions.worktreeConfig" && (args[2] == "true" || args[2] == "false") {
			return nil
		}
		if len(args) == 3 && args[1] == "--unset" && args[2] == "extensions.worktreeConfig" {
			return nil
		}
		if len(args) == 4 && args[1] == "--worktree" && args[2] == "core.hooksPath" {
			return nil
		}
		if len(args) == 4 && args[1] == "--worktree" && args[2] == "--get" && args[3] == "core.hooksPath" {
			return nil
		}
		if len(args) == 4 && args[1] == "--worktree" && args[2] == "--unset" && args[3] == "core.hooksPath" {
			return nil
		}
	case "update-ref":
		if len(args) == 4 && args[1] == "-d" && strings.HasPrefix(args[2], "refs/heads/xagent/worktree/") && validRef(args[2]) && validOID(args[3]) {
			return nil
		}
	case "worktree":
		if len(args) == 4 && args[1] == "list" && args[2] == "--porcelain" && args[3] == "-z" {
			return nil
		}
		if len(args) == 6 && args[1] == "add" && args[2] == "-b" && validTemporaryBranch(args[3]) && filepath.IsAbs(args[4]) && validOID(args[5]) {
			return nil
		}
		if len(args) == 3 && args[1] == "remove" && filepath.IsAbs(args[2]) {
			return nil
		}
	}
	return ErrForbiddenGitCommand
}

func parseWorktreeList(data []byte) ([]WorktreeInfo, error) {
	trimmed := bytes.Trim(data, "\x00\n")
	if len(trimmed) == 0 {
		return nil, nil
	}
	blocks := bytes.Split(trimmed, []byte{0, 0})
	result := make([]WorktreeInfo, 0, len(blocks))
	for _, block := range blocks {
		var info WorktreeInfo
		for _, raw := range bytes.Split(block, []byte{0}) {
			line := string(raw)
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				info.Path = value
			case "HEAD":
				info.HEAD = value
			case "branch":
				info.Branch = value
			case "bare":
				info.Bare = true
			case "detached":
				info.Detached = true
			case "locked":
				info.Locked, info.LockReason = true, value
			case "prunable":
				info.Prunable = true
			}
		}
		if !filepath.IsAbs(info.Path) || (info.HEAD != "" && !validOID(info.HEAD)) || (info.Branch != "" && !validRef(info.Branch)) {
			return nil, ErrInvalidMetadata
		}
		result = append(result, info)
	}
	return result, nil
}

func parseStatus(data []byte) StatusSnapshot {
	parts := bytes.Split(bytes.Trim(data, "\x00"), []byte{0})
	entries := make([]StatusEntry, 0, len(parts))
	for _, raw := range parts {
		if len(raw) == 0 {
			continue
		}
		line := string(raw)
		code := line
		if index := strings.IndexByte(line, ' '); index >= 0 {
			code = line[:index]
		}
		path := line
		if index := strings.LastIndexByte(line, ' '); index >= 0 {
			path = line[index+1:]
		}
		entries = append(entries, StatusEntry{Code: code, Path: path})
	}
	return StatusSnapshot{Dirty: len(entries) != 0, Entries: entries}
}

func parseRefs(data []byte) ([]RefInfo, error) {
	if len(data) == 0 {
		return nil, nil
	}
	text := string(data)
	if strings.ContainsRune(text, 0) {
		return nil, ErrInvalidMetadata
	}
	if strings.HasSuffix(text, "\n") {
		text = strings.TrimSuffix(text, "\n")
		text = strings.TrimSuffix(text, "\r")
	}
	if text == "" || strings.ContainsRune(text, '\r') {
		return nil, ErrInvalidMetadata
	}
	lines := strings.Split(text, "\n")
	refs := make([]RefInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			return nil, ErrInvalidMetadata
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !validRef(fields[0]) || !validOID(fields[1]) || (fields[2] != "" && !validRef(fields[2])) {
			return nil, ErrInvalidMetadata
		}
		refs = append(refs, RefInfo{Name: fields[0], ObjectName: fields[1], Upstream: fields[2]})
	}
	return refs, nil
}

func commandExitCode(err error) int {
	var commandError *GitCommandError
	if errors.As(err, &commandError) {
		return commandError.ExitCode
	}
	return 0
}

func validOID(value string) bool { return gitOIDPattern.MatchString(value) }
func validRef(value string) bool {
	return gitRefPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".lock")
}
func validRefPattern(value string) bool {
	return validRef(value) || (strings.HasSuffix(value, "/*") && validRef(strings.TrimSuffix(value, "/*")))
}
func validTemporaryBranch(value string) bool {
	return strings.HasPrefix(value, "xagent/worktree/") && validRef("refs/heads/"+value)
}
func validRevision(value string) bool {
	return validOID(value) || validRef(value) || regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`).MatchString(value)
}

func (e *GitCommandError) ExitStatus() string { return strconv.Itoa(e.ExitCode) }

var _ GitReader = (*GitClient)(nil)
var _ GitMutator = (*GitClient)(nil)
