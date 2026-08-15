package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/tool"
)

type migrationArtifactSink struct {
	BeginFunc func(context.Context, artifact.Metadata) (artifact.Writer, error)
}

func (s *migrationArtifactSink) Begin(ctx context.Context, metadata artifact.Metadata) (artifact.Writer, error) {
	if s == nil || s.BeginFunc == nil {
		return nil, errors.New("test artifact sink unavailable")
	}
	return s.BeginFunc(ctx, metadata)
}

type migrationArtifactWriter struct {
	WriteFunc  func([]byte) (int, error)
	CommitFunc func(context.Context) (artifact.Ref, error)
	AbortFunc  func() error
}

func (w *migrationArtifactWriter) Write(value []byte) (int, error) {
	if w == nil || w.WriteFunc == nil {
		return 0, errors.New("test artifact writer unavailable")
	}
	return w.WriteFunc(value)
}

func (w *migrationArtifactWriter) Commit(ctx context.Context) (artifact.Ref, error) {
	if w == nil || w.CommitFunc == nil {
		return artifact.Ref{}, errors.New("test artifact commit unavailable")
	}
	return w.CommitFunc(ctx)
}

func (w *migrationArtifactWriter) Abort() error {
	if w == nil || w.AbortFunc == nil {
		return errors.New("test artifact abort unavailable")
	}
	return w.AbortFunc()
}

func TestLegacyPathNeverCrossesMigrationBoundary(t *testing.T) {
	t.Run("import capability has the exact C10 shape", func(t *testing.T) {
		boundary := reflect.TypeOf((*LegacyArtifactImporter)(nil)).Elem()
		if boundary.Kind() != reflect.Interface || boundary.NumMethod() != 1 {
			t.Fatalf("LegacyArtifactImporter = %v with %d methods, want one-method interface", boundary.Kind(), boundary.NumMethod())
		}
		method := boundary.Method(0)
		if method.Name != "Import" || method.Type.NumIn() != 3 || method.Type.NumOut() != 2 {
			t.Fatalf("LegacyArtifactImporter method = %s %v, want exact Import signature", method.Name, method.Type)
		}
		wantInputs := []reflect.Type{
			reflect.TypeOf((*context.Context)(nil)).Elem(),
			reflect.TypeOf(""),
			reflect.TypeOf(artifact.Metadata{}),
		}
		for index, want := range wantInputs {
			if got := method.Type.In(index); got != want {
				t.Fatalf("Import input %d = %v, want %v", index, got, want)
			}
		}
		if got := method.Type.Out(0); got != reflect.TypeOf(artifact.Ref{}) {
			t.Fatalf("Import result = %v, want artifact.Ref", got)
		}
		if got := method.Type.Out(1); got != reflect.TypeOf((*error)(nil)).Elem() {
			t.Fatalf("Import error = %v, want error", got)
		}
	})

	t.Run("migration owns no artifact store or path capability", func(t *testing.T) {
		migrationType := reflect.TypeOf(legacyMigration{})
		if migrationType.NumField() != 2 {
			t.Fatalf("legacyMigration fields = %d, want codec and LegacyArtifactImporter", migrationType.NumField())
		}
		codecField := migrationType.Field(0)
		if codecField.Name != "codec" || codecField.Type != reflect.TypeOf((*V2RecordCodec)(nil)) {
			t.Fatalf("legacyMigration field 0 = %s %v, want codec *V2RecordCodec", codecField.Name, codecField.Type)
		}
		field := migrationType.Field(1)
		want := reflect.TypeOf((*LegacyArtifactImporter)(nil)).Elem()
		if field.Name != "artifactImporter" || field.Type != want {
			t.Fatalf("legacyMigration field = %s %v, want artifactImporter %v", field.Name, field.Type, want)
		}
		storeType := reflect.TypeOf((*artifact.Store)(nil)).Elem()
		for index := 0; index < migrationType.NumField(); index++ {
			fieldType := migrationType.Field(index).Type
			if fieldType == storeType || fieldType.Implements(storeType) {
				t.Fatalf("migration field %d exposes full artifact.Store: %v", index, fieldType)
			}
		}
	})

	t.Run("quarantined external path has no safe output channel", func(t *testing.T) {
		const (
			sessionID = "legacy-path-boundary"
			canary    = "private-legacy-path-canary"
		)
		legacyPath := filepath.Join(t.TempDir(), canary)
		createdAt := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
		legacy := legacyConversation{
			ID: sessionID,
			Messages: []legacyMessage{{
				Role: RoleToolResult, Content: "safe preview", CreatedAt: createdAt,
				ToolCallID: "call-1", ToolName: "Read", ToolResultContent: "safe preview",
				ToolResultStatus: "success", Externalized: true, ExternalPath: legacyPath,
				ExternalBytes: 12, ExternalPreview: "safe preview",
			}},
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}
		payload, err := json.Marshal(legacy)
		if err != nil {
			t.Fatalf("marshal legacy fixture: %v", err)
		}
		codec, err := NewV2RecordCodec(V2RecordCodecOptions{
			MaxRecordBytes:  64 * 1024,
			MaxSessionBytes: 128 * 1024,
			Redactor:        redact.NewRuntimeRedactor(),
		})
		if err != nil {
			t.Fatalf("new codec: %v", err)
		}

		loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(payload), legacyLoadOptions{
			Format: legacyFormatJSON, ExpectedSessionID: sessionID,
		})
		if loaded != nil {
			t.Fatal("migration published v2 state for a quarantined path")
		}
		if loadErr != ErrLegacyExternalArtifactRequired {
			t.Fatal("migration did not return the exact safe external-artifact sentinel")
		}
		if strings.Contains(loadErr.Error(), legacyPath) || strings.Contains(loadErr.Error(), canary) {
			t.Fatal("legacy path crossed the migration error boundary")
		}
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			t.Fatal("legacy path was changed before the T3.17 importer")
		}
	})
}

func TestLegacyExternalPathImportIsNonDestructive(t *testing.T) {
	const (
		sessionID     = "legacy-artifact-session"
		pathCanary    = "legacy-private-path-canary"
		payloadCanary = "legacy-private-payload-canary"
	)
	createdAt := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	legacyRoot := filepath.Join(root, "legacy")
	workspaceRoot := filepath.Join(root, "workspace")
	artifactRoot := filepath.Join(root, "artifacts")
	for _, directory := range []string{legacyRoot, workspaceRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal("create T3.17 directory")
		}
	}
	sourceDir := filepath.Join(legacyRoot, "context_blobs", sessionID)
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal("create legacy artifact directory")
	}
	sourcePath := filepath.Join(sourceDir, "0001_"+pathCanary+".json")
	sourcePayload := []byte(`{"raw":"` + payloadCanary + `"}`)
	writeT317File(t, sourcePath, sourcePayload)

	legacy := t317LegacyConversation(sessionID, sourcePath, int64(len(sourcePayload)), createdAt)
	recordPayload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal("marshal T3.17 legacy record")
	}
	recordPath := filepath.Join(legacyRoot, sessionID+".json")
	writeT317File(t, recordPath, recordPayload)
	sourceBefore := captureT315Source(t, sourcePath)
	recordBefore := captureT315Source(t, recordPath)

	opened, err := safefs.Bootstrap(legacyRoot, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap legacy safefs root")
	}
	defer opened.Root.Close()
	store, err := artifact.NewFileStore(artifact.FileStoreOptions{
		Root: artifactRoot, WorkspaceRoot: workspaceRoot,
		MaxFileBytes: 1024 * 1024, MaxTotalBytes: 2 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal("create private artifact store")
	}
	defer store.Close()
	importer, err := NewLegacyArtifactImporter(opened.Root, legacyRoot, store)
	if err != nil {
		t.Fatal("create legacy artifact importer")
	}
	codec := mustT317Codec(t)
	migration := legacyMigration{codec: codec, artifactImporter: importer}

	record, err := os.Open(recordPath)
	if err != nil {
		t.Fatal("open legacy record")
	}
	result, loadErr := migration.loadResult(context.Background(), record, legacyLoadOptions{
		Format: legacyFormatJSON, ExpectedSessionID: sessionID,
	})
	if loadErr != nil {
		_ = record.Close()
		t.Fatal("load legacy record with external artifact")
	}
	assertT315ReaderBorrowed(t, record)
	assertT315SourceUnchanged(t, sourcePath, sourceBefore)
	assertT315SourceUnchanged(t, recordPath, recordBefore)
	if !result.Available || result.Conversation == nil || result.Recovery.Status != RecoveryClean ||
		len(result.Recovery.Diagnostics) != 0 || result.Persisted != (PersistedState{}) {
		t.Fatal("successful import did not return a clean unpersisted migration")
	}
	if len(result.Conversation.Messages) != 2 || result.Conversation.Messages[1].Tool == nil ||
		result.Conversation.Messages[1].Tool.Artifact == nil {
		t.Fatal("successful import did not publish an opaque artifact reference")
	}
	toolState := result.Conversation.Messages[1].Tool
	ref := *toolState.Artifact
	if !validLegacyImportedArtifact(ref) || ref.Bytes != int64(len(sourcePayload)) || !toolState.Truncated ||
		toolState.TruncationReason.Text() != "legacy_output_externalized" {
		t.Fatal("imported artifact state is invalid")
	}
	artifactReader, openedRef, err := store.OpenForUser(context.Background(), ref.ID)
	if err != nil {
		t.Fatal("open imported artifact")
	}
	importedPayload, readErr := io.ReadAll(artifactReader)
	closeErr := artifactReader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(importedPayload, sourcePayload) || openedRef != ref {
		t.Fatal("imported artifact payload or reference changed")
	}

	plan, err := classifyV2Save(v2SaveClassificationInput{
		Conversation: result.Conversation,
		Persisted:    result.Persisted,
		Recovery:     result.Recovery.Status,
	})
	if err != nil || plan.Result.Kind != SaveSnapshot || plan.Result.Persisted.Revision != 1 || plan.Record == nil {
		t.Fatal("first post-import save is not a revision-1 snapshot")
	}
	encoded, err := codec.Encode(sessionID, *plan.Record)
	if err != nil {
		t.Fatal("encode first post-import snapshot")
	}
	for _, forbidden := range []string{sourcePath, pathCanary, payloadCanary} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatal("legacy path or private payload crossed into v2 JSONL")
		}
	}
	assertT315SourceUnchanged(t, sourcePath, sourceBefore)
	assertT315SourceUnchanged(t, recordPath, recordBefore)

	t.Run("failure returns a safe placeholder and finalizes staging", func(t *testing.T) {
		cases := []struct {
			name        string
			beginFails  bool
			writeFails  bool
			commitFails bool
			wantBegin   int
			wantAbort   int
			wantCommit  int
		}{
			{name: "begin failure", beginFails: true, wantBegin: 1},
			{name: "write failure", writeFails: true, wantBegin: 1, wantAbort: 1},
			{name: "commit failure", commitFails: true, wantBegin: 1, wantCommit: 1},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				beginCalls, abortCalls, commitCalls := 0, 0, 0
				var observedMetadata artifact.Metadata
				writer := &migrationArtifactWriter{
					WriteFunc: func(payload []byte) (int, error) {
						if testCase.writeFails {
							return 0, errors.New("write " + sourcePath + " " + payloadCanary)
						}
						return len(payload), nil
					},
					CommitFunc: func(context.Context) (artifact.Ref, error) {
						commitCalls++
						if testCase.commitFails {
							return artifact.Ref{}, errors.New("commit " + sourcePath + " " + payloadCanary)
						}
						return t317ValidRef(int64(len(sourcePayload)), createdAt), nil
					},
					AbortFunc: func() error {
						abortCalls++
						return nil
					},
				}
				sink := &migrationArtifactSink{BeginFunc: func(_ context.Context, metadata artifact.Metadata) (artifact.Writer, error) {
					beginCalls++
					observedMetadata = metadata
					if testCase.beginFails {
						return nil, errors.New("begin " + sourcePath + " " + payloadCanary)
					}
					return writer, nil
				}}
				failingImporter, importerErr := NewLegacyArtifactImporter(opened.Root, legacyRoot, sink)
				if importerErr != nil {
					t.Fatal("create failing importer fixture")
				}
				failureResult, failureErr := (legacyMigration{codec: codec, artifactImporter: failingImporter}).loadResult(
					context.Background(), bytes.NewReader(recordPayload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID},
				)
				if failureErr != nil {
					t.Fatal("recoverable import failure returned a top-level error")
				}
				assertT317PlaceholderNoLeak(t, failureResult, sourcePath, pathCanary, payloadCanary)
				if beginCalls != testCase.wantBegin || abortCalls != testCase.wantAbort || commitCalls != testCase.wantCommit {
					t.Fatal("artifact writer lifecycle mismatch")
				}
				if beginCalls > 0 && observedMetadata != (artifact.Metadata{MediaType: legacyArtifactMediaType}) {
					t.Fatal("legacy importer passed unsafe artifact metadata")
				}
				assertT315SourceUnchanged(t, sourcePath, sourceBefore)
				assertT315SourceUnchanged(t, recordPath, recordBefore)
			})
		}
	})

	t.Run("untrusted importer error and ref cannot cross the boundary", func(t *testing.T) {
		cases := []struct {
			name     string
			importer LegacyArtifactImporter
		}{
			{
				name: "unsafe error",
				importer: t317ImporterFunc(func(context.Context, string, artifact.Metadata) (artifact.Ref, error) {
					return artifact.Ref{}, errors.New("unsafe " + sourcePath + " " + payloadCanary)
				}),
			},
			{
				name: "path smuggled in ref",
				importer: t317ImporterFunc(func(context.Context, string, artifact.Metadata) (artifact.Ref, error) {
					ref := t317ValidRef(int64(len(sourcePayload)), createdAt)
					ref.ID = pathCanary
					return ref, nil
				}),
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				failureResult, failureErr := (legacyMigration{codec: codec, artifactImporter: testCase.importer}).loadResult(
					context.Background(), bytes.NewReader(recordPayload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID},
				)
				if failureErr != nil {
					t.Fatal("untrusted importer failure returned a top-level error")
				}
				assertT317PlaceholderNoLeak(t, failureResult, sourcePath, pathCanary, payloadCanary)
			})
		}
	})

	t.Run("outside path is rejected before artifact staging", func(t *testing.T) {
		outsidePath := filepath.Join(root, "outside-"+pathCanary+".json")
		writeT317File(t, outsidePath, sourcePayload)
		outsideBefore := captureT315Source(t, outsidePath)
		outsideLegacy := t317LegacyConversation(sessionID, outsidePath, int64(len(sourcePayload)), createdAt)
		outsidePayload, marshalErr := json.Marshal(outsideLegacy)
		if marshalErr != nil {
			t.Fatal("marshal outside-path fixture")
		}
		beginCalls := 0
		sink := &migrationArtifactSink{BeginFunc: func(context.Context, artifact.Metadata) (artifact.Writer, error) {
			beginCalls++
			return nil, errors.New("unexpected begin")
		}}
		outsideImporter, importerErr := NewLegacyArtifactImporter(opened.Root, legacyRoot, sink)
		if importerErr != nil {
			t.Fatal("create outside-path importer")
		}
		failureResult, failureErr := (legacyMigration{codec: codec, artifactImporter: outsideImporter}).loadResult(
			context.Background(), bytes.NewReader(outsidePayload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID},
		)
		if failureErr != nil || beginCalls != 0 {
			t.Fatal("outside path crossed into artifact staging")
		}
		assertT317PlaceholderNoLeak(t, failureResult, outsidePath, pathCanary, payloadCanary)
		assertT315SourceUnchanged(t, outsidePath, outsideBefore)
	})

	t.Run("v1 replay imports only the final external state", func(t *testing.T) {
		later := legacyMessage{Role: RoleAssistant, Content: "after external snapshot", CreatedAt: createdAt.Add(4 * time.Second)}
		v1Payload := encodeT315V1(t,
			JSONLRecord{
				Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 0,
				CreatedAt: legacy.UpdatedAt, Snapshot: &legacy,
			},
			JSONLRecord{
				Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: len(legacy.Messages),
				CreatedAt: later.CreatedAt, ConversationUpdatedAt: later.CreatedAt, Message: &later,
			},
		)
		importCalls := 0
		v1Importer := t317ImporterFunc(func(_ context.Context, path string, metadata artifact.Metadata) (artifact.Ref, error) {
			importCalls++
			if path != sourcePath || metadata != (artifact.Metadata{MediaType: legacyArtifactMediaType}) {
				return artifact.Ref{}, errors.New("unsafe importer input")
			}
			return t317ValidRef(int64(len(sourcePayload)), createdAt), nil
		})
		v1Result, v1Err := (legacyMigration{codec: codec, artifactImporter: v1Importer}).loadResult(
			context.Background(), bytes.NewReader(v1Payload), legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID},
		)
		if v1Err != nil || !v1Result.Available || v1Result.Conversation == nil ||
			len(v1Result.Conversation.Messages) != 3 || importCalls != 1 {
			t.Fatal("v1 replay did not import the final external state exactly once")
		}
	})

	t.Run("invalid later state fails before any import side effect", func(t *testing.T) {
		invalid := legacy
		invalid.Messages = append(cloneLegacyMessages(legacy.Messages), legacyMessage{
			Role: RoleToolResult, Content: "forged", ToolResultContent: "forged",
			CreatedAt: createdAt.Add(4 * time.Second), ToolCallID: "missing", ToolName: "Read",
			ToolResultStatus: "success",
		})
		invalid.UpdatedAt = createdAt.Add(5 * time.Second)
		invalidPayload, marshalErr := json.Marshal(invalid)
		if marshalErr != nil {
			t.Fatal("marshal invalid post-import fixture")
		}
		importCalls := 0
		noSideEffectImporter := t317ImporterFunc(func(context.Context, string, artifact.Metadata) (artifact.Ref, error) {
			importCalls++
			return t317ValidRef(int64(len(sourcePayload)), createdAt), nil
		})
		invalidResult, invalidErr := (legacyMigration{codec: codec, artifactImporter: noSideEffectImporter}).loadResult(
			context.Background(), bytes.NewReader(invalidPayload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID},
		)
		if invalidErr != ErrLegacyMigrationFormat || !reflect.DeepEqual(invalidResult, LoadResult{}) || importCalls != 0 {
			t.Fatal("invalid legacy state reached the importer or returned an unsafe error")
		}
	})

	t.Run("cancellation stops before staging and is not a placeholder", func(t *testing.T) {
		beginCalls := 0
		sink := &migrationArtifactSink{BeginFunc: func(context.Context, artifact.Metadata) (artifact.Writer, error) {
			beginCalls++
			return nil, errors.New("unexpected begin")
		}}
		cancelImporter, importerErr := NewLegacyArtifactImporter(opened.Root, legacyRoot, sink)
		if importerErr != nil {
			t.Fatal("create cancellation importer")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cancelResult, cancelErr := (legacyMigration{codec: codec, artifactImporter: cancelImporter}).loadResult(
			ctx, bytes.NewReader(recordPayload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID},
		)
		if !errors.Is(cancelErr, context.Canceled) || !reflect.DeepEqual(cancelResult, LoadResult{}) || beginCalls != 0 {
			t.Fatal("canceled migration staged an artifact or returned a placeholder")
		}
	})
}

func TestLegacyLoadDoesNotModifySource(t *testing.T) {
	const (
		sessionID = "legacy-session"
		canary    = "legacy-secret-canary"
	)
	createdAt := time.Date(2026, time.July, 1, 10, 0, 0, 123456789, time.FixedZone("legacy", 8*60*60))
	updatedAt := createdAt.Add(5 * time.Minute)
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(canary)
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{
		MaxRecordBytes:  256 * 1024,
		MaxSessionBytes: 1024 * 1024,
		Redactor:        redactor,
	})
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}

	legacy := legacyConversation{
		ID:    sessionID,
		Title: "title " + canary,
		Messages: []legacyMessage{
			{Role: RoleUser, Content: "question " + canary, CreatedAt: createdAt.Add(time.Second)},
			{Role: RoleToolCall, Content: "Read(...) " + canary, CreatedAt: createdAt.Add(2 * time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{"path":"` + canary + `"}`},
			{
				Role: RoleToolResult, Content: "result " + canary, CreatedAt: createdAt.Add(3 * time.Second),
				ToolCallID: "call-1", ToolName: "Read", ToolResultContent: "result " + canary,
				ToolResultStatus: "error", ToolResultSummary: "summary " + canary, ToolResultTruncated: true,
				ToolResultData:  json.RawMessage(`{"raw":"` + canary + `"}`),
				ToolResultError: json.RawMessage(`{"code":"read_failed","message":"failure ` + canary + `","recoverable":true}`),
				ToolErrorCode:   "read_failed",
			},
			{Role: RoleContextSummary, Content: "context " + canary, CreatedAt: createdAt.Add(4 * time.Second)},
		},
		Context: &legacyContextMetadata{
			Summary: "summary " + canary, LastBoundary: "boundary " + canary,
			LastCompressionAt: timePointer(createdAt.Add(4 * time.Minute)), SummaryFailureCount: 1,
			LastInputTokens: 10, LastOutputTokens: 20, LastEstimatedTokens: 30, LastEstimatedCharacters: 40,
			RecoveryDiagnostics: []JSONLDiagnostic{
				diagnostics.New("legacy_raw", diagnostics.SeverityWarning, canary).WithPath("/private/" + canary),
			},
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}

	t.Run("legacy JSON converts into safe unpersisted v2 state", func(t *testing.T) {
		data, marshalErr := json.Marshal(legacy)
		if marshalErr != nil {
			t.Fatalf("marshal legacy JSON: %v", marshalErr)
		}
		path := writeT315Source(t, sessionID+".json", data)
		before := captureT315Source(t, path)

		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open source: %v", openErr)
		}
		loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, file, legacyLoadOptions{
			Format: legacyFormatJSON, ExpectedSessionID: sessionID,
		})
		if loadErr != nil {
			_ = file.Close()
			t.Fatalf("load legacy JSON: %v", loadErr)
		}
		assertT315ReaderBorrowed(t, file)
		assertT315SourceUnchanged(t, path, before)
		assertT315SafeConversion(t, loaded, redactor, sessionID, canary, createdAt, updatedAt)

		plan, classifyErr := classifyV2Save(v2SaveClassificationInput{
			Conversation: loaded,
			Persisted:    PersistedState{},
			Recovery:     RecoveryClean,
		})
		if classifyErr != nil {
			t.Fatalf("classify first migrated save: %v", classifyErr)
		}
		if plan.Result.Kind != SaveSnapshot || plan.Result.Persisted.Revision != 1 || plan.Record == nil || plan.Record.Kind != RecordSnapshot {
			t.Fatalf("first save plan = %#v, want revision-1 snapshot", plan)
		}
		assertT315SourceUnchanged(t, path, before)
	})

	t.Run("v1 JSONL replays snapshot and later messages deterministically", func(t *testing.T) {
		first := legacyMessage{Role: RoleUser, Content: "discarded prefix", CreatedAt: createdAt.Add(time.Second)}
		snapshot := legacy
		snapshot.Messages = cloneLegacyMessages(legacy.Messages[:1])
		snapshot.Title = "snapshot " + canary
		snapshot.UpdatedAt = createdAt.Add(3 * time.Minute)
		last := legacyMessage{Role: RoleAssistant, Content: "after snapshot " + canary, CreatedAt: createdAt.Add(4 * time.Minute)}
		records := []JSONLRecord{
			{
				Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 0,
				CreatedAt: first.CreatedAt, ConversationTitle: "prefix", ConversationCreatedAt: createdAt,
				ConversationUpdatedAt: first.CreatedAt, Message: &first,
			},
			{
				Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 1,
				CreatedAt: snapshot.UpdatedAt, Snapshot: &snapshot,
				Diagnostics: []JSONLDiagnostic{diagnostics.New("legacy_drop", diagnostics.SeverityWarning, canary).WithPath("/private/" + canary)},
			},
			{
				Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 1,
				CreatedAt: last.CreatedAt, ConversationUpdatedAt: last.CreatedAt, Message: &last,
			},
		}
		data := encodeT315V1(t, records...)
		data = bytes.TrimSuffix(data, []byte{'\n'}) // Historical v1 accepted a complete final record without LF.
		path := writeT315Source(t, sessionID+".jsonl", data)
		before := captureT315Source(t, path)

		load := func() *Conversation {
			file, openErr := os.Open(path)
			if openErr != nil {
				t.Fatalf("open v1 source: %v", openErr)
			}
			loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, file, legacyLoadOptions{
				Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID,
			})
			if loadErr != nil {
				_ = file.Close()
				t.Fatalf("load v1 JSONL: %v", loadErr)
			}
			assertT315ReaderBorrowed(t, file)
			return loaded
		}
		firstLoad := load()
		secondLoad := load()
		if !reflect.DeepEqual(firstLoad, secondLoad) {
			t.Fatalf("v1 replay is not deterministic:\nfirst=%#v\nsecond=%#v", firstLoad, secondLoad)
		}
		if firstLoad.Title.Text() == "prefix" || len(firstLoad.Messages) != 2 || firstLoad.Messages[0].Content.Text() == "discarded prefix" || !strings.Contains(firstLoad.Messages[1].Content.Text(), "after snapshot") {
			t.Fatalf("snapshot replay mismatch: %#v", firstLoad)
		}
		if strings.Contains(firstLoad.Title.Text(), canary) || strings.Contains(firstLoad.Messages[1].Content.Text(), canary) {
			t.Fatal("v1 replay retained secret canary")
		}
		assertT315SourceUnchanged(t, path, before)
	})

	t.Run("success and failure preserve bytes mode mtime path and directory", func(t *testing.T) {
		valid, marshalErr := json.Marshal(legacy)
		if marshalErr != nil {
			t.Fatalf("marshal valid source: %v", marshalErr)
		}
		cases := []struct {
			name    string
			format  legacyConversationFormat
			payload []byte
			wantErr error
		}{
			{name: "JSON success", format: legacyFormatJSON, payload: valid},
			{name: "JSON malformed", format: legacyFormatJSON, payload: []byte(`{"id":"legacy-session","title":"` + canary), wantErr: ErrLegacyMigrationFormat},
			{name: "v1 malformed", format: legacyFormatJSONLV1, payload: []byte(`{"version":1,"type":"message","error":"` + canary + `"}`), wantErr: ErrLegacyMigrationFormat},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				path := writeT315Source(t, "source.data", testCase.payload)
				before := captureT315Source(t, path)
				file, openErr := os.Open(path)
				if openErr != nil {
					t.Fatalf("open source: %v", openErr)
				}
				loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, file, legacyLoadOptions{
					Format: testCase.format, ExpectedSessionID: sessionID,
				})
				if testCase.wantErr == nil {
					if loadErr != nil || loaded == nil {
						t.Fatalf("load = (%#v, %v), want success", loaded, loadErr)
					}
				} else {
					if !errors.Is(loadErr, testCase.wantErr) || loaded != nil {
						t.Fatalf("load = (%#v, %v), want nil and %v", loaded, loadErr, testCase.wantErr)
					}
					if strings.Contains(loadErr.Error(), canary) || strings.Contains(loadErr.Error(), path) {
						t.Fatalf("unsafe migration error: %q", loadErr)
					}
				}
				assertT315ReaderBorrowed(t, file)
				assertT315SourceUnchanged(t, path, before)
			})
		}
	})

	t.Run("external path is quarantined for T3.16 without being read", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "must-not-be-read-"+canary)
		legacyExternal := legacy
		legacyExternal.Messages = []legacyMessage{{
			Role: RoleToolResult, Content: "preview", CreatedAt: createdAt.Add(time.Second), ToolCallID: "call-1", ToolName: "Read",
			ToolResultContent: "preview", ToolResultStatus: "success", Externalized: true,
			ExternalPath: outside, ExternalBytes: 42, ExternalPreview: "preview",
		}}
		data, marshalErr := json.Marshal(legacyExternal)
		if marshalErr != nil {
			t.Fatalf("marshal external source: %v", marshalErr)
		}
		path := writeT315Source(t, sessionID+".json", data)
		before := captureT315Source(t, path)
		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open external source: %v", openErr)
		}
		loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, file, legacyLoadOptions{
			Format: legacyFormatJSON, ExpectedSessionID: sessionID,
		})
		if loaded != nil || !errors.Is(loadErr, ErrLegacyExternalArtifactRequired) {
			t.Fatalf("external load = (%#v, %v), want artifact-required sentinel", loaded, loadErr)
		}
		if strings.Contains(loadErr.Error(), outside) || strings.Contains(loadErr.Error(), canary) {
			t.Fatalf("external path leaked through error: %q", loadErr)
		}
		if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
			t.Fatalf("external path unexpectedly touched: %v", statErr)
		}
		assertT315ReaderBorrowed(t, file)
		assertT315SourceUnchanged(t, path, before)
	})

	t.Run("format confusion and unsafe legacy fields fail closed", func(t *testing.T) {
		legacyJSON, marshalErr := json.Marshal(legacy)
		if marshalErr != nil {
			t.Fatalf("marshal legacy JSON: %v", marshalErr)
		}
		v1 := encodeT315V1(t, JSONLRecord{
			Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 0,
			CreatedAt: updatedAt, Snapshot: &legacy,
		})
		v2 := []byte(`{"version":2,"kind":"snapshot","session_id":"legacy-session"}` + "\n")
		negative := legacy
		negative.Context = &legacyContextMetadata{LastInputTokens: -1}
		negativeJSON, marshalErr := json.Marshal(negative)
		if marshalErr != nil {
			t.Fatalf("marshal negative fixture: %v", marshalErr)
		}
		mismatched := legacy
		mismatched.ID = "different-session"
		mismatchedJSON, marshalErr := json.Marshal(mismatched)
		if marshalErr != nil {
			t.Fatalf("marshal mismatched fixture: %v", marshalErr)
		}
		mismatchedSnapshot := legacy
		mismatchedSnapshot.ID = "different-session"
		cases := []struct {
			name    string
			format  legacyConversationFormat
			payload []byte
		}{
			{name: "v1 as JSON", format: legacyFormatJSON, payload: v1},
			{name: "JSON as v1", format: legacyFormatJSONLV1, payload: legacyJSON},
			{name: "v2 as v1", format: legacyFormatJSONLV1, payload: v2},
			{name: "second JSON value", format: legacyFormatJSON, payload: append(append([]byte(nil), legacyJSON...), []byte(" {}")...)},
			{name: "negative context", format: legacyFormatJSON, payload: negativeJSON},
			{name: "unknown field", format: legacyFormatJSON, payload: []byte(`{"id":"legacy-session","unknown":"` + canary + `"}`)},
			{name: "JSON session mismatch", format: legacyFormatJSON, payload: mismatchedJSON},
			{name: "v1 record session mismatch", format: legacyFormatJSONLV1, payload: encodeT315V1(t, JSONLRecord{Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: "different-session", MessageIndex: 0, CreatedAt: updatedAt, Snapshot: &legacy})},
			{name: "v1 snapshot session mismatch", format: legacyFormatJSONLV1, payload: encodeT315V1(t, JSONLRecord{Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 0, CreatedAt: updatedAt, Snapshot: &mismatchedSnapshot})},
			{name: "v1 duplicate message index", format: legacyFormatJSONLV1, payload: encodeT315V1(t,
				JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 0, CreatedAt: createdAt.Add(time.Second), ConversationCreatedAt: createdAt, ConversationUpdatedAt: createdAt.Add(time.Second), Message: &legacyMessage{Role: RoleUser, Content: "first", CreatedAt: createdAt.Add(time.Second)}},
				JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 0, CreatedAt: createdAt.Add(2 * time.Second), ConversationUpdatedAt: createdAt.Add(2 * time.Second), Message: &legacyMessage{Role: RoleAssistant, Content: "duplicate", CreatedAt: createdAt.Add(2 * time.Second)}},
			)},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(testCase.payload), legacyLoadOptions{
					Format: testCase.format, ExpectedSessionID: sessionID,
				})
				if loaded != nil || !errors.Is(loadErr, ErrLegacyMigrationFormat) {
					t.Fatalf("load = (%#v, %v), want safe format failure", loaded, loadErr)
				}
				if strings.Contains(loadErr.Error(), canary) {
					t.Fatalf("canary leaked through error: %q", loadErr)
				}
			})
		}
	})

	t.Run("empty legacy IDs bind to the expected session", func(t *testing.T) {
		emptyJSON := legacy
		emptyJSON.ID = ""
		payload, marshalErr := json.Marshal(emptyJSON)
		if marshalErr != nil {
			t.Fatalf("marshal empty JSON ID: %v", marshalErr)
		}
		loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(payload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID})
		if loadErr != nil || loaded == nil || loaded.ID != sessionID {
			t.Fatalf("empty JSON ID load = (%#v, %v)", loaded, loadErr)
		}

		emptySnapshot := legacy
		emptySnapshot.ID = ""
		payload = encodeT315V1(t, JSONLRecord{Version: JSONLVersion, Type: RecordTypeSnapshot, MessageIndex: 0, CreatedAt: updatedAt, Snapshot: &emptySnapshot})
		loaded, loadErr = loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(payload), legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID})
		if loadErr != nil || loaded == nil || loaded.ID != sessionID {
			t.Fatalf("empty v1 IDs load = (%#v, %v)", loaded, loadErr)
		}
	})

	t.Run("v1 tool bindings survive message and snapshot boundaries", func(t *testing.T) {
		call := legacyMessage{Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`}
		result := legacyMessage{Role: RoleToolResult, Content: "ok", ToolResultContent: "ok", CreatedAt: createdAt.Add(2 * time.Second), ToolCallID: "call-1", ToolResultStatus: "success"}
		messageRecords := encodeT315V1(t,
			JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 0, CreatedAt: call.CreatedAt, ConversationTitle: "tools", ConversationCreatedAt: createdAt, ConversationUpdatedAt: call.CreatedAt, Message: &call},
			JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 1, CreatedAt: result.CreatedAt, ConversationUpdatedAt: result.CreatedAt, Message: &result},
		)
		loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(messageRecords), legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID})
		if loadErr != nil || loaded == nil || len(loaded.Messages) != 2 || loaded.Messages[1].Tool == nil || loaded.Messages[1].Tool.Name != "Read" {
			t.Fatalf("cross-record tool load = (%#v, %v)", loaded, loadErr)
		}

		snapshot := legacyConversation{ID: sessionID, Title: "tools", Messages: []legacyMessage{call}, CreatedAt: createdAt, UpdatedAt: call.CreatedAt}
		snapshotThenResult := encodeT315V1(t,
			JSONLRecord{Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 0, CreatedAt: call.CreatedAt, Snapshot: &snapshot},
			JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 1, CreatedAt: result.CreatedAt, ConversationUpdatedAt: result.CreatedAt, Message: &result},
		)
		loaded, loadErr = loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(snapshotThenResult), legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID})
		if loadErr != nil || loaded == nil || len(loaded.Messages) != 2 || loaded.Messages[1].Tool == nil || loaded.Messages[1].Tool.Name != "Read" {
			t.Fatalf("snapshot tool binding load = (%#v, %v)", loaded, loadErr)
		}

		secondResult := result
		secondResult.CreatedAt = createdAt.Add(3 * time.Second)
		duplicateResult := append(append([]byte(nil), messageRecords...), encodeT315V1(t,
			JSONLRecord{Version: JSONLVersion, Type: RecordTypeMessage, SessionID: sessionID, MessageIndex: 2, CreatedAt: secondResult.CreatedAt, ConversationUpdatedAt: secondResult.CreatedAt, Message: &secondResult},
		)...)
		loaded, loadErr = loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(duplicateResult), legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID})
		if loaded != nil || !errors.Is(loadErr, ErrLegacyMigrationFormat) {
			t.Fatalf("duplicate v1 result = (%#v, %v), want format failure", loaded, loadErr)
		}
	})

	t.Run("history integrity rejects forged tool links and message times", func(t *testing.T) {
		fixtures := []struct {
			name         string
			messages     []legacyMessage
			fixtureStart time.Time
			fixtureEnd   time.Time
		}{
			{
				name: "duplicate tool call ID",
				messages: []legacyMessage{
					{Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`},
					{Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(2 * time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`},
				},
				fixtureStart: createdAt, fixtureEnd: updatedAt,
			},
			{
				name: "unmatched tool result",
				messages: []legacyMessage{
					{Role: RoleToolResult, Content: "forged", ToolResultContent: "forged", CreatedAt: createdAt.Add(time.Second), ToolCallID: "missing", ToolName: "Read", ToolResultStatus: "success"},
				},
				fixtureStart: createdAt, fixtureEnd: updatedAt,
			},
			{
				name: "tool name mismatch",
				messages: []legacyMessage{
					{Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`},
					{Role: RoleToolResult, Content: "forged", ToolResultContent: "forged", CreatedAt: createdAt.Add(2 * time.Second), ToolCallID: "call-1", ToolName: "Bash", ToolResultStatus: "success"},
				},
				fixtureStart: createdAt, fixtureEnd: updatedAt,
			},
			{
				name: "message before conversation",
				messages: []legacyMessage{
					{Role: RoleUser, Content: "early", CreatedAt: createdAt.Add(-time.Nanosecond)},
				},
				fixtureStart: createdAt, fixtureEnd: updatedAt,
			},
			{
				name: "message after conversation",
				messages: []legacyMessage{
					{Role: RoleAssistant, Content: "late", CreatedAt: updatedAt.Add(time.Nanosecond)},
				},
				fixtureStart: createdAt, fixtureEnd: updatedAt,
			},
		}
		for _, fixture := range fixtures {
			t.Run(fixture.name, func(t *testing.T) {
				candidate := legacyConversation{
					ID: sessionID, Title: "integrity", Messages: fixture.messages,
					CreatedAt: fixture.fixtureStart, UpdatedAt: fixture.fixtureEnd,
				}
				payload, marshalErr := json.Marshal(candidate)
				if marshalErr != nil {
					t.Fatalf("marshal integrity fixture: %v", marshalErr)
				}
				loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(payload), legacyLoadOptions{
					Format: legacyFormatJSON, ExpectedSessionID: sessionID,
				})
				if loaded != nil || !errors.Is(loadErr, ErrLegacyMigrationFormat) {
					t.Fatalf("integrity load = (%#v, %v), want fixed format failure", loaded, loadErr)
				}
			})
		}
	})

	t.Run("legacy tool lifecycle keeps pre-start and post-start semantics", func(t *testing.T) {
		cases := []struct {
			name            string
			status          string
			errorCode       string
			data            json.RawMessage
			errorPayload    json.RawMessage
			wantState       tool.ExecutionState
			wantStatus      tool.ResultStatus
			wantFormatError bool
		}{
			{name: "invalid arguments rejected before start", status: "error", errorCode: tool.ErrInvalidArguments, wantState: tool.Rejected, wantStatus: tool.StatusError},
			{name: "tool not found from narrow error payload", status: "failed", errorPayload: json.RawMessage(`{"code":"tool_not_found","message":"missing","recoverable":false}`), wantState: tool.Rejected, wantStatus: tool.StatusError},
			{name: "pre-start timeout result is representable as rejected", status: "timeout", errorCode: tool.ErrTimeout, data: json.RawMessage(`{"started":false,"timed_out":true}`), wantState: tool.Rejected, wantStatus: tool.StatusTimeout},
			{name: "post-start timeout", status: "timeout", errorCode: tool.ErrTimeout, data: json.RawMessage(`{"started":true,"timed_out":true}`), wantState: tool.CancelledAfterStart, wantStatus: tool.StatusTimeout},
			{name: "executed command failure", status: "error", errorCode: tool.ErrCommandFailed, data: json.RawMessage(`{"started":true,"exit_code":1}`), wantState: tool.Completed, wantStatus: tool.StatusError},
			{name: "started conflicts with pre-start error code", status: "error", errorCode: tool.ErrInvalidArguments, data: json.RawMessage(`{"started":true}`), wantFormatError: true},
			{name: "success cannot claim not started", status: "success", data: json.RawMessage(`{"started":false}`), wantFormatError: true},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				candidate := legacyConversation{
					ID: sessionID, Title: "tool lifecycle", CreatedAt: createdAt, UpdatedAt: updatedAt,
					Messages: []legacyMessage{
						{Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(time.Second), ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`},
						{
							Role: RoleToolResult, Content: "result", ToolResultContent: "result", CreatedAt: createdAt.Add(2 * time.Second),
							ToolCallID: "call-1", ToolName: "Read", ToolResultStatus: testCase.status,
							ToolResultSummary: "summary", ToolErrorCode: testCase.errorCode,
							ToolResultData: testCase.data, ToolResultError: testCase.errorPayload,
						},
					},
				}
				payload, marshalErr := json.Marshal(candidate)
				if marshalErr != nil {
					t.Fatalf("marshal lifecycle fixture: %v", marshalErr)
				}
				loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, bytes.NewReader(payload), legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID})
				if testCase.wantFormatError {
					if loaded != nil || !errors.Is(loadErr, ErrLegacyMigrationFormat) {
						t.Fatalf("load = (%#v, %v), want format error", loaded, loadErr)
					}
					return
				}
				if loadErr != nil || loaded == nil || len(loaded.Messages) != 2 || loaded.Messages[1].Tool == nil {
					t.Fatalf("load = (%#v, %v), want tool result", loaded, loadErr)
				}
				state := loaded.Messages[1].Tool
				if state.State != testCase.wantState || state.Status != testCase.wantStatus {
					t.Fatalf("lifecycle = (%q, %q), want (%q, %q)", state.State, state.Status, testCase.wantState, testCase.wantStatus)
				}
			})
		}
	})

	t.Run("record and session budgets fail without publishing state", func(t *testing.T) {
		limited, codecErr := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 64, MaxSessionBytes: 128, Redactor: redactor})
		if codecErr != nil {
			t.Fatalf("new limited codec: %v", codecErr)
		}
		cases := []struct {
			name    string
			format  legacyConversationFormat
			payload []byte
			scope   budget.Scope
			limit   int64
		}{
			{name: "JSON session", format: legacyFormatJSON, payload: []byte(strings.Repeat(canary, 16)), scope: budget.SessionMaxSessionBytes, limit: 128},
			{name: "v1 record", format: legacyFormatJSONLV1, payload: append([]byte(strings.Repeat(canary, 4)), '\n'), scope: budget.SessionMaxRecordBytes, limit: 64},
			{name: "v1 session", format: legacyFormatJSONLV1, payload: []byte(strings.Repeat(" \n", 65)), scope: budget.SessionMaxSessionBytes, limit: 128},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				loaded, loadErr := loadLegacyConversationV2(context.Background(), limited, bytes.NewReader(testCase.payload), legacyLoadOptions{
					Format: testCase.format, ExpectedSessionID: sessionID,
				})
				var budgetErr *RecordBudgetError
				if loaded != nil || !errors.As(loadErr, &budgetErr) {
					t.Fatalf("load = (%#v, %v), want budget error", loaded, loadErr)
				}
				if budgetErr.SessionID != sessionID || budgetErr.Scope != testCase.scope || budgetErr.Limit != testCase.limit {
					t.Fatalf("budget error = %#v, want session=%q scope=%q limit=%d", budgetErr, sessionID, testCase.scope, testCase.limit)
				}
				if strings.Contains(loadErr.Error(), canary) {
					t.Fatalf("budget error retained payload: %q", loadErr)
				}
			})
		}
	})

	t.Run("pre and mid read cancellation perform no writes", func(t *testing.T) {
		data, marshalErr := json.Marshal(legacy)
		if marshalErr != nil {
			t.Fatalf("marshal cancellation fixture: %v", marshalErr)
		}
		path := writeT315Source(t, sessionID+".json", data)
		before := captureT315Source(t, path)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		preFile, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open pre-cancel source: %v", openErr)
		}
		preReader := &t315CountingReader{reader: preFile}
		loaded, loadErr := loadLegacyConversationV2(ctx, codec, preReader, legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID})
		if loaded != nil || !errors.Is(loadErr, context.Canceled) || preReader.reads != 0 {
			t.Fatalf("pre-cancel = (%#v, %v, reads=%d)", loaded, loadErr, preReader.reads)
		}
		if err := preFile.Close(); err != nil {
			t.Fatalf("close pre-cancel source: %v", err)
		}

		ctx, cancel = context.WithCancel(context.Background())
		midFile, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open mid-cancel source: %v", openErr)
		}
		midReader := &t315CancelingReader{reader: midFile, cancel: cancel}
		loaded, loadErr = loadLegacyConversationV2(ctx, codec, midReader, legacyLoadOptions{Format: legacyFormatJSON, ExpectedSessionID: sessionID})
		if loaded != nil || !errors.Is(loadErr, context.Canceled) || midReader.reads != 1 {
			t.Fatalf("mid-cancel = (%#v, %v, reads=%d)", loaded, loadErr, midReader.reads)
		}
		if err := midFile.Close(); err != nil {
			t.Fatalf("close mid-cancel source: %v", err)
		}

		v1 := encodeT315V1(t, JSONLRecord{
			Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID, MessageIndex: 0,
			CreatedAt: updatedAt, Snapshot: &legacy,
		})
		v1Path := writeT315Source(t, sessionID+".jsonl", v1)
		v1Before := captureT315Source(t, v1Path)
		v1File, openErr := os.Open(v1Path)
		if openErr != nil {
			t.Fatalf("open mid-JSONL source: %v", openErr)
		}
		ctx, cancel = context.WithCancel(context.Background())
		midJSONLReader := &t315CancelingReader{reader: v1File, cancel: cancel}
		loaded, loadErr = loadLegacyConversationV2(ctx, codec, midJSONLReader, legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID})
		if loaded != nil || !errors.Is(loadErr, context.Canceled) || midJSONLReader.reads != 1 {
			t.Fatalf("mid-JSONL-cancel = (%#v, %v, reads=%d)", loaded, loadErr, midJSONLReader.reads)
		}
		if err := v1File.Close(); err != nil {
			t.Fatalf("close mid-JSONL source: %v", err)
		}
		assertT315SourceUnchanged(t, path, before)
		assertT315SourceUnchanged(t, v1Path, v1Before)
	})

	t.Run("invalid configuration reads nothing", func(t *testing.T) {
		cases := []legacyLoadOptions{
			{Format: 0, ExpectedSessionID: sessionID},
			{Format: legacyFormatJSON, ExpectedSessionID: " bad"},
			{Format: legacyFormatJSON, ExpectedSessionID: "."},
			{Format: legacyFormatJSON, ExpectedSessionID: ".."},
			{Format: legacyFormatJSON, ExpectedSessionID: "parent/child"},
			{Format: legacyFormatJSON, ExpectedSessionID: `parent\child`},
		}
		for _, options := range cases {
			reader := &t315CountingReader{reader: strings.NewReader("{}")}
			loaded, loadErr := loadLegacyConversationV2(context.Background(), codec, reader, options)
			if loaded != nil || !errors.Is(loadErr, ErrLegacyMigrationConfig) || reader.reads != 0 {
				t.Fatalf("invalid config = (%#v, %v, reads=%d)", loaded, loadErr, reader.reads)
			}
		}
	})
}

func assertT315SafeConversion(t *testing.T, loaded *Conversation, redactor *redact.RuntimeRedactor, sessionID string, canary string, createdAt time.Time, updatedAt time.Time) {
	t.Helper()
	if loaded == nil || loaded.ID != sessionID || len(loaded.Messages) != 4 {
		t.Fatalf("loaded conversation mismatch: %#v", loaded)
	}
	if !loaded.CreatedAt.Equal(createdAt) || !loaded.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("times = (%s, %s), want (%s, %s)", loaded.CreatedAt, loaded.UpdatedAt, createdAt, updatedAt)
	}
	texts := []string{loaded.Title.Text(), loaded.Context.Summary.Text(), loaded.Context.LastBoundary.Text()}
	for index := range loaded.Messages {
		texts = append(texts, loaded.Messages[index].Content.Text())
		if loaded.Messages[index].Tool != nil {
			texts = append(texts,
				loaded.Messages[index].Tool.ArgumentsJSON.Text(),
				loaded.Messages[index].Tool.Summary.Text(),
				loaded.Messages[index].Tool.Result.Text(),
				loaded.Messages[index].Tool.TruncationReason.Text(),
			)
			if loaded.Messages[index].Tool.Error != nil {
				texts = append(texts, loaded.Messages[index].Tool.Error.Message.Text())
			}
		}
	}
	for _, value := range texts {
		if strings.Contains(value, canary) {
			t.Fatalf("unsafe text retained canary: %q", value)
		}
	}
	wantText := func(raw string) string { return redactor.Redact(raw).Text() }
	if loaded.Title.Text() != wantText("title "+canary) ||
		loaded.Messages[0].Role != RoleUser || loaded.Messages[0].Content.Text() != wantText("question "+canary) ||
		loaded.Messages[1].Role != RoleToolCall || loaded.Messages[1].Content.Text() != wantText("Read(...) "+canary) ||
		loaded.Messages[2].Role != RoleToolResult || loaded.Messages[2].Content.Text() != wantText("result "+canary) ||
		loaded.Messages[3].Role != RoleContextSummary || loaded.Messages[3].Content.Text() != wantText("context "+canary) ||
		loaded.Context.Summary.Text() != wantText("summary "+canary) || loaded.Context.LastBoundary.Text() != wantText("boundary "+canary) {
		t.Fatalf("safe text mapping lost legacy semantics: %#v", loaded)
	}
	if loaded.Context == nil || loaded.Context.SummaryFailureCount != 1 || loaded.Context.LastInputTokens != 10 || loaded.Context.LastOutputTokens != 20 || loaded.Context.LastEstimatedTokens != 30 || loaded.Context.LastEstimatedCharacters != 40 {
		t.Fatalf("context conversion mismatch: %#v", loaded.Context)
	}
	call := loaded.Messages[1].Tool
	if call == nil || call.CallID != "call-1" || call.Name != "Read" || call.ArgumentsJSON.Text() != wantText(`{"path":"`+canary+`"}`) ||
		call.State != tool.Prepared || call.Status != "" || call.Artifact != nil || call.Error != nil {
		t.Fatalf("tool call conversion mismatch: %#v", call)
	}
	result := loaded.Messages[2].Tool
	if result == nil || result.CallID != "call-1" || result.Name != "Read" || result.Summary.Text() != wantText("summary "+canary) || result.Result.Text() != wantText("result "+canary) ||
		result.State != tool.Completed || result.Status != tool.StatusError || !result.Truncated || result.Artifact != nil || result.Error == nil ||
		result.Error.Code != "read_failed" || result.Error.Message.Text() != wantText("failure "+canary) || !result.Error.Recoverable {
		t.Fatalf("tool result conversion mismatch: %#v", result)
	}
	if loaded.Context.LastCompressionAt == nil || !loaded.Context.LastCompressionAt.Equal(createdAt.Add(4*time.Minute)) {
		t.Fatalf("last compression time mismatch: %#v", loaded.Context.LastCompressionAt)
	}
	for index, want := range []time.Time{createdAt.Add(time.Second), createdAt.Add(2 * time.Second), createdAt.Add(3 * time.Second), createdAt.Add(4 * time.Second)} {
		if !loaded.Messages[index].CreatedAt.Equal(want) {
			t.Fatalf("message %d time = %s, want %s", index, loaded.Messages[index].CreatedAt, want)
		}
	}
	if result.TruncationReason.Text() != wantText("legacy_output_truncated") {
		t.Fatalf("truncation reason = %q", result.TruncationReason.Text())
	}
	if validateV2Snapshot(loaded, sessionID) != nil {
		t.Fatal("converted legacy state is not a valid v2 snapshot")
	}
}

type t315SourceState struct {
	data    []byte
	digest  [sha256.Size]byte
	mode    os.FileMode
	modTime time.Time
	info    os.FileInfo
	entries []string
}

func writeT315Source(t *testing.T, name string, data []byte) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod source: %v", err)
	}
	stamp := time.Date(2026, time.July, 2, 3, 4, 5, 123456789, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes source: %v", err)
	}
	return path
}

func captureT315Source(t *testing.T, path string) t315SourceState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read legacy source")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("stat legacy source")
	}
	return t315SourceState{
		data: data, digest: sha256.Sum256(data), mode: info.Mode(), modTime: info.ModTime(), info: info,
		entries: t315DirectoryEntries(t, filepath.Dir(path)),
	}
}

func assertT315SourceUnchanged(t *testing.T, path string, before t315SourceState) {
	t.Helper()
	after := captureT315Source(t, path)
	if !bytes.Equal(after.data, before.data) || after.digest != before.digest {
		t.Fatal("legacy source bytes changed")
	}
	if after.mode != before.mode || !after.modTime.Equal(before.modTime) {
		t.Fatal("legacy source metadata changed")
	}
	if !os.SameFile(before.info, after.info) {
		t.Fatal("legacy source was moved or replaced")
	}
	if !reflect.DeepEqual(after.entries, before.entries) {
		t.Fatal("legacy source directory changed")
	}
}

func assertT315ReaderBorrowed(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); err != nil {
		t.Fatal("migration closed caller-owned reader")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal("caller-owned reader is unusable")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close caller-owned reader")
	}
}

func t315DirectoryEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal("read legacy source directory")
	}
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].Name()
	}
	sort.Strings(names)
	return names
}

func encodeT315V1(t *testing.T, records ...JSONLRecord) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("encode v1 record: %v", err)
		}
	}
	return output.Bytes()
}

func t317LegacyConversation(sessionID string, sourcePath string, sourceBytes int64, createdAt time.Time) legacyConversation {
	return legacyConversation{
		ID:    sessionID,
		Title: "legacy artifact",
		Messages: []legacyMessage{
			{
				Role: RoleToolCall, Content: "Read({})", CreatedAt: createdAt.Add(time.Second),
				ToolCallID: "call-1", ToolName: "Read", RawToolArguments: `{}`,
			},
			{
				Role: RoleToolResult, Content: "safe external preview", CreatedAt: createdAt.Add(2 * time.Second),
				ToolCallID: "call-1", ToolName: "Read", ToolResultContent: "safe external preview",
				ToolResultStatus: "success", Externalized: true, ExternalPath: sourcePath,
				ExternalBytes: sourceBytes, ExternalPreview: "safe external preview",
			},
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt.Add(3 * time.Second),
	}
}

func cloneLegacyMessages(source []legacyMessage) []legacyMessage {
	result := append([]legacyMessage(nil), source...)
	for index := range result {
		result[index].ToolResultData = append(json.RawMessage(nil), source[index].ToolResultData...)
		result[index].ToolResultError = append(json.RawMessage(nil), source[index].ToolResultError...)
	}
	return result
}

func mustT317Codec(t *testing.T) *V2RecordCodec {
	t.Helper()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{
		MaxRecordBytes:  256 * 1024,
		MaxSessionBytes: 1024 * 1024,
		Redactor:        redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatal("create T3.17 codec")
	}
	return codec
}

func writeT317File(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal("create T3.17 fixture parent")
	}
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		t.Fatal("write T3.17 fixture")
	}
	stamp := time.Date(2026, time.August, 3, 12, 34, 56, 789, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal("set T3.17 fixture time")
	}
}

func t317ValidRef(bytes int64, createdAt time.Time) artifact.Ref {
	return artifact.Ref{
		ID:        strings.Repeat("a", legacyArtifactIDBytes*2),
		Bytes:     bytes,
		CreatedAt: createdAt,
		Available: true,
		Complete:  true,
	}
}

type t317ImporterFunc func(context.Context, string, artifact.Metadata) (artifact.Ref, error)

func (f t317ImporterFunc) Import(ctx context.Context, legacyPath string, metadata artifact.Metadata) (artifact.Ref, error) {
	return f(ctx, legacyPath, metadata)
}

func assertT317PlaceholderNoLeak(t *testing.T, result LoadResult, forbidden ...string) {
	t.Helper()
	if result.Available || result.Conversation != nil || result.Persisted != (PersistedState{}) ||
		result.Recovery.Status != RecoveryPlaceholder || result.Recovery.LastValidRevision != 0 ||
		result.Recovery.SkippedRecords != 1 || len(result.Recovery.Diagnostics) != 1 {
		t.Fatal("legacy artifact failure did not return the closed placeholder shape")
	}
	diagnostic := result.Recovery.Diagnostics[0]
	if diagnostic.Code != legacyArtifactImportDiagnosticCode || diagnostic.Message != legacyArtifactImportMessage ||
		diagnostic.Path != "" || len(diagnostic.Attributes) != 0 {
		t.Fatal("legacy artifact placeholder diagnostic is not fixed and pathless")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal("encode legacy artifact placeholder")
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(encoded, []byte(value)) {
			t.Fatal("legacy path or payload crossed the placeholder boundary")
		}
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

type t315CountingReader struct {
	reader io.Reader
	reads  int
}

func (r *t315CountingReader) Read(payload []byte) (int, error) {
	r.reads++
	return r.reader.Read(payload)
}

type t315CancelingReader struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *t315CancelingReader) Read(payload []byte) (int, error) {
	r.reads++
	count, err := r.reader.Read(payload)
	if r.reads == 1 {
		r.cancel()
	}
	return count, err
}
