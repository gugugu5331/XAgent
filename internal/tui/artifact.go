package tui

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"xagent/internal/redact"
)

const (
	artifactMetadataWidth      = 512
	artifactMetadataFieldWidth = 160
	artifactPageRunes          = 16 * 1024
	artifactPageBytes          = int64(16 * 1024)
	artifactPageLineWidth      = 512
	artifactPageLines          = 256
)

// ArtifactViewSpec is the capability-free projection produced by App. Raw
// artifact bytes, filesystem paths, readers, callbacks, and Store
// capabilities have no representation in this type.
type ArtifactViewSpec struct {
	ID                string
	Bytes             int64
	CreatedAtUnixNano int64
	Available         bool
	Complete          bool
	TruncationReason  redact.SafeText
	Error             redact.SafeText
	Page              redact.SafeText
	Offset            int64
	ConsumedBytes     int64
	HasMore           bool
}

// ArtifactView is an immutable display-only value. Page contains only the
// bounded, redacted user page supplied by App, never the artifact reader or
// its raw backing data.
type ArtifactView struct {
	id                string
	bytes             int64
	createdAtUnixNano int64
	available         bool
	complete          bool
	truncationReason  redact.SafeText
	errorText         redact.SafeText
	page              redact.SafeText
	offset            int64
	consumedBytes     int64
	hasMore           bool
}

// ArtifactOpenIntent is a pure local-user action value. It cannot open or
// read content by itself and carries no mutable or capability-bearing value.
type ArtifactOpenIntent struct {
	id     string
	offset int64
}

func NewArtifactView(spec ArtifactViewSpec) ArtifactView {
	reason := boundedArtifactSafeText(spec.TruncationReason, artifactMetadataFieldWidth, false)
	errorText := boundedArtifactSafeText(spec.Error, artifactMetadataFieldWidth, false)
	page := boundedArtifactSafeText(spec.Page, artifactPageRunes, true)

	id := spec.ID
	bytes := spec.Bytes
	offset := spec.Offset
	consumedBytes := spec.ConsumedBytes
	available := spec.Available
	if !validOpaqueArtifactID(id) {
		id = ""
		available = false
	}
	if bytes < 0 {
		bytes = 0
		available = false
	}
	if offset < 0 || offset > bytes || offset%artifactPageBytes != 0 {
		offset = 0
		available = false
	}
	if consumedBytes < 0 || consumedBytes > bytes-offset {
		consumedBytes = 0
		available = false
	}
	if strings.TrimSpace(errorText.Text()) != "" {
		available = false
	}
	hasMore := spec.HasMore && available && consumedBytes == artifactPageBytes &&
		offset <= math.MaxInt64-consumedBytes && offset+consumedBytes < bytes
	return ArtifactView{
		id: id, bytes: bytes, createdAtUnixNano: spec.CreatedAtUnixNano,
		available: available, complete: spec.Complete,
		truncationReason: reason, errorText: errorText, page: page,
		offset: offset, consumedBytes: consumedBytes, hasMore: hasMore,
	}
}

// NewArtifactOpenIntent validates the exact command value. Leading or
// trailing whitespace, uppercase hexadecimal, path-shaped IDs, and negative
// offsets are rejected rather than normalized.
func NewArtifactOpenIntent(id string, offset int64) (ArtifactOpenIntent, bool) {
	if !validOpaqueArtifactID(id) || offset < 0 || offset%artifactPageBytes != 0 {
		return ArtifactOpenIntent{}, false
	}
	return ArtifactOpenIntent{id: id, offset: offset}, true
}

func (view ArtifactView) ID() string                        { return view.id }
func (view ArtifactView) Bytes() int64                      { return view.bytes }
func (view ArtifactView) CreatedAtUnixNano() int64          { return view.createdAtUnixNano }
func (view ArtifactView) Available() bool                   { return view.available }
func (view ArtifactView) Complete() bool                    { return view.complete }
func (view ArtifactView) TruncationReason() redact.SafeText { return view.truncationReason }
func (view ArtifactView) Error() redact.SafeText            { return view.errorText }
func (view ArtifactView) Page() redact.SafeText             { return view.page }
func (view ArtifactView) Offset() int64                     { return view.offset }
func (view ArtifactView) ConsumedBytes() int64              { return view.consumedBytes }
func (view ArtifactView) HasMore() bool                     { return view.hasMore }
func (intent ArtifactOpenIntent) ID() string                { return intent.id }
func (intent ArtifactOpenIntent) Offset() int64             { return intent.offset }

func (view ArtifactView) OpenIntent() (ArtifactOpenIntent, bool) {
	if !view.canOpen() {
		return ArtifactOpenIntent{}, false
	}
	return NewArtifactOpenIntent(view.id, 0)
}

func (view ArtifactView) NextIntent() (ArtifactOpenIntent, bool) {
	if !view.canOpen() || !view.hasMore || view.consumedBytes <= 0 || view.offset > math.MaxInt64-view.consumedBytes {
		return ArtifactOpenIntent{}, false
	}
	next := view.offset + view.consumedBytes
	if next > view.bytes {
		return ArtifactOpenIntent{}, false
	}
	return NewArtifactOpenIntent(view.id, next)
}

func (view ArtifactView) PreviousIntent() (ArtifactOpenIntent, bool) {
	if !view.canOpen() || view.offset <= 0 {
		return ArtifactOpenIntent{}, false
	}
	previous := view.offset - artifactPageBytes
	if previous < 0 {
		previous = 0
	}
	return NewArtifactOpenIntent(view.id, previous)
}

func (view ArtifactView) canOpen() bool {
	return view.available && strings.TrimSpace(view.errorText.Text()) == "" && validOpaqueArtifactID(view.id)
}

// MetadataLine is the bounded, single-line projection used by regular tool
// rows. It intentionally excludes Page and paging offsets.
func (view ArtifactView) MetadataLine() string {
	id := view.id
	if id == "" {
		id = "unavailable"
	}
	availability := "Unavailable"
	if view.available {
		availability = "Available"
	}
	completeness := "Incomplete"
	if view.complete {
		completeness = "Complete"
	}
	parts := []string{
		"Artifact ID=" + id,
		fmt.Sprintf("%d bytes", view.bytes),
		availability,
		completeness,
	}
	if reason := strings.TrimSpace(view.truncationReason.Text()); reason != "" {
		parts = append(parts, "Truncation="+reason)
	}
	if message := strings.TrimSpace(view.errorText.Text()); message != "" {
		parts = append(parts, "Error="+message)
	}
	return truncateArtifactWidth(truncateArtifactRunes(safeArtifactSingleLine(strings.Join(parts, " | ")), artifactMetadataWidth), artifactMetadataWidth)
}

// View renders the dedicated user page. Regular messages must call
// MetadataLine so the page can never be copied into conversation history.
func (view ArtifactView) View() string {
	metadata := view.MetadataLine()
	page := view.page.Text()
	if page == "" {
		return metadata
	}
	paging := fmt.Sprintf("Page offset=%d | consumed=%d bytes | %s", view.offset, view.consumedBytes, artifactPageEnd(view.hasMore))
	return metadata + "\n" + truncateArtifactWidth(paging, artifactMetadataWidth) + "\n" + page
}

func artifactPageEnd(hasMore bool) string {
	if hasMore {
		return "More"
	}
	return "End"
}

func validOpaqueArtifactID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for index := 0; index < len(id); index++ {
		current := id[index]
		if !((current >= '0' && current <= '9') || (current >= 'a' && current <= 'f')) {
			return false
		}
	}
	return true
}

func boundedArtifactSafeText(value redact.SafeText, limit int, preserveNewlines bool) redact.SafeText {
	text := value.Text()
	if preserveNewlines {
		text = safeArtifactPage(text)
	} else {
		text = truncateArtifactWidth(truncateArtifactRunes(safeArtifactSingleLine(text), limit), limit)
	}
	return redact.NewRuntimeRedactor().Redact(text)
}

func safeArtifactSingleLine(value string) string {
	value = ansi.Strip(value)
	value = strings.Map(func(current rune) rune {
		if current < ' ' || current == '\x7f' || current >= '\x80' && current <= '\x9f' {
			return ' '
		}
		return current
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func safeArtifactPage(value string) string {
	value = ansi.Strip(value)
	value = strings.Map(func(current rune) rune {
		if current == '\n' {
			return current
		}
		if current < ' ' || current == '\x7f' || current >= '\x80' && current <= '\x9f' {
			return ' '
		}
		return current
	}, value)
	value = truncateArtifactRunes(value, artifactPageRunes)
	lines := strings.Split(value, "\n")
	if len(lines) > artifactPageLines {
		lines = lines[:artifactPageLines]
		lines[len(lines)-1] = truncateArtifactWidth(lines[len(lines)-1], artifactPageLineWidth-1) + "…"
	}
	for index := range lines {
		lines[index] = truncateArtifactWidth(lines[index], artifactPageLineWidth)
	}
	return strings.Join(lines, "\n")
}

func truncateArtifactRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func truncateArtifactWidth(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "…")
}
