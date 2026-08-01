package netpolicy

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"
)

const maxRawURLBytes = 16 * 1024

type HTTPPolicy interface {
	ValidateInitial(ctx context.Context, rawURL string, purpose Purpose) (Endpoint, error)
	ValidateRequest(ctx context.Context, endpoint Endpoint, target *url.URL) error
	ValidateRedirect(ctx context.Context, endpoint Endpoint, next *url.URL) error
}

type Policy struct {
	resolver addressResolver
}

func NewPolicy() *Policy {
	return &Policy{resolver: net.DefaultResolver}
}

func (p *Policy) ValidateInitial(ctx context.Context, rawURL string, purpose Purpose) (Endpoint, error) {
	if p == nil || ctx == nil {
		return Endpoint{}, policyError(CodeInvalidRequest)
	}
	select {
	case <-ctx.Done():
		return Endpoint{}, policyError(CodeInvalidRequest)
	default:
	}
	if !purpose.valid() {
		return Endpoint{}, policyError(CodeInvalidPurpose)
	}
	if rawURL == "" || len(rawURL) > maxRawURLBytes || !utf8.ValidString(rawURL) {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Fragment != "" {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	if parsed.User != nil {
		return Endpoint{}, policyError(CodeURLUserinfoForbidden)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return Endpoint{}, policyError(CodeSchemeForbidden)
	}
	hostname := strings.ToLower(parsed.Hostname())
	if !validHostname(hostname) {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !validPort(port) {
		return Endpoint{}, policyError(CodeInvalidEndpoint)
	}
	addressClass := AddressUnresolved
	var addresses []netip.Addr
	if address := net.ParseIP(hostname); address != nil {
		resolved, ok := netip.AddrFromSlice(address)
		if !ok {
			return Endpoint{}, policyError(CodeAddressForbidden)
		}
		resolved = resolved.Unmap()
		addressClass, err = classifyAddress(resolved)
		if err != nil {
			return Endpoint{}, err
		}
		addresses = []netip.Addr{resolved}
	}
	if scheme == "http" && len(addresses) == 0 {
		return Endpoint{}, policyError(CodePlainHTTPForbidden)
	}
	if len(addresses) == 0 {
		addressClass, addresses, err = p.resolve(ctx, hostname)
		if err != nil {
			return Endpoint{}, err
		}
	}
	if scheme == "http" && addressClass != AddressLoopback {
		return Endpoint{}, policyError(CodePlainHTTPForbidden)
	}

	copyURL := *parsed
	copyURL.Scheme = scheme
	copyURL.Host = canonicalAuthority(hostname, port)
	copyURL.User = nil
	return Endpoint{
		URL:          &copyURL,
		Origin:       Origin(scheme + "://" + canonicalAuthority(hostname, port)),
		AddressClass: addressClass,
		Purpose:      purpose,
		seal:         &endpointSeal{},
		hostname:     hostname,
		port:         port,
		addresses:    addresses,
	}, nil
}

func validHostname(hostname string) bool {
	if hostname == "" || strings.ContainsAny(hostname, "\x00\r\n\t /\\%") {
		return false
	}
	for _, character := range hostname {
		if character > 0x7f {
			return false
		}
	}
	return true
}

func validPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	value := 0
	for _, character := range port {
		if character < '0' || character > '9' {
			return false
		}
		value = value*10 + int(character-'0')
	}
	return value > 0 && value <= 65535
}

func canonicalAuthority(hostname, port string) string {
	return net.JoinHostPort(hostname, port)
}
