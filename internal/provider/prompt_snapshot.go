package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"xagent/internal/config"
	"xagent/internal/tool"
)

var ErrInvalidPromptPrefixSnapshot = errors.New("provider prompt prefix snapshot invalid")

// PromptPrefixSnapshot freezes the exact, already-budgeted Provider prefix
// used at a delegation boundary. It carries detached DTOs only: registries,
// observers, and executable capabilities are deliberately absent.
type PromptPrefixSnapshot struct {
	OrderedSystem   bool
	Model           string
	System          []SystemBlock
	StableSystem    []SystemBlock
	DynamicSystem   []SystemBlock
	Messages        []ModelMessage
	Tools           []ToolDefinition
	Thinking        config.ThinkingConfig
	Cache           CachePolicy
	MessagePrefix   int
	ToolFingerprint string
	Fingerprint     string
}

func CapturePromptPrefix(request ChatRequest) (PromptPrefixSnapshot, error) {
	if err := request.Validate(); err != nil {
		return PromptPrefixSnapshot{}, err
	}
	ordered := request.System != nil
	snapshot := PromptPrefixSnapshot{
		OrderedSystem: ordered,
		Model:         strings.TrimSpace(request.Model),
		Messages:      cloneModelMessages(request.Messages),
		Tools:         cloneToolDefinitions(nonEmptyToolDefinitions(toolDefinitions(request))),
		Thinking:      request.Thinking,
		Cache:         request.Cache,
		MessagePrefix: len(request.Messages),
	}
	if ordered {
		snapshot.System = normalizedSystemBlocks(request.System)
	} else {
		snapshot.StableSystem = normalizedSystemBlocks(request.StableSystem)
		snapshot.DynamicSystem = normalizedSystemBlocks(request.DynamicSystem)
	}
	var err error
	snapshot.ToolFingerprint, err = toolDefinitionsFingerprint(snapshot.Tools)
	if err != nil {
		return PromptPrefixSnapshot{}, ErrInvalidPromptPrefixSnapshot
	}
	snapshot.Fingerprint, err = promptSnapshotFingerprint(snapshot)
	if err != nil {
		return PromptPrefixSnapshot{}, ErrInvalidPromptPrefixSnapshot
	}
	if err := snapshot.Validate(); err != nil {
		return PromptPrefixSnapshot{}, err
	}
	return snapshot, nil
}

func (snapshot PromptPrefixSnapshot) Validate() error {
	if snapshot.MessagePrefix < 0 || snapshot.MessagePrefix != len(snapshot.Messages) {
		return ErrInvalidPromptPrefixSnapshot
	}
	if snapshot.OrderedSystem {
		if snapshot.System == nil || snapshot.StableSystem != nil || snapshot.DynamicSystem != nil {
			return ErrInvalidPromptPrefixSnapshot
		}
	} else if snapshot.System != nil {
		return ErrInvalidPromptPrefixSnapshot
	}
	request := snapshot.baseRequest()
	if err := request.Validate(); err != nil {
		return ErrInvalidPromptPrefixSnapshot
	}
	toolFingerprint, err := toolDefinitionsFingerprint(snapshot.Tools)
	if err != nil || toolFingerprint != snapshot.ToolFingerprint {
		return ErrInvalidPromptPrefixSnapshot
	}
	fingerprint, err := promptSnapshotFingerprint(snapshot)
	if err != nil || fingerprint != snapshot.Fingerprint {
		return ErrInvalidPromptPrefixSnapshot
	}
	return nil
}

// BuildChild derives a request without rereading parent state. Appended system
// blocks are always dynamic so role/task text cannot move the parent's stable
// cache boundary. A changed tool fingerprint disables only tool caching.
func (snapshot PromptPrefixSnapshot) BuildChild(
	appendedSystem []SystemBlock,
	appendedMessages []ModelMessage,
	tools []ToolDefinition,
) ChatRequest {
	request := snapshot.baseRequest()
	appended := cloneSystemBlocks(appendedSystem)
	for index := range appended {
		appended[index].Cacheable = false
	}
	if snapshot.OrderedSystem {
		request.System = append(request.System, appended...)
	} else {
		request.DynamicSystem = append(request.DynamicSystem, appended...)
	}
	request.Messages = append(request.Messages, cloneModelMessages(appendedMessages)...)
	request.Tools = cloneToolDefinitions(tools)
	request.ToolDefs = nil
	request.Observer = nil
	childToolFingerprint, err := toolDefinitionsFingerprint(request.Tools)
	if err != nil || childToolFingerprint != snapshot.ToolFingerprint {
		request.Cache.CacheTools = false
	}
	return request
}

func (snapshot PromptPrefixSnapshot) baseRequest() ChatRequest {
	request := ChatRequest{
		Model: snapshot.Model, Messages: cloneModelMessages(snapshot.Messages), Tools: cloneToolDefinitions(snapshot.Tools),
		Thinking: snapshot.Thinking, Cache: snapshot.Cache,
	}
	if snapshot.OrderedSystem {
		request.System = cloneSystemBlocks(snapshot.System)
	} else {
		request.StableSystem = cloneSystemBlocks(snapshot.StableSystem)
		request.DynamicSystem = cloneSystemBlocks(snapshot.DynamicSystem)
	}
	return request
}

func normalizedSystemBlocks(blocks []SystemBlock) []SystemBlock {
	if blocks == nil {
		return nil
	}
	return cloneSystemBlocks(nonEmptySystemBlocks(blocks))
}

func cloneSystemBlocks(blocks []SystemBlock) []SystemBlock {
	if blocks == nil {
		return nil
	}
	return append([]SystemBlock(nil), blocks...)
}

func cloneModelMessages(messages []ModelMessage) []ModelMessage {
	if messages == nil {
		return nil
	}
	return append([]ModelMessage(nil), messages...)
}

func cloneToolDefinitions(definitions []ToolDefinition) []ToolDefinition {
	if definitions == nil {
		return nil
	}
	cloned := make([]ToolDefinition, len(definitions))
	for index, definition := range definitions {
		cloned[index] = definition
		cloned[index].Schema = cloneProviderToolSchema(definition.Schema)
	}
	return cloned
}

func cloneProviderToolSchema(schema tool.Schema) tool.Schema {
	cloned := tool.Schema{
		Type:     schema.Type,
		Required: append([]string(nil), schema.Required...),
		Raw:      append(json.RawMessage(nil), schema.Raw...),
	}
	if schema.Properties != nil {
		cloned.Properties = make(map[string]tool.SchemaProperty, len(schema.Properties))
		for name, property := range schema.Properties {
			property.Enum = append([]string(nil), property.Enum...)
			cloned.Properties[name] = property
		}
	}
	return cloned
}

type promptSystemFingerprintBlock struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	Cacheable bool   `json:"cacheable"`
}

type promptMessageFingerprint struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ToolCallID       string `json:"tool_call_id"`
	ToolName         string `json:"tool_name"`
	ArgumentsJSON    string `json:"arguments_json"`
	ToolResult       string `json:"tool_result"`
	ToolResultStatus string `json:"tool_result_status"`
}

type promptToolFingerprint struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type promptSnapshotFingerprintPayload struct {
	Version         int                            `json:"version"`
	OrderedSystem   bool                           `json:"ordered_system"`
	Model           string                         `json:"model"`
	System          []promptSystemFingerprintBlock `json:"system"`
	StableSystem    []promptSystemFingerprintBlock `json:"stable_system"`
	DynamicSystem   []promptSystemFingerprintBlock `json:"dynamic_system"`
	Messages        []promptMessageFingerprint     `json:"messages"`
	Tools           []promptToolFingerprint        `json:"tools"`
	Thinking        config.ThinkingConfig          `json:"thinking"`
	Cache           CachePolicy                    `json:"cache"`
	MessagePrefix   int                            `json:"message_prefix"`
	ToolFingerprint string                         `json:"tool_fingerprint"`
}

func toolDefinitionsFingerprint(definitions []ToolDefinition) (string, error) {
	canonical, err := promptToolFingerprints(definitions)
	if err != nil {
		return "", err
	}
	return fingerprintJSON(struct {
		Version int                     `json:"version"`
		Tools   []promptToolFingerprint `json:"tools"`
	}{Version: 1, Tools: canonical})
}

func promptSnapshotFingerprint(snapshot PromptPrefixSnapshot) (string, error) {
	tools, err := promptToolFingerprints(snapshot.Tools)
	if err != nil {
		return "", err
	}
	return fingerprintJSON(promptSnapshotFingerprintPayload{
		Version: 1, OrderedSystem: snapshot.OrderedSystem, Model: snapshot.Model,
		System: promptSystemFingerprints(snapshot.System), StableSystem: promptSystemFingerprints(snapshot.StableSystem),
		DynamicSystem: promptSystemFingerprints(snapshot.DynamicSystem), Messages: promptMessageFingerprints(snapshot.Messages),
		Tools: tools, Thinking: snapshot.Thinking, Cache: snapshot.Cache, MessagePrefix: snapshot.MessagePrefix,
		ToolFingerprint: snapshot.ToolFingerprint,
	})
}

func promptSystemFingerprints(blocks []SystemBlock) []promptSystemFingerprintBlock {
	if blocks == nil {
		return nil
	}
	result := make([]promptSystemFingerprintBlock, len(blocks))
	for index, block := range blocks {
		result[index] = promptSystemFingerprintBlock{Name: block.Name, Content: block.Content.Text(), Cacheable: block.Cacheable}
	}
	return result
}

func promptMessageFingerprints(messages []ModelMessage) []promptMessageFingerprint {
	if messages == nil {
		return nil
	}
	result := make([]promptMessageFingerprint, len(messages))
	for index, message := range messages {
		result[index] = promptMessageFingerprint{
			Role: string(message.Role), Content: message.Content.Text(), ToolCallID: message.ToolCallID, ToolName: message.ToolName,
			ArgumentsJSON: message.ArgumentsJSON.Text(), ToolResult: message.ToolResult.Text(), ToolResultStatus: message.ToolResultStatus,
		}
	}
	return result
}

func promptToolFingerprints(definitions []ToolDefinition) ([]promptToolFingerprint, error) {
	if definitions == nil {
		return nil, nil
	}
	result := make([]promptToolFingerprint, len(definitions))
	for index, definition := range definitions {
		schema, err := json.Marshal(definition.Schema)
		if err != nil {
			return nil, err
		}
		result[index] = promptToolFingerprint{Name: definition.Name, Description: definition.Description, Schema: schema}
	}
	return result, nil
}

func fingerprintJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
