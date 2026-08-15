package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
)

func testStoreOptions(root string, now time.Time) JSONLStoreOptions {
	return JSONLStoreOptions{
		DataDir: root, Redactor: redact.NewRuntimeRedactor(),
		MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024,
		MaxScanFiles: 1000, MaxScanBytes: 10 * 1024 * 1024,
		RetentionDays: 30, GapReminderDays: 7, Now: func() time.Time { return now },
	}
}

func TestStoreCutoverUsesSingleResultAPI(t *testing.T) {
	typeOfStore := reflect.TypeOf((*Store)(nil)).Elem()
	if typeOfStore.NumMethod() != 5 {
		t.Fatalf("Store methods = %d, want 5", typeOfStore.NumMethod())
	}
	for _, name := range []string{"Create", "List", "Load", "Maintain", "Save"} {
		if _, ok := typeOfStore.MethodByName(name); !ok {
			t.Fatalf("Store missing %s", name)
		}
	}
	if _, ok := any((*JSONLStore)(nil)).(Store); !ok {
		t.Fatal("JSONLStore does not implement Store")
	}
}

func TestJSONLStoreOwnsV2BaselineAndKeepsLegacyRootReadOnly(t *testing.T) {
	now := time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC)
	root := t.TempDir()
	store, err := NewJSONLStore(testStoreOptions(root, now))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	conversation, err := store.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	AppendUserMessage(conversation, "hello")
	first, err := store.Save(context.Background(), conversation)
	if err != nil || first.Kind != SaveSnapshot || first.Persisted.Revision != 1 {
		t.Fatalf("first save = %#v, %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(root, conversation.ID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("legacy root was written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "v2", conversation.ID+".jsonl")); err != nil {
		t.Fatalf("v2 snapshot missing: %v", err)
	}

	AppendAssistantMessage(conversation, "world")
	second, err := store.Save(context.Background(), conversation)
	if err != nil || second.Kind != SaveBatch || second.Persisted.Revision != 2 {
		t.Fatalf("second save = %#v, %v", second, err)
	}
	noop, err := store.Save(context.Background(), conversation)
	if err != nil || noop.Kind != SaveNoop || noop.Persisted != second.Persisted {
		t.Fatalf("noop = %#v, %v", noop, err)
	}

	restarted, err := NewJSONLStore(testStoreOptions(root, now.Add(time.Hour)))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	loaded, err := restarted.Load(context.Background(), conversation.ID)
	if err != nil || !loaded.Available || loaded.Conversation == nil || loaded.Persisted != second.Persisted {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
	AppendAssistantMessage(loaded.Conversation, "again")
	third, err := restarted.Save(context.Background(), loaded.Conversation)
	if err != nil || third.Persisted.Revision != 3 {
		t.Fatalf("save loaded pointer = %#v, %v", third, err)
	}
}

func TestJSONLStoreMigratesLegacyJSONWithoutOverwritingIt(t *testing.T) {
	now := time.Date(2026, 8, 3, 3, 0, 0, 0, time.UTC)
	root := t.TempDir()
	legacy := legacyConversation{ID: "legacy-one", Title: "legacy", Messages: []legacyMessage{{Role: RoleUser, Content: "hello", CreatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	payload, _ := json.Marshal(legacy)
	legacyPath := filepath.Join(root, legacy.ID+".json")
	if err := os.WriteFile(legacyPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewJSONLStore(testStoreOptions(root, now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), legacy.ID)
	if err != nil || !loaded.Available || loaded.Persisted != (PersistedState{}) {
		t.Fatalf("legacy load = %#v, %v", loaded, err)
	}
	if _, err := store.Save(context.Background(), loaded.Conversation); err != nil {
		t.Fatalf("legacy first save: %v", err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || !reflect.DeepEqual(after, payload) {
		t.Fatalf("legacy source changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "v2", legacy.ID+".jsonl")); err != nil {
		t.Fatalf("migrated v2 missing: %v", err)
	}
}

type countingLegacyImporter struct{ calls int }

func (i *countingLegacyImporter) Import(context.Context, string, artifact.Metadata) (artifact.Ref, error) {
	i.calls++
	return artifact.Ref{}, ErrLegacyExternalArtifactRequired
}

func TestListDoesNotImportLegacyExternalArtifact(t *testing.T) {
	now := time.Date(2026, 8, 3, 4, 0, 0, 0, time.UTC)
	root := t.TempDir()
	legacy := legacyConversation{ID: "legacy-external", Title: "legacy", Messages: []legacyMessage{{Role: RoleToolResult, Content: "preview", CreatedAt: now, ToolCallID: "call", ToolName: "Read", ToolResultContent: "preview", ToolResultStatus: "success", Externalized: true, ExternalPath: filepath.Join(root, "source"), ExternalBytes: 1}}, CreatedAt: now, UpdatedAt: now}
	payload, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, legacy.ID+".json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	importer := &countingLegacyImporter{}
	options := testStoreOptions(root, now)
	options.LegacyArtifactImporter = importer
	store, err := NewJSONLStore(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.List(context.Background())
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Available || result.Entries[0].Recovery.Status != RecoveryPlaceholder {
		t.Fatalf("list = %#v, %v", result, err)
	}
	if importer.calls != 0 {
		t.Fatalf("List imported artifact %d times", importer.calls)
	}
}
