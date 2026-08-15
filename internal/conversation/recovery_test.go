package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
)

func TestLoadV2ValidatesRevisionAndDigestChain(t *testing.T) {
	const sessionID = "conversation-1"

	t.Run("valid snapshot batch snapshot chain", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, lines := encodeT39Records(t, codec, chain.anchor, chain.batch, chain.snapshot)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("load valid chain: %v", err)
		}
		if !reflect.DeepEqual(loaded.Conversation, chain.finalConversation) {
			t.Fatalf("loaded conversation mismatch\n got: %#v\nwant: %#v", loaded.Conversation, chain.finalConversation)
		}
		if loaded.Persisted != chain.finalPersisted {
			t.Fatalf("loaded persisted state = %#v, want %#v", loaded.Persisted, chain.finalPersisted)
		}
		if loaded.VerifiedBytes != int64(len(data)) || loaded.RecordCount != len(lines) {
			t.Fatalf("verified prefix bytes=%d records=%d, want bytes=%d records=%d", loaded.VerifiedBytes, loaded.RecordCount, len(data), len(lines))
		}
	})

	t.Run("higher revision checkpoint anchor", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		record := validV2SnapshotRecord(t, 27)
		data, _ := encodeT39Records(t, codec, record)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("load checkpoint anchor: %v", err)
		}
		want := mustComputePersistedState(t, record.Snapshot, 27)
		if loaded.Persisted != want || !reflect.DeepEqual(loaded.Conversation, record.Snapshot) {
			t.Fatalf("checkpoint load mismatch: state=%#v conversation=%#v", loaded.Persisted, loaded.Conversation)
		}
	})

	t.Run("batches apply in order and publish final updated time", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		anchor := validV2SnapshotRecord(t, 1)
		firstConversation, firstBatch, firstPersisted := appendT39Batch(t, anchor.Snapshot, mustComputePersistedState(t, anchor.Snapshot, 1), "first append", time.Second)
		finalConversation, secondBatch, finalPersisted := appendT39Batch(t, firstConversation, firstPersisted, "second append", 2*time.Second)
		data, _ := encodeT39Records(t, codec, anchor, firstBatch, secondBatch)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("load ordered batches: %v", err)
		}
		if !reflect.DeepEqual(loaded.Conversation, finalConversation) || loaded.Persisted != finalPersisted {
			t.Fatalf("ordered batch result mismatch: state=%#v conversation=%#v", loaded.Persisted, loaded.Conversation)
		}
		if got := loaded.Conversation.Messages[len(loaded.Conversation.Messages)-2].Content.Text(); got != "first append" {
			t.Fatalf("first appended message = %q", got)
		}
		if got := loaded.Conversation.Messages[len(loaded.Conversation.Messages)-1].Content.Text(); got != "second append" {
			t.Fatalf("second appended message = %q", got)
		}
		if !loaded.Conversation.UpdatedAt.Equal(secondBatch.Batch.UpdatedAt) {
			t.Fatalf("final UpdatedAt = %s, want %s", loaded.Conversation.UpdatedAt, secondBatch.Batch.UpdatedAt)
		}
	})

	t.Run("snapshot fully replaces candidate", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		anchor := validV2SnapshotRecord(t, 1)
		previous := mustComputePersistedState(t, anchor.Snapshot, 1)
		replacement := cloneConversationV2(anchor.Snapshot)
		replacement.Title = digestSafeText("replacement snapshot")
		replacement.Messages = []Message{{
			Role:      RoleAssistant,
			Content:   digestSafeText("only replacement message"),
			CreatedAt: replacement.CreatedAt.Add(20 * time.Minute),
		}}
		replacement.Context = nil
		replacement.UpdatedAt = replacement.UpdatedAt.Add(30 * time.Minute)
		replacementState := mustComputePersistedState(t, replacement, 2)
		record := JSONLRecordV2{
			Version:        JSONLVersionV2,
			Kind:           RecordSnapshot,
			SessionID:      sessionID,
			Revision:       2,
			PreviousDigest: previous.Digest,
			Digest:         replacementState.Digest,
			Snapshot:       replacement,
		}
		data, _ := encodeT39Records(t, codec, anchor, record)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("load replacement snapshot: %v", err)
		}
		if !reflect.DeepEqual(loaded.Conversation, replacement) || loaded.Persisted != replacementState {
			t.Fatalf("snapshot replacement mismatch: state=%#v conversation=%#v", loaded.Persisted, loaded.Conversation)
		}
		if len(loaded.Conversation.Messages) != 1 || loaded.Conversation.Context != nil {
			t.Fatalf("snapshot retained prior state: %#v", loaded.Conversation)
		}
	})

	t.Run("invalid successor retains last verified prefix", func(t *testing.T) {
		cases := []struct {
			name    string
			mutate  func(*JSONLRecordV2)
			wantErr error
		}{
			{name: "duplicate revision", mutate: func(r *JSONLRecordV2) { r.Revision = 1 }, wantErr: ErrV2LoadValidation},
			{name: "backward revision", mutate: func(r *JSONLRecordV2) { r.Revision = 0 }, wantErr: ErrV2LoadValidation},
			{name: "skipped revision", mutate: func(r *JSONLRecordV2) { r.Revision = 3 }, wantErr: ErrV2LoadValidation},
			{name: "missing previous digest", mutate: func(r *JSONLRecordV2) { r.PreviousDigest = "" }, wantErr: ErrV2LoadValidation},
			{name: "wrong previous digest", mutate: func(r *JSONLRecordV2) { r.PreviousDigest = t39Digest("a") }, wantErr: ErrV2LoadValidation},
			{name: "snapshot digest mismatch", mutate: func(r *JSONLRecordV2) {
				r.Kind, r.Batch, r.Snapshot = RecordSnapshot, nil, cloneConversationV2(stateDigestFixture())
				r.Snapshot.UpdatedAt = r.Snapshot.UpdatedAt.Add(time.Hour)
				r.Digest = t39Digest("b")
			}, wantErr: ErrV2LoadDigest},
			{name: "batch digest mismatch", mutate: func(r *JSONLRecordV2) { r.Digest = t39Digest("c") }, wantErr: ErrV2LoadDigest},
			{name: "batch base count mismatch", mutate: func(r *JSONLRecordV2) { r.Batch.BaseMessageCount++ }, wantErr: ErrV2LoadValidation},
			{name: "session mismatch", mutate: func(r *JSONLRecordV2) { r.SessionID = "different-session" }, wantErr: ErrV2LoadValidation},
			{name: "unknown version", mutate: func(r *JSONLRecordV2) { r.Version = 99 }, wantErr: ErrV2LoadValidation},
			{name: "unknown kind", mutate: func(r *JSONLRecordV2) { r.Kind = "delta" }, wantErr: ErrV2LoadValidation},
			{name: "missing payload", mutate: func(r *JSONLRecordV2) { r.Batch = nil }, wantErr: ErrV2LoadValidation},
			{name: "multiple payloads", mutate: func(r *JSONLRecordV2) { r.Snapshot = cloneConversationV2(stateDigestFixture()) }, wantErr: ErrV2LoadValidation},
		}

		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				codec := mustV2TestCodec(t)
				chain := newT39Chain(t)
				invalid := cloneJSONLRecordV2(chain.batch)
				testCase.mutate(&invalid)
				data, lines := encodeT39Records(t, codec, chain.anchor, invalid)

				loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("load error = %v, want %v", err, testCase.wantErr)
				}
				assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
			})
		}
	})

	t.Run("unknown wire payload retains prefix", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, codec, chain.anchor, chain.batch)
		unknown := bytes.Replace(lines[1], []byte("{"), []byte(`{"unknown_payload":"raw-record-secret-canary",`), 1)
		data := append(append([]byte(nil), lines[0]...), unknown...)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if !errors.Is(err, ErrV2RecordMalformed) {
			t.Fatalf("unknown wire payload error = %v", err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
		if strings.Contains(err.Error(), "raw-record-secret-canary") {
			t.Fatalf("unknown payload error leaked raw record: %q", err)
		}
	})

	t.Run("first invalid record returns zero state", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		record := validV2SnapshotRecord(t, 1)
		record.Digest = t39Digest("d")
		data, _ := encodeT39Records(t, codec, record)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if !errors.Is(err, ErrV2LoadDigest) {
			t.Fatalf("first invalid error = %v", err)
		}
		if !reflect.DeepEqual(loaded, v2LoadState{}) {
			t.Fatalf("first invalid record published state: %#v", loaded)
		}
	})

	t.Run("session byte budget is cumulative", func(t *testing.T) {
		permissive := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, permissive, chain.anchor, chain.batch)
		maxRecord := int64(len(lines[0]) - 1)
		if candidate := int64(len(lines[1]) - 1); candidate > maxRecord {
			maxRecord = candidate
		}
		limit := int64(len(lines[0]) + len(lines[1]) - 1)
		codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: maxRecord, MaxSessionBytes: limit})
		if err != nil {
			t.Fatalf("create cumulative-budget codec: %v", err)
		}
		data := append(append([]byte(nil), lines[0]...), lines[1]...)

		loaded, err := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		var budgetError *RecordBudgetError
		if !errors.As(err, &budgetError) || budgetError.Scope != budget.SessionMaxSessionBytes || budgetError.Limit != limit {
			t.Fatalf("cumulative budget error = %#v (%v)", budgetError, err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
	})

	t.Run("empty input", func(t *testing.T) {
		loaded, err := loadV2Records(context.Background(), mustV2TestCodec(t), sessionID, bytes.NewReader(nil))
		if !errors.Is(err, ErrV2LoadEmpty) || !reflect.DeepEqual(loaded, v2LoadState{}) {
			t.Fatalf("empty load state=%#v err=%v", loaded, err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, _ := encodeT39Records(t, codec, chain.anchor)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		loaded, err := loadV2Records(ctx, codec, sessionID, bytes.NewReader(data))
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, v2LoadState{}) {
			t.Fatalf("canceled load state=%#v err=%v", loaded, err)
		}
	})

	t.Run("cancellation during decode does not publish record", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, _ := encodeT39Records(t, codec, chain.anchor)
		ctx, cancel := context.WithCancel(context.Background())
		reader := &t39CancelingReader{reader: bytes.NewReader(data), cancel: cancel}

		loaded, err := loadV2Records(ctx, codec, sessionID, reader)
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, v2LoadState{}) {
			t.Fatalf("decode cancellation state=%#v err=%v", loaded, err)
		}
	})

	t.Run("loader does not close borrowed reader", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, _ := encodeT39Records(t, codec, chain.anchor)
		reader := &t39CloseSpy{Reader: bytes.NewReader(data)}

		if _, err := loadV2Records(context.Background(), codec, sessionID, reader); err != nil {
			t.Fatalf("load borrowed reader: %v", err)
		}
		if reader.closed {
			t.Fatal("loader closed borrowed reader")
		}
	})

	t.Run("errors never expose record content or digest", func(t *testing.T) {
		const bodyCanary = "message-body-secret-canary"
		const rawCanary = "raw-record-secret-canary"
		digestCanary := t39Digest("e")
		codec := mustV2TestCodec(t)
		record := validV2SnapshotRecord(t, 1)
		record.Snapshot.Messages[0].Content = digestSafeText(bodyCanary)
		record.Digest = digestCanary
		data, _ := encodeT39Records(t, codec, record)

		_, digestErr := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		malformed := []byte(`{"unexpected":"` + rawCanary + `"}` + "\n")
		_, malformedErr := loadV2Records(context.Background(), codec, sessionID, bytes.NewReader(malformed))
		for _, err := range []error{digestErr, malformedErr} {
			if err == nil {
				t.Fatal("canary load unexpectedly succeeded")
			}
			for _, canary := range []string{bodyCanary, rawCanary, string(digestCanary)} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("load error leaked canary %q: %q", canary, err)
				}
			}
		}
	})

	t.Run("application does not mutate inputs and returns independent state", func(t *testing.T) {
		chain := newT39Chain(t)
		anchorBefore := cloneJSONLRecordV2(chain.anchor)
		candidate, persisted, err := applyAndVerifyV2Record(v2LoadState{}, chain.anchor, sessionID)
		if err != nil {
			t.Fatalf("apply anchor: %v", err)
		}
		if !reflect.DeepEqual(chain.anchor, anchorBefore) {
			t.Fatal("anchor application mutated record input")
		}
		state := v2LoadState{Conversation: candidate, Persisted: persisted, RecordCount: 1}
		stateBefore := cloneConversationV2(state.Conversation)
		batchBefore := cloneJSONLRecordV2(chain.batch)
		next, _, err := applyAndVerifyV2Record(state, chain.batch, sessionID)
		if err != nil {
			t.Fatalf("apply batch: %v", err)
		}
		if !reflect.DeepEqual(state.Conversation, stateBefore) || !reflect.DeepEqual(chain.batch, batchBefore) {
			t.Fatal("batch application mutated record or existing candidate")
		}
		next.Messages[0].Content = digestSafeText("mutated returned candidate")
		if !reflect.DeepEqual(state.Conversation, stateBefore) {
			t.Fatal("returned candidate aliases existing verified state")
		}

		invalid := cloneJSONLRecordV2(chain.batch)
		invalid.Digest = t39Digest("f")
		stateBefore = cloneConversationV2(state.Conversation)
		if _, _, err := applyAndVerifyV2Record(state, invalid, sessionID); !errors.Is(err, ErrV2LoadDigest) {
			t.Fatalf("invalid application error = %v", err)
		}
		if !reflect.DeepEqual(state.Conversation, stateBefore) {
			t.Fatal("failed application mutated existing verified state")
		}
	})
}

func TestRecoveryHandlesTornTailAndBoundedCorruption(t *testing.T) {
	const sessionID = "conversation-1"

	t.Run("valid input reports a clean load", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, _ := encodeT39Records(t, codec, chain.anchor, chain.batch)
		wantConversation, _, wantPersisted := appendT39Batch(t, chain.anchor.Snapshot, mustComputePersistedState(t, chain.anchor.Snapshot, 1), "ordered batch append", time.Second)

		loaded, report, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("recover valid records: %v", err)
		}
		if !reflect.DeepEqual(loaded.Conversation, wantConversation) || loaded.Persisted != wantPersisted {
			t.Fatalf("clean recovery returned wrong state: %#v", loaded)
		}
		assertT310Report(t, report, RecoveryClean, 2, 0, "")
	})

	t.Run("unterminated tail preserves the verified prefix and original file", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, codec, chain.anchor, chain.batch)
		torn := bytes.TrimSuffix(lines[1], []byte{'\n'})
		data := append(append([]byte(nil), lines[0]...), torn...)
		original := bytes.Clone(data)
		path := filepath.Join(t.TempDir(), "t310-path-secret-canary.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write torn fixture: %v", err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open torn fixture: %v", err)
		}

		loaded, report, recoverErr := recoverV2Records(context.Background(), codec, sessionID, file)
		if recoverErr != nil {
			_ = file.Close()
			t.Fatalf("recover torn tail: %v", recoverErr)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("caller close torn fixture: %v", err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
		assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_torn_tail")
		assertT310FileUnchanged(t, path, original)
		assertT310NoLeaks(t, report, "t310-path-secret-canary", string(chain.batch.Digest), "ordered batch append")
	})

	t.Run("middle malformed line stops at the last verified state", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, codec, chain.anchor, chain.batch)
		badLine := []byte("{\"raw\":\"raw-record-secret-canary\"}\n")
		data := append(append(append([]byte(nil), lines[0]...), badLine...), lines[1]...)
		original := bytes.Clone(data)

		loaded, report, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("recover malformed middle line: %v", err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
		assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_corrupt_record")
		if !bytes.Equal(data, original) {
			t.Fatal("recovery modified caller-owned input bytes")
		}
		assertT310NoLeaks(t, report, "raw-record-secret-canary", string(chain.batch.Digest), "ordered batch append")
	})

	t.Run("digest chain corruption is reported without publishing the bad record", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		corrupt := cloneJSONLRecordV2(chain.batch)
		corrupt.Batch.Messages[0].Content = digestSafeText("message-body-secret-canary")
		corrupt.PreviousDigest = t39Digest("7")
		corrupt.Digest = t39Digest("8")
		data, lines := encodeT39Records(t, codec, chain.anchor, corrupt)

		loaded, report, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("recover broken digest chain: %v", err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
		assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_corrupt_record")
		assertT310NoLeaks(t, report, "message-body-secret-canary", string(corrupt.PreviousDigest), string(corrupt.Digest))
	})

	t.Run("reader error text is never retained in a recovery report", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, codec, chain.anchor)
		reader := &t310ErrorAfterReader{
			data: lines[0],
			err:  errors.New("reader-error-secret-canary"),
		}

		loaded, report, err := recoverV2Records(context.Background(), codec, sessionID, reader)
		if err != nil {
			t.Fatalf("recover reader failure: %v", err)
		}
		assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
		assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_read_failed")
		assertT310NoLeaks(t, report, "reader-error-secret-canary")
	})

	t.Run("record and cumulative session limits produce bounded reports", func(t *testing.T) {
		permissive := mustV2TestCodec(t)
		chain := newT39Chain(t)
		_, lines := encodeT39Records(t, permissive, chain.anchor, chain.batch)

		t.Run("record limit", func(t *testing.T) {
			maxRecord := int64(len(lines[0]) - 1)
			oversized := []byte(strings.Repeat("record-budget-secret-canary", int(maxRecord)/len("record-budget-secret-canary")+2) + "\n")
			codec, err := NewV2RecordCodec(V2RecordCodecOptions{
				MaxRecordBytes:  maxRecord,
				MaxSessionBytes: int64(len(lines[0]) + len(oversized) + 16),
			})
			if err != nil {
				t.Fatalf("create record-budget codec: %v", err)
			}
			data := append(append([]byte(nil), lines[0]...), oversized...)

			loaded, report, recoverErr := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
			if recoverErr != nil {
				t.Fatalf("recover record budget breach: %v", recoverErr)
			}
			assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
			assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_record_budget_exceeded")
			assertT310NoLeaks(t, report, "record-budget-secret-canary")
		})

		t.Run("session limit", func(t *testing.T) {
			maxRecord := int64(len(lines[0]) - 1)
			if second := int64(len(lines[1]) - 1); second > maxRecord {
				maxRecord = second
			}
			limit := int64(len(lines[0]) + len(lines[1]) - 1)
			codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: maxRecord, MaxSessionBytes: limit})
			if err != nil {
				t.Fatalf("create session-budget codec: %v", err)
			}
			data := append(append([]byte(nil), lines[0]...), lines[1]...)

			loaded, report, recoverErr := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
			if recoverErr != nil {
				t.Fatalf("recover session budget breach: %v", recoverErr)
			}
			assertT39AnchorPrefix(t, loaded, chain.anchor, len(lines[0]))
			assertT310Report(t, report, RecoveryPartial, 1, 1, "conversation_v2_record_budget_exceeded")
			assertT310NoLeaks(t, report, "ordered batch append", string(chain.batch.Digest))
		})
	})

	t.Run("zero verified records return a safe placeholder", func(t *testing.T) {
		cases := []struct {
			name      string
			data      []byte
			code      string
			skipped   int
			forbidden string
		}{
			{name: "empty", code: "conversation_v2_empty"},
			{name: "first malformed", data: []byte("{\"secret\":\"first-record-secret-canary\"}\n"), code: "conversation_v2_corrupt_record", skipped: 1, forbidden: "first-record-secret-canary"},
			{name: "first torn", data: []byte("{\"secret\":\"first-torn-secret-canary\"}"), code: "conversation_v2_torn_tail", skipped: 1, forbidden: "first-torn-secret-canary"},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				loaded, report, err := recoverV2Records(context.Background(), mustV2TestCodec(t), sessionID, bytes.NewReader(testCase.data))
				if err != nil {
					t.Fatalf("recover zero-prefix input: %v", err)
				}
				if !reflect.DeepEqual(loaded, v2LoadState{}) {
					t.Fatalf("placeholder published unverified state: %#v", loaded)
				}
				assertT310Report(t, report, RecoveryPlaceholder, 0, testCase.skipped, testCase.code)
				assertT310NoLeaks(t, report, testCase.forbidden)
			})
		}
	})

	t.Run("reader ownership and cancellation remain caller controlled", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		chain := newT39Chain(t)
		data, _ := encodeT39Records(t, codec, chain.anchor)

		reader := &t39CloseSpy{Reader: bytes.NewReader(data)}
		if _, _, err := recoverV2Records(context.Background(), codec, sessionID, reader); err != nil {
			t.Fatalf("recover borrowed reader: %v", err)
		}
		if reader.closed {
			t.Fatal("recovery closed its borrowed reader")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		loaded, report, err := recoverV2Records(ctx, codec, sessionID, bytes.NewReader(data))
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, v2LoadState{}) || !reflect.DeepEqual(report, RecoveryReport{}) {
			t.Fatalf("pre-canceled recovery state=%#v report=%#v err=%v", loaded, report, err)
		}

		ctx, cancel = context.WithCancel(context.Background())
		cancelingReader := &t39CancelingReader{reader: bytes.NewReader(data), cancel: cancel}
		loaded, report, err = recoverV2Records(ctx, codec, sessionID, cancelingReader)
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, v2LoadState{}) || !reflect.DeepEqual(report, RecoveryReport{}) {
			t.Fatalf("mid-read canceled recovery state=%#v report=%#v err=%v", loaded, report, err)
		}
	})

	t.Run("report shape is closed", func(t *testing.T) {
		typeOfReport := reflect.TypeOf(RecoveryReport{})
		want := []struct {
			name   string
			typeOf reflect.Type
		}{
			{name: "Status", typeOf: reflect.TypeOf(RecoveryStatus(""))},
			{name: "LastValidRevision", typeOf: reflect.TypeOf(uint64(0))},
			{name: "SkippedRecords", typeOf: reflect.TypeOf(int(0))},
			{name: "Diagnostics", typeOf: reflect.TypeOf([]diagnostics.Diagnostic(nil))},
		}
		if typeOfReport.NumField() != len(want) {
			t.Fatalf("RecoveryReport has %d fields, want closed set of %d", typeOfReport.NumField(), len(want))
		}
		for index, expected := range want {
			field := typeOfReport.Field(index)
			if field.Name != expected.name || field.Type != expected.typeOf {
				t.Fatalf("RecoveryReport field %d = %s %v, want %s %v", index, field.Name, field.Type, expected.name, expected.typeOf)
			}
		}
	})
}

func TestGapReminderUsesConfiguredDays(t *testing.T) {
	const sessionID = "conversation-1"
	codec := mustV2TestCodec(t)
	chain := newT39Chain(t)
	data, lines := encodeT39Records(t, codec, chain.anchor, chain.batch)
	original := bytes.Clone(data)
	baseline, baselineReport, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("recover reminder baseline: %v", err)
	}
	assertT310Report(t, baselineReport, RecoveryClean, 2, 0, "")
	updatedAt := baseline.Conversation.UpdatedAt

	t.Run("resolved thresholds use a strict calendar-day boundary", func(t *testing.T) {
		thresholds := []struct {
			name string
			days int64
		}{
			{name: "one day", days: 1},
			{name: "resolved default seven days", days: 7},
			{name: "thirty days", days: 30},
			{name: "maximum 3650 days", days: 3650},
		}
		moments := []struct {
			name   string
			delta  time.Duration
			wanted bool
		}{
			{name: "exact boundary", wanted: false},
			{name: "one nanosecond over", delta: time.Nanosecond, wanted: true},
			{name: "one nanosecond under", delta: -time.Nanosecond, wanted: false},
		}
		for _, threshold := range thresholds {
			for _, moment := range moments {
				t.Run(threshold.name+"/"+moment.name, func(t *testing.T) {
					clockCalls := 0
					currentTime := updatedAt.AddDate(0, 0, int(threshold.days)).Add(moment.delta)
					loaded, report, err := recoverV2RecordsForLoad(context.Background(), codec, sessionID, bytes.NewReader(data), v2RecoveryMetadataOptions{
						GapReminderDays: threshold.days,
						Now: func() time.Time {
							clockCalls++
							return currentTime
						},
					})
					if err != nil {
						t.Fatalf("recover with gap reminder: %v", err)
					}
					if clockCalls != 1 || !reflect.DeepEqual(loaded, baseline) || report.Status != RecoveryClean ||
						report.LastValidRevision != baselineReport.LastValidRevision || report.SkippedRecords != 0 {
						t.Fatalf("decorated recovery changed state/report: calls=%d state=%#v report=%#v", clockCalls, loaded, report)
					}
					wantCount := 0
					if moment.wanted {
						wantCount = 1
					}
					if got := countT314Diagnostic(report, v2RecoveryGapReminderCode); got != wantCount {
						t.Fatalf("gap reminder count=%d, want=%d: %#v", got, wantCount, report)
					}
					if moment.wanted {
						assertT314GapDiagnostic(t, report)
					} else if len(report.Diagnostics) != 0 {
						t.Fatalf("non-reminder boundary produced diagnostics: %#v", report.Diagnostics)
					}
					if persisted := mustComputePersistedState(t, loaded.Conversation, loaded.Persisted.Revision); persisted != loaded.Persisted {
						t.Fatalf("reminder changed persisted digest: got=%#v want=%#v", persisted, loaded.Persisted)
					}
				})
			}
		}
	})

	t.Run("invalid resolved values fail before reading", func(t *testing.T) {
		for _, days := range []int64{0, -1, 3651} {
			t.Run(fmt.Sprintf("days_%d", days), func(t *testing.T) {
				reader := &t314ReadSpy{Reader: bytes.NewReader(data)}
				clockCalls := 0
				loaded, report, err := recoverV2RecordsForLoad(context.Background(), codec, sessionID, reader, v2RecoveryMetadataOptions{
					GapReminderDays: days,
					Now: func() time.Time {
						clockCalls++
						return updatedAt
					},
				})
				if !errors.Is(err, ErrV2RecoveryConfig) || !reflect.DeepEqual(loaded, v2LoadState{}) ||
					!reflect.DeepEqual(report, RecoveryReport{}) || reader.reads != 0 || clockCalls != 0 {
					t.Fatalf("invalid days result: state=%#v report=%#v reads=%d clocks=%d err=%v", loaded, report, reader.reads, clockCalls, err)
				}
			})
		}

		reader := &t314ReadSpy{Reader: bytes.NewReader(data)}
		loaded, report, err := recoverV2RecordsForLoad(context.Background(), codec, sessionID, reader, v2RecoveryMetadataOptions{
			GapReminderDays: 7,
			Now:             func() time.Time { return time.Time{} },
		})
		if !errors.Is(err, ErrV2RecoveryConfig) || !reflect.DeepEqual(loaded, v2LoadState{}) ||
			!reflect.DeepEqual(report, RecoveryReport{}) || reader.reads != 0 {
			t.Fatalf("zero clock result: state=%#v report=%#v reads=%d err=%v", loaded, report, reader.reads, err)
		}
	})

	t.Run("partial recovery retains corruption metadata and adds one reminder", func(t *testing.T) {
		torn := bytes.TrimSuffix(lines[1], []byte{'\n'})
		partialData := append(append([]byte(nil), lines[0]...), torn...)
		partialBefore := bytes.Clone(partialData)
		baseState, baseReport, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(partialData))
		if err != nil {
			t.Fatalf("recover partial baseline: %v", err)
		}
		loaded, report, err := recoverV2RecordsForLoad(context.Background(), codec, sessionID, bytes.NewReader(partialData), v2RecoveryMetadataOptions{
			GapReminderDays: 7,
			Now:             func() time.Time { return baseState.Conversation.UpdatedAt.AddDate(0, 0, 7).Add(time.Nanosecond) },
		})
		if err != nil || !reflect.DeepEqual(loaded, baseState) || report.Status != RecoveryPartial ||
			report.LastValidRevision != baseReport.LastValidRevision || report.SkippedRecords != baseReport.SkippedRecords ||
			len(report.Diagnostics) != len(baseReport.Diagnostics)+1 || !reflect.DeepEqual(report.Diagnostics[:len(baseReport.Diagnostics)], baseReport.Diagnostics) {
			t.Fatalf("partial reminder changed recovery: state=%#v report=%#v err=%v", loaded, report, err)
		}
		if countT314Diagnostic(report, v2RecoveryTornTailCode) != 1 || countT314Diagnostic(report, v2RecoveryGapReminderCode) != 1 {
			t.Fatalf("partial reminder diagnostic set = %#v", report.Diagnostics)
		}
		assertT314GapDiagnostic(t, report)
		if !bytes.Equal(partialData, partialBefore) {
			t.Fatal("gap reminder modified partial source bytes")
		}
	})

	t.Run("placeholder zero and future times never remind", func(t *testing.T) {
		loaded, report, err := recoverV2RecordsForLoad(context.Background(), codec, sessionID, bytes.NewReader(nil), v2RecoveryMetadataOptions{
			GapReminderDays: 7,
			Now:             func() time.Time { return updatedAt.AddDate(1, 0, 0) },
		})
		if err != nil || !reflect.DeepEqual(loaded, v2LoadState{}) || report.Status != RecoveryPlaceholder ||
			countT314Diagnostic(report, v2RecoveryGapReminderCode) != 0 || len(report.Diagnostics) != 1 {
			t.Fatalf("placeholder reminder result: state=%#v report=%#v err=%v", loaded, report, err)
		}
		if v2GapReminderDue(time.Time{}, updatedAt, 7) || v2GapReminderDue(updatedAt.Add(time.Nanosecond), updatedAt, 7) {
			t.Fatal("zero or future UpdatedAt unexpectedly triggered reminder")
		}
	})

	t.Run("cancellation and core recovery do not publish reminder metadata", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &t314ReadSpy{Reader: bytes.NewReader(data)}
		clockCalls := 0
		loaded, report, err := recoverV2RecordsForLoad(ctx, codec, sessionID, reader, v2RecoveryMetadataOptions{
			GapReminderDays: 7,
			Now: func() time.Time {
				clockCalls++
				return updatedAt.AddDate(0, 0, 8)
			},
		})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, v2LoadState{}) ||
			!reflect.DeepEqual(report, RecoveryReport{}) || reader.reads != 0 || clockCalls != 0 {
			t.Fatalf("pre-canceled reminder result: state=%#v report=%#v reads=%d clocks=%d err=%v", loaded, report, reader.reads, clockCalls, err)
		}
		coreState, coreReport, err := recoverV2Records(context.Background(), codec, sessionID, bytes.NewReader(data))
		if err != nil || !reflect.DeepEqual(coreState, baseline) || countT314Diagnostic(coreReport, v2RecoveryGapReminderCode) != 0 {
			t.Fatalf("core recovery gained policy metadata: state=%#v report=%#v err=%v", coreState, coreReport, err)
		}
	})

	if !bytes.Equal(data, original) {
		t.Fatal("gap reminder modified caller-owned input bytes")
	}
}

func assertT310Report(t *testing.T, report RecoveryReport, status RecoveryStatus, lastRevision uint64, skipped int, code string) {
	t.Helper()
	if report.Status != status || report.LastValidRevision != lastRevision || report.SkippedRecords != skipped {
		t.Fatalf("recovery report status=%q revision=%d skipped=%d, want status=%q revision=%d skipped=%d", report.Status, report.LastValidRevision, report.SkippedRecords, status, lastRevision, skipped)
	}
	if code == "" {
		if len(report.Diagnostics) != 0 {
			t.Fatalf("clean recovery retained diagnostics: %#v", report.Diagnostics)
		}
		return
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("recovery retained %d diagnostics, want bounded single diagnostic", len(report.Diagnostics))
	}
	diagnostic := report.Diagnostics[0]
	if diagnostic.Code != code || diagnostic.Source != "conversation" || diagnostic.Severity != diagnostics.SeverityWarning {
		t.Fatalf("recovery diagnostic identity = %#v, want code=%q source=conversation severity=warning", diagnostic, code)
	}
	if diagnostic.Message == "" || len(diagnostic.Message) > 256 {
		t.Fatalf("recovery diagnostic message length = %d, want 1..256", len(diagnostic.Message))
	}
	if diagnostic.Path != "" || len(diagnostic.Attributes) != 0 {
		t.Fatalf("recovery diagnostic retained unsafe optional metadata: %#v", diagnostic)
	}
}

func assertT310NoLeaks(t *testing.T, report RecoveryReport, forbidden ...string) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal recovery report: %v", err)
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("recovery report leaked canary %q: %s", value, encoded)
		}
	}
}

func assertT310FileUnchanged(t *testing.T, path string, original []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read source after recovery: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("recovery modified source file: got %d bytes, want original %d", len(after), len(original))
	}
}

func countT314Diagnostic(report RecoveryReport, code string) int {
	count := 0
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}

func assertT314GapDiagnostic(t *testing.T, report RecoveryReport) {
	t.Helper()
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code != v2RecoveryGapReminderCode {
			continue
		}
		if diagnostic.Message != v2RecoveryGapReminderMessage || diagnostic.Source != v2RecoverySource ||
			diagnostic.Severity != diagnostics.SeverityInfo || diagnostic.Path != "" || len(diagnostic.Attributes) != 0 ||
			len(diagnostic.Message) > 256 {
			t.Fatalf("unsafe gap reminder diagnostic: %#v", diagnostic)
		}
		encoded, err := json.Marshal(diagnostic)
		if err != nil {
			t.Fatalf("marshal gap reminder diagnostic: %v", err)
		}
		for _, forbidden := range []string{
			"conversation-1",
			"安全会话",
			"first message",
			"context summary",
			time.Date(2026, time.August, 2, 14, 5, 1, 123456789, time.UTC).Format(time.RFC3339Nano),
		} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("gap reminder diagnostic leaked %q: %s", forbidden, encoded)
			}
		}
		return
	}
	t.Fatalf("missing gap reminder diagnostic in %#v", report.Diagnostics)
}

type t314ReadSpy struct {
	io.Reader
	reads int
}

func (r *t314ReadSpy) Read(buffer []byte) (int, error) {
	r.reads++
	return r.Reader.Read(buffer)
}

type t310ErrorAfterReader struct {
	data []byte
	err  error
}

func (r *t310ErrorAfterReader) Read(buffer []byte) (int, error) {
	if len(r.data) > 0 {
		count := copy(buffer, r.data)
		r.data = r.data[count:]
		return count, nil
	}
	return 0, r.err
}

type t39Chain struct {
	anchor            JSONLRecordV2
	batch             JSONLRecordV2
	snapshot          JSONLRecordV2
	finalConversation *Conversation
	finalPersisted    PersistedState
}

func newT39Chain(t *testing.T) t39Chain {
	t.Helper()
	anchor := validV2SnapshotRecord(t, 1)
	anchorState := mustComputePersistedState(t, anchor.Snapshot, 1)
	afterBatch, batch, batchState := appendT39Batch(t, anchor.Snapshot, anchorState, "ordered batch append", time.Second)
	finalConversation := cloneConversationV2(afterBatch)
	finalConversation.Title = digestSafeText("validated replacement metadata")
	finalConversation.Context.Summary = digestSafeText("validated replacement summary")
	finalConversation.UpdatedAt = finalConversation.UpdatedAt.Add(time.Second)
	finalState := mustComputePersistedState(t, finalConversation, 3)
	snapshot := JSONLRecordV2{
		Version:        JSONLVersionV2,
		Kind:           RecordSnapshot,
		SessionID:      anchor.SessionID,
		Revision:       3,
		PreviousDigest: batchState.Digest,
		Digest:         finalState.Digest,
		Snapshot:       cloneConversationV2(finalConversation),
	}
	return t39Chain{
		anchor:            anchor,
		batch:             batch,
		snapshot:          snapshot,
		finalConversation: finalConversation,
		finalPersisted:    finalState,
	}
}

func appendT39Batch(t *testing.T, previous *Conversation, persisted PersistedState, content string, advance time.Duration) (*Conversation, JSONLRecordV2, PersistedState) {
	t.Helper()
	candidate := cloneConversationV2(previous)
	candidate.Messages = append(candidate.Messages, Message{
		Role:      RoleAssistant,
		Content:   digestSafeText(content),
		CreatedAt: candidate.UpdatedAt.Add(advance),
	})
	candidate.UpdatedAt = candidate.UpdatedAt.Add(advance)
	next := mustComputePersistedState(t, candidate, persisted.Revision+1)
	record := JSONLRecordV2{
		Version:        JSONLVersionV2,
		Kind:           RecordBatch,
		SessionID:      candidate.ID,
		Revision:       next.Revision,
		PreviousDigest: persisted.Digest,
		Digest:         next.Digest,
		Batch: &MessageBatch{
			BaseMessageCount: persisted.MessageCount,
			Messages:         cloneV2Messages(candidate.Messages[persisted.MessageCount:]),
			UpdatedAt:        candidate.UpdatedAt,
		},
	}
	return candidate, record, next
}

func encodeT39Records(t *testing.T, codec *V2RecordCodec, records ...JSONLRecordV2) ([]byte, [][]byte) {
	t.Helper()
	var data []byte
	lines := make([][]byte, 0, len(records))
	for index, record := range records {
		line, err := codec.Encode(record.SessionID, record)
		if err != nil {
			t.Fatalf("encode record %d: %v", index, err)
		}
		lines = append(lines, line)
		data = append(data, line...)
	}
	return data, lines
}

func assertT39AnchorPrefix(t *testing.T, loaded v2LoadState, anchor JSONLRecordV2, verifiedBytes int) {
	t.Helper()
	wantPersisted := mustComputePersistedState(t, anchor.Snapshot, anchor.Revision)
	if loaded.RecordCount != 1 || loaded.VerifiedBytes != int64(verifiedBytes) {
		t.Fatalf("verified prefix records=%d bytes=%d, want records=1 bytes=%d", loaded.RecordCount, loaded.VerifiedBytes, verifiedBytes)
	}
	if loaded.Persisted != wantPersisted || !reflect.DeepEqual(loaded.Conversation, anchor.Snapshot) {
		t.Fatalf("last verified prefix changed: state=%#v conversation=%#v", loaded.Persisted, loaded.Conversation)
	}
}

func t39Digest(character string) StateDigest {
	return StateDigest(strings.Repeat(character, sha256HexLength))
}

type t39CancelingReader struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (r *t39CancelingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if count > 0 {
		r.cancel()
	}
	return count, err
}

type t39CloseSpy struct {
	*bytes.Reader
	closed bool
}

func (r *t39CloseSpy) Close() error {
	r.closed = true
	return nil
}
