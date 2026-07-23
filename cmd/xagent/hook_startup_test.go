package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/mcpclient"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

func TestHookConfigLoadsBeforeDependencies(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()
	hookDir := filepath.Join(projectRoot, ".xagent")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte("version: 1\nhooks:\n  - event: not_an_event\n    action:\n      type: prompt\n      content: no\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	factories := defaultStartupFactories()
	factories.stderr = ioDiscardBuffer{}
	factories.loadConfig = func(string, config.LoadOptions) (*config.AppConfig, error) {
		return &config.AppConfig{}, nil
	}
	factories.getwd = func() (string, error) { return projectRoot, nil }
	factories.userHomeDir = func() (string, error) { return homeDir, nil }
	var downstream atomic.Int32
	factories.buildRuntime = func(hook.Snapshot, hook.EngineOptions) (hook.Runtime, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}
	factories.newStore = func(conversation.JSONLStoreOptions) (conversation.ConversationStore, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}
	factories.newProvider = func(config.LLMConfig) (provider.Provider, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}
	factories.newRegistry = func(string) (*tool.Registry, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}
	factories.newMCP = func(config.MCPConfig, mcpclient.ManagerOptions) mcpRuntime {
		downstream.Add(1)
		return nil
	}
	factories.newSkillManager = func(string, string, *tool.Registry, func(string) string) (*skill.Manager, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}
	factories.runTUI = func(tea.Model) (tea.Model, error) {
		downstream.Add(1)
		return nil, errors.New("must not run")
	}

	err := runWithFactories(nil, factories)
	if err == nil || !strings.Contains(err.Error(), "Hook 配置错误") {
		t.Fatalf("error = %v, want strict Hook config failure", err)
	}
	if got := downstream.Load(); got != 0 {
		t.Fatalf("downstream factory side effects = %d, want 0", got)
	}
}

func TestFallbackBeforeSystemStart(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()
	runtime := &recordingRuntime{Runtime: hook.Noop()}
	mcp := &recordingMCP{}
	factories := defaultStartupFactories()
	factories.stderr = ioDiscardBuffer{}
	factories.loadConfig = func(string, config.LoadOptions) (*config.AppConfig, error) {
		return &config.AppConfig{Session: config.SessionConfig{Dir: "sessions"}}, nil
	}
	factories.getwd = func() (string, error) { return projectRoot, nil }
	factories.userHomeDir = func() (string, error) { return homeDir, nil }
	factories.buildRuntime = func(hook.Snapshot, hook.EngineOptions) (hook.Runtime, error) {
		return runtime, nil
	}
	factories.newProvider = func(config.LLMConfig) (provider.Provider, error) {
		return startupProvider{}, nil
	}
	factories.newMCP = func(config.MCPConfig, mcpclient.ManagerOptions) mcpRuntime { return mcp }
	factories.newSkillManager = func(string, string, *tool.Registry, func(string) string) (*skill.Manager, error) {
		return nil, errors.New("injected downstream failure")
	}
	var tuiCalls atomic.Int32
	factories.runTUI = func(tea.Model) (tea.Model, error) {
		tuiCalls.Add(1)
		return nil, nil
	}

	err := runWithFactories(nil, factories)
	if err == nil || !strings.Contains(err.Error(), "injected downstream failure") {
		t.Fatalf("error = %v, want injected failure", err)
	}
	if got := runtime.starts.Load(); got != 0 {
		t.Fatalf("system_start calls = %d, want 0", got)
	}
	if got := runtime.stops.Load(); got != 0 {
		t.Fatalf("system_stop calls = %d, want 0 before system_start", got)
	}
	if got := runtime.shutdowns.Load(); got != 1 {
		t.Fatalf("Hook shutdown calls = %d, want 1", got)
	}
	if got := mcp.starts.Load(); got != 1 {
		t.Fatalf("MCP start calls = %d, want 1", got)
	}
	if got := mcp.closes.Load(); got != 1 {
		t.Fatalf("MCP close calls = %d, want 1", got)
	}
	if got := tuiCalls.Load(); got != 0 {
		t.Fatalf("post-failure TUI factory calls = %d, want 0", got)
	}
}

func TestCompositeCloseOrder(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	record := func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}
	type contextKey struct{}
	contexts := map[shutdownTarget]string{}
	closer := newCompositeCloser(diagnostics.NewCollector(diagnostics.CollectorOptions{}), nil, ioDiscardBuffer{})
	closer.newContext = func(target shutdownTarget) (context.Context, context.CancelFunc) {
		value := string(target) + "-context"
		contexts[target] = value
		ctx := context.WithValue(context.Background(), contextKey{}, value)
		return context.WithCancel(ctx)
	}
	closer.SetApp(closeFunc(func(ctx context.Context) error {
		record("app:" + ctx.Value(contextKey{}).(string))
		return nil
	}))
	closer.SetHook(shutdownFunc(func(ctx context.Context) error {
		record("hook:" + ctx.Value(contextKey{}).(string))
		return nil
	}))
	closer.SetMCP(closeFunc(func(ctx context.Context) error {
		record("mcp:" + ctx.Value(contextKey{}).(string))
		return nil
	}))

	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := closer.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	group.Wait()

	want := []string{"app:app-context", "hook:hook-context", "mcp:mcp-context"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %#v, want %#v", order, want)
	}
	if contexts[shutdownHook] == contexts[shutdownMCP] {
		t.Fatalf("Hook and MCP reused context budget: %#v", contexts)
	}
}

func TestCompositeCloseFailureIsRedactedAndPayloadFreeOnStderr(t *testing.T) {
	const secret = "shutdown-secret"
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	var stderr bytes.Buffer
	closer := newCompositeCloser(collector, func(value string) string {
		return strings.ReplaceAll(value, secret, "[REDACTED]")
	}, &stderr)
	closer.SetHook(shutdownFunc(func(context.Context) error { return errors.New("failed with " + secret) }))

	if err := closer.Close(); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Close error = %v, want safe failure", err)
	}
	if output := stderr.String(); strings.Contains(output, secret) || strings.Contains(output, "failed with") {
		t.Fatalf("stderr leaked shutdown payload: %q", output)
	}
	items := collector.List()
	if len(items) != 1 || !strings.Contains(items[0].Message, "[REDACTED]") || strings.Contains(items[0].Message, secret) {
		t.Fatalf("diagnostics = %#v, want one redacted item", items)
	}
}

func TestCompositeCloseMirrorsSafeHookShutdownNotice(t *testing.T) {
	const secret = "shutdown-notice-secret"
	redactText := func(value string) string { return strings.ReplaceAll(value, secret, "[redacted]") }
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redactText})
	var stderr bytes.Buffer
	notice := diagnostics.New(
		hook.DiagnosticShutdownCancelled,
		diagnostics.SeverityWarning,
		"\x1b[31mbackground actions cancelled "+secret+"\x1b[0m",
	).WithAttributes(map[string]string{"stage": "shutdown"})
	closer := newCompositeCloser(collector, redactText, &stderr)
	closer.SetHook(&shutdownNoticeRuntime{collector: collector, notice: notice})
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	items := collector.List()
	if len(items) != 1 {
		t.Fatalf("diagnostics = %#v", items)
	}
	output := stderr.String()
	if !strings.Contains(output, items[0].Text()) || !strings.Contains(output, hook.DiagnosticShutdownCancelled) {
		t.Fatalf("stderr did not mirror shutdown diagnostic: %q", output)
	}
	if strings.Contains(output, secret) || strings.Contains(output, "\x1b") {
		t.Fatalf("stderr notice was unsafe: %q", output)
	}
}

func TestNoHookProcessCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		emptyFile bool
	}{
		{name: "files absent"},
		{name: "empty rules", emptyFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectRoot, homeDir := t.TempDir(), t.TempDir()
			if tc.emptyFile {
				dir := filepath.Join(projectRoot, ".xagent")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "hooks.yaml"), []byte("version: 1\nhooks: []\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			mcp := &recordingMCP{}
			var runtime *recordingRuntime
			var loadedRules int
			factories := defaultStartupFactories()
			factories.stderr = &stderr
			factories.loadConfig = func(string, config.LoadOptions) (*config.AppConfig, error) {
				return &config.AppConfig{Session: config.SessionConfig{Dir: "sessions"}}, nil
			}
			factories.getwd = func() (string, error) { return projectRoot, nil }
			factories.userHomeDir = func() (string, error) { return homeDir, nil }
			factories.buildRuntime = func(snapshot hook.Snapshot, options hook.EngineOptions) (hook.Runtime, error) {
				loadedRules = len(snapshot.Rules())
				engine, err := hook.NewEngine(snapshot, options)
				if err != nil {
					return nil, err
				}
				runtime = &recordingRuntime{Runtime: engine}
				return runtime, nil
			}
			factories.newProvider = func(config.LLMConfig) (provider.Provider, error) { return startupProvider{}, nil }
			factories.newMCP = func(config.MCPConfig, mcpclient.ManagerOptions) mcpRuntime { return mcp }
			factories.runTUI = func(model tea.Model) (tea.Model, error) { return model, nil }

			if err := runWithFactories(nil, factories); err != nil {
				t.Fatal(err)
			}
			if loadedRules != 0 || runtime == nil || runtime.starts.Load() != 1 || runtime.shutdowns.Load() != 1 || runtime.stops.Load() != 1 {
				t.Fatalf("empty Hook lifecycle changed: rules=%d runtime=%#v", loadedRules, runtime)
			}
			if mcp.starts.Load() != 1 || mcp.closes.Load() != 1 {
				t.Fatalf("existing process resources changed: starts=%d closes=%d", mcp.starts.Load(), mcp.closes.Load())
			}
			if stderr.Len() != 0 {
				t.Fatalf("empty Hook emitted stderr: %q", stderr.String())
			}
		})
	}
}

type closeFunc func(context.Context) error

func (fn closeFunc) Close(ctx context.Context) error { return fn(ctx) }

type shutdownFunc func(context.Context) error

func (fn shutdownFunc) Shutdown(ctx context.Context) error { return fn(ctx) }

type shutdownNoticeRuntime struct {
	collector *diagnostics.Collector
	notice    diagnostics.Diagnostic
}

func (r *shutdownNoticeRuntime) Shutdown(context.Context) error {
	r.collector.Add(r.notice)
	return nil
}

func (r *shutdownNoticeRuntime) ShutdownNotices() []diagnostics.Diagnostic {
	return []diagnostics.Diagnostic{r.notice}
}

type recordingRuntime struct {
	hook.Runtime
	starts    atomic.Int32
	stops     atomic.Int32
	shutdowns atomic.Int32
}

func (r *recordingRuntime) SystemStart(ctx context.Context) {
	r.starts.Add(1)
	r.Runtime.SystemStart(ctx)
}

func (r *recordingRuntime) Shutdown(ctx context.Context) error {
	r.shutdowns.Add(1)
	if r.starts.Load() > 0 {
		r.stops.Add(1)
	}
	return r.Runtime.Shutdown(ctx)
}

type recordingMCP struct {
	starts atomic.Int32
	closes atomic.Int32
}

func (m *recordingMCP) Start(context.Context)               { m.starts.Add(1) }
func (m *recordingMCP) Tools() []tool.Tool                  { return nil }
func (m *recordingMCP) StatusLine() string                  { return "" }
func (m *recordingMCP) Summary() mcpclient.StatusSummary    { return mcpclient.StatusSummary{} }
func (m *recordingMCP) Diagnostics() []mcpclient.Diagnostic { return nil }
func (m *recordingMCP) Close(context.Context) error         { m.closes.Add(1); return nil }

type startupProvider struct{}

func (startupProvider) Name() string { return "startup-test" }
func (startupProvider) StreamChat(context.Context, provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	return nil, errors.New("not used")
}

type ioDiscardBuffer struct{}

func (ioDiscardBuffer) Write(data []byte) (int, error) { return len(data), nil }
