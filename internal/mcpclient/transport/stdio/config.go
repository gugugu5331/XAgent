package stdio

import (
	"reflect"
	"strings"
	"time"

	"xagent/internal/budget"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

// Config accepts only the protected process capabilities assembled by the
// application root. It deliberately has no ProtectionMode, reusable
// ProtectionPlan, Process, Pipes, exec.Cmd, or operating-system handle field.
type Config struct {
	Executable       string
	Args             []string
	Env              []string
	WorkingDirectory *safefs.Root
	Runner           proctree.Runner
	PlanFactory      proctree.ProtectionPlanFactory
	MaxResponseBytes int64
	Lifecycle        mcptransport.Options
}

type resolvedConfig struct {
	executable       string
	args             []string
	env              []string
	workingDirectory *safefs.Root
	runner           proctree.Runner
	planFactory      proctree.ProtectionPlanFactory
	maxResponseBytes int64
	lifecycle        mcptransport.Options
	cleanupTimeout   time.Duration
}

func resolveConfig(config Config) (resolvedConfig, error) {
	if config.Executable == "" || config.Executable != strings.TrimSpace(config.Executable) ||
		strings.IndexByte(config.Executable, 0) >= 0 || config.WorkingDirectory == nil ||
		config.WorkingDirectory.Identity() == (safefs.Identity{}) || isNilInterface(config.Runner) ||
		isNilInterface(config.PlanFactory) || isNilInterface(config.Lifecycle.Diagnostics) ||
		config.Lifecycle.CleanupTimeout < 0 || config.Lifecycle.CleanupTimeout > mcptransport.MaxCleanupTimeout ||
		!validStrings(config.Args) || len(config.Env) == 0 || !validEnvironment(config.Env) {
		return resolvedConfig{}, ErrInvalidConfig
	}
	maxResponseBytes, err := resolveResponseLimit(config.MaxResponseBytes)
	if err != nil {
		return resolvedConfig{}, ErrInvalidConfig
	}
	cleanupTimeout := config.Lifecycle.CleanupTimeout
	if cleanupTimeout == 0 {
		cleanupTimeout = mcptransport.MaxCleanupTimeout
	}
	return resolvedConfig{
		executable:       config.Executable,
		args:             append([]string(nil), config.Args...),
		env:              append([]string(nil), config.Env...),
		workingDirectory: config.WorkingDirectory,
		runner:           config.Runner,
		planFactory:      config.PlanFactory,
		maxResponseBytes: maxResponseBytes,
		lifecycle:        config.Lifecycle,
		cleanupTimeout:   cleanupTimeout,
	}, nil
}

func resolveResponseLimit(configured int64) (int64, error) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope != budget.MCPMaxResponseBytes {
			continue
		}
		if configured == 0 {
			return spec.Resolve(nil)
		}
		return spec.Resolve(&configured)
	}
	return 0, ErrInvalidConfig
}

func validStrings(values []string) bool {
	for _, value := range values {
		if strings.IndexByte(value, 0) >= 0 {
			return false
		}
	}
	return true
}

func validEnvironment(values []string) bool {
	if !validStrings(values) {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		separator := strings.IndexByte(value, '=')
		if separator <= 0 {
			return false
		}
		name := strings.ToUpper(value[:separator])
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func isNilInterface(value any) bool {
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
