package repoaudit

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrivatePathsAreRejectedWithoutContent(t *testing.T) {
	t.Parallel()

	const contentCanary = "PRIVATE-CONTENT-MUST-NOT-BE-READ-OR-REPORTED"
	entries := []Entry{
		{Path: ".claude/worktrees/agent-a/private.txt", Type: "blob"},
		{Path: ".mewcode/memory/MEMORY.md", Type: "blob"},
		{Path: ".mewcode/sessions/session.jsonl", Type: "blob"},
		{Path: "fakeprovider", Type: "blob"},
		{Path: "capture.webarchive", Type: "blob"},
		{Path: "attachments/capture.webarchive", Type: "blob"},
		{Path: "photo_001.jpg", Type: "blob"},
		{Path: "photo_002.jpeg", Type: "blob"},
		{Path: "photo_003.png", Type: "blob"},
		{Path: "internal/tool/tool.go", Type: "blob", OID: contentCanary},
		{Path: "fixtures/photo_allowed.jpg", Type: "blob"},
	}

	findings := privatePathFindings(entries)
	wantPaths := []string{
		".claude/worktrees/agent-a/private.txt",
		".mewcode/memory/MEMORY.md",
		".mewcode/sessions/session.jsonl",
		"fakeprovider",
		"capture.webarchive",
		"attachments/capture.webarchive",
		"photo_001.jpg",
		"photo_002.jpeg",
		"photo_003.png",
	}
	gotPaths := make([]string, 0, len(findings))
	for _, finding := range findings {
		gotPaths = append(gotPaths, finding.Path)
		if finding.RuleID != RulePrivatePath || finding.Severity != SeverityError || finding.Line != 0 || finding.Message != privatePathMessage {
			t.Errorf("unexpected private-path finding: %#v", finding)
		}
		if strings.Contains(finding.Message, contentCanary) {
			t.Fatal("private-path finding retained file content")
		}
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("private paths = %#v, want %#v", gotPaths, wantPaths)
	}
}
