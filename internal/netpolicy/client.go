package netpolicy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var defaultSensitiveHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"Cookie",
	"Cookie2",
	"Api-Key",
	"X-Api-Key",
}

type ClientOptions struct {
	Timeout          time.Duration
	SensitiveHeaders []string
	TrustedRoots     *x509.CertPool
}

type Client interface {
	Do(req *http.Request) (*http.Response, error)
	SDKHTTPClient() *http.Client
	CloseIdleConnections()
}

type ClientFactory interface {
	New(endpoint Endpoint, options ClientOptions) (Client, error)
}

type clientFactory struct {
	policy *Policy
}

type controlledClient struct {
	httpClient *http.Client
	transport  *guardedTransport
	closeIdle  func()
}

type guardedTransport struct {
	policy           *Policy
	endpoint         Endpoint
	sensitiveHeaders map[string]struct{}
	base             *http.Transport
}

func NewClientFactory(policy *Policy) ClientFactory {
	return &clientFactory{policy: policy}
}

func (f *clientFactory) New(endpoint Endpoint, options ClientOptions) (Client, error) {
	if f == nil || f.policy == nil || f.policy.resolver == nil || options.Timeout < 0 {
		return nil, policyError(CodeInvalidRequest)
	}
	endpoint, err := snapshotEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	headers, err := sensitiveHeaderSet(options.SensitiveHeaders)
	if err != nil {
		return nil, err
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = blockedDialContext
	base.DialTLSContext = blockedDialContext
	base.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    cloneCertPool(options.TrustedRoots),
	}
	transport := &guardedTransport{
		policy:           f.policy,
		endpoint:         endpoint,
		sensitiveHeaders: headers,
		base:             base,
	}
	httpClient := &http.Client{
		Transport:     transport,
		CheckRedirect: transport.checkRedirect,
		Timeout:       options.Timeout,
	}
	return &controlledClient{
		httpClient: httpClient,
		transport:  transport,
		closeIdle:  base.CloseIdleConnections,
	}, nil
}

func (c *controlledClient) Do(request *http.Request) (*http.Response, error) {
	if c == nil || c.httpClient == nil || request == nil {
		return nil, policyError(CodeInvalidRequest)
	}
	return c.httpClient.Do(request)
}

func (c *controlledClient) SDKHTTPClient() *http.Client {
	if c == nil {
		return nil
	}
	return c.httpClient
}

func (c *controlledClient) CloseIdleConnections() {
	if c != nil && c.closeIdle != nil {
		c.closeIdle()
	}
}

func (t *guardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	prepared, sameOrigin, err := t.prepareRequest(request)
	if err != nil {
		return nil, err
	}
	if !sameOrigin {
		return nil, policyError(CodeInvalidEndpoint)
	}
	if err := t.policy.ValidateRequest(prepared.Context(), t.endpoint, prepared.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(prepared)
}

func (t *guardedTransport) prepareRequest(request *http.Request) (*http.Request, bool, error) {
	if t == nil || t.policy == nil || t.base == nil || request == nil || request.URL == nil || request.Context() == nil || !t.endpoint.valid() {
		return nil, false, policyError(CodeInvalidRequest)
	}
	prepared := request.Clone(request.Context())
	prepared.Header = request.Header.Clone()
	origin, err := requestOrigin(prepared.URL)
	if err != nil {
		stripSensitiveHeaders(prepared.Header, t.sensitiveHeaders)
		return prepared, false, nil
	}
	sameOrigin := origin == t.endpoint.Origin
	if !sameOrigin {
		stripSensitiveHeaders(prepared.Header, t.sensitiveHeaders)
	}
	return prepared, sameOrigin, nil
}

func snapshotEndpoint(endpoint Endpoint) (Endpoint, error) {
	if !endpoint.valid() || endpoint.URL.User != nil || endpoint.URL.Opaque != "" || endpoint.URL.Fragment != "" {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	origin, err := requestOrigin(endpoint.URL)
	if err != nil || origin != endpoint.Origin || endpoint.URL.Host != canonicalAuthority(endpoint.hostname, endpoint.port) {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	if strings.EqualFold(endpoint.URL.Scheme, "http") && endpoint.AddressClass != AddressLoopback {
		return Endpoint{}, policyError(CodePlainHTTPForbidden)
	}
	for _, address := range endpoint.addresses {
		class, err := classifyAddress(address)
		if err != nil || class != endpoint.AddressClass {
			return Endpoint{}, policyError(CodeInvalidEndpoint)
		}
	}
	copyURL := *endpoint.URL
	endpoint.URL = &copyURL
	endpoint.addresses = append([]netip.Addr(nil), endpoint.addresses...)
	return endpoint, nil
}

func sensitiveHeaderSet(additional []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(defaultSensitiveHeaders)+len(additional))
	for _, name := range defaultSensitiveHeaders {
		result[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for _, name := range additional {
		if !validHeaderName(name) {
			return nil, policyError(CodeInvalidRequest)
		}
		canonical := http.CanonicalHeaderKey(name)
		result[canonical] = struct{}{}
	}
	return result, nil
}

func validHeaderName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func stripSensitiveHeaders(headers http.Header, sensitive map[string]struct{}) {
	for name := range sensitive {
		headers.Del(name)
	}
}

func requestOrigin(target *url.URL) (Origin, error) {
	if target == nil || target.User != nil || target.Opaque != "" || !target.IsAbs() || target.Host == "" {
		return "", policyError(CodeInvalidEndpoint)
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", policyError(CodeSchemeForbidden)
	}
	hostname := strings.ToLower(target.Hostname())
	if !validHostname(hostname) {
		return "", policyError(CodeInvalidEndpoint)
	}
	port := target.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !validPort(port) {
		return "", policyError(CodeInvalidEndpoint)
	}
	return Origin(scheme + "://" + canonicalAuthority(hostname, port)), nil
}

func cloneCertPool(pool *x509.CertPool) *x509.CertPool {
	if pool == nil {
		return nil
	}
	return pool.Clone()
}

func blockedDialContext(context.Context, string, string) (net.Conn, error) {
	return nil, policyError(CodePolicyUnavailable)
}
