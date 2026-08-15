package main

import (
	"bytes"
	"errors"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

func TestHelpAndVersionRequireNoRuntimeInitialization(t *testing.T) {
	outputs := make(map[string]string)
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		name := strings.Join(args, "_")
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runtimeCalls := 0
			code := runCLI(args, &stdout, &stderr, func([]string) error {
				runtimeCalls++
				return errors.New("runtime initialization must not run")
			})
			if code != exitSuccess || runtimeCalls != 0 {
				t.Fatalf("args=%v code=%d runtime calls=%d", args, code, runtimeCalls)
			}
			if stdout.Len() == 0 || stderr.Len() != 0 {
				t.Fatalf("args=%v stdout=%q stderr=%q", args, stdout.String(), stderr.String())
			}
			outputs[args[0]] = stdout.String()
		})
	}
	if outputs["--help"] != outputs["-h"] {
		t.Fatalf("help aliases differ:\n--help=%q\n-h=%q", outputs["--help"], outputs["-h"])
	}
	if !strings.Contains(outputs["--help"], "Usage:") || !strings.Contains(outputs["--help"], "--config") ||
		!strings.Contains(outputs["--help"], "--version") {
		t.Fatalf("help is not a valid public usage: %q", outputs["--help"])
	}
	assertNoInternalLauncherDisclosure(t, outputs["--help"])
	if strings.TrimSpace(outputs["--version"]) == "" || !strings.Contains(strings.ToLower(outputs["--version"]), "xagent") {
		t.Fatalf("version output is empty or unidentified: %q", outputs["--version"])
	}
}

func TestPublicCLIOutputAndExitContract(t *testing.T) {
	t.Run("normal startup receives only normalized config arguments", func(t *testing.T) {
		for _, test := range []struct {
			args []string
			want []string
		}{
			{args: nil, want: nil},
			{args: []string{"--config", "project.yaml"}, want: []string{"--config", "project.yaml"}},
			{args: []string{"-config=project.yaml"}, want: []string{"--config", "project.yaml"}},
		} {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			var received []string
			code := runCLI(test.args, &stdout, &stderr, func(args []string) error {
				received = append([]string(nil), args...)
				return nil
			})
			if code != exitSuccess || stdout.Len() != 0 || stderr.Len() != 0 || !reflect.DeepEqual(received, test.want) {
				t.Fatalf("args=%v code=%d received=%v stdout=%q stderr=%q", test.args, code, received, stdout.String(), stderr.String())
			}
		}
	})

	const secretArgument = "token=t421-cli-secret"
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "unknown", args: []string{"--unknown=" + secretArgument}},
		{name: "positional", args: []string{secretArgument}},
		{name: "missing config", args: []string{"--config"}},
		{name: "empty config", args: []string{"--config="}},
		{name: "repeated config", args: []string{"--config", "one.yaml", "--config", "two.yaml"}},
		{name: "mixed help", args: []string{"--help", "--unknown=" + secretArgument}},
		{name: "mixed version", args: []string{"--version", "--config", "project.yaml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runtimeCalls := 0
			code := runCLI(test.args, &stdout, &stderr, func([]string) error {
				runtimeCalls++
				return nil
			})
			if code != exitUsage || runtimeCalls != 0 || stdout.Len() != 0 {
				t.Fatalf("code=%d runtime calls=%d stdout=%q stderr=%q", code, runtimeCalls, stdout.String(), stderr.String())
			}
			output := stderr.String()
			if !strings.Contains(output, "参数错误") || !strings.Contains(output, "Usage:") {
				t.Fatalf("safe usage error missing context: %q", output)
			}
			if strings.Contains(output, secretArgument) {
				t.Fatalf("usage error leaked rejected argument: %q", output)
			}
		})
	}

	t.Run("runtime failure is stderr only", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := runCLI(nil, &stdout, &stderr, func([]string) error {
			return errors.New("safe startup failure")
		})
		if code != exitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "运行错误: safe startup failure") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestVersionUsesInjectedTraceableBuildInfo(t *testing.T) {
	restoreVersionGlobals(t)
	const pathCanary = "/Users/private/version-path-canary"
	version = "v1.2.3"
	revision = "0123456789abcdef0123456789abcdef01234567"
	buildTime = "2026-08-03T18:00:00+08:00"
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Path: pathCanary,
			Main: debug.Module{Path: pathCanary, Version: "v9.9.9"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "ffffffffffffffffffffffffffffffffffffffff"},
				{Key: "vcs.time", Value: "2025-01-01T00:00:00Z"},
				{Key: "vcs.modified", Value: "true"},
			},
		}, true
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runCLI([]string{"--version"}, &stdout, &stderr, func([]string) error {
		return errors.New("runtime must not initialize for version")
	})
	const want = "xagent version=v1.2.3 revision=0123456789abcdef0123456789abcdef01234567 build_time=2026-08-03T10:00:00Z modified=unknown\n"
	if code != exitSuccess || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), pathCanary) || strings.Contains(stdout.String(), "v9.9.9") || strings.Contains(stdout.String(), "ffffffff") {
		t.Fatalf("injected version did not take priority or leaked build paths: %q", stdout.String())
	}
}

func TestVersionUsesBuildInfoFallback(t *testing.T) {
	t.Run("whitelisted VCS settings", func(t *testing.T) {
		restoreVersionGlobals(t)
		const pathCanary = "/Users/private/build-info-path-canary"
		readBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Path: pathCanary,
				Main: debug.Module{Path: pathCanary, Version: "v0.8.4", Replace: &debug.Module{Path: pathCanary + "/replace"}},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "89abcdef0123456789abcdef0123456789abcdef"},
					{Key: "vcs.time", Value: "2026-08-02T01:02:03Z"},
					{Key: "vcs.modified", Value: "false"},
					{Key: "local.path", Value: pathCanary},
				},
			}, true
		}
		const want = "xagent version=v0.8.4 revision=89abcdef0123456789abcdef0123456789abcdef build_time=2026-08-02T01:02:03Z modified=false\n"
		if got := versionOutput(); got != want || strings.Contains(got, pathCanary) {
			t.Fatalf("build info fallback = %q, want %q", got, want)
		}
	})

	t.Run("VCS provenance is only completed for the same revision", func(t *testing.T) {
		restoreVersionGlobals(t)
		const vcsRevision = "89abcdef0123456789abcdef0123456789abcdef"
		version = "v0.8.5"
		revision = "0123456789abcdef0123456789abcdef01234567"
		readBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: vcsRevision},
				{Key: "vcs.time", Value: "2026-08-02T01:02:03Z"},
				{Key: "vcs.modified", Value: "true"},
			}}, true
		}
		const conflicting = "xagent version=v0.8.5 revision=0123456789abcdef0123456789abcdef01234567 build_time=unknown modified=unknown\n"
		if got := versionOutput(); got != conflicting {
			t.Fatalf("conflicting VCS provenance = %q, want %q", got, conflicting)
		}

		revision = vcsRevision
		const matching = "xagent version=v0.8.5 revision=89abcdef0123456789abcdef0123456789abcdef build_time=2026-08-02T01:02:03Z modified=true\n"
		if got := versionOutput(); got != matching {
			t.Fatalf("matching VCS provenance = %q, want %q", got, matching)
		}
	})

	t.Run("incomplete or unsafe metadata remains nonempty and private", func(t *testing.T) {
		restoreVersionGlobals(t)
		const pathCanary = "/Users/private/incomplete-version-canary"
		version = pathCanary
		revision = "not-a-traceable-oid"
		buildTime = pathCanary
		readBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Path: pathCanary,
				Main: debug.Module{Path: pathCanary, Version: pathCanary},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: pathCanary},
					{Key: "vcs.time", Value: pathCanary},
					{Key: "vcs.modified", Value: pathCanary},
				},
			}, true
		}
		const want = "xagent version=dev revision=unknown build_time=unknown modified=unknown\n"
		if got := versionOutput(); got != want || strings.Contains(got, pathCanary) {
			t.Fatalf("unsafe fallback = %q, want %q", got, want)
		}

		readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
		if got := versionOutput(); got != want {
			t.Fatalf("missing build info = %q, want %q", got, want)
		}
	})
}

func restoreVersionGlobals(t *testing.T) {
	t.Helper()
	oldVersion, oldRevision, oldBuildTime := version, revision, buildTime
	oldReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version, revision, buildTime = oldVersion, oldRevision, oldBuildTime
		readBuildInfo = oldReadBuildInfo
	})
	version, revision, buildTime = "", "", ""
}
