package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"xagent/internal/artifact"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/tool"
)

var errRemoteToolAdapterOptions = errors.New("MCP remote tool adapter options are invalid")

type ToolCaller interface {
	CallTool(ctx context.Context, registeredName string, arguments map[string]any) (protocol.CallToolResult, error)
}

// CapturedToolCaller is the production MCP call boundary.  It receives the
// operation-local Capture writer before any user output is decoded into the
// complete result DTO.  The legacy ToolCaller shape remains only for the
// explicit migration adapter.
type CapturedToolCaller interface {
	CallToolCaptured(ctx context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error)
}

// RemoteToolAdapterOptions contains the locally trusted inputs required to
// bind a discovered remote tool to one server configuration and one shared
// result-sanitization boundary.
type RemoteToolAdapterOptions struct {
	RegisteredName     string
	ServerName         string
	RemoteName         string
	Description        string
	Schema             tool.Schema
	ServerConfigDigest [32]byte
	RemoteAnnotations  json.RawMessage
	Caller             ToolCaller
	ResultFactory      *tool.ResultFactory
	Capture            func(context.Context, artifact.Metadata) (*tool.Capture, error)
}

type ToolAdapter struct {
	registeredName string
	serverName     string
	remoteName     string
	description    string
	schema         tool.Schema
	caller         ToolCaller
	resultFactory  *tool.ResultFactory
	capture        func(context.Context, artifact.Metadata) (*tool.Capture, error)

	serverConfigDigest [32]byte
	remoteAnnotations  json.RawMessage
	boundToServer      bool
	legacy             bool
}

// NewRemoteToolAdapter constructs the fail-closed adapter used by the staged
// MCP implementation. Its RegistrationOptions must be supplied to Registry so
// authorization identity binds the registered name, canonical arguments, and
// final server configuration digest.
func NewRemoteToolAdapter(options RemoteToolAdapterOptions) (ToolAdapter, error) {
	if options.Capture == nil {
		return ToolAdapter{}, errRemoteToolAdapterOptions
	}
	if _, ok := options.Caller.(CapturedToolCaller); !ok {
		return ToolAdapter{}, errRemoteToolAdapterOptions
	}
	return newRemoteToolAdapter(options, false)
}

// NewLegacyRemoteToolAdapter is the explicit pre-T4.29a migration boundary.
// It refuses a Capture and never produces ResultFactory safe views, so a
// candidate Registry cannot accept it.
func NewLegacyRemoteToolAdapter(options RemoteToolAdapterOptions) (ToolAdapter, error) {
	if options.Capture != nil {
		return ToolAdapter{}, errRemoteToolAdapterOptions
	}
	return newRemoteToolAdapter(options, true)
}

func newRemoteToolAdapter(options RemoteToolAdapterOptions, legacy bool) (ToolAdapter, error) {
	if !validRegisteredToolName(options.RegisteredName) ||
		strings.TrimSpace(options.ServerName) == "" ||
		strings.TrimSpace(options.RemoteName) == "" ||
		!validServerConfigDigest(options.ServerConfigDigest) ||
		options.Caller == nil ||
		options.ResultFactory == nil ||
		!validRemoteAnnotations(options.RemoteAnnotations) {
		return ToolAdapter{}, errRemoteToolAdapterOptions
	}
	return ToolAdapter{
		registeredName:     options.RegisteredName,
		serverName:         options.ServerName,
		remoteName:         options.RemoteName,
		description:        SanitizeMetadata(options.Description, 1024),
		schema:             cloneAdapterSchema(options.Schema),
		caller:             options.Caller,
		resultFactory:      options.ResultFactory,
		capture:            options.Capture,
		serverConfigDigest: options.ServerConfigDigest,
		remoteAnnotations:  append(json.RawMessage(nil), options.RemoteAnnotations...),
		boundToServer:      true,
		legacy:             legacy,
	}, nil
}

// RegistrationOptions returns detached, locally fail-closed registry inputs.
// A remote annotation can therefore remove capabilities but can never turn an
// MCP tool into a read-only or concurrently safe local tool.
func (a ToolAdapter) RegistrationOptions() tool.RegistrationOptions {
	options := tool.RegistrationOptions{
		Policy:            tool.ExecutionPolicy{},
		RemoteAnnotations: append(json.RawMessage(nil), a.remoteAnnotations...),
	}
	if a.boundToServer {
		digest := a.serverConfigDigest
		options.TargetDigest = &digest
	}
	return options
}

func (a ToolAdapter) Name() string {
	return a.registeredName
}

func (a ToolAdapter) Description() string {
	if a.description == "" {
		return SanitizeMetadata("MCP tool from "+a.serverName+": "+a.remoteName, 1024)
	}
	return a.description
}

func (a ToolAdapter) Schema() tool.Schema {
	return cloneAdapterSchema(a.schema)
}

func (a ToolAdapter) Risk() tool.Risk {
	return tool.RiskDangerous
}

func (a ToolAdapter) UsesSafeResultBoundary() bool {
	return !a.legacy && a.resultFactory != nil && a.capture != nil
}

func (a ToolAdapter) Execute(ctx context.Context, input tool.Input) tool.Result {
	if a.legacy {
		return a.executeLegacy(ctx, input)
	}
	if a.capture == nil || a.resultFactory == nil {
		return tool.Result{}
	}
	if ctx == nil || ctx.Err() != nil {
		return tool.Result{}
	}
	capturedOutput, err := a.capture(ctx, artifact.Metadata{MediaType: "text/plain"})
	if err != nil || capturedOutput == nil {
		return a.buildResult(tool.ResultFactoryInput{
			CallID: input.CallID, Name: a.registeredName, State: tool.Completed, Status: tool.StatusError,
			Summary: "MCP 输出采集不可用", Error: &tool.Error{Code: tool.ErrCommandFailed, Message: "MCP output capture is unavailable", Recoverable: true},
		})
	}
	arguments := cloneMCPArguments(input.Arguments)
	result, callErr := a.callCaptured(ctx, arguments, capturedOutput)
	if callErr != nil {
		captured, finishErr := capturedOutput.Finish(context.Background())
		inputResult := tool.ResultFactoryInput{CallID: input.CallID, Name: a.registeredName, State: tool.Completed, Status: tool.StatusError, Summary: "MCP 工具调用失败", Error: &tool.Error{Code: tool.ErrCommandFailed, Message: "MCP remote tool call failed", Recoverable: true}}
		if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callErr, context.Canceled) {
			inputResult.State, inputResult.Status = tool.CancelledAfterStart, tool.StatusTimeout
			inputResult.Summary = "MCP 工具调用已取消或超时"
			inputResult.Error = &tool.Error{Code: tool.ErrTimeout, Message: "MCP tool call was cancelled or timed out", Recoverable: true}
		}
		return a.buildCapturedResult(inputResult, captured, finishErr)
	}
	captured, finishErr := capturedOutput.Finish(context.Background())
	inputResult := tool.ResultFactoryInput{CallID: input.CallID, Name: a.registeredName, State: tool.Completed, Status: tool.StatusSuccess, Summary: "MCP 工具调用完成"}
	if result.IsError {
		inputResult.Status = tool.StatusError
		inputResult.Summary = "MCP 工具返回错误"
		inputResult.Error = &tool.Error{Code: tool.ErrCommandFailed, Message: "MCP remote tool returned an error", Recoverable: true}
	}
	if finishErr != nil {
		inputResult.Status = tool.StatusError
		inputResult.Summary = "MCP 工具输出未完整采集"
		inputResult.Error = &tool.Error{Code: tool.ErrCommandFailed, Message: "MCP output capture failed", Recoverable: true}
	}
	return a.buildCapturedResult(inputResult, captured, finishErr)
}

// executeLegacy is the isolated pre-T4.29a migration adapter. Its results have
// no safe views and therefore cannot pass ContextManager projection.
func (a ToolAdapter) executeLegacy(ctx context.Context, input tool.Input) tool.Result {
	arguments := cloneMCPArguments(input.Arguments)
	result, err := a.call(ctx, arguments)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return tool.Failure(input, tool.ErrTimeout, "MCP 工具调用已取消或超时", true)
		}
		return tool.Failure(input, tool.ErrCommandFailed, "MCP 工具调用失败", true)
	}
	preview := mcpResultPreview(result)
	if result.IsError {
		legacy := tool.Failure(input, tool.ErrCommandFailed, "MCP 工具返回错误", true)
		legacy.Content = preview
		return legacy
	}
	return tool.Success(input, "MCP 工具调用完成", preview, nil)
}

func (a ToolAdapter) call(ctx context.Context, arguments map[string]any) (protocol.CallToolResult, error) {
	if a.caller == nil {
		return protocol.CallToolResult{}, errRemoteToolAdapterOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.caller.CallTool(ctx, a.registeredName, arguments)
}

func (a ToolAdapter) callCaptured(ctx context.Context, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	caller, ok := a.caller.(CapturedToolCaller)
	if !ok || destination == nil {
		return protocol.CallToolResult{}, errRemoteToolAdapterOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return caller.CallToolCaptured(ctx, a.registeredName, arguments, destination)
}

func (a ToolAdapter) buildResult(input tool.ResultFactoryInput) tool.Result {
	if a.resultFactory == nil {
		return tool.Result{}
	}
	result, err := a.resultFactory.Build(input)
	if err != nil {
		return tool.Result{}
	}
	return result
}

func (a ToolAdapter) buildCapturedResult(input tool.ResultFactoryInput, captured tool.CaptureResult, finishErr error) tool.Result {
	if finishErr != nil && captured == (tool.CaptureResult{}) {
		input.Preview = ""
		input.Artifact = nil
		input.CapturedBytes = 0
		input.Truncated = false
		input.TruncationReason = ""
		if input.Status == tool.StatusSuccess {
			input.Status = tool.StatusError
		}
		if input.Error == nil {
			input.Error = &tool.Error{Code: tool.ErrCommandFailed, Message: "MCP output capture failed", Recoverable: true}
		}
		return a.buildResult(input)
	}
	input.Preview = captured.Preview
	input.Artifact = captured.Artifact
	input.CapturedBytes = captured.CapturedBytes
	input.Truncated = captured.Truncated
	input.TruncationReason = string(captured.TruncationReason)
	return a.buildResult(input)
}

func writeMCPResult(destination io.Writer, result protocol.CallToolResult) error {
	if destination == nil {
		return errors.New("MCP output capture is unavailable")
	}
	wrotePart := false
	writePart := func(value string) error {
		if wrotePart {
			if _, err := io.WriteString(destination, "\n"); err != nil {
				return err
			}
		}
		wrotePart = true
		_, err := io.WriteString(destination, value)
		return err
	}
	nonText := false
	for _, block := range result.Content {
		if block.Type == "text" {
			if err := writePart(block.Text); err != nil {
				return err
			}
			continue
		}
		nonText = true
	}
	if nonText {
		if err := writePart("[non-text MCP content omitted]"); err != nil {
			return err
		}
	}
	if result.StructuredContent != nil {
		return writePart("[structured MCP content omitted]")
	}
	return nil
}

// mcpResultPreview lets decoded text cross the shared ResultFactory redaction
// boundary. Protocol-owned non-text and structured payloads are deliberately
// represented only by fixed markers; raw frames never leave the adapter.
func mcpResultPreview(result protocol.CallToolResult) string {
	parts := make([]string, 0, len(result.Content)+1)
	nonText := false
	for _, block := range result.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
			continue
		}
		nonText = true
	}
	if nonText {
		parts = append(parts, "[non-text MCP content omitted]")
	}
	if result.StructuredContent != nil {
		parts = append(parts, "[structured MCP content omitted]")
	}
	return strings.Join(parts, "\n")
}

func cloneMCPArguments(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneMCPValue(value)
	}
	return cloned
}

func cloneMCPValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMCPArguments(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneMCPValue(item)
		}
		return cloned
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func cloneAdapterSchema(schema tool.Schema) tool.Schema {
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

func validRemoteAnnotations(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value != nil
}
