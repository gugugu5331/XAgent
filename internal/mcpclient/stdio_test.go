package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStdioTransportSendsAndReceivesJSONRPC(t *testing.T) {
	server := buildFakeStdioServer(t)
	transport := NewStdioTransport(StdioConfig{Command: server, Args: []string{"normal"}})
	ctx := context.Background()
	if err := transport.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer closeTransport(t, transport)

	if err := transport.Send(ctx, RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "echo", Params: json.RawMessage(`{"hello":"world"}`)}); err != nil {
		t.Fatal(err)
	}

	select {
	case response := <-transport.Recv():
		if response.JSONRPC != "2.0" || response.ID.key() != NumberID(1).key() || string(response.Result) != `{"ok":true}` {
			t.Fatalf("unexpected response: %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestStdioTransportRecordsMalformedStdoutAndStderr(t *testing.T) {
	server := buildFakeStdioServer(t)
	transport := NewStdioTransport(StdioConfig{Command: server, Args: []string{"malformed"}})
	ctx := context.Background()
	if err := transport.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer closeTransport(t, transport)

	if err := transport.Send(ctx, RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "echo"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.Recv():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for valid response after malformed line")
	}
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	diagnostics := strings.Join(transport.Diagnostics(), "\n")
	if !strings.Contains(diagnostics, "malformed stdout JSON-RPC line") {
		t.Fatalf("missing malformed diagnostic: %q", diagnostics)
	}
	if !strings.Contains(diagnostics, "fake stderr diagnostic") {
		t.Fatalf("missing stderr diagnostic: %q", diagnostics)
	}
}

func TestStdioTransportCloseStopsProcess(t *testing.T) {
	server := buildFakeStdioServer(t)
	transport := NewStdioTransport(StdioConfig{Command: server, Args: []string{"sleep"}})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := transport.Close(ctx); err != nil && !strings.Contains(err.Error(), "stdio process exited") {
		t.Fatalf("close failed: %v", err)
	}
	if err := transport.Close(context.Background()); err != nil {
		t.Fatalf("second close should be idempotent: %v", err)
	}
}

func buildFakeStdioServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fake_stdio.go")
	binary := filepath.Join(dir, "fake_stdio")
	if err := os.WriteFile(source, []byte(fakeStdioServerSource), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", binary, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake stdio server: %v\n%s", err, output)
	}
	return binary
}

func closeTransport(t *testing.T, transport *StdioTransport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

const fakeStdioServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type request struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      any             ` + "`json:\"id\"`" + `
	Method  string          ` + "`json:\"method\"`" + `
	Params  json.RawMessage ` + "`json:\"params\"`" + `
}

func main() {
	mode := "normal"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "sleep" {
		time.Sleep(10 * time.Second)
		return
	}
	if mode == "mcp" {
		runMCP()
		return
	}
	fmt.Fprintln(os.Stderr, "fake stderr diagnostic")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if mode == "malformed" {
			fmt.Println("not-json")
		}
		response := map[string]any{"jsonrpc":"2.0", "id":req.ID, "result":map[string]bool{"ok":true}}
		data, _ := json.Marshal(response)
		fmt.Println(string(data))
	}
}

func runMCP() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/initialized" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion":"2025-06-18", "capabilities":map[string]any{}, "serverInfo":map[string]any{"name":"fake-stdio"}}
		case "tools/list":
			var params struct{ Cursor string ` + "`json:\"cursor\"`" + ` }
			_ = json.Unmarshal(req.Params, &params)
			if params.Cursor == "" {
				result = map[string]any{"tools":[]map[string]any{{"name":"echo", "inputSchema":map[string]any{"type":"object"}}}, "nextCursor":"page2"}
			} else {
				result = map[string]any{"tools":[]map[string]any{{"name":"fail", "inputSchema":map[string]any{"type":"object"}}}}
			}
		case "tools/call":
			var params struct{
				Name string ` + "`json:\"name\"`" + `
				Arguments map[string]any ` + "`json:\"arguments\"`" + `
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "rpc_error" {
				response := map[string]any{"jsonrpc":"2.0", "id":req.ID, "error":map[string]any{"code":-32000, "message":"fake rpc error"}}
				data, _ := json.Marshal(response)
				fmt.Println(string(data))
				continue
			}
			if params.Name == "fail" {
				result = map[string]any{"isError":true, "content":[]map[string]any{{"type":"text", "text":"failed"}}}
			} else {
				message, _ := params.Arguments["message"].(string)
				result = map[string]any{"content":[]map[string]any{{"type":"text", "text":message}}}
			}
		default:
			response := map[string]any{"jsonrpc":"2.0", "id":req.ID, "error":map[string]any{"code":-32601, "message":"method not found"}}
			data, _ := json.Marshal(response)
			fmt.Println(string(data))
			continue
		}
		response := map[string]any{"jsonrpc":"2.0", "id":req.ID, "result":result}
		data, _ := json.Marshal(response)
		fmt.Println(string(data))
	}
}
`
