package conversation

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestV2TypesContainNoRawPathOrUnsafeText(t *testing.T) {
	t.Run("exact result DTO shapes", func(t *testing.T) {
		assertStructFields(t, reflect.TypeOf(RecoveryReport{}), []fieldShape{
			{"Status", reflect.TypeOf(RecoveryStatus(""))},
			{"LastValidRevision", reflect.TypeOf(uint64(0))},
			{"SkippedRecords", reflect.TypeOf(int(0))},
			{"Diagnostics", reflect.TypeOf([]diagnostics.Diagnostic{})},
		})
		assertStructFields(t, reflect.TypeOf(ConversationSummary{}), []fieldShape{
			{"ID", reflect.TypeOf("")},
			{"Title", reflect.TypeOf(redact.SafeText{})},
			{"UpdatedAt", reflect.TypeOf(time.Time{})},
			{"MessageCount", reflect.TypeOf(int(0))},
		})
		assertStructFields(t, reflect.TypeOf(ListEntry{}), []fieldShape{
			{"Summary", reflect.TypeOf(ConversationSummary{})},
			{"Available", reflect.TypeOf(false)},
			{"Recovery", reflect.TypeOf(RecoveryReport{})},
		})
		assertStructFields(t, reflect.TypeOf(PersistedState{}), []fieldShape{
			{"Revision", reflect.TypeOf(uint64(0))},
			{"MessageCount", reflect.TypeOf(int(0))},
			{"Digest", reflect.TypeOf(StateDigest(""))},
			{"MessagesDigest", reflect.TypeOf(StateDigest(""))},
		})
		assertStructFields(t, reflect.TypeOf(ListResult{}), []fieldShape{
			{"Entries", reflect.TypeOf([]ListEntry{})},
			{"Truncated", reflect.TypeOf(false)},
			{"ScannedFiles", reflect.TypeOf(int(0))},
			{"ScannedBytes", reflect.TypeOf(int64(0))},
			{"Diagnostics", reflect.TypeOf([]diagnostics.Diagnostic{})},
		})
		assertStructFields(t, reflect.TypeOf(LoadResult{}), []fieldShape{
			{"Conversation", reflect.TypeOf((*Conversation)(nil))},
			{"Available", reflect.TypeOf(false)},
			{"Persisted", reflect.TypeOf(PersistedState{})},
			{"Recovery", reflect.TypeOf(RecoveryReport{})},
		})
		assertStructFields(t, reflect.TypeOf(SaveResult{}), []fieldShape{
			{"Kind", reflect.TypeOf(SaveKind(""))},
			{"Persisted", reflect.TypeOf(PersistedState{})},
		})
		assertStructFields(t, reflect.TypeOf(MaintenanceResult{}), []fieldShape{
			{"ScannedFiles", reflect.TypeOf(int(0))},
			{"ScannedBytes", reflect.TypeOf(int64(0))},
			{"Deleted", reflect.TypeOf(int(0))},
			{"Skipped", reflect.TypeOf(int(0))},
			{"Truncated", reflect.TypeOf(false)},
			{"Diagnostics", reflect.TypeOf([]diagnostics.Diagnostic{})},
		})
	})

	t.Run("safe persisted graph", func(t *testing.T) {
		assertStructFields(t, reflect.TypeOf(Conversation{}), []fieldShape{
			{"ID", reflect.TypeOf("")},
			{"Title", reflect.TypeOf(redact.SafeText{})},
			{"Messages", reflect.TypeOf([]Message{})},
			{"Context", reflect.TypeOf((*ContextMetadata)(nil))},
			{"CreatedAt", reflect.TypeOf(time.Time{})},
			{"UpdatedAt", reflect.TypeOf(time.Time{})},
		})
		assertStructFields(t, reflect.TypeOf(Message{}), []fieldShape{
			{"Role", reflect.TypeOf(MessageRole(""))},
			{"Content", reflect.TypeOf(redact.SafeText{})},
			{"CreatedAt", reflect.TypeOf(time.Time{})},
			{"Tool", reflect.TypeOf((*ToolState)(nil))},
			{"Subagent", reflect.TypeOf((*SubagentNotificationMessage)(nil))},
		})
		assertStructFields(t, reflect.TypeOf(SubagentNotificationMessage{}), []fieldShape{
			{"NotificationID", reflect.TypeOf("")},
			{"TaskID", reflect.TypeOf("")},
			{"Status", reflect.TypeOf("")},
			{"Summary", reflect.TypeOf(redact.SafeText{})},
			{"SummaryTruncated", reflect.TypeOf(false)},
			{"TruncationReason", reflect.TypeOf(redact.SafeText{})},
			{"StopReason", reflect.TypeOf("")},
			{"CreatedAt", reflect.TypeOf(time.Time{})},
		})
		assertStructFields(t, reflect.TypeOf(ToolState{}), []fieldShape{
			{"CallID", reflect.TypeOf("")},
			{"Name", reflect.TypeOf("")},
			{"ArgumentsJSON", reflect.TypeOf(redact.SafeText{})},
			{"State", reflect.TypeOf(tool.ExecutionState(""))},
			{"Status", reflect.TypeOf(tool.ResultStatus(""))},
			{"Summary", reflect.TypeOf(redact.SafeText{})},
			{"Result", reflect.TypeOf(redact.SafeText{})},
			{"Truncated", reflect.TypeOf(false)},
			{"TruncationReason", reflect.TypeOf(redact.SafeText{})},
			{"Artifact", reflect.TypeOf((*artifact.Ref)(nil))},
			{"Error", reflect.TypeOf((*tool.SafeError)(nil))},
		})
		assertStructFields(t, reflect.TypeOf(ContextMetadata{}), []fieldShape{
			{"Summary", reflect.TypeOf(redact.SafeText{})},
			{"LastBoundary", reflect.TypeOf(redact.SafeText{})},
			{"LastCompressionAt", reflect.TypeOf((*time.Time)(nil))},
			{"SummaryFailureCount", reflect.TypeOf(int(0))},
			{"LastInputTokens", reflect.TypeOf(int64(0))},
			{"LastOutputTokens", reflect.TypeOf(int64(0))},
			{"LastEstimatedTokens", reflect.TypeOf(int64(0))},
			{"LastEstimatedCharacters", reflect.TypeOf(int(0))},
		})
		assertStructFields(t, reflect.TypeOf(artifact.Ref{}), []fieldShape{
			{"ID", reflect.TypeOf("")},
			{"Bytes", reflect.TypeOf(int64(0))},
			{"CreatedAt", reflect.TypeOf(time.Time{})},
			{"Available", reflect.TypeOf(false)},
			{"Complete", reflect.TypeOf(false)},
		})
		assertStructFields(t, reflect.TypeOf(tool.SafeError{}), []fieldShape{
			{"Code", reflect.TypeOf("")},
			{"Message", reflect.TypeOf(redact.SafeText{})},
			{"Recoverable", reflect.TypeOf(false)},
		})

		walkV2Type(t, reflect.TypeOf(Conversation{}), "Conversation", map[reflect.Type]bool{})
	})

	t.Run("closed status values", func(t *testing.T) {
		if RecoveryClean != "clean" || RecoveryPartial != "partial" || RecoveryPlaceholder != "placeholder" {
			t.Fatalf("unexpected recovery status values: %q %q %q", RecoveryClean, RecoveryPartial, RecoveryPlaceholder)
		}
		if SaveNoop != "noop" || SaveBatch != "batch" || SaveSnapshot != "snapshot" {
			t.Fatalf("unexpected save kind values: %q %q %q", SaveNoop, SaveBatch, SaveSnapshot)
		}
	})

	t.Run("legacy path isolation", func(t *testing.T) {
		assertExternalPathIsLegacyOnly(t)
	})
}

type fieldShape struct {
	name   string
	typeOf reflect.Type
}

func assertStructFields(t *testing.T, got reflect.Type, want []fieldShape) {
	t.Helper()
	if got.NumField() != len(want) {
		t.Fatalf("%s has %d fields, want %d", got, got.NumField(), len(want))
	}
	for index, expected := range want {
		field := got.Field(index)
		if field.Name != expected.name || field.Type != expected.typeOf {
			t.Fatalf("%s field %d = %s %v, want %s %v", got, index, field.Name, field.Type, expected.name, expected.typeOf)
		}
	}
}

func walkV2Type(t *testing.T, current reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	for current.Kind() == reflect.Pointer || current.Kind() == reflect.Slice || current.Kind() == reflect.Array {
		if current.Kind() == reflect.Slice && current.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("%s retains raw bytes", path)
		}
		current = current.Elem()
	}

	if current == reflect.TypeOf(redact.SafeText{}) || current == reflect.TypeOf(time.Time{}) || current == reflect.TypeOf(artifact.Ref{}) {
		return
	}
	if current == reflect.TypeOf(json.RawMessage{}) {
		t.Fatalf("%s retains json.RawMessage", path)
	}
	switch current.Kind() {
	case reflect.Map, reflect.Interface:
		t.Fatalf("%s retains open-ended %v data", path, current.Kind())
	case reflect.String:
		if !allowedV2String(path, current) {
			t.Fatalf("%s retains plain string type %v", path, current)
		}
	case reflect.Struct:
		if seen[current] {
			return
		}
		seen[current] = true
		for index := 0; index < current.NumField(); index++ {
			field := current.Field(index)
			lowerName := strings.ToLower(field.Name)
			if strings.Contains(lowerName, "path") || strings.Contains(lowerName, "external") || strings.Contains(lowerName, "stdout") || strings.Contains(lowerName, "stderr") || strings.HasPrefix(lowerName, "raw") {
				t.Fatalf("%s.%s exposes a forbidden persisted field", path, field.Name)
			}
			walkV2Type(t, field.Type, path+"."+field.Name, seen)
		}
	}
}

func allowedV2String(path string, current reflect.Type) bool {
	if current == reflect.TypeOf(MessageRole("")) || current == reflect.TypeOf(tool.ExecutionState("")) || current == reflect.TypeOf(tool.ResultStatus("")) {
		return true
	}
	switch path {
	case "Conversation.ID", "Conversation.Messages.Tool.CallID", "Conversation.Messages.Tool.Name", "Conversation.Messages.Tool.Error.Code",
		"Conversation.Messages.Subagent.NotificationID", "Conversation.Messages.Subagent.TaskID",
		"Conversation.Messages.Subagent.Status", "Conversation.Messages.Subagent.StopReason":
		return current == reflect.TypeOf("")
	default:
		return false
	}
}

func assertExternalPathIsLegacyOnly(t *testing.T) {
	t.Helper()
	files, err := parser.ParseDir(token.NewFileSet(), ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg := files["conversation"]
	if pkg == nil {
		t.Fatal("conversation package AST not found")
	}

	found := 0
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						if name.Name != "ExternalPath" {
							continue
						}
						found++
						if typeSpec.Name.Name != "legacyMessage" {
							t.Fatalf("ExternalPath declared on %s, want private legacyMessage DTO", typeSpec.Name.Name)
						}
						comment := ""
						if general.Doc != nil {
							comment += general.Doc.Text()
						}
						if typeSpec.Doc != nil {
							comment += typeSpec.Doc.Text()
						}
						if !strings.Contains(strings.ToLower(comment), "legacy") {
							t.Fatal("Message.ExternalPath is not explicitly documented as legacy-only")
						}
					}
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("ExternalPath declarations = %d, want exactly one legacy declaration", found)
	}
}
