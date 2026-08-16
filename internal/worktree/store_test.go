package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validRecordForTest(root string) Record {
	now := time.Date(2026, 8, 16, 1, 2, 3, 0, time.UTC)
	return Record{
		SchemaVersion: RecordSchemaVersion, Revision: 1,
		WorkspaceID: "0123456789abcdef0123456789abcdef", OwnerID: "abcdefabcdefabcdefabcdefabcdefab",
		RepositoryIdentity: RepositoryIdentity{Root: root, CommonDir: filepath.Join(root, ".git"), Digest: strings.Repeat("a", 64)},
		LogicalName:        "review/task", Directory: filepath.Join(root, ".xagent", "worktrees", "tasks", "01", "0123456789abcdef0123456789abcdef"),
		Branch: "xagent/worktree/0123456789abcdef0123456789abcdef", BaseOID: strings.Repeat("b", 40), HeadOID: strings.Repeat("b", 40),
		State: StateReady, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
}

func TestRecordRoundTripAndIntegrity(t *testing.T) {
	record := validRecordForTest(t.TempDir())
	// The domain default must accept every logical name allowed by the
	// resolved configuration default (192 bytes total, 64 per segment).
	record.LogicalName = strings.Join([]string{
		strings.Repeat("a", 50), strings.Repeat("b", 50), strings.Repeat("c", 50),
	}, "/")
	encoded, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.IntegrityDigest == "" || decoded.WorkspaceID != record.WorkspaceID || decoded.Revision != record.Revision {
		t.Fatalf("record 往返错误：%#v", decoded)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	raw["logical_name"] = "tampered"
	tampered, _ := json.Marshal(raw)
	if _, err := DecodeRecord(tampered); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("篡改应被完整性校验拒绝：%v", err)
	}
}

func TestRecordEncodingAcceptsConfiguredLogicalNameAboveDefault(t *testing.T) {
	record := validRecordForTest(t.TempDir())
	record.LogicalName = strings.Join([]string{
		strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200), strings.Repeat("d", 200),
	}, "/")
	if _, err := EncodeRecord(record); err != nil {
		t.Fatalf("record rejected a logical name within the approved hard limits: %v", err)
	}
}

func TestRecordDecodeRejectsUnknownSchemaAndFields(t *testing.T) {
	record := validRecordForTest(t.TempDir())
	encoded, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(encoded, &raw)
	raw["schema_version"] = 99
	unknownVersion, _ := json.Marshal(raw)
	if _, err := DecodeRecord(unknownVersion); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("未知 schema 应拒绝：%v", err)
	}
	raw["schema_version"] = float64(RecordSchemaVersion)
	raw["secret_content"] = "canary"
	unknownField, _ := json.Marshal(raw)
	if _, err := DecodeRecord(unknownField); err == nil {
		t.Fatal("未知/秘密内容字段必须拒绝")
	}
}

func TestRecordEncodeRejectsUnsafeNestedMetadata(t *testing.T) {
	record := validRecordForTest(t.TempDir())
	record.Manifest = ManifestRef{Path: "../manifest.json", Digest: strings.Repeat("c", 64)}
	if _, err := EncodeRecord(record); err == nil {
		t.Fatal("不安全 manifest 引用必须拒绝")
	}
	record = validRecordForTest(t.TempDir())
	record.Lease = LeaseRecord{OwnerID: "not-an-id", Mode: "active", AcquiredAt: record.CreatedAt, Heartbeat: record.CreatedAt}
	if _, err := EncodeRecord(record); err == nil {
		t.Fatal("非法 lease 身份必须拒绝")
	}
	record = validRecordForTest(t.TempDir())
	record.Settlement = SettlementRecord{State: SettlementState("unknown")}
	if _, err := EncodeRecord(record); err == nil {
		t.Fatal("非法 settlement 状态必须拒绝")
	}
	record = validRecordForTest(t.TempDir())
	record.Directory = filepath.Join(record.RepositoryIdentity.Root, "outside")
	if _, err := EncodeRecord(record); err == nil {
		t.Fatal("record 目录必须精确匹配受管 Workspace 路径")
	}
}

func TestManifestRoundTripRejectsUnsafeEntries(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		WorkspaceID:   "0123456789abcdef0123456789abcdef",
		CreatedAt:     time.Date(2026, 8, 16, 1, 2, 3, 0, time.UTC),
		Entries: []ManifestEntry{{
			Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryCreated,
			Size: 12, Digest: strings.Repeat("c", 64), Mode: 0o600, IdentityDigest: strings.Repeat("d", 64),
		}},
	}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil || decoded.IntegrityDigest == "" || len(decoded.Entries) != 1 {
		t.Fatalf("manifest 往返错误：%#v %v", decoded, err)
	}
	manifest.Entries[0].Path = "../secret"
	if _, err := EncodeManifest(manifest); err == nil {
		t.Fatal("manifest 只允许安全相对路径")
	}
	manifest.Entries[0].Path = "config/local.yaml"
	manifest.Entries[0].Mode = os.ModeSetuid | 0o600
	if _, err := EncodeManifest(manifest); err == nil {
		t.Fatal("manifest 不得记录带特殊权限位的产物")
	}
}

func TestNewFileStoreRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real-control")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(parent, "linked-control")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(linkRoot); err == nil {
		t.Fatal("Store 控制根不得是符号链接")
	}
}

func TestFileStoreCreateCASAndTombstone(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(filepath.Join(t.TempDir(), ".control"))
	if err != nil {
		t.Fatal(err)
	}
	record := validRecordForTest(t.TempDir())
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, record); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重复创建应失败：%v", err)
	}
	loaded, err := store.Load(ctx, record.WorkspaceID)
	if err != nil || loaded.Revision != 1 {
		t.Fatalf("加载失败：%#v %v", loaded, err)
	}
	next := loaded.Clone()
	next.State = StateActive
	if err := store.CompareAndSwap(ctx, next, 0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("错误 revision 应冲突：%v", err)
	}
	if err := store.CompareAndSwap(ctx, next, 1); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load(ctx, record.WorkspaceID)
	if err != nil || loaded.Revision != 2 || loaded.State != StateActive {
		t.Fatalf("CAS 未发布新 revision：%#v %v", loaded, err)
	}
	if err := store.WriteTombstone(ctx, Tombstone{SchemaVersion: TombstoneSchemaVersion, WorkspaceID: record.WorkspaceID, DeletedAt: time.Now().UTC(), RecordRevision: loaded.Revision}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Root(), "tombstones", record.WorkspaceID+".json"))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("tombstone 权限不安全：%v %#o", err, info.Mode().Perm())
	}
}

func TestFileStoreFailedWritePreservesOldRecord(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(filepath.Join(t.TempDir(), ".control"))
	if err != nil {
		t.Fatal(err)
	}
	record := validRecordForTest(t.TempDir())
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func(string, string) error { return errors.New("injected interruption") }
	next := record.Clone()
	next.State = StateActive
	if err := store.CompareAndSwap(ctx, next, 1); err == nil {
		t.Fatal("应注入写入中断")
	}
	loaded, err := store.Load(ctx, record.WorkspaceID)
	if err != nil || loaded.Revision != 1 || loaded.State != StateReady {
		t.Fatalf("原子写失败破坏了旧记录：%#v %v", loaded, err)
	}
}

func TestDecodeRecordRejectsOversizedMetadata(t *testing.T) {
	if _, err := DecodeRecord(make([]byte, maxMetadataBytes+1)); !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("超大元数据应拒绝：%v", err)
	}
}
