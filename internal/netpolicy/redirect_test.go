package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestRedirectNeverForwardsCredentialsAcrossBoundary(t *testing.T) {
	resolver := &fixedResolver{addresses: map[string][]net.IPAddr{
		"api.example.test":   {{IP: net.ParseIP("203.0.113.10")}},
		"other.example.test": {{IP: net.ParseIP("203.0.113.20")}},
	}}
	policy := newPolicyWithResolver(resolver)
	endpoint, err := policy.ValidateInitial(context.Background(), "https://api.example.test/v1?opaque=initial-marker", PurposeProvider)
	if err != nil {
		t.Fatal("redirect endpoint fixture was rejected")
	}
	clientValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{SensitiveHeaders: []string{"X-Scoped-Auth"}})
	if err != nil {
		t.Fatal("controlled redirect client construction failed")
	}
	bridge := clientValue.SDKHTTPClient()

	sameOrigin := redirectRequest(t, "https://api.example.test/v2?opaque=same-origin-marker")
	if err := bridge.CheckRedirect(sameOrigin, nil); err != nil {
		t.Fatal("safe same-origin redirect was rejected")
	}
	assertSensitiveHeadersRemoved(t, sameOrigin)

	crossOrigin := redirectRequest(t, "https://other.example.test/v2?opaque=cross-origin-marker")
	if err := bridge.CheckRedirect(crossOrigin, nil); err == nil {
		t.Fatal("cross-origin redirect was accepted")
	} else if strings.Contains(err.Error(), "other.example.test") || strings.Contains(err.Error(), "cross-origin-marker") {
		t.Fatal("cross-origin redirect error exposed endpoint data")
	}
	assertSensitiveHeadersRemoved(t, crossOrigin)

	downgrade := redirectRequest(t, "http://api.example.test/v2?opaque=downgrade-marker")
	if err := bridge.CheckRedirect(downgrade, nil); err == nil {
		t.Fatal("HTTPS downgrade redirect was accepted")
	}
	assertSensitiveHeadersRemoved(t, downgrade)

	resolver.addresses["api.example.test"] = []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}
	rebound := redirectRequest(t, "https://api.example.test/v3?opaque=rebound-marker")
	err = bridge.CheckRedirect(rebound, nil)
	var policyErr *Error
	if !errors.As(err, &policyErr) || policyErr.Code != CodeAddressClassChanged {
		t.Fatal("redirect DNS address-class change was accepted")
	}
	assertSensitiveHeadersRemoved(t, rebound)

	manual := redirectRequest(t, "https://other.example.test/v4?opaque=manual-marker")
	if _, err := bridge.Transport.RoundTrip(manual); err == nil {
		t.Fatal("SDK-constructed cross-origin request bypassed the guarded transport")
	}
}

func redirectRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal("redirect request fixture was invalid")
	}
	request.Header.Set("Authorization", "present")
	request.Header.Set("Proxy-Authorization", "present")
	request.Header.Set("Cookie", "present")
	request.Header.Set("X-Api-Key", "present")
	request.Header.Set("X-Scoped-Auth", "present")
	return request
}

func assertSensitiveHeadersRemoved(t *testing.T, request *http.Request) {
	t.Helper()
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Scoped-Auth"} {
		if request.Header.Get(name) != "" {
			t.Fatal("redirect retained a sensitive header")
		}
	}
}
