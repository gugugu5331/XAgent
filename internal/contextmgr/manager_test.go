package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
)

func TestPrepareExternalizesLargeToolResult(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "call", "Bash", "success", "ok", strings.Repeat("x", 80), "", false, nil, nil)
	dataDir := t.TempDir()
	manager := New(&summaryProvider{summary: "unused"}, dataDir, testConfig())
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 1 || !result.Changed {
		t.Fatalf("expected one externalized result: %#v", result)
	}
	message := conv.Messages[0]
	if !message.Externalized || message.ExternalPath == "" || !strings.Contains(message.ToolResultContent, "artifact_id") {
		t.Fatalf("message not externalized: %#v", message)
	}
	info, err := os.Stat(message.ExternalPath)
	if err != nil {
		t.Fatalf("external file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("external file permissions = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(message.ExternalPath))
	if err != nil {
		t.Fatalf("external dir missing: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("external dir permissions = %v, want 0700", dirInfo.Mode().Perm())
	}
	if !strings.HasPrefix(message.ExternalPath, dataDir) {
		t.Fatalf("external path %q not under data dir %q", message.ExternalPath, dataDir)
	}
	contextMessages := conversation.ContextMessages(conv)
	if strings.Contains(contextMessages[0].ToolResultContent, message.ExternalPath) {
		t.Fatalf("context message leaked external path: %#v", contextMessages[0])
	}
	if !strings.Contains(contextMessages[0].ToolResultContent, "artifact_id") {
		t.Fatalf("context message missing artifact id hint: %#v", contextMessages[0])
	}
}

func TestPrepareExternalizesMultipleToolResultsByTotal(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "a", "Read", "success", "", strings.Repeat("a", 30), "", false, nil, nil)
	conversation.AppendToolResultMessage(conv, "b", "Read", "success", "", strings.Repeat("b", 30), "", false, nil, nil)
	cfg := testConfig()
	cfg.ToolResultThresholdChars = 100
	cfg.ToolResultsThresholdChars = 40
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), cfg)
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 1 {
		t.Fatalf("expected one externalized by total threshold: %#v", result)
	}
}

func TestPrepareIsIdempotentForExternalizedResults(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "call", "Bash", "success", "ok", strings.Repeat("x", 80), "", false, nil, nil)
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	if _, err := manager.Prepare(context.Background(), conv, ModeAuto); err != nil {
		t.Fatal(err)
	}
	firstPath := conv.Messages[0].ExternalPath
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 0 || conv.Messages[0].ExternalPath != firstPath {
		t.Fatalf("externalization not idempotent: %#v path=%s", result, conv.Messages[0].ExternalPath)
	}
}

func TestManualCompactSummarizesAndKeepsRecentMessages(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, "user")
		conversation.AppendAssistantMessage(conv, "assistant")
	}
	manager := New(&summaryProvider{summary: "## 当前目标\n继续工作"}, t.TempDir(), testConfig())
	result, err := manager.CompactNow(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summarized || conv.Messages[0].Role != conversation.RoleContextSummary || conv.Messages[1].Role != conversation.RoleContextBoundary {
		t.Fatalf("summary not applied: %#v messages=%#v", result, conv.Messages[:2])
	}
	if len(conv.Messages) < 7 {
		t.Fatalf("recent messages not preserved: %d", len(conv.Messages))
	}
}

func TestSummaryFailureCircuitBreaker(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, strings.Repeat("x", 20))
	}
	cfg := testConfig()
	cfg.SummaryFailureLimit = 3
	manager := New(&summaryProvider{err: errors.New("boom")}, t.TempDir(), cfg)
	for i := 0; i < 3; i++ {
		_, _ = manager.CompactNow(context.Background(), conv)
	}
	if conversation.EnsureContext(conv).SummaryFailureCount != 3 {
		t.Fatalf("expected three failures, got %#v", conversation.EnsureContext(conv))
	}
	_, err := manager.CompactNow(context.Background(), conv)
	if err == nil || !strings.Contains(err.Error(), "熔断") {
		t.Fatalf("expected circuit breaker error, got %v", err)
	}
}

func TestPrepareCompatibilityWrapper(t *testing.T) {
	now := time.Unix(100, 0)
	legacyConv := conversation.NewConversation("conv", now)
	optionsConv := conversation.NewConversation("conv", now)
	legacy := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	withOptions := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())

	legacyResult, legacyErr := legacy.Prepare(context.Background(), legacyConv, ModeAuto)
	optionsResult, optionsErr := withOptions.PrepareWithOptions(context.Background(), optionsConv, PrepareOptions{Mode: ModeAuto, PersistArtifacts: true})
	if legacyErr != nil || optionsErr != nil {
		t.Fatalf("compatibility prepare errors: legacy=%v options=%v", legacyErr, optionsErr)
	}
	if !reflect.DeepEqual(legacyResult, optionsResult) || !reflect.DeepEqual(legacyConv, optionsConv) {
		t.Fatalf("compatibility wrapper drifted:\nlegacy=%#v %#v\noptions=%#v %#v", legacyResult, legacyConv, optionsResult, optionsConv)
	}
}

type noHookCompactionObserver struct{}

func (noHookCompactionObserver) Before(context.Context, Attempt) any       { return nil }
func (noHookCompactionObserver) After(context.Context, any, Result, error) {}

func TestNoHookContextCompatibility(t *testing.T) {
	type snapshot struct {
		result       Result
		conversation string
		artifact     string
		summaryInput string
	}
	normalize := func(t *testing.T, conv *conversation.Conversation, result Result, artifactPath string, summaryInput string) snapshot {
		t.Helper()
		artifact := ""
		if artifactPath != "" {
			data, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatalf("read compatibility artifact: %v", err)
			}
			artifact = string(data)
		}
		conv.CreatedAt = time.Time{}
		conv.UpdatedAt = time.Time{}
		if conv.Context != nil {
			conv.Context.LastCompressionAt = nil
		}
		for index := range conv.Messages {
			conv.Messages[index].CreatedAt = time.Time{}
			if conv.Messages[index].ExternalPath != "" {
				conv.Messages[index].ExternalPath = "$ARTIFACT"
			}
		}
		data, err := json.Marshal(conv)
		if err != nil {
			t.Fatalf("marshal compatibility conversation: %v", err)
		}
		return snapshot{result: result, conversation: string(data), artifact: artifact, summaryInput: summaryInput}
	}

	observers := []struct {
		name     string
		observer CompactionObserver
	}{{name: "nil", observer: nil}, {name: "noop", observer: noHookCompactionObserver{}}}

	t.Run("auto externalization", func(t *testing.T) {
		var baseline *snapshot
		for _, item := range observers {
			conv := conversation.NewConversation("compat-auto", time.Unix(10, 0))
			conversation.AppendToolResultMessage(conv, "read-1", "Read", "success", "large", strings.Repeat("payload", 20), "", false, nil, nil)
			manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
			result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{
				Mode: ModeAuto, PersistArtifacts: true, Observer: item.observer,
			})
			if err != nil {
				t.Fatalf("%s auto prepare: %v", item.name, err)
			}
			artifactPath := ""
			if len(conv.Messages) > 0 {
				artifactPath = conv.Messages[0].ExternalPath
			}
			got := normalize(t, conv, result, artifactPath, "")
			if baseline == nil {
				copy := got
				baseline = &copy
			} else if !reflect.DeepEqual(*baseline, got) {
				t.Fatalf("%s observer changed auto compact:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
			}
		}
		if baseline == nil || !baseline.result.Changed || baseline.result.Externalized != 1 || baseline.result.Summarized || baseline.artifact == "" || !strings.Contains(baseline.conversation, "$ARTIFACT") {
			t.Fatalf("legacy auto compact golden changed: %#v", baseline)
		}
	})

	t.Run("manual summary", func(t *testing.T) {
		var baseline *snapshot
		for _, item := range observers {
			conv := conversation.NewConversation("compat-manual", time.Unix(20, 0))
			for index := 0; index < 8; index++ {
				conversation.AppendUserMessage(conv, "question")
				conversation.AppendAssistantMessage(conv, "answer")
			}
			for index := range conv.Messages {
				conv.Messages[index].CreatedAt = time.Unix(int64(100+index), 0)
			}
			conv.UpdatedAt = time.Unix(200, 0)
			providerImpl := &summaryProvider{summary: "## 当前目标\n保持兼容"}
			manager := New(providerImpl, t.TempDir(), testConfig())
			result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{
				Mode: ModeManual, PersistArtifacts: true, Observer: item.observer,
			})
			if err != nil {
				t.Fatalf("%s manual prepare: %v", item.name, err)
			}
			summaryInput := ""
			if len(providerImpl.lastReq.Messages) == 1 {
				summaryInput = providerImpl.lastReq.Messages[0].Content
			}
			got := normalize(t, conv, result, "", summaryInput)
			if baseline == nil {
				copy := got
				baseline = &copy
			} else if !reflect.DeepEqual(*baseline, got) {
				t.Fatalf("%s observer changed manual compact:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
			}
		}
		if baseline == nil || !baseline.result.Changed || !baseline.result.Summarized || baseline.result.Externalized != 0 ||
			!strings.Contains(baseline.summaryInput, "question") || !strings.Contains(baseline.conversation, "保持兼容") {
			t.Fatalf("legacy manual compact golden changed: %#v", baseline)
		}
	})
}

func TestCompactionPreflightDoesNotMutate(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Unix(100, 0))
	conversation.AppendToolResultMessage(conv, "call", "Read", "success", "", strings.Repeat("x", 80), "", false, nil, nil)
	before, err := json.Marshal(conv)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := len(conversation.ContextMessages(conv))
	wantEstimated := EstimateConversationTokens(conv)
	observer := &recordingCompactionObserver{before: func(attempt Attempt) {
		after, marshalErr := json.Marshal(conv)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if string(after) != string(before) {
			t.Fatalf("Conversation mutated before observer:\nbefore=%s\nafter=%s", before, after)
		}
		if attempt.Messages != wantMessages || attempt.EstimatedTokens != wantEstimated {
			t.Fatalf("unexpected immutable preflight stats: %#v", attempt)
		}
	}}
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	if _, err = manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeAuto, PersistArtifacts: true, Observer: observer}); err != nil {
		t.Fatal(err)
	}
	if observer.beforeCalls != 1 || observer.afterCalls != 1 {
		t.Fatalf("observer calls = before:%d after:%d", observer.beforeCalls, observer.afterCalls)
	}
}

func TestCompactionObserverBoundaries(t *testing.T) {
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendUserMessage(conv, "small")
	autoObserver := &recordingCompactionObserver{}
	if _, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeAuto, PersistArtifacts: true, Observer: autoObserver}); err != nil {
		t.Fatal(err)
	}
	if autoObserver.beforeCalls != 0 || autoObserver.afterCalls != 0 {
		t.Fatalf("non-attempting auto prepare emitted observer events: %#v", autoObserver)
	}

	manualObserver := &recordingCompactionObserver{}
	result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeManual, PersistArtifacts: true, Observer: manualObserver})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || manualObserver.beforeCalls != 1 || manualObserver.afterCalls != 1 {
		t.Fatalf("manual no-op was not observed exactly once: result=%#v observer=%#v", result, manualObserver)
	}
	if manualObserver.token != manualObserver.afterToken {
		t.Fatalf("observer token changed: before=%v after=%v", manualObserver.token, manualObserver.afterToken)
	}
	if manualObserver.afterResult.AfterMessages != 1 || manualObserver.afterResult.AfterEstimatedTokens <= 0 {
		t.Fatalf("observer did not receive post-attempt stats: %#v", manualObserver.afterResult)
	}
}

func TestCompactionAttemptError(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, strings.Repeat("x", 20))
	}
	cfg := testConfig()
	cfg.ModelWindowTokens = 1
	cfg.AutoMarginTokens = 0
	cfg.SummaryFailureLimit = 3
	observer := &recordingCompactionObserver{}
	manager := New(&summaryProvider{err: errors.New("summary canary")}, t.TempDir(), cfg)
	result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeAuto, PersistArtifacts: true, Observer: observer})
	if err != nil {
		t.Fatalf("auto compatibility path should swallow first summary error: %v", err)
	}
	if result.Summarized || observer.afterErr == nil || !strings.Contains(observer.afterErr.Error(), "summary canary") {
		t.Fatalf("attempt error was not reported to observer: result=%#v err=%v", result, observer.afterErr)
	}
}

func TestTransientPrepare(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, strings.Repeat("u", 20))
	}
	conversation.AppendToolResultMessage(conv, "large", "Read", "success", "", strings.Repeat("secret", 30), "", false, nil, nil)
	cfg := testConfig()
	cfg.ModelWindowTokens = 1
	cfg.AutoMarginTokens = 0
	dataDir := t.TempDir()
	observer := &recordingCompactionObserver{}
	manager := New(&summaryProvider{summary: "## 当前目标\ntransient"}, dataDir, cfg)
	result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeAuto, PersistArtifacts: false, Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summarized || result.Externalized != 0 || observer.beforeCalls != 1 || observer.afterCalls != 1 {
		t.Fatalf("transient summary result = %#v observer=%#v", result, observer)
	}
	if result.AfterMessages >= 9 || result.AfterEstimatedTokens <= 0 {
		t.Fatalf("transient post-summary stats are unavailable: result=%#v", result)
	}
	for _, message := range conv.Messages {
		if message.Externalized || message.ExternalPath != "" {
			t.Fatalf("transient prepare persisted a tool artifact: %#v", message)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "context_blobs")); !os.IsNotExist(err) {
		t.Fatalf("transient prepare created context_blobs: %v", err)
	}
}

func TestCompactionObserverFailureIsFailOpen(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendUserMessage(conv, "small")
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeManual, PersistArtifacts: true, Observer: panickingCompactionObserver{}})
	if err != nil || result.Changed {
		t.Fatalf("observer panic changed compaction result: result=%#v err=%v", result, err)
	}
}

func testConfig() config.ContextConfig {
	return config.ContextConfig{
		Enabled:                   boolPtr(true),
		ToolResultThresholdChars:  40,
		ToolResultsThresholdChars: 1000,
		ModelWindowTokens:         100000,
		AutoMarginTokens:          1000,
		ManualMarginTokens:        100,
		RecentKeepTokens:          10,
		RecentKeepMessages:        5,
		SummaryFailureLimit:       3,
		PreviewChars:              16,
	}
}

func boolPtr(value bool) *bool {
	return &value
}

type summaryProvider struct {
	summary string
	err     error
	lastReq provider.ChatRequest
}

type recordingCompactionObserver struct {
	before      func(Attempt)
	beforeCalls int
	afterCalls  int
	token       any
	afterToken  any
	afterResult Result
	afterErr    error
}

func (o *recordingCompactionObserver) Before(_ context.Context, attempt Attempt) any {
	o.beforeCalls++
	if o.before != nil {
		o.before(attempt)
	}
	o.token = &struct{}{}
	return o.token
}

func (o *recordingCompactionObserver) After(_ context.Context, token any, result Result, err error) {
	o.afterCalls++
	o.afterToken = token
	o.afterResult = result
	o.afterErr = err
}

type panickingCompactionObserver struct{}

func (panickingCompactionObserver) Before(context.Context, Attempt) any {
	panic("before")
}

func (panickingCompactionObserver) After(context.Context, any, Result, error) {
	panic("after")
}

func (p *summaryProvider) Name() string { return "summary" }

func (p *summaryProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.lastReq = req
	out := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(out)
		if p.err != nil {
			out <- provider.StreamEvent{Type: provider.StreamEventError, Err: p.err}
			return
		}
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: p.summary}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}()
	return out, nil
}
