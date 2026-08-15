package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestRequestBudgeterIsClosedConcreteValue(t *testing.T) {
	typeOfBudgeter := reflect.TypeOf(RequestBudgeter{})
	if typeOfBudgeter.Kind() != reflect.Struct {
		t.Fatalf("RequestBudgeter kind = %s, want struct", typeOfBudgeter.Kind())
	}
	if typeOfBudgeter.NumField() != 2 {
		t.Fatalf("RequestBudgeter field count = %d, want version and validity marker", typeOfBudgeter.NumField())
	}
	for index := 0; index < typeOfBudgeter.NumField(); index++ {
		field := typeOfBudgeter.Field(index)
		if field.IsExported() {
			t.Fatalf("RequestBudgeter field %s is exported", field.Name)
		}
		if field.Type.Kind() != reflect.Uint8 && field.Type.Kind() != reflect.Uint64 {
			t.Fatalf("RequestBudgeter field %s has mutable/capability type %v", field.Name, field.Type)
		}
	}
	constructed := NewRequestBudgeter()
	if !constructed.valid() {
		t.Fatal("NewRequestBudgeter returned an invalid value")
	}
	if (RequestBudgeter{}).valid() {
		t.Fatal("zero RequestBudgeter is valid")
	}
	if _, err := (RequestBudgeter{}).measureBytes(1); err == nil {
		t.Fatal("zero RequestBudgeter performed measurement")
	}

	file, err := parser.ParseFile(token.NewFileSet(), "manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse manager.go: %v", err)
	}
	constructors := 0
	compositesOutsideConstructor := 0
	budgeterTypes := 0
	for _, declaration := range file.Decls {
		if general, ok := declaration.(*ast.GenDecl); ok {
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || !strings.Contains(typeSpec.Name.Name, "RequestBudgeter") {
					continue
				}
				budgeterTypes++
				if typeSpec.Name.Name != "RequestBudgeter" {
					t.Errorf("parallel budgeter type %s is forbidden", typeSpec.Name.Name)
				}
				if _, ok := typeSpec.Type.(*ast.StructType); !ok {
					t.Errorf("RequestBudgeter is replaceable through %T", typeSpec.Type)
				}
			}
		}
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		returnsBudgeter := false
		if function.Type.Results != nil {
			for _, field := range function.Type.Results.List {
				if identifier, ok := field.Type.(*ast.Ident); ok && identifier.Name == "RequestBudgeter" {
					returnsBudgeter = true
				}
			}
		}
		if returnsBudgeter {
			constructors++
			if function.Name.Name != "NewRequestBudgeter" || function.Recv != nil || function.Type.Params.NumFields() != 0 {
				t.Errorf("unexpected RequestBudgeter constructor %s", function.Name.Name)
			}
		}
		if function.Recv != nil {
			for _, receiver := range function.Recv.List {
				if pointer, ok := receiver.Type.(*ast.StarExpr); ok {
					if identifier, ok := pointer.X.(*ast.Ident); ok && identifier.Name == "RequestBudgeter" {
						t.Errorf("RequestBudgeter has mutable pointer method %s", function.Name.Name)
					}
				}
			}
		}
		if function.Body == nil || function.Name.Name == "NewRequestBudgeter" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if identifier, ok := literal.Type.(*ast.Ident); ok && identifier.Name == "RequestBudgeter" {
				compositesOutsideConstructor++
			}
			return true
		})
	}
	if budgeterTypes != 1 || constructors != 1 || compositesOutsideConstructor != 0 {
		t.Fatalf("RequestBudgeter types=%d constructors=%d composites outside constructor=%d, want 1,1,0", budgeterTypes, constructors, compositesOutsideConstructor)
	}
}

func TestPlanningTokensAreCeilBytesOverFour(t *testing.T) {
	budgeter := NewRequestBudgeter()
	cases := []struct {
		bytes int64
		want  int64
	}{
		{bytes: 0, want: 0},
		{bytes: 1, want: 1},
		{bytes: 2, want: 1},
		{bytes: 3, want: 1},
		{bytes: 4, want: 1},
		{bytes: 5, want: 2},
		{bytes: 7, want: 2},
		{bytes: 8, want: 2},
		{bytes: 9, want: 3},
		{bytes: math.MaxInt64, want: math.MaxInt64/4 + 1},
	}
	for _, testCase := range cases {
		measure, err := budgeter.measureBytes(testCase.bytes)
		if err != nil {
			t.Fatalf("measure %d bytes: %v", testCase.bytes, err)
		}
		if measure.Bytes != testCase.bytes || measure.PlanningTokens != testCase.want {
			t.Fatalf("measureBytes(%d) = %#v, want tokens=%d", testCase.bytes, measure, testCase.want)
		}
	}
	if measure, err := budgeter.measureBytes(-1); err == nil || measure != (RequestMeasure{}) {
		t.Fatalf("negative byte measure = (%#v,%v), want zero,error", measure, err)
	}
}

func TestRequestMeasureArithmeticFailsClosed(t *testing.T) {
	budgeter := NewRequestBudgeter()
	left := RequestMeasure{Bytes: 7, PlanningTokens: 2}
	right := RequestMeasure{Bytes: 5, PlanningTokens: 2}
	if got, err := budgeter.addMeasures(left, right); err != nil || got != (RequestMeasure{Bytes: 12, PlanningTokens: 4}) {
		t.Fatalf("add measures = (%#v,%v)", got, err)
	}
	if got, err := budgeter.multiplyMeasure(left, 3); err != nil || got != (RequestMeasure{Bytes: 21, PlanningTokens: 6}) {
		t.Fatalf("multiply measure = (%#v,%v)", got, err)
	}
	if got, err := budgeter.multiplyMeasure(left, 0); err != nil || got != (RequestMeasure{}) {
		t.Fatalf("multiply by zero = (%#v,%v)", got, err)
	}

	badAdds := [][2]RequestMeasure{
		{{Bytes: -1}, {}},
		{{PlanningTokens: -1}, {}},
		{{Bytes: math.MaxInt64}, {Bytes: 1}},
		{{PlanningTokens: math.MaxInt64}, {PlanningTokens: 1}},
	}
	for _, pair := range badAdds {
		if got, err := budgeter.addMeasures(pair[0], pair[1]); err == nil || got != (RequestMeasure{}) {
			t.Fatalf("invalid add %#v + %#v = (%#v,%v), want zero,error", pair[0], pair[1], got, err)
		}
	}
	badMultiplies := []struct {
		measure RequestMeasure
		factor  int64
	}{
		{measure: RequestMeasure{Bytes: -1}, factor: 1},
		{measure: RequestMeasure{PlanningTokens: -1}, factor: 1},
		{measure: RequestMeasure{Bytes: 1}, factor: -1},
		{measure: RequestMeasure{Bytes: math.MaxInt64}, factor: 2},
		{measure: RequestMeasure{PlanningTokens: math.MaxInt64}, factor: 2},
	}
	for _, testCase := range badMultiplies {
		if got, err := budgeter.multiplyMeasure(testCase.measure, testCase.factor); err == nil || got != (RequestMeasure{}) {
			t.Fatalf("invalid multiply %#v * %d = (%#v,%v), want zero,error", testCase.measure, testCase.factor, got, err)
		}
	}
	if got, err := (RequestBudgeter{}).addMeasures(left, right); err == nil || got != (RequestMeasure{}) {
		t.Fatalf("zero budgeter add = (%#v,%v), want zero,error", got, err)
	}
	if got, err := (RequestBudgeter{}).multiplyMeasure(left, 2); err == nil || got != (RequestMeasure{}) {
		t.Fatalf("zero budgeter multiply = (%#v,%v), want zero,error", got, err)
	}
}

func TestRequestByteBoundUsesWorstCaseJSONEncoding(t *testing.T) {
	budgeter := NewRequestBudgeter()
	stringsToMeasure := []string{"", "a", "中", "🙂", "e\u0301", string([]byte{0xff, 'x'})}
	for _, value := range stringsToMeasure {
		measure, err := budgeter.measureJSONValue(context.Background(), value)
		if err != nil {
			t.Fatalf("measure JSON string %q: %v", value, err)
		}
		want := int64(2 + 6*len(value))
		if measure.Bytes != want {
			t.Fatalf("JSON string byte bound for %q = %d, want %d", value, measure.Bytes, want)
		}
	}

	values := []any{
		nil,
		true,
		false,
		int64(math.MinInt64),
		uint64(math.MaxUint64),
		json.Number("-123.456e+78"),
		[]any{"enum", true, int64(7)},
		map[string]any{"text": "中", "enabled": false, "count": int64(9)},
		tool.Schema{Type: "object", Required: []string{"path"}, Properties: map[string]tool.SchemaProperty{
			"path": {Type: "string", Description: "文件路径", Enum: []string{"a", "中"}},
		}},
		tool.Schema{Raw: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)},
	}
	for _, value := range values {
		measure, err := budgeter.measureJSONValue(context.Background(), value)
		if err != nil {
			t.Fatalf("measure JSON value %T: %v", value, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal comparison value %T: %v", value, err)
		}
		if measure.Bytes < int64(len(encoded)) {
			t.Fatalf("JSON bound for %T = %d, serialized=%d", value, measure.Bytes, len(encoded))
		}
		if measure.PlanningTokens != measure.Bytes/4+boolInt64(measure.Bytes%4 != 0) {
			t.Fatalf("JSON measure has inconsistent planning tokens: %#v", measure)
		}
	}

	invalidValues := []any{json.Number("01"), math.NaN(), math.Inf(1), make(chan int), tool.Schema{Raw: json.RawMessage(`{"type":`)}}
	for _, value := range invalidValues {
		if measure, err := budgeter.measureJSONValue(context.Background(), value); err == nil || measure != (RequestMeasure{}) {
			t.Fatalf("invalid JSON value %T measured as (%#v,%v), want zero,error", value, measure, err)
		}
	}
}

func TestRequestBytePrimitivesDoNotAllocateWholeInputCopy(t *testing.T) {
	budgeter := NewRequestBudgeter()
	small := strings.Repeat("s", 1024)
	large := strings.Repeat("l", 1<<20)
	allocated := func(value string) int64 {
		var last RequestMeasure
		result := testing.Benchmark(func(benchmark *testing.B) {
			for index := 0; index < benchmark.N; index++ {
				measure, err := budgeter.measureJSONValue(context.Background(), value)
				if err != nil {
					benchmark.Fatal(err)
				}
				last = measure
			}
		})
		if last.Bytes == 0 {
			t.Fatal("benchmark measurement was optimized away")
		}
		return result.AllocedBytesPerOp()
	}
	smallBytes := allocated(small)
	largeBytes := allocated(large)
	if largeBytes > smallBytes+4096 {
		t.Fatalf("large input allocated a proportional copy: small=%d large=%d", smallBytes, largeBytes)
	}
}

func TestRequestByteTraversalCancelsWithinFixedChunks(t *testing.T) {
	budgeter := NewRequestBudgeter()
	longContext := &cancelAfterChecksContext{cancelAt: 2}
	longValue := strings.Repeat("x", requestStringCheckBytes*3)
	if measure, err := budgeter.measureJSONValue(longContext, longValue); !errors.Is(err, context.Canceled) || measure != (RequestMeasure{}) {
		t.Fatalf("long string cancellation = (%#v,%v), checks=%d", measure, err, longContext.checks)
	}
	if longContext.checks != 2 {
		t.Fatalf("long string cancellation checks = %d, want 2", longContext.checks)
	}

	properties := make(map[string]tool.SchemaProperty, requestSchemaNodeCheckCount*2+8)
	for index := 0; index < requestSchemaNodeCheckCount*2+8; index++ {
		properties[string(rune(0x1000+index))] = tool.SchemaProperty{Type: "string"}
	}
	schemaContext := &cancelAfterChecksContext{cancelAt: requestSchemaNodeCheckCount + 2}
	if measure, err := budgeter.measureJSONValue(schemaContext, tool.Schema{Type: "object", Properties: properties}); !errors.Is(err, context.Canceled) || measure != (RequestMeasure{}) {
		t.Fatalf("schema cancellation = (%#v,%v), checks=%d", measure, err, schemaContext.checks)
	}
	if schemaContext.checks > requestSchemaNodeCheckCount+2 {
		t.Fatalf("schema cancellation exceeded fixed node checks: %d", schemaContext.checks)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if measure, err := budgeter.measureJSONValue(canceled, "small"); !errors.Is(err, context.Canceled) || measure != (RequestMeasure{}) {
		t.Fatalf("pre-canceled measurement = (%#v,%v), want zero,canceled", measure, err)
	}
}

func TestMeasureRequestCoversSystemModelInputHistoryThinkingAndCache(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	request := provider.ChatRequest{
		Model: "effective-model",
		StableSystem: []provider.SystemBlock{
			{Name: "stable", Content: redactor.Redact("stable system"), Cacheable: true},
			{Name: "skill-sop", Content: redactor.Redact("approved skill SOP"), Cacheable: true},
		},
		DynamicSystem: []provider.SystemBlock{{Name: "dynamic", Content: redactor.Redact("dynamic system")}},
		Messages: []provider.ModelMessage{
			{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("selected history input")},
			{Role: provider.ModelMessageRoleAssistant, Content: redactor.Redact("selected history response")},
			{Role: provider.ModelMessageRoleContextSummary, Content: redactor.Redact("safe summary")},
			{Role: provider.ModelMessageRoleContextBoundary, Content: redactor.Redact("safe boundary")},
			{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("current input")},
		},
		Thinking: config.ThinkingConfig{Enabled: true, Show: true, BudgetTokens: 4096},
		Cache: provider.CachePolicy{
			EnablePromptCache:    true,
			SystemBreakpointName: "skill-sop",
			CacheTools:           true,
		},
	}
	full, err := budgeter.MeasureRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("measure full request: %v", err)
	}
	if full.Bytes <= 0 || full.PlanningTokens != full.Bytes/4+boolInt64(full.Bytes%4 != 0) {
		t.Fatalf("full request measure = %#v", full)
	}

	mutations := []struct {
		name   string
		mutate func(*provider.ChatRequest)
	}{
		{name: "model", mutate: func(value *provider.ChatRequest) { value.Model = "" }},
		{name: "stable and skill SOP", mutate: func(value *provider.ChatRequest) { value.StableSystem = nil }},
		{name: "dynamic system", mutate: func(value *provider.ChatRequest) { value.DynamicSystem = nil }},
		{name: "input and history", mutate: func(value *provider.ChatRequest) { value.Messages = nil }},
		{name: "thinking", mutate: func(value *provider.ChatRequest) { value.Thinking = config.ThinkingConfig{} }},
		{name: "cache", mutate: func(value *provider.ChatRequest) { value.Cache = provider.CachePolicy{} }},
	}
	for _, mutation := range mutations {
		candidate := request
		mutation.mutate(&candidate)
		measured, measureErr := budgeter.MeasureRequest(context.Background(), candidate)
		if measureErr != nil {
			t.Fatalf("measure request without %s: %v", mutation.name, measureErr)
		}
		if measured == full {
			t.Fatalf("request field %s did not affect canonical measure %#v", mutation.name, measured)
		}
	}
}

func TestMeasureRequestCanonicalizesNilEmptyAndFieldOrder(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	emptyNil := provider.ChatRequest{}
	emptySlices := provider.ChatRequest{
		System:        []provider.SystemBlock{},
		StableSystem:  []provider.SystemBlock{},
		DynamicSystem: []provider.SystemBlock{},
		Messages:      []provider.ModelMessage{},
		Tools:         []provider.ToolDefinition{},
	}
	nilMeasure, err := budgeter.MeasureRequest(context.Background(), emptyNil)
	if err != nil {
		t.Fatalf("measure nil request: %v", err)
	}
	emptyMeasure, err := budgeter.MeasureRequest(context.Background(), emptySlices)
	if err != nil {
		t.Fatalf("measure empty request: %v", err)
	}
	if nilMeasure != emptyMeasure {
		t.Fatalf("nil request measure %#v != empty request measure %#v", nilMeasure, emptyMeasure)
	}

	ordered := provider.ChatRequest{
		Model: "  model-x  ",
		System: []provider.SystemBlock{
			{Name: "  first  ", Content: redactor.Redact("one"), Cacheable: true},
			{Name: "ignored-empty", Content: redactor.Redact(" \t\n")},
			{Name: "second", Content: redactor.Redact("two")},
		},
		StableSystem:  []provider.SystemBlock{{Name: "legacy", Content: redactor.Redact("must be ignored")}},
		DynamicSystem: []provider.SystemBlock{{Name: "legacy-dynamic", Content: redactor.Redact("must also be ignored")}},
		Messages: []provider.ModelMessage{
			{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("one")},
			{Role: provider.ModelMessageRoleAssistant, Content: redactor.Redact("two")},
		},
		Cache: provider.CachePolicy{SystemBreakpointName: "  first  "},
	}
	canonical := ordered
	canonical.Model = "model-x"
	canonical.System = []provider.SystemBlock{
		{Name: "first", Content: redactor.Redact("one"), Cacheable: true},
		{Name: "second", Content: redactor.Redact("two")},
	}
	canonical.StableSystem = nil
	canonical.DynamicSystem = nil
	canonical.Cache.SystemBreakpointName = "first"
	orderedMeasure, err := budgeter.MeasureRequest(context.Background(), ordered)
	if err != nil {
		t.Fatalf("measure ordered request: %v", err)
	}
	canonicalMeasure, err := budgeter.MeasureRequest(context.Background(), canonical)
	if err != nil {
		t.Fatalf("measure canonical request: %v", err)
	}
	if orderedMeasure != canonicalMeasure {
		t.Fatalf("normalized request measure %#v != canonical measure %#v", orderedMeasure, canonicalMeasure)
	}

	reordered := canonical
	reordered.System = []provider.SystemBlock{canonical.System[1], canonical.System[0]}
	reordered.Messages = []provider.ModelMessage{canonical.Messages[1], canonical.Messages[0]}
	reorderedMeasure, err := budgeter.MeasureRequest(context.Background(), reordered)
	if err != nil {
		t.Fatalf("measure reordered request: %v", err)
	}
	if reorderedMeasure != canonicalMeasure {
		t.Fatalf("canonical field traversal changed byte total: reordered=%#v canonical=%#v", reorderedMeasure, canonicalMeasure)
	}
}

func TestMeasureRequestRejectsUnknownShape(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	requestType := reflect.TypeOf(provider.ChatRequest{})
	knownFields := map[string]struct{}{
		"Model": {}, "System": {}, "StableSystem": {}, "DynamicSystem": {}, "Messages": {},
		"Thinking": {}, "Tools": {}, "ToolDefs": {}, "Cache": {}, "Observer": {},
	}
	if requestType.NumField() != len(knownFields) {
		t.Fatalf("ChatRequest field count = %d, canonical budgeter knows %d", requestType.NumField(), len(knownFields))
	}
	for index := 0; index < requestType.NumField(); index++ {
		if _, known := knownFields[requestType.Field(index).Name]; !known {
			t.Fatalf("ChatRequest has unknown field %s", requestType.Field(index).Name)
		}
	}

	invalid := []provider.ChatRequest{
		{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRole("future"), Content: redactor.Redact("value")}}},
		{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("value"), ToolCallID: "unexpected"}}},
		{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleToolCall, ToolCallID: "call", ArgumentsJSON: redactor.Redact(`{}`)}}},
		{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleToolResult, ToolResultStatus: "success", ToolResult: redactor.Redact("value")}}},
		{Tools: []provider.ToolDefinition{{Name: "Read", Schema: tool.Schema{Raw: json.RawMessage(`{"type":`)}}}},
		{Thinking: config.ThinkingConfig{BudgetTokens: -1}},
	}
	for index, request := range invalid {
		if measure, measureErr := budgeter.MeasureRequest(context.Background(), request); measureErr == nil || measure != (RequestMeasure{}) {
			t.Fatalf("invalid request %d measured as (%#v,%v), want zero,error", index, measure, measureErr)
		}
	}
	if measure, err := budgeter.MeasureRequest(nil, provider.ChatRequest{}); err == nil || measure != (RequestMeasure{}) {
		t.Fatalf("nil context request measure = (%#v,%v), want zero,error", measure, err)
	}
	if measure, err := (RequestBudgeter{}).MeasureRequest(context.Background(), provider.ChatRequest{}); err == nil || measure != (RequestMeasure{}) {
		t.Fatalf("zero budgeter request measure = (%#v,%v), want zero,error", measure, err)
	}
}

func TestMeasureRequestCoversToolSchemaCallsResultsAndFraming(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatalf("new result factory: %v", err)
	}
	ref := &artifact.Ref{
		ID: strings.Repeat("a", 64), Bytes: 8192, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Available: true, Complete: true,
	}
	result, err := resultFactory.Build(tool.ResultFactoryInput{
		CallID: "call-1", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "safe summary", Preview: "safe preview", Artifact: ref, CapturedBytes: ref.Bytes,
		Truncated: true, TruncationReason: string(tool.CaptureTruncatedInline),
	})
	if err != nil {
		t.Fatalf("build projected result: %v", err)
	}
	request := provider.ChatRequest{
		Model: "model-with-tools",
		Messages: []provider.ModelMessage{
			{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("read a file")},
			{Role: provider.ModelMessageRoleToolCall, Content: redactor.Redact("safe call metadata"), ToolCallID: "call-1", ToolName: "Read", ArgumentsJSON: redactor.Redact(`{"path":"README.md"}`)},
			{Role: provider.ModelMessageRoleToolResult, Content: result.ModelContent(), ToolCallID: "call-1", ToolName: "Read", ToolResult: result.ModelContent(), ToolResultStatus: string(tool.StatusSuccess)},
			{Role: provider.ModelMessageRoleAssistant, Content: redactor.Redact("done")},
		},
		Tools: []provider.ToolDefinition{
			{Name: "Read", Description: "Read a file", Schema: tool.ObjectSchema([]string{"path"}, map[string]tool.SchemaProperty{
				"path": {Type: "string", Description: "path to read", Enum: []string{"README.md", "go.mod"}},
			})},
			{Name: "Nested", Description: "Nested raw schema", Schema: tool.Schema{Raw: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
		},
		Cache: provider.CachePolicy{EnablePromptCache: true, CacheTools: true},
	}
	full, err := budgeter.MeasureRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("measure request with tools: %v", err)
	}
	withoutTools := request
	withoutTools.Tools = nil
	withoutToolsMeasure, err := budgeter.MeasureRequest(context.Background(), withoutTools)
	if err != nil {
		t.Fatalf("measure request without definitions: %v", err)
	}
	withoutChain := request
	withoutChain.Messages = []provider.ModelMessage{request.Messages[0], request.Messages[3]}
	withoutChainMeasure, err := budgeter.MeasureRequest(context.Background(), withoutChain)
	if err != nil {
		t.Fatalf("measure request without tool chain: %v", err)
	}
	if full.Bytes <= withoutToolsMeasure.Bytes || full.Bytes <= withoutChainMeasure.Bytes {
		t.Fatalf("tool schema/chain not covered: full=%#v no-tools=%#v no-chain=%#v", full, withoutToolsMeasure, withoutChainMeasure)
	}
	if !strings.Contains(result.ModelContent().Text(), ref.ID) {
		t.Fatal("fixture model projection does not contain opaque artifact ref")
	}
}

func TestMeasureRequestNeverReadsArtifactPayload(t *testing.T) {
	method, ok := reflect.TypeOf(RequestBudgeter{}).MethodByName("MeasureRequest")
	if !ok || method.Type.NumIn() != 3 || method.Type.In(2) != reflect.TypeOf(provider.ChatRequest{}) {
		t.Fatalf("MeasureRequest signature can acquire non-request capability: %#v", method.Type)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse manager.go: %v", err)
	}
	forbiddenSelectors := map[string]struct{}{"OpenForUser": {}, "Begin": {}, "Read": {}, "ReadAll": {}}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		name := function.Name.Name
		if name != "MeasureRequest" && !strings.HasPrefix(name, "addCanonical") && name != "validateBaseMessage" && name != "addToolSchema" && name != "addToolSchemaProperty" && name != "addRawJSON" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok {
				if _, forbidden := forbiddenSelectors[selector.Sel.Name]; forbidden {
					t.Errorf("request measurement helper %s can read artifact payload through %s", name, selector.Sel.Name)
				}
			}
			return true
		})
	}

	redactor := redact.NewRuntimeRedactor()
	refJSON := `{"state":"completed","status":"success","summary":"safe","artifact":{"id":"` + strings.Repeat("b", 64) + `","bytes":7,"created_at":"2026-08-03T00:00:00Z","available":true,"complete":true},"truncated":true}`
	request := provider.ChatRequest{Messages: []provider.ModelMessage{{
		Role: provider.ModelMessageRoleToolResult, ToolCallID: "call", ToolName: "Read",
		Content: redactor.Redact(refJSON), ToolResult: redactor.Redact(refJSON), ToolResultStatus: string(tool.StatusSuccess),
	}}}
	if measure, measureErr := NewRequestBudgeter().MeasureRequest(context.Background(), request); measureErr != nil || measure.Bytes == 0 {
		t.Fatalf("measure opaque artifact metadata = (%#v,%v)", measure, measureErr)
	}
}

func TestMeasureRequestHandlesNestedSchemaAndInvalidUTF8(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	invalidByte := string([]byte{0xff})
	nestedRaw := json.RawMessage(`{"type":"object","properties":{"中🙂é":{"type":"array","items":{"oneOf":[{"type":"string"},{"type":"object","description":"` + invalidByte + `"}]}}}}`)
	request := provider.ChatRequest{
		Tools: []provider.ToolDefinition{{
			Name: "嵌套" + invalidByte, Description: "组合 é 🙂 " + invalidByte,
			Schema: tool.Schema{Raw: nestedRaw},
		}},
		Messages: []provider.ModelMessage{{
			Role: provider.ModelMessageRoleToolCall, ToolCallID: "调用-1", ToolName: "嵌套" + invalidByte,
			ArgumentsJSON: redactor.Redact(`{"中🙂é":"` + invalidByte + `"}`),
		}},
	}
	measure, err := budgeter.MeasureRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("measure nested unicode request: %v", err)
	}
	if measure.Bytes <= int64(len(nestedRaw)) {
		t.Fatalf("nested schema framing was not counted: measure=%#v raw=%d", measure, len(nestedRaw))
	}

	invalid := request
	invalid.Tools = []provider.ToolDefinition{{Name: "broken", Schema: tool.Schema{Raw: json.RawMessage(`{"type":`)}}}
	if invalidMeasure, invalidErr := budgeter.MeasureRequest(context.Background(), invalid); invalidErr == nil || invalidMeasure != (RequestMeasure{}) {
		t.Fatalf("invalid nested schema measured as (%#v,%v), want zero,error", invalidMeasure, invalidErr)
	}
}

func TestConversationTurnMeasureIsAdditiveUpperBound(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatalf("new result factory: %v", err)
	}
	ref := &artifact.Ref{
		ID: strings.Repeat("d", 64), Bytes: 4097, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Available: true, Complete: true,
	}
	result, err := resultFactory.Build(tool.ResultFactoryInput{
		CallID: "call-turn", Name: "Lookup", State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "safe summary", Preview: "safe preview 中文🙂é", Artifact: ref, CapturedBytes: ref.Bytes,
		Truncated: true, TruncationReason: string(tool.CaptureTruncatedInline),
	})
	if err != nil {
		t.Fatalf("build turn result: %v", err)
	}
	persisted := result.PersistedContent()
	arguments := redactor.Redact(`{"query":"中文🙂é"}`)
	turn := []conversation.Message{
		{Role: conversation.RoleUser, Content: redactor.Redact("question ASCII 中文 🙂 é")},
		{Role: conversation.RoleThinking, Content: redactor.Redact(strings.Repeat("not sent", 64))},
		{Role: conversation.RoleAssistant, Content: redactor.Redact("I will look it up")},
		{Role: conversation.RoleToolCall, Content: redactor.Redact("safe call metadata"), Tool: &conversation.ToolState{
			CallID: "call-turn", Name: "Lookup", ArgumentsJSON: arguments, State: tool.Prepared,
		}},
		{Role: conversation.RoleToolResult, Content: persisted, Tool: &conversation.ToolState{
			CallID: "call-turn", Name: "Lookup", State: tool.Completed, Status: tool.StatusSuccess,
			Summary: redactor.Redact("not separately sent"), Result: persisted, Truncated: true,
			TruncationReason: redactor.Redact(string(tool.CaptureTruncatedInline)), Artifact: ref,
		}},
		{Role: conversation.RoleAssistant, Content: redactor.Redact("answer")},
	}
	providerTurn := []provider.ModelMessage{
		{Role: provider.ModelMessageRoleUser, Content: turn[0].Content},
		{Role: provider.ModelMessageRoleAssistant, Content: turn[2].Content},
		{Role: provider.ModelMessageRoleToolCall, Content: turn[3].Content, ToolCallID: "call-turn", ToolName: "Lookup", ArgumentsJSON: arguments},
		{Role: provider.ModelMessageRoleToolResult, Content: persisted, ToolCallID: "call-turn", ToolName: "Lookup", ToolResult: persisted, ToolResultStatus: string(tool.StatusSuccess)},
		{Role: provider.ModelMessageRoleAssistant, Content: turn[5].Content},
	}

	turnMeasure, err := budgeter.MeasureConversationTurn(context.Background(), turn)
	if err != nil {
		t.Fatalf("measure conversation turn: %v", err)
	}
	if turnMeasure.Bytes <= 0 || turnMeasure.PlanningTokens != turnMeasure.Bytes/4+boolInt64(turnMeasure.Bytes%4 != 0) {
		t.Fatalf("turn measure = %#v", turnMeasure)
	}
	if !strings.Contains(persisted.Text(), ref.ID) {
		t.Fatal("turn fixture does not carry the opaque artifact ref in its final safe projection")
	}
	thinkingOnly, err := budgeter.MeasureConversationTurn(context.Background(), turn[1:2])
	if err != nil || thinkingOnly != (RequestMeasure{}) {
		t.Fatalf("thinking-only range = (%#v,%v), want zero,nil", thinkingOnly, err)
	}

	current := provider.ModelMessage{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("current input")}
	base := provider.ChatRequest{Model: "custom/model", Messages: []provider.ModelMessage{current}}
	baseMeasure, err := budgeter.MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatalf("measure base request: %v", err)
	}
	final := base
	final.Messages = append(append([]provider.ModelMessage(nil), providerTurn...), current)
	finalMeasure, err := budgeter.MeasureRequest(context.Background(), final)
	if err != nil {
		t.Fatalf("measure final request: %v", err)
	}
	bound, err := budgeter.addMeasures(baseMeasure, turnMeasure)
	if err != nil {
		t.Fatalf("add base and turn measures: %v", err)
	}
	if finalMeasure.Bytes > bound.Bytes || finalMeasure.PlanningTokens > bound.PlanningTokens {
		t.Fatalf("final measure %#v exceeds additive bound %#v", finalMeasure, bound)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse manager.go: %v", err)
	}
	measurementFunctions := map[string]struct{}{
		"MeasureConversationTurn": {}, "addCanonicalConversationTurn": {}, "canonicalConversationMessage": {}, "addCanonicalMessage": {},
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		if _, relevant := measurementFunctions[function.Name.Name]; !relevant {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, selectorOK := node.(*ast.SelectorExpr)
			if selectorOK {
				switch selector.Sel.Name {
				case "ContextMessages", "OpenForUser", "Begin", "Read", "ReadAll":
					t.Errorf("turn measurement helper %s can copy a conversation or read artifact payload through %s", function.Name.Name, selector.Sel.Name)
				}
			}
			return true
		})
	}
}

func TestFinalMeasureDoesNotExceedIncrementalBound(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	firstTurn := []conversation.Message{
		{Role: conversation.RoleUser, Content: redactor.Redact("first question")},
		{Role: conversation.RoleAssistant, Content: redactor.Redact("first answer")},
	}
	secondTurn := []conversation.Message{
		{Role: conversation.RoleUser, Content: redactor.Redact("第二个问题 🙂")},
		{Role: conversation.RoleAssistant, Content: redactor.Redact("第二个回答 é")},
	}
	base := provider.ChatRequest{
		Model:        "arbitrary-openai-compatible/model",
		StableSystem: []provider.SystemBlock{{Name: "skill-sop", Content: redactor.Redact("safe SOP"), Cacheable: true}},
		Cache:        provider.CachePolicy{EnablePromptCache: true, SystemBreakpointName: "skill-sop"},
	}
	baseMeasure, err := budgeter.MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatalf("measure base: %v", err)
	}
	bound := baseMeasure
	for index, turn := range [][]conversation.Message{firstTurn, secondTurn} {
		turnMeasure, measureErr := budgeter.MeasureConversationTurn(context.Background(), turn)
		if measureErr != nil {
			t.Fatalf("measure turn %d: %v", index, measureErr)
		}
		bound, measureErr = budgeter.addMeasures(bound, turnMeasure)
		if measureErr != nil {
			t.Fatalf("accumulate turn %d: %v", index, measureErr)
		}
	}
	final := base
	final.Messages = []provider.ModelMessage{
		{Role: provider.ModelMessageRoleUser, Content: firstTurn[0].Content},
		{Role: provider.ModelMessageRoleAssistant, Content: firstTurn[1].Content},
		{Role: provider.ModelMessageRoleUser, Content: secondTurn[0].Content},
		{Role: provider.ModelMessageRoleAssistant, Content: secondTurn[1].Content},
	}
	finalMeasure, err := budgeter.MeasureRequest(context.Background(), final)
	if err != nil {
		t.Fatalf("measure final: %v", err)
	}
	if finalMeasure.Bytes > bound.Bytes || finalMeasure.PlanningTokens > bound.PlanningTokens {
		t.Fatalf("final measure %#v exceeds incremental bound %#v", finalMeasure, bound)
	}
	if finalMeasure.Bytes >= bound.Bytes {
		t.Fatalf("worst insertion framing did not permit a smaller final measure: final=%#v bound=%#v", finalMeasure, bound)
	}

	unaccounted := final
	unaccounted.Messages = append(append([]provider.ModelMessage(nil), final.Messages...), provider.ModelMessage{
		Role: provider.ModelMessageRoleAssistant, Content: redactor.Redact(strings.Repeat("unaccounted", 1024)),
	})
	unaccountedMeasure, err := budgeter.MeasureRequest(context.Background(), unaccounted)
	if err != nil {
		t.Fatalf("measure unaccounted final: %v", err)
	}
	if unaccountedMeasure.Bytes <= bound.Bytes && unaccountedMeasure.PlanningTokens <= bound.PlanningTokens {
		t.Fatalf("larger final was not detectable: final=%#v bound=%#v", unaccountedMeasure, bound)
	}
	if invalid, addErr := budgeter.addMeasures(bound, RequestMeasure{Bytes: -1}); addErr == nil || invalid != (RequestMeasure{}) {
		t.Fatalf("negative incremental measure = (%#v,%v), want zero,error", invalid, addErr)
	}
}

func TestTurnMeasureCancellationReturnsNoPartialResult(t *testing.T) {
	budgeter := NewRequestBudgeter()
	redactor := redact.NewRuntimeRedactor()
	longTurn := []conversation.Message{{
		Role: conversation.RoleUser, Content: redactor.Redact(strings.Repeat("x", requestStringCheckBytes*3)),
	}}
	canceling := &cancelAfterChecksContext{cancelAt: 5}
	if measure, err := budgeter.MeasureConversationTurn(canceling, longTurn); !errors.Is(err, context.Canceled) || measure != (RequestMeasure{}) {
		t.Fatalf("long turn cancellation = (%#v,%v), checks=%d", measure, err, canceling.checks)
	}
	if canceling.checks > 5 {
		t.Fatalf("long turn cancellation exceeded fixed checks: %d", canceling.checks)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if measure, err := budgeter.MeasureConversationTurn(canceled, longTurn); !errors.Is(err, context.Canceled) || measure != (RequestMeasure{}) {
		t.Fatalf("pre-canceled turn = (%#v,%v), want zero,canceled", measure, err)
	}
	invalid := [][]conversation.Message{
		{{Role: conversation.MessageRole("future"), Content: redactor.Redact("value")}},
		{{Role: conversation.RoleUser, Content: redactor.Redact("value"), Tool: &conversation.ToolState{CallID: "unexpected"}}},
		{{Role: conversation.RoleToolCall, Content: redactor.Redact("call")}},
		{{Role: conversation.RoleToolResult, Content: redactor.Redact("result"), Tool: &conversation.ToolState{CallID: "call", Name: "Read", Result: redactor.Redact("result")}}},
		{{Role: conversation.RoleThinking, Content: redactor.Redact("hidden"), Tool: &conversation.ToolState{CallID: "unexpected"}}},
	}
	for index, turn := range invalid {
		if measure, err := budgeter.MeasureConversationTurn(context.Background(), turn); err == nil || measure != (RequestMeasure{}) {
			t.Fatalf("invalid turn %d = (%#v,%v), want zero,error", index, measure, err)
		}
	}
	if measure, err := budgeter.MeasureConversationTurn(nil, nil); err == nil || measure != (RequestMeasure{}) {
		t.Fatalf("nil-context turn = (%#v,%v), want zero,error", measure, err)
	}
	if measure, err := (RequestBudgeter{}).MeasureConversationTurn(context.Background(), nil); err == nil || measure != (RequestMeasure{}) {
		t.Fatalf("zero-budgeter turn = (%#v,%v), want zero,error", measure, err)
	}
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

type cancelAfterChecksContext struct {
	checks   int
	cancelAt int
}

func (*cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelAfterChecksContext) Done() <-chan struct{}       { return nil }
func (*cancelAfterChecksContext) Value(any) any               { return nil }

func (c *cancelAfterChecksContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
