package tool

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GrepTool struct {
	projectRoot string
}

func NewGrepTool(projectRoot string) Tool {
	return &GrepTool{projectRoot: projectRoot}
}

func (t *GrepTool) Name() string { return "Grep" }

func (t *GrepTool) Description() string {
	return "Search text content in project files by plain text or regular expression. Use this dedicated tool to inspect project content before editing; searched paths must stay within the project."
}

func (t *GrepTool) Risk() Risk { return RiskSafe }

func (t *GrepTool) Schema() Schema {
	return ObjectSchema([]string{"pattern"}, map[string]SchemaProperty{
		"pattern": StringProperty("Text or regex pattern to search for."),
		"path":    StringProperty("Optional file or directory path relative to project root."),
		"regex":   BoolProperty("Whether pattern is a regular expression."),
	})
}

func (t *GrepTool) Execute(ctx context.Context, input Input) Result {
	pattern, ok := stringArg(input.Arguments, "pattern")
	if !ok {
		return Failure(input, ErrInvalidArguments, "pattern 参数不能为空", true)
	}
	searchPath := optionalStringArg(input.Arguments, "path")
	if searchPath == "" {
		searchPath = "."
	}
	root, err := ResolveProjectPath(t.projectRoot, searchPath)
	if err != nil {
		return Failure(input, errorCode(err), err.Error(), true)
	}
	useRegex := boolArg(input.Arguments, "regex")
	var re *regexp.Regexp
	if useRegex {
		re, err = regexp.Compile(pattern)
		if err != nil {
			return Failure(input, ErrInvalidArguments, fmt.Sprintf("正则表达式无效: %v", err), true)
		}
	}

	matches := []string{}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if len(matches) >= 200 {
			return filepath.SkipAll
		}
		return grepFile(path, t.projectRoot, pattern, re, useRegex, &matches)
	})
	if walkErr != nil {
		return Failure(input, ErrNotFound, fmt.Sprintf("搜索失败: %v", walkErr), true)
	}
	status := StatusSuccess
	if len(matches) == 0 {
		status = StatusError
	}
	result := Result{CallID: input.CallID, Name: input.Name, Status: status, Summary: fmt.Sprintf("Found %d matches", len(matches)), Content: joinLines(matches), Data: map[string]any{"pattern": pattern, "count": len(matches), "matches": matches}}
	if len(matches) == 0 {
		result.Error = &Error{Code: ErrNoResults, Message: "没有搜索到匹配内容", Recoverable: true}
	}
	return result
}

func grepFile(path string, projectRoot string, pattern string, re *regexp.Regexp, useRegex bool, matches *[]string) error {
	resolved, fileContent, err := ReadProjectFile(projectRoot, path)
	if err != nil {
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(fileContent)))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		matched := strings.Contains(line, pattern)
		if useRegex {
			matched = re.MatchString(line)
		}
		if matched {
			rel := RelativeToRoot(projectRoot, resolved)
			*matches = append(*matches, fmt.Sprintf("%s:%d:%s", rel, lineNumber, strings.TrimSpace(line)))
			if len(*matches) >= 200 {
				return nil
			}
		}
	}
	return nil
}
