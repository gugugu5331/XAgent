package repoaudit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	maxGitmodulesBytes    = 1 << 20
	gitlinkFindingMessage = "gitlink is not explicitly declared with a matching .gitmodules path"
)

// gitlinkFindings requires the actual gitlink set, policy declarations and
// .gitmodules paths to be identical. It is pure and never creates a mapping.
func gitlinkFindings(entries []Entry, gitmodules []byte, gitmodulesPresent bool, policy Policy) ([]Finding, error) {
	actual := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Mode == 0160000 || entry.Type == "commit" {
			actual[entry.Path] = struct{}{}
		}
	}
	declared := make(map[string]struct{}, len(policy.Gitlinks))
	for _, declaration := range policy.Gitlinks {
		declared[declaration.Path] = struct{}{}
	}
	mapped := make(map[string]struct{})
	if gitmodulesPresent {
		var err error
		mapped, err = parseGitmodulesPaths(gitmodules)
		if err != nil {
			return nil, err
		}
	}

	violations := make(map[string]struct{})
	for name := range actual {
		if _, ok := declared[name]; !ok {
			violations[name] = struct{}{}
		}
		if _, ok := mapped[name]; !ok {
			violations[name] = struct{}{}
		}
	}
	for name := range declared {
		if _, ok := actual[name]; !ok {
			violations[name] = struct{}{}
		}
		if _, ok := mapped[name]; !ok {
			violations[name] = struct{}{}
		}
	}
	for name := range mapped {
		if _, ok := actual[name]; !ok {
			violations[name] = struct{}{}
		}
		if _, ok := declared[name]; !ok {
			violations[name] = struct{}{}
		}
	}
	paths := make([]string, 0, len(violations))
	for name := range violations {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	findings := make([]Finding, 0, len(paths))
	for _, name := range paths {
		findings = append(findings, Finding{
			RuleID:   RuleGitlink,
			Path:     name,
			Severity: SeverityError,
			Message:  gitlinkFindingMessage,
		})
	}
	return findings, nil
}

func parseGitmodulesPaths(data []byte) (map[string]struct{}, error) {
	if len(data) > maxGitmodulesBytes {
		return nil, fmt.Errorf(".gitmodules exceeds %d bytes", maxGitmodulesBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New(".gitmodules contains NUL")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxGitmodulesBytes)
	paths := make(map[string]struct{})
	sections := make(map[string]struct{})
	currentSection := ""
	currentPath := ""
	commitSection := func() error {
		if currentSection == "" {
			return nil
		}
		if currentPath == "" {
			return errors.New(".gitmodules submodule section has no path")
		}
		if _, exists := paths[currentPath]; exists {
			return errors.New(".gitmodules contains a duplicate path")
		}
		paths[currentPath] = struct{}{}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if err := commitSection(); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(line, "]") {
				return nil, errors.New(".gitmodules contains an invalid section")
			}
			section := strings.TrimSpace(line[1 : len(line)-1])
			if !strings.HasPrefix(section, `submodule "`) || !strings.HasSuffix(section, `"`) || len(section) <= len(`submodule ""`) {
				return nil, errors.New(".gitmodules contains a non-submodule section")
			}
			if _, exists := sections[section]; exists {
				return nil, errors.New(".gitmodules contains a duplicate submodule section")
			}
			sections[section] = struct{}{}
			currentSection = section
			currentPath = ""
			continue
		}
		if currentSection == "" {
			return nil, errors.New(".gitmodules property appears outside a submodule section")
		}
		separator := strings.IndexByte(line, '=')
		if separator <= 0 {
			return nil, errors.New(".gitmodules contains an invalid property")
		}
		key := strings.ToLower(strings.TrimSpace(line[:separator]))
		value := strings.TrimSpace(line[separator+1:])
		if key != "path" {
			continue
		}
		if currentPath != "" {
			return nil, errors.New(".gitmodules submodule section contains duplicate path properties")
		}
		if err := validateRepoPath(value); err != nil {
			return nil, errors.New(".gitmodules contains an invalid submodule path")
		}
		currentPath = value
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("read .gitmodules failed")
	}
	if err := commitSection(); err != nil {
		return nil, err
	}
	return paths, nil
}
