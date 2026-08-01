package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestInitialEndpointPolicy(t *testing.T) {
	policy := newPolicyWithResolver(&fixedResolver{addresses: map[string][]net.IPAddr{
		"api.example.test": {{IP: net.ParseIP("203.0.113.10")}},
	}})
	valid := []struct {
		raw          string
		purpose      Purpose
		origin       Origin
		addressClass AddressClass
	}{
		{
			raw: "https://api.example.test/v1?request=opaque", purpose: PurposeProvider,
			origin: "https://api.example.test:443", addressClass: AddressPublic,
		},
		{
			raw: "http://127.0.0.1:8080/rpc", purpose: PurposeMCP,
			origin: "http://127.0.0.1:8080", addressClass: AddressLoopback,
		},
		{
			raw: "http://[::1]/hook", purpose: PurposeHook,
			origin: "http://[::1]:80", addressClass: AddressLoopback,
		},
	}
	for _, test := range valid {
		endpoint, err := policy.ValidateInitial(context.Background(), test.raw, test.purpose)
		if err != nil {
			t.Fatal("valid initial endpoint was rejected")
		}
		if !endpoint.valid() || endpoint.Origin != test.origin || endpoint.AddressClass != test.addressClass || endpoint.Purpose != test.purpose {
			t.Fatal("validated endpoint did not preserve its safe identity")
		}
		if endpoint.URL == nil || endpoint.URL.User != nil || strings.Contains(string(endpoint.Origin), "?") {
			t.Fatal("validated origin retained sensitive URL fields")
		}
	}

	const userCanary = "user-secret-canary"
	const queryCanary = "query-secret-canary"
	invalid := []struct {
		raw     string
		purpose Purpose
		code    ErrorCode
	}{
		{"https://" + userCanary + ":password@example.test/path?token=" + queryCanary, PurposeProvider, CodeURLUserinfoForbidden},
		{"ftp://example.test/archive?token=" + queryCanary, PurposeProvider, CodeSchemeForbidden},
		{"http://example.test/path?token=" + queryCanary, PurposeProvider, CodePlainHTTPForbidden},
		{"http://192.168.1.2/path?token=" + queryCanary, PurposeMCP, CodePlainHTTPForbidden},
		{"http://localhost/path?token=" + queryCanary, PurposeHook, CodePlainHTTPForbidden},
		{"relative/path?token=" + queryCanary, PurposeProvider, CodeInvalidEndpoint},
		{"https://example.test/#fragment-" + queryCanary, PurposeProvider, CodeInvalidEndpoint},
		{"https://example.test/path?token=" + queryCanary, 0, CodeInvalidPurpose},
	}
	for _, test := range invalid {
		endpoint, err := policy.ValidateInitial(context.Background(), test.raw, test.purpose)
		if err == nil || endpoint.valid() {
			t.Fatal("unsafe initial endpoint was accepted")
		}
		var policyErr *Error
		if !errors.As(err, &policyErr) || policyErr.Code != test.code {
			t.Fatal("unsafe endpoint returned the wrong safe error code")
		}
		message := err.Error()
		if strings.Contains(message, userCanary) || strings.Contains(message, queryCanary) || strings.Contains(message, test.raw) {
			t.Fatal("initial endpoint error exposed sensitive URL data")
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := policy.ValidateInitial(canceled, "https://example.test/", PurposeProvider); err == nil {
		t.Fatal("canceled initial endpoint validation was accepted")
	}
}

type fixedResolver struct {
	addresses map[string][]net.IPAddr
	err       error
}

func (r *fixedResolver) LookupIPAddr(_ context.Context, stringHost string) ([]net.IPAddr, error) {
	if r == nil || r.err != nil {
		return nil, r.err
	}
	addresses := r.addresses[stringHost]
	result := make([]net.IPAddr, len(addresses))
	for index := range addresses {
		result[index] = net.IPAddr{IP: append(net.IP(nil), addresses[index].IP...), Zone: addresses[index].Zone}
	}
	return result, nil
}

func TestResolverPreventsAddressClassChange(t *testing.T) {
	resolver := &fixedResolver{addresses: map[string][]net.IPAddr{
		"service.example.test": {
			{IP: net.ParseIP("203.0.113.20")},
			{IP: net.ParseIP("203.0.113.10")},
			{IP: net.ParseIP("203.0.113.20")},
		},
		"mixed.example.test": {
			{IP: net.ParseIP("203.0.113.30")},
			{IP: net.ParseIP("10.0.0.2")},
		},
	}}
	policy := newPolicyWithResolver(resolver)
	endpoint, err := policy.ValidateInitial(context.Background(), "https://service.example.test/v1?opaque=resolver-canary", PurposeProvider)
	if err != nil {
		t.Fatal("public endpoint resolution failed")
	}
	if endpoint.AddressClass != AddressPublic || len(endpoint.addresses) != 2 || endpoint.addresses[0].String() != "203.0.113.10" {
		t.Fatal("resolved addresses were not classified, deduplicated, and sorted")
	}

	resolver.addresses["service.example.test"] = []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}
	err = policy.revalidateEndpoint(context.Background(), endpoint)
	var policyErr *Error
	if !errors.As(err, &policyErr) || policyErr.Code != CodeAddressClassChanged {
		t.Fatal("public-to-private resolution change was accepted")
	}
	if strings.Contains(err.Error(), "service.example.test") || strings.Contains(err.Error(), "resolver-canary") {
		t.Fatal("resolution error exposed endpoint data")
	}

	if _, err := policy.ValidateInitial(context.Background(), "https://mixed.example.test/?opaque=resolver-canary", PurposeMCP); err == nil {
		t.Fatal("mixed DNS address classes were accepted")
	} else if !errors.As(err, &policyErr) || policyErr.Code != CodeAddressClassMixed {
		t.Fatal("mixed DNS classes returned the wrong safe error")
	}

	classes := map[string]AddressClass{
		"127.0.0.1":   AddressLoopback,
		"10.0.0.1":    AddressPrivate,
		"169.254.1.1": AddressLinkLocal,
		"8.8.8.8":     AddressPublic,
	}
	for raw, want := range classes {
		address := net.ParseIP(raw)
		parsed, ok := netip.AddrFromSlice(address)
		if !ok {
			t.Fatal("classification fixture was invalid")
		}
		got, err := classifyAddress(parsed.Unmap())
		if err != nil || got != want {
			t.Fatal("address classification was incorrect")
		}
	}
}
