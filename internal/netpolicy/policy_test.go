package netpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInitialEndpointPolicy(t *testing.T) {
	policy := NewPolicy()
	valid := []struct {
		raw          string
		purpose      Purpose
		origin       Origin
		addressClass AddressClass
	}{
		{
			raw: "https://api.example.test/v1?request=opaque", purpose: PurposeProvider,
			origin: "https://api.example.test:443", addressClass: AddressUnresolved,
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
