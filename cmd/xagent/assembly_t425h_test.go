package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"xagent/internal/config"
)

// TestAssemblyHasNoHTTPTransportInjection freezes C13's deliberately narrow
// Assembly input surface. Network customisation is restricted to a CA pool;
// callers must never be able to smuggle in an HTTP client or any part of its
// transport policy through the composition root.
func TestAssemblyHasNoHTTPTransportInjection(t *testing.T) {
	assemblyType := reflect.TypeOf(Assembly{})
	if assemblyType.NumField() != 1 {
		t.Fatalf("Assembly field count = %d, want only the private production factory graph", assemblyType.NumField())
	}
	factoryField := assemblyType.Field(0)
	if factoryField.Name != "factories" || factoryField.Type != reflect.TypeOf(assemblyFactories{}) || factoryField.IsExported() {
		t.Fatalf("Assembly retained an input or injectable capability: %#v", factoryField)
	}

	options := reflect.TypeOf(AssemblyOptions{})
	want := map[string]reflect.Type{
		"Paths":        reflect.TypeOf(RuntimePaths{}),
		"Config":       reflect.TypeOf(ConfigInputs{}),
		"LookupEnv":    reflect.TypeOf((func(string) (string, bool))(nil)),
		"TrustedRoots": reflect.TypeOf((*x509.CertPool)(nil)),
		"Stdin":        reflect.TypeOf((*bytes.Reader)(nil)).Elem(),
		"Stdout":       reflect.TypeOf((*bytes.Buffer)(nil)).Elem(),
		"Stderr":       reflect.TypeOf((*bytes.Buffer)(nil)).Elem(),
	}
	if options.NumField() != len(want) {
		t.Fatalf("AssemblyOptions field count = %d, want %d; network injection surface changed", options.NumField(), len(want))
	}
	for name, fieldType := range want {
		field, ok := options.FieldByName(name)
		if !ok {
			t.Fatalf("AssemblyOptions is missing required field %q", name)
		}
		if name == "Stdin" || name == "Stdout" || name == "Stderr" {
			// These are intentionally interface-typed I/O capabilities.
			if field.Type.Kind() != reflect.Interface {
				t.Fatalf("AssemblyOptions.%s = %v, want I/O interface", name, field.Type)
			}
			continue
		}
		if field.Type != fieldType {
			t.Fatalf("AssemblyOptions.%s = %v, want %v", name, field.Type, fieldType)
		}
	}
	for index := 0; index < options.NumField(); index++ {
		field := options.Field(index)
		for _, forbidden := range []string{"http.", "Client", "Transport", "RoundTripper", "Proxy", "Dial", "Redirect", "Cookie", "Handler"} {
			if field.Name != "TrustedRoots" && strings.Contains(field.Name+" "+field.Type.String(), forbidden) {
				t.Fatalf("AssemblyOptions.%s (%v) exposes forbidden HTTP injection capability %q", field.Name, field.Type, forbidden)
			}
		}
	}
}

// TestTrustedRootsReachProviderHookAndMCPOnlyThroughNetpolicy makes the three
// HTTP owners explicit. Assembly may propagate a CA pool only as
// netpolicy.ClientOptions; netpolicy is responsible for cloning it while
// retaining ownership of the client, transport and all protocol policy.
func TestTrustedRootsReachProviderHookAndMCPOnlyThroughNetpolicy(t *testing.T) {
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatalf("read assembly source: %v", err)
	}
	text := string(source)
	if got := strings.Count(text, "TrustedRoots: state.security.trustedRoots"); got != 3 {
		t.Fatalf("TrustedRoots reaches %d adapter ClientOptions values, want provider, hook, and MCP exactly once each", got)
	}
	for _, marker := range []string{
		"providerClient, err := state.security.networkClients.New(providerEndpoint, netpolicy.ClientOptions{",
		"ClientOptions: netpolicy.ClientOptions{TrustedRoots: state.security.trustedRoots}",
		"HTTPClientOptions: netpolicy.ClientOptions{TrustedRoots: state.security.trustedRoots}",
		"trustedRoots: options.TrustedRoots",
		"trustedRoots:    cloneAssemblyTrustedRoots(request.trustedRoots)",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("TrustedRoots propagation is no longer confined to netpolicy: missing %q", marker)
		}
	}
	if strings.Contains(text, "provider.ProviderOptions{\n\t\t\tEndpoint:        providerEndpoint,\n\t\t\tTrustedRoots:") {
		t.Fatal("provider received TrustedRoots outside netpolicy.ClientOptions")
	}

	callerRoots := x509.NewCertPool()
	callerRoots.AddCert(&x509.Certificate{Raw: []byte("initial-test-root"), RawSubject: []byte("initial-test-root")})
	var buildSnapshot *x509.CertPool
	assembly := defaultAssembly()
	assembly.factories = assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if state == nil || state.options == nil || state.options.TrustedRoots == nil {
			t.Fatal("stage did not receive the Build-private TrustedRoots snapshot")
		}
		if stage == assemblyStageConfig {
			buildSnapshot = state.options.TrustedRoots
			if buildSnapshot == callerRoots || len(buildSnapshot.Subjects()) != 1 {
				t.Fatal("Build retained the caller certificate pool instead of cloning it")
			}
			callerRoots.AddCert(&x509.Certificate{Raw: []byte("post-build-caller-root"), RawSubject: []byte("post-build-caller-root")})
			if len(callerRoots.Subjects()) != 2 || len(buildSnapshot.Subjects()) != 1 {
				t.Fatal("caller mutation changed the Build-private certificate pool")
			}
		} else if state.options.TrustedRoots != buildSnapshot {
			t.Fatal("Assembly stages did not share one immutable Build snapshot")
		}
		return register(func(context.Context) error { return nil })
	})
	root := t.TempDir()
	runtime, err := assembly.Build(context.Background(), validT425hAssemblyOptions(root, callerRoots))
	if err != nil || runtime == nil {
		t.Fatalf("Build with caller certificate pool failed: runtime=%v err=%v", runtime, err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close certificate-pool test Runtime: %v", err)
	}
}

func TestAssemblyBuildRejectsInvalidOptionsBeforeStages(t *testing.T) {
	root := t.TempDir()
	base := validT425hAssemblyOptions(root, nil)
	var typedNilReader *bytes.Reader
	var typedNilWriter *bytes.Buffer
	tests := []struct {
		name   string
		mutate func(*AssemblyOptions)
	}{
		{name: "missing project root", mutate: func(options *AssemblyOptions) { options.Paths.ProjectRoot = "" }},
		{name: "relative user config root", mutate: func(options *AssemblyOptions) { options.Paths.UserConfigRoot = "relative" }},
		{name: "noncanonical data root", mutate: func(options *AssemblyOptions) { options.Paths.UserDataRoot += string(filepath.Separator) + "." }},
		{name: "relative config input", mutate: func(options *AssemblyOptions) { options.Config.ProjectPath = "config.yaml" }},
		{name: "nil environment lookup", mutate: func(options *AssemblyOptions) { options.LookupEnv = nil }},
		{name: "nil stdin", mutate: func(options *AssemblyOptions) { options.Stdin = nil }},
		{name: "typed nil stdin", mutate: func(options *AssemblyOptions) { options.Stdin = typedNilReader }},
		{name: "nil stdout", mutate: func(options *AssemblyOptions) { options.Stdout = nil }},
		{name: "typed nil stdout", mutate: func(options *AssemblyOptions) { options.Stdout = typedNilWriter }},
		{name: "nil stderr", mutate: func(options *AssemblyOptions) { options.Stderr = nil }},
		{name: "typed nil stderr", mutate: func(options *AssemblyOptions) { options.Stderr = typedNilWriter }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stageCalls atomic.Int32
			assembly := defaultAssembly()
			assembly.factories = assemblyFactoriesForTest(func(_ int, _ assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
				stageCalls.Add(1)
				return register(func(context.Context) error { return nil })
			})
			options := base
			test.mutate(&options)
			runtime, err := assembly.Build(context.Background(), options)
			if err == nil || runtime != nil {
				t.Fatalf("invalid Assembly options published Runtime: runtime=%v err=%v", runtime, err)
			}
			if stageCalls.Load() != 0 {
				t.Fatalf("invalid Assembly options entered %d stages", stageCalls.Load())
			}
		})
	}
}

func validT425hAssemblyOptions(root string, trustedRoots *x509.CertPool) AssemblyOptions {
	return AssemblyOptions{
		Paths: RuntimePaths{
			ProjectRoot:    root,
			UserConfigRoot: filepath.Join(root, "config"),
			UserDataRoot:   filepath.Join(root, "data"),
			UserCacheRoot:  filepath.Join(root, "cache"),
		},
		LookupEnv:    os.LookupEnv,
		TrustedRoots: trustedRoots,
		Stdin:        strings.NewReader(""),
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	}
}

// TestRunCLIUsesSystemRoots freezes the normal-CLI Assembly seam without
// performing T4.29a's later atomic publication cutover. The seam must prepare
// every OS-derived root and explicit system trust input now; runArgs remains
// on the legacy graph until the complete App/TUI Runtime can be published.
func TestRunCLIUsesSystemRoots(t *testing.T) {
	options, err := assemblyOptionsFromSystem([]string{"--config", config.DefaultConfigFile})
	if err != nil {
		t.Fatalf("prepare normal CLI Assembly options: %v", err)
	}
	for name, root := range map[string]string{
		"project":     options.Paths.ProjectRoot,
		"user config": options.Paths.UserConfigRoot,
		"user data":   options.Paths.UserDataRoot,
		"user cache":  options.Paths.UserCacheRoot,
	} {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			t.Fatalf("%s root is not absolute and canonical: %q", name, root)
		}
	}
	if options.Config.ProjectPath == "" || !filepath.IsAbs(options.Config.ProjectPath) ||
		filepath.Clean(options.Config.ProjectPath) != options.Config.ProjectPath {
		t.Fatalf("project config is not absolute and canonical: %q", options.Config.ProjectPath)
	}
	if options.Config.UserPath != "" && (!filepath.IsAbs(options.Config.UserPath) ||
		filepath.Clean(options.Config.UserPath) != options.Config.UserPath) {
		t.Fatalf("user config is not absolute and canonical: %q", options.Config.UserPath)
	}
	if options.LookupEnv == nil || options.Stdin != os.Stdin || options.Stdout != os.Stdout ||
		options.Stderr != os.Stderr || options.TrustedRoots != nil {
		t.Fatal("normal CLI did not use explicit process I/O, environment lookup, and system trust roots")
	}

	source, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read CLI Assembly seam: %v", err)
	}
	text := string(source)
	for _, marker := range []string{"func buildAssemblyFromCLI(", "defaultAssembly().Build(", "TrustedRoots: nil"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("normal CLI Assembly seam is incomplete: missing %q", marker)
		}
	}
	assertLegacyProductionEntryUnchanged(t)
}

// TestDefaultAssemblyPublishesSingleRuntimeRegistry proves the published
// Runtime retains the one registry created by Build, and that repeated normal
// close requests cannot replay the close table. The test factory only avoids
// external effects; the Build/Runtime publication path is the production one.
func TestDefaultAssemblyPublishesSingleRuntimeRegistry(t *testing.T) {
	assembly := defaultAssembly()
	for index, builder := range assembly.factories.ordered() {
		if builder == nil {
			t.Fatalf("defaultAssembly factory %d is nil", index)
		}
	}

	var closes atomic.Int32
	assembly.factories = assemblyFactoriesForTest(func(_ int, _ assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
		return register(func(context.Context) error {
			closes.Add(1)
			return nil
		})
	})
	roots := t.TempDir()
	runtime, err := assembly.Build(context.Background(), validT425hAssemblyOptions(roots, nil))
	if err != nil || runtime == nil {
		t.Fatalf("Assembly.Build did not publish Runtime: runtime=%v err=%v", runtime, err)
	}
	if runtime.owners == nil || runtime.owners.registrationCount() != assemblyStageCount {
		t.Fatal("published Runtime does not retain Build's complete single ownership registry")
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("first Runtime.Close: %v", err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("second Runtime.Close: %v", err)
	}
	if got := closes.Load(); got != assemblyStageCount {
		t.Fatalf("registry close calls = %d, want exactly %d (one shared registry)", got, assemblyStageCount)
	}
}
