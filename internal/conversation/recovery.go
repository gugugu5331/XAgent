package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"xagent/internal/diagnostics"
)

type RecoveringStore interface {
	Recover(ctx context.Context, id string) (*Conversation, RecoveryReport, error)
}

type RecoveryReport struct {
	Diagnostics          []diagnostics.Diagnostic `json:"diagnostics,omitempty"`
	SkippedLines         int                      `json:"skipped_lines,omitempty"`
	TruncatedFromMessage int                      `json:"truncated_from_message,omitempty"`
	TimeGapReminder      bool                     `json:"time_gap_reminder,omitempty"`
}

func (s *JSONLStore) Recover(ctx context.Context, id string) (*Conversation, RecoveryReport, error) {
	conversation, persistedCount, report, err := s.recoverFromPath(ctx, s.path(id), id)
	if err != nil {
		if s.jsonlMissing(id) {
			legacy, legacyErr := s.loadLegacyJSON(ctx, id)
			if legacyErr != nil {
				return nil, report, err
			}
			report.TruncatedFromMessage = -1
			report.add("jsonl_recovery_legacy_json", "已从旧 JSON 会话文件恢复；下次保存会写入 JSONL 并保留原文件", s.legacyPath(id))
			s.mu.Lock()
			s.progress[legacy.ID] = 0
			s.mu.Unlock()
			return legacy, report, nil
		}
		return nil, report, err
	}
	s.mu.Lock()
	s.progress[conversation.ID] = persistedCount
	s.mu.Unlock()
	return conversation, report, nil
}

func (s *JSONLStore) recoverFromPath(ctx context.Context, path string, fallbackID string) (*Conversation, int, RecoveryReport, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, RecoveryReport{}, fmt.Errorf("恢复 JSONL 会话失败: %w", err)
	}
	defer file.Close()

	conversation := &Conversation{ID: fallbackID, Title: "新会话", Messages: []Message{}}
	report := RecoveryReport{TruncatedFromMessage: -1}
	messageCount := 0
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if err := ctxErr(ctx); err != nil {
			return nil, 0, report, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record JSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			report.Diagnostics = append(report.Diagnostics, recoverableJSONLDiagnostic("jsonl_recovery_bad_line", lineNumber).WithPath(path))
			report.SkippedLines++
			continue
		}
		if items := record.Validate(); hasJSONLErrors(items) {
			report.Diagnostics = append(report.Diagnostics, withRecoveryPath(recoverableValidationDiagnostics(items, lineNumber), path)...)
			report.SkippedLines++
			continue
		}
		switch record.Type {
		case RecordTypeMessage:
			if record.Message.Role == RoleToolResult && !hasOpenToolCall(conversation.Messages, record.Message.ToolCallID) {
				report.add("jsonl_recovery_unmatched_tool_result", "跳过未匹配 tool_call 的 tool_result", path)
				report.SkippedLines++
				continue
			}
			applyMessageRecord(conversation, record)
			messageCount++
		case RecordTypeSnapshot:
			if record.Snapshot != nil {
				clone := cloneConversation(record.Snapshot)
				conversation = &clone
				messageCount = len(conversation.Messages)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		report.add("jsonl_recovery_scan_failed", fmt.Sprintf("读取 JSONL 会话记录失败: %v", err), path)
	}

	truncatedFrom := truncateUnclosedToolCall(conversation)
	if truncatedFrom >= 0 {
		report.TruncatedFromMessage = truncatedFrom
		messageCount = len(conversation.Messages)
		report.add("jsonl_recovery_unclosed_tool_call", "检测到未闭合 tool_call，已从该消息截断", path)
	}
	if s.shouldInsertTimeGapReminder(conversation) {
		AppendContextBoundaryMessage(conversation, fmt.Sprintf("距离上次会话已超过恢复提醒阈值；继续前请重新确认当前文件和外部状态。上次更新时间：%s", conversation.UpdatedAt.Format(time.RFC3339)))
		report.TimeGapReminder = true
		messageCount = len(conversation.Messages)
	}
	return conversation, messageCount, report, nil
}

func (r *RecoveryReport) add(code string, message string, path string) {
	r.Diagnostics = append(r.Diagnostics, newJSONLDiagnostic(code, message, diagnostics.SeverityWarning).WithPath(path))
}

func withRecoveryPath(items []JSONLDiagnostic, path string) []JSONLDiagnostic {
	for index := range items {
		items[index] = items[index].WithPath(path)
	}
	return items
}

func hasOpenToolCall(messages []Message, callID string) bool {
	if strings.TrimSpace(callID) == "" {
		return false
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.ToolCallID != callID {
			continue
		}
		switch message.Role {
		case RoleToolResult:
			return false
		case RoleToolCall:
			return true
		}
	}
	return false
}

func truncateUnclosedToolCall(conversation *Conversation) int {
	open := map[string]int{}
	for index, message := range conversation.Messages {
		switch message.Role {
		case RoleToolCall:
			if strings.TrimSpace(message.ToolCallID) != "" {
				open[message.ToolCallID] = index
			}
		case RoleToolResult:
			delete(open, message.ToolCallID)
		}
	}
	if len(open) == 0 {
		return -1
	}
	cut := len(conversation.Messages)
	for _, index := range open {
		if index < cut {
			cut = index
		}
	}
	conversation.Messages = cloneMessages(conversation.Messages[:cut])
	return cut
}

func (s *JSONLStore) shouldInsertTimeGapReminder(conversation *Conversation) bool {
	if conversation == nil || conversation.UpdatedAt.IsZero() {
		return false
	}
	return s.now().Sub(conversation.UpdatedAt) > 7*24*time.Hour
}
