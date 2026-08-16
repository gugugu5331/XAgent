package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

type CommandRequest struct {
	Command     string
	ProjectRoot string
	Environment map[string]string
	EventJSON   []byte
}

type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

type CommandRunner interface {
	Run(context.Context, CommandRequest) (CommandResult, error)
}

type disabledWorkspaceCommandRunner struct{}

func (disabledWorkspaceCommandRunner) Run(context.Context, CommandRequest) (CommandResult, error) {
	return CommandResult{}, fmt.Errorf("workspace command runtime is unavailable")
}

// ShellCommandRunner owns only the narrow capabilities required to start one
// protected Hook command. Plans are created by the assembly-owned factory and
// are handed directly to the shared proctree Runner; Hook rules cannot create,
// serialize, replace, or weaken them.
type ShellCommandRunner struct {
	Limits           Limits
	Redactor         *redact.RuntimeRedactor
	Runner           proctree.Runner
	Plans            proctree.ProtectionPlanFactory
	WorkingDirectory *safefs.Root
	ProjectRoot      string
	WorkspaceBound   bool
	JoinGrace        time.Duration
}

const maxCommandCleanupGrace = 2 * time.Second

type limitBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	exceeded bool
}

func (b *limitBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exceeded {
		return 0, errors.New("output exceeds limit")
	}
	if len(b.data)+len(data) > b.limit {
		b.exceeded = true
		return 0, errors.New("output exceeds limit")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *limitBuffer) bytes() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...), b.exceeded
}

func (r *ShellCommandRunner) Run(ctx context.Context, request CommandRequest) (CommandResult, error) {
	if ctx == nil {
		return CommandResult{}, fmt.Errorf("command context is invalid")
	}
	if r == nil {
		return CommandResult{}, fmt.Errorf("protected command runtime is unavailable")
	}
	limits := normalizeLimits(r.Limits)
	if len(request.Command) > limits.CommandBytes {
		return CommandResult{}, fmt.Errorf("command exceeds limit")
	}
	if len(request.EventJSON) > limits.EventJSONBytes {
		return CommandResult{}, fmt.Errorf("event payload exceeds limit")
	}
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if !r.validRuntime(request.ProjectRoot) {
		return CommandResult{}, fmt.Errorf("protected command runtime is unavailable")
	}

	executable, args, err := selectCommandShell(request.Command)
	if err != nil {
		return CommandResult{}, fmt.Errorf("protected command shell is unavailable")
	}
	plan, err := r.Plans.Create(ctx)
	if err != nil {
		return CommandResult{}, fmt.Errorf("protected command plan is unavailable")
	}
	process, err := r.Runner.Start(ctx, proctree.Request{
		Executable: executable,
		Args:       append([]string(nil), args...),
		WorkingDir: r.WorkingDirectory,
		Env:        controlledEnvironment(r.ProjectRoot, request.Environment),
		Mode:       proctree.ProtectionRequired,
		Protection: plan,
	})
	if err != nil || process == nil {
		if process != nil {
			_ = closeCommandProcess(process, r.cleanupGrace())
		}
		return CommandResult{}, fmt.Errorf("command start failed")
	}

	stdout := &limitBuffer{limit: limits.CommandStdoutBytes}
	stderr := &limitBuffer{limit: limits.CommandStderrBytes}
	processResult, waitErr, ioErr, closeErr := runCommandProcess(ctx, process, request.EventJSON, stdout, stderr, r.cleanupGrace())
	if contextErr := ctx.Err(); contextErr != nil {
		return CommandResult{}, contextErr
	}
	out, outExceeded := stdout.bytes()
	errOut, errExceeded := stderr.bytes()
	if outExceeded || errExceeded {
		return CommandResult{}, fmt.Errorf("command output exceeds limit")
	}
	if ioErr != nil {
		return CommandResult{}, fmt.Errorf("command I/O failed")
	}
	if waitErr != nil || closeErr != nil {
		return CommandResult{}, fmt.Errorf("command settlement failed")
	}
	if processResult.Cancelled || processResult.TimedOut {
		return CommandResult{}, fmt.Errorf("command interrupted")
	}
	if processResult.ExitCode != 0 {
		return CommandResult{}, fmt.Errorf("command failed")
	}
	if r.Redactor != nil {
		out = []byte(r.Redactor.Text(string(out)))
		errOut = []byte(r.Redactor.Text(string(errOut)))
	}
	return CommandResult{Stdout: out, Stderr: errOut}, nil
}

func (r *ShellCommandRunner) validRuntime(requestRoot string) bool {
	if r == nil || r.Runner == nil || r.Plans == nil || r.WorkingDirectory == nil ||
		r.WorkingDirectory.Identity() == (safefs.Identity{}) {
		return false
	}
	expected := stringsCleanAbsolutePath(r.ProjectRoot)
	actual := stringsCleanAbsolutePath(requestRoot)
	if expected == "" || actual != expected {
		return false
	}
	return !r.WorkspaceBound || liveWorkspaceIdentity(expected, r.WorkingDirectory.Identity())
}

func stringsCleanAbsolutePath(value string) string {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return ""
	}
	return value
}

func (r *ShellCommandRunner) cleanupGrace() time.Duration {
	if r != nil && r.JoinGrace > 0 && r.JoinGrace <= maxCommandCleanupGrace {
		return r.JoinGrace
	}
	return time.Second
}

type commandIOResult struct {
	err error
}

type commandWaitResult struct {
	result proctree.Result
	err    error
}

func runCommandProcess(
	ctx context.Context,
	process proctree.Process,
	eventJSON []byte,
	stdout *limitBuffer,
	stderr *limitBuffer,
	cleanupGrace time.Duration,
) (proctree.Result, error, error, error) {
	if process == nil || stdout == nil || stderr == nil {
		return proctree.Result{}, fmt.Errorf("command process is unavailable"), nil, nil
	}
	pipes := process.Pipes()
	if pipes.Stdin == nil || pipes.Stdout == nil || pipes.Stderr == nil {
		return proctree.Result{}, fmt.Errorf("command pipes are unavailable"), nil, closeCommandProcess(process, cleanupGrace)
	}

	ioDone := make(chan commandIOResult, 3)
	go func() {
		err := writeCommandInput(pipes.Stdin, eventJSON)
		if closeErr := process.CloseStdin(); err == nil {
			err = closeErr
		}
		ioDone <- commandIOResult{err: err}
	}()
	go func() {
		_, err := io.Copy(stdout, pipes.Stdout)
		ioDone <- commandIOResult{err: err}
	}()
	go func() {
		_, err := io.Copy(stderr, pipes.Stderr)
		ioDone <- commandIOResult{err: err}
	}()

	waitDone := make(chan commandWaitResult, 1)
	go func() {
		result, err := process.Wait(context.Background())
		waitDone <- commandWaitResult{result: result, err: err}
	}()

	remainingIO := 3
	var waited commandWaitResult
	waitComplete := false
	var ioErr error
	var closeErr error
	var closeOnce sync.Once
	closeProcess := func() {
		closeOnce.Do(func() {
			closeErr = closeCommandProcess(process, cleanupGrace)
		})
	}
	ctxDone := ctx.Done()
	for remainingIO > 0 || !waitComplete {
		select {
		case completed := <-ioDone:
			remainingIO--
			if completed.err != nil && ioErr == nil {
				ioErr = completed.err
				closeProcess()
			}
		case waited = <-waitDone:
			waitComplete = true
			closeProcess()
		case <-ctxDone:
			ctxDone = nil
			closeProcess()
		}
	}
	closeProcess()
	return waited.result, waited.err, ioErr, closeErr
}

// closeCommandProcess deliberately derives cleanup from Background rather
// than the request context. Cancellation stops the Hook action; this second,
// bounded context still gives proctree time to terminate and reap descendants.
func closeCommandProcess(process proctree.Process, grace time.Duration) error {
	if process == nil {
		return nil
	}
	if grace <= 0 {
		grace = time.Second
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return process.Close(cleanupCtx)
}

func writeCommandInput(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func controlledEnvironment(projectRoot string, configured map[string]string) []string {
	allowed := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}
	values := map[string]string{}
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range configured {
		values[key] = value
	}
	values["PWD"] = projectRoot
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
