package tui

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestArtifactOpenIsLocalUserOnly(t *testing.T) {
	t.Run("regular view exposes bounded metadata and a capability free intent", func(t *testing.T) {
		id := strings.Repeat("a", 64)
		view := NewArtifactView(ArtifactViewSpec{
			ID: id, Bytes: 65536, CreatedAtUnixNano: 1234, Available: true, Complete: false,
			TruncationReason: artifactSafeText("capture_hard_limit"),
			Page:             artifactSafeText("dedicated-page-canary"),
			Offset:           artifactPageBytes,
			ConsumedBytes:    artifactPageBytes,
			HasMore:          true,
		})

		metadata := view.MetadataLine()
		for _, want := range []string{id, "65536 bytes", "Available", "Incomplete", "capture_hard_limit"} {
			if !strings.Contains(metadata, want) {
				t.Fatalf("artifact metadata view missing %q: %q", want, metadata)
			}
		}
		if strings.Contains(metadata, "dedicated-page-canary") {
			t.Fatalf("regular metadata included the dedicated user page: %q", metadata)
		}
		if view.CreatedAtUnixNano() != 1234 || view.Offset() != artifactPageBytes || view.ConsumedBytes() != artifactPageBytes || !view.HasMore() {
			t.Fatalf("artifact scalar projection changed: %#v", view)
		}
		if !strings.Contains(view.View(), "dedicated-page-canary") {
			t.Fatalf("dedicated artifact view omitted its safe page: %q", view.View())
		}

		intent, ok := view.OpenIntent()
		if !ok || intent.ID() != id || intent.Offset() != 0 {
			t.Fatalf("available artifact did not emit its initial opaque intent: ok=%t id=%q offset=%d", ok, intent.ID(), intent.Offset())
		}
		assertArtifactValueIsCapabilityFree(t, reflect.TypeOf(view), map[reflect.Type]bool{})
		assertArtifactValueIsCapabilityFree(t, reflect.TypeOf(intent), map[reflect.Type]bool{})
	})

	t.Run("opaque ids and offsets are exact rather than normalized", func(t *testing.T) {
		validID := strings.Repeat("b", 64)
		if intent, ok := NewArtifactOpenIntent(validID, artifactPageBytes); !ok || intent.ID() != validID || intent.Offset() != artifactPageBytes {
			t.Fatalf("valid intent rejected: %#v ok=%t", intent, ok)
		}
		invalidIDs := []string{
			"", strings.Repeat("b", 63), strings.Repeat("b", 65),
			" " + validID, validID + " ", strings.Repeat("B", 64),
			strings.Repeat("b", 62) + "/x", strings.Repeat("é", 64),
		}
		for _, id := range invalidIDs {
			if intent, ok := NewArtifactOpenIntent(id, 0); ok || intent.ID() != "" || intent.Offset() != 0 {
				t.Fatalf("invalid ID produced intent: id=%q intent=%#v ok=%t", id, intent, ok)
			}
			view := NewArtifactView(ArtifactViewSpec{ID: id, Available: true})
			if view.ID() != "" || view.Available() || strings.Contains(view.MetadataLine(), id) && id != "" {
				t.Fatalf("invalid ID entered display state: id=%q view=%#v output=%q", id, view, view.MetadataLine())
			}
		}
		if intent, ok := NewArtifactOpenIntent(validID, -1); ok || intent.ID() != "" {
			t.Fatalf("negative offset produced intent: %#v", intent)
		}
		if intent, ok := NewArtifactOpenIntent(validID, 7); ok || intent.ID() != "" {
			t.Fatalf("unaligned offset produced intent: %#v", intent)
		}
	})

	t.Run("unavailable error and inconsistent views cannot open", func(t *testing.T) {
		for _, test := range []struct {
			name string
			spec ArtifactViewSpec
		}{
			{name: "unavailable", spec: ArtifactViewSpec{ID: strings.Repeat("c", 64), Available: false}},
			{name: "negative bytes", spec: ArtifactViewSpec{ID: strings.Repeat("c", 64), Bytes: -1, Available: true}},
			{name: "safe error", spec: ArtifactViewSpec{
				ID: strings.Repeat("c", 64), Available: true,
				Error: artifactSafeText("artifact 无法打开，请确认 ID 后重试"),
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				view := NewArtifactView(test.spec)
				if intent, ok := view.OpenIntent(); ok || intent.ID() != "" {
					t.Fatalf("restricted artifact emitted an open intent: %#v", intent)
				}
			})
		}
	})

	t.Run("terminal controls and output growth are bounded", func(t *testing.T) {
		id := strings.Repeat("d", 64)
		injected := strings.Repeat("界\u0301", artifactPageRunes) + "\x1b]52;c;clipboard\a\x1b[2J\x00\x7f\u0085"
		view := NewArtifactView(ArtifactViewSpec{
			ID: id, Bytes: math.MaxInt64, Available: true, Complete: true,
			TruncationReason: artifactSafeText(strings.Repeat("r\u0301", 500) + "\x1b]8;;file:///private/path\aopen\x1b]8;;\a"),
			Page:             artifactSafeText(injected),
			ConsumedBytes:    1,
		})
		for name, output := range map[string]string{"metadata": view.MetadataLine(), "page": view.Page().Text(), "view": view.View()} {
			if strings.ContainsAny(output, "\x1b\a\x00\x7f\u0085") || strings.Contains(output, "52;c;clipboard") || strings.Contains(output, "file:///private/path") {
				t.Fatalf("%s retained terminal controls: %q", name, output)
			}
		}
		if ansi.StringWidth(view.MetadataLine()) > artifactMetadataWidth || utf8.RuneCountInString(view.MetadataLine()) > artifactMetadataWidth {
			t.Fatalf("metadata exceeded fixed bounds: width=%d runes=%d", ansi.StringWidth(view.MetadataLine()), utf8.RuneCountInString(view.MetadataLine()))
		}
		if utf8.RuneCountInString(view.Page().Text()) > artifactPageRunes {
			t.Fatalf("page exceeded rune bound: %d", utf8.RuneCountInString(view.Page().Text()))
		}
		lines := strings.Split(view.Page().Text(), "\n")
		if len(lines) > artifactPageLines {
			t.Fatalf("page exceeded line bound: %d", len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > artifactPageLineWidth {
				t.Fatalf("page line exceeded width bound: %d", ansi.StringWidth(line))
			}
		}
	})

	t.Run("paging emits only strict value intents", func(t *testing.T) {
		id := strings.Repeat("e", 64)
		view := NewArtifactView(ArtifactViewSpec{
			ID: id, Bytes: 4 * artifactPageBytes, Available: true, Complete: true,
			Page: artifactSafeText("safe page"), Offset: artifactPageBytes, ConsumedBytes: artifactPageBytes, HasMore: true,
		})
		next, nextOK := view.NextIntent()
		previous, previousOK := view.PreviousIntent()
		if !nextOK || next.ID() != id || next.Offset() != 2*artifactPageBytes {
			t.Fatalf("unexpected next intent: %#v ok=%t", next, nextOK)
		}
		if !previousOK || previous.ID() != id || previous.Offset() != 0 {
			t.Fatalf("unexpected previous intent: %#v ok=%t", previous, previousOK)
		}
		last := NewArtifactView(ArtifactViewSpec{ID: id, Bytes: 3*artifactPageBytes + 512, Available: true, Offset: 3 * artifactPageBytes, ConsumedBytes: 512})
		if intent, ok := last.NextIntent(); ok || intent.ID() != "" {
			t.Fatalf("last page emitted next intent: %#v", intent)
		}
		overflowOffset := math.MaxInt64 - math.MaxInt64%artifactPageBytes
		inconsistent := NewArtifactView(ArtifactViewSpec{
			ID: id, Bytes: math.MaxInt64, Available: true, Offset: overflowOffset, ConsumedBytes: artifactPageBytes, HasMore: true,
		})
		if inconsistent.Available() || inconsistent.HasMore() {
			t.Fatal("offset plus consumed bytes exceeding total bytes remained available")
		}
		if _, ok := inconsistent.NextIntent(); ok {
			t.Fatal("overflowing page emitted next intent")
		}
	})

	t.Run("real messages use metadata only and advertise the local command", func(t *testing.T) {
		id := strings.Repeat("f", 64)
		messages := NewMessagesView(false)
		messages.UpsertTool(events.ToolDisplay{
			CallID: "call-1", Name: "Bash", Status: events.ToolDisplaySuccess,
			Artifact:  &events.ArtifactRef{ID: id, Bytes: 123, Available: true, Complete: false},
			Truncated: true, TruncationReason: artifactSafeText("inline_preview_limit"),
		})
		output := messages.View()
		for _, want := range []string{id, "123 bytes", "Available", "Incomplete", "inline_preview_limit", "/artifact " + id} {
			if !strings.Contains(output, want) {
				t.Fatalf("regular tool line missing %q: %q", want, output)
			}
		}
		for _, forbidden := range []string{"raw-artifact-canary", "/private/artifacts", "dedicated-page-canary"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("regular tool line exposed %q: %q", forbidden, output)
			}
		}

		messages = NewMessagesView(false)
		messages.UpsertTool(events.ToolDisplay{
			CallID: "call-2", Name: "Bash", Status: events.ToolDisplayError,
			Artifact: &events.ArtifactRef{ID: "/private/artifacts/raw.artifact", Bytes: 9, Available: true},
		})
		output = messages.View()
		if strings.Contains(output, "/private/artifacts") || strings.Contains(output, "/artifact /private") {
			t.Fatalf("invalid artifact ID reached regular messages: %q", output)
		}
	})
}

func artifactSafeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func assertArtifactValueIsCapabilityFree(t *testing.T, valueType reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[valueType] {
		return
	}
	seen[valueType] = true
	for valueType.Kind() == reflect.Array {
		valueType = valueType.Elem()
	}
	switch valueType.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.UnsafePointer, reflect.Func, reflect.Chan, reflect.Map:
		t.Fatalf("artifact display value exposes capability-bearing type %v", valueType)
	case reflect.Slice:
		if valueType.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("artifact display value exposes raw bytes through %v", valueType)
		}
		t.Fatalf("artifact display value exposes mutable slice %v", valueType)
	}
	if strings.Contains(valueType.PkgPath(), "/artifact") || valueType.PkgPath() == "io" {
		t.Fatalf("artifact display value imports forbidden domain/capability type %v", valueType)
	}
	if valueType.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		name := strings.ToLower(field.Name)
		for _, forbidden := range []string{"path", "reader", "store", "callback"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("%v.%s exposes forbidden %s state", valueType, field.Name, forbidden)
			}
		}
		assertArtifactValueIsCapabilityFree(t, field.Type, seen)
	}
}
