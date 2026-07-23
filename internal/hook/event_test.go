package hook

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testFactory(t *testing.T, limits Limits) *eventFactory {
	t.Helper()
	ids := []string{"e", "t", "m"}
	index := 0
	factory, err := newEventFactory(t.TempDir(), limits, func() time.Time { return time.Date(2026, 7, 23, 1, 2, 3, 4, time.FixedZone("x", 8*3600)) }, func() string { value := ids[index%len(ids)]; index++; return value })
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestEventJSONOmission(t *testing.T) {
	f := testFactory(t, Limits{})
	event := f.freeze(f.base(EventSystemStart))
	payload, ok := event.JSON()
	if !ok {
		t.Fatal("payload unavailable")
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"execution", "session", "turn", "message", "tool", "compact"} {
		if _, exists := object[key]; exists {
			t.Fatalf("unexpected %s", key)
		}
	}
	if object["schema_version"].(float64) != 1 || object["event"] != "system_start" {
		t.Fatalf("payload = %s", payload)
	}
}

func TestEventSequenceAndClock(t *testing.T) {
	f := testFactory(t, Limits{})
	const count = 100
	sequences := make(chan uint64, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() { defer group.Done(); sequences <- f.freeze(f.base(EventSystemStart)).value.Sequence }()
	}
	group.Wait()
	close(sequences)
	seen := map[uint64]bool{}
	for sequence := range sequences {
		if sequence == 0 || seen[sequence] {
			t.Fatalf("duplicate sequence %d", sequence)
		}
		seen[sequence] = true
	}
	event := f.freeze(f.base(EventSystemStart)).value
	if event.OccurredAt != "2026-07-22T17:02:03.000000004Z" || !filepath.IsAbs(event.Project.Root) {
		t.Fatalf("event = %#v", event)
	}
}

func TestEventSnapshotImmutability(t *testing.T) {
	f := testFactory(t, Limits{})
	arguments := map[string]any{"number": json.Number("1.0"), "nested": map[string]any{"value": "original"}, "items": []any{"a"}}
	c := f.executionBase(EventToolBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	c.Tool = &ToolContext{CallID: "c", Name: "Bash", Arguments: arguments}
	event := f.freeze(c)
	originalJSON, ok := event.JSON()
	if !ok {
		t.Fatal("payload unavailable")
	}
	arguments["number"] = "changed"
	arguments["nested"].(map[string]any)["value"] = "changed"
	arguments["items"].([]any)[0] = "changed"
	copy := event.Context()
	copy.Tool.Arguments["nested"].(map[string]any)["value"] = "again"
	field, err := compileField(EventToolBefore, "tool.arguments.nested.value")
	if err != nil {
		t.Fatal(err)
	}
	if got := event.lookup(field); !got.exists || got.value != "original" {
		t.Fatalf("snapshot mutated: %#v", got)
	}
	afterJSON, ok := event.JSON()
	if !ok || string(afterJSON) != string(originalJSON) {
		t.Fatal("JSON snapshot mutated")
	}
}

func TestEventJSONLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.EventJSONBytes = 256
	f := testFactory(t, limits)
	c := f.executionBase(EventMessageBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	c.Message = &MessageContext{ID: "m", Role: MessageUser, Content: string(make([]byte, 300))}
	event := f.freeze(c)
	if _, ok := event.JSON(); ok {
		t.Fatal("oversize payload available")
	}
	field, _ := compileField(EventMessageBefore, "message.content")
	if value := event.lookup(field); !value.exists || len(value.value.(string)) != 300 {
		t.Fatal("in-memory lookup lost")
	}
}

func TestEventJSONExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	limits.EventJSONBytes = 512
	f := testFactory(t, limits)
	c := f.executionBase(EventMessageBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	c.Message = &MessageContext{ID: "m", Role: MessageUser}
	baseline, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	padding := limits.EventJSONBytes - len(baseline)
	if padding <= 0 {
		t.Fatalf("baseline is %d bytes", len(baseline))
	}
	c.Message.Content = strings.Repeat("x", padding)
	exact := f.freeze(c)
	payload, ok := exact.JSON()
	if !ok || len(payload) != limits.EventJSONBytes {
		t.Fatalf("exact payload = %d,%v", len(payload), ok)
	}

	c.Message.Content += "x"
	over := f.freeze(c)
	if payload, ok := over.JSON(); ok || payload != nil {
		t.Fatalf("limit+1 payload exposed: %d,%v", len(payload), ok)
	}
	if over.json != nil {
		t.Fatalf("oversize payload allocated %d bytes", len(over.json))
	}
}

func TestOversizeEventRetainsScalarLookupWithBoundedSnapshot(t *testing.T) {
	limits := DefaultLimits()
	limits.EventJSONBytes = 256
	f := testFactory(t, limits)
	huge := strings.Repeat("z", 8<<20)
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}

	messageContext := f.executionBase(EventMessageBefore, ref)
	messageContext.Message = &MessageContext{ID: "m", Role: MessageUser, Content: huge}
	message := f.freeze(messageContext)
	messageField, err := compileField(EventMessageBefore, "message.content")
	if err != nil {
		t.Fatal(err)
	}
	messageValue := message.lookup(messageField)
	if !messageValue.exists || messageValue.kind != scalarString || len(messageValue.value.(string)) != len(huge) {
		t.Fatal("oversize message scalar unavailable")
	}
	if message.json != nil {
		t.Fatalf("oversize message allocated %d payload bytes", len(message.json))
	}

	toolContext := f.executionBase(EventToolBefore, ref)
	toolContext.Tool = &ToolContext{CallID: "c", Name: "Read", Arguments: map[string]any{
		"nested": map[string]any{"value": huge},
		"number": json.Number("90071992547409931234567890.125"),
	}}
	tool := f.freeze(toolContext)
	toolField, err := compileField(EventToolBefore, "tool.arguments.nested.value")
	if err != nil {
		t.Fatal(err)
	}
	toolValue := tool.lookup(toolField)
	if !toolValue.exists || toolValue.kind != scalarString || len(toolValue.value.(string)) != len(huge) {
		t.Fatal("oversize tool scalar unavailable")
	}
	numberField, err := compileField(EventToolBefore, "tool.arguments.number")
	if err != nil {
		t.Fatal(err)
	}
	numberValue := tool.lookup(numberField)
	if !numberValue.exists || numberValue.kind != scalarNumber || numberValue.value.(numberScalar).text != "90071992547409931234567890.125" {
		t.Fatalf("number precision changed: %#v", numberValue)
	}
	if tool.json != nil {
		t.Fatalf("oversize tool allocated %d payload bytes", len(tool.json))
	}
}

func TestEventJSONPreflightMatchesEncodingJSON(t *testing.T) {
	f := testFactory(t, Limits{})
	duration := int64(0)
	c := f.executionBase(EventToolAfter, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModePlan})
	c.Tool = &ToolContext{
		CallID: "c<&\n", Name: "Read\u2028", Status: ToolSuccess, DurationMS: &duration,
		Arguments: map[string]any{
			"string": "quote\" slash\\ tab\t <>& \u2029",
			"number": json.Number("1.2300e+12"),
			"bool":   true,
			"nil":    nil,
			"object": map[string]any{"x": "y"},
			"array":  []any{"x", json.Number("2"), false},
			"bytes":  []byte{0, 1, 2, 3},
		},
		Result: &ToolResultContext{Content: "result", Error: &ToolResultError{Code: "x", Message: "bad", Recoverable: true}},
	}
	want, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, valid := eventJSONSize(c, len(want)+100)
	if !valid || got != len(want) {
		t.Fatalf("preflight = %d,%v; marshal = %d", got, valid, len(want))
	}
	if payload, ok := f.freeze(c).JSON(); !ok || string(payload) != string(want) {
		t.Fatalf("payload mismatch: %q", payload)
	}
}

func TestEventJSONSizeDifferential(t *testing.T) {
	f := testFactory(t, Limits{})
	invalidUTF8 := string([]byte{'a', 0xff, 'b', 0xfe})
	values := []struct {
		name  string
		value any
	}{
		{name: "escaped ascii", value: "quote\" slash\\ controls\b\f\n\r\t <>&"},
		{name: "utf8", value: "中文🙂\u2028\u2029"},
		{name: "invalid utf8", value: invalidUTF8},
		{name: "nil", value: nil},
		{name: "nil map", value: map[string]any(nil)},
		{name: "nil array", value: []any(nil)},
		{name: "nested", value: map[string]any{"invalid\xff": []any{"x", nil, map[string]any{"y": true}}}},
		{name: "int", value: int64(-9223372036854775807)},
		{name: "uint", value: uint64(18446744073709551615)},
		{name: "float32", value: float32(1.234567)},
		{name: "float64", value: 1.2345678901234567e-120},
		{name: "number", value: json.Number("90071992547409931234567890.125e-10")},
		{name: "bytes", value: []byte{0, 1, 2, 253, 254, 255}},
	}
	for _, item := range values {
		t.Run(item.name, func(t *testing.T) {
			c := f.executionBase(EventToolBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
			c.Tool = &ToolContext{CallID: invalidUTF8, Name: "Read\u2028<&", Arguments: map[string]any{"value": item.value}}
			want, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			got, valid := eventJSONSize(c, len(want))
			if !valid || got != len(want) {
				t.Fatalf("preflight = %d,%v; marshal = %d (%s)", got, valid, len(want), want)
			}
			if len(want) > 0 {
				over, valid := eventJSONSize(c, len(want)-1)
				if !valid || over != len(want) {
					t.Fatalf("limit+1 preflight = %d,%v; marshal = %d", over, valid, len(want))
				}
			}
		})
	}
}

func TestEventObjectPresenceMatrix(t *testing.T) {
	f := testFactory(t, Limits{})
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}
	zeroDuration := int64(0)
	cases := []struct {
		event Event
		build func() EventContext
	}{
		{EventSystemStart, func() EventContext { return f.base(EventSystemStart) }},
		{EventSystemStop, func() EventContext { return f.base(EventSystemStop) }},
		{EventSessionStart, func() EventContext {
			c := f.base(EventSessionStart)
			c.Session = &SessionContext{ID: "s", State: SessionNew}
			return c
		}},
		{EventSessionEnd, func() EventContext {
			c := f.base(EventSessionEnd)
			c.Session = &SessionContext{ID: "s", EndReason: SessionEndExit}
			return c
		}},
		{EventTurnStart, func() EventContext { return f.executionBase(EventTurnStart, ref) }},
		{EventTurnEnd, func() EventContext { c := f.executionBase(EventTurnEnd, ref); c.Turn.Status = TurnCompleted; return c }},
		{EventMessageBefore, func() EventContext {
			c := f.executionBase(EventMessageBefore, ref)
			c.Message = &MessageContext{ID: "m", Role: MessageUser, Content: "x"}
			return c
		}},
		{EventMessageAfter, func() EventContext {
			c := f.executionBase(EventMessageAfter, ref)
			c.Message = &MessageContext{ID: "m", Role: MessageUser, Content: "x"}
			return c
		}},
		{EventToolBefore, func() EventContext {
			c := f.executionBase(EventToolBefore, ref)
			c.Tool = &ToolContext{CallID: "c", Name: "Read", Arguments: map[string]any{}}
			return c
		}},
		{EventToolAfter, func() EventContext {
			c := f.executionBase(EventToolAfter, ref)
			c.Tool = &ToolContext{CallID: "c", Name: "Read", Arguments: map[string]any{}, Status: ToolSuccess, DurationMS: &zeroDuration, Result: &ToolResultContext{}}
			return c
		}},
		{EventCompactBefore, func() EventContext {
			c := f.executionBase(EventCompactBefore, ref)
			c.Compact = &CompactContext{Reason: CompactAuto, Before: CompactStats{Messages: 1}}
			return c
		}},
		{EventCompactAfter, func() EventContext {
			c := f.executionBase(EventCompactAfter, ref)
			c.Compact = &CompactContext{Reason: CompactAuto, Before: CompactStats{Messages: 1}, Status: CompactSuccess}
			return c
		}},
	}
	for _, item := range cases {
		t.Run(string(item.event), func(t *testing.T) {
			frozen := f.freeze(item.build())
			value := frozen.Context()
			if value.Event != item.event || value.Sequence == 0 {
				t.Fatal("common fields")
			}
			if (item.event == EventMessageBefore || item.event == EventMessageAfter) != (value.Message != nil) {
				t.Fatal("message presence")
			}
			if (item.event == EventToolBefore || item.event == EventToolAfter) != (value.Tool != nil) {
				t.Fatal("tool presence")
			}
			if item.event == EventToolBefore || item.event == EventToolAfter {
				payload, ok := frozen.JSON()
				if !ok {
					t.Fatal("tool payload unavailable")
				}
				var object map[string]any
				if err := json.Unmarshal(payload, &object); err != nil {
					t.Fatal(err)
				}
				toolObject, ok := object["tool"].(map[string]any)
				if !ok {
					t.Fatalf("tool JSON = %#v", object["tool"])
				}
				for _, key := range []string{"status", "duration_ms", "result"} {
					_, exists := toolObject[key]
					if exists != (item.event == EventToolAfter) {
						t.Fatalf("%s presence for %s = %v", key, item.event, exists)
					}
				}
			}
		})
	}
}
