package mcpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	mcptransport "xagent/internal/mcpclient/transport"
	mcphttp "xagent/internal/mcpclient/transport/http"
	mcpstdio "xagent/internal/mcpclient/transport/stdio"
	"xagent/internal/netpolicy"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/tool"
)

var (
	errManagerDependencies      = errors.New("MCP manager dependencies are invalid")
	errManagerTransportFactory  = errors.New("MCP manager transport factory failed")
	errManagerEnvironment       = errors.New("MCP stdio environment is invalid")
	errManagerUnsupportedServer = errors.New("MCP server transport is unsupported")
)

// ManagerDependencies are security capabilities assembled by the process
// root. Manager never creates permissive network clients, unprotected process
// runners, private roots, or independent sanitization boundaries itself.
type ManagerDependencies struct {
	Diagnostics     diagnostics.BoundedSink
	RuntimeRedactor *redact.RuntimeRedactor
	ResultFactory   *tool.ResultFactory
	Capture         func(context.Context, artifact.Metadata) (*tool.Capture, error)

	HTTPPolicy        netpolicy.HTTPPolicy
	HTTPClientFactory netpolicy.ClientFactory
	HTTPClientOptions netpolicy.ClientOptions

	StdioRunner      proctree.Runner
	StdioPlanFactory proctree.ProtectionPlanFactory
	StdioRoot        *safefs.Root
	Environment      []string
}

type managerSession interface {
	Start(context.Context) error
	Snapshot() serverSessionSnapshot
	CallTool(context.Context, string, map[string]any) (protocol.CallToolResult, error)
	Close(context.Context) error
}

type capturedManagerSession interface {
	CallToolCaptured(context.Context, string, map[string]any, io.Writer) (protocol.CallToolResult, error)
}

type managerSessionFactory interface {
	New(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error)
}

type managerSessionFactoryFunc func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error)

func (factory managerSessionFactoryFunc) New(
	ctx context.Context,
	serverName string,
	serverConfig config.MCPServerConfig,
	options ManagerOptions,
) (managerSession, error) {
	return factory(ctx, serverName, serverConfig, options)
}

// secureManagerSessionFactory is the sole production session factory. Both
// transports converge on serverSession and managedConnection before a session
// can be published by Manager.
type secureManagerSessionFactory struct {
	dependencies ManagerDependencies
}

func (factory secureManagerSessionFactory) New(
	ctx context.Context,
	_ string,
	serverConfig config.MCPServerConfig,
	options ManagerOptions,
) (managerSession, error) {
	if ctx == nil {
		return nil, errManagerTransportFactory
	}

	var created mcptransport.Transport
	var err error
	switch serverConfig.Type {
	case config.MCPTransportHTTP:
		created, err = factory.newHTTP(ctx, serverConfig, options)
	case config.MCPTransportStdio:
		created, err = factory.newStdio(serverConfig, options)
	default:
		return nil, errManagerUnsupportedServer
	}
	if err != nil || created == nil {
		return nil, errManagerTransportFactory
	}

	session, err := newServerSession(serverSessionOptions{
		Transport:        created,
		Diagnostics:      factory.dependencies.Diagnostics,
		CleanupTimeout:   options.CleanupTimeout,
		MaxResponseBytes: options.MaxResponseBytes,
		MaxPages:         int64(options.MaxPages),
		MaxTools:         int64(options.MaxTools),
		ClientInfo:       protocol.ClientInfo{Name: "xagent", Version: "0.1.0"},
	})
	if err != nil {
		_ = created.Close(context.Background())
		return nil, errManagerTransportFactory
	}
	return session, nil
}

func (factory secureManagerSessionFactory) newHTTP(
	ctx context.Context,
	serverConfig config.MCPServerConfig,
	options ManagerOptions,
) (mcptransport.Transport, error) {
	if dependencyMissing(factory.dependencies.HTTPPolicy) || dependencyMissing(factory.dependencies.HTTPClientFactory) {
		return nil, errManagerDependencies
	}
	endpoint, err := factory.dependencies.HTTPPolicy.ValidateInitial(ctx, serverConfig.URL, netpolicy.PurposeMCP)
	if err != nil {
		return nil, errManagerTransportFactory
	}
	clientOptions := factory.dependencies.HTTPClientOptions
	clientOptions.SensitiveHeaders = append([]string(nil), clientOptions.SensitiveHeaders...)
	clientOptions.Timeout = options.DefaultTimeout
	return mcphttp.New(mcphttp.Config{
		Endpoint:          endpoint,
		ClientFactory:     factory.dependencies.HTTPClientFactory,
		ClientOptions:     clientOptions,
		Headers:           cloneStringMap(serverConfig.Headers),
		ProtocolVersion:   protocol.SupportedProtocolVersion,
		MaxResponseBytes:  options.MaxResponseBytes,
		MaxProtocolErrors: options.MaxProtocolErrors,
		Lifecycle: mcptransport.Options{
			Diagnostics:    factory.dependencies.Diagnostics,
			CleanupTimeout: options.CleanupTimeout,
		},
	})
}

func (factory secureManagerSessionFactory) newStdio(
	serverConfig config.MCPServerConfig,
	options ManagerOptions,
) (mcptransport.Transport, error) {
	if dependencyMissing(factory.dependencies.StdioRunner) ||
		dependencyMissing(factory.dependencies.StdioPlanFactory) ||
		factory.dependencies.StdioRoot == nil {
		return nil, errManagerDependencies
	}
	environment, err := mergeEnvironment(factory.dependencies.Environment, serverConfig.Env)
	if err != nil {
		return nil, errManagerEnvironment
	}
	return mcpstdio.New(mcpstdio.Config{
		Executable:       serverConfig.Command,
		Args:             append([]string(nil), serverConfig.Args...),
		Env:              environment,
		WorkingDirectory: factory.dependencies.StdioRoot,
		Runner:           factory.dependencies.StdioRunner,
		PlanFactory:      factory.dependencies.StdioPlanFactory,
		MaxResponseBytes: options.MaxResponseBytes,
		Lifecycle: mcptransport.Options{
			Diagnostics:    factory.dependencies.Diagnostics,
			CleanupTimeout: options.CleanupTimeout,
		},
	})
}

func validateManagerDependencies(cfg config.MCPConfig, dependencies ManagerDependencies) error {
	if dependencyMissing(dependencies.Diagnostics) || dependencies.RuntimeRedactor == nil || dependencies.ResultFactory == nil {
		return errManagerDependencies
	}
	needHTTP := false
	needStdio := false
	for _, server := range cfg.Servers {
		if server.Disabled || (server.Type == config.MCPTransportStdio && server.Source == "project") {
			continue
		}
		switch server.Type {
		case config.MCPTransportHTTP:
			needHTTP = true
		case config.MCPTransportStdio:
			needStdio = true
		}
	}
	if needHTTP && (dependencyMissing(dependencies.HTTPPolicy) || dependencyMissing(dependencies.HTTPClientFactory)) {
		return errManagerDependencies
	}
	if needStdio && (dependencyMissing(dependencies.StdioRunner) ||
		dependencyMissing(dependencies.StdioPlanFactory) || dependencies.StdioRoot == nil || len(dependencies.Environment) == 0) {
		return errManagerDependencies
	}
	return nil
}

func dependencyMissing(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// mergeEnvironment creates a complete deterministic process environment.
// Names are compared case-insensitively on every platform so a configuration
// cannot produce different duplicate-key behavior after cross-compilation.
func mergeEnvironment(base []string, overrides map[string]string) ([]string, error) {
	type entry struct {
		name  string
		value string
	}
	merged := make(map[string]entry, len(base)+len(overrides))
	for _, raw := range base {
		separator := strings.IndexByte(raw, '=')
		if separator <= 0 {
			return nil, errManagerEnvironment
		}
		name := raw[:separator]
		value := raw[separator+1:]
		if !validEnvironmentEntry(name, value) {
			return nil, errManagerEnvironment
		}
		merged[canonicalEnvironmentName(name)] = entry{name: name, value: value}
	}

	overrideNames := make([]string, 0, len(overrides))
	for name := range overrides {
		overrideNames = append(overrideNames, name)
	}
	sort.Strings(overrideNames)
	seenOverrides := make(map[string]struct{}, len(overrides))
	for _, name := range overrideNames {
		value := overrides[name]
		if !validEnvironmentEntry(name, value) {
			return nil, errManagerEnvironment
		}
		canonical := canonicalEnvironmentName(name)
		if _, duplicate := seenOverrides[canonical]; duplicate {
			return nil, errManagerEnvironment
		}
		seenOverrides[canonical] = struct{}{}
		merged[canonical] = entry{name: name, value: value}
	}
	if len(merged) == 0 {
		return nil, errManagerEnvironment
	}

	canonicalNames := make([]string, 0, len(merged))
	for name := range merged {
		canonicalNames = append(canonicalNames, name)
	}
	sort.Strings(canonicalNames)
	result := make([]string, 0, len(canonicalNames))
	for _, canonical := range canonicalNames {
		item := merged[canonical]
		result = append(result, item.name+"="+item.value)
	}
	return result, nil
}

func validEnvironmentEntry(name string, value string) bool {
	return name != "" && strings.IndexByte(name, '=') < 0 && strings.IndexByte(name, 0) < 0 && strings.IndexByte(value, 0) < 0
}

func canonicalEnvironmentName(name string) string {
	return strings.ToUpper(name)
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func canonicalHeaderName(name string) string {
	return http.CanonicalHeaderKey(name)
}
