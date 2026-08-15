package tool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/diagnostics"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

func TestBashIdentityDistinguishesSemanticBytes(t *testing.T) {
	rootPath := t.TempDir()
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap Bash identity root failed")
	}
	defer opened.Root.Close()
	registry, err := NewRegistry(rootPath)
	if err != nil {
		t.Fatal("create Bash identity registry failed")
	}
	executor := NewExecutorWithWriteAccess(registry, rootPath, time.Second, 1024, opened.Root, opened.Capabilities.Ordinary())
	identity := func(id, arguments string) permission.CallIdentity {
		t.Helper()
		validated, validateErr := executor.PrepareCall(context.Background(), Call{ID: id, Name: "Bash", ArgumentsJSON: arguments})
		if validateErr != nil {
			t.Fatal("prepare Bash identity call failed")
		}
		got, identityErr := executor.CallIdentity(validated)
		if identityErr != nil {
			t.Fatal("build Bash identity failed")
		}
		return got
	}
	base := identity("base", `{"command":"printf x"}`)
	newline := identity("newline", `{"command":"printf x\n"}`)
	control := identity("control", `{"command":"printf x\u0000"}`)
	if base == newline || base == control || newline == control {
		t.Fatal("Bash identity collapsed distinct semantic bytes")
	}
}

func TestBashCannotWriteProtectedPermissionPaths(t *testing.T) {
	rootPath := t.TempDir()
	permissionDirectory := filepath.Join(rootPath, ".xagent")
	if err := os.Mkdir(permissionDirectory, 0o700); err != nil {
		t.Fatal("create protected permission directory failed")
	}
	permissionPath := filepath.Join(permissionDirectory, "permissions.local.yaml")
	const original = "version: 1\nrules: []\n"
	if err := os.WriteFile(permissionPath, []byte(original), 0o600); err != nil {
		t.Fatal("create protected permission file failed")
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap protected Bash root failed")
	}
	defer opened.Root.Close()
	scratchParent := t.TempDir()
	plans, err := proctree.NewProtectionPlanFactory([]*safefs.Root{opened.Root}, scratchParent)
	if err != nil {
		t.Fatal("create protected Bash plan factory failed")
	}
	runner, err := proctree.NewRunner(proctree.Options{CleanupTimeout: time.Second, Diagnostics: bashDiagnosticSink{}})
	if err != nil {
		t.Fatal("create protected Bash runner failed")
	}
	capture := newTestCapture(t, &captureTestStore{}, 1024, 4096)
	bash, err := NewProtectedBashTool(rootPath, BashRuntime{
		Runner:           runner,
		Plans:            plans,
		WorkingDirectory: opened.Root,
		Capture:          fixedBashCapture(capture),
		ResultFactory:    bashResultFactory(t),
	})
	if err != nil {
		t.Fatal("create protected Bash tool failed")
	}
	command := `printf compromised > .xagent/permissions.local.yaml`
	if runtime.GOOS == "windows" {
		command = `> .xagent\permissions.local.yaml echo compromised`
	}
	result := bash.Execute(context.Background(), Input{
		Name:      "Bash",
		CallID:    "protected-permission",
		Arguments: map[string]any{"command": command},
	})
	if result.Status == StatusSuccess {
		t.Fatal("protected Bash reported success while writing a permission file")
	}
	content, err := os.ReadFile(permissionPath)
	if err != nil || string(content) != original {
		t.Fatal("protected Bash changed a permission file")
	}
	entries, err := os.ReadDir(scratchParent)
	if err != nil || len(entries) != 0 {
		t.Fatal("protected Bash retained per-run scratch")
	}
}

func TestBashCancellationReapsDescendants(t *testing.T) {
	rootPath := t.TempDir()
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap cancellation root failed")
	}
	defer opened.Root.Close()
	process := newBlockingBashProcess()
	runner := &bashTestRunner{process: process, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	capture := newTestCapture(t, &captureTestStore{}, 64, 1024)
	bash, err := NewProtectedBashTool(rootPath, BashRuntime{
		Runner:           runner,
		Plans:            bashTestPlanFactory{},
		WorkingDirectory: opened.Root,
		Capture:          fixedBashCapture(capture),
		ResultFactory:    bashResultFactory(t),
	})
	if err != nil {
		t.Fatal("create cancellation Bash tool failed")
	}
	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- bash.Execute(ctx, Input{Name: "Bash", CallID: "cancel", Arguments: map[string]any{"command": "long-running"}})
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Bash did not cross the fake protected start boundary")
	}
	cancel()
	select {
	case result := <-resultCh:
		if result.Status != StatusError || result.ExecutionState() != CancelledAfterStart {
			t.Fatalf("canceled Bash result = %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Bash did not finish independent cleanup")
	}
	if !process.wasClosed() || !process.wasReaped() || process.stdinCloseCount() != 1 {
		t.Fatal("canceled Bash did not close stdin and reap its process tree")
	}
}

func TestBashCaptureHonorsHardLimit(t *testing.T) {
	rootPath := t.TempDir()
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap capture root failed")
	}
	defer opened.Root.Close()
	store := &captureTestStore{}
	capture := newTestCapture(t, store, 8, 16)
	process := newStaticBashProcess(strings.Repeat("stdout", 64), strings.Repeat("stderr", 64), proctree.Result{})
	runner := &bashTestRunner{process: process, started: make(chan struct{})}
	bash, err := NewProtectedBashTool(rootPath, BashRuntime{
		Runner:           runner,
		Plans:            bashTestPlanFactory{},
		WorkingDirectory: opened.Root,
		Capture:          fixedBashCapture(capture),
		ResultFactory:    bashResultFactory(t),
	})
	if err != nil {
		t.Fatal("create capture Bash tool failed")
	}
	result := bash.Execute(context.Background(), Input{Name: "Bash", CallID: "capture", Arguments: map[string]any{"command": "large-output"}})
	meta := result.OutputMeta()
	if result.Status != StatusError || !result.Truncated || meta.Artifact == nil || meta.Artifact.Complete ||
		meta.TruncationReason.Text() != string(CaptureTruncatedHardLimit) {
		t.Fatalf("hard-limit Bash result = %#v meta=%#v", result, meta)
	}
	if len(store.last.data) != 16 || !process.wasClosed() || process.stdinCloseCount() != 1 {
		t.Fatal("hard-limit Bash did not stop the producer at the exact cap")
	}
	request := runner.lastRequest()
	if request.Mode != proctree.ProtectionRequired || request.Executable == "" || len(request.Args) == 0 {
		t.Fatal("Bash did not use the protected runner request")
	}
}

type bashDiagnosticSink struct{}

func (bashDiagnosticSink) Add(diagnostics.SanitizeInput) {}

type bashTestPlanFactory struct{}

func (bashTestPlanFactory) Create(context.Context) (proctree.ProtectionPlan, error) {
	return proctree.ProtectionPlan{}, nil
}

func fixedBashCapture(capture *Capture) func(context.Context, artifact.Metadata) (*Capture, error) {
	return func(context.Context, artifact.Metadata) (*Capture, error) { return capture, nil }
}

func bashResultFactory(t *testing.T) *ResultFactory {
	t.Helper()
	factory, err := NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

type bashTestRunner struct {
	mu       sync.Mutex
	process  proctree.Process
	startErr error
	request  proctree.Request
	started  chan struct{}
	once     sync.Once
}

func (r *bashTestRunner) Start(_ context.Context, request proctree.Request) (proctree.Process, error) {
	r.mu.Lock()
	r.request = request
	r.mu.Unlock()
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	return r.process, r.startErr
}

func (r *bashTestRunner) lastRequest() proctree.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.request
}

type bashTestWriteCloser struct {
	mu     sync.Mutex
	closed int
}

func (*bashTestWriteCloser) Write(input []byte) (int, error) { return len(input), nil }
func (w *bashTestWriteCloser) Close() error {
	w.mu.Lock()
	w.closed++
	w.mu.Unlock()
	return nil
}
func (w *bashTestWriteCloser) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

type staticBashProcess struct {
	pipes      proctree.Pipes
	result     proctree.Result
	stdin      *bashTestWriteCloser
	mu         sync.Mutex
	closeCalls int
}

func newStaticBashProcess(stdout, stderr string, result proctree.Result) *staticBashProcess {
	stdin := &bashTestWriteCloser{}
	return &staticBashProcess{
		pipes: proctree.Pipes{
			Stdin:  stdin,
			Stdout: io.NopCloser(strings.NewReader(stdout)),
			Stderr: io.NopCloser(strings.NewReader(stderr)),
		},
		result: result,
		stdin:  stdin,
	}
}

func (p *staticBashProcess) Pipes() proctree.Pipes { return p.pipes }
func (p *staticBashProcess) CloseStdin() error     { return p.stdin.Close() }
func (p *staticBashProcess) Wait(context.Context) (proctree.Result, error) {
	return p.result, nil
}
func (p *staticBashProcess) Terminate(ctx context.Context) error { return p.Close(ctx) }
func (p *staticBashProcess) Close(context.Context) error {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	return nil
}
func (p *staticBashProcess) wasClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls == 1
}
func (p *staticBashProcess) stdinCloseCount() int { return p.stdin.count() }

type blockingBashProcess struct {
	pipes     proctree.Pipes
	stdin     *bashTestWriteCloser
	stdout    *io.PipeWriter
	stderr    *io.PipeWriter
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	reaped    bool
}

func newBlockingBashProcess() *blockingBashProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	stdin := &bashTestWriteCloser{}
	return &blockingBashProcess{
		pipes:  proctree.Pipes{Stdin: stdin, Stdout: stdoutReader, Stderr: stderrReader},
		stdin:  stdin,
		stdout: stdoutWriter,
		stderr: stderrWriter,
		done:   make(chan struct{}),
	}
}

func (p *blockingBashProcess) Pipes() proctree.Pipes { return p.pipes }
func (p *blockingBashProcess) CloseStdin() error     { return p.stdin.Close() }
func (p *blockingBashProcess) Wait(ctx context.Context) (proctree.Result, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		p.reaped = true
		p.mu.Unlock()
		return proctree.Result{Cancelled: true}, nil
	case <-ctx.Done():
		return proctree.Result{}, ctx.Err()
	}
}
func (p *blockingBashProcess) Terminate(ctx context.Context) error { return p.Close(ctx) }
func (p *blockingBashProcess) Close(context.Context) error {
	p.closeOnce.Do(func() {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.done)
	})
	return nil
}
func (p *blockingBashProcess) wasClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
func (p *blockingBashProcess) wasReaped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reaped
}
func (p *blockingBashProcess) stdinCloseCount() int { return p.stdin.count() }

var _ proctree.Runner = (*bashTestRunner)(nil)
var _ proctree.Process = (*staticBashProcess)(nil)
var _ proctree.Process = (*blockingBashProcess)(nil)
