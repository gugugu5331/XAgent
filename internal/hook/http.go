package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

type httpAction struct {
	url       *url.URL
	method    string
	headers   http.Header
	sendEvent bool
	decision  bool
}

type HTTPRequest struct {
	URL       *url.URL
	Method    string
	Headers   http.Header
	SendEvent bool
	EventJSON []byte
}

type HTTPResult struct{ Body []byte }

type HTTPRunner interface {
	Run(context.Context, HTTPRequest) (HTTPResult, error)
}

type DefaultHTTPRunner struct {
	Limits        Limits
	Policy        netpolicy.HTTPPolicy
	ClientFactory netpolicy.ClientFactory
	ClientOptions netpolicy.ClientOptions
	Redactor      *redact.RuntimeRedactor

	mu      sync.Mutex
	clients map[string]netpolicy.Client
	closed  bool
}

func compileHTTPAction(config ActionConfig, limits Limits, lookup func(string) (string, bool), runtimeRedactor *redact.RuntimeRedactor) (*httpAction, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, fmt.Errorf("url is required")
	}
	if err := validateURLText(config.URL, limits.HTTPURLBytes); err != nil || strings.Contains(config.URL, "{{") || strings.Contains(config.URL, "${") {
		return nil, fmt.Errorf("invalid static URL")
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || validateURLStructure(parsed, limits.HTTPURLBytes) != nil {
		return nil, fmt.Errorf("invalid static URL")
	}
	method := config.Method
	if method == "" && !config.present["method"] {
		method = http.MethodPost
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("unsupported HTTP method")
	}
	sendEvent := true
	if config.SendEvent != nil {
		sendEvent = *config.SendEvent
	}
	if len(config.Headers) > limits.HTTPHeaderCount {
		return nil, fmt.Errorf("header count exceeds limit")
	}
	headers := make(http.Header, len(config.Headers))
	seen := map[string]bool{}
	for name, raw := range config.Headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if canonical == "" || len(name) > limits.HTTPHeaderNameBytes || !validHeaderName(name) {
			return nil, fmt.Errorf("invalid header name")
		}
		lower := strings.ToLower(canonical)
		if seen[lower] {
			return nil, fmt.Errorf("duplicate header")
		}
		seen[lower] = true
		if lower == "host" || lower == "content-length" || (sendEvent && lower == "content-type") {
			return nil, fmt.Errorf("forbidden header")
		}
		expanded, expansions, err := expandEnvironment(raw, lookup)
		if err != nil {
			return nil, err
		}
		if len(expanded) > limits.HTTPHeaderValueBytes || !validHeaderValue(expanded) {
			return nil, fmt.Errorf("invalid header value")
		}
		registerExpansion(runtimeRedactor, name, expanded, expansions)
		headers.Set(canonical, expanded)
	}
	return &httpAction{url: parsed, method: method, headers: headers, sendEvent: sendEvent, decision: config.Decision}, nil
}

func validateURLText(raw string, maxBytes int) error {
	if raw == "" || len(raw) > maxBytes || containsURLControl(raw) {
		return fmt.Errorf("invalid HTTP URL")
	}
	return nil
}

func validateURLStructure(target *url.URL, maxBytes int) error {
	if target == nil || validateURLText(target.String(), maxBytes) != nil || !target.IsAbs() || target.Host == "" || target.Opaque != "" || target.Fragment != "" || target.User != nil {
		return fmt.Errorf("invalid HTTP URL")
	}
	// url.URL stores several escaped components in decoded form. Inspect those
	// fields too so percent-encoded controls cannot bypass the raw URL check.
	for _, component := range []string{target.Scheme, target.Opaque, target.Host, target.Path, target.RawPath, target.RawQuery, target.Fragment, target.RawFragment} {
		if containsURLControl(component) {
			return fmt.Errorf("invalid HTTP URL")
		}
	}
	return nil
}

func containsURLControl(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return true
		}
	}
	return false
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	const separators = "()<>@,;:\\\"/[]?={} \t"
	for _, r := range value {
		if r < 0x21 || r > 0x7e || strings.ContainsRune(separators, r) {
			return false
		}
	}
	return true
}
func validHeaderValue(value string) bool {
	for _, r := range value {
		if r == '\r' || r == '\n' || r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return false
		}
	}
	return true
}

func (r *DefaultHTTPRunner) Run(ctx context.Context, request HTTPRequest) (HTTPResult, error) {
	if r == nil || ctx == nil || request.URL == nil {
		return HTTPResult{}, fmt.Errorf("HTTP runner is unavailable")
	}
	limits := normalizeLimits(r.Limits)
	body := []byte(nil)
	if request.SendEvent {
		if len(request.EventJSON) > limits.HTTPRequestBytes {
			return HTTPResult{}, fmt.Errorf("request body exceeds limit")
		}
		body = append([]byte(nil), request.EventJSON...)
	}
	rawURL := request.URL.String()
	if err := validateURLText(rawURL, limits.HTTPURLBytes); err != nil {
		return HTTPResult{}, fmt.Errorf("HTTP target rejected")
	}
	var reader io.Reader
	if request.SendEvent {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, rawURL, reader)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("create HTTP request")
	}
	req.Header = request.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if request.SendEvent {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	if !requestHeadersWithinLimits(req.Header, limits) {
		return HTTPResult{}, fmt.Errorf("request headers exceed limit")
	}
	policy, factory, err := r.networkComponents()
	if err != nil {
		return HTTPResult{}, err
	}
	endpoint, err := policy.ValidateInitial(ctx, rawURL, netpolicy.PurposeHook)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("HTTP target rejected")
	}
	client, err := r.clientFor(endpoint, factory, req.Header)
	if err != nil {
		return HTTPResult{}, err
	}
	response, err := client.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return HTTPResult{}, fmt.Errorf("HTTP request failed")
	}
	if response == nil || response.Body == nil {
		return HTTPResult{}, fmt.Errorf("HTTP response is unavailable")
	}
	if !responseHeadersWithinLimit(response.Header, limits.HTTPResponseBytes) {
		_ = response.Body.Close()
		return HTTPResult{}, fmt.Errorf("HTTP response headers exceed limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		summary := diagnostics.SafeHTTPErrorSummary(response, diagnostics.HTTPErrorSummaryOptions{
			MaxBodyBytes: limits.HTTPErrorPreviewBytes,
			Redactor:     r.Redactor,
		})
		_ = response.Body.Close()
		return HTTPResult{}, safeHTTPResponseError(summary)
	}
	if response.ContentLength > int64(limits.HTTPResponseBytes) {
		_ = response.Body.Close()
		return HTTPResult{}, fmt.Errorf("HTTP response exceeds limit")
	}
	limited := io.LimitReader(response.Body, int64(limits.HTTPResponseBytes)+1)
	responseBody, readErr := io.ReadAll(limited)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return HTTPResult{}, fmt.Errorf("HTTP response read failed")
	}
	if len(responseBody) > limits.HTTPResponseBytes {
		return HTTPResult{}, fmt.Errorf("HTTP response exceeds limit")
	}
	if r.Redactor != nil {
		responseBody = []byte(r.Redactor.Text(string(responseBody)))
	}
	return HTTPResult{Body: responseBody}, nil
}

func (r *DefaultHTTPRunner) networkComponents() (netpolicy.HTTPPolicy, netpolicy.ClientFactory, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, fmt.Errorf("HTTP runner is closed")
	}
	if r.Policy == nil && r.ClientFactory == nil {
		policy := netpolicy.NewPolicy()
		r.Policy = policy
		r.ClientFactory = netpolicy.NewClientFactory(policy)
	}
	if r.Policy == nil || r.ClientFactory == nil {
		return nil, nil, fmt.Errorf("HTTP network policy is unavailable")
	}
	return r.Policy, r.ClientFactory, nil
}

func (r *DefaultHTTPRunner) clientFor(endpoint netpolicy.Endpoint, factory netpolicy.ClientFactory, headers http.Header) (netpolicy.Client, error) {
	headerNames := append([]string(nil), r.ClientOptions.SensitiveHeaders...)
	for name := range headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	key := string(endpoint.Origin) + "\x00" + strings.Join(headerNames, "\x00")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("HTTP runner is closed")
	}
	if client := r.clients[key]; client != nil {
		return client, nil
	}
	options := r.ClientOptions
	options.SensitiveHeaders = headerNames
	client, err := factory.New(endpoint, options)
	if err != nil || client == nil {
		return nil, fmt.Errorf("HTTP client creation failed")
	}
	if r.clients == nil {
		r.clients = make(map[string]netpolicy.Client)
	}
	r.clients[key] = client
	return client, nil
}

// CloseIdleConnections is the single close hook owned by the Hook Engine. It
// permanently closes this runner to prevent clients from being created after
// the Engine has started shutdown.
func (r *DefaultHTTPRunner) CloseIdleConnections() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	clients := make([]netpolicy.Client, 0, len(r.clients))
	for _, client := range r.clients {
		clients = append(clients, client)
	}
	r.mu.Unlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

func requestHeadersWithinLimits(headers http.Header, limits Limits) bool {
	count := 0
	for name, values := range headers {
		if len(name) > limits.HTTPHeaderNameBytes || !validHeaderName(name) {
			return false
		}
		if len(values) == 0 {
			count++
		}
		for _, value := range values {
			count++
			if len(value) > limits.HTTPHeaderValueBytes || !validHeaderValue(value) {
				return false
			}
		}
		if count > limits.HTTPHeaderCount {
			return false
		}
	}
	return true
}

func responseHeadersWithinLimit(headers http.Header, limit int) bool {
	remaining := limit - 2
	if remaining < 0 {
		return false
	}
	for name, values := range headers {
		for _, value := range values {
			lineBytes := len(name) + 2 + len(value) + 2
			if lineBytes > remaining {
				return false
			}
			remaining -= lineBytes
		}
	}
	return true
}

func safeHTTPResponseError(summary diagnostics.HTTPErrorSummary) error {
	if summary.BodyPreview.Text() == "" {
		return fmt.Errorf("HTTP non-success response: status=%d", summary.StatusCode)
	}
	return fmt.Errorf(
		"HTTP non-success response: status=%d media_type=%q preview=%q truncated=%t observed_bytes=%d",
		summary.StatusCode,
		summary.MediaType,
		summary.BodyPreview.Text(),
		summary.Truncated,
		summary.ObservedBytes,
	)
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return &url.URL{}
	}
	clone := *value
	return &clone
}
