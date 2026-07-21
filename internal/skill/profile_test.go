package skill

import (
	"reflect"
	"testing"
)

func TestBuildProfileUnrestricted(t *testing.T) {
	request := ProfileRequest{
		Snapshot:     Snapshot{Catalog: []CatalogItem{{Name: "demo", Description: "demo", Mode: ModeShared}}},
		Activity:     ActivitySnapshot{ReadRoots: []string{"/z", "/a", "/a"}},
		DefaultModel: "default-model",
		BaseTools:    []string{"Read", "Bash", LoadSkillToolName},
	}
	profile, err := BuildProfile(request)
	if err != nil {
		t.Fatal(err)
	}
	if profile.AllowedTools != nil || profile.Model != "default-model" || !profile.Persist || !profile.UpdateMemory {
		t.Fatalf("unexpected unrestricted profile: %#v", profile)
	}
	if !reflect.DeepEqual(profile.ReadRoots, []string{"/a", "/z"}) {
		t.Fatalf("read roots were not normalized: %#v", profile.ReadRoots)
	}
	request.Snapshot.Catalog[0].Name = "mutated"
	request.Activity.ReadRoots[0] = "mutated"
	if profile.Catalog[0].Name != "demo" || profile.ReadRoots[0] != "/a" {
		t.Fatal("profile aliases request data")
	}
}

func TestBuildProfileWhitelistAndPlanIntersection(t *testing.T) {
	base := []string{"Read", "Glob", "Grep", "Bash", "Write", LoadSkillToolName}
	profile, err := BuildProfile(ProfileRequest{
		Activity:     ActivitySnapshot{AllowedTools: []string{"Read", "Bash", "Unknown"}, Model: "skill-model"},
		DefaultModel: "default", BaseTools: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertToolSet(t, profile.AllowedTools, "Read", "Bash", LoadSkillToolName)
	if profile.Model != "skill-model" {
		t.Fatalf("skill model did not override default: %#v", profile)
	}

	plan, err := BuildProfile(ProfileRequest{
		Activity:  ActivitySnapshot{AllowedTools: []string{"Read", "Bash", "Unknown"}},
		BaseTools: base, ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertToolSet(t, plan.AllowedTools, "Read", LoadSkillToolName)

	readOnlyUnrestricted, err := BuildProfile(ProfileRequest{BaseTools: base, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	assertToolSet(t, readOnlyUnrestricted.AllowedTools, "Read", "Glob", "Grep", LoadSkillToolName)
}

func TestBuildProfileEmptyIntersectionAndSystemTool(t *testing.T) {
	profile, err := BuildProfile(ProfileRequest{
		Activity:  ActivitySnapshot{AllowedTools: []string{}},
		BaseTools: []string{"Read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertToolSet(t, profile.AllowedTools, LoadSkillToolName)

	isolated, err := BuildProfile(ProfileRequest{
		Snapshot:         Snapshot{Catalog: []CatalogItem{{Name: "one"}}},
		Activity:         ActivitySnapshot{AllowedTools: []string{"Read"}},
		BaseTools:        []string{"Read", LoadSkillToolName},
		IndependentDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isolated.IndependentDepth != 1 || isolated.Persist || isolated.UpdateMemory {
		t.Fatalf("isolated side effects were not disabled: %#v", isolated)
	}
	clone := isolated.Clone()
	delete(clone.AllowedTools, "Read")
	clone.Catalog[0].Name = "mutated"
	if _, exists := isolated.AllowedTools["Read"]; !exists || isolated.Catalog[0].Name != "one" {
		t.Fatal("profile clone aliases mutable state")
	}
}

func TestBuildProfileRejectsInvalidInput(t *testing.T) {
	if _, err := BuildProfile(ProfileRequest{IndependentDepth: -1}); err == nil {
		t.Fatal("expected negative depth error")
	}
	if _, err := BuildProfile(ProfileRequest{BaseTools: []string{"Read", " "}}); err == nil {
		t.Fatal("expected empty base tool error")
	}
}

func assertToolSet(t *testing.T, actual map[string]struct{}, expected ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		want[name] = struct{}{}
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("unexpected tool set:\nwant %#v\n got %#v", want, actual)
	}
}
