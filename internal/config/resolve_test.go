package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

var approvedAppConfigNumericKeys = []string{
	"agent.max_iterations",
	"agent.max_unknown_tool_calls",
	"artifact.max_file_bytes",
	"artifact.max_total_bytes",
	"artifact.retention_days",
	"context.auto_margin_tokens",
	"context.manual_margin_tokens",
	"context.model_window_tokens",
	"context.preview_chars",
	"context.recent_keep_messages",
	"context.recent_keep_tokens",
	"context.summary_failure_limit",
	"context.tool_result_threshold_chars",
	"context.tool_results_threshold_chars",
	"diagnostics.max_item_bytes",
	"diagnostics.max_items",
	"diagnostics.max_total_bytes",
	"files.read_max_bytes",
	"files.scan_max_bytes",
	"files.scan_max_directories",
	"files.scan_max_files",
	"files.scan_max_lines",
	"instructions.max_expanded_bytes",
	"instructions.max_file_bytes",
	"instructions.max_files",
	"instructions.max_include_depth",
	"instructions.max_total_bytes",
	"lifecycle.cleanup_timeout_ms",
	"llm.request_timeout_ms",
	"llm.stream.max_event_bytes",
	"llm.stream.max_events",
	"llm.stream.max_response_bytes",
	"llm.stream.max_text_bytes",
	"llm.stream.max_thinking_bytes",
	"llm.stream.max_tool_arguments_bytes",
	"llm.thinking.budget_tokens",
	"mcp.default_timeout_ms",
	"mcp.max_pages",
	"mcp.max_protocol_errors",
	"mcp.max_response_bytes",
	"mcp.max_tools",
	"mcp.servers.<name>.timeout_ms",
	"memory.max_candidate_bytes",
	"memory.max_index_bytes",
	"memory.max_index_lines",
	"memory.update_concurrency",
	"memory.update_queue_size",
	"memory.update_timeout_ms",
	"session.gap_reminder_days",
	"session.max_record_bytes",
	"session.max_scan_bytes",
	"session.max_scan_files",
	"session.max_session_bytes",
	"session.retention_days",
	"tool.capture_bytes",
	"tool.inline_output_bytes",
	"tool.timeout_ms",
}

func TestAppConfigNumericManifestIsExact(t *testing.T) {
	manifest := appConfigNumericKeys()
	sort.Strings(manifest)
	if !reflect.DeepEqual(manifest, approvedAppConfigNumericKeys) {
		t.Fatal("AppConfig numeric manifest differs from the approved snapshot")
	}

	partialKeys := partialNumericKeys(t)
	if _, ok := partialKeys["tool.max_output_bytes"]; !ok {
		t.Fatal("PartialAppConfig lost the approved legacy tool output input")
	}
	delete(partialKeys, "tool.max_output_bytes")
	actual := make([]string, 0, len(partialKeys))
	for path := range partialKeys {
		actual = append(actual, path)
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, approvedAppConfigNumericKeys) {
		t.Fatal("PartialAppConfig numeric leaves differ from the approved snapshot")
	}

	cases := numericBoundaryCases(10, 1, 100)
	if len(cases) != 7 || cases[0].name != "absent_default" || cases[6].encoded != "9223372036854775808" {
		t.Fatal("numeric boundary matrix skeleton is incomplete")
	}
}

func TestNonAppConfigNumericInputsAreAbsentFromPartialConfig(t *testing.T) {
	typeOf := reflect.TypeOf(PartialAppConfig{})
	for _, forbidden := range []string{"hook", "hooks", "skill"} {
		if _, ok := yamlField(typeOf, forbidden); ok {
			t.Fatal("PartialAppConfig contains a numeric input owned by another loader")
		}
	}
	permissionField, ok := yamlField(typeOf, "permission")
	if !ok || permissionField.Type != reflect.TypeOf(PartialPermissionConfig{}) || permissionField.Type.NumField() != 1 ||
		permissionField.Type.Field(0).Tag.Get("yaml") != "mode" {
		t.Fatal("PartialAppConfig absorbed permission RuleFile inputs")
	}
	if partialContainsYAMLName(typeOf, "history") || partialContainsYAMLName(typeOf, "version") {
		t.Fatal("PartialAppConfig absorbed Skill history or owner-specific version inputs")
	}
	for _, path := range appConfigNumericKeys() {
		if strings.HasPrefix(path, "hook.") || strings.HasPrefix(path, "hooks.") ||
			strings.HasPrefix(path, "skill.") || path == "permission.version" || strings.HasSuffix(path, ".history") {
			t.Fatal("AppConfig numeric manifest contains an input owned by another loader")
		}
	}
}

type numericBoundaryCase struct {
	name      string
	candidate Optional[int64]
	encoded   string
	want      int64
	wantValid bool
}

func numericBoundaryCases(defaultValue int64, minimum int64, hardCap int64) []numericBoundaryCase {
	return []numericBoundaryCase{
		{name: "absent_default", want: defaultValue, wantValid: true},
		{name: "minimum", candidate: Optional[int64]{Set: true, Value: minimum}, want: minimum, wantValid: true},
		{name: "hard_cap", candidate: Optional[int64]{Set: true, Value: hardCap}, want: hardCap, wantValid: true},
		{name: "hard_cap_plus_one", candidate: Optional[int64]{Set: true, Value: hardCap + 1}},
		{name: "explicit_zero", candidate: Optional[int64]{Set: true, Value: 0}},
		{name: "negative", candidate: Optional[int64]{Set: true, Value: -1}},
		{name: "overflow", encoded: "9223372036854775808"},
	}
}

func partialNumericKeys(t *testing.T) map[string]struct{} {
	t.Helper()
	result := make(map[string]struct{})
	collectPartialNumericKeys(t, reflect.TypeOf(PartialAppConfig{}), "", result)
	return result
}

func collectPartialNumericKeys(t *testing.T, typeOf reflect.Type, path string, result map[string]struct{}) {
	t.Helper()
	if optionalValueType, ok := optionalTypeValue(typeOf); ok {
		if optionalValueType.Kind() >= reflect.Int && optionalValueType.Kind() <= reflect.Int64 {
			result[path] = struct{}{}
		}
		return
	}
	if typeOf.Kind() == reflect.Map {
		collectPartialNumericKeys(t, typeOf.Elem(), joinConfigPath(path, "<name>"), result)
		return
	}
	if typeOf.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		collectPartialNumericKeys(t, field.Type, joinConfigPath(path, name), result)
	}
}

func optionalTypeValue(typeOf reflect.Type) (reflect.Type, bool) {
	if typeOf.Kind() != reflect.Struct || typeOf.PkgPath() != reflect.TypeOf(Optional[int]{}).PkgPath() ||
		!strings.HasPrefix(typeOf.Name(), "Optional[") || typeOf.NumField() != 2 ||
		typeOf.Field(0).Name != "Set" || typeOf.Field(1).Name != "Value" {
		return nil, false
	}
	return typeOf.Field(1).Type, true
}

func yamlField(typeOf reflect.Type, name string) (reflect.StructField, bool) {
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if strings.Split(field.Tag.Get("yaml"), ",")[0] == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func partialContainsYAMLName(typeOf reflect.Type, name string) bool {
	if _, ok := optionalTypeValue(typeOf); ok {
		return false
	}
	if typeOf.Kind() == reflect.Map || typeOf.Kind() == reflect.Slice {
		return partialContainsYAMLName(typeOf.Elem(), name)
	}
	if typeOf.Kind() != reflect.Struct {
		return false
	}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if strings.Split(field.Tag.Get("yaml"), ",")[0] == name || partialContainsYAMLName(field.Type, name) {
			return true
		}
	}
	return false
}
