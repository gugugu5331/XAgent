package protocol

import "encoding/json"

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
