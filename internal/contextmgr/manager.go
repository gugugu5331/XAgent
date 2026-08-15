package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeManual Mode = "manual"

	providerStreamCloseWait = 2 * time.Second

	requestMeasurementVersion  uint8  = 1
	requestBudgeterValidMarker uint64 = 0x584147454e545242

	requestStringCheckBytes     = 64 * 1024
	requestSchemaNodeCheckCount = 1024
	requestJSONMaxDepth         = 10_000
)

// RequestMeasure is the versioned request-planning measure shared by request
// and complete-turn accounting. Both dimensions are non-negative upper bounds.
type RequestMeasure struct {
	Bytes          int64
	PlanningTokens int64
}

// RequestBudgeter is an immutable concrete value. Its private marker prevents
// a zero value or caller literal from becoming an accepted measurement owner.
type RequestBudgeter struct {
	version uint8
	marker  uint64
}

type requestByteCounter struct {
	ctx              context.Context
	budgeter         RequestBudgeter
	bytes            int64
	rawBytesSinceCtx int64
	schemaNodes      int
}

// NewRequestBudgeter is the only constructor for a valid RequestBudgeter.
func NewRequestBudgeter() RequestBudgeter {
	return RequestBudgeter{version: requestMeasurementVersion, marker: requestBudgeterValidMarker}
}

func (b RequestBudgeter) valid() bool {
	return b.version == requestMeasurementVersion && b.marker == requestBudgeterValidMarker
}

func (b RequestBudgeter) requireValid() error {
	if !b.valid() {
		return errors.New("request budgeter is invalid")
	}
	return nil
}

// Validate allows an injected owner to reject a zero or forged budgeter
// without performing a measurement.
func (b RequestBudgeter) Validate() error {
	return b.requireValid()
}

func (b RequestBudgeter) measureBytes(bytes int64) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	if bytes < 0 {
		return RequestMeasure{}, errors.New("request byte measure is negative")
	}
	tokens := bytes / 4
	if bytes%4 != 0 {
		tokens++
	}
	return RequestMeasure{Bytes: bytes, PlanningTokens: tokens}, nil
}

func (b RequestBudgeter) addMeasures(left RequestMeasure, right RequestMeasure) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	bytes, err := checkedAddRequestValue(left.Bytes, right.Bytes)
	if err != nil {
		return RequestMeasure{}, err
	}
	tokens, err := checkedAddRequestValue(left.PlanningTokens, right.PlanningTokens)
	if err != nil {
		return RequestMeasure{}, err
	}
	return RequestMeasure{Bytes: bytes, PlanningTokens: tokens}, nil
}

func (b RequestBudgeter) multiplyMeasure(measure RequestMeasure, factor int64) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	bytes, err := checkedMultiplyRequestValue(measure.Bytes, factor)
	if err != nil {
		return RequestMeasure{}, err
	}
	tokens, err := checkedMultiplyRequestValue(measure.PlanningTokens, factor)
	if err != nil {
		return RequestMeasure{}, err
	}
	return RequestMeasure{Bytes: bytes, PlanningTokens: tokens}, nil
}

func (b RequestBudgeter) measureJSONValue(ctx context.Context, value any) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	if ctx == nil {
		return RequestMeasure{}, errors.New("request measurement context is nil")
	}
	counter := requestByteCounter{ctx: ctx, budgeter: b}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.addJSONValue(value, 0); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	measure, err := b.measureBytes(counter.bytes)
	if err != nil {
		return RequestMeasure{}, err
	}
	return measure, nil
}

// MeasureRequest returns the canonical request-planning upper bound.
func (b RequestBudgeter) MeasureRequest(ctx context.Context, request provider.ChatRequest) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	if ctx == nil {
		return RequestMeasure{}, errors.New("request measurement context is nil")
	}
	if request.Thinking.BudgetTokens < 0 {
		return RequestMeasure{}, errors.New("request thinking budget is negative")
	}
	counter := requestByteCounter{ctx: ctx, budgeter: b}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.addCanonicalRequest(request); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	measure, err := b.measureBytes(counter.bytes)
	if err != nil {
		return RequestMeasure{}, err
	}
	return measure, nil
}

// MeasureConversationTurn returns the additive upper-bound contribution of
// the Provider-visible messages in one read-only conversation index range.
// Each included message reserves one worst-case insertion separator so the
// result can be added to any canonical base request without remeasuring it.
func (b RequestBudgeter) MeasureConversationTurn(ctx context.Context, messages []conversation.Message) (RequestMeasure, error) {
	if err := b.requireValid(); err != nil {
		return RequestMeasure{}, err
	}
	if ctx == nil {
		return RequestMeasure{}, errors.New("conversation turn measurement context is nil")
	}
	counter := requestByteCounter{ctx: ctx, budgeter: b}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.addCanonicalConversationTurn(messages); err != nil {
		return RequestMeasure{}, err
	}
	if err := counter.checkContext(); err != nil {
		return RequestMeasure{}, err
	}
	measure, err := b.measureBytes(counter.bytes)
	if err != nil {
		return RequestMeasure{}, err
	}
	return measure, nil
}

func (c *requestByteCounter) addCanonicalRequest(request provider.ChatRequest) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if err := c.addJSONObjectField(&first, "version"); err != nil {
		return err
	}
	if err := c.addUnsignedInteger(uint64(requestMeasurementVersion)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "model"); err != nil {
		return err
	}
	if err := c.addJSONString(strings.TrimSpace(request.Model)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "system"); err != nil {
		return err
	}
	if err := c.addCanonicalSystemBlocks(request); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "messages"); err != nil {
		return err
	}
	if err := c.addCanonicalMessages(request.Messages); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "tools"); err != nil {
		return err
	}
	if err := c.addCanonicalTools(request); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "thinking"); err != nil {
		return err
	}
	if err := c.addCanonicalThinking(request.Thinking.Enabled, request.Thinking.Show, request.Thinking.BudgetTokens); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "cache"); err != nil {
		return err
	}
	if err := c.addCanonicalCache(request.Cache); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "adapter_framing"); err != nil {
		return err
	}
	if err := c.addCanonicalAdapterFraming(); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalSystemBlocks(request provider.ChatRequest) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if len(request.System) > 0 {
		for _, block := range request.System {
			if err := c.addCanonicalSystemBlock(&first, "ordered", block); err != nil {
				return err
			}
		}
	} else {
		for _, block := range request.StableSystem {
			if err := c.addCanonicalSystemBlock(&first, "stable", block); err != nil {
				return err
			}
		}
		for _, block := range request.DynamicSystem {
			if err := c.addCanonicalSystemBlock(&first, "dynamic", block); err != nil {
				return err
			}
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalSystemBlock(first *bool, kind string, block provider.SystemBlock) error {
	content := block.Content.Text()
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if err := c.addJSONArrayItem(first); err != nil {
		return err
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	firstField := true
	if err := c.addJSONObjectField(&firstField, "kind"); err != nil {
		return err
	}
	if err := c.addJSONString(kind); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "name"); err != nil {
		return err
	}
	if err := c.addJSONString(strings.TrimSpace(block.Name)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "content"); err != nil {
		return err
	}
	if err := c.addJSONString(content); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "cacheable"); err != nil {
		return err
	}
	if block.Cacheable {
		if err := c.addBytes(4); err != nil {
			return err
		}
	} else if err := c.addBytes(5); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalMessages(messages []provider.ModelMessage) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	for _, message := range messages {
		if err := c.validateBaseMessage(message); err != nil {
			return err
		}
		if err := c.addJSONArrayItem(&first); err != nil {
			return err
		}
		if err := c.addCanonicalMessage(message); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalConversationTurn(messages []conversation.Message) error {
	for index := range messages {
		if err := c.checkContext(); err != nil {
			return err
		}
		message, included, err := canonicalConversationMessage(messages[index])
		if err != nil {
			return err
		}
		if !included {
			continue
		}
		if err := c.validateBaseMessage(message); err != nil {
			return err
		}
		// A comma is the largest insertion framing needed for one additional
		// message. It is deliberately reserved even when the base array is empty.
		if err := c.addBytes(1); err != nil {
			return err
		}
		if err := c.addCanonicalMessage(message); err != nil {
			return err
		}
	}
	return nil
}

func canonicalConversationMessage(message conversation.Message) (provider.ModelMessage, bool, error) {
	var role provider.ModelMessageRole
	switch message.Role {
	case conversation.RoleUser:
		role = provider.ModelMessageRoleUser
	case conversation.RoleAssistant:
		role = provider.ModelMessageRoleAssistant
	case conversation.RoleToolCall:
		role = provider.ModelMessageRoleToolCall
	case conversation.RoleToolResult:
		role = provider.ModelMessageRoleToolResult
	case conversation.RoleContextSummary:
		role = provider.ModelMessageRoleContextSummary
	case conversation.RoleContextBoundary:
		role = provider.ModelMessageRoleContextBoundary
	case conversation.RoleThinking:
		if message.Tool != nil {
			return provider.ModelMessage{}, false, errors.New("conversation thinking message contains tool state")
		}
		return provider.ModelMessage{}, false, nil
	default:
		return provider.ModelMessage{}, false, errors.New("conversation message role is unsupported")
	}

	projected := provider.ModelMessage{Role: role, Content: message.Content}
	if message.Tool == nil {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult {
			return provider.ModelMessage{}, false, errors.New("conversation tool message has no tool state")
		}
		return projected, true, nil
	}
	if message.Role != conversation.RoleToolCall && message.Role != conversation.RoleToolResult {
		return provider.ModelMessage{}, false, errors.New("conversation base message contains tool state")
	}
	projected.ToolCallID = message.Tool.CallID
	projected.ToolName = message.Tool.Name
	projected.ArgumentsJSON = message.Tool.ArgumentsJSON
	projected.ToolResult = message.Tool.Result
	projected.ToolResultStatus = string(message.Tool.Status)
	return projected, true, nil
}

func (c *requestByteCounter) addCanonicalMessage(message provider.ModelMessage) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	firstField := true
	if err := c.addJSONObjectField(&firstField, "role"); err != nil {
		return err
	}
	if err := c.addJSONString(string(message.Role)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "content"); err != nil {
		return err
	}
	if err := c.addJSONString(message.Content.Text()); err != nil {
		return err
	}
	switch message.Role {
	case provider.ModelMessageRoleToolCall:
		if err := c.addCanonicalToolCallFields(&firstField, message); err != nil {
			return err
		}
	case provider.ModelMessageRoleToolResult:
		if err := c.addCanonicalToolResultFields(&firstField, message); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) validateBaseMessage(message provider.ModelMessage) error {
	switch message.Role {
	case provider.ModelMessageRoleUser, provider.ModelMessageRoleAssistant,
		provider.ModelMessageRoleContextSummary, provider.ModelMessageRoleContextBoundary:
		if message.ToolCallID != "" || message.ToolName != "" || message.ArgumentsJSON.Text() != "" ||
			message.ToolResult.Text() != "" || message.ToolResultStatus != "" {
			return errors.New("request base message contains tool fields")
		}
		return nil
	case provider.ModelMessageRoleToolCall:
		if strings.TrimSpace(message.ToolCallID) == "" || strings.TrimSpace(message.ToolName) == "" ||
			message.ToolResult.Text() != "" || message.ToolResultStatus != "" {
			return errors.New("request tool call message is invalid")
		}
		return nil
	case provider.ModelMessageRoleToolResult:
		if strings.TrimSpace(message.ToolCallID) == "" || strings.TrimSpace(message.ToolName) == "" ||
			message.ArgumentsJSON.Text() != "" || message.ToolResult.Text() == "" || !validRequestToolResultStatus(message.ToolResultStatus) {
			return errors.New("request tool result message is invalid")
		}
		return nil
	default:
		return errors.New("request message role is unsupported")
	}
}

func (c *requestByteCounter) addCanonicalToolCallFields(first *bool, message provider.ModelMessage) error {
	fields := [...]struct {
		name  string
		value string
	}{
		{name: "tool_call_id", value: message.ToolCallID},
		{name: "tool_name", value: message.ToolName},
		{name: "arguments_json", value: message.ArgumentsJSON.Text()},
		{name: "openai_tool_type", value: "function"},
		{name: "anthropic_block_type", value: "tool_use"},
	}
	for _, field := range fields {
		if err := c.addJSONObjectField(first, field.name); err != nil {
			return err
		}
		if err := c.addJSONString(field.value); err != nil {
			return err
		}
	}
	return nil
}

func (c *requestByteCounter) addCanonicalToolResultFields(first *bool, message provider.ModelMessage) error {
	fields := [...]struct {
		name  string
		value string
	}{
		{name: "tool_call_id", value: message.ToolCallID},
		{name: "tool_name", value: message.ToolName},
		{name: "tool_result", value: message.ToolResult.Text()},
		{name: "tool_result_status", value: message.ToolResultStatus},
		{name: "anthropic_block_type", value: "tool_result"},
	}
	for _, field := range fields {
		if err := c.addJSONObjectField(first, field.name); err != nil {
			return err
		}
		if err := c.addJSONString(field.value); err != nil {
			return err
		}
	}
	if err := c.addJSONObjectField(first, "anthropic_is_error"); err != nil {
		return err
	}
	if message.ToolResultStatus == string(tool.StatusSuccess) {
		return c.addBytes(5)
	}
	return c.addBytes(4)
}

func validRequestToolResultStatus(status string) bool {
	switch tool.ResultStatus(status) {
	case tool.StatusSuccess, tool.StatusError, tool.StatusDenied, tool.StatusTimeout:
		return true
	default:
		return false
	}
}

func (c *requestByteCounter) addCanonicalTools(request provider.ChatRequest) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if len(request.Tools) > 0 {
		for _, definition := range request.Tools {
			if err := c.addCanonicalToolDefinition(&first, definition.Name, definition.Description, definition.Schema); err != nil {
				return err
			}
		}
	} else if request.ToolDefs != nil {
		if err := c.checkContext(); err != nil {
			return err
		}
		definitions := request.ToolDefs.OpenAIDefinitions()
		if err := c.checkContext(); err != nil {
			return err
		}
		for _, definition := range definitions {
			if err := c.addCanonicalToolDefinition(
				&first,
				definition.Function.Name,
				definition.Function.Description,
				definition.Function.Parameters,
			); err != nil {
				return err
			}
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalToolDefinition(first *bool, name string, description string, schema tool.Schema) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if err := c.addJSONArrayItem(first); err != nil {
		return err
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	firstField := true
	if err := c.addJSONObjectField(&firstField, "type"); err != nil {
		return err
	}
	if err := c.addJSONString("function"); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "name"); err != nil {
		return err
	}
	if err := c.addJSONString(name); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "description"); err != nil {
		return err
	}
	if err := c.addJSONString(strings.TrimSpace(description)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "schema"); err != nil {
		return err
	}
	if err := c.addToolSchema(schema, 0); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&firstField, "anthropic_input_schema"); err != nil {
		return err
	}
	if err := c.addBytes(4); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalAdapterFraming() error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	boolFields := [...]string{"openai_stream", "openai_include_usage"}
	for _, name := range boolFields {
		if err := c.addJSONObjectField(&first, name); err != nil {
			return err
		}
		if err := c.addBytes(4); err != nil {
			return err
		}
	}
	if err := c.addJSONObjectField(&first, "openai_tool_type"); err != nil {
		return err
	}
	if err := c.addJSONString("function"); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "anthropic_max_tokens"); err != nil {
		return err
	}
	if err := c.addUnsignedInteger(64_000); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "anthropic_cache_control_type"); err != nil {
		return err
	}
	if err := c.addJSONString("ephemeral"); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalThinking(enabled bool, show bool, budgetTokens int) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if err := c.addJSONObjectField(&first, "enabled"); err != nil {
		return err
	}
	if enabled {
		if err := c.addBytes(4); err != nil {
			return err
		}
	} else if err := c.addBytes(5); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "show"); err != nil {
		return err
	}
	if show {
		if err := c.addBytes(4); err != nil {
			return err
		}
	} else if err := c.addBytes(5); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "budget_tokens"); err != nil {
		return err
	}
	if err := c.addSignedInteger(int64(budgetTokens)); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addCanonicalCache(cache provider.CachePolicy) error {
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if err := c.addJSONObjectField(&first, "enable_prompt_cache"); err != nil {
		return err
	}
	if cache.EnablePromptCache {
		if err := c.addBytes(4); err != nil {
			return err
		}
	} else if err := c.addBytes(5); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "system_breakpoint_name"); err != nil {
		return err
	}
	if err := c.addJSONString(strings.TrimSpace(cache.SystemBreakpointName)); err != nil {
		return err
	}
	if err := c.addJSONObjectField(&first, "cache_tools"); err != nil {
		return err
	}
	if cache.CacheTools {
		if err := c.addBytes(4); err != nil {
			return err
		}
	} else if err := c.addBytes(5); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addJSONArrayItem(first *bool) error {
	if err := c.checkContext(); err != nil {
		return err
	}
	if !*first {
		if err := c.addBytes(1); err != nil {
			return err
		}
	}
	*first = false
	return nil
}

func (c *requestByteCounter) checkContext() error {
	return c.ctx.Err()
}

func (c *requestByteCounter) addBytes(bytes int64) error {
	measured, err := checkedAddRequestValue(c.bytes, bytes)
	if err != nil {
		return err
	}
	c.bytes = measured
	return nil
}

func (c *requestByteCounter) addJSONString(value string) error {
	if err := c.addBytes(2); err != nil {
		return err
	}
	for remaining := len(value); remaining > 0; {
		if err := c.checkContext(); err != nil {
			return err
		}
		chunk := remaining
		if chunk > requestStringCheckBytes {
			chunk = requestStringCheckBytes
		}
		encodedBytes, err := checkedMultiplyRequestValue(int64(chunk), 6)
		if err != nil {
			return err
		}
		if err := c.addBytes(encodedBytes); err != nil {
			return err
		}
		remaining -= chunk
	}
	return nil
}

func (c *requestByteCounter) visitSchemaNode() error {
	c.schemaNodes++
	if c.schemaNodes < requestSchemaNodeCheckCount {
		return nil
	}
	c.schemaNodes = 0
	return c.checkContext()
}

func (c *requestByteCounter) consumeRawBytes(bytes int) error {
	for bytes > 0 {
		remaining := int64(requestStringCheckBytes) - c.rawBytesSinceCtx
		chunk := int64(bytes)
		if chunk > remaining {
			chunk = remaining
		}
		c.rawBytesSinceCtx += chunk
		bytes -= int(chunk)
		if c.rawBytesSinceCtx == requestStringCheckBytes {
			c.rawBytesSinceCtx = 0
			if err := c.checkContext(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *requestByteCounter) addJSONValue(value any, depth int) error {
	if depth > requestJSONMaxDepth {
		return errors.New("request JSON value exceeds maximum depth")
	}
	if err := c.checkContext(); err != nil {
		return err
	}
	switch typed := value.(type) {
	case nil:
		return c.addBytes(4)
	case string:
		return c.addJSONString(typed)
	case bool:
		if typed {
			return c.addBytes(4)
		}
		return c.addBytes(5)
	case json.Number:
		if err := c.validateJSONNumber(typed); err != nil {
			return err
		}
		return c.addBytes(int64(len(typed)))
	case int:
		return c.addSignedInteger(int64(typed))
	case int8:
		return c.addSignedInteger(int64(typed))
	case int16:
		return c.addSignedInteger(int64(typed))
	case int32:
		return c.addSignedInteger(int64(typed))
	case int64:
		return c.addSignedInteger(typed)
	case uint:
		return c.addUnsignedInteger(uint64(typed))
	case uint8:
		return c.addUnsignedInteger(uint64(typed))
	case uint16:
		return c.addUnsignedInteger(uint64(typed))
	case uint32:
		return c.addUnsignedInteger(uint64(typed))
	case uint64:
		return c.addUnsignedInteger(typed)
	case uintptr:
		return c.addUnsignedInteger(uint64(typed))
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return errors.New("request JSON number is not finite")
		}
		return c.addBytes(24)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return errors.New("request JSON number is not finite")
		}
		return c.addBytes(24)
	case json.RawMessage:
		return c.addRawJSON(typed)
	case []string:
		if typed == nil {
			return c.addBytes(4)
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		for index, item := range typed {
			if err := c.checkContext(); err != nil {
				return err
			}
			if index > 0 {
				if err := c.addBytes(1); err != nil {
					return err
				}
			}
			if err := c.addJSONString(item); err != nil {
				return err
			}
		}
		return c.addBytes(1)
	case []any:
		return c.addJSONArray(typed, depth)
	case map[string]string:
		return c.addJSONStringMap(typed)
	case map[string]any:
		return c.addJSONMap(typed, depth)
	case tool.Schema:
		return c.addToolSchema(typed, depth)
	case tool.SchemaProperty:
		return c.addToolSchemaProperty(typed, depth)
	default:
		return fmt.Errorf("request JSON value type %T is unsupported", value)
	}
}

func (c *requestByteCounter) addJSONArray(values []any, depth int) error {
	if values == nil {
		return c.addBytes(4)
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	for index, value := range values {
		if err := c.checkContext(); err != nil {
			return err
		}
		if index > 0 {
			if err := c.addBytes(1); err != nil {
				return err
			}
		}
		if err := c.addJSONValue(value, depth+1); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addJSONStringMap(values map[string]string) error {
	if values == nil {
		return c.addBytes(4)
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	for key, value := range values {
		if err := c.checkContext(); err != nil {
			return err
		}
		if !first {
			if err := c.addBytes(1); err != nil {
				return err
			}
		}
		first = false
		if err := c.addJSONString(key); err != nil {
			return err
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		if err := c.addJSONString(value); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addJSONMap(values map[string]any, depth int) error {
	if values == nil {
		return c.addBytes(4)
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	for key, value := range values {
		if err := c.checkContext(); err != nil {
			return err
		}
		if !first {
			if err := c.addBytes(1); err != nil {
				return err
			}
		}
		first = false
		if err := c.addJSONString(key); err != nil {
			return err
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		if err := c.addJSONValue(value, depth+1); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addSignedInteger(value int64) error {
	bytes := int64(0)
	magnitude := uint64(value)
	if value < 0 {
		bytes = 1
		magnitude = uint64(-(value + 1)) + 1
	}
	return c.addBytes(bytes + decimalDigits(magnitude))
}

func (c *requestByteCounter) addUnsignedInteger(value uint64) error {
	return c.addBytes(decimalDigits(value))
}

func decimalDigits(value uint64) int64 {
	digits := int64(1)
	for value >= 10 {
		value /= 10
		digits++
	}
	return digits
}

func (c *requestByteCounter) validateJSONNumber(value json.Number) error {
	if value == "" {
		return errors.New("request JSON number is empty")
	}
	index := 0
	consume := func() error {
		if err := c.consumeRawBytes(1); err != nil {
			return err
		}
		index++
		return nil
	}
	if value[index] == '-' {
		if err := consume(); err != nil {
			return err
		}
		if index == len(value) {
			return errors.New("request JSON number is invalid")
		}
	}
	if value[index] == '0' {
		if err := consume(); err != nil {
			return err
		}
		if index < len(value) && value[index] >= '0' && value[index] <= '9' {
			return errors.New("request JSON number has a leading zero")
		}
	} else {
		if value[index] < '1' || value[index] > '9' {
			return errors.New("request JSON number is invalid")
		}
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			if err := consume(); err != nil {
				return err
			}
		}
	}
	if index < len(value) && value[index] == '.' {
		if err := consume(); err != nil {
			return err
		}
		start := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			if err := consume(); err != nil {
				return err
			}
		}
		if index == start {
			return errors.New("request JSON number fraction is invalid")
		}
	}
	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		if err := consume(); err != nil {
			return err
		}
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			if err := consume(); err != nil {
				return err
			}
		}
		start := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			if err := consume(); err != nil {
				return err
			}
		}
		if index == start {
			return errors.New("request JSON number exponent is invalid")
		}
	}
	if index != len(value) {
		return errors.New("request JSON number is invalid")
	}
	return nil
}

func (c *requestByteCounter) addToolSchema(schema tool.Schema, depth int) error {
	if err := c.visitSchemaNode(); err != nil {
		return err
	}
	if len(schema.Raw) > 0 {
		return c.addRawJSON(schema.Raw)
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if schema.Type != "" {
		if err := c.addJSONObjectField(&first, "type"); err != nil {
			return err
		}
		if err := c.addJSONString(schema.Type); err != nil {
			return err
		}
	}
	if len(schema.Properties) > 0 {
		if err := c.addJSONObjectField(&first, "properties"); err != nil {
			return err
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		firstProperty := true
		for name, property := range schema.Properties {
			if err := c.addJSONObjectField(&firstProperty, name); err != nil {
				return err
			}
			if err := c.addToolSchemaProperty(property, depth+1); err != nil {
				return err
			}
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
	}
	if len(schema.Required) > 0 {
		if err := c.addJSONObjectField(&first, "required"); err != nil {
			return err
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		for index, required := range schema.Required {
			if err := c.checkContext(); err != nil {
				return err
			}
			if index > 0 {
				if err := c.addBytes(1); err != nil {
					return err
				}
			}
			if err := c.addJSONString(required); err != nil {
				return err
			}
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addToolSchemaProperty(property tool.SchemaProperty, depth int) error {
	if depth > requestJSONMaxDepth {
		return errors.New("request tool schema exceeds maximum depth")
	}
	if err := c.visitSchemaNode(); err != nil {
		return err
	}
	if err := c.addBytes(1); err != nil {
		return err
	}
	first := true
	if err := c.addJSONObjectField(&first, "type"); err != nil {
		return err
	}
	if err := c.addJSONString(property.Type); err != nil {
		return err
	}
	if property.Description != "" {
		if err := c.addJSONObjectField(&first, "description"); err != nil {
			return err
		}
		if err := c.addJSONString(property.Description); err != nil {
			return err
		}
	}
	if len(property.Enum) > 0 {
		if err := c.addJSONObjectField(&first, "enum"); err != nil {
			return err
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
		for index, value := range property.Enum {
			if err := c.checkContext(); err != nil {
				return err
			}
			if index > 0 {
				if err := c.addBytes(1); err != nil {
					return err
				}
			}
			if err := c.addJSONString(value); err != nil {
				return err
			}
		}
		if err := c.addBytes(1); err != nil {
			return err
		}
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addJSONObjectField(first *bool, name string) error {
	if err := c.checkContext(); err != nil {
		return err
	}
	if !*first {
		if err := c.addBytes(1); err != nil {
			return err
		}
	}
	*first = false
	if err := c.addJSONString(name); err != nil {
		return err
	}
	return c.addBytes(1)
}

func (c *requestByteCounter) addRawJSON(raw json.RawMessage) error {
	if raw == nil {
		return c.addBytes(4)
	}
	if err := c.addBytes(int64(len(raw))); err != nil {
		return err
	}
	parser := requestRawJSONParser{counter: c, raw: raw}
	if err := parser.validate(); err != nil {
		return err
	}
	return nil
}

type requestRawJSONParser struct {
	counter *requestByteCounter
	raw     []byte
	index   int
}

func (p *requestRawJSONParser) validate() error {
	if len(p.raw) == 0 {
		return errors.New("request raw JSON is empty")
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if err := p.parseValue(0); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.index != len(p.raw) {
		return errors.New("request raw JSON has trailing data")
	}
	return nil
}

func (p *requestRawJSONParser) parseValue(depth int) error {
	if depth > requestJSONMaxDepth {
		return errors.New("request raw JSON exceeds maximum depth")
	}
	if err := p.counter.visitSchemaNode(); err != nil {
		return err
	}
	if p.index >= len(p.raw) {
		return errors.New("request raw JSON value is incomplete")
	}
	switch p.raw[p.index] {
	case '{':
		return p.parseObject(depth)
	case '[':
		return p.parseArray(depth)
	case '"':
		return p.parseString()
	case 't':
		return p.parseLiteral("true")
	case 'f':
		return p.parseLiteral("false")
	case 'n':
		return p.parseLiteral("null")
	default:
		return p.parseNumber()
	}
}

func (p *requestRawJSONParser) parseObject(depth int) error {
	if err := p.advance(1); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.take('}') {
		return p.advance(1)
	}
	for {
		if err := p.counter.checkContext(); err != nil {
			return err
		}
		if p.index >= len(p.raw) || p.raw[p.index] != '"' {
			return errors.New("request raw JSON object key is invalid")
		}
		if err := p.parseString(); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		if !p.take(':') {
			return errors.New("request raw JSON object is missing colon")
		}
		if err := p.advance(1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		if err := p.parseValue(depth + 1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		if p.take('}') {
			return p.advance(1)
		}
		if !p.take(',') {
			return errors.New("request raw JSON object separator is invalid")
		}
		if err := p.advance(1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
	}
}

func (p *requestRawJSONParser) parseArray(depth int) error {
	if err := p.advance(1); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.take(']') {
		return p.advance(1)
	}
	for {
		if err := p.counter.checkContext(); err != nil {
			return err
		}
		if err := p.parseValue(depth + 1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		if p.take(']') {
			return p.advance(1)
		}
		if !p.take(',') {
			return errors.New("request raw JSON array separator is invalid")
		}
		if err := p.advance(1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
	}
}

func (p *requestRawJSONParser) parseString() error {
	if !p.take('"') {
		return errors.New("request raw JSON string is invalid")
	}
	if err := p.advance(1); err != nil {
		return err
	}
	for p.index < len(p.raw) {
		value := p.raw[p.index]
		if value == '"' {
			return p.advance(1)
		}
		if value < 0x20 {
			return errors.New("request raw JSON string contains a control byte")
		}
		if value != '\\' {
			if err := p.advance(1); err != nil {
				return err
			}
			continue
		}
		if err := p.advance(1); err != nil {
			return err
		}
		if p.index >= len(p.raw) {
			return errors.New("request raw JSON escape is incomplete")
		}
		escape := p.raw[p.index]
		if escape == 'u' {
			if len(p.raw)-p.index < 5 {
				return errors.New("request raw JSON unicode escape is incomplete")
			}
			for offset := 1; offset <= 4; offset++ {
				if !isJSONHex(p.raw[p.index+offset]) {
					return errors.New("request raw JSON unicode escape is invalid")
				}
			}
			if err := p.advance(5); err != nil {
				return err
			}
			continue
		}
		if escape != '"' && escape != '\\' && escape != '/' && escape != 'b' && escape != 'f' && escape != 'n' && escape != 'r' && escape != 't' {
			return errors.New("request raw JSON escape is invalid")
		}
		if err := p.advance(1); err != nil {
			return err
		}
	}
	return errors.New("request raw JSON string is incomplete")
}

func (p *requestRawJSONParser) parseLiteral(literal string) error {
	if len(p.raw)-p.index < len(literal) {
		return errors.New("request raw JSON literal is incomplete")
	}
	for offset := 0; offset < len(literal); offset++ {
		if p.raw[p.index+offset] != literal[offset] {
			return errors.New("request raw JSON literal is invalid")
		}
	}
	return p.advance(len(literal))
}

func (p *requestRawJSONParser) parseNumber() error {
	start := p.index
	if p.take('-') {
		if err := p.advance(1); err != nil {
			return err
		}
	}
	if p.index >= len(p.raw) {
		return errors.New("request raw JSON number is incomplete")
	}
	if p.take('0') {
		if err := p.advance(1); err != nil {
			return err
		}
		if p.index < len(p.raw) && isJSONDigit(p.raw[p.index]) {
			return errors.New("request raw JSON number has a leading zero")
		}
	} else {
		if p.raw[p.index] < '1' || p.raw[p.index] > '9' {
			return errors.New("request raw JSON number is invalid")
		}
		for p.index < len(p.raw) && isJSONDigit(p.raw[p.index]) {
			if err := p.advance(1); err != nil {
				return err
			}
		}
	}
	if p.take('.') {
		if err := p.advance(1); err != nil {
			return err
		}
		fraction := p.index
		for p.index < len(p.raw) && isJSONDigit(p.raw[p.index]) {
			if err := p.advance(1); err != nil {
				return err
			}
		}
		if p.index == fraction {
			return errors.New("request raw JSON number fraction is invalid")
		}
	}
	if p.take('e') || p.take('E') {
		if err := p.advance(1); err != nil {
			return err
		}
		if p.take('+') || p.take('-') {
			if err := p.advance(1); err != nil {
				return err
			}
		}
		exponent := p.index
		for p.index < len(p.raw) && isJSONDigit(p.raw[p.index]) {
			if err := p.advance(1); err != nil {
				return err
			}
		}
		if p.index == exponent {
			return errors.New("request raw JSON number exponent is invalid")
		}
	}
	if p.index == start {
		return errors.New("request raw JSON number is invalid")
	}
	return nil
}

func (p *requestRawJSONParser) skipWhitespace() error {
	for p.index < len(p.raw) {
		switch p.raw[p.index] {
		case ' ', '\t', '\n', '\r':
			if err := p.advance(1); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *requestRawJSONParser) take(value byte) bool {
	return p.index < len(p.raw) && p.raw[p.index] == value
}

func (p *requestRawJSONParser) advance(bytes int) error {
	if bytes < 0 || bytes > len(p.raw)-p.index {
		return errors.New("request raw JSON parser overflow")
	}
	if err := p.counter.consumeRawBytes(bytes); err != nil {
		return err
	}
	p.index += bytes
	return nil
}

func isJSONDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isJSONHex(value byte) bool {
	return isJSONDigit(value) || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func checkedAddRequestValue(left int64, right int64) (int64, error) {
	if left < 0 || right < 0 {
		return 0, errors.New("request measure is negative")
	}
	if right > math.MaxInt64-left {
		return 0, errors.New("request measure addition overflow")
	}
	return left + right, nil
}

func checkedMultiplyRequestValue(value int64, factor int64) (int64, error) {
	if value < 0 || factor < 0 {
		return 0, errors.New("request measure multiplication is negative")
	}
	if value == 0 || factor == 0 {
		return 0, nil
	}
	if factor > math.MaxInt64/value {
		return 0, errors.New("request measure multiplication overflow")
	}
	return value * factor, nil
}

// CompactionObserver observes a single real compaction attempt. The token
// returned by Before is request-local and is passed back unchanged to After.
// Implementations must not mutate the Conversation through this interface.
type CompactionObserver interface {
	Before(context.Context, Attempt) any
	After(context.Context, any, Result, error)
}

type Attempt struct {
	Reason          string
	Messages        int
	EstimatedTokens int64
}

type PrepareOptions struct {
	Mode             Mode
	PersistArtifacts bool
	Observer         CompactionObserver
}

type Manager struct {
	provider          provider.Provider
	redactor          *redact.RuntimeRedactor
	cfg               config.ContextConfig
	inlineOutputBytes int64
}

type ManagerOptions struct {
	Context           config.ContextConfig
	InlineOutputBytes int64
	RuntimeRedactor   *redact.RuntimeRedactor
}

type ToolResultProjection struct {
	ModelContent     redact.SafeText
	UserView         tool.UserView
	PersistedContent redact.SafeText
	OutputMeta       tool.OutputMeta
}

type Result struct {
	Changed      bool
	Externalized int
	Summarized   bool
	// Estimated retains the legacy pre-summary estimate returned by Prepare.
	Estimated int64
	// AfterMessages and AfterEstimatedTokens describe the post-attempt view
	// delivered to CompactionObserver.After without changing legacy fields.
	AfterMessages        int
	AfterEstimatedTokens int64
	Message              string
	CircuitBroken        bool
}

func New(providerImpl provider.Provider, options ManagerOptions) (*Manager, error) {
	if options.RuntimeRedactor == nil {
		return nil, errors.New("context manager runtime redactor is unavailable")
	}
	if err := validateContextOptions(options.Context); err != nil {
		return nil, err
	}
	if options.InlineOutputBytes < 1 || options.InlineOutputBytes > 1<<20 {
		return nil, errors.New("context manager inline output limit is invalid")
	}
	if config.Enabled(options.Context.Enabled, true) && providerImpl == nil {
		return nil, errors.New("context manager provider is unavailable")
	}
	return &Manager{
		provider:          providerImpl,
		redactor:          options.RuntimeRedactor,
		cfg:               options.Context,
		inlineOutputBytes: options.InlineOutputBytes,
	}, nil
}

func validateContextOptions(cfg config.ContextConfig) error {
	const (
		oneMiB  = 1 << 20
		fourMiB = 4 << 20
	)
	if cfg.ToolResultThresholdChars < 1 || cfg.ToolResultThresholdChars > oneMiB ||
		cfg.ToolResultsThresholdChars < 1 || cfg.ToolResultsThresholdChars > fourMiB ||
		cfg.ModelWindowTokens < 2 || cfg.ModelWindowTokens > 10_000_000 ||
		cfg.AutoMarginTokens < 1 || cfg.AutoMarginTokens > 1_000_000 ||
		cfg.ManualMarginTokens < 1 || cfg.ManualMarginTokens > 1_000_000 ||
		cfg.RecentKeepTokens < 1 || cfg.RecentKeepTokens > 1_000_000 ||
		cfg.RecentKeepMessages < 1 || cfg.RecentKeepMessages > 10_000 ||
		cfg.SummaryFailureLimit < 1 || cfg.SummaryFailureLimit > 100 ||
		cfg.PreviewChars < 1 || cfg.PreviewChars > oneMiB ||
		cfg.ToolResultThresholdChars > cfg.ToolResultsThresholdChars ||
		cfg.AutoMarginTokens >= cfg.ModelWindowTokens ||
		cfg.ManualMarginTokens >= cfg.ModelWindowTokens ||
		cfg.RecentKeepTokens >= cfg.ModelWindowTokens {
		return errors.New("context manager context options are invalid")
	}
	return nil
}

func validResultStatus(status tool.ResultStatus) bool {
	switch status {
	case tool.StatusSuccess, tool.StatusError, tool.StatusDenied, tool.StatusTimeout:
		return true
	default:
		return false
	}
}

func validOpaqueArtifactID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for index := 0; index < len(id); index++ {
		if id[index] < '0' || id[index] > '9' {
			if id[index] < 'a' || id[index] > 'f' {
				return false
			}
		}
	}
	return true
}

func validIncompleteReason(reason string) bool {
	switch reason {
	case "capture_hard_limit", "artifact_hard_limit", "capture_write_failure", "capture_canceled":
		return true
	default:
		return false
	}
}

func (m *Manager) Prepare(ctx context.Context, conv *conversation.Conversation, mode Mode) (Result, error) {
	return m.PrepareWithOptions(ctx, conv, PrepareOptions{Mode: mode})
}

func (m *Manager) PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts PrepareOptions) (result Result, err error) {
	if m == nil || conv == nil || !config.Enabled(m.cfg.Enabled, true) {
		return result, nil
	}
	if opts.Mode == "" {
		opts.Mode = ModeAuto
	}
	preflight := m.preflight(conv, opts)
	var attemptErr error
	var observerToken any
	if preflight.realAttempt {
		observerToken = notifyCompactionBefore(ctx, opts.Observer, preflight.attempt)
	}
	defer func() {
		result.AfterMessages = len(contextMessages(conv))
		result.AfterEstimatedTokens = EstimateConversationTokens(conv)
		if result.Summarized {
			result.AfterEstimatedTokens = int64(countConversationChars(conv) / 4)
		}
		if preflight.realAttempt {
			notifyCompactionAfter(ctx, opts.Observer, observerToken, result, attemptErr)
		}
	}()

	estimated := EstimateConversationTokens(conv)
	result.Estimated = estimated
	meta := ensureContext(conv)
	meta.LastEstimatedTokens = estimated
	meta.LastEstimatedCharacters = countConversationChars(conv)

	if !m.shouldSummarize(conv, estimated, opts.Mode) {
		return result, nil
	}
	if meta.SummaryFailureCount >= m.cfg.SummaryFailureLimit {
		result.CircuitBroken = true
		if opts.Mode == ModeManual {
			attemptErr = fmt.Errorf("上下文摘要连续失败 %d 次，已熔断", meta.SummaryFailureCount)
			return result, attemptErr
		}
		return result, nil
	}

	summary, cutoff, summaryErr := m.summarize(ctx, conv)
	if summaryErr != nil {
		attemptErr = summaryErr
		meta.SummaryFailureCount++
		if opts.Mode == ModeManual || meta.SummaryFailureCount >= m.cfg.SummaryFailureLimit {
			return result, summaryErr
		}
		return result, nil
	}
	m.applySummary(conv, summary, cutoff)
	result.Changed = true
	result.Summarized = true
	result.Message = "上下文已压缩并生成结构化摘要"
	return result, nil
}

func (m *Manager) CompactNow(ctx context.Context, conv *conversation.Conversation) (Result, error) {
	return m.PrepareWithOptions(ctx, conv, PrepareOptions{Mode: ModeManual})
}

type compactionPreflight struct {
	attempt     Attempt
	realAttempt bool
}

func (m *Manager) preflight(conv *conversation.Conversation, opts PrepareOptions) compactionPreflight {
	estimated := EstimateConversationTokens(conv)
	plan := compactionPreflight{attempt: Attempt{
		Reason:          string(opts.Mode),
		Messages:        len(contextMessages(conv)),
		EstimatedTokens: estimated,
	}}
	summaryAttempt := m.shouldSummarize(conv, estimated, opts.Mode)
	if conv.Context != nil && conv.Context.SummaryFailureCount >= m.cfg.SummaryFailureLimit {
		summaryAttempt = false
	}
	plan.realAttempt = opts.Mode == ModeManual || summaryAttempt
	return plan
}

func notifyCompactionBefore(ctx context.Context, observer CompactionObserver, attempt Attempt) (token any) {
	if observer == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			token = nil
		}
	}()
	return observer.Before(ctx, attempt)
}

func notifyCompactionAfter(ctx context.Context, observer CompactionObserver, token any, result Result, err error) {
	if observer == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	observer.After(ctx, token, result, err)
}

func (m *Manager) ProjectToolResult(result tool.Result) (ToolResultProjection, error) {
	modelContent := result.ModelContent()
	userView := result.UserView()
	persistedContent := result.PersistedContent()
	outputMeta := result.OutputMeta()

	projection := ToolResultProjection{
		ModelContent:     modelContent,
		UserView:         userView,
		PersistedContent: persistedContent,
		OutputMeta:       outputMeta,
	}
	if m == nil || m.redactor == nil || m.inlineOutputBytes < 1 || m.inlineOutputBytes > 1<<20 {
		return ToolResultProjection{}, errors.New("context manager is unavailable")
	}
	if modelContent.Text() == "" || persistedContent.Text() == "" ||
		!utf8.ValidString(modelContent.Text()) || !utf8.ValidString(persistedContent.Text()) ||
		!utf8.ValidString(userView.Summary.Text()) || !utf8.ValidString(userView.Preview.Text()) ||
		!utf8.ValidString(userView.TruncationReason.Text()) {
		return ToolResultProjection{}, errors.New("tool result safe views are invalid")
	}
	if !userView.State.CanProduceResult() || !validResultStatus(userView.Status) {
		return ToolResultProjection{}, errors.New("tool result state is invalid")
	}
	if userView.Error != nil && !utf8.ValidString(userView.Error.Message.Text()) {
		return ToolResultProjection{}, errors.New("tool result safe error is invalid")
	}
	if int64(len(userView.Preview.Text())) > m.inlineOutputBytes || outputMeta.CapturedBytes < 0 {
		return ToolResultProjection{}, errors.New("tool result output bounds are invalid")
	}
	if userView.Truncated != outputMeta.Truncated ||
		userView.TruncationReason.Text() != outputMeta.TruncationReason.Text() ||
		(userView.Artifact == nil) != (outputMeta.Artifact == nil) {
		return ToolResultProjection{}, errors.New("tool result projections are inconsistent")
	}
	if userView.Artifact == nil {
		if outputMeta.CapturedBytes > m.inlineOutputBytes || outputMeta.Truncated || outputMeta.TruncationReason.Text() != "" {
			return ToolResultProjection{}, errors.New("inline tool result metadata is invalid")
		}
		return projection, nil
	}
	if *userView.Artifact != *outputMeta.Artifact ||
		!validOpaqueArtifactID(outputMeta.Artifact.ID) ||
		outputMeta.CapturedBytes <= 0 || outputMeta.Artifact.Bytes != outputMeta.CapturedBytes ||
		outputMeta.Artifact.CreatedAt.IsZero() || !outputMeta.Artifact.Available ||
		!outputMeta.Truncated {
		return ToolResultProjection{}, errors.New("artifact tool result metadata is invalid")
	}
	reason := outputMeta.TruncationReason.Text()
	if outputMeta.Artifact.Complete {
		if outputMeta.CapturedBytes <= m.inlineOutputBytes || reason != "inline_preview_limit" {
			return ToolResultProjection{}, errors.New("complete artifact truncation reason is invalid")
		}
	} else if !validIncompleteReason(reason) {
		return ToolResultProjection{}, errors.New("incomplete artifact truncation reason is invalid")
	}
	return projection, nil
}

func (m *Manager) UpdateUsage(conv *conversation.Conversation, usage provider.Usage) error {
	if m == nil {
		return errors.New("context manager is unavailable")
	}
	if conv == nil {
		return errors.New("context manager conversation is unavailable")
	}
	values := [...]int64{usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens}
	var total int64
	for _, value := range values {
		if value < 0 || value > int64(^uint64(0)>>1)-total {
			return errors.New("provider usage snapshot is invalid")
		}
		total += value
	}
	meta := ensureContext(conv)
	meta.LastInputTokens = usage.InputTokens
	meta.LastOutputTokens = usage.OutputTokens
	meta.LastEstimatedTokens = usage.InputTokens
	meta.LastEstimatedCharacters = countConversationChars(conv)
	return nil
}

func (m *Manager) shouldSummarize(conv *conversation.Conversation, estimated int64, mode Mode) bool {
	margin := m.cfg.AutoMarginTokens
	if mode == ModeManual {
		margin = m.cfg.ManualMarginTokens
	}
	if mode == ModeManual {
		return len(conv.Messages) > m.cfg.RecentKeepMessages
	}
	return estimated >= m.cfg.ModelWindowTokens-margin
}

func (m *Manager) summarize(ctx context.Context, conv *conversation.Conversation) (summary string, summarized int, err error) {
	cutoff := summaryCutoff(conv.Messages, m.cfg.RecentKeepTokens, m.cfg.RecentKeepMessages)
	if cutoff <= 0 {
		return "", 0, fmt.Errorf("没有可摘要的早期上下文")
	}
	prompt := summaryPrompt(conv.Messages[:cutoff])
	stream, err := m.provider.StreamChat(ctx, provider.ChatRequest{Messages: []provider.ModelMessage{{
		Role:    provider.ModelMessageRoleUser,
		Content: m.redactor.Redact(prompt),
	}}})
	if err != nil {
		return "", 0, fmt.Errorf("请求上下文摘要失败: %w", err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), providerStreamCloseWait)
		defer cancelClose()
		if closeErr := stream.Close(closeCtx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭上下文摘要流失败: %w", closeErr))
			if err != nil {
				summary = ""
				summarized = 0
			}
		}
	}()
	var builder strings.Builder
	for event := range stream.Events() {
		switch event.Type {
		case provider.StreamEventTextDelta:
			builder.WriteString(event.Delta.Text())
		case provider.StreamEventToolCall:
			return "", 0, fmt.Errorf("摘要请求禁止工具调用")
		case provider.StreamEventError:
			if event.Error == nil {
				return "", 0, fmt.Errorf("上下文摘要返回空错误事件")
			}
			return "", 0, event.Error
		case provider.StreamEventDone:
			summary := extractFinalSummary(builder.String())
			if strings.TrimSpace(summary) == "" {
				return "", 0, fmt.Errorf("摘要结果为空")
			}
			return summary, cutoff, nil
		}
	}
	return "", 0, fmt.Errorf("摘要流异常结束")
}

func (m *Manager) applySummary(conv *conversation.Conversation, summary string, cutoff int) {
	boundary := "上下文已压缩。摘要只提供线索，不是完整事实来源；如需代码、工具输出或文件细节，请重新读取对应文件或外置工具结果，不要根据摘要脑补。"
	now := time.Now()
	kept := append([]conversation.Message{}, conv.Messages[cutoff:]...)
	conv.Messages = nil
	conv.Messages = append(conv.Messages,
		conversation.Message{Role: conversation.RoleContextSummary, Content: m.redactor.Redact(summary), CreatedAt: now},
		conversation.Message{Role: conversation.RoleContextBoundary, Content: m.redactor.Redact(boundary), CreatedAt: now},
	)
	conv.Messages = append(conv.Messages, kept...)
	meta := ensureContext(conv)
	meta.Summary = m.redactor.Redact(summary)
	meta.LastBoundary = m.redactor.Redact(boundary)
	meta.SummaryFailureCount = 0
	meta.LastCompressionAt = &now
	conv.UpdatedAt = now
}

func EstimateConversationTokens(conv *conversation.Conversation) int64 {
	if conv == nil {
		return 0
	}
	chars := countConversationChars(conv)
	meta := conv.Context
	if meta == nil {
		return int64(chars / 4)
	}
	if meta.LastInputTokens > 0 && meta.LastEstimatedCharacters > 0 {
		delta := chars - meta.LastEstimatedCharacters
		if delta < 0 {
			delta = 0
		}
		return meta.LastInputTokens + int64(delta/4)
	}
	return int64(chars / 4)
}

func countConversationChars(conv *conversation.Conversation) int {
	if conv == nil {
		return 0
	}
	count := 0
	for _, message := range contextMessages(conv) {
		count += safeMessageSize(message)
	}
	return count
}

func summaryCutoff(messages []conversation.Message, keepTokens int64, keepMessages int) int {
	if len(messages) <= keepMessages {
		return 0
	}
	keepChars := int(keepTokens * 4)
	chars := 0
	cutoff := len(messages)
	for index := len(messages) - 1; index >= 0; index-- {
		chars += safeMessageSize(messages[index])
		if len(messages)-index >= keepMessages && chars >= keepChars {
			cutoff = index
			break
		}
	}
	if cutoff == len(messages) {
		cutoff = len(messages) - keepMessages
	}
	for cutoff < len(messages) && messages[cutoff].Role == conversation.RoleToolResult {
		cutoff++
	}
	if cutoff <= 0 || cutoff >= len(messages) {
		return 0
	}
	return cutoff
}

func summaryPrompt(messages []conversation.Message) string {
	type summaryTool struct {
		CallID    string            `json:"call_id"`
		Name      string            `json:"name"`
		Arguments string            `json:"arguments,omitempty"`
		Status    tool.ResultStatus `json:"status,omitempty"`
		Summary   string            `json:"summary,omitempty"`
		Result    string            `json:"result,omitempty"`
	}
	type summaryMessage struct {
		Role    conversation.MessageRole `json:"role"`
		Content string                   `json:"content"`
		Tool    *summaryTool             `json:"tool,omitempty"`
	}
	safe := make([]summaryMessage, len(messages))
	for index := range messages {
		safe[index] = summaryMessage{Role: messages[index].Role, Content: messages[index].Content.Text()}
		if messages[index].Tool != nil {
			safe[index].Tool = &summaryTool{
				CallID: messages[index].Tool.CallID, Name: messages[index].Tool.Name,
				Arguments: messages[index].Tool.ArgumentsJSON.Text(), Status: messages[index].Tool.Status,
				Summary: messages[index].Tool.Summary.Text(), Result: messages[index].Tool.Result.Text(),
			}
		}
	}
	data, _ := json.MarshalIndent(safe, "", "  ")
	return "你正在为 XAgent 压缩较早的对话上下文。禁止调用任何工具；只基于下面提供的消息生成摘要。先在内部写分析草稿，再输出正式摘要；最终回复只能包含正式摘要，不要包含草稿。\n\n正式摘要必须使用固定章节：\n1. 当前目标\n2. 已完成事项\n3. 关键决策与约束\n4. 重要文件/符号线索\n5. 工具结果与外置文件索引\n6. 未完成任务/下一步\n7. 风险与不能臆测的内容\n\n较早消息 JSON：\n" + string(data)
}

func extractFinalSummary(text string) string {
	text = strings.TrimSpace(text)
	for _, marker := range []string{"<summary>", "正式摘要：", "正式摘要:"} {
		if index := strings.LastIndex(text, marker); index >= 0 {
			text = strings.TrimSpace(text[index+len(marker):])
		}
	}
	text = strings.TrimSuffix(text, "</summary>")
	return strings.TrimSpace(text)
}

func ensureContext(conv *conversation.Conversation) *conversation.ContextMetadata {
	if conv == nil {
		return &conversation.ContextMetadata{}
	}
	if conv.Context == nil {
		conv.Context = &conversation.ContextMetadata{}
	}
	return conv.Context
}

func contextMessages(conv *conversation.Conversation) []conversation.Message {
	if conv == nil {
		return nil
	}
	result := make([]conversation.Message, 0, len(conv.Messages))
	for _, message := range conv.Messages {
		switch message.Role {
		case conversation.RoleUser, conversation.RoleAssistant, conversation.RoleToolCall,
			conversation.RoleToolResult, conversation.RoleContextSummary, conversation.RoleContextBoundary:
			result = append(result, message)
		}
	}
	return result
}

func safeMessageSize(message conversation.Message) int {
	size := len(message.Content.Text())
	if message.Tool != nil {
		size += len(message.Tool.ArgumentsJSON.Text()) + len(message.Tool.Result.Text()) + len(message.Tool.Summary.Text())
	}
	return size
}
