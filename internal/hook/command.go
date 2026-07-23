package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"xagent/internal/redact"
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

type ShellCommandRunner struct {
	Limits         Limits
	Redactor       *redact.RuntimeRedactor
	TerminateGrace time.Duration
	JoinGrace      time.Duration
	pipeFactory    commandPipeFactory
}

// commandPipeFactory is deliberately limited to the three os/exec pipe
// constructors. Besides keeping ownership of process creation in the runner,
// the seam lets tests inject failures at the actual stdin-write and
// stdout/stderr-read boundaries.
type commandPipeFactory interface {
	stdin(*exec.Cmd) (io.WriteCloser, error)
	stdout(*exec.Cmd) (io.ReadCloser, error)
	stderr(*exec.Cmd) (io.ReadCloser, error)
}

type execCommandPipeFactory struct{}

func (execCommandPipeFactory) stdin(cmd *exec.Cmd) (io.WriteCloser, error) {
	return cmd.StdinPipe()
}

func (execCommandPipeFactory) stdout(cmd *exec.Cmd) (io.ReadCloser, error) {
	return cmd.StdoutPipe()
}

func (execCommandPipeFactory) stderr(cmd *exec.Cmd) (io.ReadCloser, error) {
	return cmd.StderrPipe()
}

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
	cmd := exec.Command("/bin/sh", "-c", request.Command)
	cmd.Dir = request.ProjectRoot
	cmd.Env = controlledEnvironment(request.ProjectRoot, request.Environment)
	factory := r.pipeFactory
	if factory == nil {
		factory = execCommandPipeFactory{}
	}
	stdin, err := factory.stdin(cmd)
	if err != nil {
		return CommandResult{}, fmt.Errorf("command pipe setup failed")
	}
	stdoutPipe, err := factory.stdout(cmd)
	if err != nil {
		_ = stdin.Close()
		return CommandResult{}, fmt.Errorf("command pipe setup failed")
	}
	stderrPipe, err := factory.stderr(cmd)
	if err != nil {
		_ = stdin.Close()
		_ = stdoutPipe.Close()
		return CommandResult{}, fmt.Errorf("command pipe setup failed")
	}
	closePipes := func() {
		_ = stdin.Close()
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
	}
	setupCommand(cmd)
	if err := cmd.Start(); err != nil {
		closePipes()
		return CommandResult{}, fmt.Errorf("command start failed")
	}
	stdout := &limitBuffer{limit: limits.CommandStdoutBytes}
	stderr := &limitBuffer{limit: limits.CommandStderrBytes}
	type ioResult struct{ err error }
	ioDone := make(chan ioResult, 3)
	go func() {
		err := writeCommandInput(stdin, request.EventJSON)
		if closeErr := stdin.Close(); err == nil {
			err = closeErr
		}
		ioDone <- ioResult{err: err}
	}()
	go func() {
		_, err := io.Copy(stdout, stdoutPipe)
		_ = stdoutPipe.Close()
		ioDone <- ioResult{err: err}
	}()
	go func() {
		_, err := io.Copy(stderr, stderrPipe)
		_ = stderrPipe.Close()
		ioDone <- ioResult{err: err}
	}()

	grace := r.TerminateGrace
	if grace <= 0 {
		grace = 100 * time.Millisecond
	}
	joinGrace := r.JoinGrace
	if joinGrace <= 0 {
		joinGrace = time.Second
	}
	terminated := false
	var joinTimer *time.Timer
	var joinDone <-chan time.Time
	var waitDone chan error
	var waitErr error
	waitComplete := false
	startWait := func() {
		if waitDone != nil {
			return
		}
		waitDone = make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()
	}
	terminate := func() {
		if terminated {
			return
		}
		terminated = true
		terminateCommand(cmd, grace)
		// A descendant can leave the command's process group while retaining an
		// inherited stdout/stderr descriptor. Killing the original group then
		// cannot produce EOF, so explicitly close our pipe ends before joining
		// the copy goroutines. The root process is already force-killed above,
		// which makes cmd.Wait safe to start concurrently on this failure path.
		closePipes()
		startWait()
		joinTimer = time.NewTimer(joinGrace)
		joinDone = joinTimer.C
	}
	ctxDone := ctx.Done()
	var contextErr, ioErr error
	remaining := 3
	for remaining > 0 || (terminated && !waitComplete) {
		select {
		case result := <-ioDone:
			remaining--
			if result.err != nil && ioErr == nil {
				ioErr = result.err
				terminate()
			}
		case waitErr = <-waitDone:
			waitComplete = true
			waitDone = nil
		case <-ctxDone:
			contextErr = ctx.Err()
			ctxDone = nil
			terminate()
		case <-joinDone:
			// Standard os/exec pipes unblock when closed and Process.Kill makes
			// Wait reapable, so reaching this guard indicates an OS/pipe contract
			// failure. Buffered completion channels ensure late settlement cannot
			// block a goroutine while the caller still gets a bounded return.
			_ = cmd.Process.Kill()
			if contextErr != nil {
				return CommandResult{}, contextErr
			}
			if ioErr != nil {
				return CommandResult{}, fmt.Errorf("command I/O failed")
			}
			return CommandResult{}, fmt.Errorf("command settlement timed out")
		}
	}
	if joinTimer != nil {
		if !joinTimer.Stop() {
			select {
			case <-joinTimer.C:
			default:
			}
		}
	}
	if !waitComplete {
		startWait()
	}
	for !waitComplete {
		select {
		case waitErr = <-waitDone:
			waitComplete = true
		case <-ctxDone:
			contextErr = ctx.Err()
			ctxDone = nil
			terminate()
		case <-joinDone:
			_ = cmd.Process.Kill()
			if contextErr != nil {
				return CommandResult{}, contextErr
			}
			return CommandResult{}, fmt.Errorf("command settlement timed out")
		}
	}
	if contextErr != nil {
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
	if waitErr != nil {
		return CommandResult{}, fmt.Errorf("command failed")
	}
	if r.Redactor != nil {
		out = []byte(r.Redactor.Text(string(out)))
		errOut = []byte(r.Redactor.Text(string(errOut)))
	}
	return CommandResult{Stdout: out, Stderr: errOut}, nil
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
