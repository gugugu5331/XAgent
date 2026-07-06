package conversation

import (
	"encoding/json"
	"fmt"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const JSONLVersion = 1

type RecordType string

const (
	RecordTypeMessage  RecordType = "message"
	RecordTypeSnapshot RecordType = "snapshot"
)

type JSONLDiagnostic = diagnostics.Diagnostic

var (
	JSONLSeverityInfo    = diagnostics.SeverityInfo
	JSONLSeverityWarning = diagnostics.SeverityWarning
	JSONLSeverityError   = diagnostics.SeverityError
)

type JSONLRecord struct {
	Version               int               `json:"version"`
	Type                  RecordType        `json:"type"`
	SessionID             string            `json:"session_id,omitempty"`
	MessageIndex          int               `json:"message_index"`
	CreatedAt             time.Time         `json:"created_at"`
	ConversationTitle     string            `json:"conversation_title,omitempty"`
	ConversationCreatedAt time.Time         `json:"conversation_created_at,omitempty"`
	ConversationUpdatedAt time.Time         `json:"conversation_updated_at,omitempty"`
	Message               *Message          `json:"message,omitempty"`
	Snapshot              *Conversation     `json:"snapshot,omitempty"`
	Diagnostics           []JSONLDiagnostic `json:"diagnostics,omitempty"`
	Error                 string            `json:"error,omitempty"`
}

func (r JSONLRecord) Validate() []JSONLDiagnostic {
	var items []JSONLDiagnostic
	if r.Version != JSONLVersion {
		items = append(items, newJSONLDiagnostic("jsonl_unknown_version", fmt.Sprintf("未知 JSONL 版本 %d", r.Version), JSONLSeverityError))
	}
	switch r.Type {
	case RecordTypeMessage:
		if r.Message == nil {
			items = append(items, newJSONLDiagnostic("jsonl_missing_message", "message 记录缺少 message 字段", JSONLSeverityError))
		}
	case RecordTypeSnapshot:
		if r.Snapshot == nil {
			items = append(items, newJSONLDiagnostic("jsonl_missing_snapshot", "snapshot 记录缺少 snapshot 字段", JSONLSeverityError))
		}
	default:
		items = append(items, newJSONLDiagnostic("jsonl_invalid_record_type", fmt.Sprintf("非法 JSONL 记录类型 %q", r.Type), JSONLSeverityError))
	}
	return items
}

func (r JSONLRecord) Sanitized() JSONLRecord {
	r.Error = redact.Text(r.Error)
	for i := range r.Diagnostics {
		r.Diagnostics[i] = r.Diagnostics[i].Safe(redact.Text)
	}
	return r
}

func (r JSONLRecord) MarshalJSON() ([]byte, error) {
	type alias JSONLRecord
	sanitized := r.Sanitized()
	return json.Marshal(alias(sanitized))
}

func newJSONLDiagnostic(code string, message string, severity diagnostics.Severity) JSONLDiagnostic {
	return diagnostics.New(code, severity, message).Safe(redact.Text)
}
