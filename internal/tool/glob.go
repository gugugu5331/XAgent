package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"xagent/internal/budget"
	"xagent/internal/safefs"
)

const (
	maxGlobResults    = 200
	maxGlobScanErrors = 32
)

var (
	errGlobResultLimit = errors.New("glob result limit reached")
	errGlobOutputLimit = errors.New("glob output limit reached")
)

type GlobTool struct {
	projectRoot string
	limits      globLimits
}

type globLimits struct {
	bytes       int64
	files       int64
	directories int64
	results     int64
	outputBytes int64
}

type globState struct {
	counter       *budget.Counter
	outputCounter *budget.Counter
	results       []string
	seen          map[string]struct{}
	scanErrors    []string
	truncated     bool
	reason        string
}

func NewGlobTool(projectRoot string) Tool {
	return &GlobTool{projectRoot: projectRoot, limits: defaultGlobLimits()}
}

func (t *GlobTool) Name() string { return "Glob" }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern in the project and active Skills' read-only package roots. Use this dedicated tool to discover allowed paths before reading or editing; matches must stay within an allowed root."
}

func (t *GlobTool) Risk() Risk { return RiskSafe }

func (t *GlobTool) Schema() Schema {
	return ObjectSchema([]string{"pattern"}, map[string]SchemaProperty{
		"pattern": StringProperty("Glob pattern. Relative patterns search the project and active Skill package roots; absolute patterns must stay in one allowed root."),
	})
}

func (t *GlobTool) Execute(ctx context.Context, input Input) Result {
	if ctx == nil || ctx.Err() != nil {
		return globFailure(input, ErrTimeout, "文件匹配已取消")
	}
	pattern, ok := stringArg(input.Arguments, "pattern")
	if !ok {
		return Failure(input, ErrInvalidArguments, "pattern 参数不能为空", true)
	}
	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return globFailure(input, errorCode(err), "文件匹配范围无效")
	}
	patterns, err := scopedGlobPatterns(scope, pattern)
	if err != nil {
		code := errorCode(err)
		if errors.Is(err, filepath.ErrBadPattern) {
			code = ErrInvalidArguments
		}
		return globFailure(input, code, "glob pattern 无效")
	}

	limits := t.limits
	execution := readExecutionFromContext(ctx)
	if execution.scanBytes != 0 || execution.scanFiles != 0 || execution.scanDirectories != 0 || execution.scanLines != 0 || execution.outputBytes != 0 {
		limits = globLimits{
			bytes:       execution.scanBytes,
			files:       execution.scanFiles,
			directories: execution.scanDirectories,
			results:     execution.scanLines,
			outputBytes: execution.outputBytes,
		}
	}
	counter, err := newFileScanCounter(fileScanLimits{
		bytes:       limits.bytes,
		files:       limits.files,
		directories: limits.directories,
		lines:       limits.results,
	})
	if err != nil {
		return globFailure(input, ErrNotFound, "文件匹配预算无效")
	}
	outputCounter, err := newReadCounter(budget.ToolInlineOutputBytes, budget.Bytes, limits.outputBytes)
	if err != nil {
		return globFailure(input, ErrNotFound, "文件匹配输出预算无效")
	}
	state := &globState{
		counter:       counter,
		outputCounter: outputCounter,
		results:       make([]string, 0, 16),
		seen:          make(map[string]struct{}),
	}

	for _, candidate := range patterns {
		if err := ctx.Err(); err != nil {
			return state.result(input, pattern, StatusError, &Error{Code: ErrTimeout, Message: "文件匹配已取消", Recoverable: true})
		}
		root, closeRoot, err := openGlobRoot(ctx, candidate.root)
		if err != nil {
			if ctx.Err() != nil {
				return state.result(input, pattern, StatusError, &Error{Code: ErrTimeout, Message: "文件匹配已取消", Recoverable: true})
			}
			state.addScanError(candidate.label, "root_open_failed")
			continue
		}
		walkBase := globWalkBase(candidate.relative)
		baseInfo, statErr := os.Lstat(filepath.Join(candidate.root, filepath.FromSlash(walkBase)))
		if os.IsNotExist(statErr) {
			closeRoot()
			continue
		}
		if statErr != nil || !baseInfo.IsDir() {
			closeRoot()
			state.addScanError(candidate.label, "walk_root_failed")
			continue
		}
		scanErr := root.Walk(ctx, walkBase, counter, func(entry safefs.Entry) error {
			return state.visit(scope, candidate, entry)
		})
		closeRoot()
		if scanErr == nil {
			continue
		}
		switch {
		case errors.Is(scanErr, context.Canceled), errors.Is(scanErr, context.DeadlineExceeded), ctx.Err() != nil:
			return state.result(input, pattern, StatusError, &Error{Code: ErrTimeout, Message: "文件匹配已取消", Recoverable: true})
		case errors.Is(scanErr, errGlobResultLimit), errors.Is(scanErr, errGlobOutputLimit):
			return state.result(input, pattern, StatusSuccess, nil)
		default:
			var limitErr *budget.LimitError
			if errors.As(scanErr, &limitErr) {
				state.truncated = true
				state.reason = fileScanBudgetReason(limitErr.Dimension)
				return state.result(input, pattern, StatusSuccess, nil)
			}
			state.addScanError(candidate.label, "walk_failed")
		}
	}
	return state.result(input, pattern, StatusSuccess, nil)
}

type scopedGlobPattern struct {
	root     string
	relative string
	project  bool
	label    string
}

func scopedGlobPatterns(scope ReadScope, pattern string) ([]scopedGlobPattern, error) {
	cleaned := filepath.Clean(pattern)
	if _, err := filepath.Match(cleaned, cleaned); err != nil {
		return nil, err
	}
	if filepath.IsAbs(cleaned) {
		canonical, err := canonicalGlobPattern(cleaned)
		if err != nil {
			return nil, err
		}
		root, project, ok := readRootForPath(scope, canonical)
		if !ok {
			return nil, fmt.Errorf("%s: glob pattern 位于允许的只读根外", ErrPathOutsideProject)
		}
		relative, err := filepath.Rel(root, canonical)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New(ErrPathOutsideProject)
		}
		return []scopedGlobPattern{{
			root:     root,
			relative: filepath.ToSlash(relative),
			project:  project,
			label:    displayReadPath(scope, root, root, project),
		}}, nil
	}

	patterns := make([]scopedGlobPattern, 0, len(scope.ExtraRoots)+1)
	for _, candidateRoot := range readRoots(scope) {
		canonical, err := canonicalGlobPattern(filepath.Join(candidateRoot.path, cleaned))
		if err != nil {
			return nil, err
		}
		if !isPathInsideRoot(candidateRoot.path, canonical) {
			return nil, fmt.Errorf("%s: glob pattern 位于允许的只读根外", ErrPathOutsideProject)
		}
		relative, err := filepath.Rel(candidateRoot.path, canonical)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New(ErrPathOutsideProject)
		}
		patterns = append(patterns, scopedGlobPattern{
			root:     candidateRoot.path,
			relative: filepath.ToSlash(relative),
			project:  candidateRoot.project,
			label:    displayReadPath(scope, candidateRoot.path, candidateRoot.path, candidateRoot.project),
		})
	}
	return patterns, nil
}

func canonicalGlobPattern(pattern string) (string, error) {
	meta := strings.IndexAny(pattern, "*?[")
	if meta < 0 {
		return resolveWithExistingAncestor(pattern)
	}
	separator := strings.LastIndex(pattern[:meta], string(os.PathSeparator))
	if separator < 0 {
		return "", fmt.Errorf("%s: 绝对 glob pattern 缺少根目录", ErrInvalidArguments)
	}
	staticDir := pattern[:separator+1]
	remainder := pattern[separator+1:]
	resolvedDir, err := resolveWithExistingAncestor(staticDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDir, remainder), nil
}

func globWalkBase(relativePattern string) string {
	native := filepath.FromSlash(relativePattern)
	meta := strings.IndexAny(native, "*?[")
	if meta < 0 {
		base := filepath.Dir(native)
		if base == "" {
			return "."
		}
		return filepath.ToSlash(base)
	}
	separator := strings.LastIndex(native[:meta], string(filepath.Separator))
	if separator < 0 {
		return "."
	}
	base := native[:separator]
	if base == "" {
		return "."
	}
	return filepath.ToSlash(base)
}

func openGlobRoot(ctx context.Context, rootPath string) (*safefs.Root, func(), error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, func() {}, context.Canceled
	}
	execution := readExecutionFromContext(ctx)
	if execution.root != nil && execution.rootPath == rootPath {
		return execution.root, func() {}, nil
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		return nil, func() {}, errors.New("glob root is unavailable")
	}
	return opened.Root, func() { _ = opened.Root.Close() }, nil
}

func (s *globState) visit(scope ReadScope, pattern scopedGlobPattern, entry safefs.Entry) error {
	// Entry.Path is user-controlled traversal metadata. Reserve its bytes
	// before matching or copying it into any result state.
	if err := consumeGlobBytes(s.counter, int64(len(entry.Path))); err != nil {
		return err
	}
	if entry.IsDir() || !entry.Mode.IsRegular() {
		return nil
	}
	matched, err := filepath.Match(filepath.FromSlash(pattern.relative), filepath.FromSlash(entry.Path))
	if err != nil {
		return err
	}
	if !matched {
		return nil
	}
	absolute := filepath.Join(pattern.root, filepath.FromSlash(entry.Path))
	if _, exists := s.seen[absolute]; exists {
		return nil
	}
	if len(s.results) >= maxGlobResults {
		s.truncated = true
		s.reason = "glob.max_results"
		return errGlobResultLimit
	}
	if err := s.counter.Consume(budget.Lines, 1); err != nil {
		return mapFileScanLimit(err, budget.FilesScanMaxLines)
	}
	display := displayReadPath(scope, pattern.root, absolute, pattern.project)
	separator := int64(0)
	if len(s.results) > 0 {
		separator = 1
	}
	needed := int64(len(display)) + separator
	if needed > s.outputCounter.Remaining(budget.Bytes) {
		s.truncated = true
		s.reason = string(budget.ToolInlineOutputBytes)
		return errGlobOutputLimit
	}
	if err := s.outputCounter.Consume(budget.Bytes, needed); err != nil {
		return err
	}
	s.seen[absolute] = struct{}{}
	s.results = append(s.results, display)
	return nil
}

func consumeGlobBytes(counter *budget.Counter, amount int64) error {
	if amount <= 0 {
		return nil
	}
	if err := counter.Consume(budget.Bytes, amount); err == nil {
		return nil
	}
	used := counter.Snapshot().Used(budget.Bytes)
	remaining := counter.Remaining(budget.Bytes)
	if remaining > 0 {
		if err := counter.Consume(budget.Bytes, remaining); err != nil {
			return errors.New("glob byte budget failed")
		}
	}
	return &budget.LimitError{
		Scope:     string(budget.FilesScanMaxBytes),
		Dimension: budget.Bytes,
		Limit:     used + remaining,
		Observed:  used + amount,
	}
}

func (s *globState) addScanError(display, code string) {
	s.truncated = true
	if s.reason == "" {
		s.reason = "files.scan_errors"
	}
	if len(s.scanErrors) >= maxGlobScanErrors {
		return
	}
	s.scanErrors = append(s.scanErrors, display+":"+code)
}

func (s *globState) result(input Input, pattern string, status ResultStatus, resultErr *Error) Result {
	sort.Strings(s.results)
	snapshot := s.counter.Snapshot()
	files := append([]string(nil), s.results...)
	data := map[string]any{
		"pattern":       pattern,
		"count":         len(files),
		"files":         files,
		"scanned_files": snapshot.Used(budget.Files),
		"directories":   snapshot.Used(budget.Directories),
		"bytes":         snapshot.Used(budget.Bytes),
		"results":       snapshot.Used(budget.Lines),
	}
	if len(s.scanErrors) > 0 {
		data["scan_errors"] = append([]string(nil), s.scanErrors...)
	}
	if s.reason != "" {
		data["truncation_reason"] = s.reason
	}
	return Result{
		CallID:    input.CallID,
		Name:      input.Name,
		Status:    status,
		Summary:   fmt.Sprintf("Found %d files", len(files)),
		Content:   joinLines(files),
		Data:      data,
		Error:     resultErr,
		Truncated: s.truncated,
	}
}

func defaultGlobLimits() globLimits {
	return globLimits{
		bytes:       defaultBudgetValue(budget.FilesScanMaxBytes),
		files:       defaultBudgetValue(budget.FilesScanMaxFiles),
		directories: defaultBudgetValue(budget.FilesScanMaxDirectories),
		results:     defaultBudgetValue(budget.FilesScanMaxLines),
		outputBytes: defaultBudgetValue(budget.ToolInlineOutputBytes),
	}
}

func globFailure(input Input, code, message string) Result {
	return Failure(input, code, message, true)
}
