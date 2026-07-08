package contextmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
)

type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeManual Mode = "manual"
)

type Manager struct {
	provider provider.Provider
	dataDir  string
	cfg      config.ContextConfig
}

type Result struct {
	Changed       bool
	Externalized  int
	Summarized    bool
	Estimated     int64
	Message       string
	CircuitBroken bool
}

func New(provider provider.Provider, dataDir string, cfg config.ContextConfig) *Manager {
	return &Manager{provider: provider, dataDir: dataDir, cfg: cfg}
}

func (m *Manager) Prepare(ctx context.Context, conv *conversation.Conversation, mode Mode) (Result, error) {
	var result Result
	if m == nil || conv == nil || !config.Enabled(m.cfg.Enabled, true) {
		return result, nil
	}
	externalized, err := m.externalizeLargeToolResults(conv)
	if err != nil {
		return result, err
	}
	result.Externalized = externalized
	result.Changed = externalized > 0

	estimated := EstimateConversationTokens(conv)
	result.Estimated = estimated
	meta := conversation.EnsureContext(conv)
	meta.LastEstimatedTokens = estimated
	meta.LastEstimatedCharacters = countConversationChars(conv)

	if !m.shouldSummarize(conv, estimated, mode) {
		if externalized > 0 {
			result.Message = fmt.Sprintf("已外置 %d 条大型工具结果", externalized)
		}
		return result, nil
	}
	if meta.SummaryFailureCount >= m.cfg.SummaryFailureLimit {
		result.CircuitBroken = true
		if mode == ModeManual {
			return result, fmt.Errorf("上下文摘要连续失败 %d 次，已熔断", meta.SummaryFailureCount)
		}
		return result, nil
	}

	summary, cutoff, err := m.summarize(ctx, conv)
	if err != nil {
		meta.SummaryFailureCount++
		if mode == ModeManual || meta.SummaryFailureCount >= m.cfg.SummaryFailureLimit {
			return result, err
		}
		return result, nil
	}
	m.applySummary(conv, summary, cutoff)
	result.Changed = true
	result.Summarized = true
	result.Message = "上下文已压缩并生成结构化摘要"
	return result, nil
}

func (m *Manager) CompactNow(ctx context.Context, conv *conversation.Conversation) (Result, error) {
	return m.Prepare(ctx, conv, ModeManual)
}

func (m *Manager) UpdateUsage(conv *conversation.Conversation, usage *provider.Usage) {
	if m == nil || conv == nil || usage == nil {
		return
	}
	meta := conversation.EnsureContext(conv)
	meta.LastInputTokens = usage.InputTokens
	meta.LastOutputTokens = usage.OutputTokens
	meta.LastEstimatedTokens = usage.InputTokens
	meta.LastEstimatedCharacters = countConversationChars(conv)
}

func (m *Manager) externalizeLargeToolResults(conv *conversation.Conversation) (int, error) {
	large := make([]int, 0)
	total := 0
	for index, message := range conv.Messages {
		if message.Role != conversation.RoleToolResult || message.Externalized {
			continue
		}
		size := len(message.ToolResultContent)
		if size == 0 {
			size = len(message.Content)
		}
		total += size
		if size > m.cfg.ToolResultThresholdChars {
			large = append(large, index)
		}
	}
	if total > m.cfg.ToolResultsThresholdChars {
		indexes := make([]int, 0)
		for index, message := range conv.Messages {
			if message.Role == conversation.RoleToolResult && !message.Externalized {
				indexes = append(indexes, index)
			}
		}
		sort.Slice(indexes, func(i, j int) bool {
			return toolResultSize(conv.Messages[indexes[i]]) > toolResultSize(conv.Messages[indexes[j]])
		})
		for _, index := range indexes {
			if containsIndex(large, index) {
				continue
			}
			large = append(large, index)
			total -= toolResultSize(conv.Messages[index])
			if total <= m.cfg.ToolResultsThresholdChars {
				break
			}
		}
	}
	count := 0
	for _, index := range large {
		if err := m.externalizeMessage(conv, index); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (m *Manager) externalizeMessage(conv *conversation.Conversation, index int) error {
	message := &conv.Messages[index]
	payload := externalPayload{
		Role:                message.Role,
		Content:             message.Content,
		ToolCallID:          message.ToolCallID,
		ToolName:            message.ToolName,
		ToolResultContent:   message.ToolResultContent,
		ToolResultStatus:    message.ToolResultStatus,
		ToolResultSummary:   message.ToolResultSummary,
		ToolResultTruncated: message.ToolResultTruncated,
		ToolResultData:      message.ToolResultData,
		ToolResultError:     message.ToolResultError,
		ToolErrorCode:       message.ToolErrorCode,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化外置工具结果失败: %w", err)
	}
	dir := filepath.Join(m.dataDir, "context_blobs", filepath.Base(conv.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建上下文外置目录失败: %w", err)
	}
	name := fmt.Sprintf("%04d_%s.json", index, safeFilePart(message.ToolCallID))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入外置工具结果失败: %w", err)
	}
	preview := previewText(message.ToolResultContent, m.cfg.PreviewChars)
	if preview == "" {
		preview = previewText(message.Content, m.cfg.PreviewChars)
	}
	message.Externalized = true
	message.ExternalPath = path
	message.ExternalBytes = int64(len(data))
	message.ExternalPreview = preview
	message.Content = conversationExternalMarker(*message)
	message.ToolResultContent = message.Content
	message.ToolResultData = nil
	message.ToolResultError = nil
	now := time.Now()
	conversation.EnsureContext(conv).LastCompressionAt = &now
	conv.UpdatedAt = now
	return nil
}

func (m *Manager) shouldSummarize(conv *conversation.Conversation, estimated int64, mode Mode) bool {
	margin := m.cfg.AutoMarginTokens
	if mode == ModeManual {
		margin = m.cfg.ManualMarginTokens
	}
	if mode == ModeManual {
		return len(conv.Messages) > m.cfg.RecentKeepMessages
	}
	return estimated >= m.cfg.ModelWindowTokens-margin
}

func (m *Manager) summarize(ctx context.Context, conv *conversation.Conversation) (string, int, error) {
	cutoff := summaryCutoff(conv.Messages, m.cfg.RecentKeepTokens, m.cfg.RecentKeepMessages)
	if cutoff <= 0 {
		return "", 0, fmt.Errorf("没有可摘要的早期上下文")
	}
	prompt := summaryPrompt(conv.Messages[:cutoff])
	stream, err := m.provider.StreamChat(ctx, provider.ChatRequest{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: prompt}}})
	if err != nil {
		return "", 0, fmt.Errorf("请求上下文摘要失败: %w", err)
	}
	var builder strings.Builder
	for event := range stream {
		switch event.Type {
		case provider.StreamEventTextDelta:
			builder.WriteString(event.Delta)
		case provider.StreamEventToolCall:
			return "", 0, fmt.Errorf("摘要请求禁止工具调用")
		case provider.StreamEventError:
			return "", 0, event.Err
		case provider.StreamEventDone:
			summary := extractFinalSummary(builder.String())
			if strings.TrimSpace(summary) == "" {
				return "", 0, fmt.Errorf("摘要结果为空")
			}
			return summary, cutoff, nil
		}
	}
	return "", 0, fmt.Errorf("摘要流异常结束")
}

func (m *Manager) applySummary(conv *conversation.Conversation, summary string, cutoff int) {
	boundary := "上下文已压缩。摘要只提供线索，不是完整事实来源；如需代码、工具输出或文件细节，请重新读取对应文件或外置工具结果，不要根据摘要脑补。"
	now := time.Now()
	kept := append([]conversation.Message{}, conv.Messages[cutoff:]...)
	conv.Messages = nil
	conversation.AppendContextSummaryMessage(conv, summary)
	conversation.AppendContextBoundaryMessage(conv, boundary)
	conv.Messages = append(conv.Messages, kept...)
	meta := conversation.EnsureContext(conv)
	meta.Summary = summary
	meta.LastBoundary = boundary
	meta.SummaryFailureCount = 0
	meta.LastCompressionAt = &now
	conv.UpdatedAt = now
}

type externalPayload struct {
	Role                conversation.MessageRole `json:"role"`
	Content             string                   `json:"content"`
	ToolCallID          string                   `json:"tool_call_id,omitempty"`
	ToolName            string                   `json:"tool_name,omitempty"`
	ToolResultContent   string                   `json:"tool_result_content,omitempty"`
	ToolResultStatus    string                   `json:"tool_result_status,omitempty"`
	ToolResultSummary   string                   `json:"tool_result_summary,omitempty"`
	ToolResultTruncated bool                     `json:"tool_result_truncated,omitempty"`
	ToolResultData      json.RawMessage          `json:"tool_result_data,omitempty"`
	ToolResultError     json.RawMessage          `json:"tool_result_error,omitempty"`
	ToolErrorCode       string                   `json:"tool_error_code,omitempty"`
}

func EstimateConversationTokens(conv *conversation.Conversation) int64 {
	if conv == nil {
		return 0
	}
	chars := countConversationChars(conv)
	meta := conversation.EnsureContext(conv)
	if meta.LastInputTokens > 0 && meta.LastEstimatedCharacters > 0 {
		delta := chars - meta.LastEstimatedCharacters
		if delta < 0 {
			delta = 0
		}
		return meta.LastInputTokens + int64(delta/4)
	}
	return int64(chars / 4)
}

func countConversationChars(conv *conversation.Conversation) int {
	if conv == nil {
		return 0
	}
	count := 0
	for _, message := range conversation.ContextMessages(conv) {
		count += len(message.Content) + len(message.ToolResultContent) + len(message.RawToolArguments)
	}
	return count
}

func summaryCutoff(messages []conversation.Message, keepTokens int64, keepMessages int) int {
	if len(messages) <= keepMessages {
		return 0
	}
	keepChars := int(keepTokens * 4)
	chars := 0
	cutoff := len(messages)
	for index := len(messages) - 1; index >= 0; index-- {
		chars += len(messages[index].Content) + len(messages[index].ToolResultContent) + len(messages[index].RawToolArguments)
		if len(messages)-index >= keepMessages && chars >= keepChars {
			cutoff = index
			break
		}
	}
	if cutoff == len(messages) {
		cutoff = len(messages) - keepMessages
	}
	for cutoff < len(messages) && messages[cutoff].Role == conversation.RoleToolResult {
		cutoff++
	}
	if cutoff <= 0 || cutoff >= len(messages) {
		return 0
	}
	return cutoff
}

func summaryPrompt(messages []conversation.Message) string {
	data, _ := json.MarshalIndent(messages, "", "  ")
	return "你正在为 XAgent 压缩较早的对话上下文。禁止调用任何工具；只基于下面提供的消息生成摘要。先在内部写分析草稿，再输出正式摘要；最终回复只能包含正式摘要，不要包含草稿。\n\n正式摘要必须使用固定章节：\n1. 当前目标\n2. 已完成事项\n3. 关键决策与约束\n4. 重要文件/符号线索\n5. 工具结果与外置文件索引\n6. 未完成任务/下一步\n7. 风险与不能臆测的内容\n\n较早消息 JSON：\n" + string(data)
}

func extractFinalSummary(text string) string {
	text = strings.TrimSpace(text)
	for _, marker := range []string{"<summary>", "正式摘要：", "正式摘要:"} {
		if index := strings.LastIndex(text, marker); index >= 0 {
			text = strings.TrimSpace(text[index+len(marker):])
		}
	}
	text = strings.TrimSuffix(text, "</summary>")
	return strings.TrimSpace(text)
}

func toolResultSize(message conversation.Message) int {
	if message.ToolResultContent != "" {
		return len(message.ToolResultContent)
	}
	return len(message.Content)
}

func containsIndex(indexes []int, want int) bool {
	for _, index := range indexes {
		if index == want {
			return true
		}
	}
	return false
}

func previewText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n[预览已截断]"
}

func conversationExternalMarker(message conversation.Message) string {
	return fmt.Sprintf("[工具结果已外置保存，artifact_id: %s，bytes: %d。如需完整细节，请由本地用户显式查看该 artifact，不要根据预览或摘要脑补。]", safeFilePart(message.ToolCallID), message.ExternalBytes)
}

func safeFilePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "tool_result"
	}
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	result := builder.String()
	if len(result) > 64 {
		result = result[:64]
	}
	return result
}
