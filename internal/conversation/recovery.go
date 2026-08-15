package conversation

import (
	"context"
	"errors"
	"io"
	"time"

	"xagent/internal/diagnostics"
)

const (
	v2RecoverySource             = "conversation"
	v2RecoveryGapReminderCode    = "conversation_v2_gap_reminder"
	v2RecoveryTornTailCode       = "conversation_v2_torn_tail"
	v2RecoveryCorruptCode        = "conversation_v2_corrupt_record"
	v2RecoveryBudgetCode         = "conversation_v2_record_budget_exceeded"
	v2RecoveryReadCode           = "conversation_v2_read_failed"
	v2RecoveryEmptyCode          = "conversation_v2_empty"
	v2RecoveryTornTailMessage    = "The final incomplete conversation record was ignored."
	v2RecoveryCorruptMessage     = "Conversation recovery stopped at a damaged record."
	v2RecoveryBudgetMessage      = "Conversation recovery stopped at a record budget limit."
	v2RecoveryReadMessage        = "Conversation recovery stopped after a record read failure."
	v2RecoveryEmptyMessage       = "The conversation file contains no complete records."
	v2RecoveryGapReminderMessage = "This conversation has been inactive beyond the configured reminder interval. Reconfirm current files and external state before continuing."
	v2MaxGapReminderDays         = int64(3650)
)

var ErrV2RecoveryConfig = errors.New("v2_recovery_invalid_config")

// v2RecoveryMetadataOptions contains only resolved session values. Config
// owns defaults and presence; this leaf boundary rejects zero rather than
// inferring that it means the approved default.
type v2RecoveryMetadataOptions struct {
	GapReminderDays int64
	Now             func() time.Time
}

// recoverV2RecordsForLoad decorates the non-destructive recovery result with
// Load-only metadata. List and Maintain deliberately continue using
// recoverV2Records so an inactivity reminder is not confused with corruption
// or maintenance diagnostics before the atomic Store cutover in T3.18.
func recoverV2RecordsForLoad(
	ctx context.Context,
	codec *V2RecordCodec,
	sessionID string,
	reader io.Reader,
	options v2RecoveryMetadataOptions,
) (v2LoadState, RecoveryReport, error) {
	if ctx == nil || codec == nil || reader == nil || !validRecordIdentifier(sessionID) {
		return v2LoadState{}, RecoveryReport{}, ErrV2LoadValidation
	}
	if err := ctx.Err(); err != nil {
		return v2LoadState{}, RecoveryReport{}, err
	}
	if options.GapReminderDays < 1 || options.GapReminderDays > v2MaxGapReminderDays {
		return v2LoadState{}, RecoveryReport{}, ErrV2RecoveryConfig
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	currentTime := now()
	if currentTime.IsZero() {
		return v2LoadState{}, RecoveryReport{}, ErrV2RecoveryConfig
	}
	if err := ctx.Err(); err != nil {
		return v2LoadState{}, RecoveryReport{}, err
	}

	state, report, err := recoverV2Records(ctx, codec, sessionID, reader)
	if err != nil {
		return state, report, err
	}
	if err := ctx.Err(); err != nil {
		return state, RecoveryReport{}, err
	}
	if state.Conversation == nil || !v2GapReminderDue(state.Conversation.UpdatedAt, currentTime, options.GapReminderDays) {
		return state, report, nil
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == v2RecoveryGapReminderCode {
			return state, report, nil
		}
	}
	diagnostic := diagnostics.New(v2RecoveryGapReminderCode, diagnostics.SeverityInfo, v2RecoveryGapReminderMessage).WithSource(v2RecoverySource)
	if codec.redactor != nil {
		diagnostic = diagnostic.Safe(codec.redactor.Text)
	}
	report.Diagnostics = append(report.Diagnostics, diagnostic)
	return state, report, nil
}

func v2GapReminderDue(updatedAt time.Time, currentTime time.Time, gapDays int64) bool {
	return gapDays >= 1 && gapDays <= v2MaxGapReminderDays && !updatedAt.IsZero() && !currentTime.IsZero() &&
		updatedAt.Before(currentTime.AddDate(0, 0, -int(gapDays)))
}

// recoverV2Records performs a non-destructive recovery read. File ownership
// stays with the caller: this function never closes, truncates, replaces, or
// otherwise writes through reader.
func recoverV2Records(ctx context.Context, codec *V2RecordCodec, sessionID string, reader io.Reader) (v2LoadState, RecoveryReport, error) {
	if ctx == nil || codec == nil || reader == nil || !validRecordIdentifier(sessionID) {
		return v2LoadState{}, RecoveryReport{}, ErrV2LoadValidation
	}
	if err := ctx.Err(); err != nil {
		return v2LoadState{}, RecoveryReport{}, err
	}

	state, err := loadV2Records(ctx, codec, sessionID, reader)
	if err == nil {
		return state, RecoveryReport{Status: RecoveryClean, LastValidRevision: state.Persisted.Revision}, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrV2RecordCodecConfig) {
		return state, RecoveryReport{}, err
	}

	status := RecoveryPartial
	if state.RecordCount == 0 {
		status = RecoveryPlaceholder
	}
	switch {
	case errors.Is(err, ErrV2LoadEmpty):
		return state, newV2RecoveryReport(codec, status, state.Persisted.Revision, 0, v2RecoveryEmptyCode, v2RecoveryEmptyMessage), nil
	case errors.Is(err, ErrV2RecordTruncated):
		return state, newV2RecoveryReport(codec, status, state.Persisted.Revision, 1, v2RecoveryTornTailCode, v2RecoveryTornTailMessage), nil
	case isV2RecordBudgetError(err):
		return state, newV2RecoveryReport(codec, status, state.Persisted.Revision, 1, v2RecoveryBudgetCode, v2RecoveryBudgetMessage), nil
	case errors.Is(err, ErrV2RecordRead):
		return state, newV2RecoveryReport(codec, status, state.Persisted.Revision, 1, v2RecoveryReadCode, v2RecoveryReadMessage), nil
	case errors.Is(err, ErrV2RecordMalformed), errors.Is(err, ErrV2LoadValidation), errors.Is(err, ErrV2LoadDigest):
		return state, newV2RecoveryReport(codec, status, state.Persisted.Revision, 1, v2RecoveryCorruptCode, v2RecoveryCorruptMessage), nil
	default:
		return state, RecoveryReport{}, err
	}
}

func isV2RecordBudgetError(err error) bool {
	var budgetError *RecordBudgetError
	return errors.As(err, &budgetError)
}

func newV2RecoveryReport(codec *V2RecordCodec, status RecoveryStatus, revision uint64, skipped int, code string, message string) RecoveryReport {
	diagnostic := diagnostics.New(code, diagnostics.SeverityWarning, message).WithSource(v2RecoverySource)
	if codec != nil && codec.redactor != nil {
		diagnostic = diagnostic.Safe(codec.redactor.Text)
	}
	return RecoveryReport{
		Status:            status,
		LastValidRevision: revision,
		SkippedRecords:    skipped,
		Diagnostics:       []diagnostics.Diagnostic{diagnostic},
	}
}
