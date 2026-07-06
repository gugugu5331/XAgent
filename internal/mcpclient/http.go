package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	headerContentType       = "Content-Type"
	headerAccept            = "Accept"
	headerSessionID         = "Mcp-Session-Id"
	headerProtocolVersion   = "MCP-Protocol-Version"
	contentTypeJSON         = "application/json"
	contentTypeEventStream  = "text/event-stream"
	acceptStreamableHTTPMCP = "application/json, text/event-stream"
)

type HTTPConfig struct {
	URL              string
	Headers          map[string]string
	ProtocolVersion  string
	MaxResponseBytes int64
	Client           *http.Client
}

type HTTPTransport struct {
	config HTTPConfig
	client *http.Client
	url    *url.URL
	recv   chan RPCResponse

	mu             sync.Mutex
	sessionID      string
	protocolErrors []string
}

func NewHTTPTransport(config HTTPConfig) (*HTTPTransport, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil {
		return nil, err
	}
	if err := validateMCPHTTPURL(parsed); err != nil {
		return nil, err
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	client.CheckRedirect = protectRedirectHeaders(client.CheckRedirect)
	return &HTTPTransport{config: config, client: client, url: parsed, recv: make(chan RPCResponse, 16)}, nil
}

func (t *HTTPTransport) Send(ctx context.Context, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set(headerContentType, contentTypeJSON)
	request.Header.Set(headerAccept, acceptStreamableHTTPMCP)
	if t.config.ProtocolVersion != "" {
		request.Header.Set(headerProtocolVersion, t.config.ProtocolVersion)
	}
	for key, value := range t.config.Headers {
		if isBlockedHTTPHeader(key) {
			return fmt.Errorf("http header %s cannot be configured", key)
		}
		request.Header.Set(key, value)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		request.Header.Set(headerSessionID, t.sessionID)
	}
	t.mu.Unlock()

	response, err := t.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if sessionID := response.Header.Get(headerSessionID); sessionID != "" {
		t.mu.Lock()
		t.sessionID = sessionID
		t.mu.Unlock()
	}
	if response.StatusCode == http.StatusAccepted {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("mcp http status %d", response.StatusCode)
	}
	if response.ContentLength > maxResponseBytes(t.config.MaxResponseBytes) {
		return fmt.Errorf("mcp http response exceeds max response size")
	}
	contentType := response.Header.Get(headerContentType)
	if strings.Contains(contentType, contentTypeEventStream) {
		return t.readSSE(response.Body)
	}
	return t.readJSON(response.Body)
}

func (t *HTTPTransport) Recv() <-chan RPCResponse {
	return t.recv
}

func (t *HTTPTransport) Diagnostics() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.protocolErrors...)
}

func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *HTTPTransport) readJSON(body io.Reader) error {
	data, err := readLimited(body, maxResponseBytes(t.config.MaxResponseBytes))
	if err != nil {
		return err
	}
	var response RPCResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}
	if response.JSONRPC != "2.0" {
		t.recordProtocolError("invalid http JSON-RPC version")
		return nil
	}
	t.recv <- response
	return nil
}

func (t *HTTPTransport) readSSE(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), int(maxResponseBytes(t.config.MaxResponseBytes)))
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				if err := t.handleSSEData(data.String()); err != nil {
					return err
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if err := t.handleSSEData(data.String()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (t *HTTPTransport) handleSSEData(data string) error {
	var response RPCResponse
	if err := json.Unmarshal([]byte(data), &response); err != nil {
		t.recordProtocolError("malformed sse JSON-RPC event")
		return nil
	}
	if response.JSONRPC != "2.0" {
		t.recordProtocolError("invalid sse JSON-RPC version")
		return nil
	}
	t.recv <- response
	return nil
}

func (t *HTTPTransport) recordProtocolError(message string) {
	t.mu.Lock()
	t.protocolErrors = append(t.protocolErrors, message)
	t.mu.Unlock()
}

func validateMCPHTTPURL(parsed *url.URL) error {
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("mcp http transport requires https except localhost")
	default:
		return errors.New("mcp http transport requires http or https")
	}
}

func isBlockedHTTPHeader(key string) bool {
	switch strings.ToLower(key) {
	case "host", "content-length":
		return true
	default:
		return false
	}
}

func protectRedirectHeaders(existing func(req *http.Request, via []*http.Request) error) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && !sameHost(req.URL, via[len(via)-1].URL) {
			req.Header.Del("Authorization")
		}
		if existing != nil {
			return existing(req, via)
		}
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

func sameHost(a *url.URL, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
