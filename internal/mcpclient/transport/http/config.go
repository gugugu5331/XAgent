package http

import (
	"encoding/json"
	stdhttp "net/http"
	"sort"
	"strings"

	"xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
)

const (
	headerAccept           = "Accept"
	headerContentType      = "Content-Type"
	headerProtocolVersion  = "MCP-Protocol-Version"
	headerSessionID        = "Mcp-Session-Id"
	contentTypeJSON        = "application/json"
	contentTypeEventStream = "text/event-stream"
	acceptMCP              = "application/json, text/event-stream"
)

// Config accepts only a sealed endpoint and a factory capable of creating a
// private guarded client. It deliberately has no Client, http.Client,
// RoundTripper, redirect, dial, proxy, cookie jar, or protocol-handler field.
type Config struct {
	Endpoint          netpolicy.Endpoint
	ClientFactory     netpolicy.ClientFactory
	ClientOptions     netpolicy.ClientOptions
	Headers           map[string]string
	ProtocolVersion   string
	MaxResponseBytes  int64
	MaxProtocolErrors int64
	Lifecycle         transport.Options
}

type resolvedConfig struct {
	endpoint          netpolicy.Endpoint
	target            string
	factory           netpolicy.ClientFactory
	clientOptions     netpolicy.ClientOptions
	headers           stdhttp.Header
	protocolVersion   string
	maxResponseBytes  int64
	maxProtocolErrors int64
	lifecycle         transport.Options
}

func resolveConfig(config Config) (resolvedConfig, error) {
	if config.ClientFactory == nil || config.Endpoint.URL == nil || config.Endpoint.Origin == "" ||
		config.Endpoint.Purpose != netpolicy.PurposeMCP || config.Endpoint.AddressClass == netpolicy.AddressUnresolved ||
		!config.Endpoint.URL.IsAbs() || config.Endpoint.URL.Host == "" || config.Endpoint.URL.User != nil ||
		config.Endpoint.URL.Opaque != "" || config.Endpoint.URL.Fragment != "" || config.ClientOptions.Timeout < 0 ||
		config.Lifecycle.Diagnostics == nil || config.Lifecycle.CleanupTimeout < 0 ||
		config.Lifecycle.CleanupTimeout > transport.MaxCleanupTimeout || len(config.ProtocolVersion) > 128 ||
		(config.ProtocolVersion != "" && config.ProtocolVersion != strings.TrimSpace(config.ProtocolVersion)) ||
		!validHeaderValue(config.ProtocolVersion) {
		return resolvedConfig{}, ErrInvalidConfig
	}
	maxResponseBytes, err := resolveJSONResponseLimit(config.MaxResponseBytes)
	if err != nil {
		return resolvedConfig{}, ErrInvalidConfig
	}
	maxProtocolErrors, err := resolveSSEProtocolErrorLimit(config.MaxProtocolErrors)
	if err != nil {
		return resolvedConfig{}, ErrInvalidConfig
	}
	for _, name := range config.ClientOptions.SensitiveHeaders {
		if !validHeaderName(name) {
			return resolvedConfig{}, ErrInvalidConfig
		}
	}

	endpoint := config.Endpoint
	endpointURL := *config.Endpoint.URL
	endpoint.URL = &endpointURL
	headers := make(stdhttp.Header, len(config.Headers))
	sensitive := append([]string(nil), config.ClientOptions.SensitiveHeaders...)
	for name, value := range config.Headers {
		canonical := stdhttp.CanonicalHeaderKey(name)
		if canonical == "" || !validHeaderName(name) || !validHeaderValue(value) || transportOwnedHeader(canonical) {
			return resolvedConfig{}, ErrInvalidConfig
		}
		headers.Set(canonical, value)
		sensitive = append(sensitive, canonical)
	}
	clientOptions := config.ClientOptions
	clientOptions.SensitiveHeaders = canonicalHeaderNames(sensitive)
	return resolvedConfig{
		endpoint:          endpoint,
		target:            endpointURL.String(),
		factory:           config.ClientFactory,
		clientOptions:     clientOptions,
		headers:           headers,
		protocolVersion:   config.ProtocolVersion,
		maxResponseBytes:  maxResponseBytes,
		maxProtocolErrors: maxProtocolErrors,
		lifecycle:         config.Lifecycle,
	}, nil
}

func transportOwnedHeader(name string) bool {
	switch name {
	case headerAccept, headerContentType, headerProtocolVersion, headerSessionID, "Host", "Content-Length":
		return true
	default:
		return false
	}
}

func validHeaderName(value string) bool {
	if value == "" || value != strings.TrimSpace(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, character := range value {
		if character == '\r' || character == '\n' || character == 0 || (character < 0x20 && character != '\t') || character == 0x7f {
			return false
		}
	}
	return true
}

func canonicalHeaderNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		canonical := stdhttp.CanonicalHeaderKey(name)
		if canonical != "" {
			seen[canonical] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func cloneFrame(frame json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), frame...)
}
