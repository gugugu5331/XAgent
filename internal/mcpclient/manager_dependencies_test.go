package mcpclient

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/netpolicy"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/tool"
)

func managerTestDependencies(t *testing.T, sink diagnostics.BoundedSink) ManagerDependencies {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	if sink == nil {
		sink = &managerTestDiagnosticSink{}
	}
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	return ManagerDependencies{
		Diagnostics:     sink,
		RuntimeRedactor: redactor,
		ResultFactory:   resultFactory,
	}
}

func managerHTTPTestDependencies(t *testing.T) ManagerDependencies {
	t.Helper()
	dependencies := managerTestDependencies(t, nil)
	policy := netpolicy.NewPolicy()
	dependencies.HTTPPolicy = policy
	dependencies.HTTPClientFactory = netpolicy.NewClientFactory(policy)
	return dependencies
}

func managerStdioTestDependencies(t *testing.T, workingDirectory string) ManagerDependencies {
	t.Helper()
	dependencies := managerTestDependencies(t, nil)
	opened, err := safefs.Bootstrap(workingDirectory, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	planFactory, err := proctree.NewProtectionPlanFactory([]*safefs.Root{opened.Root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := proctree.NewRunner(proctree.Options{CleanupTimeout: time.Second, Diagnostics: dependencies.Diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	dependencies.StdioRunner = runner
	dependencies.StdioPlanFactory = planFactory
	dependencies.StdioRoot = opened.Root
	dependencies.Environment = os.Environ()
	return dependencies
}

func newManagerWithFactoryForTest(
	t *testing.T,
	cfg config.MCPConfig,
	options ManagerOptions,
	factory managerSessionFactory,
	sink diagnostics.BoundedSink,
) *Manager {
	t.Helper()
	manager, err := newManager(cfg, options, managerTestDependencies(t, sink), factory)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestManagerCaptureDependencySelectsSafeCandidateAdapter(t *testing.T) {
	dependencies := managerTestDependencies(t, nil)
	captureCalls := 0
	dependencies.Capture = func(context.Context, artifact.Metadata) (*tool.Capture, error) {
		captureCalls++
		return nil, nil
	}
	manager := newManagerForAdapterBranchTest(t, dependencies)
	tools, routes := manager.buildToolCandidates(managerAdapterBranchServer(), []protocol.RemoteTool{{
		Name: "echo", InputSchema: []byte(`{"type":"object"}`),
	}}, map[string]bool{})
	if len(tools) != 1 || len(routes) != 1 {
		t.Fatalf("safe candidate tools/routes = %d/%d, want 1/1", len(tools), len(routes))
	}
	adapter, ok := tools[0].(ToolAdapter)
	if !ok || adapter.legacy || !adapter.UsesSafeResultBoundary() || adapter.capture == nil {
		t.Fatalf("candidate manager produced a non-safe adapter: %#v", tools[0])
	}
	if adapter.resultFactory != dependencies.ResultFactory {
		t.Fatal("candidate adapter did not retain the injected ResultFactory identity")
	}
	_, _ = adapter.capture(context.Background(), artifact.Metadata{MediaType: "text/plain"})
	if captureCalls != 1 {
		t.Fatalf("candidate adapter Capture calls = %d, want 1", captureCalls)
	}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.RegisterWithOptions(adapter, adapter.RegistrationOptions()); err != nil {
		t.Fatalf("candidate adapter was rejected by safe Registry: %v", err)
	}
}

func TestManagerNilCapturePreservesLegacyMigrationAdapter(t *testing.T) {
	dependencies := managerTestDependencies(t, nil)
	if dependencies.Capture != nil {
		t.Fatal("legacy test dependencies unexpectedly provide Capture")
	}
	manager := newManagerForAdapterBranchTest(t, dependencies)
	tools, routes := manager.buildToolCandidates(managerAdapterBranchServer(), []protocol.RemoteTool{{
		Name: "echo", InputSchema: []byte(`{"type":"object"}`),
	}}, map[string]bool{})
	if len(tools) != 1 || len(routes) != 1 {
		t.Fatalf("legacy tools/routes = %d/%d, want 1/1", len(tools), len(routes))
	}
	adapter, ok := tools[0].(ToolAdapter)
	if !ok || !adapter.legacy || adapter.UsesSafeResultBoundary() || adapter.capture != nil {
		t.Fatalf("nil Capture did not select the legacy migration adapter: %#v", tools[0])
	}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.RegisterWithOptions(adapter, adapter.RegistrationOptions()); err == nil {
		t.Fatal("safe Registry accepted the legacy migration adapter")
	}
}

func newManagerForAdapterBranchTest(t *testing.T, dependencies ManagerDependencies) *Manager {
	t.Helper()
	manager, err := newManager(
		config.MCPConfig{},
		ManagerOptions{},
		dependencies,
		managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func managerAdapterBranchServer() *managedServer {
	return &managedServer{name: "server", configDigest: sha256.Sum256([]byte("server config"))}
}

type managerTestDiagnosticSink struct{}

func (*managerTestDiagnosticSink) Add(diagnostics.SanitizeInput) {}
