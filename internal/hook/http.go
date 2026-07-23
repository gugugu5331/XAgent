package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

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

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type DefaultHTTPRunner struct {
	Limits   Limits
	Resolver Resolver
	Dialer   *net.Dialer
	Redactor *redact.RuntimeRedactor

	// transport is an internal test seam. Production callers always use the
	// hardened transport returned by newHTTPTransport.
	transport http.RoundTripper
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
	if err := validateTarget(parsed); err != nil {
		return nil, err
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

func validateTarget(target *url.URL) error {
	if target == nil || target.Hostname() == "" {
		return fmt.Errorf("invalid HTTP target")
	}
	if target.Scheme == "https" {
		return nil
	}
	if target.Scheme != "http" {
		return fmt.Errorf("unsupported HTTP scheme")
	}
	host := target.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("plaintext HTTP requires loopback")
	}
	return nil
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
	limits := normalizeLimits(r.Limits)
	body := []byte(nil)
	if request.SendEvent {
		if len(request.EventJSON) > limits.HTTPRequestBytes {
			return HTTPResult{}, fmt.Errorf("request body exceeds limit")
		}
		body = append([]byte(nil), request.EventJSON...)
	}
	resolver := r.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := r.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	transport := r.transport
	if transport == nil {
		transport = newHTTPTransport(limits, resolver, dialer)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	current := cloneURL(request.URL)
	for redirects := 0; ; redirects++ {
		if err := validateURLStructure(current, limits.HTTPURLBytes); err != nil {
			return HTTPResult{}, err
		}
		if err := validateTarget(current); err != nil {
			return HTTPResult{}, err
		}
		var reader io.Reader
		if request.SendEvent {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, request.Method, current.String(), reader)
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
		response, err := client.Do(req)
		if err != nil {
			return HTTPResult{}, fmt.Errorf("HTTP request failed")
		}
		if !responseHeadersWithinLimit(response.Header, limits.HTTPResponseBytes) {
			_ = response.Body.Close()
			return HTTPResult{}, fmt.Errorf("HTTP response headers exceed limit")
		}
		if isRedirect(response.StatusCode) {
			_ = response.Body.Close()
			if redirects >= limits.HTTPRedirects {
				return HTTPResult{}, fmt.Errorf("redirect limit exceeded")
			}
			rawLocation := response.Header.Get("Location")
			if err := validateURLText(rawLocation, limits.HTTPURLBytes); err != nil {
				return HTTPResult{}, fmt.Errorf("invalid redirect")
			}
			location, err := response.Location()
			if err != nil {
				return HTTPResult{}, fmt.Errorf("invalid redirect")
			}
			if err := validateURLStructure(location, limits.HTTPURLBytes); err != nil {
				return HTTPResult{}, fmt.Errorf("invalid redirect")
			}
			if err := validateTarget(location); err != nil {
				return HTTPResult{}, fmt.Errorf("invalid redirect")
			}
			if !sameOrigin(current, location) || (current.Scheme == "https" && location.Scheme != "https") {
				return HTTPResult{}, fmt.Errorf("redirect violates origin policy")
			}
			current = location
			continue
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
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return HTTPResult{}, fmt.Errorf("HTTP non-success status")
		}
		if r.Redactor != nil {
			responseBody = []byte(r.Redactor.Text(string(responseBody)))
		}
		return HTTPResult{Body: responseBody}, nil
	}
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func newHTTPTransport(limits Limits, resolver Resolver, dialer contextDialer) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		MaxResponseHeaderBytes: int64(limits.HTTPResponseBytes),
		DialContext:            secureDialer(resolver, dialer),
	}
}

func responseHeadersWithinLimit(headers http.Header, limit int) bool {
	// Count the encoded field lines and the terminating CRLF. The status line is
	// not part of the response Header limit.
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

func secureDialer(resolver Resolver, dialer contextDialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if host != "localhost" {
			return dialer.DialContext(ctx, network, address)
		}
		addresses, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, fmt.Errorf("localhost resolution failed")
		}
		for _, candidate := range addresses {
			if !candidate.IP.IsLoopback() {
				return nil, fmt.Errorf("localhost resolved outside loopback")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
}

func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}
func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return &url.URL{}
	}
	clone := *value
	return &clone
}
func origin(value *url.URL) string {
	port := value.Port()
	if port == "" {
		if value.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(value.Scheme) + "://" + strings.ToLower(value.Hostname()) + ":" + port
}
func sameOrigin(left, right *url.URL) bool { return origin(left) == origin(right) }
