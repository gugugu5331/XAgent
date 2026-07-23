package hook

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestCompileTemplate(t *testing.T) {
	for _, source := range []string{"{{tool.arguments.path}}", "before {{tool.name}} after"} {
		if _, err := compileTemplate(EventToolBefore, source, 1024); err != nil {
			t.Fatalf("%q: %v", source, err)
		}
	}
	for _, source := range []string{"{{ tool.name }}", "{{tool.name|default}}", "{{tool.result.content}}", "{{tool.name", "tool.name}}"} {
		if _, err := compileTemplate(EventToolBefore, source, 1024); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	event := testToolEvent(t)
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("secret-canary")
	event.value.Tool.Arguments["secret"] = "secret-canary"
	event = frozenFromContext(t, event.value)
	template, err := compileTemplate(EventToolBefore, "{{tool.arguments.n}}/{{tool.arguments.flag}}/{{tool.arguments.secret}}", 1024)
	if err != nil {
		t.Fatal(err)
	}
	got, err := template.render(event, 1024, runtimeRedactor)
	if err != nil || got != "1/true/[redacted]" {
		t.Fatalf("got %q,%v", got, err)
	}
	missing, _ := compileTemplate(EventToolBefore, "prefix{{tool.arguments.missing}}", 1024)
	if value, err := missing.render(event, 1024, nil); err == nil || value != "" {
		t.Fatalf("partial render %q,%v", value, err)
	}
}

func TestRenderTemplateChecksDynamicFragmentBeforeWrite(t *testing.T) {
	limits := DefaultLimits()
	limits.EventJSONBytes = 128
	f := testFactory(t, limits)
	c := f.executionBase(EventToolBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	c.Tool = &ToolContext{CallID: "c", Name: "Read", Arguments: map[string]any{"value": strings.Repeat("x", 8<<20)}}
	template, err := compileTemplate(EventToolBefore, "prefix:{{tool.arguments.value}}", 128)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := template.render(f.freeze(c), 32, nil); err == nil || value != "" {
		t.Fatalf("oversize render = %q,%v", value, err)
	}

	c.Tool.Arguments = map[string]any{"value": json.Number("1e1000000")}
	numeric := f.freeze(c)
	if value, err := template.render(numeric, 32, nil); err == nil || value != "" {
		t.Fatalf("expanded numeric render = %q,%v", value, err)
	}
}

func TestAppendTemplateFragmentRejectsBeforeWrite(t *testing.T) {
	var output bytes.Buffer
	output.WriteString("prefix")
	before := append([]byte(nil), output.Bytes()...)
	if err := appendTemplateFragment(&output, strings.Repeat("x", 1024), 16); err == nil {
		t.Fatal("oversize fragment accepted")
	}
	if !bytes.Equal(output.Bytes(), before) {
		t.Fatalf("buffer changed before rejection: %q", output.Bytes())
	}
	if err := appendTemplateFragment(&output, strings.Repeat("x", 10), 16); err != nil || output.Len() != 16 {
		t.Fatalf("exact fragment = %d,%v", output.Len(), err)
	}
}

func TestCanonicalDecimalBoundedChecksExponentBeforeExpansion(t *testing.T) {
	if value, ok := canonicalDecimalBounded("1e1000000", 64); ok || value != "" {
		t.Fatalf("oversize exponent expanded: %q,%v", value, ok)
	}
	if value, ok := canonicalDecimalBounded("1.2300e2", 3); !ok || value != "123" {
		t.Fatalf("bounded decimal = %q,%v", value, ok)
	}
	if value, ok := canonicalDecimalBounded("1e+2", 3); !ok || value != "100" {
		t.Fatalf("positive exponent = %q,%v", value, ok)
	}
	if value, ok := canonicalDecimalBounded("1.2300e2", 2); ok || value != "" {
		t.Fatalf("over-limit decimal = %q,%v", value, ok)
	}
}

func TestCanonicalDecimalBoundedHugeMantissa(t *testing.T) {
	const zeroCount = 8 << 20
	compressedOne := "1" + strings.Repeat("0", zeroCount) + "e-" + strconv.Itoa(zeroCount)
	if value, ok := canonicalDecimalBounded(compressedOne, 1); !ok || value != "1" {
		t.Fatalf("compressed huge mantissa = %q,%v", value, ok)
	}
	if value, ok := canonicalDecimalBounded(strings.Repeat("1", zeroCount), 64); ok || value != "" {
		t.Fatalf("significant huge mantissa rendered %d bytes,%v", len(value), ok)
	}
}

func frozenFromContext(t *testing.T, c EventContext) *frozenEvent {
	t.Helper()
	return testFactory(t, Limits{}).freeze(c)
}
