package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
)

func TestJSONLRecordValidation(t *testing.T) {
	message := Message{Role: RoleUser, Content: "hello", CreatedAt: time.Unix(1, 0)}
	valid := JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, Message: &message}
	if diagnostics := valid.Validate(); len(diagnostics) != 0 {
		t.Fatalf("有效记录不应返回诊断: %#v", diagnostics)
	}

	invalidVersion := JSONLRecord{Version: 99, Type: RecordTypeMessage, Message: &message}
	assertHasDiagnostic(t, invalidVersion.Validate(), "jsonl_unknown_version")

	invalidType := JSONLRecord{Version: JSONLVersion, Type: RecordType("bad"), Message: &message}
	assertHasDiagnostic(t, invalidType.Validate(), "jsonl_invalid_record_type")

	missingMessage := JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage}
	assertHasDiagnostic(t, missingMessage.Validate(), "jsonl_missing_message")

	snapshot := Conversation{ID: "session", Messages: []Message{message}}
	validSnapshot := JSONLRecord{Version: JSONLVersion, Type: RecordTypeSnapshot, Snapshot: &snapshot}
	if diagnostics := validSnapshot.Validate(); len(diagnostics) != 0 {
		t.Fatalf("有效 snapshot 不应返回诊断: %#v", diagnostics)
	}

	record := JSONLRecord{
		Version:  JSONLVersion,
		Type:     RecordTypeSnapshot,
		Snapshot: &snapshot,
		Error:    "api_key=secret-token Authorization: Bearer abc123 sk-ant-test",
		Diagnostics: []JSONLDiagnostic{{
			Code:     "x",
			Message:  "password=hunter2",
			Path:     "/tmp/token=secret",
			Severity: diagnostics.SeverityWarning,
		}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	text := string(data)
	for _, leaked := range []string{"secret-token", "abc123", "sk-ant-test", "hunter2", "token=secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("记录应脱敏敏感文本 %q: %s", leaked, text)
		}
	}
}

func TestJSONLStoreCreateSaveLoad(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 10, 11, 12, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	conversation, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if !strings.HasPrefix(conversation.ID, "20260705-101112-") {
		t.Fatalf("会话 ID 格式不符合 YYYYMMDD-HHMMSS-xxxx: %s", conversation.ID)
	}

	AppendUserMessage(conversation, "你好")
	AppendAssistantMessage(conversation, "你好，有什么可以帮你？")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("save conversation: %v", err)
	}

	loaded, err := store.Load(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if loaded.ID != conversation.ID {
		t.Fatalf("加载的会话 ID = %q, want %q", loaded.ID, conversation.ID)
	}
	if loaded.Title != "你好" {
		t.Fatalf("加载的标题 = %q, want 你好", loaded.Title)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("加载的消息数 = %d, want 2", len(loaded.Messages))
	}
	if loaded.Messages[0].Role != RoleUser || loaded.Messages[0].Content != "你好" {
		t.Fatalf("第一条消息不匹配: %#v", loaded.Messages[0])
	}
	if countJSONLLines(t, store.path(conversation.ID)) != 2 {
		t.Fatalf("首次保存应写入 2 条 message 记录")
	}
}

func TestJSONLStoreSaveAppendsOnlyNewMessages(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	AppendUserMessage(conversation, "第一条")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("first save: %v", err)
	}
	firstSize := fileSize(t, store.path(conversation.ID))
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("second save without changes: %v", err)
	}
	if size := fileSize(t, store.path(conversation.ID)); size != firstSize {
		t.Fatalf("无新增消息时不应重复追加，size = %d, want %d", size, firstSize)
	}

	AppendAssistantMessage(conversation, "第二条")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("third save: %v", err)
	}
	if lines := countJSONLLines(t, store.path(conversation.ID)); lines != 2 {
		t.Fatalf("增量保存后 JSONL 行数 = %d, want 2", lines)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Save(ctx, conversation); err != nil {
				t.Errorf("concurrent save: %v", err)
			}
		}()
	}
	wg.Wait()
	if lines := countJSONLLines(t, store.path(conversation.ID)); lines != 2 {
		t.Fatalf("单进程锁应避免并发重复追加，JSONL 行数 = %d, want 2", lines)
	}
}

func TestJSONLStoreDetectsAppendConflictAndWritesSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	AppendUserMessage(conversation, "本地消息")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("first save: %v", err)
	}

	foreignMessage := Message{Role: RoleAssistant, Content: "外部追加", CreatedAt: time.Date(2026, 7, 5, 11, 1, 0, 0, time.UTC)}
	foreignRecord := JSONLRecord{
		Version:               JSONLVersion,
		Type:                  RecordTypeMessage,
		SessionID:             conversation.ID,
		MessageIndex:          1,
		CreatedAt:             time.Date(2026, 7, 5, 11, 1, 0, 0, time.UTC),
		ConversationTitle:     conversation.Title,
		ConversationCreatedAt: conversation.CreatedAt,
		ConversationUpdatedAt: conversation.UpdatedAt,
		Message:               &foreignMessage,
	}
	appendRawRecord(t, store.path(conversation.ID), foreignRecord)

	AppendAssistantMessage(conversation, "本地新增")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("conflict save should write snapshot repair instead of failing: %v", err)
	}

	records := readJSONLRecords(t, store.path(conversation.ID))
	if len(records) != 3 {
		t.Fatalf("冲突后应保留原记录并追加 1 条 snapshot，行数 = %d, want 3", len(records))
	}
	last := records[len(records)-1]
	if last.Type != RecordTypeSnapshot {
		t.Fatalf("冲突修复记录类型 = %q, want snapshot", last.Type)
	}
	assertHasDiagnostic(t, last.Diagnostics, "jsonl_append_conflict")
	if last.Snapshot == nil || len(last.Snapshot.Messages) != 2 {
		t.Fatalf("snapshot 应包含当前完整内存会话: %#v", last.Snapshot)
	}

	loaded, err := store.Load(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("load after snapshot: %v", err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "本地新增" {
		t.Fatalf("加载时 snapshot 应修复为本地完整会话: %#v", loaded.Messages)
	}

	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("snapshot 后无新增 save 不应重复追加: %v", err)
	}
	if lines := countJSONLLines(t, store.path(conversation.ID)); lines != 3 {
		t.Fatalf("snapshot 后不应重复追加完整历史，行数 = %d, want 3", lines)
	}
}

func TestJSONLStoreDetectsDuplicateMessageConflictAndWritesSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	AppendUserMessage(conversation, "重复消息")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("first save: %v", err)
	}
	appendRawRecord(t, store.path(conversation.ID), store.messageRecord(conversation, 0))

	AppendAssistantMessage(conversation, "本地新增")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("duplicate conflict save: %v", err)
	}
	records := readJSONLRecords(t, store.path(conversation.ID))
	last := records[len(records)-1]
	if last.Type != RecordTypeSnapshot {
		t.Fatalf("重复冲突应追加 snapshot，last = %q", last.Type)
	}
	assertHasDiagnostic(t, last.Diagnostics, "jsonl_duplicate_message_conflict")
}

func TestJSONLStoreListScansRecordsWithoutMeta(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	older := NewConversation("older", time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(older, "旧会话")
	older.UpdatedAt = time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, older); err != nil {
		t.Fatalf("save older: %v", err)
	}

	newer := NewConversation("newer", time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(newer, "新会话")
	newer.UpdatedAt = time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}

	if err := os.WriteFile(filepath.Join(store.dataDir, "meta.json"), []byte(`{"id":"ignored"}`), 0o600); err != nil {
		t.Fatalf("write fake meta: %v", err)
	}
	writeRawText(t, filepath.Join(store.dataDir, "broken.jsonl"), "not json\n")

	conversations, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(conversations) != 3 {
		t.Fatalf("len(conversations) = %d, want 3: %#v", len(conversations), conversations)
	}
	ids := map[string]Conversation{}
	for _, conversation := range conversations {
		ids[conversation.ID] = conversation
	}
	if _, ok := ids["newer"]; !ok {
		t.Fatalf("list missing newer: %#v", conversations)
	}
	if _, ok := ids["older"]; !ok {
		t.Fatalf("list missing older: %#v", conversations)
	}
	broken, ok := ids["broken"]
	if !ok || !strings.Contains(broken.Title, "可恢复") {
		t.Fatalf("list should include recoverable broken placeholder: %#v", conversations)
	}
	if ids["newer"].Title != "新会话" || len(ids["newer"].Messages) != 1 {
		t.Fatalf("newer summary reconstructed incorrectly: %#v", ids["newer"])
	}
}

func TestJSONLStoreLoadsLegacyJSONWithoutDeletingOriginal(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	legacy := NewConversation("legacy", time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(legacy, "旧格式")
	legacy.UpdatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(store.legacyPath(legacy.ID), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	loaded, err := store.Load(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if loaded.ID != legacy.ID || len(loaded.Messages) != 1 || loaded.Messages[0].Content != "旧格式" {
		t.Fatalf("loaded legacy mismatch: %#v", loaded)
	}
	if _, err := os.Stat(store.path(legacy.ID)); !os.IsNotExist(err) {
		t.Fatalf("Load should not migrate immediately, stat err = %v", err)
	}

	AppendAssistantMessage(loaded, "迁移后回复")
	if err := store.Save(ctx, loaded); err != nil {
		t.Fatalf("save migrated jsonl: %v", err)
	}
	if _, err := os.Stat(store.legacyPath(legacy.ID)); err != nil {
		t.Fatalf("legacy json should remain: %v", err)
	}
	migrated, err := store.Load(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("load migrated jsonl: %v", err)
	}
	if len(migrated.Messages) != 2 || migrated.Messages[1].Content != "迁移后回复" {
		t.Fatalf("migrated messages mismatch: %#v", migrated.Messages)
	}
}

func TestJSONLRecoverLoadsLegacyJSONWithoutDeletingOriginal(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	legacy := NewConversation("legacy-recover", time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(legacy, "旧恢复")
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(store.legacyPath(legacy.ID), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	recovered, report, err := store.Recover(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("recover legacy: %v", err)
	}
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_recovery_legacy_json")
	if recovered.ID != legacy.ID || len(recovered.Messages) != 1 || recovered.Messages[0].Content != "旧恢复" {
		t.Fatalf("recovered legacy mismatch: %#v", recovered)
	}
	if err := store.Save(ctx, recovered); err != nil {
		t.Fatalf("save recovered legacy: %v", err)
	}
	if _, err := os.Stat(store.legacyPath(legacy.ID)); err != nil {
		t.Fatalf("legacy json should remain: %v", err)
	}
	if _, err := os.Stat(store.path(legacy.ID)); err != nil {
		t.Fatalf("jsonl should be created after save: %v", err)
	}
}

func TestJSONLRecoverSkipsBadLinesAndTruncatesUnclosedToolCall(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation := NewConversation("recover", time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(conversation, "开始")
	if err := store.Save(ctx, conversation); err != nil {
		t.Fatalf("save initial: %v", err)
	}

	writeRawText(t, store.path(conversation.ID), `not-json
`)
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: 99, Type: RecordTypeMessage, Message: &Message{Role: RoleUser, Content: "bad version", CreatedAt: time.Date(2026, 7, 5, 10, 1, 0, 0, time.UTC)}})
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 0, CreatedAt: time.Date(2026, 7, 5, 10, 2, 0, 0, time.UTC), Message: &Message{Role: RoleUser, Content: "恢复用户", CreatedAt: time.Date(2026, 7, 5, 10, 2, 0, 0, time.UTC)}})
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 1, CreatedAt: time.Date(2026, 7, 5, 10, 3, 0, 0, time.UTC), Message: &Message{Role: RoleToolResult, ToolCallID: "missing", Content: "伪造结果", CreatedAt: time.Date(2026, 7, 5, 10, 3, 0, 0, time.UTC)}})
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 2, CreatedAt: time.Date(2026, 7, 5, 10, 4, 0, 0, time.UTC), Message: &Message{Role: RoleToolCall, ToolCallID: "open", ToolName: "Read", Content: "Read({})", CreatedAt: time.Date(2026, 7, 5, 10, 4, 0, 0, time.UTC)}})
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 3, CreatedAt: time.Date(2026, 7, 5, 10, 5, 0, 0, time.UTC), Message: &Message{Role: RoleAssistant, Content: "不应保留", CreatedAt: time.Date(2026, 7, 5, 10, 5, 0, 0, time.UTC)}})

	recovered, report, err := store.Recover(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.SkippedLines != 3 {
		t.Fatalf("SkippedLines = %d, want 3, diagnostics=%#v", report.SkippedLines, report.Diagnostics)
	}
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_recovery_bad_line")
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_unknown_version")
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_recovery_unmatched_tool_result")
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_recovery_unclosed_tool_call")
	if report.TruncatedFromMessage != 1 {
		t.Fatalf("TruncatedFromMessage = %d, want 1", report.TruncatedFromMessage)
	}
	if len(recovered.Messages) != 1 || recovered.Messages[0].Content != "恢复用户" {
		t.Fatalf("recovered messages = %#v, want only recovered user before open tool call", recovered.Messages)
	}
}

func TestJSONLRecoverInsertsTimeGapReminder(t *testing.T) {
	ctx := context.Background()
	oldUpdated := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation := NewConversation("gap", oldUpdated)
	message := Message{Role: RoleUser, Content: "很久以前", CreatedAt: oldUpdated}
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 0, CreatedAt: oldUpdated, ConversationTitle: "旧会话", ConversationCreatedAt: oldUpdated, ConversationUpdatedAt: oldUpdated, Message: &message})

	recovered, report, err := store.Recover(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !report.TimeGapReminder {
		t.Fatalf("expected time gap reminder report")
	}
	if len(recovered.Messages) != 2 || recovered.Messages[1].Role != RoleContextBoundary {
		t.Fatalf("expected context boundary reminder appended: %#v", recovered.Messages)
	}
	if err := store.Save(ctx, recovered); err != nil {
		t.Fatalf("save recovered reminder: %v", err)
	}
	if lines := countJSONLLines(t, store.path(conversation.ID)); lines != 2 {
		t.Fatalf("recovered reminder should be appendable, lines = %d, want 2", lines)
	}
}

func TestListIncludesRecoverableCorruptJSONLWithoutLeakingLine(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	path := store.path("corrupt-list")
	badLine := `{"version":1,"type":"message","message":{"role":"user","content":"token=should-not-leak"}`
	writeRawText(t, path, badLine+"\n")

	conversations, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	var placeholder *Conversation
	for index := range conversations {
		if conversations[index].ID == "corrupt-list" {
			placeholder = &conversations[index]
			break
		}
	}
	if placeholder == nil {
		t.Fatalf("List 应包含损坏 JSONL 的可恢复占位: %#v", conversations)
	}
	if !strings.Contains(placeholder.Title, "可恢复") {
		t.Fatalf("占位标题应提示可恢复: %#v", placeholder)
	}
	if placeholder.Context == nil || len(placeholder.Context.RecoveryDiagnostics) == 0 {
		t.Fatalf("占位应包含恢复诊断: %#v", placeholder.Context)
	}
	data, err := json.Marshal(placeholder.Context.RecoveryDiagnostics)
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	text := string(data)
	for _, leaked := range []string{"token=should-not-leak", badLine} {
		if strings.Contains(text, leaked) {
			t.Fatalf("恢复诊断不应包含坏行原文 %q: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "line") || !strings.Contains(text, "recoverable") {
		t.Fatalf("恢复诊断应包含行号和可恢复状态: %s", text)
	}
}

func TestRecoveredCorruptJSONLCanSaveAgain(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	conversation := NewConversation("corrupt-save", time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC))
	AppendUserMessage(conversation, "恢复前")
	appendRawRecord(t, store.path(conversation.ID), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: conversation.ID, MessageIndex: 0, CreatedAt: time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC), ConversationTitle: conversation.Title, ConversationCreatedAt: conversation.CreatedAt, ConversationUpdatedAt: conversation.UpdatedAt, Message: &conversation.Messages[0]})
	badLine := `{"version":1,"type":"message","message":{"role":"user","content":"password=bad-line-secret"}`
	file, err := os.OpenFile(store.path(conversation.ID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open append bad line: %v", err)
	}
	if _, err := file.WriteString(badLine + "\n"); err != nil {
		t.Fatalf("write bad line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close bad line file: %v", err)
	}

	recovered, report, err := store.Recover(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.SkippedLines != 1 {
		t.Fatalf("SkippedLines = %d, want 1", report.SkippedLines)
	}
	AppendAssistantMessage(recovered, "恢复后继续")
	if err := store.Save(ctx, recovered); err != nil {
		t.Fatalf("Recover 后 Save 不应被旧坏行阻断: %v", err)
	}

	records := readJSONLRecordsSkippingBadLines(t, store.path(conversation.ID))
	last := records[len(records)-1]
	if last.Type != RecordTypeSnapshot {
		t.Fatalf("检测到旧坏行后应追加 repair snapshot，last = %q", last.Type)
	}
	assertHasDiagnostic(t, last.Diagnostics, "jsonl_repair_snapshot")
	if last.Snapshot == nil || len(last.Snapshot.Messages) != 2 || last.Snapshot.Messages[1].Content != "恢复后继续" {
		t.Fatalf("repair snapshot 应包含恢复后完整会话: %#v", last.Snapshot)
	}
	data, err := json.Marshal(last.Diagnostics)
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	if strings.Contains(string(data), "password=bad-line-secret") || strings.Contains(string(data), badLine) {
		t.Fatalf("repair diagnostics 不应包含坏行原文: %s", data)
	}
}

func TestJSONLStoreCleanupDeletesExpiredSessionsWithinRoot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	oldConversation := NewConversation("old", now.Add(-40*24*time.Hour))
	oldConversation.Messages = []Message{{Role: RoleUser, Content: "old", CreatedAt: now.Add(-40 * 24 * time.Hour)}}
	oldConversation.UpdatedAt = now.Add(-40 * 24 * time.Hour)
	appendRawRecord(t, store.path("old"), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: oldConversation.ID, MessageIndex: 0, CreatedAt: oldConversation.UpdatedAt, ConversationTitle: oldConversation.Title, ConversationCreatedAt: oldConversation.CreatedAt, ConversationUpdatedAt: oldConversation.UpdatedAt, Message: &oldConversation.Messages[0]})
	newConversation := NewConversation("new", now.Add(-2*24*time.Hour))
	newConversation.Messages = []Message{{Role: RoleUser, Content: "new", CreatedAt: now.Add(-2 * 24 * time.Hour)}}
	newConversation.UpdatedAt = now.Add(-2 * 24 * time.Hour)
	appendRawRecord(t, store.path("new"), JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: newConversation.ID, MessageIndex: 0, CreatedAt: newConversation.UpdatedAt, ConversationTitle: newConversation.Title, ConversationCreatedAt: newConversation.CreatedAt, ConversationUpdatedAt: newConversation.UpdatedAt, Message: &newConversation.Messages[0]})

	report, err := store.Cleanup(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if report.Deleted != 1 || report.Skipped != 0 {
		t.Fatalf("cleanup report = %#v, want deleted=1 skipped=0", report)
	}
	if _, err := os.Stat(store.path("old")); !os.IsNotExist(err) {
		t.Fatalf("old jsonl should be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(store.path("new")); err != nil {
		t.Fatalf("new jsonl should remain: %v", err)
	}
}

func TestJSONLStoreCleanupRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store, err := NewJSONLStore(JSONLStoreOptions{DataDir: t.TempDir(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "escaped.jsonl")
	oldConversation := NewConversation("escaped", now.Add(-40*24*time.Hour))
	oldConversation.Messages = []Message{{Role: RoleUser, Content: "escaped", CreatedAt: now.Add(-40 * 24 * time.Hour)}}
	oldConversation.UpdatedAt = now.Add(-40 * 24 * time.Hour)
	file, err := os.OpenFile(outsideFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open outside: %v", err)
	}
	if err := writeJSONLRecord(file, JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: oldConversation.ID, MessageIndex: 0, CreatedAt: oldConversation.UpdatedAt, ConversationTitle: oldConversation.Title, ConversationCreatedAt: oldConversation.CreatedAt, ConversationUpdatedAt: oldConversation.UpdatedAt, Message: &oldConversation.Messages[0]}); err != nil {
		t.Fatalf("write outside jsonl: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close outside: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(store.dataDir, "escaped.jsonl")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	report, err := store.Cleanup(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if report.Deleted != 0 || report.Skipped != 1 {
		t.Fatalf("cleanup report = %#v, want deleted=0 skipped=1", report)
	}
	assertHasDiagnostic(t, report.Diagnostics, "jsonl_cleanup_path_escape")
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside target should remain: %v", err)
	}
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func assertHasDiagnostic(t *testing.T, items []JSONLDiagnostic, code string) {
	t.Helper()
	for _, item := range items {
		if item.Code == code {
			return
		}
	}
	t.Fatalf("诊断中缺少 code %q: %#v", code, items)
}

func countJSONLLines(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return count
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func appendRawRecord(t *testing.T, path string, record JSONLRecord) {
	t.Helper()
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer file.Close()
	if err := writeJSONLRecord(file, record); err != nil {
		t.Fatalf("append raw record: %v", err)
	}
}

func writeRawText(t *testing.T, path string, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Clean(path), []byte(text), 0o600); err != nil {
		t.Fatalf("write raw text %s: %v", path, err)
	}
}

func readJSONLRecords(t *testing.T, path string) []JSONLRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	var records []JSONLRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record JSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("unmarshal %s: %v", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return records
}

func readJSONLRecordsSkippingBadLines(t *testing.T, path string) []JSONLRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	var records []JSONLRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record JSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return records
}
