package conversation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
)

func TestSaveClassificationUsesNormalizedState(t *testing.T) {
	t.Run("normalized equality is noop", func(t *testing.T) {
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 17)
		current := cloneConversationV2(previous)
		zone := time.FixedZone("save-classifier", 8*60*60)
		current.CreatedAt = current.CreatedAt.In(zone)
		current.UpdatedAt = current.UpdatedAt.In(zone)
		for index := range current.Messages {
			current.Messages[index].CreatedAt = current.Messages[index].CreatedAt.In(zone)
			if state := current.Messages[index].Tool; state != nil && state.Artifact != nil {
				state.Artifact.CreatedAt = state.Artifact.CreatedAt.In(zone)
			}
		}
		if current.Context != nil && current.Context.LastCompressionAt != nil {
			converted := current.Context.LastCompressionAt.In(zone)
			current.Context.LastCompressionAt = &converted
		}

		input := v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean}
		beforeCurrent := cloneConversationV2(current)
		beforePrevious := cloneConversationV2(previous)
		plan, err := classifyV2Save(input)
		if err != nil {
			t.Fatalf("classify normalized state: %v", err)
		}
		if plan.Result.Kind != SaveNoop || plan.Result.Persisted != persisted || plan.Record != nil {
			t.Fatalf("normalized equality plan = %#v, want exact noop", plan)
		}
		if !reflect.DeepEqual(current, beforeCurrent) || !reflect.DeepEqual(previous, beforePrevious) {
			t.Fatal("noop classification mutated input state")
		}
	})

	t.Run("nil and empty normalization is noop", func(t *testing.T) {
		now := time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)
		previous := &Conversation{ID: "empty-session", CreatedAt: now, UpdatedAt: now}
		persisted := mustComputePersistedState(t, previous, 3)
		current := cloneConversationV2(previous)
		current.Messages = []Message{}
		current.Context = &ContextMetadata{}

		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
		if err != nil || plan.Result.Kind != SaveNoop || plan.Result.Persisted != persisted || plan.Record != nil {
			t.Fatalf("nil/empty normalized plan = %#v, err=%v", plan, err)
		}
	})

	t.Run("pure tail append creates exactly one batch", func(t *testing.T) {
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 4)
		current := cloneConversationV2(previous)
		current.Messages = append(current.Messages, Message{
			Role:      RoleAssistant,
			Content:   digestSafeText("new tail"),
			CreatedAt: current.UpdatedAt.Add(time.Second),
		})
		current.UpdatedAt = current.UpdatedAt.Add(2 * time.Second)

		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
		if err != nil {
			t.Fatalf("classify tail append: %v", err)
		}
		if plan.Result.Kind != SaveBatch || plan.Record == nil || plan.Record.Kind != RecordBatch || plan.Record.Batch == nil || plan.Record.Snapshot != nil {
			t.Fatalf("tail append plan = %#v, want one batch", plan)
		}
		if plan.Record.Revision != persisted.Revision+1 || plan.Record.PreviousDigest != persisted.Digest || plan.Record.Batch.BaseMessageCount != persisted.MessageCount || len(plan.Record.Batch.Messages) != 1 {
			t.Fatalf("batch linkage/payload mismatch: %#v", plan.Record)
		}
		want := mustComputePersistedState(t, current, persisted.Revision+1)
		if plan.Result.Persisted != want || plan.Record.Digest != want.Digest || plan.Record.Batch.UpdatedAt != current.UpdatedAt {
			t.Fatalf("batch final state = %#v / %#v, want %#v", plan.Result, plan.Record, want)
		}
		if err := plan.Record.Validate(RecordValidationContext{ExpectedSessionID: current.ID, Previous: &persisted}); err != nil {
			t.Fatalf("classified batch does not validate: %v", err)
		}

		current.Messages[len(current.Messages)-1].Content = digestSafeText("mutated after classification")
		if plan.Record.Batch.Messages[0].Content.Text() != "new tail" {
			t.Fatal("batch payload aliases caller messages")
		}
	})

	t.Run("existing or metadata changes create snapshot", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*Conversation)
		}{
			{name: "existing message", mutate: func(value *Conversation) { value.Messages[0].Content = digestSafeText("edited") }},
			{name: "existing tool state", mutate: func(value *Conversation) { value.Messages[1].Tool.Status = "success" }},
			{name: "title", mutate: func(value *Conversation) { value.Title = digestSafeText("changed title") }},
			{name: "context", mutate: func(value *Conversation) { value.Context.Summary = digestSafeText("changed summary") }},
			{name: "created time", mutate: func(value *Conversation) { value.CreatedAt = value.CreatedAt.Add(time.Second) }},
			{name: "updated time without append", mutate: func(value *Conversation) { value.UpdatedAt = value.UpdatedAt.Add(time.Second) }},
			{name: "removed message", mutate: func(value *Conversation) { value.Messages = value.Messages[:1] }},
			{name: "reordered messages", mutate: func(value *Conversation) {
				value.Messages[0], value.Messages[1] = value.Messages[1], value.Messages[0]
			}},
			{name: "append plus title", mutate: func(value *Conversation) {
				value.Messages = append(value.Messages, Message{Role: RoleAssistant, Content: digestSafeText("tail"), CreatedAt: value.UpdatedAt.Add(time.Second)})
				value.Title = digestSafeText("changed with append")
				value.UpdatedAt = value.UpdatedAt.Add(2 * time.Second)
			}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				previous := stateDigestFixture()
				persisted := mustComputePersistedState(t, previous, 8)
				current := cloneConversationV2(previous)
				testCase.mutate(current)

				plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
				if err != nil {
					t.Fatalf("classify snapshot change: %v", err)
				}
				assertV2SnapshotSavePlan(t, plan, current, persisted)
			})
		}
	})

	t.Run("recovery forces snapshot even when state is unchanged", func(t *testing.T) {
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 12)
		current := cloneConversationV2(previous)
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryPartial})
		if err != nil {
			t.Fatalf("classify recovered unchanged state: %v", err)
		}
		assertV2SnapshotSavePlan(t, plan, current, persisted)
		if plan.Result.Persisted.Digest != persisted.Digest {
			t.Fatal("recovery checkpoint unexpectedly changed normalized state digest")
		}
	})

	t.Run("recovery turns an append into snapshot", func(t *testing.T) {
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 9)
		current := cloneConversationV2(previous)
		current.Messages = append(current.Messages, Message{Role: RoleAssistant, Content: digestSafeText("recovered tail"), CreatedAt: current.UpdatedAt.Add(time.Second)})
		current.UpdatedAt = current.UpdatedAt.Add(2 * time.Second)
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryPartial})
		if err != nil {
			t.Fatalf("classify recovered append: %v", err)
		}
		assertV2SnapshotSavePlan(t, plan, current, persisted)
	})

	t.Run("new conversation starts with snapshot anchor", func(t *testing.T) {
		current := stateDigestFixture()
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Recovery: RecoveryClean})
		if err != nil {
			t.Fatalf("classify new conversation: %v", err)
		}
		if plan.Result.Kind != SaveSnapshot || plan.Result.Persisted.Revision != 1 || plan.Record == nil || plan.Record.Revision != 1 || plan.Record.PreviousDigest != "" {
			t.Fatalf("new conversation plan = %#v, want revision-one snapshot anchor", plan)
		}
		if err := plan.Record.Validate(RecordValidationContext{ExpectedSessionID: current.ID}); err != nil {
			t.Fatalf("new snapshot anchor does not validate: %v", err)
		}
	})

	t.Run("invalid baselines fail closed", func(t *testing.T) {
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 2)
		cases := []struct {
			name  string
			input v2SaveClassificationInput
		}{
			{name: "nil conversation", input: v2SaveClassificationInput{Previous: previous, Persisted: persisted, Recovery: RecoveryClean}},
			{name: "placeholder recovery", input: v2SaveClassificationInput{Conversation: cloneConversationV2(previous), Previous: previous, Persisted: persisted, Recovery: RecoveryPlaceholder}},
			{name: "missing previous", input: v2SaveClassificationInput{Conversation: cloneConversationV2(previous), Persisted: persisted, Recovery: RecoveryClean}},
			{name: "mismatched previous", input: v2SaveClassificationInput{Conversation: cloneConversationV2(previous), Previous: func() *Conversation {
				value := cloneConversationV2(previous)
				value.Title = digestSafeText("wrong")
				return value
			}(), Persisted: persisted, Recovery: RecoveryClean}},
			{name: "revision overflow", input: v2SaveClassificationInput{Conversation: cloneConversationV2(previous), Previous: previous, Persisted: PersistedState{Revision: math.MaxUint64, MessageCount: persisted.MessageCount, Digest: persisted.Digest, MessagesDigest: persisted.MessagesDigest}, Recovery: RecoveryPartial}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				plan, err := classifyV2Save(testCase.input)
				if !errors.Is(err, ErrV2SaveClassification) || !reflect.DeepEqual(plan, v2SavePlan{}) {
					t.Fatalf("invalid classification = %#v, err=%v", plan, err)
				}
			})
		}
	})
}

func assertV2SnapshotSavePlan(t *testing.T, plan v2SavePlan, current *Conversation, persisted PersistedState) {
	t.Helper()
	if plan.Result.Kind != SaveSnapshot || plan.Record == nil || plan.Record.Kind != RecordSnapshot || plan.Record.Snapshot == nil || plan.Record.Batch != nil {
		t.Fatalf("snapshot plan = %#v", plan)
	}
	want := mustComputePersistedState(t, current, persisted.Revision+1)
	if plan.Result.Persisted != want || plan.Record.Revision != want.Revision || plan.Record.Digest != want.Digest || plan.Record.PreviousDigest != persisted.Digest {
		t.Fatalf("snapshot linkage/state = %#v / %#v, want %#v", plan.Result, plan.Record, want)
	}
	if err := plan.Record.Validate(RecordValidationContext{ExpectedSessionID: current.ID, Previous: &persisted}); err != nil {
		t.Fatalf("classified snapshot does not validate: %v", err)
	}
	before := cloneConversationV2(plan.Record.Snapshot)
	if len(current.Messages) > 0 {
		current.Messages[0].Content = digestSafeText("mutated after snapshot classification")
	}
	if !reflect.DeepEqual(plan.Record.Snapshot, before) {
		t.Fatal("snapshot payload aliases caller conversation")
	}
}

func TestBatchSavePublishesExactlyOneRevision(t *testing.T) {
	ctx := context.Background()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024})
	if err != nil {
		t.Fatalf("create v2 codec: %v", err)
	}
	previous := stateDigestFixture()
	persisted := mustComputePersistedState(t, previous, 31)
	current := cloneConversationV2(previous)
	current.Messages = append(current.Messages, Message{Role: RoleAssistant, Content: digestSafeText("one durable tail"), CreatedAt: current.UpdatedAt.Add(time.Second)})
	current.UpdatedAt = current.UpdatedAt.Add(2 * time.Second)
	plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
	if err != nil || plan.Result.Kind != SaveBatch {
		t.Fatalf("classify batch: plan=%#v err=%v", plan, err)
	}

	t.Run("real append writes one complete revision", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversation.jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create existing append target: %v", err)
		}
		published := persisted
		committer := newV2BatchCommitter(codec)
		result, err := committer.commitBatch(ctx, path, plan, &published)
		if err != nil {
			t.Fatalf("commit batch: %v", err)
		}
		if result != plan.Result || published != plan.Result.Persisted || published.Revision != persisted.Revision+1 {
			t.Fatalf("published state/result = %#v / %#v, want %#v", published, result, plan.Result)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read committed batch: %v", err)
		}
		wantLine, err := codec.Encode(current.ID, *plan.Record)
		if err != nil {
			t.Fatalf("encode expected batch: %v", err)
		}
		if !bytes.Equal(data, wantLine) || bytes.Count(data, []byte{'\n'}) != 1 {
			t.Fatalf("committed bytes are not exactly one complete JSONL record: got=%d want=%d", len(data), len(wantLine))
		}
		decoder, err := codec.NewDecoder(current.ID, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("create committed record decoder: %v", err)
		}
		decoded, err := decoder.Decode()
		if err != nil || !reflect.DeepEqual(decoded, *plan.Record) {
			t.Fatalf("reload committed batch = %#v, err=%v", decoded, err)
		}
		if err := decoded.Validate(RecordValidationContext{ExpectedSessionID: current.ID, Previous: &persisted}); err != nil {
			t.Fatalf("committed batch linkage invalid: %v", err)
		}

		beforeRetry := append([]byte(nil), data...)
		if result, err := committer.commitBatch(ctx, path, plan, &published); !errors.Is(err, ErrV2BatchCommit) || !reflect.DeepEqual(result, SaveResult{}) {
			t.Fatalf("stale duplicate plan result=%#v err=%v", result, err)
		}
		afterRetry, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(afterRetry, beforeRetry) || published != plan.Result.Persisted {
			t.Fatalf("stale retry changed disk/state: bytes_equal=%v state=%#v err=%v", bytes.Equal(afterRetry, beforeRetry), published, err)
		}
	})

	t.Run("encode completes before open", func(t *testing.T) {
		limited, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 1, MaxSessionBytes: 1024})
		if err != nil {
			t.Fatalf("create limited codec: %v", err)
		}
		committer := newV2BatchCommitter(limited)
		opened := false
		committer.openAppend = func(string) (v2AppendFile, error) {
			opened = true
			return nil, errors.New("unexpected open")
		}
		published := persisted
		if _, err := committer.commitBatch(ctx, "unused.jsonl", plan, &published); err == nil {
			t.Fatal("over-limit batch unexpectedly committed")
		}
		if opened || published != persisted {
			t.Fatalf("encode failure opened file or published state: opened=%v state=%#v", opened, published)
		}
	})

	t.Run("persistence barriers gate publication", func(t *testing.T) {
		cases := []struct {
			name      string
			failAt    string
			wantTrace []string
		}{
			{name: "open", failAt: "open", wantTrace: []string{"open"}},
			{name: "write", failAt: "write", wantTrace: []string{"open", "write", "close"}},
			{name: "flush", failAt: "flush", wantTrace: []string{"open", "write", "flush", "close"}},
			{name: "sync", failAt: "sync", wantTrace: []string{"open", "write", "flush", "sync", "close"}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				published := persisted
				trace := &v2BatchCommitTrace{failAt: testCase.failAt, published: &published, initial: persisted, desired: plan.Result.Persisted}
				committer := tracedV2BatchCommitter(codec, trace)
				result, err := committer.commitBatch(ctx, "trace.jsonl", plan, &published)
				if !errors.Is(err, ErrV2BatchCommit) || !reflect.DeepEqual(result, SaveResult{}) {
					t.Fatalf("barrier failure result=%#v err=%v", result, err)
				}
				if published != persisted {
					t.Fatalf("%s failure advanced state: %#v", testCase.failAt, published)
				}
				if !reflect.DeepEqual(trace.events, testCase.wantTrace) {
					t.Fatalf("%s trace = %v, want %v", testCase.failAt, trace.events, testCase.wantTrace)
				}
			})
		}
	})

	t.Run("sync precedes publication and short writes complete", func(t *testing.T) {
		published := persisted
		trace := &v2BatchCommitTrace{published: &published, initial: persisted, desired: plan.Result.Persisted, maxWrite: 11}
		committer := tracedV2BatchCommitter(codec, trace)
		result, err := committer.commitBatch(ctx, "trace.jsonl", plan, &published)
		if err != nil || result != plan.Result || published != plan.Result.Persisted {
			t.Fatalf("traced success result=%#v state=%#v err=%v", result, published, err)
		}
		if !trace.syncSawInitial || !trace.closeSawDesired {
			t.Fatalf("publication ordering not observed: sync_initial=%v close_desired=%v", trace.syncSawInitial, trace.closeSawDesired)
		}
		if trace.writeCalls < 2 || len(trace.data) == 0 || trace.data[len(trace.data)-1] != '\n' {
			t.Fatalf("short writes were not completed: calls=%d bytes=%d", trace.writeCalls, len(trace.data))
		}
		wantPrefix := []string{"open", "write"}
		if len(trace.events) < len(wantPrefix) || !reflect.DeepEqual(trace.events[:len(wantPrefix)], wantPrefix) ||
			!reflect.DeepEqual(trace.events[len(trace.events)-3:], []string{"flush", "sync", "close"}) {
			t.Fatalf("successful barrier order = %v", trace.events)
		}
	})
}

type v2BatchCommitTrace struct {
	failAt          string
	events          []string
	data            []byte
	published       *PersistedState
	initial         PersistedState
	desired         PersistedState
	maxWrite        int
	writeCalls      int
	syncSawInitial  bool
	closeSawDesired bool
}

type tracedV2AppendFile struct {
	trace *v2BatchCommitTrace
}

type blockingSyncV2AppendFile struct {
	*os.File
	syncEntered chan<- struct{}
	releaseSync <-chan struct{}
}

type cancelAfterPathRemovedContext struct {
	context.Context
	path string
	done chan struct{}
	once sync.Once
}

func (c *cancelAfterPathRemovedContext) Done() <-chan struct{} {
	return c.done
}

func (c *cancelAfterPathRemovedContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(c.path); os.IsNotExist(err) {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func (f *blockingSyncV2AppendFile) Sync() error {
	close(f.syncEntered)
	<-f.releaseSync
	return f.File.Sync()
}

func (f *tracedV2AppendFile) Write(data []byte) (int, error) {
	f.trace.events = append(f.trace.events, "write")
	f.trace.writeCalls++
	if f.trace.failAt == "write" {
		return 0, errors.New("injected write failure")
	}
	count := len(data)
	if f.trace.maxWrite > 0 && count > f.trace.maxWrite {
		count = f.trace.maxWrite
	}
	f.trace.data = append(f.trace.data, data[:count]...)
	return count, nil
}

func (f *tracedV2AppendFile) Sync() error {
	f.trace.events = append(f.trace.events, "sync")
	f.trace.syncSawInitial = f.trace.published != nil && *f.trace.published == f.trace.initial
	if f.trace.failAt == "sync" {
		return errors.New("injected sync failure")
	}
	return nil
}

func (f *tracedV2AppendFile) Close() error {
	f.trace.events = append(f.trace.events, "close")
	f.trace.closeSawDesired = f.trace.published != nil && *f.trace.published == f.trace.desired
	return nil
}

type tracedV2FlushWriter struct {
	file  *tracedV2AppendFile
	trace *v2BatchCommitTrace
}

func (w *tracedV2FlushWriter) Write(data []byte) (int, error) {
	return w.file.Write(data)
}

func (w *tracedV2FlushWriter) Flush() error {
	w.trace.events = append(w.trace.events, "flush")
	if w.trace.failAt == "flush" {
		return errors.New("injected flush failure")
	}
	return nil
}

func tracedV2BatchCommitter(codec *V2RecordCodec, trace *v2BatchCommitTrace) *v2BatchCommitter {
	committer := newV2BatchCommitter(codec)
	committer.openAppend = func(string) (v2AppendFile, error) {
		trace.events = append(trace.events, "open")
		if trace.failAt == "open" {
			return nil, errors.New("injected open failure")
		}
		return &tracedV2AppendFile{trace: trace}, nil
	}
	committer.newBuffer = func(writer io.Writer) v2FlushWriter {
		file, ok := writer.(*tracedV2AppendFile)
		if !ok {
			return nil
		}
		return &tracedV2FlushWriter{file: file, trace: trace}
	}
	return committer
}

func TestSnapshotSaveIsAtomicAndReloadable(t *testing.T) {
	ctx := context.Background()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024})
	if err != nil {
		t.Fatalf("create v2 codec: %v", err)
	}

	t.Run("new snapshot anchor is reloadable", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "conversation.jsonl")
		current := stateDigestFixture()
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Recovery: RecoveryClean})
		if err != nil || plan.Result.Kind != SaveSnapshot {
			t.Fatalf("classify snapshot anchor: plan=%#v err=%v", plan, err)
		}
		published := PersistedState{}
		result, err := newV2SnapshotCommitter(codec).commitSnapshot(ctx, path, 0, plan, &published)
		if err != nil {
			t.Fatalf("commit snapshot anchor: %v", err)
		}
		if result != plan.Result || published != plan.Result.Persisted || published.Revision != 1 {
			t.Fatalf("snapshot anchor publication = %#v / %#v", result, published)
		}
		records := reloadV2Records(t, path, current.ID)
		if len(records) != 1 || records[0].Revision != 1 || records[0].Snapshot == nil || !reflect.DeepEqual(records[0].Snapshot, current) {
			t.Fatalf("reloaded snapshot anchor = %#v", records)
		}
		if err := records[0].Validate(RecordValidationContext{ExpectedSessionID: current.ID}); err != nil {
			t.Fatalf("reloaded anchor validation: %v", err)
		}
		assertOnlyV2TargetRemains(t, directory, filepath.Base(path))
	})

	t.Run("verified prefix replaces corrupt tail", func(t *testing.T) {
		const corruptCanary = "unverified-tail-secret-canary"
		directory := t.TempDir()
		path := filepath.Join(directory, "conversation.jsonl")
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 1)
		anchor := validV2SnapshotRecord(t, 1)
		anchorLine, err := codec.Encode(previous.ID, anchor)
		if err != nil {
			t.Fatalf("encode anchor: %v", err)
		}
		original := append(append([]byte(nil), anchorLine...), []byte(corruptCanary)...)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatalf("seed corrupt-tail file: %v", err)
		}

		current := cloneConversationV2(previous)
		current.Title = digestSafeText("recovered snapshot")
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryPartial})
		if err != nil || plan.Result.Kind != SaveSnapshot {
			t.Fatalf("classify recovery snapshot: plan=%#v err=%v", plan, err)
		}
		published := persisted
		result, err := newV2SaveCommitter(codec).commit(ctx, path, int64(len(anchorLine)), current, plan, &published)
		if err != nil {
			t.Fatalf("commit recovery snapshot: %v", err)
		}
		if result != plan.Result || published != plan.Result.Persisted || published.Revision != 2 {
			t.Fatalf("recovery snapshot publication = %#v / %#v", result, published)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read replaced snapshot file: %v", err)
		}
		if bytes.Contains(data, []byte(corruptCanary)) || !bytes.HasPrefix(data, anchorLine) || bytes.Count(data, []byte{'\n'}) != 2 {
			t.Fatalf("replacement retained unverified bytes or lost verified prefix")
		}

		// Recreate the codec and open the file again so the assertion does not
		// reuse the writer or any in-memory record from the commit path.
		freshCodec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024})
		if err != nil {
			t.Fatalf("recreate v2 codec: %v", err)
		}
		records := reloadV2RecordsWithCodec(t, freshCodec, path, current.ID)
		if len(records) != 2 || records[1].Revision != 2 || records[1].Snapshot == nil || !reflect.DeepEqual(records[1].Snapshot, current) {
			t.Fatalf("fresh reload final snapshot = %#v", records)
		}
		if err := records[0].Validate(RecordValidationContext{ExpectedSessionID: current.ID}); err != nil {
			t.Fatalf("fresh reload anchor invalid: %v", err)
		}
		if err := records[1].Validate(RecordValidationContext{ExpectedSessionID: current.ID, Previous: &persisted}); err != nil {
			t.Fatalf("fresh reload successor invalid: %v", err)
		}
		if got := mustComputePersistedState(t, records[1].Snapshot, records[1].Revision); got != published {
			t.Fatalf("fresh reload state = %#v, want %#v", got, published)
		}
		assertOnlyV2TargetRemains(t, directory, filepath.Base(path))
	})

	t.Run("pre-replace failures preserve old file and state", func(t *testing.T) {
		cases := []string{"create", "flush", "sync", "replace"}
		for _, failAt := range cases {
			t.Run(failAt, func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "conversation.jsonl")
				previous := stateDigestFixture()
				persisted := mustComputePersistedState(t, previous, 1)
				anchor := validV2SnapshotRecord(t, 1)
				anchorLine, err := codec.Encode(previous.ID, anchor)
				if err != nil {
					t.Fatalf("encode failure-fixture anchor: %v", err)
				}
				oldBytes := append(append([]byte(nil), anchorLine...), []byte("old-unverified-tail")...)
				if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
					t.Fatalf("seed failure fixture: %v", err)
				}
				current := cloneConversationV2(previous)
				current.Title = digestSafeText("replacement candidate")
				current.UpdatedAt = current.UpdatedAt.Add(time.Second)
				plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryPartial})
				if err != nil {
					t.Fatalf("classify failure fixture: %v", err)
				}
				published := persisted
				committer := faultingV2SnapshotCommitter(codec, failAt)
				result, err := committer.commitSnapshot(ctx, path, int64(len(anchorLine)), plan, &published)
				if !errors.Is(err, ErrV2SnapshotCommit) || !reflect.DeepEqual(result, SaveResult{}) {
					t.Fatalf("%s failure result=%#v err=%v", failAt, result, err)
				}
				if published != persisted {
					t.Fatalf("%s failure advanced state: %#v", failAt, published)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, oldBytes) {
					t.Fatalf("%s failure changed old file: equal=%v err=%v", failAt, bytes.Equal(after, oldBytes), err)
				}
				assertOnlyV2TargetRemains(t, directory, filepath.Base(path))
			})
		}
	})
}

func reloadV2Records(t *testing.T, path string, sessionID string) []JSONLRecordV2 {
	t.Helper()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	return reloadV2RecordsWithCodec(t, codec, path, sessionID)
}

func reloadV2RecordsWithCodec(t *testing.T, codec *V2RecordCodec, path string, sessionID string) []JSONLRecordV2 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open v2 records: %v", err)
	}
	defer file.Close()
	decoder, err := codec.NewDecoder(sessionID, file)
	if err != nil {
		t.Fatalf("create v2 file decoder: %v", err)
	}
	var records []JSONLRecordV2
	for {
		record, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatalf("decode v2 record %d: %v", len(records), err)
		}
		records = append(records, record)
	}
}

func assertOnlyV2TargetRemains(t *testing.T, directory string, target string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read snapshot directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != target {
		t.Fatalf("snapshot staging residue in %s: %v", directory, entries)
	}
}

type faultingV2StageFile struct {
	v2StageFile
	failAt string
}

func (f *faultingV2StageFile) Sync() error {
	if f.failAt == "sync" {
		return errors.New("injected snapshot sync failure")
	}
	return f.v2StageFile.Sync()
}

type faultingV2FlushWriter struct {
	v2FlushWriter
	fail bool
}

func (w *faultingV2FlushWriter) Flush() error {
	if w.fail {
		return errors.New("injected snapshot flush failure")
	}
	return w.v2FlushWriter.Flush()
}

func faultingV2SnapshotCommitter(codec *V2RecordCodec, failAt string) *v2SnapshotCommitter {
	committer := newV2SnapshotCommitter(codec)
	originalCreate := committer.createStage
	originalBuffer := committer.newBuffer
	originalReplace := committer.replace
	committer.createStage = func(directory string, pattern string) (v2StageFile, error) {
		if failAt == "create" {
			return nil, errors.New("injected snapshot create failure")
		}
		stage, err := originalCreate(directory, pattern)
		if err != nil {
			return nil, err
		}
		return &faultingV2StageFile{v2StageFile: stage, failAt: failAt}, nil
	}
	committer.newBuffer = func(writer io.Writer) v2FlushWriter {
		return &faultingV2FlushWriter{v2FlushWriter: originalBuffer(writer), fail: failAt == "flush"}
	}
	committer.replace = func(stage string, target string) error {
		if failAt == "replace" || failAt == "rename" {
			return errors.New("injected snapshot replace failure")
		}
		return originalReplace(stage, target)
	}
	return committer
}

func TestSaveFailurePreservesLastCommittedState(t *testing.T) {
	ctx := context.Background()
	codec := mustV2TestCodec(t)

	t.Run("batch encode write and sync failures", func(t *testing.T) {
		for _, failAt := range []string{"encode", "write", "sync"} {
			t.Run(failAt, func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "conversation.jsonl")
				previous, persisted, anchorLine := seedV2AnchorFile(t, codec, path)
				current := cloneConversationV2(previous)
				current.Messages = append(current.Messages, Message{Role: RoleAssistant, Content: digestSafeText("retryable batch"), CreatedAt: current.UpdatedAt.Add(time.Second)})
				current.UpdatedAt = current.UpdatedAt.Add(2 * time.Second)
				plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
				if err != nil {
					t.Fatalf("classify batch failure fixture: %v", err)
				}
				published := persisted
				committer := newV2BatchCommitter(codec)
				if failAt == "encode" {
					committer.codec = mustV2LimitedTestCodec(t)
				} else {
					committer.openAppend = func(path string) (v2AppendFile, error) {
						file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
						if err != nil {
							return nil, err
						}
						return &faultingRealV2AppendFile{File: file, failAt: failAt}, nil
					}
				}
				result, err := committer.commitBatch(ctx, path, plan, &published)
				if err == nil || !reflect.DeepEqual(result, SaveResult{}) || published != persisted {
					t.Fatalf("%s batch failure result=%#v state=%#v err=%v", failAt, result, published, err)
				}
				afterFailure, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(afterFailure, anchorLine) {
					t.Fatalf("%s batch failure changed last committed bytes: equal=%v err=%v", failAt, bytes.Equal(afterFailure, anchorLine), err)
				}

				result, err = newV2BatchCommitter(codec).commitBatch(ctx, path, plan, &published)
				if err != nil || result.Persisted.Revision != 2 || published.Revision != 2 {
					t.Fatalf("%s batch retry skipped/failed revision: result=%#v state=%#v err=%v", failAt, result, published, err)
				}
				_, loaded := loadStrictV2State(t, path, previous.ID)
				if loaded != published || loaded.Revision != persisted.Revision+1 {
					t.Fatalf("%s batch retry reload = %#v, want %#v", failAt, loaded, published)
				}
			})
		}
	})

	t.Run("snapshot encode write sync and rename failures", func(t *testing.T) {
		for _, failAt := range []string{"encode", "write", "sync", "rename"} {
			t.Run(failAt, func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "conversation.jsonl")
				previous, persisted, anchorLine := seedV2AnchorFile(t, codec, path)
				current := cloneConversationV2(previous)
				current.Title = digestSafeText("retryable snapshot")
				current.UpdatedAt = current.UpdatedAt.Add(time.Second)
				plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
				if err != nil {
					t.Fatalf("classify snapshot failure fixture: %v", err)
				}
				published := persisted
				committer := faultingV2SnapshotCommitter(codec, failAt)
				if failAt == "encode" {
					committer = newV2SnapshotCommitter(mustV2LimitedTestCodec(t))
				}
				result, err := committer.commitSnapshot(ctx, path, int64(len(anchorLine)), plan, &published)
				if err == nil || !reflect.DeepEqual(result, SaveResult{}) || published != persisted {
					t.Fatalf("%s snapshot failure result=%#v state=%#v err=%v", failAt, result, published, err)
				}
				afterFailure, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(afterFailure, anchorLine) {
					t.Fatalf("%s snapshot failure changed old file: equal=%v err=%v", failAt, bytes.Equal(afterFailure, anchorLine), err)
				}
				assertOnlyV2TargetRemains(t, directory, filepath.Base(path))

				result, err = newV2SnapshotCommitter(codec).commitSnapshot(ctx, path, int64(len(anchorLine)), plan, &published)
				if err != nil || result.Persisted.Revision != 2 || published.Revision != 2 {
					t.Fatalf("%s snapshot retry skipped/failed revision: result=%#v state=%#v err=%v", failAt, result, published, err)
				}
				_, loaded := loadStrictV2State(t, path, previous.ID)
				if loaded != published || loaded.Revision != persisted.Revision+1 {
					t.Fatalf("%s snapshot retry reload = %#v, want %#v", failAt, loaded, published)
				}
			})
		}
	})
}

type faultingRealV2AppendFile struct {
	*os.File
	failAt    string
	syncCalls int
}

func (f *faultingRealV2AppendFile) Write(data []byte) (int, error) {
	if f.failAt != "write" {
		return f.File.Write(data)
	}
	count := len(data) / 2
	if count == 0 {
		count = 1
	}
	written, _ := f.File.Write(data[:count])
	return written, errors.New("injected real append write failure")
}

func (f *faultingRealV2AppendFile) Sync() error {
	f.syncCalls++
	if f.failAt == "sync" && f.syncCalls == 1 {
		return errors.New("injected real append sync failure")
	}
	return f.File.Sync()
}

func (f *faultingV2StageFile) Write(data []byte) (int, error) {
	if f.failAt != "write" {
		return f.v2StageFile.Write(data)
	}
	count := len(data) / 2
	if count == 0 {
		count = 1
	}
	written, _ := f.v2StageFile.Write(data[:count])
	return written, errors.New("injected snapshot write failure")
}

func mustV2TestCodec(t *testing.T) *V2RecordCodec {
	t.Helper()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func mustV2LimitedTestCodec(t *testing.T) *V2RecordCodec {
	t.Helper()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 1, MaxSessionBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func seedV2AnchorFile(t *testing.T, codec *V2RecordCodec, path string) (*Conversation, PersistedState, []byte) {
	t.Helper()
	previous := stateDigestFixture()
	persisted := mustComputePersistedState(t, previous, 1)
	anchor := validV2SnapshotRecord(t, 1)
	line, err := codec.Encode(previous.ID, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	return previous, persisted, line
}

const (
	v2CrashChildEnvironment   = "XAGENT_T38_CRASH_CHILD"
	v2CrashBarrierEnvironment = "XAGENT_T38_CRASH_BARRIER"
	v2CrashTargetEnvironment  = "XAGENT_T38_CRASH_TARGET"
	v2CrashMarkerEnvironment  = "XAGENT_T38_CRASH_MARKER"
)

func TestProcessInterruptionAtCommitBarriersPreservesLastRevision(t *testing.T) {
	if os.Getenv(v2CrashChildEnvironment) == "1" {
		runV2CrashBarrierChild(t)
		return
	}

	cases := []struct {
		barrier      string
		wantRevision uint64
	}{
		{barrier: "write-before-fsync", wantRevision: 1},
		{barrier: "fsync-before-publish", wantRevision: 2},
		{barrier: "staging-before-replace", wantRevision: 1},
		{barrier: "replace-before-state-publish", wantRevision: 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.barrier, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conversation.jsonl")
			marker := filepath.Join(directory, "barrier.ready")
			codec := mustV2TestCodec(t)
			previous, _, _ := seedV2AnchorFile(t, codec, path)

			command := exec.Command(os.Args[0], "-test.run=^TestProcessInterruptionAtCommitBarriersPreservesLastRevision$", "-test.count=1")
			command.Env = append(os.Environ(),
				v2CrashChildEnvironment+"=1",
				v2CrashBarrierEnvironment+"="+testCase.barrier,
				v2CrashTargetEnvironment+"="+path,
				v2CrashMarkerEnvironment+"="+marker,
			)
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			if err := command.Start(); err != nil {
				t.Fatalf("start crash child: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			waitForV2CrashMarker(t, marker, command, done)
			if err := command.Process.Kill(); err != nil {
				t.Fatalf("kill crash child: %v", err)
			}
			<-done

			conversation, persisted := loadStrictV2State(t, path, previous.ID)
			if persisted.Revision != testCase.wantRevision || conversation == nil {
				t.Fatalf("%s reload revision=%d conversation_nil=%v, want %d", testCase.barrier, persisted.Revision, conversation == nil, testCase.wantRevision)
			}
		})
	}
}

func waitForV2CrashMarker(t *testing.T, marker string, command *exec.Cmd, done <-chan error) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("crash child exited before barrier: %v", err)
		case <-ticker.C:
			if _, err := os.Stat(marker); err == nil {
				return
			}
		case <-deadline.C:
			_ = command.Process.Kill()
			t.Fatal("timed out waiting for crash barrier")
		}
	}
}

func runV2CrashBarrierChild(t *testing.T) {
	barrier := os.Getenv(v2CrashBarrierEnvironment)
	path := os.Getenv(v2CrashTargetEnvironment)
	marker := os.Getenv(v2CrashMarkerEnvironment)
	codec := mustV2TestCodec(t)
	previous := stateDigestFixture()
	persisted := mustComputePersistedState(t, previous, 1)

	appendCurrent := cloneConversationV2(previous)
	appendCurrent.Messages = append(appendCurrent.Messages, Message{Role: RoleAssistant, Content: digestSafeText("crash batch"), CreatedAt: appendCurrent.UpdatedAt.Add(time.Second)})
	appendCurrent.UpdatedAt = appendCurrent.UpdatedAt.Add(2 * time.Second)
	batchPlan, err := classifyV2Save(v2SaveClassificationInput{Conversation: appendCurrent, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
	if err != nil {
		t.Fatal(err)
	}
	batchLine, err := codec.Encode(previous.ID, *batchPlan.Record)
	if err != nil {
		t.Fatal(err)
	}

	snapshotCurrent := cloneConversationV2(previous)
	snapshotCurrent.Title = digestSafeText("crash snapshot")
	snapshotCurrent.UpdatedAt = snapshotCurrent.UpdatedAt.Add(time.Second)
	snapshotPlan, err := classifyV2Save(v2SaveClassificationInput{Conversation: snapshotCurrent, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
	if err != nil {
		t.Fatal(err)
	}
	snapshotLine, err := codec.Encode(previous.ID, *snapshotPlan.Record)
	if err != nil {
		t.Fatal(err)
	}
	anchorLine, err := codec.Encode(previous.ID, validV2SnapshotRecord(t, 1))
	if err != nil {
		t.Fatal(err)
	}

	switch barrier {
	case "write-before-fsync":
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(batchLine[:len(batchLine)/2]); err != nil {
			t.Fatal(err)
		}
	case "fsync-before-publish":
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFullV2Record(file, batchLine); err != nil || file.Sync() != nil {
			t.Fatal("batch fsync barrier failed")
		}
	case "staging-before-replace", "replace-before-state-publish":
		stage, err := os.CreateTemp(filepath.Dir(path), ".xagent-crash-stage-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := stage.Chmod(0o600); err != nil || writeFullV2Record(stage, anchorLine) != nil || writeFullV2Record(stage, snapshotLine) != nil || stage.Sync() != nil {
			t.Fatal("snapshot staging barrier failed")
		}
		if barrier == "replace-before-state-publish" {
			if err := stage.Close(); err != nil || replaceV2FileAtomic(stage.Name(), path) != nil {
				t.Fatal("snapshot replace barrier failed")
			}
		}
	default:
		t.Fatal("unknown crash barrier")
	}
	signalV2CrashBarrier(t, marker)
}

func signalV2CrashBarrier(t *testing.T, marker string) {
	t.Helper()
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("ready")); err != nil || file.Sync() != nil || file.Close() != nil {
		t.Fatal("publish crash barrier marker")
	}
	for {
		time.Sleep(time.Second)
	}
}

func loadStrictV2State(t *testing.T, path string, sessionID string) (*Conversation, PersistedState) {
	t.Helper()
	codec := mustV2TestCodec(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder, err := codec.NewDecoder(sessionID, file)
	if err != nil {
		t.Fatal(err)
	}
	var conversation *Conversation
	var persisted PersistedState
	for {
		record, err := decoder.Decode()
		if errors.Is(err, io.EOF) || (errors.Is(err, ErrV2RecordTruncated) && conversation != nil) {
			return conversation, persisted
		}
		if err != nil {
			t.Fatalf("strict v2 reload failed: %v", err)
		}
		context := RecordValidationContext{ExpectedSessionID: sessionID}
		if persisted.Revision > 0 {
			context.Previous = &persisted
		}
		if err := record.Validate(context); err != nil {
			t.Fatalf("strict v2 record validation failed: %v", err)
		}
		candidate := conversation
		switch record.Kind {
		case RecordSnapshot:
			candidate = cloneConversationV2(record.Snapshot)
		case RecordBatch:
			if candidate == nil || record.Batch.BaseMessageCount != len(candidate.Messages) {
				t.Fatal("strict v2 batch base mismatch")
			}
			candidate = cloneConversationV2(candidate)
			candidate.Messages = append(candidate.Messages, cloneV2MessageSlice(record.Batch.Messages)...)
			candidate.UpdatedAt = record.Batch.UpdatedAt
		default:
			t.Fatal("strict v2 reload unknown record kind")
		}
		computed := mustComputePersistedState(t, candidate, record.Revision)
		if computed.Digest != record.Digest {
			t.Fatal("strict v2 reload digest mismatch")
		}
		conversation = candidate
		persisted = computed
	}
}

func TestRecordAndSessionBudgetsUseC8Keys(t *testing.T) {
	const sessionID = "conversation-1"
	const bodyCanary = "record-body-secret-canary"
	largeBody := strings.Repeat("x", 70*1024) + bodyCanary
	record := validV2SnapshotRecord(t, 1)
	record.Snapshot.Messages[0].Content = digestSafeText(largeBody)
	record.Digest = mustComputePersistedState(t, record.Snapshot, record.Revision).Digest

	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 128 * 1024, MaxSessionBytes: 512 * 1024})
	if err != nil {
		t.Fatalf("create v2 record codec: %v", err)
	}
	line, err := codec.Encode(sessionID, record)
	if err != nil {
		t.Fatalf("encode legal record larger than 64 KiB: %v", err)
	}
	if len(line) <= 64*1024 || line[len(line)-1] != '\n' {
		t.Fatalf("encoded record framing/size = %d bytes", len(line))
	}

	t.Run("bounded round trip", func(t *testing.T) {
		decoder, err := codec.NewDecoder(sessionID, bytes.NewReader(line))
		if err != nil {
			t.Fatalf("create decoder: %v", err)
		}
		decoded, err := decoder.Decode()
		if err != nil {
			t.Fatalf("decode legal large record: %v", err)
		}
		if !reflect.DeepEqual(decoded, record) {
			t.Fatal("v2 record changed across bounded codec round trip")
		}
		if err := decoded.Validate(RecordValidationContext{ExpectedSessionID: sessionID}); err != nil {
			t.Fatalf("decoded record no longer validates: %v", err)
		}
		if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
			t.Fatalf("decoder terminal error = %v, want io.EOF", err)
		}
	})

	t.Run("encode enforces record key before publication", func(t *testing.T) {
		limited, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: 1, MaxSessionBytes: int64(len(line))})
		if err != nil {
			t.Fatalf("create record-limited codec: %v", err)
		}
		encoded, err := limited.Encode(sessionID, record)
		if encoded != nil {
			t.Fatalf("over-limit encode published %d bytes", len(encoded))
		}
		assertRecordBudgetError(t, err, sessionID, budget.SessionMaxRecordBytes, 1, bodyCanary, string(record.Digest))
	})

	t.Run("decode enforces record key before JSON", func(t *testing.T) {
		limit := int64(len(line) - 2)
		limited, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: limit, MaxSessionBytes: int64(len(line))})
		if err != nil {
			t.Fatalf("create record-limited decoder codec: %v", err)
		}
		decoder, err := limited.NewDecoder(sessionID, bytes.NewReader(line))
		if err != nil {
			t.Fatalf("create record-limited decoder: %v", err)
		}
		decoded, err := decoder.Decode()
		if !reflect.DeepEqual(decoded, JSONLRecordV2{}) {
			t.Fatalf("over-limit decode published a partial record: %#v", decoded)
		}
		assertRecordBudgetError(t, err, sessionID, budget.SessionMaxRecordBytes, limit, bodyCanary, string(record.Digest))
	})

	t.Run("reads accumulate session key", func(t *testing.T) {
		limit := int64(2*len(line) - 1)
		limited, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: int64(len(line)), MaxSessionBytes: limit})
		if err != nil {
			t.Fatalf("create session-limited codec: %v", err)
		}
		decoder, err := limited.NewDecoder(sessionID, bytes.NewReader(bytes.Repeat(line, 2)))
		if err != nil {
			t.Fatalf("create session-limited decoder: %v", err)
		}
		if _, err := decoder.Decode(); err != nil {
			t.Fatalf("first record should fit cumulative session budget: %v", err)
		}
		decoded, err := decoder.Decode()
		if !reflect.DeepEqual(decoded, JSONLRecordV2{}) {
			t.Fatalf("session-over-limit decode published a partial record: %#v", decoded)
		}
		assertRecordBudgetError(t, err, sessionID, budget.SessionMaxSessionBytes, limit, bodyCanary, string(record.Digest))
	})

	t.Run("malformed errors do not retain raw records", func(t *testing.T) {
		raw := []byte("{\"version\":2,\"unknown\":\"" + bodyCanary + "\"}\n")
		decoder, err := codec.NewDecoder(sessionID, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("create malformed decoder: %v", err)
		}
		if _, err := decoder.Decode(); !errors.Is(err, ErrV2RecordMalformed) || strings.Contains(err.Error(), bodyCanary) {
			t.Fatalf("malformed error was not fixed and safe: %v", err)
		}
	})
}

func assertRecordBudgetError(t *testing.T, err error, sessionID string, scope budget.Scope, limit int64, forbidden ...string) {
	t.Helper()
	var limitErr *RecordBudgetError
	if !errors.As(err, &limitErr) {
		t.Fatalf("budget error = %v (%T), want *RecordBudgetError", err, err)
	}
	if limitErr.SessionID != sessionID || limitErr.Scope != scope || limitErr.Limit != limit {
		t.Fatalf("budget error metadata = %#v, want session=%q scope=%q limit=%d", limitErr, sessionID, scope, limit)
	}
	typeOfError := reflect.TypeOf(*limitErr)
	if typeOfError.NumField() != 3 {
		t.Fatalf("RecordBudgetError contains %d fields, want only session ID, budget scope, and limit", typeOfError.NumField())
	}
	for _, canary := range forbidden {
		if canary != "" && strings.Contains(limitErr.Error(), canary) {
			t.Fatalf("budget error leaked record data %q: %q", canary, limitErr.Error())
		}
	}
}
func TestListReturnsPartialResultsPlaceholdersAndTopLevelError(t *testing.T) {
	t.Run("valid recovered and unavailable files remain visible in stable order", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		root := t.TempDir()
		sharedTime := time.Date(2026, time.August, 2, 18, 0, 0, 0, time.UTC)
		partialTime := sharedTime.Add(-time.Hour)

		files := map[string][]byte{}
		files["alpha.jsonl"] = t311WriteSnapshot(t, codec, root, "alpha", "alpha title", sharedTime)
		files["bravo.jsonl"] = t311WriteSnapshot(t, codec, root, "bravo", "bravo title", sharedTime)
		partial := t311SnapshotLine(t, codec, "partial", "partial title", partialTime)
		partial = append(partial, []byte("{\"body\":\"partial-body-secret-canary\"}\n")...)
		t311WriteFile(t, filepath.Join(root, "partial.jsonl"), partial)
		files["partial.jsonl"] = bytes.Clone(partial)

		files["empty.jsonl"] = []byte{}
		t311WriteFile(t, filepath.Join(root, "empty.jsonl"), files["empty.jsonl"])
		badDigest := strings.Repeat("9", sha256HexLength)
		files["first-bad.jsonl"] = []byte("{\"body\":\"first-body-secret-canary\",\"digest\":\"" + badDigest + "\"}\n")
		t311WriteFile(t, filepath.Join(root, "first-bad.jsonl"), files["first-bad.jsonl"])
		oldTime := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
		for _, name := range []string{"empty.jsonl", "first-bad.jsonl"} {
			if err := os.Chtimes(filepath.Join(root, name), oldTime, oldTime); err != nil {
				t.Fatalf("set placeholder time: %v", err)
			}
		}

		ignored := filepath.Join(root, "not-a-session.txt")
		t311WriteFile(t, ignored, []byte("unrelated-resource-secret-canary"))
		sentinel, err := os.Open(ignored)
		if err != nil {
			t.Fatalf("open unrelated sentinel: %v", err)
		}
		defer sentinel.Close()

		result, err := listV2Sessions(context.Background(), codec, root, v2ListOptions{})
		if err != nil {
			t.Fatalf("list mixed v2 files: %v", err)
		}
		if result.Truncated || len(result.Diagnostics) != 0 {
			t.Fatalf("unbounded complete list reported truncation: %#v", result)
		}
		wantBytes := int64(0)
		for _, data := range files {
			wantBytes += int64(len(data))
		}
		wantScannedFiles := len(files) + 1 // the unrelated direct child is still bounded enumeration work
		if result.ScannedFiles != wantScannedFiles || result.ScannedBytes != wantBytes {
			t.Fatalf("scan accounting files=%d bytes=%d, want files=%d bytes=%d", result.ScannedFiles, result.ScannedBytes, wantScannedFiles, wantBytes)
		}
		if len(result.Entries) != len(files) {
			t.Fatalf("list returned %d entries, want every one of %d candidate files", len(result.Entries), len(files))
		}

		byID := t311EntriesByID(t, result.Entries)
		for _, id := range []string{"alpha", "bravo"} {
			entry := byID[id]
			if !entry.Available || entry.Recovery.Status != RecoveryClean || entry.Recovery.LastValidRevision != 1 || entry.Recovery.SkippedRecords != 0 {
				t.Fatalf("clean entry %q = %#v", id, entry)
			}
			if entry.Summary.ID != id || entry.Summary.Title.Text() != id+" title" || !entry.Summary.UpdatedAt.Equal(sharedTime) || entry.Summary.MessageCount != len(stateDigestFixture().Messages) {
				t.Fatalf("clean summary %q = %#v", id, entry.Summary)
			}
		}
		partialEntry := byID["partial"]
		if !partialEntry.Available || partialEntry.Recovery.Status != RecoveryPartial || partialEntry.Recovery.LastValidRevision != 1 || partialEntry.Recovery.SkippedRecords != 1 {
			t.Fatalf("partial entry = %#v", partialEntry)
		}
		if partialEntry.Summary.Title.Text() != "partial title" || !partialEntry.Summary.UpdatedAt.Equal(partialTime) {
			t.Fatalf("partial summary did not use last verified state: %#v", partialEntry.Summary)
		}
		for _, id := range []string{"empty", "first-bad"} {
			entry := byID[id]
			if entry.Available || entry.Recovery.Status != RecoveryPlaceholder || entry.Summary.ID != id {
				t.Fatalf("placeholder entry %q = %#v", id, entry)
			}
		}
		if result.Entries[0].Summary.ID != "alpha" || result.Entries[1].Summary.ID != "bravo" {
			t.Fatalf("equal-time entries are not ordered by raw ID bytes: %q, %q", result.Entries[0].Summary.ID, result.Entries[1].Summary.ID)
		}
		t311AssertSorted(t, result.Entries)
		t311AssertSafeDiagnostics(t, result, root, "partial-body-secret-canary", "first-body-secret-canary", badDigest)

		for name, before := range files {
			after, readErr := os.ReadFile(filepath.Join(root, name))
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("list modified source %q: equal=%v err=%v", name, bytes.Equal(after, before), readErr)
			}
		}
		buffer := make([]byte, 1)
		if _, err := sentinel.Read(buffer); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("list closed unrelated resource: %v", err)
		}
	})

	t.Run("file and byte limits return trusted prefixes with bounded diagnostics", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		root := t.TempDir()
		sizes := map[string]int64{}
		for index, id := range []string{"a", "b", "c"} {
			data := t311WriteSnapshot(t, codec, root, id, id+" title", time.Date(2026, time.August, 2, 10+index, 0, 0, 0, time.UTC))
			sizes[id] = int64(len(data))
		}

		byFiles, err := listV2Sessions(context.Background(), codec, root, v2ListOptions{MaxScanFiles: 2, MaxScanBytes: 1 << 20})
		if err != nil {
			t.Fatalf("list with file limit: %v", err)
		}
		if !byFiles.Truncated || byFiles.ScannedFiles != 2 || byFiles.ScannedBytes != sizes["a"]+sizes["b"] || len(byFiles.Entries) != 2 {
			t.Fatalf("file-limited result = %#v", byFiles)
		}
		t311AssertTruncationDiagnostic(t, byFiles)

		byBytes, err := listV2Sessions(context.Background(), codec, root, v2ListOptions{MaxScanFiles: 100, MaxScanBytes: sizes["a"]})
		if err != nil {
			t.Fatalf("list with byte limit: %v", err)
		}
		if !byBytes.Truncated || byBytes.ScannedFiles != 2 || len(byBytes.Entries) != 1 || byBytes.ScannedBytes != sizes[byBytes.Entries[0].Summary.ID] {
			t.Fatalf("byte-limited result = %#v", byBytes)
		}
		t311AssertTruncationDiagnostic(t, byBytes)
		t311AssertSafeDiagnostics(t, byFiles, root)
		t311AssertSafeDiagnostics(t, byBytes, root)
	})

	t.Run("unrelated direct entries cannot bypass the file scan limit", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"one.tmp", "two.txt", "three.json"} {
			t311WriteFile(t, filepath.Join(root, name), []byte("unrelated-scan-budget-canary"))
		}

		result, err := listV2Sessions(context.Background(), mustV2TestCodec(t), root, v2ListOptions{MaxScanFiles: 2, MaxScanBytes: 1 << 20})
		if err != nil {
			t.Fatalf("list unrelated entries: %v", err)
		}
		if !result.Truncated || result.ScannedFiles != 2 || result.ScannedBytes != 0 || len(result.Entries) != 0 {
			t.Fatalf("unrelated-entry budget result = %#v", result)
		}
		t311AssertTruncationDiagnostic(t, result)
		t311AssertSafeDiagnostics(t, result, root, "unrelated-scan-budget-canary")
	})

	t.Run("root failures and cancellation are top level errors", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		missingRoot := filepath.Join(t.TempDir(), "missing-root-secret-canary")
		result, err := listV2Sessions(context.Background(), codec, missingRoot, v2ListOptions{})
		if !errors.Is(err, ErrV2ListDirectory) || !reflect.DeepEqual(result, ListResult{}) {
			t.Fatalf("missing root result=%#v err=%v", result, err)
		}

		notDirectory := filepath.Join(t.TempDir(), "not-directory-secret-canary")
		t311WriteFile(t, notDirectory, []byte("not a directory"))
		result, err = listV2Sessions(context.Background(), codec, notDirectory, v2ListOptions{})
		if !errors.Is(err, ErrV2ListDirectory) || !reflect.DeepEqual(result, ListResult{}) {
			t.Fatalf("untrustworthy enumeration result=%#v err=%v", result, err)
		}

		result, err = listV2Sessions(context.Background(), codec, t.TempDir(), v2ListOptions{MaxScanFiles: -1})
		if !errors.Is(err, ErrV2ListConfig) || !reflect.DeepEqual(result, ListResult{}) {
			t.Fatalf("invalid options result=%#v err=%v", result, err)
		}

		realRoot := t.TempDir()
		linkedRoot := filepath.Join(t.TempDir(), "linked-root-secret-canary")
		if linkErr := os.Symlink(realRoot, linkedRoot); linkErr == nil {
			result, err = listV2Sessions(context.Background(), codec, linkedRoot, v2ListOptions{})
			if !errors.Is(err, ErrV2ListDirectory) || !reflect.DeepEqual(result, ListResult{}) {
				t.Fatalf("linked root result=%#v err=%v", result, err)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err = listV2Sessions(ctx, codec, t.TempDir(), v2ListOptions{})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, ListResult{}) {
			t.Fatalf("canceled list result=%#v err=%v", result, err)
		}
	})

	t.Run("symbolic links and non regular candidates fail closed", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		root := t.TempDir()
		directoryCandidate := filepath.Join(root, "directory.jsonl")
		if err := os.Mkdir(directoryCandidate, 0o700); err != nil {
			t.Fatalf("create non-regular candidate: %v", err)
		}

		outside := filepath.Join(t.TempDir(), "outside.jsonl")
		outsideData := t311SnapshotLine(t, codec, "linked", "symlink-target-body-secret-canary", time.Now().UTC())
		t311WriteFile(t, outside, outsideData)
		symlinkPath := filepath.Join(root, "linked.jsonl")
		symlinkCreated := os.Symlink(outside, symlinkPath) == nil

		result, err := listV2Sessions(context.Background(), codec, root, v2ListOptions{})
		if err != nil {
			t.Fatalf("list unsupported candidates: %v", err)
		}
		byID := t311EntriesByID(t, result.Entries)
		if _, exists := byID["directory"]; exists {
			t.Fatalf("directory candidate escaped the root-level regular-file boundary: %#v", byID["directory"])
		}
		if symlinkCreated {
			entry := byID["linked"]
			if entry.Available || entry.Recovery.Status != RecoveryPlaceholder || !t311HasDiagnostic(entry.Recovery.Diagnostics, "conversation_v2_list_unsupported_entry") {
				t.Fatalf("symlink was followed or omitted: %#v", entry)
			}
		}
		if result.ScannedBytes != 0 {
			t.Fatalf("unsupported entries consumed regular-file bytes: %d", result.ScannedBytes)
		}
		after, readErr := os.ReadFile(outside)
		if readErr != nil || !bytes.Equal(after, outsideData) {
			t.Fatalf("listing symlink modified target: equal=%v err=%v", bytes.Equal(after, outsideData), readErr)
		}
		t311AssertSafeDiagnostics(t, result, root, outside, "symlink-target-body-secret-canary")
	})
}

func t311WriteSnapshot(t *testing.T, codec *V2RecordCodec, root string, id string, title string, updatedAt time.Time) []byte {
	t.Helper()
	data := t311SnapshotLine(t, codec, id, title, updatedAt)
	t311WriteFile(t, filepath.Join(root, id+".jsonl"), data)
	return data
}

func t311SnapshotLine(t *testing.T, codec *V2RecordCodec, id string, title string, updatedAt time.Time) []byte {
	t.Helper()
	snapshot := cloneConversationV2(stateDigestFixture())
	snapshot.ID = id
	snapshot.Title = digestSafeText(title)
	snapshot.UpdatedAt = updatedAt
	persisted := mustComputePersistedState(t, snapshot, 1)
	record := JSONLRecordV2{Version: JSONLVersionV2, Kind: RecordSnapshot, SessionID: id, Revision: 1, Digest: persisted.Digest, Snapshot: snapshot}
	line, err := codec.Encode(id, record)
	if err != nil {
		t.Fatalf("encode %q snapshot: %v", id, err)
	}
	return line
}

func TestSessionSizeCheckpointRemainsLoadable(t *testing.T) {
	t.Run("exact boundary keeps history and one byte overflow checkpoints", func(t *testing.T) {
		fixture := newT312AppendFixture(t)
		exactLimit := int64(len(fixture.anchorLine) + len(fixture.nextLine))
		if int64(len(fixture.checkpointLine)) > exactLimit-1 {
			t.Fatalf("fixture cannot isolate checkpoint boundary: checkpoint=%d append=%d", len(fixture.checkpointLine), exactLimit)
		}

		t.Run("exact boundary", func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, fixture.previous.ID+".jsonl")
			codec := t312Codec(t, fixture.maxRecordPayload(), exactLimit)
			anchorLine := t312Encode(t, codec, fixture.previous.ID, fixture.anchor)
			if err := os.WriteFile(path, anchorLine, 0o600); err != nil {
				t.Fatal(err)
			}

			published := fixture.persisted
			result, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(anchorLine)), fixture.current, fixture.plan, &published)
			if err != nil {
				t.Fatalf("commit exact-boundary append: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if result.Kind != SaveBatch || result != fixture.plan.Result || published != fixture.plan.Result.Persisted ||
				int64(len(data)) != exactLimit || bytes.Count(data, []byte{'\n'}) != 2 {
				t.Fatalf("exact-boundary result=%#v published=%#v bytes=%d lines=%d", result, published, len(data), bytes.Count(data, []byte{'\n'}))
			}
		})

		t.Run("one byte overflow", func(t *testing.T) {
			limit := exactLimit - 1
			directory := t.TempDir()
			path := filepath.Join(directory, fixture.previous.ID+".jsonl")
			codec := t312Codec(t, fixture.maxRecordPayload(), limit)
			anchorLine := t312Encode(t, codec, fixture.previous.ID, fixture.anchor)
			checkpointLine := t312Encode(t, codec, fixture.previous.ID, *fixture.checkpoint.Record)
			if err := os.WriteFile(path, anchorLine, 0o600); err != nil {
				t.Fatal(err)
			}

			published := fixture.persisted
			result, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(anchorLine)), fixture.current, fixture.plan, &published)
			if err != nil {
				t.Fatalf("commit checkpoint: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if result.Kind != SaveSnapshot || result.Persisted != fixture.plan.Result.Persisted || published != result.Persisted ||
				!bytes.Equal(data, checkpointLine) || bytes.Count(data, []byte{'\n'}) != 1 || int64(len(data)) > limit {
				t.Fatalf("checkpoint result=%#v published=%#v bytes=%d lines=%d", result, published, len(data), bytes.Count(data, []byte{'\n'}))
			}
			records := reloadV2RecordsWithCodec(t, t312Codec(t, fixture.maxRecordPayload(), limit), path, fixture.previous.ID)
			if len(records) != 1 || records[0].Kind != RecordSnapshot || records[0].Revision != fixture.plan.Record.Revision ||
				records[0].PreviousDigest != "" || records[0].Digest != fixture.plan.Record.Digest {
				t.Fatalf("checkpoint anchor = %#v", records)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := loadV2Records(context.Background(), t312Codec(t, fixture.maxRecordPayload(), limit), fixture.previous.ID, file)
			_ = file.Close()
			if err != nil || loaded.RecordCount != 1 || loaded.VerifiedBytes != int64(len(data)) ||
				loaded.Persisted != result.Persisted || !reflect.DeepEqual(loaded.Conversation, fixture.current) {
				t.Fatalf("fresh checkpoint load=%#v err=%v", loaded, err)
			}

			continued := cloneConversationV2(loaded.Conversation)
			continued.Messages = append(continued.Messages, Message{Role: RoleAssistant, Content: digestSafeText("after checkpoint"), CreatedAt: continued.UpdatedAt.Add(time.Second)})
			continued.UpdatedAt = continued.UpdatedAt.Add(2 * time.Second)
			next, err := classifyV2Save(v2SaveClassificationInput{Conversation: continued, Previous: loaded.Conversation, Persisted: loaded.Persisted, Recovery: RecoveryClean})
			if err != nil || next.Result.Kind != SaveBatch || next.Record.Revision != result.Persisted.Revision+1 || next.Record.PreviousDigest != result.Persisted.Digest {
				t.Fatalf("continued plan=%#v err=%v", next, err)
			}
			assertOnlyV2TargetRemains(t, directory, filepath.Base(path))
		})
	})

	t.Run("snapshot plans checkpoint while noop remains byte stable", func(t *testing.T) {
		wide := mustV2TestCodec(t)
		previous := stateDigestFixture()
		persisted := mustComputePersistedState(t, previous, 1)
		anchor := validV2SnapshotRecord(t, 1)
		anchorLine := t312Encode(t, wide, previous.ID, anchor)
		current := cloneConversationV2(previous)
		current.Title = digestSafeText("checkpointed metadata")
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
		plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
		if err != nil || plan.Result.Kind != SaveSnapshot {
			t.Fatalf("classify successor snapshot: plan=%#v err=%v", plan, err)
		}
		checkpoint, err := makeV2CheckpointPlan(current, plan)
		if err != nil {
			t.Fatal(err)
		}
		checkpointLine := t312Encode(t, wide, previous.ID, *checkpoint.Record)
		successorLine := t312Encode(t, wide, previous.ID, *plan.Record)
		limit := int64(len(successorLine))
		recordLimit := t312MaxInt64(int64(len(anchorLine)-1), int64(len(checkpointLine)-1), int64(len(successorLine)-1))
		codec := t312Codec(t, recordLimit, limit)
		directory := t.TempDir()
		path := filepath.Join(directory, previous.ID+".jsonl")
		if err := os.WriteFile(path, t312Encode(t, codec, previous.ID, anchor), 0o600); err != nil {
			t.Fatal(err)
		}
		published := persisted
		result, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(anchorLine)), current, plan, &published)
		if err != nil || result.Kind != SaveSnapshot || result.Persisted != plan.Result.Persisted {
			t.Fatalf("metadata checkpoint result=%#v err=%v", result, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
			t.Fatalf("metadata checkpoint bytes=%d err=%v", len(data), err)
		}
		records := reloadV2RecordsWithCodec(t, codec, path, previous.ID)
		if len(records) != 1 || records[0].PreviousDigest != "" || !reflect.DeepEqual(records[0].Snapshot, current) {
			t.Fatalf("metadata checkpoint record=%#v", records)
		}

		noop, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: current, Persisted: published, Recovery: RecoveryClean})
		if err != nil || noop.Result.Kind != SaveNoop {
			t.Fatalf("classify noop after checkpoint: %#v err=%v", noop, err)
		}
		before := append([]byte(nil), data...)
		noopResult, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(data)), current, noop, &published)
		after, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || noopResult != noop.Result || !bytes.Equal(before, after) || published != noop.Result.Persisted {
			t.Fatalf("noop result=%#v err=%v read=%v changed=%v", noopResult, err, readErr, !bytes.Equal(before, after))
		}
	})

	t.Run("checkpoint obeys both byte budgets without changing the old state", func(t *testing.T) {
		fixture := newT312AppendFixture(t)
		oldBytes := append([]byte(nil), fixture.anchorLine...)
		total := int64(len(fixture.anchorLine) + len(fixture.nextLine))

		t.Run("record budget", func(t *testing.T) {
			recordLimit := t312MaxInt64(int64(len(fixture.anchorLine)-1), int64(len(fixture.nextLine)-1))
			if int64(len(fixture.checkpointLine)-1) <= recordLimit {
				t.Fatalf("fixture checkpoint payload=%d must exceed record limit=%d", len(fixture.checkpointLine)-1, recordLimit)
			}
			limit := total - 1
			codec := t312Codec(t, recordLimit, limit)
			path := filepath.Join(t.TempDir(), fixture.previous.ID+".jsonl")
			if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			published := fixture.persisted
			result, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(oldBytes)), fixture.current, fixture.plan, &published)
			assertRecordBudgetError(t, err, fixture.previous.ID, budget.SessionMaxRecordBytes, recordLimit, "checkpoint-tail-canary", string(fixture.plan.Result.Persisted.Digest))
			assertT312SaveFailurePreserved(t, path, oldBytes, result, published, fixture.persisted)
		})

		t.Run("session budget includes newline", func(t *testing.T) {
			limit := int64(len(fixture.checkpointLine) - 1)
			if int64(len(fixture.anchorLine)) > limit || int64(len(fixture.nextLine)-1) > limit {
				t.Fatalf("fixture cannot isolate LF boundary: anchor=%d next=%d limit=%d", len(fixture.anchorLine), len(fixture.nextLine), limit)
			}
			codec := t312Codec(t, limit, limit)
			path := filepath.Join(t.TempDir(), fixture.previous.ID+".jsonl")
			if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			published := fixture.persisted
			result, err := newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(oldBytes)), fixture.current, fixture.plan, &published)
			assertRecordBudgetError(t, err, fixture.previous.ID, budget.SessionMaxSessionBytes, limit, "checkpoint-tail-canary", string(fixture.plan.Result.Persisted.Digest))
			assertT312SaveFailurePreserved(t, path, oldBytes, result, published, fixture.persisted)
		})
	})

	t.Run("atomic replacement failure and cancellation preserve the target", func(t *testing.T) {
		fixture := newT312AppendFixture(t)
		limit := int64(len(fixture.anchorLine) + len(fixture.nextLine) - 1)
		codec := t312Codec(t, fixture.maxRecordPayload(), limit)
		directory := t.TempDir()
		path := filepath.Join(directory, fixture.previous.ID+".jsonl")
		oldBytes := t312Encode(t, codec, fixture.previous.ID, fixture.anchor)
		if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		published := fixture.persisted
		committer := faultingV2SnapshotCommitter(codec, "replace")
		openedPrefix := false
		committer.openVerified = func(string) (io.ReadCloser, error) {
			openedPrefix = true
			return nil, errors.New("checkpoint must not copy history")
		}
		result, err := committer.commitCheckpoint(context.Background(), path, fixture.checkpoint, &published)
		if !errors.Is(err, ErrV2SnapshotCommit) || openedPrefix {
			t.Fatalf("replace failure result=%#v err=%v openedPrefix=%v", result, err, openedPrefix)
		}
		assertT312SaveFailurePreserved(t, path, oldBytes, result, published, fixture.persisted)
		assertOnlyV2TargetRemains(t, directory, filepath.Base(path))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err = newV2SaveCommitter(codec).commit(ctx, path, int64(len(oldBytes)), fixture.current, fixture.plan, &published)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancel result=%#v err=%v", result, err)
		}
		assertT312SaveFailurePreserved(t, path, oldBytes, result, published, fixture.persisted)

		result, err = newV2SaveCommitter(codec).commit(context.Background(), path, 0, fixture.current, fixture.plan, &published)
		if !errors.Is(err, ErrV2SaveCommit) {
			t.Fatalf("inconsistent verified prefix result=%#v err=%v", result, err)
		}
		assertT312SaveFailurePreserved(t, path, oldBytes, result, published, fixture.persisted)

		tamperedBytes := append([]byte(nil), oldBytes...)
		tamperedBytes[0] = '['
		if err := os.WriteFile(path, tamperedBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		result, err = newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(tamperedBytes)), fixture.current, fixture.plan, &published)
		if !errors.Is(err, ErrV2SaveCommit) {
			t.Fatalf("same-length tampered baseline result=%#v err=%v", result, err)
		}
		assertT312SaveFailurePreserved(t, path, tamperedBytes, result, published, fixture.persisted)
		if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		result, err = newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(oldBytes)), fixture.current, fixture.plan, &published)
		if !errors.Is(err, ErrV2SaveCommit) || result != (SaveResult{}) || published != fixture.persisted {
			t.Fatalf("deleted baseline result=%#v published=%#v err=%v", result, published, err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("batch save recreated a deleted baseline: %v", statErr)
		}
		if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
			t.Fatal(err)
		}

		result, err = newV2SaveCommitter(codec).commit(context.Background(), path, int64(len(oldBytes)), fixture.current, fixture.plan, &published)
		if err != nil || result.Kind != SaveSnapshot || result.Persisted != fixture.plan.Result.Persisted {
			t.Fatalf("checkpoint retry result=%#v err=%v", result, err)
		}
	})
}

type t312AppendFixture struct {
	previous       *Conversation
	current        *Conversation
	persisted      PersistedState
	anchor         JSONLRecordV2
	plan           v2SavePlan
	checkpoint     v2SavePlan
	anchorLine     []byte
	nextLine       []byte
	checkpointLine []byte
}

func newT312AppendFixture(t *testing.T) t312AppendFixture {
	t.Helper()
	codec := mustV2TestCodec(t)
	previous := stateDigestFixture()
	persisted := mustComputePersistedState(t, previous, 1)
	anchor := validV2SnapshotRecord(t, 1)
	current := cloneConversationV2(previous)
	current.Messages = append(current.Messages, Message{
		Role:      RoleAssistant,
		Content:   digestSafeText(strings.Repeat("checkpoint-tail-", 8) + "checkpoint-tail-canary"),
		CreatedAt: current.UpdatedAt.Add(time.Second),
	})
	current.UpdatedAt = current.UpdatedAt.Add(2 * time.Second)
	plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean})
	if err != nil || plan.Result.Kind != SaveBatch {
		t.Fatalf("classify T3.12 fixture: plan=%#v err=%v", plan, err)
	}
	checkpoint, err := makeV2CheckpointPlan(current, plan)
	if err != nil {
		t.Fatalf("make T3.12 checkpoint: %v", err)
	}
	return t312AppendFixture{
		previous:       previous,
		current:        current,
		persisted:      persisted,
		anchor:         anchor,
		plan:           plan,
		checkpoint:     checkpoint,
		anchorLine:     t312Encode(t, codec, previous.ID, anchor),
		nextLine:       t312Encode(t, codec, previous.ID, *plan.Record),
		checkpointLine: t312Encode(t, codec, previous.ID, *checkpoint.Record),
	}
}

func (f t312AppendFixture) maxRecordPayload() int64 {
	return t312MaxInt64(int64(len(f.anchorLine)-1), int64(len(f.nextLine)-1), int64(len(f.checkpointLine)-1))
}

func t312Codec(t *testing.T, recordLimit int64, sessionLimit int64) *V2RecordCodec {
	t.Helper()
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: recordLimit, MaxSessionBytes: sessionLimit})
	if err != nil {
		t.Fatalf("create T3.12 codec record=%d session=%d: %v", recordLimit, sessionLimit, err)
	}
	return codec
}

func t312Encode(t *testing.T, codec *V2RecordCodec, sessionID string, record JSONLRecordV2) []byte {
	t.Helper()
	line, err := codec.Encode(sessionID, record)
	if err != nil {
		t.Fatalf("encode T3.12 record revision=%d: %v", record.Revision, err)
	}
	return line
}

func t312MaxInt64(values ...int64) int64 {
	var maximum int64
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func assertT312SaveFailurePreserved(t *testing.T, path string, oldBytes []byte, result SaveResult, published PersistedState, want PersistedState) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, oldBytes) || result != (SaveResult{}) || published != want {
		t.Fatalf("failed checkpoint changed state: result=%#v published=%#v bytesEqual=%v readErr=%v", result, published, bytes.Equal(data, oldBytes), err)
	}
}

func TestMaintainHonorsC8RetentionAndScanLimits(t *testing.T) {
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)

	t.Run("default and explicit retention use verified logical time", func(t *testing.T) {
		cases := []struct {
			name          string
			retentionDays int64
			ages          []int
			wantDeleted   int
		}{
			{name: "resolved default thirty days", retentionDays: 30, ages: []int{31, 30, 29}, wantDeleted: 1},
			{name: "explicit seven days", retentionDays: 7, ages: []int{8, 7, 6}, wantDeleted: 1},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				codec := mustV2TestCodec(t)
				root := t.TempDir()
				var wantBytes int64
				paths := make([]string, len(testCase.ages))
				for index, age := range testCase.ages {
					id := fmt.Sprintf("retention-%d-%d", testCase.retentionDays, age)
					data := t311WriteSnapshot(t, codec, root, id, "retention title", now.AddDate(0, 0, -age))
					wantBytes += int64(len(data))
					paths[index] = filepath.Join(root, id+".jsonl")
				}
				// File mtimes deliberately contradict the logical timestamps.
				if err := os.Chtimes(paths[0], now, now); err != nil {
					t.Fatal(err)
				}
				veryOldMTime := now.AddDate(-2, 0, 0)
				if err := os.Chtimes(paths[len(paths)-1], veryOldMTime, veryOldMTime); err != nil {
					t.Fatal(err)
				}

				result, err := maintainV2Sessions(context.Background(), codec, root, v2MaintenanceOptions{
					RetentionDays: testCase.retentionDays,
					Now:           func() time.Time { return now },
				})
				if err != nil {
					t.Fatalf("maintain retention fixture: %v", err)
				}
				if result.Deleted != testCase.wantDeleted || result.Skipped != 0 || result.Truncated ||
					result.ScannedFiles != len(testCase.ages) || result.ScannedBytes != wantBytes || len(result.Diagnostics) != 0 {
					t.Fatalf("retention result = %#v", result)
				}
				if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
					t.Fatalf("strictly expired conversation was retained: %v", err)
				}
				for _, path := range paths[1:] {
					if _, err := os.Stat(path); err != nil {
						t.Fatalf("boundary/recent conversation was deleted: %v", err)
					}
				}
			})
		}

		t.Run("latest batch time keeps an old anchor active", func(t *testing.T) {
			codec := mustV2TestCodec(t)
			root := t.TempDir()
			path := filepath.Join(root, "active-chain.jsonl")
			previous := cloneConversationV2(stateDigestFixture())
			previous.ID = "active-chain"
			previous.UpdatedAt = now.AddDate(0, 0, -60)
			persisted := mustComputePersistedState(t, previous, 1)
			anchor := JSONLRecordV2{
				Version: JSONLVersionV2, Kind: RecordSnapshot, SessionID: previous.ID,
				Revision: 1, Digest: persisted.Digest, Snapshot: cloneConversationV2(previous),
			}
			anchorLine := t312Encode(t, codec, previous.ID, anchor)
			current := cloneConversationV2(previous)
			current.Messages = append(current.Messages, Message{
				Role: RoleAssistant, Content: digestSafeText("recent activity"), CreatedAt: now.AddDate(0, 0, -1),
			})
			current.UpdatedAt = now.AddDate(0, 0, -1)
			plan, err := classifyV2Save(v2SaveClassificationInput{
				Conversation: current, Previous: previous, Persisted: persisted, Recovery: RecoveryClean,
			})
			if err != nil || plan.Result.Kind != SaveBatch {
				t.Fatalf("classify active chain: plan=%#v err=%v", plan, err)
			}
			batchLine := t312Encode(t, codec, previous.ID, *plan.Record)
			t311WriteFile(t, path, append(anchorLine, batchLine...))

			result, err := maintainV2Sessions(context.Background(), codec, root, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
			if err != nil || result.Deleted != 0 || result.ScannedFiles != 1 || result.ScannedBytes != int64(len(anchorLine)+len(batchLine)) {
				t.Fatalf("active multi-revision result=%#v err=%v", result, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("active multi-revision conversation was deleted: %v", err)
			}
		})
	})

	t.Run("file and byte budgets bound streaming enumeration", func(t *testing.T) {
		t.Run("every direct child consumes the file budget", func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"one.txt", "two.json", "three.tmp"} {
				t311WriteFile(t, filepath.Join(root, name), []byte("unrelated-maintenance-canary"))
			}
			result, err := maintainV2Sessions(context.Background(), mustV2TestCodec(t), root, v2MaintenanceOptions{
				RetentionDays: 30,
				MaxScanFiles:  2,
				MaxScanBytes:  1 << 20,
				Now:           func() time.Time { return now },
			})
			if err != nil || !result.Truncated || result.ScannedFiles != 2 || result.ScannedBytes != 0 || result.Deleted != 0 || result.Skipped != 0 {
				t.Fatalf("file-budget result=%#v err=%v", result, err)
			}
			assertT313Diagnostic(t, result.Diagnostics, v2MaintenanceTruncatedCode)

			exactRoot := t.TempDir()
			for _, name := range []string{"one.txt", "two.json"} {
				t311WriteFile(t, filepath.Join(exactRoot, name), []byte("exact-file-budget"))
			}
			exact, err := maintainV2Sessions(context.Background(), mustV2TestCodec(t), exactRoot, v2MaintenanceOptions{
				RetentionDays: 30,
				MaxScanFiles:  2,
				MaxScanBytes:  1 << 20,
				Now:           func() time.Time { return now },
			})
			if err != nil || exact.Truncated || exact.ScannedFiles != 2 {
				t.Fatalf("exact file budget result=%#v err=%v", exact, err)
			}
		})

		t.Run("whole files are reserved before reading", func(t *testing.T) {
			codec := mustV2TestCodec(t)
			root := t.TempDir()
			var size int64
			for _, id := range []string{"byte-a", "byte-b", "byte-c"} {
				data := t311WriteSnapshot(t, codec, root, id, "title-x", now.AddDate(0, 0, -40))
				if size == 0 {
					size = int64(len(data))
				} else if int64(len(data)) != size {
					t.Fatalf("byte-budget fixtures differ: %d != %d", len(data), size)
				}
			}
			result, err := maintainV2Sessions(context.Background(), codec, root, v2MaintenanceOptions{
				RetentionDays: 30,
				MaxScanFiles:  100,
				MaxScanBytes:  size,
				Now:           func() time.Time { return now },
			})
			if err != nil || !result.Truncated || result.ScannedFiles != 2 || result.ScannedBytes != size || result.Deleted != 1 {
				t.Fatalf("exact byte-budget result=%#v err=%v", result, err)
			}

			tooSmallRoot := t.TempDir()
			data := t311WriteSnapshot(t, codec, tooSmallRoot, "byte-z", "title-z", now.AddDate(0, 0, -40))
			tooSmall, err := maintainV2Sessions(context.Background(), codec, tooSmallRoot, v2MaintenanceOptions{
				RetentionDays: 30,
				MaxScanFiles:  100,
				MaxScanBytes:  int64(len(data) - 1),
				Now:           func() time.Time { return now },
			})
			if err != nil || !tooSmall.Truncated || tooSmall.ScannedFiles != 1 || tooSmall.ScannedBytes != 0 || tooSmall.Deleted != 0 {
				t.Fatalf("sub-file byte-budget result=%#v err=%v", tooSmall, err)
			}
			if _, err := os.Stat(filepath.Join(tooSmallRoot, "byte-z.jsonl")); err != nil {
				t.Fatalf("unscanned file changed: %v", err)
			}
		})
	})

	t.Run("legacy damaged and linked entries are preserved while clean expiry continues", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		root := t.TempDir()
		legacyJSON := filepath.Join(root, "legacy.json")
		legacyJSONData := []byte(`{"id":"legacy","body":"legacy-json-secret-canary"}`)
		t311WriteFile(t, legacyJSON, legacyJSONData)

		legacyV1 := filepath.Join(root, "legacy-v1.jsonl")
		legacyV1Data := []byte("{\"version\":1,\"body\":\"legacy-v1-secret-canary\"}\n")
		t311WriteFile(t, legacyV1, legacyV1Data)

		partialPath := filepath.Join(root, "partial-old.jsonl")
		partialData := t311SnapshotLine(t, codec, "partial-old", "partial title", now.AddDate(0, 0, -60))
		partialData = append(partialData, []byte("{\"body\":\"partial-tail-secret-canary\"}\n")...)
		t311WriteFile(t, partialPath, partialData)

		expiredData := t311WriteSnapshot(t, codec, root, "expired-clean", "expired title", now.AddDate(0, 0, -60))
		directoryPath := filepath.Join(root, "directory.jsonl")
		if err := os.Mkdir(directoryPath, 0o700); err != nil {
			t.Fatal(err)
		}

		outside := filepath.Join(t.TempDir(), "outside.jsonl")
		outsideData := t311SnapshotLine(t, codec, "linked-old", "outside-secret-canary", now.AddDate(0, 0, -60))
		t311WriteFile(t, outside, outsideData)
		linkedPath := filepath.Join(root, "linked-old.jsonl")
		linked := os.Symlink(outside, linkedPath) == nil

		result, err := maintainV2Sessions(context.Background(), codec, root, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatalf("maintain mixed entries: %v", err)
		}
		wantFiles := 5
		wantSkipped := 2
		if linked {
			wantFiles++
			wantSkipped++
		}
		wantBytes := int64(len(legacyV1Data) + len(partialData) + len(expiredData))
		if result.Truncated || result.ScannedFiles != wantFiles || result.ScannedBytes != wantBytes || result.Deleted != 1 || result.Skipped != wantSkipped {
			t.Fatalf("mixed maintenance result = %#v", result)
		}
		for path, before := range map[string][]byte{
			legacyJSON:  legacyJSONData,
			legacyV1:    legacyV1Data,
			partialPath: partialData,
			outside:     outsideData,
		} {
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("preserved source changed: equal=%v err=%v", bytes.Equal(after, before), readErr)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "expired-clean.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("clean expired v2 file was not deleted: %v", err)
		}
		assertT313Diagnostic(t, result.Diagnostics, v2MaintenanceLegacyPreservedCode)
		if linked {
			assertT313Diagnostic(t, result.Diagnostics, v2MaintenanceUnsupportedEntryCode)
		}
		assertT313DiagnosticsSafe(t, result.Diagnostics, root, outside, "legacy-json-secret-canary", "legacy-v1-secret-canary", "partial-tail-secret-canary", "outside-secret-canary")
	})

	t.Run("configuration root and cancellation errors fail closed", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		missing := filepath.Join(t.TempDir(), "missing-maintenance-root")
		result, err := maintainV2Sessions(context.Background(), codec, missing, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
		if !errors.Is(err, ErrV2MaintenanceDirectory) || !reflect.DeepEqual(result, MaintenanceResult{}) {
			t.Fatalf("missing root result=%#v err=%v", result, err)
		}

		badOptions := []v2MaintenanceOptions{
			{RetentionDays: 0, Now: func() time.Time { return now }},
			{RetentionDays: -1, Now: func() time.Time { return now }},
			{RetentionDays: 3651, Now: func() time.Time { return now }},
			{RetentionDays: 30, MaxScanFiles: -1, Now: func() time.Time { return now }},
			{RetentionDays: 30, MaxScanFiles: 100001, Now: func() time.Time { return now }},
			{RetentionDays: 30, MaxScanBytes: (1 << 30) + 1, Now: func() time.Time { return now }},
			{RetentionDays: 30, Now: func() time.Time { return time.Time{} }},
		}
		for index, options := range badOptions {
			result, err = maintainV2Sessions(context.Background(), codec, t.TempDir(), options)
			if !errors.Is(err, ErrV2MaintenanceConfig) || !reflect.DeepEqual(result, MaintenanceResult{}) {
				t.Fatalf("bad options %d result=%#v err=%v", index, result, err)
			}
		}

		result, err = maintainV2Sessions(context.Background(), codec, "relative-root", v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
		if !errors.Is(err, ErrV2MaintenanceConfig) || !reflect.DeepEqual(result, MaintenanceResult{}) {
			t.Fatalf("relative root result=%#v err=%v", result, err)
		}

		realRoot := t.TempDir()
		linkedRoot := filepath.Join(t.TempDir(), "linked-maintenance-root")
		if os.Symlink(realRoot, linkedRoot) == nil {
			result, err = maintainV2Sessions(context.Background(), codec, linkedRoot, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
			if !errors.Is(err, ErrV2MaintenanceDirectory) || !reflect.DeepEqual(result, MaintenanceResult{}) {
				t.Fatalf("linked root result=%#v err=%v", result, err)
			}
		}

		cancelRoot := t.TempDir()
		before := t311WriteSnapshot(t, codec, cancelRoot, "cancel-old", "cancel title", now.AddDate(0, 0, -60))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err = maintainV2Sessions(ctx, codec, cancelRoot, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, MaintenanceResult{}) {
			t.Fatalf("canceled maintenance result=%#v err=%v", result, err)
		}
		after, readErr := os.ReadFile(filepath.Join(cancelRoot, "cancel-old.jsonl"))
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("pre-canceled maintenance changed file: equal=%v err=%v", bytes.Equal(after, before), readErr)
		}
	})

	t.Run("mid-scan cancellation returns accumulated deletion result", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		root := t.TempDir()
		expiredPath := filepath.Join(root, "cancel-after-delete.jsonl")
		expiredBytes := t311WriteSnapshot(t, codec, root, "cancel-after-delete", "expired title", now.AddDate(0, 0, -60))
		recentPath := filepath.Join(root, "recent-survivor.jsonl")
		recentBytes := t311WriteSnapshot(t, codec, root, "recent-survivor", "recent title", now.AddDate(0, 0, -1))
		ctx := &cancelAfterPathRemovedContext{
			Context: context.Background(),
			path:    expiredPath,
			done:    make(chan struct{}),
		}

		result, err := maintainV2Sessions(ctx, codec, root, v2MaintenanceOptions{RetentionDays: 30, Now: func() time.Time { return now }})
		if !errors.Is(err, context.Canceled) || result.Deleted != 1 || result.Skipped != 0 || result.Truncated ||
			result.ScannedFiles < 1 || result.ScannedFiles > 2 || result.ScannedBytes < int64(len(expiredBytes)) ||
			result.ScannedBytes > int64(len(expiredBytes)+len(recentBytes)) {
			t.Fatalf("mid-scan cancellation result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
			t.Fatalf("expired file was not deleted before cancellation: %v", err)
		}
		after, readErr := os.ReadFile(recentPath)
		if readErr != nil || !bytes.Equal(after, recentBytes) {
			t.Fatalf("cancellation changed recent file: equal=%v err=%v", bytes.Equal(after, recentBytes), readErr)
		}
	})

	t.Run("save and maintain share the cancellable mutation gate", func(t *testing.T) {
		codec := mustV2TestCodec(t)
		fixture := newT312AppendFixture(t)
		root := t.TempDir()
		savePath := filepath.Join(root, fixture.previous.ID+".jsonl")
		if err := os.WriteFile(savePath, fixture.anchorLine, 0o600); err != nil {
			t.Fatal(err)
		}
		syncEntered := make(chan struct{})
		releaseSync := make(chan struct{})
		defer func() {
			select {
			case <-releaseSync:
			default:
				close(releaseSync)
			}
		}()
		committer := newV2SaveCommitter(codec)
		committer.batch.openAppend = func(path string) (v2AppendFile, error) {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, err
			}
			return &blockingSyncV2AppendFile{File: file, syncEntered: syncEntered, releaseSync: releaseSync}, nil
		}
		type saveOutcome struct {
			result SaveResult
			err    error
		}
		published := fixture.persisted
		saveDone := make(chan saveOutcome, 1)
		go func() {
			result, err := committer.commit(context.Background(), savePath, int64(len(fixture.anchorLine)), fixture.current, fixture.plan, &published)
			saveDone <- saveOutcome{result: result, err: err}
		}()
		select {
		case <-syncEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("save did not reach the blocked sync barrier")
		}

		type maintainOutcome struct {
			result MaintenanceResult
			err    error
		}
		maintainCtx, cancelMaintain := context.WithCancel(context.Background())
		maintainDone := make(chan maintainOutcome, 1)
		go func() {
			result, err := maintainV2Sessions(maintainCtx, codec, root, v2MaintenanceOptions{
				RetentionDays: 30,
				Now:           func() time.Time { return now.AddDate(1, 0, 0) },
			})
			maintainDone <- maintainOutcome{result: result, err: err}
		}()
		select {
		case outcome := <-maintainDone:
			t.Fatalf("maintenance bypassed save mutation gate: result=%#v err=%v", outcome.result, outcome.err)
		case <-time.After(250 * time.Millisecond):
		}
		if _, err := os.Stat(savePath); err != nil {
			t.Fatalf("maintenance changed target while save held the gate: %v", err)
		}
		cancelMaintain()
		maintained := <-maintainDone
		if !errors.Is(maintained.err, context.Canceled) || !reflect.DeepEqual(maintained.result, MaintenanceResult{}) {
			t.Fatalf("canceled maintenance result=%#v err=%v", maintained.result, maintained.err)
		}

		close(releaseSync)
		saved := <-saveDone
		if saved.err != nil || saved.result != fixture.plan.Result || published != fixture.plan.Result.Persisted {
			t.Fatalf("save after maintenance cancellation result=%#v published=%#v err=%v", saved.result, published, saved.err)
		}
		records := reloadV2RecordsWithCodec(t, codec, savePath, fixture.previous.ID)
		if len(records) != 2 || records[1].Kind != RecordBatch || records[1].Revision != fixture.plan.Result.Persisted.Revision {
			t.Fatalf("saved chain after gate cancellation = %#v", records)
		}
	})
}

func assertT313Diagnostic(t *testing.T, diagnosticsList []diagnostics.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnosticsList {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing maintenance diagnostic %q in %#v", code, diagnosticsList)
}

func assertT313DiagnosticsSafe(t *testing.T, diagnosticsList []diagnostics.Diagnostic, forbidden ...string) {
	t.Helper()
	if len(diagnosticsList) > 7 {
		t.Fatalf("maintenance diagnostics are not code-bounded: %d", len(diagnosticsList))
	}
	encoded, err := json.Marshal(diagnosticsList)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range diagnosticsList {
		if diagnostic.Source != v2RecoverySource || diagnostic.Severity != diagnostics.SeverityWarning ||
			diagnostic.Message == "" || len(diagnostic.Message) > 256 || diagnostic.Path != "" || len(diagnostic.Attributes) != 0 {
			t.Fatalf("unsafe maintenance diagnostic: %#v", diagnostic)
		}
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("maintenance diagnostics leaked %q: %s", value, encoded)
		}
	}
}

func t311WriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func t311EntriesByID(t *testing.T, entries []ListEntry) map[string]ListEntry {
	t.Helper()
	result := make(map[string]ListEntry, len(entries))
	for _, entry := range entries {
		id := entry.Summary.ID
		if _, duplicate := result[id]; duplicate {
			t.Fatalf("duplicate list entry ID %q", id)
		}
		result[id] = entry
	}
	return result
}

func t311AssertSorted(t *testing.T, entries []ListEntry) {
	t.Helper()
	for index := 1; index < len(entries); index++ {
		previous, current := entries[index-1].Summary, entries[index].Summary
		if previous.UpdatedAt.Before(current.UpdatedAt) || (previous.UpdatedAt.Equal(current.UpdatedAt) && previous.ID > current.ID) {
			t.Fatalf("entries are not stably sorted at %d: %#v before %#v", index, previous, current)
		}
	}
}

func t311AssertTruncationDiagnostic(t *testing.T, result ListResult) {
	t.Helper()
	if len(result.Diagnostics) != 1 || !t311HasDiagnostic(result.Diagnostics, "conversation_v2_list_truncated") {
		t.Fatalf("truncated list diagnostics = %#v", result.Diagnostics)
	}
}

func t311HasDiagnostic(items []diagnostics.Diagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func t311AssertSafeDiagnostics(t *testing.T, result ListResult, forbidden ...string) {
	t.Helper()
	all := append([]diagnostics.Diagnostic(nil), result.Diagnostics...)
	for _, entry := range result.Entries {
		all = append(all, entry.Recovery.Diagnostics...)
	}
	for _, item := range all {
		if item.Message == "" || len(item.Message) > 256 || item.Path != "" || len(item.Attributes) != 0 {
			t.Fatalf("unbounded or unsafe list diagnostic: %#v", item)
		}
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("marshal list diagnostics: %v", err)
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("list diagnostics leaked %q: %s", value, encoded)
		}
	}
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
