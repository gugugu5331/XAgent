package mcpclient

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"xagent/internal/config"
)

func TestMergeEnvironmentIsDeterministicAndCaseInsensitive(t *testing.T) {
	base := []string{"zeta=base", "Path=/first", "PATH=/second", "alpha=base"}
	overrides := map[string]string{"path": "/override", "BETA": "configured"}
	want := []string{"alpha=base", "BETA=configured", "path=/override", "zeta=base"}

	first, err := mergeEnvironment(base, overrides)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeEnvironment(base, map[string]string{"BETA": "configured", "path": "/override"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("merged environments = %#v and %#v, want %#v", first, second, want)
	}
	if _, err := mergeEnvironment([]string{"BROKEN"}, nil); !errors.Is(err, errManagerEnvironment) {
		t.Fatalf("invalid inherited environment error = %v", err)
	}
	if _, err := mergeEnvironment([]string{"PATH=/bin"}, map[string]string{"Path": "one", "PATH": "two"}); !errors.Is(err, errManagerEnvironment) {
		t.Fatalf("case-colliding override error = %v", err)
	}
}

func TestServerConfigDigestIsCanonicalAndComplete(t *testing.T) {
	options := ManagerOptions{
		MaxTools: 17, MaxPages: 3, MaxResponseBytes: 4096,
		MaxProtocolErrors: 7, DefaultTimeout: 5 * time.Second,
	}
	first := config.MCPServerConfig{
		Type: config.MCPTransportHTTP, URL: "https://example.invalid/mcp", Source: "user",
		Headers: map[string]string{"Authorization": "Bearer secret", "X-Trace": "one"},
	}
	reordered := first
	reordered.Headers = map[string]string{"X-Trace": "one", "authorization": "Bearer secret"}

	firstDigest, err := serverConfigDigest(first, options)
	if err != nil {
		t.Fatal(err)
	}
	reorderedDigest, err := serverConfigDigest(reordered, options)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != reorderedDigest {
		t.Fatal("equivalent header spelling or map order changed server config digest")
	}
	changed := first
	changed.URL = "https://example.invalid/other"
	changedDigest, err := serverConfigDigest(changed, options)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("changed endpoint reused server config digest")
	}
}

func TestManagerRequiresExplicitDependenciesAndSingleStart(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {Type: config.MCPTransportHTTP, URL: "https://example.invalid/mcp"},
	}}
	if _, err := NewManager(cfg, ManagerOptions{}, ManagerDependencies{}); !errors.Is(err, errManagerDependencies) {
		t.Fatalf("missing dependency error = %v", err)
	}

	manager, err := NewManager(config.MCPConfig{}, ManagerOptions{}, managerTestDependencies(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); !errors.Is(err, errManagerAlreadyStarted) {
		t.Fatalf("second Start error = %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSecureFactoryBuildsProtectedStdioSession(t *testing.T) {
	workingDirectory := t.TempDir()
	dependencies := managerStdioTestDependencies(t, workingDirectory)
	factory := secureManagerSessionFactory{dependencies: dependencies}
	session, err := factory.New(context.Background(), "stdio", config.MCPServerConfig{
		Type: config.MCPTransportStdio, Command: "/safe/fake-mcp",
		Env: map[string]string{"PATH": "/configured/bin"},
	}, ManagerOptions{
		MaxTools: 8, MaxPages: 2, MaxResponseBytes: 4096,
		MaxProtocolErrors: 4, DefaultTimeout: time.Second, CleanupTimeout: time.Second,
	})
	if err != nil || session == nil {
		t.Fatalf("build protected stdio session: session=%T err=%v", session, err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
