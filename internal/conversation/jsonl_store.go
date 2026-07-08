package conversation

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xagent/internal/diagnostics"
)

type JSONLStoreOptions struct {
	DataDir string
	Now     func() time.Time
}

type JSONLStore struct {
	dataDir string
	now     func() time.Time

	mu       sync.Mutex
	progress map[string]int
}

type jsonlDiskState struct {
	messageCount   int
	messages       []Message
	diagnostics    []JSONLDiagnostic
	badLineCount   int
	lastBadLine    int
	lastRepairLine int
}

func (s jsonlDiskState) hasUnrepairedBadLines() bool {
	return s.badLineCount > 0 && s.lastRepairLine < s.lastBadLine
}

func NewJSONLStore(options JSONLStoreOptions) (*JSONLStore, error) {
	if strings.TrimSpace(options.DataDir) == "" {
		return nil, fmt.Errorf("会话数据目录不能为空")
	}
	if err := os.MkdirAll(options.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建会话数据目录: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &JSONLStore{dataDir: options.DataDir, now: now, progress: make(map[string]int)}, nil
}

func (s *JSONLStore) Create(ctx context.Context) (*Conversation, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	conversation := NewConversation(s.newID(), s.now())
	s.mu.Lock()
	s.progress[conversation.ID] = 0
	s.mu.Unlock()
	return conversation, nil
}

func (s *JSONLStore) Save(ctx context.Context, conversation *Conversation) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if conversation == nil {
		return fmt.Errorf("无法保存空会话")
	}
	if strings.TrimSpace(conversation.ID) == "" {
		return fmt.Errorf("无法保存缺少 ID 的会话")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.path(conversation.ID)
	state, err := s.readDiskMessageState(path)
	if err != nil {
		return err
	}
	diskCount := state.messageCount
	diskMessages := state.messages
	persisted := s.progress[conversation.ID]

	if state.hasUnrepairedBadLines() {
		return s.appendRepairSnapshot(ctx, path, conversation, diskCount, state.diagnostics)
	}

	if diskCount < persisted {
		return s.appendSnapshot(ctx, path, conversation, diskCount, "jsonl_append_conflict", "磁盘 JSONL 行数少于内存持久化计数")
	}
	if diskCount > persisted {
		if hasDuplicatePrefix(diskMessages, conversation.Messages, persisted) {
			return s.appendSnapshot(ctx, path, conversation, diskCount, "jsonl_duplicate_message_conflict", "检测到重复消息或并发追加冲突")
		}
		return s.appendSnapshot(ctx, path, conversation, diskCount, "jsonl_append_conflict", "检测到磁盘 JSONL 已被其他写入追加")
	}

	if persisted > len(conversation.Messages) {
		return s.appendSnapshot(ctx, path, conversation, diskCount, "jsonl_append_conflict", "内存会话消息少于已持久化计数")
	}
	if persisted == len(conversation.Messages) {
		return nil
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开 JSONL 会话文件失败: %w", err)
	}
	defer file.Close()

	conversation.UpdatedAt = s.now()
	for index := persisted; index < len(conversation.Messages); index++ {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		if err := writeJSONLRecord(file, s.messageRecord(conversation, index)); err != nil {
			return fmt.Errorf("写入 JSONL 会话记录失败: %w", err)
		}
	}
	s.progress[conversation.ID] = len(conversation.Messages)
	return nil
}

func (s *JSONLStore) Load(ctx context.Context, id string) (*Conversation, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	conversation, count, err := s.loadFromPath(ctx, s.path(id), id)
	if err != nil {
		if s.jsonlMissing(id) {
			legacy, legacyErr := s.loadLegacyJSON(ctx, id)
			if legacyErr != nil {
				return nil, err
			}
			s.mu.Lock()
			s.progress[legacy.ID] = 0
			s.mu.Unlock()
			return legacy, nil
		}
		return nil, err
	}
	s.mu.Lock()
	s.progress[conversation.ID] = count
	s.mu.Unlock()
	return conversation, nil
}

func (s *JSONLStore) List(ctx context.Context) ([]Conversation, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, fmt.Errorf("读取 JSONL 会话列表失败: %w", err)
	}
	conversations := make([]Conversation, 0, len(entries))
	for _, entry := range entries {
		if err := ctxErr(ctx); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		path := filepath.Join(s.dataDir, entry.Name())
		conversation, _, err := s.loadFromPath(ctx, path, id)
		if err != nil {
			placeholder, ok := s.recoverableListPlaceholder(path, id)
			if ok {
				conversations = append(conversations, placeholder)
			}
			continue
		}
		conversations = append(conversations, *conversation)
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	return conversations, nil
}

func (s *JSONLStore) path(id string) string {
	name := filepath.Base(id) + ".jsonl"
	return filepath.Join(s.dataDir, name)
}

func (s *JSONLStore) legacyPath(id string) string {
	name := filepath.Base(id) + ".json"
	return filepath.Join(s.dataDir, name)
}

func (s *JSONLStore) jsonlMissing(id string) bool {
	_, err := os.Stat(s.path(id))
	return os.IsNotExist(err)
}

func (s *JSONLStore) loadLegacyJSON(ctx context.Context, id string) (*Conversation, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.legacyPath(id))
	if err != nil {
		return nil, fmt.Errorf("加载旧 JSON 会话失败: %w", err)
	}
	var conversation Conversation
	if err := json.Unmarshal(data, &conversation); err != nil {
		return nil, fmt.Errorf("旧 JSON 会话格式无效: %w", err)
	}
	if strings.TrimSpace(conversation.ID) == "" {
		conversation.ID = id
	}
	return &conversation, nil
}

func (s *JSONLStore) newID() string {
	stamp := s.now().Format("20060102-150405")
	suffixBytes := make([]byte, 2)
	if _, err := rand.Read(suffixBytes); err != nil {
		return fmt.Sprintf("%s-%04d", stamp, s.now().UnixNano()%10000)
	}
	return fmt.Sprintf("%s-%s", stamp, hex.EncodeToString(suffixBytes))
}

func (s *JSONLStore) messageRecord(conversation *Conversation, index int) JSONLRecord {
	message := conversation.Messages[index]
	return JSONLRecord{
		Version:               JSONLVersion,
		Type:                  RecordTypeMessage,
		SessionID:             conversation.ID,
		MessageIndex:          index,
		CreatedAt:             s.now(),
		ConversationTitle:     conversation.Title,
		ConversationCreatedAt: conversation.CreatedAt,
		ConversationUpdatedAt: conversation.UpdatedAt,
		Message:               &message,
	}
}

func (s *JSONLStore) snapshotRecord(conversation *Conversation, messageIndex int, diagnostic JSONLDiagnostic) JSONLRecord {
	snapshot := cloneConversation(conversation)
	return JSONLRecord{
		Version:      JSONLVersion,
		Type:         RecordTypeSnapshot,
		SessionID:    conversation.ID,
		MessageIndex: messageIndex,
		CreatedAt:    s.now(),
		Snapshot:     &snapshot,
		Diagnostics:  []JSONLDiagnostic{diagnostic},
	}
}

func (s *JSONLStore) appendSnapshot(ctx context.Context, path string, conversation *Conversation, diskCount int, code string, message string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开 JSONL 会话文件失败: %w", err)
	}
	defer file.Close()

	conversation.UpdatedAt = s.now()
	diagnostic := newJSONLDiagnostic(code, message, diagnostics.SeverityWarning)
	if err := writeJSONLRecord(file, s.snapshotRecord(conversation, diskCount, diagnostic)); err != nil {
		return fmt.Errorf("写入 JSONL snapshot 记录失败: %w", err)
	}
	s.progress[conversation.ID] = len(conversation.Messages)
	return nil
}

func (s *JSONLStore) appendRepairSnapshot(ctx context.Context, path string, conversation *Conversation, diskCount int, lineDiagnostics []JSONLDiagnostic) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开 JSONL 会话文件失败: %w", err)
	}
	defer file.Close()

	conversation.UpdatedAt = s.now()
	diagnosticsList := append([]JSONLDiagnostic(nil), lineDiagnostics...)
	diagnosticsList = append(diagnosticsList, newJSONLDiagnostic("jsonl_repair_snapshot", "检测到可恢复的损坏 JSONL 行，已追加 snapshot repair 以便后续保存继续", diagnostics.SeverityWarning))
	record := s.snapshotRecord(conversation, diskCount, newJSONLDiagnostic("jsonl_repair_snapshot", "检测到可恢复的损坏 JSONL 行，已追加 snapshot repair 以便后续保存继续", diagnostics.SeverityWarning))
	record.Diagnostics = diagnosticsList
	if err := writeJSONLRecord(file, record); err != nil {
		return fmt.Errorf("写入 JSONL repair snapshot 记录失败: %w", err)
	}
	s.progress[conversation.ID] = len(conversation.Messages)
	return nil
}

func (s *JSONLStore) readDiskMessageState(path string) (jsonlDiskState, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return jsonlDiskState{}, nil
	}
	if err != nil {
		return jsonlDiskState{}, fmt.Errorf("读取 JSONL 会话文件失败: %w", err)
	}
	defer file.Close()

	state := jsonlDiskState{}
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record JSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			state.badLineCount++
			state.lastBadLine = lineNumber
			state.diagnostics = append(state.diagnostics, recoverableJSONLDiagnostic("jsonl_recovery_bad_line", lineNumber))
			continue
		}
		if items := record.Validate(); hasJSONLErrors(items) {
			state.badLineCount++
			state.lastBadLine = lineNumber
			state.diagnostics = append(state.diagnostics, recoverableValidationDiagnostics(items, lineNumber)...)
			continue
		}
		if record.Type == RecordTypeSnapshot && hasDiagnosticCode(record.Diagnostics, "jsonl_repair_snapshot") {
			state.lastRepairLine = lineNumber
		}
		switch record.Type {
		case RecordTypeMessage:
			state.messages = append(state.messages, *record.Message)
		case RecordTypeSnapshot:
			if record.Snapshot != nil {
				state.messages = cloneMessages(record.Snapshot.Messages)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return jsonlDiskState{}, fmt.Errorf("读取 JSONL 会话记录失败: %w", err)
	}
	state.messageCount = len(state.messages)
	return state, nil
}

func (s *JSONLStore) recoverableListPlaceholder(path string, id string) (Conversation, bool) {
	state, err := s.readDiskMessageState(path)
	if err != nil || !state.hasUnrepairedBadLines() {
		return Conversation{}, false
	}
	updatedAt := s.now()
	if info, statErr := os.Stat(path); statErr == nil {
		updatedAt = info.ModTime()
	}
	placeholder := Conversation{
		ID:        id,
		Title:     "可恢复会话：" + id,
		Messages:  []Message{},
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
		Context: &ContextMetadata{
			RecoveryDiagnostics: state.diagnostics,
		},
	}
	return placeholder, true
}

func recoverableJSONLDiagnostic(code string, lineNumber int) JSONLDiagnostic {
	message := fmt.Sprintf("JSONL 第 %d 行可恢复损坏，已记录行号、错误类型和可恢复状态", lineNumber)
	return newJSONLDiagnostic(code, message, diagnostics.SeverityWarning).WithPath(fmt.Sprintf("line:%d;recoverable:true", lineNumber))
}

func recoverableValidationDiagnostics(items []JSONLDiagnostic, lineNumber int) []JSONLDiagnostic {
	diagnosticsList := make([]JSONLDiagnostic, 0, len(items))
	for _, item := range items {
		code := item.Code
		if strings.TrimSpace(code) == "" {
			code = "jsonl_recovery_invalid_record"
		}
		diagnosticsList = append(diagnosticsList, recoverableJSONLDiagnostic(code, lineNumber))
	}
	return diagnosticsList
}

func (s *JSONLStore) loadFromPath(ctx context.Context, path string, fallbackID string) (*Conversation, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("加载 JSONL 会话失败: %w", err)
	}
	defer file.Close()

	conversation := &Conversation{ID: fallbackID, Title: "新会话", Messages: []Message{}}
	messageCount := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := ctxErr(ctx); err != nil {
			return nil, 0, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record JSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, 0, fmt.Errorf("JSONL 会话记录格式无效: %w", err)
		}
		if diagnostics := record.Validate(); hasJSONLErrors(diagnostics) {
			return nil, 0, fmt.Errorf("JSONL 会话记录校验失败: %s", diagnostics[0].Message)
		}
		switch record.Type {
		case RecordTypeMessage:
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
		return nil, 0, fmt.Errorf("读取 JSONL 会话记录失败: %w", err)
	}
	return conversation, messageCount, nil
}

func applyMessageRecord(conversation *Conversation, record JSONLRecord) {
	if record.SessionID != "" {
		conversation.ID = record.SessionID
	}
	if record.ConversationTitle != "" {
		conversation.Title = record.ConversationTitle
	}
	if !record.ConversationCreatedAt.IsZero() {
		conversation.CreatedAt = record.ConversationCreatedAt
	}
	if !record.ConversationUpdatedAt.IsZero() {
		conversation.UpdatedAt = record.ConversationUpdatedAt
	}
	conversation.Messages = append(conversation.Messages, *record.Message)
	if conversation.CreatedAt.IsZero() {
		conversation.CreatedAt = record.Message.CreatedAt
	}
	if conversation.UpdatedAt.IsZero() || record.Message.CreatedAt.After(conversation.UpdatedAt) {
		conversation.UpdatedAt = record.Message.CreatedAt
	}
}

func writeJSONLRecord(writer io.Writer, record JSONLRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func hasJSONLErrors(items []JSONLDiagnostic) bool {
	for _, item := range items {
		if item.Severity == diagnostics.SeverityError {
			return true
		}
	}
	return false
}

func hasDiagnosticCode(items []JSONLDiagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func hasDuplicatePrefix(diskMessages []Message, memoryMessages []Message, persisted int) bool {
	limit := persisted
	if len(diskMessages) < limit {
		limit = len(diskMessages)
	}
	if len(memoryMessages) < limit {
		limit = len(memoryMessages)
	}
	for i := 0; i < limit; i++ {
		if messagesEqual(diskMessages[i], memoryMessages[i]) {
			continue
		}
		return false
	}
	for i := persisted; i < len(diskMessages); i++ {
		for _, message := range memoryMessages {
			if messagesEqual(diskMessages[i], message) {
				return true
			}
		}
	}
	return false
}

func messagesEqual(a Message, b Message) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func cloneConversation(conversation *Conversation) Conversation {
	if conversation == nil {
		return Conversation{Messages: []Message{}}
	}
	clone := *conversation
	clone.Messages = cloneMessages(conversation.Messages)
	return clone
}

func cloneMessages(messages []Message) []Message {
	clone := make([]Message, len(messages))
	copy(clone, messages)
	return clone
}
