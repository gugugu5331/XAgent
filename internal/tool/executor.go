package tool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"xagent/internal/budget"
	"xagent/internal/permission"
	"xagent/internal/safefs"
)

type Executor struct {
	Registry       *Registry
	ProjectRoot    string
	Timeout        time.Duration
	MaxOutputBytes int
	ReadMaxBytes   int64
	ReadMaxLines   int64
	ScanMaxBytes   int64
	ScanMaxFiles   int64
	ScanMaxDirs    int64
	ScanMaxLines   int64
	TicketVerifier permission.TicketVerifier

	rootOnce sync.Once
	root     *safefs.Root
	rootErr  error

	extraRootsMu sync.Mutex
	extraRoots   map[string]*safefs.Root
}

func NewExecutor(registry *Registry, projectRoot string, timeout time.Duration, maxOutputBytes int) *Executor {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = 32 * 1024
	}
	return &Executor{
		Registry:       registry,
		ProjectRoot:    projectRoot,
		Timeout:        timeout,
		MaxOutputBytes: maxOutputBytes,
		ReadMaxBytes:   defaultBudgetValue(budget.FilesReadMaxBytes),
		ReadMaxLines:   defaultBudgetValue(budget.FilesScanMaxLines),
		ScanMaxBytes:   defaultBudgetValue(budget.FilesScanMaxBytes),
		ScanMaxFiles:   defaultBudgetValue(budget.FilesScanMaxFiles),
		ScanMaxDirs:    defaultBudgetValue(budget.FilesScanMaxDirectories),
		ScanMaxLines:   defaultBudgetValue(budget.FilesScanMaxLines),
	}
}

func (e *Executor) NeedsConfirmation(call Call) bool {
	tool, ok := e.Registry.Get(call.Name)
	return ok && tool.Risk() == RiskDangerous
}

func (e *Executor) ExecuteAuthorized(ctx context.Context, call Call, ticket permission.ExecutionTicket) Result {
	validated, err := e.PrepareCall(ctx, call)
	if err != nil {
		return e.validationFailure(call, err)
	}
	return e.ExecuteValidatedAuthorized(ctx, validated, ticket)
}

// ExecuteValidatedAuthorized verifies and consumes a single-use execution
// ticket before executing the already parsed call.
func (e *Executor) ExecuteValidatedAuthorized(ctx context.Context, validated ValidatedCall, ticket permission.ExecutionTicket) Result {
	call := validated.Call
	if ctx == nil || ctx.Err() != nil {
		return Result{}
	}
	if validated.Tool == nil || validated.Tool.Name() != call.Name || validated.Arguments == nil || len(validated.CanonicalArguments()) == 0 {
		return e.validationFailure(call, fmt.Errorf("validated call is inconsistent"))
	}

	// Re-parse through the immutable Registry snapshot and re-bind live
	// resources at the actual start boundary. The freshly prepared call is also
	// the only value passed to the tool, so callers cannot mutate an authorized
	// Arguments map into a different execution.
	current, err := e.PrepareCall(ctx, call)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool resources are unavailable for execution.", Recoverable: true})
	}
	identity, err := e.CallIdentity(current)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Execution ticket verification is unavailable.", Recoverable: true})
	}
	if ctx.Err() != nil {
		return Result{}
	}
	if e.TicketVerifier == nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Execution ticket verification is unavailable.", Recoverable: true})
	}
	if err := e.TicketVerifier.VerifyAndConsume(ticket, call.ID, identity); err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonRuleDeny, ModelMessage: "Execution ticket does not match this tool call.", Recoverable: true})
	}
	if ctx.Err() != nil {
		return Result{}
	}
	return e.executeValidated(ctx, current)
}

// PrepareCall validates a call and freezes its current filesystem bindings.
// Authorizers must issue a ticket for CallIdentity of this exact value.
func (e *Executor) PrepareCall(ctx context.Context, call Call) (ValidatedCall, error) {
	if e == nil || e.Registry == nil {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	if !isFileToolName(call.Name) {
		return e.Registry.ValidateCall(call)
	}
	rootPath, err := e.bindingRootPath(ctx, call)
	if err != nil {
		return ValidatedCall{}, err
	}
	if call.Name == "Read" || call.Name == "Grep" {
		rootPath, err = canonicalReadRoot(rootPath)
		if err != nil {
			return ValidatedCall{}, errors.New("executor read root is unavailable")
		}
	}
	root, err := e.openRootAt(rootPath)
	if err != nil {
		return ValidatedCall{}, err
	}
	return e.Registry.ValidateCallWithContext(call, ValidationContext{Root: root, ProjectRoot: rootPath})
}

// CallIdentity builds the authorization identity from execution semantics,
// including the actual Bash shell/environment and frozen file/MCP bindings.
func (e *Executor) CallIdentity(validated ValidatedCall) (permission.CallIdentity, error) {
	if validated.Call.Name != "Bash" {
		return permission.NewCallIdentity(validated.IdentityInput())
	}
	root, err := e.openRoot()
	if err != nil {
		return permission.CallIdentity{}, err
	}
	command, ok := validated.Arguments["command"].(string)
	if !ok {
		return permission.CallIdentity{}, errors.New("bash command is invalid")
	}
	return permission.NewBashIdentity(permission.BashIdentityInput{
		Shell:             executionShell(),
		WorkingDirectory:  root.Identity(),
		RawCommand:        []byte(command),
		EnvironmentDigest: executionEnvironmentDigest(os.Environ()),
	})
}

func (e *Executor) openRoot() (*safefs.Root, error) {
	if e == nil {
		return nil, errors.New("executor is unavailable")
	}
	e.rootOnce.Do(func() {
		opened, err := safefs.Bootstrap(e.ProjectRoot, safefs.Policy{})
		if err != nil {
			e.rootErr = errors.New("executor root is unavailable")
			return
		}
		e.root = opened.Root
	})
	if e.rootErr != nil || e.root == nil {
		return nil, errors.New("executor root is unavailable")
	}
	return e.root, nil
}

func (e *Executor) openRootAt(rootPath string) (*safefs.Root, error) {
	if rootPath == e.ProjectRoot {
		return e.openRoot()
	}
	e.extraRootsMu.Lock()
	defer e.extraRootsMu.Unlock()
	if root := e.extraRoots[rootPath]; root != nil {
		return root, nil
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		return nil, errors.New("executor read root is unavailable")
	}
	if e.extraRoots == nil {
		e.extraRoots = make(map[string]*safefs.Root)
	}
	e.extraRoots[rootPath] = opened.Root
	return opened.Root, nil
}

func (e *Executor) bindingRootPath(ctx context.Context, call Call) (string, error) {
	if call.Name != "Read" && call.Name != "Grep" {
		return e.ProjectRoot, nil
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call.ArgumentsJSON), &arguments); err != nil {
		return e.ProjectRoot, nil
	}
	path, _ := arguments["path"].(string)
	if call.Name == "Grep" && path == "" {
		path = "."
	}
	scope, err := effectiveReadScope(ctx, e.ProjectRoot)
	if err != nil {
		return "", errors.New("executor read scope is unavailable")
	}
	target, err := resolveReadPath(scope, path)
	if err != nil {
		return "", errors.New("file tool path is outside the opened roots")
	}
	return target.root, nil
}

func executionShell() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "/bin/sh"
}

func executionEnvironmentDigest(environment []string) [32]byte {
	values := append([]string(nil), environment...)
	sort.Strings(values)
	hash := sha256.New()
	var length [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func (e *Executor) Execute(ctx context.Context, call Call) Result {
	validated, err := e.Registry.ValidateCall(call)
	if err != nil {
		return e.validationFailure(call, err)
	}
	return e.executeValidated(ctx, validated)
}

func (e *Executor) executeValidated(ctx context.Context, validated ValidatedCall) Result {
	call := validated.Call
	execCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	if call.Name == "Read" {
		execCtx = withReadExecution(execCtx, readExecution{
			root:        validated.executionRoot,
			rootPath:    validated.executionRootPath,
			fileBytes:   e.ReadMaxBytes,
			lines:       e.ReadMaxLines,
			outputBytes: int64(e.MaxOutputBytes),
		})
	} else if call.Name == "Grep" || call.Name == "Glob" {
		execCtx = withReadExecution(execCtx, readExecution{
			root:            validated.executionRoot,
			rootPath:        validated.executionRootPath,
			outputBytes:     int64(e.MaxOutputBytes),
			scanBytes:       e.ScanMaxBytes,
			scanFiles:       e.ScanMaxFiles,
			scanDirectories: e.ScanMaxDirs,
			scanLines:       e.ScanMaxLines,
		})
	}

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- validated.Tool.Execute(execCtx, Input{Name: call.Name, CallID: call.ID, RawArguments: call.ArgumentsJSON, Arguments: validated.Arguments})
	}()

	select {
	case <-execCtx.Done():
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusTimeout,
			Summary: "工具执行超时",
			Error:   &Error{Code: ErrTimeout, Message: "工具执行超过时间限制", Recoverable: true},
			Data:    map[string]any{"timed_out": true},
		}
	case result := <-resultCh:
		return e.truncate(result)
	}
}

func (e *Executor) validationFailure(call Call, err error) Result {
	if errors.Is(err, errToolNotRegistered) {
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusError,
			Summary: fmt.Sprintf("未知工具: %s", call.Name),
			Error:   &Error{Code: ErrToolNotFound, Message: fmt.Sprintf("工具 %q 未注册", call.Name), Recoverable: true},
		}
	}
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusError,
		Summary: "工具参数不是有效 JSON",
		Error:   &Error{Code: ErrInvalidArguments, Message: err.Error(), Recoverable: true},
	}
}

func (e *Executor) permissionDenied(call Call, decision permission.Decision) Result {
	message := permission.DeniedModelMessage(decision)
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusDenied,
		Summary: "Permission denied before executing " + call.Name,
		Content: message,
		Data:    permission.DeniedResultData(decision),
		Error:   &Error{Code: ErrPermissionDenied, Message: message, Recoverable: true},
	}
}

func (e *Executor) Denied(call Call) Result {
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusDenied,
		Summary: "用户拒绝执行工具",
		Content: "用户拒绝执行该工具调用。",
		Error:   &Error{Code: ErrPermissionDenied, Message: "用户拒绝执行该工具调用", Recoverable: true},
	}
}

func (e *Executor) truncate(result Result) Result {
	outputLimit := e.MaxOutputBytes
	if outputLimit <= 0 {
		outputLimit = 32 * 1024
	}
	fieldLimit := outputLimit
	if fieldLimit < len("（输出已截断）") {
		fieldLimit = len("（输出已截断）")
	}
	originalSummary := result.Summary
	result.Summary, _ = truncateString(result.Summary, fieldLimit)
	if result.Error != nil {
		cloned := *result.Error
		cloned.Message, result.Truncated = truncateAndMark(cloned.Message, fieldLimit, result.Truncated)
		result.Error = &cloned
	}
	content, truncated := truncateString(result.Content, outputLimit)
	result.Content = content
	if truncated {
		result.Truncated = true
	}
	dataLimit := outputLimit / 2
	if dataLimit < 1 {
		dataLimit = 1
	}
	for _, key := range []string{"stdout", "stderr", "content"} {
		if value, ok := result.Data[key].(string); ok {
			truncatedValue, wasTruncated := truncateString(value, dataLimit)
			result.Data[key] = truncatedValue
			if wasTruncated {
				result.Truncated = true
			}
		}
	}
	if len(originalSummary) > fieldLimit {
		result.Truncated = true
	}
	if result.Truncated && !strings.Contains(result.Summary, "截断") {
		result.Summary = truncatedSummary(originalSummary, fieldLimit)
	}
	return result
}

func truncateAndMark(value string, maxBytes int, alreadyTruncated bool) (string, bool) {
	value, truncated := truncateString(value, maxBytes)
	return value, alreadyTruncated || truncated
}

func truncatedSummary(value string, maxBytes int) string {
	const suffix = "（输出已截断）"
	if maxBytes < len(suffix) {
		maxBytes = len(suffix)
	}
	prefix, _ := utf8Prefix(value, maxBytes-len(suffix))
	return prefix + suffix
}

func truncateString(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	const marker = "\n...[truncated]"
	if maxBytes <= len(marker) {
		return marker[:maxBytes], true
	}
	prefix, _ := utf8Prefix(value, maxBytes-len(marker))
	return prefix + marker, true
}

func utf8Prefix(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", value != ""
	}
	if len(value) <= maxBytes {
		return value, false
	}
	prefix := value[:maxBytes]
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix, true
}
