package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const SupportedProtocolVersion = "2025-06-18"

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeRequest struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
	Capabilities    map[string]any `json:"capabilities"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ServerInfo      map[string]any `json:"serverInfo,omitempty"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
}

type ListToolsRequest struct {
	Cursor string `json:"cursor,omitempty"`
}

type ListToolsResult struct {
	Tools      []RemoteTool `json:"tools"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

type RemoteTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

type CallToolRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content           []ContentBlock `json:"content,omitempty"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Raw  json.RawMessage `json:"-"`
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type block ContentBlock
	var decoded block
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = ContentBlock(decoded)
	b.Raw = append([]byte(nil), data...)
	return nil
}

type ProtocolClient struct {
	connection *Connection
}

func NewProtocolClient(transport Transport) *ProtocolClient {
	connection := NewConnection(transport)
	return &ProtocolClient{connection: connection}
}

func (c *ProtocolClient) Start(ctx context.Context) {
	c.connection.Start(ctx)
}

func (c *ProtocolClient) Initialize(ctx context.Context) (InitializeResult, error) {
	request := InitializeRequest{
		ProtocolVersion: SupportedProtocolVersion,
		ClientInfo:      ClientInfo{Name: "xagent", Version: "0.1.0"},
		Capabilities:    map[string]any{},
	}
	var result InitializeResult
	if err := c.connection.Request(ctx, "initialize", request, &result); err != nil {
		return InitializeResult{}, err
	}
	if result.ProtocolVersion != SupportedProtocolVersion {
		return InitializeResult{}, fmt.Errorf("unsupported MCP protocol version %q", result.ProtocolVersion)
	}
	if err := c.connection.Notify(ctx, "notifications/initialized", nil); err != nil {
		return InitializeResult{}, err
	}
	return result, nil
}

func (c *ProtocolClient) ListTools(ctx context.Context, maxPages int, maxTools int) ([]RemoteTool, error) {
	if maxPages <= 0 {
		maxPages = 32
	}
	if maxTools <= 0 {
		maxTools = 128
	}
	seen := map[string]bool{}
	cursor := ""
	tools := []RemoteTool{}
	for page := 0; page < maxPages; page++ {
		var result ListToolsResult
		if err := c.connection.Request(ctx, "tools/list", ListToolsRequest{Cursor: cursor}, &result); err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if len(tools) > maxTools {
			return nil, fmt.Errorf("mcp tools/list exceeded max tools %d", maxTools)
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("mcp tools/list repeated cursor %q", result.NextCursor)
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("mcp tools/list exceeded max pages %d", maxPages)
}

func (c *ProtocolClient) CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error) {
	if name == "" {
		return CallToolResult{}, errors.New("tool name is empty")
	}
	var result CallToolResult
	if err := c.connection.Request(ctx, "tools/call", CallToolRequest{Name: name, Arguments: arguments}, &result); err != nil {
		return CallToolResult{}, err
	}
	return result, nil
}
