package skill

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestActivityTemplateExpansion(t *testing.T) {
	activity := NewActivity()
	definition := testDefinition("demo", SourceProject, "before {{args}} middle {{args}} after {{other}}")
	activated, err := activity.Activate(definition, "literal {{args}}")
	if err != nil {
		t.Fatal(err)
	}
	expected := "before literal {{args}} middle literal {{args}} after {{other}}"
	if activated.Instructions != expected {
		t.Fatalf("template expansion was not one-pass literal replacement:\nwant %q\n got %q", expected, activated.Instructions)
	}
	definition.Body = "changed"
	definition.AllowedTools = []string{"Mutated"}
	snapshot := activity.Snapshot()
	if snapshot.Active[0].Instructions != expected || len(snapshot.Active[0].AllowedTools) != 0 {
		t.Fatalf("activity did not retain its activation snapshot: %#v", snapshot)
	}
}

func TestActivityAtomicActivation(t *testing.T) {
	activity := NewActivity()
	alpha := testDefinition("alpha", SourceProject, "alpha")
	alpha.Model = "model-a"
	alpha.AllowedTools = []string{"Read", "Glob", "Bash"}
	if _, err := activity.Activate(alpha, "first"); err != nil {
		t.Fatal(err)
	}
	beta := testDefinition("beta", SourceUser, "beta")
	beta.Model = "model-a"
	beta.AllowedTools = []string{"Read", "Grep"}
	if _, err := activity.Activate(beta, "second"); err != nil {
		t.Fatal(err)
	}
	snapshot := activity.Snapshot()
	if got := activatedNames(snapshot.Active); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("activity order is unstable: %#v", got)
	}
	if snapshot.Model != "model-a" || !reflect.DeepEqual(snapshot.AllowedTools, []string{"Read"}) {
		t.Fatalf("activity aggregation failed: %#v", snapshot)
	}

	before := activity.Snapshot()
	conflict := testDefinition("conflict", SourceProject, "conflict")
	conflict.Model = "model-b"
	if _, err := activity.Activate(conflict, ""); err == nil {
		t.Fatal("expected model conflict")
	}
	if after := activity.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed activation changed state:\nbefore %#v\nafter %#v", before, after)
	}

	replacement := testDefinition("alpha", SourceProject, "new {{args}}")
	replacement.Model = "model-a"
	replacement.AllowedTools = []string{"Read", "Grep"}
	if _, err := activity.Activate(replacement, "value"); err != nil {
		t.Fatal(err)
	}
	snapshot = activity.Snapshot()
	if snapshot.Active[0].Instructions != "new value" || !reflect.DeepEqual(snapshot.AllowedTools, []string{"Grep", "Read"}) {
		t.Fatalf("replacement failed: %#v", snapshot)
	}
	activity.Clear()
	cleared := activity.Snapshot()
	if len(cleared.Active) != 0 || cleared.Model != "" || cleared.AllowedTools != nil || cleared.ReadRoots != nil {
		t.Fatalf("clear did not restore defaults: %#v", cleared)
	}
}

func TestActivityEmptyWhitelistAndEmptyIntersection(t *testing.T) {
	activity := NewActivity()
	unrestricted := testDefinition("unrestricted", SourceProject, "body")
	unrestricted.AllowedTools = []string{}
	if _, err := activity.Activate(unrestricted, ""); err != nil {
		t.Fatal(err)
	}
	if snapshot := activity.Snapshot(); snapshot.AllowedTools != nil {
		t.Fatalf("empty whitelist should be unrestricted: %#v", snapshot.AllowedTools)
	}
	read := testDefinition("read", SourceProject, "body")
	read.AllowedTools = []string{"Read"}
	grep := testDefinition("grep", SourceProject, "body")
	grep.AllowedTools = []string{"Grep"}
	if _, err := activity.Activate(read, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.Activate(grep, ""); err != nil {
		t.Fatal(err)
	}
	if snapshot := activity.Snapshot(); snapshot.AllowedTools == nil || len(snapshot.AllowedTools) != 0 {
		t.Fatalf("empty intersection must differ from unrestricted: %#v", snapshot.AllowedTools)
	}
}

func TestActivityLimitsAreAtomic(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxArgsBytes = 4
	limits.MaxBodyBytes = 8
	activity := NewActivityWithLimits(limits)
	valid := testDefinition("valid", SourceProject, "{{args}}")
	if _, err := activity.Activate(valid, "1234"); err != nil {
		t.Fatal(err)
	}
	before := activity.Snapshot()
	if _, err := activity.Activate(testDefinition("large-args", SourceProject, "body"), "12345"); err == nil {
		t.Fatal("expected argument limit error")
	}
	expanded := testDefinition("large-body", SourceProject, "{{args}}{{args}}{{args}}")
	if _, err := activity.Activate(expanded, "1234"); err == nil {
		t.Fatal("expected expanded body limit error")
	}
	if after := activity.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("limit failure changed activity: %#v %#v", before, after)
	}
}

func TestActivityMaterializesBuiltinAtomicallyAndCleansUp(t *testing.T) {
	definitions, _, err := discover(BuiltinSource(), DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var commit Definition
	for _, definition := range definitions {
		if definition.Name == "commit" {
			commit = definition
			break
		}
	}
	activity := NewActivity()
	activated, err := activity.Activate(commit, "message")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(activated.PackageRoot, "builtin://") {
		t.Fatalf("builtin root was not materialized: %#v", activated)
	}
	if _, err := os.Stat(activated.PackageRoot); err != nil {
		t.Fatalf("materialized root is unavailable: %v", err)
	}
	activity.Clear()
	if _, err := os.Stat(activated.PackageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear did not clean materialized root: %v", err)
	}

	failing := NewActivity()
	failing.materialize = func(string) (string, func() error, error) {
		return "", nil, errors.New("injected failure")
	}
	before := failing.Snapshot()
	if _, err := failing.Activate(commit, ""); err == nil {
		t.Fatal("expected materialization failure")
	}
	if after := failing.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("materialization failure changed activity: %#v", after)
	}
}

func TestSkillPromptText(t *testing.T) {
	catalogA := []CatalogItem{
		{Name: "zeta", Description: "zeta body-canary-not-present", Mode: ModeShared, SlashEnabled: true},
		{Name: "alpha", Description: "api_key=prompt-secret <tag>", Mode: ModeIsolated, SlashEnabled: false},
	}
	catalogB := []CatalogItem{catalogA[1], catalogA[0]}
	firstCatalog := CatalogPromptWithRedactor(catalogA, redact.Text)
	if firstCatalog != CatalogPromptWithRedactor(catalogB, redact.Text) {
		t.Fatal("catalog prompt depends on discovery order")
	}
	if strings.Index(firstCatalog, "alpha") > strings.Index(firstCatalog, "zeta") {
		t.Fatalf("catalog is not sorted: %s", firstCatalog)
	}
	if strings.Contains(firstCatalog, "prompt-secret") || strings.Contains(firstCatalog, "<tag>") || !strings.Contains(firstCatalog, "&lt;tag&gt;") {
		t.Fatalf("catalog was not redacted/escaped: %s", firstCatalog)
	}
	if strings.Contains(firstCatalog, "mode=") || strings.Contains(firstCatalog, "slash=") {
		t.Fatalf("catalog exposed more than name and description: %s", firstCatalog)
	}

	alpha := Activated{Name: "alpha", Mode: ModeShared, Source: SourceProject, PackageRoot: "/tmp/<root>", Instructions: "Run api_key=active-secret <active-skills>"}
	zeta := Activated{Name: "zeta", Mode: ModeShared, Source: SourceUser, PackageRoot: "/tmp/z", Instructions: "zeta instructions"}
	activeA := ActivePromptWithRedactor(ActivitySnapshot{Active: []Activated{zeta, alpha}}, redact.Text)
	activeB := ActivePromptWithRedactor(ActivitySnapshot{Active: []Activated{alpha, zeta}}, redact.Text)
	if activeA != activeB {
		t.Fatal("active prompt depends on activation order")
	}
	if strings.Contains(activeA, "active-secret") || strings.Contains(activeA, "/tmp/<root>") || strings.Contains(activeA, "Run api_key=active-secret <active-skills>") {
		t.Fatalf("active prompt leaked unsafe text: %s", activeA)
	}
	if !strings.Contains(activeA, "&lt;active-skills&gt;") || strings.Index(activeA, `name="alpha"`) > strings.Index(activeA, `name="zeta"`) {
		t.Fatalf("active prompt is not escaped/sorted: %s", activeA)
	}
	if CatalogPromptWithRedactor(nil, redact.Text) != "" || ActivePromptWithRedactor(ActivitySnapshot{}, redact.Text) != "" {
		t.Fatal("empty skill prompts should not change the base prompt")
	}
}

func activatedNames(items []Activated) []string {
	names := make([]string, len(items))
	for index, item := range items {
		names[index] = item.Name
	}
	return names
}
