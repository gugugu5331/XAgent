package proctree_test

import (
	"reflect"
	"runtime"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

type contractSink struct{}

func (contractSink) Add(diagnostics.SanitizeInput) {}

func TestPublicRunnerFactorySelectsExactPlatform(t *testing.T) {
	runner, err := proctree.NewRunner(proctree.Options{CleanupTimeout: time.Second, Diagnostics: contractSink{}})
	if err != nil {
		t.Fatal("create public platform runner failed")
	}
	want := map[string]string{
		"darwin":  "*proctree.posixRunner",
		"linux":   "*proctree.linuxRunner",
		"windows": "*proctree.windowsRunner",
	}[runtime.GOOS]
	if want == "" {
		want = "*proctree.unsupportedRunner"
	}
	if got := reflect.TypeOf(runner).String(); got != want {
		t.Fatalf("public runner type = %q, want %q", got, want)
	}

	opened, err := safefs.Bootstrap(t.TempDir(), safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap public factory root failed")
	}
	defer opened.Root.Close()
	if _, err := proctree.NewProtectionPlanFactory([]*safefs.Root{opened.Root}, t.TempDir()); err != nil {
		t.Fatal("public protection plan factory was not constructible")
	}
}
