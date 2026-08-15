package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestContextManagerConstructorIsClosed(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	cfg := testConfig()
	manager, err := New(&summaryProvider{}, ManagerOptions{Context: cfg, InlineOutputBytes: 8, RuntimeRedactor: redactor})
	if err != nil || manager == nil {
		t.Fatal("valid context manager was rejected")
	}
	managerType := reflect.TypeOf(*manager)
	for index := 0; index < managerType.NumField(); index++ {
		name := strings.ToLower(managerType.Field(index).Name)
		if strings.Contains(name, "path") || strings.Contains(name, "root") || strings.Contains(name, "datadir") {
			t.Fatal("context manager retained a path capability")
		}
	}
	minimum := cfg
	minimum.ToolResultThresholdChars = 1
	minimum.ToolResultsThresholdChars = 1
	minimum.ModelWindowTokens = 2
	minimum.AutoMarginTokens = 1
	minimum.ManualMarginTokens = 1
	minimum.RecentKeepTokens = 1
	minimum.RecentKeepMessages = 1
	minimum.SummaryFailureLimit = 1
	minimum.PreviewChars = 1
	maximum := cfg
	maximum.ToolResultThresholdChars = 1 << 20
	maximum.ToolResultsThresholdChars = 4 << 20
	maximum.ModelWindowTokens = 10_000_000
	maximum.AutoMarginTokens = 1_000_000
	maximum.ManualMarginTokens = 1_000_000
	maximum.RecentKeepTokens = 1_000_000
	maximum.RecentKeepMessages = 10_000
	maximum.SummaryFailureLimit = 100
	maximum.PreviewChars = 1 << 20
	for _, boundary := range []struct {
		context config.ContextConfig
		inline  int64
	}{{context: minimum, inline: 1}, {context: maximum, inline: 1 << 20}} {
		candidate, candidateErr := New(&summaryProvider{}, ManagerOptions{Context: boundary.context, InlineOutputBytes: boundary.inline, RuntimeRedactor: redactor})
		if candidateErr != nil || candidate == nil {
			t.Fatal("valid context manager boundary was rejected")
		}
	}

	invalid := []ManagerOptions{
		{Context: cfg, InlineOutputBytes: 8},
		{Context: cfg, InlineOutputBytes: 0, RuntimeRedactor: redactor},
		{Context: cfg, InlineOutputBytes: 1<<20 + 1, RuntimeRedactor: redactor},
	}
	invalidContexts := make([]config.ContextConfig, 0, 22)
	appendInvalid := func(update func(*config.ContextConfig)) {
		candidate := cfg
		update(&candidate)
		invalidContexts = append(invalidContexts, candidate)
	}
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ToolResultThresholdChars = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ToolResultThresholdChars = 1<<20 + 1 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ToolResultsThresholdChars = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ToolResultsThresholdChars = 4<<20 + 1 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ModelWindowTokens = 1 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ModelWindowTokens = 10_000_001 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.AutoMarginTokens = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.AutoMarginTokens = 1_000_001 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ManualMarginTokens = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ManualMarginTokens = 1_000_001 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.RecentKeepTokens = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.RecentKeepTokens = 1_000_001 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.RecentKeepMessages = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.RecentKeepMessages = 10_001 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.SummaryFailureLimit = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.SummaryFailureLimit = 101 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.PreviewChars = 0 })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.PreviewChars = 1<<20 + 1 })
	appendInvalid(func(candidate *config.ContextConfig) {
		candidate.ToolResultThresholdChars = candidate.ToolResultsThresholdChars + 1
	})
	appendInvalid(func(candidate *config.ContextConfig) { candidate.AutoMarginTokens = candidate.ModelWindowTokens })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.ManualMarginTokens = candidate.ModelWindowTokens })
	appendInvalid(func(candidate *config.ContextConfig) { candidate.RecentKeepTokens = candidate.ModelWindowTokens })
	for _, invalidContext := range invalidContexts {
		invalid = append(invalid, ManagerOptions{Context: invalidContext, InlineOutputBytes: 8, RuntimeRedactor: redactor})
	}
	for _, options := range invalid {
		if candidate, candidateErr := New(&summaryProvider{}, options); candidateErr == nil || candidate != nil {
			t.Fatal("invalid context manager options were accepted")
		}
	}
	if candidate, candidateErr := New(nil, ManagerOptions{Context: cfg, InlineOutputBytes: 8, RuntimeRedactor: redactor}); candidateErr == nil || candidate != nil {
		t.Fatal("enabled context manager accepted a nil provider")
	}
	disabled := false
	cfg.Enabled = &disabled
	if candidate, candidateErr := New(nil, ManagerOptions{Context: cfg, InlineOutputBytes: 8, RuntimeRedactor: redactor}); candidateErr != nil || candidate == nil {
		t.Fatal("disabled context manager rejected a nil provider")
	}
}

func TestContextManagerProjectionMatrix(t *testing.T) {
	const validID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	now := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	manager := newTestManager(t, &summaryProvider{}, testConfig(), redactor, 8)
	type projectionCase struct {
		name   string
		input  tool.ResultFactoryInput
		mutate func(tool.Result) tool.Result
		wantOK bool
	}
	inline := func(preview string, captured int64) tool.ResultFactoryInput {
		return tool.ResultFactoryInput{CallID: "call", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess, Summary: "ok", Preview: preview, CapturedBytes: captured}
	}
	withRef := func(complete bool, reason string) tool.ResultFactoryInput {
		return tool.ResultFactoryInput{CallID: "call", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess, Summary: "ok", Preview: "12345678", Artifact: &artifact.Ref{ID: validID, Bytes: 9, CreatedAt: now, Available: true, Complete: complete}, CapturedBytes: 9, Truncated: true, TruncationReason: reason}
	}
	mutateViews := func(change func(*tool.UserView, *tool.OutputMeta)) func(tool.Result) tool.Result {
		return func(result tool.Result) tool.Result {
			userView := result.UserView()
			outputMeta := result.OutputMeta()
			change(&userView, &outputMeta)
			result = setPrivateResultField(t, result, "userView", userView)
			return setPrivateResultField(t, result, "outputMeta", outputMeta)
		}
	}
	cases := []projectionCase{
		{name: "no_ref_ascii_below_cap", input: inline("1234567", 7), wantOK: true},
		{name: "no_ref_ascii_at_cap", input: inline("12345678", 8), wantOK: true},
		{name: "no_ref_ascii_over_cap_rejected", input: inline("123456789", 9)},
		{name: "no_ref_utf8_at_cap", input: inline("你a🙂", 8), wantOK: true},
		{name: "no_ref_utf8_over_cap_rejected", input: inline("你ab🙂", 9)},
		{name: "no_ref_truncated_rejected", input: inline("123", 3), mutate: mutateViews(func(user *tool.UserView, meta *tool.OutputMeta) {
			user.Truncated = true
			meta.Truncated = true
			user.TruncationReason = redactor.Redact("capture_write_failure")
			meta.TruncationReason = redactor.Redact("capture_write_failure")
		})},
		{name: "ref_complete_inline_limit", input: withRef(true, "inline_preview_limit"), wantOK: true},
		{name: "ref_incomplete_capture_hard_limit", input: withRef(false, "capture_hard_limit"), wantOK: true},
		{name: "ref_incomplete_artifact_hard_limit", input: withRef(false, "artifact_hard_limit"), wantOK: true},
		{name: "ref_incomplete_write_failure", input: withRef(false, "capture_write_failure"), wantOK: true},
		{name: "ref_incomplete_canceled", input: withRef(false, "capture_canceled"), wantOK: true},
		{name: "ref_id_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Artifact.ID = strings.Repeat("a", 64) })},
		{name: "ref_bytes_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Artifact.Bytes++ })},
		{name: "ref_created_at_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) {
			user.Artifact.CreatedAt = user.Artifact.CreatedAt.Add(time.Second)
		})},
		{name: "ref_available_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Artifact.Available = false })},
		{name: "ref_complete_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Artifact.Complete = false })},
		{name: "truncated_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Truncated = false })},
		{name: "reason_mismatch_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) {
			user.TruncationReason = redactor.Redact("capture_hard_limit")
		})},
		{name: "preview_over_cap_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.Preview = redactor.Redact("123456789") })},
		{name: "unavailable_ref_rejected", input: withRef(true, "inline_preview_limit"), mutate: mutateViews(func(user *tool.UserView, meta *tool.OutputMeta) {
			user.Artifact.Available = false
			meta.Artifact.Available = false
		})},
		{name: "cancelled_before_start_rejected", input: inline("ok", 2), mutate: mutateViews(func(user *tool.UserView, _ *tool.OutputMeta) { user.State = tool.CancelledBeforeStart })},
		{name: "zero_byte_write_failure_synthetic_error", input: tool.ResultFactoryInput{CallID: "call", Name: "Read", State: tool.Completed, Status: tool.StatusError, Summary: "failed", CapturedBytes: 0, Error: &tool.Error{Code: tool.ErrInternalRoutingRequired, Message: "failed"}}, wantOK: true},
		{name: "commit_failure_synthetic_error", input: tool.ResultFactoryInput{CallID: "call", Name: "Read", State: tool.Completed, Status: tool.StatusError, Summary: "failed", CapturedBytes: 0, Error: &tool.Error{Code: tool.ErrInternalRoutingRequired, Message: "failed"}}, wantOK: true},
		{name: "legacy_threshold_extremes_equal", input: inline("12345678", 8), wantOK: true},
	}
	expected := make(map[string]bool, len(cases))
	for _, name := range strings.Fields(`
		no_ref_ascii_below_cap no_ref_ascii_at_cap no_ref_ascii_over_cap_rejected
		no_ref_utf8_at_cap no_ref_utf8_over_cap_rejected no_ref_truncated_rejected
		ref_complete_inline_limit ref_incomplete_capture_hard_limit ref_incomplete_artifact_hard_limit
		ref_incomplete_write_failure ref_incomplete_canceled ref_id_mismatch_rejected
		ref_bytes_mismatch_rejected ref_created_at_mismatch_rejected ref_available_mismatch_rejected
		ref_complete_mismatch_rejected truncated_mismatch_rejected reason_mismatch_rejected
		preview_over_cap_rejected unavailable_ref_rejected cancelled_before_start_rejected
		zero_byte_write_failure_synthetic_error commit_failure_synthetic_error legacy_threshold_extremes_equal
	`) {
		expected[name] = true
	}
	seen := make(map[string]bool, len(cases))
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			seen[item.name] = true
			result := buildToolResult(t, redactor, item.input)
			if item.mutate != nil {
				result = item.mutate(result)
			}
			projection, err := manager.ProjectToolResult(result)
			if item.name == "ref_bytes_mismatch_rejected" {
				capturedMismatch := buildToolResult(t, redactor, withRef(true, "inline_preview_limit"))
				outputMeta := capturedMismatch.OutputMeta()
				outputMeta.CapturedBytes++
				capturedMismatch = setPrivateResultField(t, capturedMismatch, "outputMeta", outputMeta)
				failed, mismatchErr := manager.ProjectToolResult(capturedMismatch)
				if mismatchErr == nil || !reflect.DeepEqual(failed, ToolResultProjection{}) {
					t.Fatal("captured byte mismatch published a projection")
				}
			}
			if item.name == "legacy_threshold_extremes_equal" {
				low := testConfig()
				low.ToolResultThresholdChars, low.ToolResultsThresholdChars, low.PreviewChars = 1, 1, 1
				high := testConfig()
				high.ToolResultThresholdChars, high.ToolResultsThresholdChars, high.PreviewChars = 1<<20, 1<<20, 1<<20
				lowProvider := &summaryProvider{summary: "## 当前目标\nthreshold invariant"}
				highProvider := &summaryProvider{summary: "## 当前目标\nthreshold invariant"}
				lowManager := newTestManager(t, lowProvider, low, redactor, 8)
				highManager := newTestManager(t, highProvider, high, redactor, 8)
				lowProjection, lowErr := lowManager.ProjectToolResult(result)
				highProjection, highErr := highManager.ProjectToolResult(result)
				if lowErr != nil || highErr != nil || !reflect.DeepEqual(lowProjection, highProjection) {
					t.Fatal("legacy context thresholds changed projection")
				}
				lowConversation := conversation.NewConversation("low", now)
				highConversation := conversation.NewConversation("high", now)
				for index := 0; index < 8; index++ {
					conversation.AppendUserMessage(lowConversation, "question")
					conversation.AppendAssistantMessage(lowConversation, "answer")
					conversation.AppendUserMessage(highConversation, "question")
					conversation.AppendAssistantMessage(highConversation, "answer")
				}
				lowResult, lowSummaryErr := lowManager.CompactNow(context.Background(), lowConversation)
				highResult, highSummaryErr := highManager.CompactNow(context.Background(), highConversation)
				if lowSummaryErr != nil || highSummaryErr != nil || !reflect.DeepEqual(lowResult, highResult) ||
					conversation.EnsureContext(lowConversation).Summary.Text() != conversation.EnsureContext(highConversation).Summary.Text() ||
					len(lowProvider.lastReq.Messages) != 1 || len(highProvider.lastReq.Messages) != 1 ||
					lowProvider.lastReq.Messages[0].Content.Text() != highProvider.lastReq.Messages[0].Content.Text() {
					t.Fatal("legacy context thresholds changed context summary behavior")
				}
			}
			if item.wantOK && err != nil {
				t.Fatal("valid projection was rejected")
			}
			if !item.wantOK && err == nil {
				t.Fatal("invalid projection was accepted")
			}
			if err != nil && !reflect.DeepEqual(projection, ToolResultProjection{}) {
				t.Fatal("failed projection published partial state")
			}
		})
	}
	if !reflect.DeepEqual(seen, expected) {
		t.Fatal("projection case set is incomplete")
	}
}

func TestContextManagerUsageSnapshotValidation(t *testing.T) {
	manager := newTestManager(t, &summaryProvider{}, testConfig(), redact.NewRuntimeRedactor(), 8)
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendUserMessage(conv, "hello")
	usage := provider.Usage{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40}
	if err := manager.UpdateUsage(conv, usage); err != nil {
		t.Fatal("valid usage snapshot was rejected")
	}
	if conv.Context == nil || conv.Context.LastInputTokens != 10 || conv.Context.LastOutputTokens != 20 || conv.Context.LastEstimatedTokens != 10 || conv.Context.LastEstimatedCharacters == 0 {
		t.Fatal("valid usage snapshot was not committed atomically")
	}
	baseline := *conv.Context
	invalid := []provider.Usage{
		{InputTokens: -1}, {OutputTokens: -1}, {CacheCreationInputTokens: -1}, {CacheReadInputTokens: -1},
		{InputTokens: math.MaxInt64, OutputTokens: 1},
	}
	for _, candidate := range invalid {
		if err := manager.UpdateUsage(conv, candidate); err == nil || !reflect.DeepEqual(baseline, *conv.Context) {
			t.Fatal("invalid usage snapshot changed conversation state")
		}
	}
	if err := manager.UpdateUsage(nil, usage); err == nil {
		t.Fatal("nil conversation usage update was accepted")
	}
	var nilManager *Manager
	if err := nilManager.UpdateUsage(conv, usage); err == nil {
		t.Fatal("nil manager usage update was accepted")
	}
}

func TestContextManagerNeverReadsRawArtifact(t *testing.T) {
	const rawCanary = "RAW_CONTEXT_CANARY_4f8d0b"
	const pathCanary = "/private/context/path/canary"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(rawCanary)
	redactor.RegisterSecret(pathCanary)
	poison := &poisonArtifactStore{raw: rawCanary, path: pathCanary}
	ref := poison.reference()
	manager := newTestManager(t, &summaryProvider{summary: "## 当前目标\nsafe"}, testConfig(), redactor, 16)
	result := buildToolResult(t, redactor, tool.ResultFactoryInput{CallID: "call", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess, Summary: rawCanary, Preview: pathCanary, Artifact: &ref, CapturedBytes: ref.Bytes, Truncated: true, TruncationReason: "inline_preview_limit"})
	projection, err := manager.ProjectToolResult(result)
	if err != nil {
		t.Fatalf("safe projection failed: %v", err)
	}
	assertProjectionHasNoCanary(t, projection, rawCanary, pathCanary)

	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	storeOptions := conversation.JSONLStoreOptions{DataDir: dataDir, Redactor: redactor, MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024, MaxScanFiles: 100, MaxScanBytes: 4 * 1024 * 1024, RetentionDays: 30, GapReminderDays: 7, Now: func() time.Time { return now }}
	store, err := conversation.NewJSONLStore(storeOptions)
	if err != nil {
		t.Fatal("conversation store setup failed")
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal("conversation creation failed")
	}
	conversation.AppendToolResultMessage(conv, "call", "Read", "success", projection.UserView.Summary.Text(), projection.PersistedContent.Text(), "", projection.OutputMeta.Truncated, nil, nil)
	message := &conv.Messages[len(conv.Messages)-1]
	message.Tool.Artifact = projection.OutputMeta.Artifact
	message.Tool.TruncationReason = projection.OutputMeta.TruncationReason
	for index := 0; index < 8; index++ {
		conversation.AppendUserMessage(conv, "safe question")
		conversation.AppendAssistantMessage(conv, "safe answer")
	}
	if _, err := manager.CompactNow(context.Background(), conv); err != nil {
		t.Fatal("context summary failed")
	}
	if _, err := store.Save(context.Background(), conv); err != nil {
		t.Fatal("conversation save failed")
	}
	restarted, err := conversation.NewJSONLStore(storeOptions)
	if err != nil {
		t.Fatal("conversation store restart failed")
	}
	loaded, err := restarted.Load(context.Background(), conv.ID)
	if err != nil || !loaded.Available || loaded.Conversation == nil {
		t.Fatal("conversation reload failed")
	}
	encoded, err := json.Marshal(loaded.Conversation)
	if err != nil {
		t.Fatal("conversation inspection failed")
	}
	if containsAnyCanary(string(encoded), rawCanary, pathCanary) || poison.openCount != 0 {
		t.Fatal("raw artifact crossed the context boundary")
	}
	files, err := filepath.Glob(filepath.Join(dataDir, "v2", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatal("conversation persistence inspection failed")
	}
	disk, err := os.ReadFile(files[0])
	if err != nil || containsAnyCanary(string(disk), rawCanary, pathCanary) {
		t.Fatal("raw artifact crossed the persistence boundary")
	}
	requests := manager.provider.(*summaryProvider).lastReq.Messages
	if len(requests) != 1 {
		t.Fatal("provider summary request was not captured")
	}
	if containsAnyCanary(requests[0].Content.Text(), rawCanary, pathCanary) {
		t.Fatal("raw artifact crossed the provider boundary")
	}
}

func TestManualCompactSummarizesAndKeepsRecentMessages(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, "user")
		conversation.AppendAssistantMessage(conv, "assistant")
	}
	providerImpl := &summaryProvider{summary: "## 当前目标\n继续工作"}
	manager := newTestManager(t, providerImpl, testConfig(), redact.NewRuntimeRedactor(), 8)
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
	if providerImpl.closed != 1 {
		t.Fatalf("summary stream close calls = %d, want 1", providerImpl.closed)
	}
	if len(providerImpl.lastReq.Messages) != 1 || !strings.Contains(providerImpl.lastReq.Messages[0].Content.Text(), "user") {
		t.Fatalf("summary request did not use safe Provider DTO: %#v", providerImpl.lastReq.Messages)
	}
}

func TestSummaryFailureCircuitBreaker(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, strings.Repeat("x", 20))
	}
	cfg := testConfig()
	cfg.SummaryFailureLimit = 3
	manager := newTestManager(t, &summaryProvider{err: errors.New("boom")}, cfg, redact.NewRuntimeRedactor(), 8)
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
	legacy := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)
	withOptions := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)

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
		summaryInput string
		summary      string
	}
	normalize := func(t *testing.T, conv *conversation.Conversation, result Result, summaryInput string) snapshot {
		t.Helper()
		conv.CreatedAt = time.Time{}
		conv.UpdatedAt = time.Time{}
		if conv.Context != nil {
			conv.Context.LastCompressionAt = nil
		}
		for index := range conv.Messages {
			conv.Messages[index].CreatedAt = time.Time{}
		}
		data, err := json.Marshal(conv)
		if err != nil {
			t.Fatalf("marshal compatibility conversation: %v", err)
		}
		summary := ""
		if conv.Context != nil {
			summary = conv.Context.Summary.Text()
		}
		return snapshot{result: result, conversation: string(data), summaryInput: summaryInput, summary: summary}
	}

	observers := []struct {
		name     string
		observer CompactionObserver
	}{{name: "nil", observer: nil}, {name: "noop", observer: noHookCompactionObserver{}}}

	t.Run("auto no-op", func(t *testing.T) {
		var baseline *snapshot
		for _, item := range observers {
			conv := conversation.NewConversation("compat-auto", time.Unix(10, 0))
			conversation.AppendToolResultMessage(conv, "read-1", "Read", "success", "large", strings.Repeat("payload", 20), "", false, nil, nil)
			manager := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)
			result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{
				Mode: ModeAuto, PersistArtifacts: true, Observer: item.observer,
			})
			if err != nil {
				t.Fatalf("%s auto prepare: %v", item.name, err)
			}
			got := normalize(t, conv, result, "")
			if baseline == nil {
				copy := got
				baseline = &copy
			} else if !reflect.DeepEqual(*baseline, got) {
				t.Fatalf("%s observer changed auto compact:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
			}
		}
		if baseline == nil || baseline.result.Changed || baseline.result.Externalized != 0 || baseline.result.Summarized {
			t.Fatalf("auto no-op compatibility changed: %#v", baseline)
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
			manager := newTestManager(t, providerImpl, testConfig(), redact.NewRuntimeRedactor(), 8)
			result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{
				Mode: ModeManual, PersistArtifacts: true, Observer: item.observer,
			})
			if err != nil {
				t.Fatalf("%s manual prepare: %v", item.name, err)
			}
			summaryInput := ""
			if len(providerImpl.lastReq.Messages) == 1 {
				summaryInput = providerImpl.lastReq.Messages[0].Content.Text()
			}
			got := normalize(t, conv, result, summaryInput)
			if baseline == nil {
				copy := got
				baseline = &copy
			} else if !reflect.DeepEqual(*baseline, got) {
				t.Fatalf("%s observer changed manual compact:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
			}
		}
		if baseline == nil || !baseline.result.Changed || !baseline.result.Summarized || baseline.result.Externalized != 0 ||
			!strings.Contains(baseline.summaryInput, "question") || !strings.Contains(baseline.summary, "保持兼容") {
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
	manager := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)
	if _, err = manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeManual, PersistArtifacts: true, Observer: observer}); err != nil {
		t.Fatal(err)
	}
	if observer.beforeCalls != 1 || observer.afterCalls != 1 {
		t.Fatalf("observer calls = before:%d after:%d", observer.beforeCalls, observer.afterCalls)
	}
}

func TestCompactionObserverBoundaries(t *testing.T) {
	manager := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)
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
	cfg.ModelWindowTokens = 2
	cfg.AutoMarginTokens = 1
	cfg.ManualMarginTokens = 1
	cfg.RecentKeepTokens = 1
	cfg.SummaryFailureLimit = 3
	observer := &recordingCompactionObserver{}
	manager := newTestManager(t, &summaryProvider{err: errors.New("summary canary")}, cfg, redact.NewRuntimeRedactor(), 8)
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
	cfg.ModelWindowTokens = 2
	cfg.AutoMarginTokens = 1
	cfg.ManualMarginTokens = 1
	cfg.RecentKeepTokens = 1
	observer := &recordingCompactionObserver{}
	manager := newTestManager(t, &summaryProvider{summary: "## 当前目标\ntransient"}, cfg, redact.NewRuntimeRedactor(), 8)
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
		if message.Tool != nil && message.Tool.Artifact != nil {
			t.Fatalf("transient prepare persisted a tool artifact: %#v", message)
		}
	}
}

func TestCompactionObserverFailureIsFailOpen(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendUserMessage(conv, "small")
	manager := newTestManager(t, &summaryProvider{summary: "unused"}, testConfig(), redact.NewRuntimeRedactor(), 8)
	result, err := manager.PrepareWithOptions(context.Background(), conv, PrepareOptions{Mode: ModeManual, PersistArtifacts: true, Observer: panickingCompactionObserver{}})
	if err != nil || result.Changed {
		t.Fatalf("observer panic changed compaction result: result=%#v err=%v", result, err)
	}
}

func newTestManager(t *testing.T, providerImpl provider.Provider, cfg config.ContextConfig, redactor *redact.RuntimeRedactor, inlineBytes int64) *Manager {
	t.Helper()
	manager, err := New(providerImpl, ManagerOptions{Context: cfg, InlineOutputBytes: inlineBytes, RuntimeRedactor: redactor})
	if err != nil {
		t.Fatal("context manager setup failed")
	}
	return manager
}

func buildToolResult(t *testing.T, redactor *redact.RuntimeRedactor, input tool.ResultFactoryInput) tool.Result {
	t.Helper()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal("result factory setup failed")
	}
	result, err := factory.Build(input)
	if err != nil {
		t.Fatal("result factory rejected test fixture")
	}
	return result
}

func setPrivateResultField(t *testing.T, result tool.Result, name string, value any) tool.Result {
	t.Helper()
	field := reflect.ValueOf(&result).Elem().FieldByName(name)
	if !field.IsValid() || !field.CanAddr() {
		t.Fatal("result fixture field is unavailable")
	}
	writable := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	replacement := reflect.ValueOf(value)
	if !replacement.IsValid() || replacement.Type() != field.Type() {
		t.Fatal("result fixture field type is invalid")
	}
	writable.Set(replacement)
	return result
}

func assertProjectionHasNoCanary(t *testing.T, projection ToolResultProjection, canaries ...string) {
	t.Helper()
	values := []string{
		projection.ModelContent.Text(), projection.UserView.Summary.Text(), projection.UserView.Preview.Text(),
		projection.UserView.TruncationReason.Text(), projection.PersistedContent.Text(), projection.OutputMeta.TruncationReason.Text(),
	}
	if projection.UserView.Error != nil {
		values = append(values, projection.UserView.Error.Message.Text())
	}
	for _, value := range values {
		if containsAnyCanary(value, canaries...) {
			t.Fatal("canary escaped a safe projection")
		}
	}
}

func containsAnyCanary(value string, canaries ...string) bool {
	for _, canary := range canaries {
		if strings.Contains(value, canary) {
			return true
		}
	}
	return false
}

type poisonArtifactStore struct {
	raw       string
	path      string
	openCount int
}

func (store *poisonArtifactStore) reference() artifact.Ref {
	return artifact.Ref{
		ID:        "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Bytes:     int64(len(store.raw) + len(store.path)),
		CreatedAt: time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC),
		Available: true,
		Complete:  true,
	}
}

func (*poisonArtifactStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return nil, errors.New("poison store begin is forbidden")
}

func (store *poisonArtifactStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	store.openCount++
	panic("poison store open is forbidden")
}

func (*poisonArtifactStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}

func (*poisonArtifactStore) Close() error { return nil }

var _ artifact.Store = (*poisonArtifactStore)(nil)

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
	closed  int
}

type summaryStream struct {
	events <-chan provider.StreamEvent
	close  func()
}

func (s *summaryStream) Events() <-chan provider.StreamEvent { return s.events }

func (s *summaryStream) Close(context.Context) error {
	if s.close != nil {
		s.close()
		s.close = nil
	}
	return nil
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

func (p *summaryProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (provider.ChatStream, error) {
	p.lastReq = req
	out := make(chan provider.StreamEvent, 2)
	redactor := redact.NewRuntimeRedactor()
	go func() {
		defer close(out)
		if p.err != nil {
			out <- provider.StreamEvent{Type: provider.StreamEventError, Error: &diagnostics.SafeError{
				Code: "summary_failed", Source: "contextmgr.test", Message: redactor.Redact(p.err.Error()),
			}}
			return
		}
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: redactor.Redact(p.summary)}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}()
	return &summaryStream{events: out, close: func() { p.closed++ }}, nil
}
