package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDiscoverSingleFiles(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "zeta.md"), "zeta", ModeShared, "zeta body", nil, 0, "")
	writeSkillFile(t, filepath.Join(root, "alpha.md"), "Alpha", ModeShared, "alpha body", []string{"Read"}, 0, "")
	if err := os.WriteFile(filepath.Join(root, "broken.md"), []byte("not frontmatter token=secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	definitions, diagnosticItems, err := discover(SourceFS{Source: SourceProject, Root: root}, DefaultLimits(), func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[redacted]")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 || definitions[0].Name != "alpha" || definitions[1].Name != "zeta" {
		t.Fatalf("unexpected definitions: %#v", definitions)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if definitions[0].PackageRoot != canonicalRoot || !filepath.IsAbs(definitions[0].EntryPath) || definitions[0].Fingerprint == "" {
		t.Fatalf("paths/fingerprint not normalized: %#v", definitions[0])
	}
	if len(diagnosticItems) != 1 || diagnosticItems[0].Code != "skill_entry_invalid" {
		t.Fatalf("unexpected diagnostics: %#v", diagnosticItems)
	}
	if strings.Contains(diagnosticItems[0].Text(), "secret-value") {
		t.Fatalf("diagnostic leaked secret: %#v", diagnosticItems[0])
	}
}

func TestDiscoverDirectoryPackages(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "review")
	writeSkillFile(t, filepath.Join(packageRoot, "SKILL.md"), "review", ModeIsolated, "review body", nil, 1, "")
	if err := os.WriteFile(filepath.Join(packageRoot, "example.md"), []byte("must not be loaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(packageRoot, "nested")
	writeSkillFile(t, filepath.Join(nested, "SKILL.md"), "nested", ModeShared, "nested", nil, 0, "")
	target := filepath.Join(root, "target.md")
	writeSkillFile(t, target, "target", ModeShared, "target", nil, 0, "")
	if err := os.Symlink(target, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	linkedPackage := filepath.Join(root, "linked-package")
	if err := os.MkdirAll(linkedPackage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkedPackage, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	definitions, diagnosticItems, err := discover(SourceFS{Source: SourceProject, Root: root}, DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := definitionNames(definitions); !reflect.DeepEqual(got, []string{"review", "target"}) {
		t.Fatalf("unexpected discovered names: %#v", got)
	}
	for _, definition := range definitions {
		if strings.Contains(definition.Body, "must not be loaded") || definition.Name == "nested" {
			t.Fatalf("auxiliary or nested entry leaked into discovery: %#v", definition)
		}
	}
	canonicalPackageRoot, err := filepath.EvalSymlinks(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	if definitions[0].PackageRoot != canonicalPackageRoot {
		t.Fatalf("unexpected package root: %q", definitions[0].PackageRoot)
	}
	if len(diagnosticItems) != 2 {
		t.Fatalf("expected two symlink diagnostics, got %#v", diagnosticItems)
	}
}

func TestDiscoverLimitsAndDeterministicOrder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"c", "a", "b"} {
		writeSkillFile(t, filepath.Join(root, name+".md"), name, ModeShared, name, nil, 0, "")
	}
	limits := DefaultLimits()
	limits.MaxFiles = 2
	first, firstDiagnostics, err := discover(SourceFS{Source: SourceUser, Root: root}, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDiagnostics, err := discover(SourceFS{Source: SourceUser, Root: root}, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(firstDiagnostics, secondDiagnostics) {
		t.Fatalf("discovery is not deterministic:\n%#v\n%#v", first, second)
	}
	if got := definitionNames(first); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("entry limit did not use sorted order: %#v", got)
	}
	if len(firstDiagnostics) != 1 || firstDiagnostics[0].Code != "skill_source_entry_limit" {
		t.Fatalf("missing limit diagnostic: %#v", firstDiagnostics)
	}

	oversizedRoot := t.TempDir()
	writeSkillFile(t, filepath.Join(oversizedRoot, "large.md"), "large", ModeShared, strings.Repeat("x", 64), nil, 0, "")
	smallLimits := DefaultLimits()
	smallLimits.MaxEntryBytes = 32
	definitions, diagnosticItems, err := discover(SourceFS{Source: SourceProject, Root: oversizedRoot}, smallLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 0 || len(diagnosticItems) != 1 || diagnosticItems[0].Code != "skill_entry_too_large" {
		t.Fatalf("unexpected oversized result: %#v %#v", definitions, diagnosticItems)
	}
}

func TestDiscoveryIsBoundedAndCrossPlatform(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "alpha.md"), "alpha", ModeShared, "alpha", nil, 0, "")
	writeSkillFile(t, filepath.Join(root, "review", "SKILL.md"), "review", ModeIsolated, "review", nil, 1, "")
	writeSkillFile(t, filepath.Join(root, "review", "nested", "SKILL.md"), "nested", ModeShared, "nested", nil, 0, "")

	definitions, diagnosticItems, err := discoverContext(context.Background(), SourceFS{Source: SourceProject, Root: root}, DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := definitionNames(definitions); !reflect.DeepEqual(got, []string{"alpha", "review"}) {
		t.Fatalf("physical discovery changed Skill package semantics: %#v", got)
	}
	if len(diagnosticItems) != 0 {
		t.Fatalf("bounded physical discovery returned diagnostics: %#v", diagnosticItems)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := discoverContext(canceled, SourceFS{Source: SourceProject, Root: root}, DefaultLimits(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("physical discovery ignored cancellation: %v", err)
	}

	boundedRoot := t.TempDir()
	for index := 0; index < DefaultMaxFiles+2; index++ {
		name := fmt.Sprintf("ignored-%03d.txt", index)
		if err := os.WriteFile(filepath.Join(boundedRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := DefaultLimits()
	limits.MaxFiles = 1
	definitions, diagnosticItems, err = discoverContext(context.Background(), SourceFS{Source: SourceUser, Root: boundedRoot}, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 0 || len(diagnosticItems) != 1 || diagnosticItems[0].Code != "skill_source_scan_limit" {
		t.Fatalf("physical discovery did not fail closed at its traversal bound: %#v %#v", definitions, diagnosticItems)
	}
}

func TestDiscoverUnreadableAndOversizedEntriesDoNotBlockValidEntries(t *testing.T) {
	valid := []byte("---\nname: valid\ndescription: valid description\nmode: shared\n---\nvalid body")
	unreadable := []byte("---\nname: unreadable\ndescription: unreadable description\nmode: shared\n---\nunreadable body")
	files := fstest.MapFS{
		"large.md":      &fstest.MapFile{Data: []byte(strings.Repeat("x", 512)), Mode: 0o600},
		"unreadable.md": &fstest.MapFile{Data: unreadable, Mode: 0o600},
		"valid.md":      &fstest.MapFile{Data: valid, Mode: 0o600},
	}
	sourceFS := failingEntryOpenFS{FS: files, failures: map[string]error{"unreadable.md": fs.ErrPermission}}
	limits := DefaultLimits()
	limits.MaxEntryBytes = 256

	definitions, diagnosticItems, err := discover(SourceFS{Source: SourceProject, FS: sourceFS, Root: "."}, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := definitionNames(definitions); !reflect.DeepEqual(got, []string{"valid"}) {
		t.Fatalf("bad entries blocked or polluted valid discovery: %#v", got)
	}
	codes := make([]string, len(diagnosticItems))
	for index, item := range diagnosticItems {
		codes[index] = item.Code
	}
	if !reflect.DeepEqual(codes, []string{"skill_entry_read_failed", "skill_entry_too_large"}) {
		t.Fatalf("unexpected diagnostics: %#v", diagnosticItems)
	}
}

func TestPhysicalEntryOpenRejectsPackageSwappedToSymlink(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "demo")
	if err := os.Mkdir(packageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(packageRoot, "SKILL.md"), "demo", ModeShared, "inside", nil, 0, "")
	listing, err := listPhysicalEntries(context.Background(), SourceFS{Source: SourceProject, Root: root}, DefaultLimits(), nil)
	if err != nil || len(listing.entries) != 1 {
		t.Fatalf("unexpected listed entries: %#v %v", listing, err)
	}
	defer listing.close()
	outside := t.TempDir()
	writeSkillFile(t, filepath.Join(outside, "SKILL.md"), "demo", ModeShared, "outside secret", nil, 0, "")
	if err := os.RemoveAll(packageRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, packageRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if data, err := readSourceEntry(context.Background(), listing.entries[0], DefaultMaxEntryBytes); err == nil {
		t.Fatalf("entry open followed a package symlink swapped after listing: %q", data)
	}
}

func writeSkillFile(t *testing.T, filename, name string, mode Mode, body string, tools []string, history int, model string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "---\nname: %s\ndescription: %s description\n", name, name)
	if len(tools) > 0 {
		builder.WriteString("allowed_tools:\n")
		for _, toolName := range tools {
			fmt.Fprintf(&builder, "  - %s\n", toolName)
		}
	}
	fmt.Fprintf(&builder, "mode: %s\n", mode)
	if history != 0 {
		fmt.Fprintf(&builder, "history: %d\n", history)
	}
	if model != "" {
		fmt.Fprintf(&builder, "model: %s\n", model)
	}
	fmt.Fprintf(&builder, "---\n%s", body)
	if err := os.WriteFile(filename, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func definitionNames(definitions []Definition) []string {
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name
	}
	return names
}

type failingEntryOpenFS struct {
	fs.FS
	failures map[string]error
}

func (f failingEntryOpenFS) Open(name string) (fs.File, error) {
	if err := f.failures[name]; err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f.FS.Open(name)
}
