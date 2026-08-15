package conversation

import (
	"context"
	"errors"
	"io"
)

var (
	ErrV2LoadEmpty      = errors.New("v2_load_empty")
	ErrV2LoadValidation = errors.New("v2_load_validation_failed")
	ErrV2LoadDigest     = errors.New("v2_load_digest_mismatch")
)

// v2LoadState is the last completely verified prefix. On a non-nil error the
// caller may inspect it for recovery, but must not report a clean load.
type v2LoadState struct {
	Conversation  *Conversation
	Persisted     PersistedState
	VerifiedBytes int64
	RecordCount   int
}

// loadV2Records borrows reader and never closes it. Each record is decoded,
// linked, applied to an independent candidate, and digest-verified before the
// result advances.
func loadV2Records(ctx context.Context, codec *V2RecordCodec, sessionID string, reader io.Reader) (v2LoadState, error) {
	if ctx == nil || codec == nil || reader == nil || !validRecordIdentifier(sessionID) {
		return v2LoadState{}, ErrV2LoadValidation
	}
	decoder, err := codec.NewDecoder(sessionID, reader)
	if err != nil {
		return v2LoadState{}, err
	}
	state := v2LoadState{}
	for {
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		default:
		}

		record, err := decoder.Decode()
		if contextErr := ctx.Err(); contextErr != nil {
			return state, contextErr
		}
		if errors.Is(err, io.EOF) {
			if state.RecordCount == 0 {
				return state, ErrV2LoadEmpty
			}
			return state, nil
		}
		if err != nil {
			return state, err
		}

		candidate, persisted, err := applyAndVerifyV2Record(state, record, sessionID)
		if err != nil {
			return state, err
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return state, contextErr
		}
		state.Conversation = candidate
		state.Persisted = persisted
		state.VerifiedBytes = decoder.BytesRead()
		state.RecordCount++
	}
}

func applyAndVerifyV2Record(state v2LoadState, record JSONLRecordV2, sessionID string) (*Conversation, PersistedState, error) {
	validation := RecordValidationContext{ExpectedSessionID: sessionID}
	if state.RecordCount > 0 {
		validation.Previous = &state.Persisted
	}
	if err := record.Validate(validation); err != nil {
		return nil, PersistedState{}, ErrV2LoadValidation
	}

	var candidate *Conversation
	switch record.Kind {
	case RecordSnapshot:
		candidate = cloneV2Conversation(record.Snapshot)
	case RecordBatch:
		if state.Conversation == nil || record.Batch == nil || record.Batch.BaseMessageCount != len(state.Conversation.Messages) {
			return nil, PersistedState{}, ErrV2LoadValidation
		}
		candidate = cloneV2Conversation(state.Conversation)
		candidate.Messages = append(candidate.Messages, cloneV2MessageSlice(record.Batch.Messages)...)
		candidate.UpdatedAt = record.Batch.UpdatedAt
	default:
		return nil, PersistedState{}, ErrV2LoadValidation
	}

	persisted, err := computePersistedState(candidate, record.Revision)
	if err != nil {
		return nil, PersistedState{}, ErrV2LoadValidation
	}
	if persisted.Digest != record.Digest {
		return nil, PersistedState{}, ErrV2LoadDigest
	}
	return candidate, persisted, nil
}
