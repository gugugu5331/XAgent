package permission

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"xagent/internal/safefs"
)

func TestCallIdentityCanonicalizesArguments(t *testing.T) {
	first, err := NewCallIdentity(CallIdentityInput{
		ToolName:           "Example",
		CanonicalArguments: []byte(` { "z": [3, true, null], "a": {"text":"<x>", "n":1.2300} } `),
	})
	if err != nil {
		t.Fatalf("create first call identity: %v", err)
	}
	second, err := NewCallIdentity(CallIdentityInput{
		ToolName:           "Example",
		CanonicalArguments: []byte(`{"a":{"n":1.2300,"text":"\u003cx\u003e"},"z":[3,true,null]}`),
	})
	if err != nil {
		t.Fatalf("create second call identity: %v", err)
	}
	if first != second {
		t.Fatal("equivalent argument objects produced different identities")
	}

	different, err := NewCallIdentity(CallIdentityInput{
		ToolName:           "Example",
		CanonicalArguments: []byte(`{"a":{"n":1.2301,"text":"<x>"},"z":[3,true,null]}`),
	})
	if err != nil {
		t.Fatalf("create changed-argument identity: %v", err)
	}
	if first == different {
		t.Fatal("different canonical arguments produced the same identity")
	}

	for _, input := range []CallIdentityInput{
		{ToolName: "", CanonicalArguments: []byte(`{}`)},
		{ToolName: " \t\n", CanonicalArguments: []byte(`{}`)},
		{ToolName: "Example", CanonicalArguments: []byte(`{"unterminated":`)},
		{ToolName: "Example", CanonicalArguments: []byte(`["not", "an", "object"]`)},
	} {
		if _, err := NewCallIdentity(input); err == nil {
			t.Fatalf("invalid call identity input was accepted: %#v", input)
		}
	}
}

func TestCallIdentityBindsFileAndMCPResources(t *testing.T) {
	rootPath := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatalf("bootstrap first root: %v", err)
	}
	defer func() {
		if err := opened.Root.Close(); err != nil {
			t.Errorf("close first root: %v", err)
		}
	}()
	other, err := safefs.Bootstrap(t.TempDir(), safefs.Policy{})
	if err != nil {
		t.Fatalf("bootstrap second root: %v", err)
	}
	defer func() {
		if err := other.Root.Close(); err != nil {
			t.Errorf("close second root: %v", err)
		}
	}()

	firstBinding, err := opened.Root.Bind("first.txt")
	if err != nil {
		t.Fatalf("bind first resource: %v", err)
	}
	secondBinding, err := opened.Root.Bind("second.txt")
	if err != nil {
		t.Fatalf("bind second resource: %v", err)
	}
	workingDirectory := opened.Root.Identity()
	otherWorkingDirectory := other.Root.Identity()
	environmentDigest := sha256.Sum256([]byte("environment-a"))
	otherEnvironmentDigest := sha256.Sum256([]byte("environment-b"))
	targetDigest := sha256.Sum256([]byte("mcp-target-a"))
	otherTargetDigest := sha256.Sum256([]byte("mcp-target-b"))

	base := CallIdentityInput{
		ToolName:           "mcp__server__read",
		CanonicalArguments: []byte(`{"path":"first.txt"}`),
		WorkingDirectory:   &workingDirectory,
		ResourceBindings:   []safefs.Binding{firstBinding, secondBinding},
		EnvironmentDigest:  &environmentDigest,
		TargetDigest:       &targetDigest,
	}
	want, err := NewCallIdentity(base)
	if err != nil {
		t.Fatalf("create complete call identity: %v", err)
	}

	reordered := base
	reordered.ResourceBindings = []safefs.Binding{secondBinding, firstBinding}
	got, err := NewCallIdentity(reordered)
	if err != nil {
		t.Fatalf("create reordered-resource identity: %v", err)
	}
	if got != want {
		t.Fatal("resource enumeration order changed the identity")
	}

	checks := []struct {
		name   string
		mutate func(*CallIdentityInput)
	}{
		{name: "tool", mutate: func(input *CallIdentityInput) { input.ToolName = "mcp__server__write" }},
		{name: "arguments", mutate: func(input *CallIdentityInput) { input.CanonicalArguments = []byte(`{"path":"second.txt"}`) }},
		{name: "working directory", mutate: func(input *CallIdentityInput) { input.WorkingDirectory = &otherWorkingDirectory }},
		{name: "binding", mutate: func(input *CallIdentityInput) { input.ResourceBindings = []safefs.Binding{firstBinding} }},
		{name: "environment", mutate: func(input *CallIdentityInput) { input.EnvironmentDigest = &otherEnvironmentDigest }},
		{name: "MCP target", mutate: func(input *CallIdentityInput) { input.TargetDigest = &otherTargetDigest }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			input := base
			check.mutate(&input)
			identity, err := NewCallIdentity(input)
			if err != nil {
				t.Fatalf("create changed identity: %v", err)
			}
			if identity == want {
				t.Fatal("changed call context retained the original identity")
			}
		})
	}

	duplicate := base
	duplicate.ResourceBindings = []safefs.Binding{firstBinding, firstBinding}
	if _, err := NewCallIdentity(duplicate); err == nil {
		t.Fatal("duplicate resource binding was accepted")
	}
	invalidBinding := base
	invalidBinding.ResourceBindings = []safefs.Binding{{}}
	if _, err := NewCallIdentity(invalidBinding); err == nil {
		t.Fatal("invalid resource binding was accepted")
	}
}

func TestBashIdentityDistinguishesRawSemanticBytes(t *testing.T) {
	opened, err := safefs.Bootstrap(t.TempDir(), safefs.Policy{})
	if err != nil {
		t.Fatalf("bootstrap working directory: %v", err)
	}
	defer func() {
		if err := opened.Root.Close(); err != nil {
			t.Errorf("close working directory: %v", err)
		}
	}()
	other, err := safefs.Bootstrap(t.TempDir(), safefs.Policy{})
	if err != nil {
		t.Fatalf("bootstrap other working directory: %v", err)
	}
	defer func() {
		if err := other.Root.Close(); err != nil {
			t.Errorf("close other working directory: %v", err)
		}
	}()

	baseCommand := []byte(`printf 'safe value'`)
	environmentDigest := sha256.Sum256([]byte("PATH=/usr/bin"))
	base := BashIdentityInput{
		Shell:             "/bin/sh",
		WorkingDirectory:  opened.Root.Identity(),
		RawCommand:        baseCommand,
		EnvironmentDigest: environmentDigest,
	}
	want, err := NewBashIdentity(base)
	if err != nil {
		t.Fatalf("create base Bash identity: %v", err)
	}
	repeated, err := NewBashIdentity(base)
	if err != nil || repeated != want {
		t.Fatalf("identical Bash input was unstable: identity=%#v err=%v", repeated, err)
	}

	semanticVariants := []struct {
		name    string
		command []byte
	}{
		{name: "leading space", command: []byte(` printf 'safe value'`)},
		{name: "repeated space display collision", command: []byte(`printf  'safe value'`)},
		{name: "trailing newline display collision", command: []byte("printf 'safe value'\n")},
		{name: "embedded newline", command: []byte("printf 'safe value'\nprintf extra")},
		{name: "different quotes", command: []byte(`printf "safe value"`)},
		{name: "NUL control byte", command: []byte("printf 'safe value'\x00")},
		{name: "escape control byte", command: []byte("printf 'safe value'\x1b")},
	}
	for _, variant := range semanticVariants {
		t.Run(variant.name, func(t *testing.T) {
			input := base
			input.RawCommand = variant.command
			identity, err := NewBashIdentity(input)
			if err != nil {
				t.Fatalf("create semantic variant identity: %v", err)
			}
			if identity == want {
				t.Fatal("different raw command bytes reused the base identity")
			}
		})
	}
	for _, collision := range []string{`printf  'safe value'`, "printf 'safe value'\n"} {
		if NormalizeCommand(collision) != NormalizeCommand(string(baseCommand)) {
			t.Fatalf("test fixture is not a normalized-display collision: %q", collision)
		}
	}

	contextVariants := []struct {
		name   string
		mutate func(*BashIdentityInput)
	}{
		{name: "shell", mutate: func(input *BashIdentityInput) { input.Shell = "/bin/zsh" }},
		{name: "working directory", mutate: func(input *BashIdentityInput) { input.WorkingDirectory = other.Root.Identity() }},
		{name: "environment", mutate: func(input *BashIdentityInput) { input.EnvironmentDigest = sha256.Sum256([]byte("PATH=/opt/bin")) }},
	}
	for _, variant := range contextVariants {
		t.Run(variant.name, func(t *testing.T) {
			input := base
			variant.mutate(&input)
			identity, err := NewBashIdentity(input)
			if err != nil {
				t.Fatalf("create context variant identity: %v", err)
			}
			if identity == want {
				t.Fatal("changed Bash execution context reused the base identity")
			}
		})
	}

	for _, invalid := range []BashIdentityInput{
		{Shell: "", WorkingDirectory: opened.Root.Identity(), RawCommand: baseCommand, EnvironmentDigest: environmentDigest},
		{Shell: " \t\n", WorkingDirectory: opened.Root.Identity(), RawCommand: baseCommand, EnvironmentDigest: environmentDigest},
		{Shell: "/bin/sh", WorkingDirectory: safefs.Identity{}, RawCommand: baseCommand, EnvironmentDigest: environmentDigest},
	} {
		if _, err := NewBashIdentity(invalid); err == nil {
			t.Fatalf("invalid Bash identity input was accepted: %#v", invalid)
		}
	}

	baseCommand[0] = 'x'
	if repeated, err := NewBashIdentity(BashIdentityInput{
		Shell:             "/bin/sh",
		WorkingDirectory:  opened.Root.Identity(),
		RawCommand:        []byte(`printf 'safe value'`),
		EnvironmentDigest: environmentDigest,
	}); err != nil || repeated != want {
		t.Fatal("mutating caller-owned command bytes changed an existing identity")
	}
}
