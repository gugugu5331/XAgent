package tool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"xagent/internal/budget"
	"xagent/internal/safefs"
)

const (
	maxGrepMatches    = 200
	maxGrepScanErrors = 32
)

var (
	errGrepMatchLimit  = errors.New("grep match limit reached")
	errGrepOutputLimit = errors.New("grep output limit reached")
)

type grepOpenFile func(context.Context, *safefs.Root, string) (io.ReadCloser, error)

type GrepTool struct {
	projectRoot string
	limits      grepLimits
	openFile    grepOpenFile
}

type grepLimits struct {
	bytes       int64
	files       int64
	directories int64
	lines       int64
	outputBytes int64
}

type grepState struct {
	pattern       string
	re            *regexp.Regexp
	useRegex      bool
	counter       *budget.Counter
	outputCounter *budget.Counter
	lineMaxBytes  int64
	matches       []string
	scanErrors    []string
	truncated     bool
	reason        string
}

func NewGrepTool(projectRoot string) Tool {
	return &GrepTool{projectRoot: projectRoot, limits: defaultGrepLimits()}
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
	if ctx == nil || ctx.Err() != nil {
		return grepFailure(input, ErrTimeout, "搜索已取消")
	}
	pattern, ok := stringArg(input.Arguments, "pattern")
	if !ok {
		return Failure(input, ErrInvalidArguments, "pattern 参数不能为空", true)
	}
	searchPath := optionalStringArg(input.Arguments, "path")
	if searchPath == "" {
		searchPath = "."
	}
	useRegex := boolArg(input.Arguments, "regex")
	var re *regexp.Regexp
	var err error
	if useRegex {
		re, err = regexp.Compile(pattern)
		if err != nil {
			return Failure(input, ErrInvalidArguments, "正则表达式无效", true)
		}
	}

	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return grepFailure(input, errorCode(err), "搜索范围无效")
	}
	target, root, closeRoot, err := readRootInScope(ctx, scope, searchPath)
	if err != nil {
		if ctx.Err() != nil {
			return grepFailure(input, ErrTimeout, "搜索已取消")
		}
		return grepFailure(input, errorCode(err), "无法打开搜索范围")
	}
	defer closeRoot()
	relative, err := relativeReadTarget(target, true)
	if err != nil {
		return grepFailure(input, errorCode(err), "搜索范围无效")
	}

	limits := t.limits
	execution := readExecutionFromContext(ctx)
	if execution.scanBytes != 0 || execution.scanFiles != 0 || execution.scanDirectories != 0 || execution.scanLines != 0 || execution.outputBytes != 0 {
		limits = grepLimits{
			bytes:       execution.scanBytes,
			files:       execution.scanFiles,
			directories: execution.scanDirectories,
			lines:       execution.scanLines,
			outputBytes: execution.outputBytes,
		}
	}
	counter, err := newGrepCounter(limits)
	if err != nil {
		return grepFailure(input, ErrNotFound, "搜索预算无效")
	}
	outputCounter, err := newReadCounter(budget.ToolInlineOutputBytes, budget.Bytes, limits.outputBytes)
	if err != nil {
		return grepFailure(input, ErrNotFound, "搜索输出预算无效")
	}
	state := &grepState{
		pattern:       pattern,
		re:            re,
		useRegex:      useRegex,
		counter:       counter,
		outputCounter: outputCounter,
		lineMaxBytes:  limits.outputBytes,
		matches:       make([]string, 0, 16),
	}

	info, statErr := os.Lstat(target.absolute)
	var scanErr error
	if statErr != nil {
		state.addScanError(target.display, "stat_failed")
	} else if info.IsDir() {
		scanErr = root.Walk(ctx, relative, counter, func(entry safefs.Entry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry.IsDir() {
				return nil
			}
			entryTarget := resolvedReadTarget(scope, target.root, filepath.Join(target.root, filepath.FromSlash(entry.Path)), target.project)
			return t.scanFile(ctx, root, entry.Path, entryTarget.display, state)
		})
	} else {
		if err := counter.Consume(budget.Files, 1); err != nil {
			scanErr = err
		} else {
			scanErr = t.scanFile(ctx, root, relative, target.display, state)
		}
	}

	if scanErr != nil {
		switch {
		case errors.Is(scanErr, context.Canceled), errors.Is(scanErr, context.DeadlineExceeded), ctx.Err() != nil:
			return state.result(input, pattern, StatusError, &Error{Code: ErrTimeout, Message: "搜索已取消", Recoverable: true})
		case errors.Is(scanErr, errGrepMatchLimit), errors.Is(scanErr, errGrepOutputLimit):
			// The state already records the exact bounded reason.
		default:
			var limitErr *budget.LimitError
			if errors.As(scanErr, &limitErr) {
				state.truncated = true
				state.reason = grepBudgetReason(limitErr.Dimension)
			} else {
				state.addScanError(target.display, "walk_failed")
			}
		}
	}
	if len(state.matches) == 0 {
		return state.result(input, pattern, StatusError, &Error{Code: ErrNoResults, Message: "没有搜索到匹配内容", Recoverable: true})
	}
	return state.result(input, pattern, StatusSuccess, nil)
}

func (t *GrepTool) scanFile(ctx context.Context, root *safefs.Root, relative, display string, state *grepState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opener := t.openFile
	if opener == nil {
		opener = func(ctx context.Context, root *safefs.Root, relative string) (io.ReadCloser, error) {
			return root.OpenRead(ctx, relative)
		}
	}
	file, err := opener(ctx, root, relative)
	if ctx.Err() != nil {
		if file != nil {
			_ = file.Close()
		}
		return ctx.Err()
	}
	if err != nil || file == nil {
		if file != nil {
			_ = file.Close()
		}
		state.addScanError(display, "open_failed")
		return nil
	}
	scanErr := scanGrepReader(ctx, file, display, state)
	closeErr := file.Close()
	if scanErr != nil {
		if errors.Is(scanErr, errGrepMatchLimit) || errors.Is(scanErr, errGrepOutputLimit) || errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
			return scanErr
		}
		var limitErr *budget.LimitError
		if errors.As(scanErr, &limitErr) {
			return scanErr
		}
		state.addScanError(display, "read_failed")
		return nil
	}
	if closeErr != nil {
		state.addScanError(display, "close_failed")
	}
	return nil
}

func scanGrepReader(ctx context.Context, input io.Reader, display string, state *grepState) error {
	reader := bufio.NewReaderSize(grepContextReader{ctx: ctx, reader: input}, readChunkBytes)
	initialCapacity := state.lineMaxBytes
	if initialCapacity > 4*1024 {
		initialCapacity = 4 * 1024
	}
	line := make([]byte, 0, int(initialCapacity))
	lineNumber := int64(0)
	lineStarted := false
	lineTooLong := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fragment, readErr := reader.ReadSlice('\n')
		accepted, limitErr := consumeGrepBytes(state.counter, fragment)
		if len(accepted) > 0 {
			if !lineStarted {
				if err := state.counter.Consume(budget.Lines, 1); err != nil {
					return mapGrepLimit(err, budget.FilesScanMaxLines)
				}
				lineNumber++
				lineStarted = true
			}
			content := accepted
			if content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}
			remaining := state.lineMaxBytes - int64(len(line))
			if int64(len(content)) > remaining {
				if remaining > 0 {
					line = append(line, content[:int(remaining)]...)
				}
				lineTooLong = true
			} else if !lineTooLong {
				line = append(line, content...)
			}
		}

		lineComplete := len(accepted) > 0 && accepted[len(accepted)-1] == '\n'
		if errors.Is(readErr, io.EOF) && lineStarted {
			lineComplete = true
		}
		if lineComplete {
			if lineTooLong {
				state.addScanError(fmt.Sprintf("%s:%d", display, lineNumber), "line_too_long")
			} else if state.matchesLine(line) {
				if err := state.addMatch(display, lineNumber, line); err != nil {
					return err
				}
			}
			line = line[:0]
			lineStarted = false
			lineTooLong = false
		}
		if limitErr != nil {
			return limitErr
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if errors.Is(readErr, bufio.ErrBufferFull) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("grep reader failed")
		}
	}
}

type grepContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r grepContextReader) Read(destination []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(destination)
}

func consumeGrepBytes(counter *budget.Counter, input []byte) ([]byte, error) {
	amount := int64(len(input))
	if amount == 0 {
		return nil, nil
	}
	if err := counter.Consume(budget.Bytes, amount); err == nil {
		return input, nil
	} else {
		used := counter.Snapshot().Used(budget.Bytes)
		remaining := counter.Remaining(budget.Bytes)
		if remaining > 0 {
			if consumeErr := counter.Consume(budget.Bytes, remaining); consumeErr != nil {
				return nil, errors.New("grep byte budget failed")
			}
		}
		return input[:int(remaining)], &budget.LimitError{
			Scope:     string(budget.FilesScanMaxBytes),
			Dimension: budget.Bytes,
			Limit:     used + remaining,
			Observed:  used + amount,
		}
	}
}

func (s *grepState) matchesLine(line []byte) bool {
	if s.useRegex {
		return s.re.Match(line)
	}
	return strings.Contains(string(line), s.pattern)
}

func (s *grepState) addMatch(display string, lineNumber int64, line []byte) error {
	match := fmt.Sprintf("%s:%d:%s", display, lineNumber, strings.TrimSpace(string(line)))
	separator := int64(0)
	if len(s.matches) > 0 {
		separator = 1
	}
	needed := int64(len(match)) + separator
	remaining := s.outputCounter.Remaining(budget.Bytes)
	if needed > remaining {
		if remaining > separator {
			allowed := remaining - separator
			match = boundedUTF8Preview([]byte(match), int(allowed))
			if separator > 0 {
				_ = s.outputCounter.Consume(budget.Bytes, separator)
			}
			_ = s.outputCounter.Consume(budget.Bytes, int64(len(match)))
			s.matches = append(s.matches, match)
		}
		s.truncated = true
		s.reason = string(budget.ToolInlineOutputBytes)
		return errGrepOutputLimit
	}
	if err := s.outputCounter.Consume(budget.Bytes, needed); err != nil {
		return err
	}
	s.matches = append(s.matches, match)
	if len(s.matches) >= maxGrepMatches {
		s.truncated = true
		s.reason = "grep.max_matches"
		return errGrepMatchLimit
	}
	return nil
}

func (s *grepState) addScanError(display, code string) {
	s.truncated = true
	if s.reason == "" {
		s.reason = "files.scan_errors"
	}
	if len(s.scanErrors) >= maxGrepScanErrors {
		return
	}
	s.scanErrors = append(s.scanErrors, display+":"+code)
}

func (s *grepState) result(input Input, pattern string, status ResultStatus, resultErr *Error) Result {
	snapshot := s.counter.Snapshot()
	data := map[string]any{
		"pattern":     pattern,
		"count":       len(s.matches),
		"matches":     append([]string(nil), s.matches...),
		"files":       snapshot.Used(budget.Files),
		"directories": snapshot.Used(budget.Directories),
		"bytes":       snapshot.Used(budget.Bytes),
		"lines":       snapshot.Used(budget.Lines),
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
		Summary:   fmt.Sprintf("Found %d matches", len(s.matches)),
		Content:   joinLines(s.matches),
		Data:      data,
		Error:     resultErr,
		Truncated: s.truncated,
	}
}

func newGrepCounter(limits grepLimits) (*budget.Counter, error) {
	scopes := []struct {
		scope     budget.Scope
		dimension budget.Dimension
		value     int64
	}{
		{budget.FilesScanMaxBytes, budget.Bytes, limits.bytes},
		{budget.FilesScanMaxFiles, budget.Files, limits.files},
		{budget.FilesScanMaxDirectories, budget.Directories, limits.directories},
		{budget.FilesScanMaxLines, budget.Lines, limits.lines},
	}
	effectiveEntries := make([]budget.Limit, 0, len(scopes))
	hardEntries := make([]budget.Limit, 0, len(scopes))
	for _, item := range scopes {
		spec, ok := budgetSpec(item.scope)
		if !ok || spec.Dimension != item.dimension {
			return nil, errors.New("grep budget specification is unavailable")
		}
		resolved, err := spec.Resolve(&item.value)
		if err != nil {
			return nil, errors.New("grep budget is invalid")
		}
		effectiveEntries = append(effectiveEntries, budget.Limit{Dimension: item.dimension, Value: resolved})
		hardEntries = append(hardEntries, budget.Limit{Dimension: item.dimension, Value: spec.HardCap})
	}
	effective, err := budget.NewLimits(effectiveEntries...)
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(hardEntries...)
	if err != nil {
		return nil, err
	}
	return budget.NewCounter(effective, hard)
}

func defaultGrepLimits() grepLimits {
	return grepLimits{
		bytes:       defaultBudgetValue(budget.FilesScanMaxBytes),
		files:       defaultBudgetValue(budget.FilesScanMaxFiles),
		directories: defaultBudgetValue(budget.FilesScanMaxDirectories),
		lines:       defaultBudgetValue(budget.FilesScanMaxLines),
		outputBytes: defaultBudgetValue(budget.ToolInlineOutputBytes),
	}
}

func budgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func mapGrepLimit(err error, scope budget.Scope) error {
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return err
	}
	return &budget.LimitError{Scope: string(scope), Dimension: limitErr.Dimension, Limit: limitErr.Limit, Observed: limitErr.Observed}
}

func grepBudgetReason(dimension budget.Dimension) string {
	switch dimension {
	case budget.Bytes:
		return string(budget.FilesScanMaxBytes)
	case budget.Files:
		return string(budget.FilesScanMaxFiles)
	case budget.Directories:
		return string(budget.FilesScanMaxDirectories)
	case budget.Lines:
		return string(budget.FilesScanMaxLines)
	default:
		return "files.scan_limit"
	}
}

func grepFailure(input Input, code, message string) Result {
	return Failure(input, code, message, true)
}
