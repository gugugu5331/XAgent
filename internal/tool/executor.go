package tool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

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
	resultFactory  *ResultFactory
	provenance     *executorProvenance

	rootOnce                sync.Once
	root                    *safefs.Root
	rootErr                 error
	ordinaryWriteCapability safefs.Capability

	extraRootsMu sync.Mutex
	extraRoots   map[string]*safefs.Root
}

// executorProvenance is an unforgeable, process-local seal. Validated calls
// may cross the authorized execution boundary only through the exact Executor
// that froze their registry, root, and resource bindings.
type executorProvenance struct{ seal byte }

// NewExecutor is the legacy execution adapter retained until T4.29a. Results
// produced through it are compatibility values and must not be projected or
// persisted by the safe candidate path.
func NewExecutor(registry *Registry, projectRoot string, timeout time.Duration, maxOutputBytes int) *Executor {
	return newExecutor(registry, projectRoot, timeout, maxOutputBytes, nil)
}

// NewExecutorWithResultFactory constructs the safe candidate executor. The
// injected factory is shared with every registered safe-candidate producer.
func NewExecutorWithResultFactory(registry *Registry, projectRoot string, timeout time.Duration, maxOutputBytes int, factory *ResultFactory) (*Executor, error) {
	if registry == nil || !registry.safeCandidate || factory == nil {
		return nil, errors.New("safe tool executor dependencies are unavailable")
	}
	return newExecutor(registry, projectRoot, timeout, maxOutputBytes, factory), nil
}

func newExecutor(registry *Registry, projectRoot string, timeout time.Duration, maxOutputBytes int, factory *ResultFactory) *Executor {
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
		resultFactory:  factory,
		provenance:     &executorProvenance{},
	}
}

// NewExecutorWithWriteAccess constructs an Executor whose Write and Edit
// tools receive only the ordinary capability retained by the assembly root.
// Root and capability remain independently validated by safefs on every
// atomic publication.
func NewExecutorWithWriteAccess(
	registry *Registry,
	projectRoot string,
	timeout time.Duration,
	maxOutputBytes int,
	root *safefs.Root,
	capability safefs.Capability,
) *Executor {
	executor := NewExecutor(registry, projectRoot, timeout, maxOutputBytes)
	executor.rootOnce.Do(func() {
		executor.root = root
	})
	executor.ordinaryWriteCapability = capability
	return executor
}

// NewExecutorWithWriteAccessAndResultFactory is the write-capable safe
// candidate constructor. It does not construct a factory or any capture.
func NewExecutorWithWriteAccessAndResultFactory(
	registry *Registry,
	projectRoot string,
	timeout time.Duration,
	maxOutputBytes int,
	root *safefs.Root,
	capability safefs.Capability,
	factory *ResultFactory,
) (*Executor, error) {
	executor, err := NewExecutorWithResultFactory(registry, projectRoot, timeout, maxOutputBytes, factory)
	if err != nil {
		return nil, err
	}
	executor.rootOnce.Do(func() {
		executor.root = root
	})
	executor.ordinaryWriteCapability = capability
	return executor, nil
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
	if e == nil {
		return Result{}
	}
	return e.ExecuteValidatedAuthorizedWithVerifier(ctx, validated, ticket, e.TicketVerifier, nil)
}

func (e *Executor) ownsValidatedCall(validated ValidatedCall) bool {
	return e != nil && e.provenance != nil && validated.provenance == e.provenance
}

// ExecuteValidatedAuthorizedWithVerifier verifies and consumes a single-use
// execution ticket at the actual start boundary. Task executors inject their
// scope-bound verifier here so the legacy Executor verifier is never consulted
// and the ticket cannot be consumed twice. Authorized read caching starts only
// after the verifier succeeds.
func (e *Executor) ExecuteValidatedAuthorizedWithVerifier(
	ctx context.Context,
	validated ValidatedCall,
	ticket permission.ExecutionTicket,
	verifier permission.TicketVerifier,
	cache AuthorizedResultCache,
) Result {
	call := validated.Call
	if e == nil || ctx == nil || ctx.Err() != nil {
		return Result{}
	}
	if !e.ownsValidatedCall(validated) ||
		validated.Tool == nil || validated.Tool.Name() != call.Name || validated.executor == nil || validated.executor.Name() != call.Name || validated.Arguments == nil || len(validated.CanonicalArguments()) == 0 {
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
	if verifier == nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Execution ticket verification is unavailable.", Recoverable: true})
	}
	if err := verifier.VerifyAndConsume(ticket, call.ID, identity); err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonRuleDeny, ModelMessage: "Execution ticket does not match this tool call.", Recoverable: true})
	}
	if ctx.Err() != nil {
		return Result{}
	}

	cacheable := cache != nil && isCacheableReadTool(call.Name)
	lookupFailed := false
	if cacheable {
		cached, hit, lookupErr := cache.Lookup(ctx, current)
		if ctx.Err() != nil {
			return Result{}
		}
		if lookupErr == nil && hit && validAuthorizedCacheHit(cached, current.Call) {
			return cached
		}
		lookupFailed = lookupErr != nil || hit
	}

	result := e.executeValidated(ctx, current)
	storeFailed := false
	if cacheable && ctx.Err() == nil && result.CallID != "" {
		storeFailed = cache.Store(ctx, current, result) != nil
	}
	return withReadCacheDiagnostics(result, lookupFailed, storeFailed)
}

func validAuthorizedCacheHit(result Result, call Call) bool {
	return result.CallID == call.ID &&
		result.Name == call.Name &&
		result.Status.valid() &&
		result.ExecutionState().CanProduceResult() &&
		result.ModelContent().Text() != "" &&
		result.PersistedContent().Text() != ""
}

func withReadCacheDiagnostics(result Result, lookupFailed, storeFailed bool) Result {
	if !lookupFailed && !storeFailed {
		return result
	}
	data := make(map[string]any, len(result.Data)+2)
	for key, value := range result.Data {
		data[key] = value
	}
	if lookupFailed {
		data["read_cache_lookup"] = "error"
	}
	if storeFailed {
		data["read_cache_store"] = "error"
	}
	result.Data = data
	return result
}

// PrepareCall validates a call and freezes its current filesystem bindings.
// Authorizers must issue a ticket for CallIdentity of this exact value.
func (e *Executor) PrepareCall(ctx context.Context, call Call) (ValidatedCall, error) {
	if e == nil || e.Registry == nil {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	if !isFileToolName(call.Name) {
		validated, err := e.Registry.ValidateCall(call)
		if err != nil {
			return ValidatedCall{}, err
		}
		validated.provenance = e.provenance
		return validated, nil
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
	validated, err := e.Registry.ValidateCallWithContext(call, ValidationContext{Root: root, ProjectRoot: rootPath})
	if err != nil {
		return ValidatedCall{}, err
	}
	validated.provenance = e.provenance
	return validated, nil
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
	selection, err := selectShell(command)
	if err != nil {
		return permission.CallIdentity{}, errors.New("bash shell is unavailable")
	}
	return permission.NewBashIdentity(permission.BashIdentityInput{
		Shell:             selection.identity,
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
		return "", fmt.Errorf("%s: executor read scope is unavailable", ErrPathOutsideProject)
	}
	target, err := resolveReadPath(scope, path)
	if err != nil {
		return "", fmt.Errorf("%s: file tool path is outside the opened roots", ErrPathOutsideProject)
	}
	return target.root, nil
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
	} else if call.Name == "Write" || call.Name == "Edit" {
		execCtx = withWriteExecution(execCtx, writeExecution{
			root:       validated.executionRoot,
			rootPath:   validated.executionRootPath,
			capability: e.ordinaryWriteCapability,
		})
	}

	input := Input{Name: call.Name, CallID: call.ID, RawArguments: call.ArgumentsJSON, Arguments: validated.Arguments}
	execute := func() Result {
		return validated.executor.Execute(execCtx, input)
	}

	if e.resultFactory != nil {
		// Candidate producers own their cancellation and Capture terminal state.
		// Execute synchronously so no second synthetic timeout can race a later
		// producer Commit or leave an unreferenced artifact behind.
		return execute()
	}

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- execute()
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
		return result
	}
}

func (e *Executor) validationFailure(call Call, err error) Result {
	if e != nil && e.resultFactory != nil {
		input := ResultFactoryInput{
			CallID: call.ID,
			Name:   call.Name,
			State:  Rejected,
			Status: StatusError,
		}
		switch {
		case errors.Is(err, errToolNotRegistered):
			input.Summary = "未知工具"
			input.Error = &Error{Code: ErrToolNotFound, Message: "工具未注册", Recoverable: true}
		case errorCode(err) == ErrPathOutsideProject:
			input.Summary = "工具路径位于已打开根目录外"
			input.Error = &Error{Code: ErrPathOutsideProject, Message: "工具路径位于已打开根目录外", Recoverable: true}
		default:
			input.Summary = "工具参数不是有效 JSON"
			input.Error = &Error{Code: ErrInvalidArguments, Message: "工具参数无效", Recoverable: true}
		}
		return buildSyntheticResult(e.resultFactory, input)
	}
	if errors.Is(err, errToolNotRegistered) {
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusError,
			Summary: fmt.Sprintf("未知工具: %s", call.Name),
			Error:   &Error{Code: ErrToolNotFound, Message: fmt.Sprintf("工具 %q 未注册", call.Name), Recoverable: true},
		}
	}
	if errorCode(err) == ErrPathOutsideProject {
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusError,
			Summary: "工具路径位于已打开根目录外",
			Error:   &Error{Code: ErrPathOutsideProject, Message: "工具路径位于已打开根目录外", Recoverable: true},
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
	if e != nil && e.resultFactory != nil {
		return buildSyntheticResult(e.resultFactory, ResultFactoryInput{
			CallID:  call.ID,
			Name:    call.Name,
			State:   Rejected,
			Status:  StatusDenied,
			Summary: "Permission denied before executing tool",
			Preview: message,
			Error:   &Error{Code: ErrPermissionDenied, Message: message, Recoverable: true},
		})
	}
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
	if e != nil && e.resultFactory != nil {
		return buildSyntheticResult(e.resultFactory, ResultFactoryInput{
			CallID:  call.ID,
			Name:    call.Name,
			State:   Rejected,
			Status:  StatusDenied,
			Summary: "用户拒绝执行工具",
			Preview: "用户拒绝执行该工具调用。",
			Error:   &Error{Code: ErrPermissionDenied, Message: "用户拒绝执行该工具调用", Recoverable: true},
		})
	}
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusDenied,
		Summary: "用户拒绝执行工具",
		Content: "用户拒绝执行该工具调用。",
		Error:   &Error{Code: ErrPermissionDenied, Message: "用户拒绝执行该工具调用", Recoverable: true},
	}
}

func buildSyntheticResult(factory *ResultFactory, input ResultFactoryInput) Result {
	if factory == nil {
		return Result{}
	}
	input.Artifact = nil
	input.CapturedBytes = int64(len(input.Preview))
	input.Truncated = false
	input.TruncationReason = ""
	result, err := factory.Build(input)
	if err != nil {
		return Result{}
	}
	return result
}

func buildCapturedResult(factory *ResultFactory, input ResultFactoryInput, captured CaptureResult, finishErr error) Result {
	if finishErr != nil && captured == (CaptureResult{}) {
		input.Preview = ""
		input.Artifact = nil
		input.CapturedBytes = 0
		input.Truncated = false
		input.TruncationReason = ""
		if input.Status == StatusSuccess {
			input.Status = StatusError
		}
		if input.Error == nil {
			input.Error = &Error{Code: ErrCommandFailed, Message: "工具输出采集失败", Recoverable: true}
		}
		return buildSyntheticResult(factory, input)
	}
	input.Preview = captured.Preview
	input.Artifact = cloneArtifactRef(captured.Artifact)
	input.CapturedBytes = captured.CapturedBytes
	input.Truncated = captured.Truncated
	input.TruncationReason = string(captured.TruncationReason)
	if finishErr != nil && input.Status == StatusSuccess {
		input.Status = StatusError
		input.Error = &Error{Code: ErrCommandFailed, Message: "工具输出未完整采集", Recoverable: true}
	}
	if factory == nil {
		return Result{}
	}
	result, err := factory.Build(input)
	if err != nil {
		return Result{}
	}
	return result
}
