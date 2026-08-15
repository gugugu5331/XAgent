package skill

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"xagent/internal/conversation"
)

const maxSkillHistoryTurns = 1000

const completeTurnContextCheckInterval = 1024

type skillHistoryRangeViolation struct {
	maximum      int
	aboveMaximum bool
}

func (e *skillHistoryRangeViolation) Error() string {
	return fmt.Sprintf("skill metadata field history must be an integer in range 0..%d", e.maximum)
}

type skillDiagnosticIdentity struct {
	source string
	path   string
}

func validateFrontmatterHistory(document yaml.Node) (bool, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	mapping := document.Content[0]
	mode := Mode("")
	var history *yaml.Node
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		value := mapping.Content[index+1]
		switch key.Value {
		case "mode":
			if value.Kind == yaml.ScalarNode {
				mode = Mode(value.Value)
			}
		case "history":
			history = value
		}
	}
	if history == nil {
		return false, nil
	}
	maximum := maxSkillHistoryTurns
	if normalizeSkillMode(mode) == ModeShared {
		maximum = 0
	}
	if history.Kind != yaml.ScalarNode || history.Tag != "!!int" {
		return true, skillHistoryRangeError(maximum, false)
	}
	var value int64
	if err := history.Decode(&value); err != nil || value < 0 {
		return true, skillHistoryRangeError(maximum, false)
	}
	if value > int64(maximum) {
		return true, skillHistoryRangeError(maximum, true)
	}
	return true, nil
}

func validateSkillHistory(value int, mode Mode) error {
	maximum := maxSkillHistoryTurns
	if mode == ModeShared {
		maximum = 0
	}
	if value < 0 {
		return skillHistoryRangeError(maximum, false)
	}
	if value > maximum {
		return skillHistoryRangeError(maximum, true)
	}
	return nil
}

func skillHistoryRangeError(maximum int, aboveMaximum bool) error {
	return &skillHistoryRangeViolation{maximum: maximum, aboveMaximum: aboveMaximum}
}

func normalizeSkillMode(mode Mode) Mode {
	return Mode(strings.ToLower(strings.TrimSpace(string(mode))))
}

func overLimitHistoryEntries(ctx context.Context, options ManagerOptions) (map[skillDiagnosticIdentity]struct{}, error) {
	overLimit := make(map[skillDiagnosticIdentity]struct{})
	for _, source := range options.Sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		listing, err := listSourceEntries(ctx, source, options.Limits, options.Redact)
		if err != nil {
			return nil, err
		}
		err = collectOverLimitHistoryEntries(ctx, listing, options, overLimit)
		closeErr := listing.close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close skill source root")
		}
	}
	return overLimit, nil
}

func collectOverLimitHistoryEntries(ctx context.Context, listing *sourceListing, options ManagerOptions, destination map[skillDiagnosticIdentity]struct{}) error {
	for _, entry := range listing.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.rejected != "" || entry.size > options.Limits.MaxEntryBytes {
			continue
		}
		data, err := readSourceEntry(ctx, entry, options.Limits.MaxEntryBytes)
		if err != nil {
			continue
		}
		_, _, parseErr := ParseWithLimits(data, options.Limits)
		var violation *skillHistoryRangeViolation
		if !errors.As(parseErr, &violation) || !violation.aboveMaximum || violation.maximum != maxSkillHistoryTurns {
			continue
		}
		destination[skillDiagnosticIdentity{
			source: string(entry.source),
			path:   safeValue(entry.entryPath, options.Redact),
		}] = struct{}{}
	}
	return nil
}

// CompleteTurnRange identifies one complete user-to-assistant turn in the
// caller-owned conversation slice. The end index is exclusive.
type CompleteTurnRange struct {
	start int
	end   int
}

func (r CompleteTurnRange) Start() int { return r.start }

func (r CompleteTurnRange) End() int { return r.end }

// Messages returns a read-only view of the indexed turn. Callers must not
// mutate the returned slice or its messages.
func (r CompleteTurnRange) Messages(messages []conversation.Message) ([]conversation.Message, error) {
	if r.start < 0 || r.end <= r.start || r.end > len(messages) {
		return nil, fmt.Errorf("complete conversation turn range is invalid")
	}
	return messages[r.start:r.end], nil
}

// CompleteTurnScanner walks complete turns from newest to oldest. It keeps
// only immutable slice bounds and never copies the conversation.
type CompleteTurnScanner struct {
	cursor       int
	messageCount int
}

func NewCompleteTurnScanner(messages []conversation.Message) CompleteTurnScanner {
	return CompleteTurnScanner{cursor: len(messages), messageCount: len(messages)}
}

// Next returns the next complete turn from newest to oldest. Incomplete edge
// segments and turns with an open or mismatched tool chain are skipped.
func (s *CompleteTurnScanner) Next(ctx context.Context, messages []conversation.Message) (CompleteTurnRange, bool, error) {
	if s == nil {
		return CompleteTurnRange{}, false, fmt.Errorf("complete conversation turn scanner is nil")
	}
	if ctx == nil {
		return CompleteTurnRange{}, false, fmt.Errorf("complete conversation turn scan context is nil")
	}
	if err := ctx.Err(); err != nil {
		return CompleteTurnRange{}, false, err
	}
	if len(messages) != s.messageCount || s.cursor < 0 || s.cursor > len(messages) {
		return CompleteTurnRange{}, false, fmt.Errorf("conversation changed during complete turn scan")
	}

	for cursor := s.cursor; cursor > 0; {
		if err := ctx.Err(); err != nil {
			return CompleteTurnRange{}, false, err
		}
		end := cursor
		start, found, err := findPreviousTurnStart(ctx, messages, end)
		if err != nil {
			return CompleteTurnRange{}, false, err
		}
		if !found {
			s.cursor = 0
			return CompleteTurnRange{}, false, nil
		}
		complete, err := isCompleteTurnRange(ctx, messages, start, end)
		if err != nil {
			return CompleteTurnRange{}, false, err
		}
		s.cursor = start
		cursor = start
		if complete {
			return CompleteTurnRange{start: start, end: end}, true, nil
		}
	}
	return CompleteTurnRange{}, false, nil
}

func findPreviousTurnStart(ctx context.Context, messages []conversation.Message, end int) (int, bool, error) {
	for index, traversed := end-1, 0; index >= 0; index, traversed = index-1, traversed+1 {
		if traversed%completeTurnContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		if messages[index].Role == conversation.RoleUser {
			return index, true, nil
		}
	}
	return 0, false, nil
}

func isCompleteTurnRange(ctx context.Context, messages []conversation.Message, start, end int) (bool, error) {
	if start < 0 || end <= start || end > len(messages) || messages[start].Role != conversation.RoleUser {
		return false, nil
	}
	lastVisibleRole := conversation.RoleUser
	pendingCallID := ""
	pendingToolName := ""
	for index := start + 1; index < end; index++ {
		if (index-start-1)%completeTurnContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		message := messages[index]
		switch message.Role {
		case conversation.RoleThinking:
			continue
		case conversation.RoleAssistant:
			if pendingCallID != "" {
				return false, nil
			}
			lastVisibleRole = message.Role
		case conversation.RoleToolCall:
			if pendingCallID != "" || message.Tool == nil || message.Tool.CallID == "" || message.Tool.Name == "" {
				return false, nil
			}
			pendingCallID = message.Tool.CallID
			pendingToolName = message.Tool.Name
			lastVisibleRole = message.Role
		case conversation.RoleToolResult:
			if pendingCallID == "" || message.Tool == nil || message.Tool.CallID != pendingCallID || message.Tool.Name != pendingToolName {
				return false, nil
			}
			pendingCallID = ""
			pendingToolName = ""
			lastVisibleRole = message.Role
		default:
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return pendingCallID == "" && lastVisibleRole == conversation.RoleAssistant, nil
}
