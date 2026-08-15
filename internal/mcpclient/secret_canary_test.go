package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"xagent/internal/config"
	"xagent/internal/mcpclient/protocol"
)

const mcpSecretCanary = "XAGENT_MCP_SECRET_CANARY_20260802_7F31"

func TestMCPSecretCanaryDoesNotReachSnapshotOrDiagnostics(t *testing.T) {
	runMCPSecretCanaryManagerScenario(t)
}

func TestMCPSecretCanaryTestOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate current test executable")
	}
	command := exec.Command(executable,
		"-test.run=^(TestMCPSecretCanaryDoesNotReachSnapshotOrDiagnostics|TestAdapterCanaryReturnsOnlySafeViews)$",
		"-test.count=1",
		"-test.v",
	)
	output, childErr := command.CombinedOutput()
	if containsMCPSecretMaterial(string(output), mcpSecretCanary) {
		t.Fatal("MCP canary appeared in child test output")
	}
	if childErr != nil {
		t.Fatal("MCP canary child tests failed")
	}
}
func runMCPSecretCanaryManagerScenario(t *testing.T) {
	t.Helper()
	const (
		factoryFailureServer = "factory-failure"
		startFailureServer   = "start-failure"
		runningServer        = "running"
	)

	startFailure := newMCPSecretCanarySession(serverSessionStateFailed, nil)
	startFailure.startErr = errors.New("session start failed: " + mcpSecretCanary)
	running := newMCPSecretCanarySession(serverSessionStateRunning, []protocol.RemoteTool{{
		Name:        "safe_tool",
		Description: "safe remote description",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`),
	}})

	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		factoryFailureServer: {
			Type:    config.MCPTransportHTTP,
			URL:     "https://safe.example.invalid/mcp",
			Headers: map[string]string{"X-MCP-Secret": mcpSecretCanary},
		},
		startFailureServer: {
			Type:    config.MCPTransportStdio,
			Command: "safe-command",
			Env:     map[string]string{"XAGENT_MCP_SECRET": mcpSecretCanary},
		},
		runningServer: {
			Type: config.MCPTransportHTTP,
			URL:  "https://safe.example.invalid/running",
		},
	}}
	var sawHeader, sawEnvironment atomic.Bool
	manager := newManagerWithFactoryForTest(t, cfg, ManagerOptions{}, managerSessionFactoryFunc(func(
		_ context.Context,
		serverName string,
		serverConfig config.MCPServerConfig,
		_ ManagerOptions,
	) (managerSession, error) {
		switch serverName {
		case factoryFailureServer:
			if serverConfig.Headers["X-MCP-Secret"] == mcpSecretCanary {
				sawHeader.Store(true)
			}
			return nil, errors.New("factory failed: " + mcpSecretCanary)
		case startFailureServer:
			if serverConfig.Env["XAGENT_MCP_SECRET"] == mcpSecretCanary {
				sawEnvironment.Store(true)
			}
			return startFailure, nil
		case runningServer:
			return running, nil
		default:
			return nil, errors.New("unexpected test server")
		}
	}), nil)
	manager.Start(context.Background())
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Fatal("close MCP canary manager")
		}
	})

	if !sawHeader.Load() || !sawEnvironment.Load() {
		t.Fatal("manager session factory did not receive the secret-bearing transport configuration")
	}
	if startFailure.startCalls.Load() != 1 || running.startCalls.Load() != 1 {
		t.Fatal("manager sessions did not observe the expected startup calls")
	}

	snapshot := manager.Snapshot()
	if snapshot.State != ManagerStateRunning || len(snapshot.Tools) != 1 {
		t.Fatal("manager canary snapshot has an unexpected state or tool count")
	}
	assertMCPSecretCanaryAbsent(t, "manager snapshot state", managerStateText(snapshot.State))
	for _, published := range snapshot.Tools {
		if published == nil {
			t.Fatal("manager canary snapshot contains a nil tool")
		}
		schema, err := json.Marshal(published.Schema())
		if err != nil {
			t.Fatal("marshal published MCP tool schema")
		}
		assertMCPSecretCanaryAbsent(t, "manager snapshot tool",
			published.Name(), published.Description(), string(published.Risk()), string(schema))
	}

	diagnostics := manager.Diagnostics()
	if len(diagnostics) != 2 {
		t.Fatal("manager canary diagnostics have an unexpected count")
	}
	diagnosticJSON, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatal("marshal manager canary diagnostics")
	}
	assertMCPSecretCanaryAbsent(t, "manager diagnostics", string(diagnosticJSON))

	summary := manager.Summary()
	if summary.Ready != 1 || summary.Failed != 2 {
		t.Fatal("manager canary summary has unexpected server counts")
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		t.Fatal("marshal manager canary summary")
	}
	assertMCPSecretCanaryAbsent(t, "manager summary", string(summaryJSON))
	assertMCPSecretCanaryAbsent(t, "manager status line", manager.StatusLine())
}

func managerStateText(state ManagerState) string {
	switch state {
	case ManagerStateNew:
		return "new"
	case ManagerStateStarting:
		return "starting"
	case ManagerStateRunning:
		return "running"
	case ManagerStateClosing:
		return "closing"
	case ManagerStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

func assertMCPSecretCanaryAbsent(t *testing.T, label string, values ...string) {
	t.Helper()
	for _, value := range values {
		if containsMCPSecretMaterial(value, mcpSecretCanary) {
			t.Fatal(label + " contains the raw MCP canary")
		}
	}
}

func containsMCPSecretMaterial(value string, secret string) bool {
	fragments := []string{secret, secret[:len(secret)/2], secret[len(secret)/2:]}
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

type mcpSecretCanarySession struct {
	state      serverSessionState
	tools      []protocol.RemoteTool
	startErr   error
	startCalls atomic.Int32
	closeCalls atomic.Int32
}

func newMCPSecretCanarySession(state serverSessionState, tools []protocol.RemoteTool) *mcpSecretCanarySession {
	return &mcpSecretCanarySession{state: state, tools: cloneSessionTools(tools)}
}

func (session *mcpSecretCanarySession) Start(context.Context) error {
	session.startCalls.Add(1)
	return session.startErr
}

func (session *mcpSecretCanarySession) Snapshot() serverSessionSnapshot {
	return serverSessionSnapshot{
		State: session.state, ToolsSupported: session.state == serverSessionStateRunning,
		Tools: cloneSessionTools(session.tools),
	}
}

func (session *mcpSecretCanarySession) CallTool(context.Context, string, map[string]any) (protocol.CallToolResult, error) {
	return protocol.CallToolResult{}, nil
}

func (session *mcpSecretCanarySession) Close(context.Context) error {
	session.closeCalls.Add(1)
	return nil
}
