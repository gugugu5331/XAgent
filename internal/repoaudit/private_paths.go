package repoaudit

import (
	"path"
	"strings"
)

const privatePathMessage = "repository path is reserved for local private data"

var privateDirectoryPrefixes = [...]string{
	".claude/worktrees/",
	".mewcode/memory/",
	".mewcode/sessions/",
}

// privatePathFindings checks entry names only. It deliberately accepts no
// content reader, so this rule cannot retain or disclose file bytes.
func privatePathFindings(entries []Entry) []Finding {
	findings := make([]Finding, 0)
	for _, entry := range entries {
		if !isPrivateRepositoryPath(entry.Path) {
			continue
		}
		findings = append(findings, Finding{
			RuleID:   RulePrivatePath,
			Path:     entry.Path,
			Severity: SeverityError,
			Message:  privatePathMessage,
		})
	}
	return findings
}

func isPrivateRepositoryPath(name string) bool {
	for _, prefix := range privateDirectoryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if name == "fakeprovider" || strings.HasSuffix(name, ".webarchive") {
		return true
	}
	if strings.Contains(name, "/") || !strings.HasPrefix(name, "photo_") {
		return false
	}
	switch path.Ext(name) {
	case ".jpg", ".jpeg", ".png":
		return true
	default:
		return false
	}
}
