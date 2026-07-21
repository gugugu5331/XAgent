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
	return "Search text in the project or an active Skill's read-only package root by plain text or regular expression. Use this dedicated tool to inspect content before editing; searched paths must stay within an allowed root."
}

func (t *GrepTool) Risk() Risk { return RiskSafe }

func (t *GrepTool) Schema() Schema {
	return ObjectSchema([]string{"pattern"}, map[string]SchemaProperty{
		"pattern": StringProperty("Text or regex pattern to search for."),
		"path":    StringProperty("Optional project-relative path or absolute path inside an active Skill's allowed package root."),
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
	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("读取范围无效: %v", err), true)
	}
	root, err := resolveReadPath(scope, searchPath)
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
	walkErr := walkGrepFiles(ctx, root.root, root.absolute, func(path string) error {
		if len(matches) >= 200 {
			return errGrepLimit
		}
		return grepFile(path, scope, pattern, re, useRegex, &matches)
	})
	if walkErr != nil && walkErr != errGrepLimit {
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

var errGrepLimit = fmt.Errorf("grep result limit reached")

func walkGrepFiles(ctx context.Context, root string, path string, visit func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return visit(path)
	}
	entries, err := readDirNoFollow(root, path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		child := filepath.Join(path, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			if err := visit(child); err != nil {
				return err
			}
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}
		if entryInfo.IsDir() {
			if err := walkGrepFiles(ctx, root, child, visit); err != nil {
				return err
			}
			continue
		}
		if err := visit(child); err != nil {
			return err
		}
	}
	return nil
}

func grepFile(path string, scope ReadScope, pattern string, re *regexp.Regexp, useRegex bool, matches *[]string) error {
	target, fileContent, err := readFileInScope(scope, path)
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
			*matches = append(*matches, fmt.Sprintf("%s:%d:%s", target.display, lineNumber, strings.TrimSpace(line)))
			if len(*matches) >= 200 {
				return nil
			}
		}
	}
	return nil
}
