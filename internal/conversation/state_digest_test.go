package conversation

import (
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestStateDigestCoversEveryPersistedField(t *testing.T) {
	t.Run("deterministic versioned digests", func(t *testing.T) {
		conversation := stateDigestFixture()
		first := mustComputePersistedState(t, conversation, 7)
		second := mustComputePersistedState(t, cloneConversationV2(conversation), 8)
		if first.Digest != second.Digest || first.MessagesDigest != second.MessagesDigest {
			t.Fatalf("equivalent state produced unstable digests: %#v / %#v", first, second)
		}
		if first.Revision != 7 || second.Revision != 8 {
			t.Fatalf("revision was not copied independently: %#v / %#v", first, second)
		}
		if first.MessageCount != len(conversation.Messages) || second.MessageCount != len(conversation.Messages) {
			t.Fatalf("message count not published: %#v / %#v", first, second)
		}
		assertSHA256Digest(t, first.Digest)
		assertSHA256Digest(t, first.MessagesDigest)
		if first.Digest == first.MessagesDigest {
			t.Fatal("state and messages digest domains collided")
		}

		const wantState = "0bd1a0c242dfbbea7334acc788ab163894cd49e403e10a7cac675cab87980404"
		const wantMessages = "eb76ba8796d39dfeef148bfab3e55b5075d028bc0bee7cde83f78fd98d18d95b"
		if first.Digest != wantState || first.MessagesDigest != wantMessages {
			t.Fatalf("digest golden changed: state=%s messages=%s", first.Digest, first.MessagesDigest)
		}
	})

	t.Run("nil empty and time normalization", func(t *testing.T) {
		nilState := &Conversation{}
		emptyState := &Conversation{
			Title:    digestSafeText(""),
			Messages: []Message{},
			Context:  &ContextMetadata{},
		}
		first := mustComputePersistedState(t, nilState, 0)
		second := mustComputePersistedState(t, emptyState, 0)
		if first != second {
			t.Fatalf("nil and empty state were not normalized: %#v / %#v", first, second)
		}

		conversation := stateDigestFixture()
		otherZone := cloneConversationV2(conversation)
		zone := time.FixedZone("digest-test", 8*60*60)
		otherZone.CreatedAt = otherZone.CreatedAt.In(zone)
		otherZone.UpdatedAt = otherZone.UpdatedAt.In(zone)
		for index := range otherZone.Messages {
			otherZone.Messages[index].CreatedAt = otherZone.Messages[index].CreatedAt.In(zone)
			if state := otherZone.Messages[index].Tool; state != nil && state.Artifact != nil {
				state.Artifact.CreatedAt = state.Artifact.CreatedAt.In(zone)
			}
		}
		if otherZone.Context != nil && otherZone.Context.LastCompressionAt != nil {
			converted := otherZone.Context.LastCompressionAt.In(zone)
			otherZone.Context.LastCompressionAt = &converted
		}
		if got, want := mustComputePersistedState(t, otherZone, 0), mustComputePersistedState(t, conversation, 0); got != want {
			t.Fatalf("equal instants in different locations changed digest: %#v / %#v", got, want)
		}

		withMonotonic := stateDigestFixture()
		instant := time.Now()
		withMonotonic.CreatedAt = instant
		withMonotonic.Messages[0].CreatedAt = instant
		withoutMonotonic := cloneConversationV2(withMonotonic)
		withoutMonotonic.CreatedAt = instant.Round(0)
		withoutMonotonic.Messages[0].CreatedAt = instant.Round(0)
		if got, want := mustComputePersistedState(t, withMonotonic, 0), mustComputePersistedState(t, withoutMonotonic, 0); got != want {
			t.Fatalf("monotonic clock data changed digest: %#v / %#v", got, want)
		}
	})

	mutations := stateDigestMutations()
	t.Run("every persisted field changes its owning digest", func(t *testing.T) {
		assertMutationRegistryComplete(t, mutations)
		baselineConversation := stateDigestFixture()
		baseline := mustComputePersistedState(t, baselineConversation, 11)
		for _, mutation := range mutations {
			t.Run(strings.NewReplacer("[]", "_item", ".$", "_").Replace(mutation.path), func(t *testing.T) {
				changedConversation := cloneConversationV2(baselineConversation)
				mutation.apply(changedConversation)
				changed := mustComputePersistedState(t, changedConversation, 11)
				if changed.Digest == baseline.Digest {
					t.Fatalf("%s did not change full state digest", mutation.path)
				}
				if mutation.changesMessages && changed.MessagesDigest == baseline.MessagesDigest {
					t.Fatalf("%s did not change messages digest", mutation.path)
				}
				if !mutation.changesMessages && changed.MessagesDigest != baseline.MessagesDigest {
					t.Fatalf("%s unexpectedly changed messages digest", mutation.path)
				}
			})
		}
	})

	t.Run("invalid canonical text fails closed", func(t *testing.T) {
		conversation := stateDigestFixture()
		conversation.ID = string([]byte{0xff})
		if state, err := computePersistedState(conversation, 1); err == nil || state != (PersistedState{}) {
			t.Fatalf("invalid UTF-8 produced persisted state: %#v, %v", state, err)
		}
		if state, err := computePersistedState(nil, 1); err == nil || state != (PersistedState{}) {
			t.Fatalf("nil conversation produced persisted state: %#v, %v", state, err)
		}
	})
}

type stateDigestMutation struct {
	path            string
	changesMessages bool
	apply           func(*Conversation)
}

func stateDigestMutations() []stateDigestMutation {
	messageMutation := func(path string, apply func(*Conversation)) stateDigestMutation {
		return stateDigestMutation{path: path, changesMessages: true, apply: apply}
	}
	stateMutation := func(path string, apply func(*Conversation)) stateDigestMutation {
		return stateDigestMutation{path: path, apply: apply}
	}
	return []stateDigestMutation{
		stateMutation("Conversation.ID", func(value *Conversation) { value.ID += "-changed" }),
		stateMutation("Conversation.Title", func(value *Conversation) { value.Title = digestSafeText("changed title") }),
		messageMutation("Conversation.Messages.$length", func(value *Conversation) {
			value.Messages = append(value.Messages, Message{Role: RoleAssistant, Content: digestSafeText("appended"), CreatedAt: value.UpdatedAt.Add(time.Second)})
		}),
		messageMutation("Conversation.Messages.$order", func(value *Conversation) {
			value.Messages[0], value.Messages[1] = value.Messages[1], value.Messages[0]
		}),
		messageMutation("Conversation.Messages[].Role", func(value *Conversation) { value.Messages[0].Role = RoleAssistant }),
		messageMutation("Conversation.Messages[].Content", func(value *Conversation) { value.Messages[0].Content = digestSafeText("changed content") }),
		messageMutation("Conversation.Messages[].CreatedAt", func(value *Conversation) {
			value.Messages[0].CreatedAt = value.Messages[0].CreatedAt.Add(time.Nanosecond)
		}),
		messageMutation("Conversation.Messages[].Tool.$present", func(value *Conversation) { value.Messages[1].Tool = nil }),
		messageMutation("Conversation.Messages[].Tool.CallID", func(value *Conversation) { value.Messages[1].Tool.CallID += "-changed" }),
		messageMutation("Conversation.Messages[].Tool.Name", func(value *Conversation) { value.Messages[1].Tool.Name += "-changed" }),
		messageMutation("Conversation.Messages[].Tool.ArgumentsJSON", func(value *Conversation) { value.Messages[1].Tool.ArgumentsJSON = digestSafeText(`{"changed":true}`) }),
		messageMutation("Conversation.Messages[].Tool.State", func(value *Conversation) { value.Messages[1].Tool.State = tool.CancelledAfterStart }),
		messageMutation("Conversation.Messages[].Tool.Status", func(value *Conversation) { value.Messages[1].Tool.Status = tool.StatusSuccess }),
		messageMutation("Conversation.Messages[].Tool.Summary", func(value *Conversation) { value.Messages[1].Tool.Summary = digestSafeText("changed summary") }),
		messageMutation("Conversation.Messages[].Tool.Result", func(value *Conversation) { value.Messages[1].Tool.Result = digestSafeText("changed result") }),
		messageMutation("Conversation.Messages[].Tool.Truncated", func(value *Conversation) { value.Messages[1].Tool.Truncated = false }),
		messageMutation("Conversation.Messages[].Tool.TruncationReason", func(value *Conversation) {
			value.Messages[1].Tool.TruncationReason = digestSafeText("changed reason")
		}),
		messageMutation("Conversation.Messages[].Tool.Artifact.$present", func(value *Conversation) { value.Messages[1].Tool.Artifact = nil }),
		messageMutation("Conversation.Messages[].Tool.Artifact.ID", func(value *Conversation) { value.Messages[1].Tool.Artifact.ID += "-changed" }),
		messageMutation("Conversation.Messages[].Tool.Artifact.Bytes", func(value *Conversation) { value.Messages[1].Tool.Artifact.Bytes++ }),
		messageMutation("Conversation.Messages[].Tool.Artifact.CreatedAt", func(value *Conversation) {
			value.Messages[1].Tool.Artifact.CreatedAt = value.Messages[1].Tool.Artifact.CreatedAt.Add(time.Nanosecond)
		}),
		messageMutation("Conversation.Messages[].Tool.Artifact.Available", func(value *Conversation) { value.Messages[1].Tool.Artifact.Available = false }),
		messageMutation("Conversation.Messages[].Tool.Artifact.Complete", func(value *Conversation) { value.Messages[1].Tool.Artifact.Complete = false }),
		messageMutation("Conversation.Messages[].Tool.Error.$present", func(value *Conversation) { value.Messages[1].Tool.Error = nil }),
		messageMutation("Conversation.Messages[].Tool.Error.Code", func(value *Conversation) { value.Messages[1].Tool.Error.Code += "-changed" }),
		messageMutation("Conversation.Messages[].Tool.Error.Message", func(value *Conversation) { value.Messages[1].Tool.Error.Message = digestSafeText("changed error") }),
		messageMutation("Conversation.Messages[].Tool.Error.Recoverable", func(value *Conversation) { value.Messages[1].Tool.Error.Recoverable = false }),
		stateMutation("Conversation.Context.Summary", func(value *Conversation) { value.Context.Summary = digestSafeText("changed context summary") }),
		stateMutation("Conversation.Context.LastBoundary", func(value *Conversation) { value.Context.LastBoundary = digestSafeText("changed boundary") }),
		stateMutation("Conversation.Context.LastCompressionAt.$present", func(value *Conversation) { value.Context.LastCompressionAt = nil }),
		stateMutation("Conversation.Context.LastCompressionAt", func(value *Conversation) {
			changed := value.Context.LastCompressionAt.Add(time.Nanosecond)
			value.Context.LastCompressionAt = &changed
		}),
		stateMutation("Conversation.Context.SummaryFailureCount", func(value *Conversation) { value.Context.SummaryFailureCount++ }),
		stateMutation("Conversation.Context.LastInputTokens", func(value *Conversation) { value.Context.LastInputTokens++ }),
		stateMutation("Conversation.Context.LastOutputTokens", func(value *Conversation) { value.Context.LastOutputTokens++ }),
		stateMutation("Conversation.Context.LastEstimatedTokens", func(value *Conversation) { value.Context.LastEstimatedTokens++ }),
		stateMutation("Conversation.Context.LastEstimatedCharacters", func(value *Conversation) { value.Context.LastEstimatedCharacters++ }),
		stateMutation("Conversation.CreatedAt", func(value *Conversation) { value.CreatedAt = value.CreatedAt.Add(time.Nanosecond) }),
		stateMutation("Conversation.UpdatedAt", func(value *Conversation) { value.UpdatedAt = value.UpdatedAt.Add(time.Nanosecond) }),
	}
}

func stateDigestFixture() *Conversation {
	createdAt := time.Date(2026, time.August, 2, 14, 0, 1, 123456789, time.UTC)
	compressionAt := createdAt.Add(3 * time.Minute)
	return &Conversation{
		ID:    "conversation-1",
		Title: digestSafeText("安全会话"),
		Messages: []Message{
			{Role: RoleUser, Content: digestSafeText("first message"), CreatedAt: createdAt.Add(time.Second)},
			{
				Role:      RoleToolResult,
				Content:   digestSafeText("bounded tool content"),
				CreatedAt: createdAt.Add(2 * time.Second),
				Tool: &ToolState{
					CallID:           "call-1",
					Name:             "Read",
					ArgumentsJSON:    digestSafeText(`{"path":"@workspace/file"}`),
					State:            tool.Completed,
					Status:           tool.StatusError,
					Summary:          digestSafeText("tool summary"),
					Result:           digestSafeText("persisted result"),
					Truncated:        true,
					TruncationReason: digestSafeText(string(tool.CaptureTruncatedInline)),
					Artifact: &artifact.Ref{
						ID:        strings.Repeat("f", 64),
						Bytes:     4097,
						CreatedAt: createdAt.Add(4 * time.Second),
						Available: true,
						Complete:  true,
					},
					Error: &tool.SafeError{
						Code:        "read_failed",
						Message:     digestSafeText("safe tool error"),
						Recoverable: true,
					},
				},
			},
		},
		Context: &ContextMetadata{
			Summary:                 digestSafeText("context summary"),
			LastBoundary:            digestSafeText("context boundary"),
			LastCompressionAt:       &compressionAt,
			SummaryFailureCount:     1,
			LastInputTokens:         101,
			LastOutputTokens:        202,
			LastEstimatedTokens:     303,
			LastEstimatedCharacters: 404,
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt.Add(5 * time.Minute),
	}
}

func cloneConversationV2(source *Conversation) *Conversation {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Messages = append([]Message(nil), source.Messages...)
	for index := range cloned.Messages {
		if source.Messages[index].Tool == nil {
			continue
		}
		toolState := *source.Messages[index].Tool
		if toolState.Artifact != nil {
			artifactRef := *toolState.Artifact
			toolState.Artifact = &artifactRef
		}
		if toolState.Error != nil {
			safeError := *toolState.Error
			toolState.Error = &safeError
		}
		cloned.Messages[index].Tool = &toolState
	}
	if source.Context != nil {
		context := *source.Context
		if source.Context.LastCompressionAt != nil {
			lastCompressionAt := *source.Context.LastCompressionAt
			context.LastCompressionAt = &lastCompressionAt
		}
		cloned.Context = &context
	}
	return &cloned
}

func assertMutationRegistryComplete(t *testing.T, mutations []stateDigestMutation) {
	t.Helper()
	want := persistedDigestPaths(reflect.TypeOf(Conversation{}), "Conversation")
	got := make([]string, 0, len(mutations))
	seen := make(map[string]struct{}, len(mutations))
	for _, mutation := range mutations {
		if _, duplicate := seen[mutation.path]; duplicate {
			t.Fatalf("duplicate digest mutation for %s", mutation.path)
		}
		seen[mutation.path] = struct{}{}
		got = append(got, mutation.path)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted field mutation registry mismatch\n got: %v\nwant: %v", got, want)
	}
}

func persistedDigestPaths(current reflect.Type, path string) []string {
	safeTextType := reflect.TypeOf(redact.SafeText{})
	timeType := reflect.TypeOf(time.Time{})
	if current == safeTextType || current == timeType {
		return []string{path}
	}

	switch current.Kind() {
	case reflect.Pointer:
		if current == reflect.TypeOf((*ContextMetadata)(nil)) {
			return persistedDigestPaths(current.Elem(), path)
		}
		paths := []string{path + ".$present"}
		return append(paths, persistedDigestPaths(current.Elem(), path)...)
	case reflect.Slice:
		paths := []string{path + ".$length", path + ".$order"}
		return append(paths, persistedDigestPaths(current.Elem(), path+"[]")...)
	case reflect.Struct:
		paths := []string{}
		for index := 0; index < current.NumField(); index++ {
			field := current.Field(index)
			paths = append(paths, persistedDigestPaths(field.Type, path+"."+field.Name)...)
		}
		return sortedStrings(paths)
	default:
		return []string{path}
	}
}

func sortedStrings(values []string) []string {
	sort.Strings(values)
	return values
}

func mustComputePersistedState(t *testing.T, conversation *Conversation, revision uint64) PersistedState {
	t.Helper()
	state, err := computePersistedState(conversation, revision)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertSHA256Digest(t *testing.T, digest StateDigest) {
	t.Helper()
	encoded := string(digest)
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != 32 || encoded != strings.ToLower(encoded) {
		t.Fatalf("digest %q is not 64-character lowercase SHA-256", encoded)
	}
}

func digestSafeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}
