package conversation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
)

var (
	ErrV2MaintenanceConfig    = errors.New("v2_maintenance_invalid_config")
	ErrV2MaintenanceDirectory = errors.New("v2_maintenance_directory_failed")
)

const (
	v2MaxRetentionDays = int64(3650)

	v2MaintenanceTruncatedCode           = "conversation_v2_maintenance_truncated"
	v2MaintenanceInvalidNameCode         = "conversation_v2_maintenance_invalid_name"
	v2MaintenanceUnsupportedEntryCode    = "conversation_v2_maintenance_unsupported_entry"
	v2MaintenanceOpenFailedCode          = "conversation_v2_maintenance_open_failed"
	v2MaintenanceLegacyPreservedCode     = "conversation_v2_maintenance_legacy_preserved"
	v2MaintenanceDeleteRaceCode          = "conversation_v2_maintenance_delete_race"
	v2MaintenanceDeleteFailedCode        = "conversation_v2_maintenance_delete_failed"
	v2MaintenanceTruncatedMessage        = "Conversation maintenance reached its scan budget."
	v2MaintenanceInvalidNameMessage      = "A conversation entry with an invalid identifier was preserved."
	v2MaintenanceUnsupportedEntryMessage = "A non-regular conversation entry was preserved."
	v2MaintenanceOpenFailedMessage       = "A conversation file could not be verified and was preserved."
	v2MaintenanceLegacyPreservedMessage  = "A legacy or recoverable conversation file was preserved."
	v2MaintenanceDeleteRaceMessage       = "A conversation file changed before deletion and was preserved."
	v2MaintenanceDeleteFailedMessage     = "An expired conversation file could not be deleted."
)

// v2MaintenanceOptions contains only resolved C8 values. The config owner
// applies defaults; this boundary defensively rejects zero and out-of-range
// values rather than inferring presence again.
type v2MaintenanceOptions struct {
	RetentionDays int64
	MaxScanFiles  int64
	MaxScanBytes  int64
	Now           func() time.Time
}

// maintainV2Sessions is the bounded v2 maintenance implementation retained
// beside the legacy Store until the atomic API cutover in T3.18. Only a fully
// verified v2 chain can be deleted; legacy, partial, corrupt, and unsupported
// entries are always preserved.
func maintainV2Sessions(
	ctx context.Context,
	codec *V2RecordCodec,
	dataRoot string,
	options v2MaintenanceOptions,
) (MaintenanceResult, error) {
	if ctx == nil || codec == nil || codec.redactor == nil || codec.maxRecordBytes <= 0 || codec.maxSessionBytes <= 0 ||
		dataRoot == "" || !filepath.IsAbs(dataRoot) || filepath.Clean(dataRoot) != dataRoot {
		return MaintenanceResult{}, ErrV2MaintenanceConfig
	}
	if err := ctx.Err(); err != nil {
		return MaintenanceResult{}, err
	}
	retentionDays, err := resolveV2MaintenanceRetentionDays(options.RetentionDays)
	if err != nil {
		return MaintenanceResult{}, err
	}
	maxFiles, err := resolveV2ListLimit(budget.SessionMaxScanFiles, budget.Files, options.MaxScanFiles)
	if err != nil {
		return MaintenanceResult{}, ErrV2MaintenanceConfig
	}
	maxBytes, err := resolveV2ListLimit(budget.SessionMaxScanBytes, budget.Bytes, options.MaxScanBytes)
	if err != nil {
		return MaintenanceResult{}, ErrV2MaintenanceConfig
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	currentTime := now()
	if currentTime.IsZero() {
		return MaintenanceResult{}, ErrV2MaintenanceConfig
	}
	cutoff := currentTime.AddDate(0, 0, -int(retentionDays))
	if err := acquireV2PersistenceMutation(ctx); err != nil {
		return MaintenanceResult{}, err
	}
	defer releaseV2PersistenceMutation()
	if err := ctx.Err(); err != nil {
		return MaintenanceResult{}, err
	}

	observedRoot, err := os.Lstat(dataRoot)
	if err != nil || observedRoot.Mode()&os.ModeSymlink != 0 || !observedRoot.IsDir() {
		if contextErr := ctx.Err(); contextErr != nil {
			return MaintenanceResult{}, contextErr
		}
		return MaintenanceResult{}, ErrV2MaintenanceDirectory
	}
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return MaintenanceResult{}, contextErr
		}
		return MaintenanceResult{}, ErrV2MaintenanceDirectory
	}
	defer root.Close()
	openedRoot, err := root.Stat(".")
	if err != nil || !openedRoot.IsDir() || !os.SameFile(observedRoot, openedRoot) {
		if contextErr := ctx.Err(); contextErr != nil {
			return MaintenanceResult{}, contextErr
		}
		return MaintenanceResult{}, ErrV2MaintenanceDirectory
	}
	directory, err := root.Open(".")
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return MaintenanceResult{}, contextErr
		}
		return MaintenanceResult{}, ErrV2MaintenanceDirectory
	}
	defer directory.Close()

	result := MaintenanceResult{}
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entries, readErr := directory.ReadDir(1)
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if errors.Is(readErr, io.EOF) && len(entries) == 0 {
			break
		}
		if (readErr != nil && !errors.Is(readErr, io.EOF)) || len(entries) != 1 {
			return result, ErrV2MaintenanceDirectory
		}
		if int64(result.ScannedFiles) >= maxFiles {
			markV2MaintenanceTruncated(&result, codec)
			break
		}
		result.ScannedFiles++
		entry := entries[0]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		name := entry.Name()
		sessionID := strings.TrimSuffix(name, ".jsonl")
		if !validV2ListSessionID(sessionID) {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceInvalidNameCode, v2MaintenanceInvalidNameMessage)
			continue
		}
		observed, statErr := root.Lstat(name)
		if statErr != nil {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceOpenFailedCode, v2MaintenanceOpenFailedMessage)
			continue
		}
		if observed.Mode()&os.ModeSymlink != 0 || !observed.Mode().IsRegular() || observed.Size() < 0 {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceUnsupportedEntryCode, v2MaintenanceUnsupportedEntryMessage)
			continue
		}
		if observed.Size() > maxBytes-result.ScannedBytes {
			markV2MaintenanceTruncated(&result, codec)
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}

		file, openErr := root.Open(name)
		if openErr != nil {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceOpenFailedCode, v2MaintenanceOpenFailedMessage)
			continue
		}
		opened, openedErr := file.Stat()
		if openedErr != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || opened.Size() < 0 {
			_ = file.Close()
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceUnsupportedEntryCode, v2MaintenanceUnsupportedEntryMessage)
			continue
		}
		if opened.Size() > maxBytes-result.ScannedBytes {
			_ = file.Close()
			markV2MaintenanceTruncated(&result, codec)
			break
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return result, err
		}

		result.ScannedBytes += opened.Size()
		state, report, recoverErr := recoverV2Records(ctx, codec, sessionID, io.LimitReader(file, opened.Size()))
		closeErr := file.Close()
		if recoverErr != nil {
			if errors.Is(recoverErr, context.Canceled) || errors.Is(recoverErr, context.DeadlineExceeded) {
				return result, recoverErr
			}
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceOpenFailedCode, v2MaintenanceOpenFailedMessage)
			continue
		}
		if closeErr != nil {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceOpenFailedCode, v2MaintenanceOpenFailedMessage)
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if report.Status != RecoveryClean || state.Conversation == nil || state.RecordCount == 0 || state.VerifiedBytes != opened.Size() {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceLegacyPreservedCode, v2MaintenanceLegacyPreservedMessage)
			continue
		}
		if state.Conversation.UpdatedAt.IsZero() || !state.Conversation.UpdatedAt.Before(cutoff) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		beforeDelete, statErr := root.Lstat(name)
		if statErr != nil || beforeDelete.Mode()&os.ModeSymlink != 0 || !beforeDelete.Mode().IsRegular() ||
			!os.SameFile(opened, beforeDelete) || beforeDelete.Size() != opened.Size() || !beforeDelete.ModTime().Equal(opened.ModTime()) {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceDeleteRaceCode, v2MaintenanceDeleteRaceMessage)
			continue
		}
		if err := root.Remove(name); err != nil {
			markV2MaintenanceSkipped(&result, codec, v2MaintenanceDeleteFailedCode, v2MaintenanceDeleteFailedMessage)
			continue
		}
		result.Deleted++
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func resolveV2MaintenanceRetentionDays(configured int64) (int64, error) {
	if configured < 1 || configured > v2MaxRetentionDays {
		return 0, ErrV2MaintenanceConfig
	}
	return configured, nil
}

func markV2MaintenanceSkipped(result *MaintenanceResult, codec *V2RecordCodec, code string, message string) {
	if result == nil {
		return
	}
	result.Skipped++
	appendV2MaintenanceDiagnostic(result, codec, code, message)
}

func markV2MaintenanceTruncated(result *MaintenanceResult, codec *V2RecordCodec) {
	if result == nil || result.Truncated {
		return
	}
	result.Truncated = true
	appendV2MaintenanceDiagnostic(result, codec, v2MaintenanceTruncatedCode, v2MaintenanceTruncatedMessage)
}

func appendV2MaintenanceDiagnostic(result *MaintenanceResult, codec *V2RecordCodec, code string, message string) {
	if result == nil || codec == nil || codec.redactor == nil {
		return
	}
	for _, existing := range result.Diagnostics {
		if existing.Code == code {
			return
		}
	}
	diagnostic := diagnostics.New(code, diagnostics.SeverityWarning, message).WithSource(v2RecoverySource)
	result.Diagnostics = append(result.Diagnostics, diagnostic.Safe(codec.redactor.Text))
}
