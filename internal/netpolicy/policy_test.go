package netpolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestClientFactoryHasNoExternalTransportInjection(t *testing.T) {
	optionsType := reflect.TypeOf(ClientOptions{})
	wantOptions := map[string]reflect.Type{
		"Timeout":          reflect.TypeOf(time.Duration(0)),
		"SensitiveHeaders": reflect.TypeOf([]string(nil)),
		"TrustedRoots":     reflect.TypeOf((*x509.CertPool)(nil)),
	}
	if optionsType.NumField() != len(wantOptions) {
		t.Fatal("client options expose an unexpected injection field")
	}
	for index := 0; index < optionsType.NumField(); index++ {
		field := optionsType.Field(index)
		if wantOptions[field.Name] != field.Type {
			t.Fatal("client options expose an unexpected injection type")
		}
	}

	clientType := reflect.TypeOf((*Client)(nil)).Elem()
	wantMethods := map[string]bool{"Do": true, "SDKHTTPClient": true, "CloseIdleConnections": true}
	if clientType.NumMethod() != len(wantMethods) {
		t.Fatal("controlled client exposes unexpected methods")
	}
	for index := 0; index < clientType.NumMethod(); index++ {
		if !wantMethods[clientType.Method(index).Name] {
			t.Fatal("controlled client exposes a network customization method")
		}
	}

	factoryType := reflect.TypeOf((*ClientFactory)(nil)).Elem()
	method, ok := factoryType.MethodByName("New")
	if !ok || factoryType.NumMethod() != 1 || method.Type.NumIn() != 2 || method.Type.In(0) != reflect.TypeOf(Endpoint{}) || method.Type.In(1) != optionsType {
		t.Fatal("client factory accepts something other than endpoint and fixed options")
	}
}

func TestTrustedRootsAreCloned(t *testing.T) {
	roots := x509.NewCertPool()
	roots.AddCert(generatedCertificate(t, 1))
	policy, endpoint := loopbackEndpoint(t)
	clientValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{TrustedRoots: roots})
	if err != nil {
		t.Fatal("client with trusted roots was rejected")
	}
	client := clientValue.(*controlledClient)
	cloned := client.transport.base.TLSClientConfig.RootCAs
	if cloned == nil || cloned == roots || len(cloned.Subjects()) != 1 {
		t.Fatal("trusted roots were not cloned into the private transport")
	}

	roots.AddCert(generatedCertificate(t, 2))
	if len(roots.Subjects()) != 2 || len(cloned.Subjects()) != 1 {
		t.Fatal("mutating caller roots changed an existing client")
	}
	systemClient, err := NewClientFactory(policy).New(endpoint, ClientOptions{})
	if err != nil || systemClient.(*controlledClient).transport.base.TLSClientConfig.RootCAs != nil {
		t.Fatal("nil trusted roots did not retain system root behavior")
	}
}

func TestClientFactoryScopesCredentials(t *testing.T) {
	policy, endpoint := loopbackEndpoint(t)
	clientValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{SensitiveHeaders: []string{"X-Scoped-Auth"}})
	if err != nil {
		t.Fatal("client with an additional sensitive header was rejected")
	}
	client := clientValue.(*controlledClient)

	sameOrigin, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/resource", nil)
	sameOrigin.Header.Set("Authorization", "present")
	sameOrigin.Header.Set("X-Scoped-Auth", "present")
	prepared, same, err := client.transport.prepareRequest(sameOrigin)
	if err != nil || !same || prepared.Header.Get("Authorization") == "" || prepared.Header.Get("X-Scoped-Auth") == "" {
		t.Fatal("same-origin credentials were not retained")
	}

	crossOrigin, _ := http.NewRequest(http.MethodGet, "http://127.0.0.2/resource", nil)
	crossOrigin.Header = sameOrigin.Header.Clone()
	prepared, same, err = client.transport.prepareRequest(crossOrigin)
	if err != nil || same || prepared.Header.Get("Authorization") != "" || prepared.Header.Get("X-Scoped-Auth") != "" {
		t.Fatal("cross-origin credentials were not stripped before rejection")
	}
	if crossOrigin.Header.Get("Authorization") == "" || crossOrigin.Header.Get("X-Scoped-Auth") == "" {
		t.Fatal("credential scoping mutated the caller request")
	}

	if _, err := NewClientFactory(policy).New(endpoint, ClientOptions{SensitiveHeaders: []string{"bad header"}}); err == nil {
		t.Fatal("an invalid sensitive header name was accepted")
	}
	mutated := endpoint
	mutated.AddressClass = AddressPublic
	if _, err := NewClientFactory(policy).New(mutated, ClientOptions{}); err == nil {
		t.Fatal("a mutated endpoint identity was accepted")
	}
}

func TestSDKBridgeRetainsGuardedTransport(t *testing.T) {
	policy, endpoint := loopbackEndpoint(t)
	firstValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{})
	if err != nil {
		t.Fatal("first controlled client construction failed")
	}
	secondValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{})
	if err != nil {
		t.Fatal("second controlled client construction failed")
	}
	first := firstValue.(*controlledClient)
	second := secondValue.(*controlledClient)
	bridge := firstValue.SDKHTTPClient()
	if bridge == nil || bridge != first.httpClient || bridge.Transport != first.transport || bridge.CheckRedirect == nil || bridge.Jar != nil {
		t.Fatal("SDK bridge did not retain the controlled client policy")
	}
	if first.httpClient == second.httpClient || first.transport == second.transport || first.transport.base == second.transport.base {
		t.Fatal("separate owners shared a client or transport")
	}
}

func TestClientClosesIdleConnections(t *testing.T) {
	policy, endpoint := loopbackEndpoint(t)
	clientValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{})
	if err != nil {
		t.Fatal("controlled client construction failed")
	}
	client := clientValue.(*controlledClient)
	closed := 0
	client.closeIdle = func() { closed++ }
	clientValue.CloseIdleConnections()
	clientValue.CloseIdleConnections()
	if closed != 2 {
		t.Fatal("idle connection close was not forwarded")
	}
}

func loopbackEndpoint(t *testing.T) (*Policy, Endpoint) {
	t.Helper()
	policy := NewPolicy()
	endpoint, err := policy.ValidateInitial(context.Background(), "http://127.0.0.1/resource", PurposeProvider)
	if err != nil {
		t.Fatal("loopback endpoint fixture was rejected")
	}
	return policy, endpoint
}

func generatedCertificate(t *testing.T, serial int64) *x509.Certificate {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("TLS fixture key could not be generated")
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "netpolicy-test-root"},
		NotBefore:             time.Unix(1, 0),
		NotAfter:              time.Unix(2, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal("TLS fixture certificate could not be generated")
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal("TLS fixture certificate could not be parsed")
	}
	return certificate
}
