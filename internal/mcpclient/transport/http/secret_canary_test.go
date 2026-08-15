package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

const httpSecretCanary = "XAGENT_MCP_SECRET_CANARY_20260802_7F31"

func TestHTTPTransportSecretCanaryNeverCrossesSafeBoundaries(t *testing.T) {
	t.Run("request failure closes body without exposing credentials", func(t *testing.T) {
		sink := newHTTPSecretCanarySink(t)
		body := newChunkedResponseBody([]byte("upstream rejected "+httpSecretCanary), 3)
		client := &canaryFailureClient{body: body, returnError: true}
		factory := &recordingClientFactory{client: client}
		created, err := New(Config{
			Endpoint:      testMCPEndpoint(t),
			ClientFactory: factory,
			Headers: map[string]string{
				"Authorization": "Bearer " + httpSecretCanary,
				"X-MCP-Secret":  httpSecretCanary,
			},
			Lifecycle: mcptransport.Options{Diagnostics: sink},
		})
		if err != nil {
			t.Fatal("construct HTTP canary transport failed")
		}
		t.Cleanup(func() { _ = created.Close(context.Background()) })
		assertHTTPSecretHeadersScoped(t, factory.options.SensitiveHeaders)
		if err := created.Start(context.Background()); err != nil {
			t.Fatal("start HTTP canary transport failed")
		}

		err = created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != ErrRequestFailed {
			t.Fatal("HTTP canary request did not collapse to the fixed request failure")
		}
		if containsHTTPSecretMaterial(err.Error()) {
			t.Fatal("HTTP canary request failure exposed a credential")
		}
		if !client.authorizationSeen.Load() || !client.customCredentialSeen.Load() {
			t.Fatal("HTTP canary fixture did not receive both initial credentials")
		}
		if client.doCalls.Load() != 1 {
			t.Fatal("HTTP canary fixture received an unexpected request count")
		}
		if body.closes.Load() != 1 {
			t.Fatal("HTTP canary response body was not closed exactly once")
		}
		if body.bytesRead.Load() != 0 {
			t.Fatal("HTTP canary response body was read after the client failure")
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal("close HTTP canary transport failed")
		}
		if client.closeCalls.Load() != 1 {
			t.Fatal("HTTP canary client was not closed exactly once")
		}
		assertHTTPSecretCanaryDiagnosticsAbsent(t, sink)
	})

	t.Run("HTTP status failure closes unread body without exposing credentials", func(t *testing.T) {
		sink := newHTTPSecretCanarySink(t)
		body := newChunkedResponseBody([]byte("status failure "+httpSecretCanary), 3)
		client := &canaryFailureClient{body: body}
		factory := &recordingClientFactory{client: client}
		created, err := New(Config{
			Endpoint:      testMCPEndpoint(t),
			ClientFactory: factory,
			Headers: map[string]string{
				"Authorization": "Bearer " + httpSecretCanary,
				"X-MCP-Secret":  httpSecretCanary,
			},
			Lifecycle: mcptransport.Options{Diagnostics: sink},
		})
		if err != nil {
			t.Fatal("construct HTTP status canary transport failed")
		}
		t.Cleanup(func() { _ = created.Close(context.Background()) })
		assertHTTPSecretHeadersScoped(t, factory.options.SensitiveHeaders)
		if err := created.Start(context.Background()); err != nil {
			t.Fatal("start HTTP status canary transport failed")
		}

		err = created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
		if err != ErrResponseUnsupported {
			t.Fatal("HTTP status canary did not collapse to the fixed unsupported response")
		}
		if containsHTTPSecretMaterial(err.Error()) {
			t.Fatal("HTTP status failure exposed credential material")
		}
		if !client.authorizationSeen.Load() || !client.customCredentialSeen.Load() || client.doCalls.Load() != 1 {
			t.Fatal("HTTP status canary fixture did not observe the expected credential-bearing request")
		}
		if body.closes.Load() != 1 || body.bytesRead.Load() != 0 {
			t.Fatal("HTTP status canary body was not closed unread")
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal("close HTTP status canary transport failed")
		}
		if client.closeCalls.Load() != 1 {
			t.Fatal("HTTP status canary client was not closed exactly once")
		}
		assertHTTPSecretCanaryDiagnosticsAbsent(t, sink)
	})

	t.Run("malicious SSE protocol errors remain bounded", func(t *testing.T) {
		const errorLimit int64 = 2
		stream := []byte(
			"data: \"" + httpSecretCanary + "-1\n\n" +
				"data: \"" + httpSecretCanary + "-2\n\n" +
				"data: \"" + httpSecretCanary + "-3\n\n" +
				"data: {\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n\n",
		)
		body := newChunkedResponseBody(stream, 3)
		client := &scriptedJSONClient{responses: []*stdhttp.Response{sseResponse(body)}}
		sink := newHTTPSecretCanarySink(t)
		created, err := New(Config{
			Endpoint:          testMCPEndpoint(t),
			ClientFactory:     &recordingClientFactory{client: client},
			MaxResponseBytes:  512,
			MaxProtocolErrors: errorLimit,
			Lifecycle:         mcptransport.Options{Diagnostics: sink},
		})
		if err != nil {
			t.Fatal("construct SSE canary transport failed")
		}
		t.Cleanup(func() { _ = created.Close(context.Background()) })
		if err := created.Start(context.Background()); err != nil {
			t.Fatal("start SSE canary transport failed")
		}
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)); err != nil {
			t.Fatal("start malicious SSE canary response failed")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		event, receiveErr := created.Receive(ctx)
		if len(event.Frame) != 0 {
			t.Fatal("malicious SSE canary returned a frame")
		}
		var limitErr *budget.LimitError
		if !errors.As(receiveErr, &limitErr) {
			t.Fatal("malicious SSE canary did not return a bounded fatal error")
		}
		if limitErr.Scope != string(budget.MCPMaxProtocolErrors) ||
			limitErr.Dimension != budget.ProtocolErrors ||
			limitErr.Limit != errorLimit || limitErr.Observed != errorLimit+1 {
			t.Fatal("malicious SSE canary returned incorrect limit metadata")
		}
		if containsHTTPSecretMaterial(receiveErr.Error()) {
			t.Fatal("malicious SSE fatal error exposed the canary")
		}
		if body.closes.Load() != 1 {
			t.Fatal("malicious SSE canary body was not closed exactly once")
		}
		if body.bytesRead.Load() >= int64(len(stream)) {
			t.Fatal("malicious SSE canary consumed the valid suffix")
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal("close SSE canary transport failed")
		}
		if client.closeCalls.Load() != 1 {
			t.Fatal("SSE canary client was not closed exactly once")
		}
		assertHTTPSecretCanaryDiagnosticsAbsent(t, sink)
	})
}

func TestHTTPSecretCanaryTestOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate HTTP canary test executable")
	}
	command := exec.Command(executable,
		"-test.run=^(TestHTTPTransportSecretCanaryNeverCrossesSafeBoundaries|TestHTTPSecretCanaryRedirectNeverForwardsCredentials)$",
		"-test.count=1",
		"-test.v",
	)
	output, childErr := command.CombinedOutput()
	if containsHTTPSecretMaterial(string(output)) {
		t.Fatal("HTTP canary appeared in child test output")
	}
	if childErr != nil {
		t.Fatal("HTTP canary child tests failed")
	}
}

func TestHTTPSecretCanaryRedirectNeverForwardsCredentials(t *testing.T) {
	var targetHits atomic.Int64
	var targetCredentialSeen atomic.Bool
	target := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		targetHits.Add(1)
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-MCP-Secret") != "" {
			targetCredentialSeen.Store(true)
		}
		response.WriteHeader(stdhttp.StatusNoContent)
	}))
	defer target.Close()

	var sourceHits atomic.Int64
	var sourceAuthorizationSeen atomic.Bool
	var sourceCustomCredentialSeen atomic.Bool
	source := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		sourceHits.Add(1)
		sourceAuthorizationSeen.Store(request.Header.Get("Authorization") == "Bearer "+httpSecretCanary)
		sourceCustomCredentialSeen.Store(request.Header.Get("X-MCP-Secret") == httpSecretCanary)
		response.Header().Set("Location", target.URL+"/steal?token="+url.QueryEscape(httpSecretCanary))
		response.WriteHeader(stdhttp.StatusTemporaryRedirect)
	}))
	defer source.Close()

	policy := netpolicy.NewPolicy()
	endpoint, err := policy.ValidateInitial(context.Background(), source.URL, netpolicy.PurposeMCP)
	if err != nil {
		t.Fatal("validate redirect canary source failed")
	}
	sink := newHTTPSecretCanarySink(t)
	created, err := New(Config{
		Endpoint:      endpoint,
		ClientFactory: netpolicy.NewClientFactory(policy),
		Headers: map[string]string{
			"Authorization": "Bearer " + httpSecretCanary,
			"X-MCP-Secret":  httpSecretCanary,
		},
		Lifecycle: mcptransport.Options{Diagnostics: sink},
	})
	if err != nil {
		t.Fatal("construct redirect canary transport failed")
	}
	t.Cleanup(func() { _ = created.Close(context.Background()) })
	if err := created.Start(context.Background()); err != nil {
		t.Fatal("start redirect canary transport failed")
	}

	err = created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	if err != ErrRequestFailed {
		t.Fatal("cross-origin redirect did not collapse to the fixed request failure")
	}
	if containsHTTPSecretMaterial(err.Error()) {
		t.Fatal("cross-origin redirect failure exposed the canary")
	}
	if sourceHits.Load() != 1 {
		t.Fatal("redirect source received an unexpected request count")
	}
	if !sourceAuthorizationSeen.Load() || !sourceCustomCredentialSeen.Load() {
		t.Fatal("redirect source did not receive both initial credentials")
	}
	if targetHits.Load() != 0 {
		t.Fatal("cross-origin redirect target received a request")
	}
	if targetCredentialSeen.Load() {
		t.Fatal("cross-origin redirect target received a credential")
	}

	if err := created.Close(context.Background()); err != nil {
		t.Fatal("close redirect canary transport failed")
	}
	assertHTTPSecretCanaryDiagnosticsAbsent(t, sink)
}

func newHTTPSecretCanarySink(t *testing.T) *diagnostics.Sink {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(httpSecretCanary)
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      8,
		MaxItemBytes:  512,
		MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal("construct HTTP canary diagnostics sink failed")
	}
	return sink
}

func assertHTTPSecretHeadersScoped(t *testing.T, names []string) {
	t.Helper()
	wanted := map[string]bool{
		"Authorization": true,
		"X-Mcp-Secret":  true,
	}
	for _, name := range names {
		delete(wanted, stdhttp.CanonicalHeaderKey(name))
	}
	for name := range wanted {
		t.Fatalf("HTTP canary header was not scoped as sensitive: %s", name)
	}
}

func assertHTTPSecretCanaryDiagnosticsAbsent(t *testing.T, sink *diagnostics.Sink) {
	t.Helper()
	for _, item := range sink.Snapshot().Items() {
		values := []string{
			item.Diagnostic.Code,
			item.Diagnostic.Source,
			item.Diagnostic.Hint,
			string(item.Diagnostic.Severity),
			item.Diagnostic.Message.Text(),
		}
		for _, value := range values {
			if containsHTTPSecretMaterial(value) {
				t.Fatal("HTTP diagnostics exposed the canary")
			}
		}
	}
}

type canaryFailureClient struct {
	body                 *chunkedResponseBody
	returnError          bool
	authorizationSeen    atomic.Bool
	customCredentialSeen atomic.Bool
	doCalls              atomic.Int64
	closeCalls           atomic.Int64
}

func (client *canaryFailureClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.doCalls.Add(1)
	client.authorizationSeen.Store(request.Header.Get("Authorization") == "Bearer "+httpSecretCanary)
	client.customCredentialSeen.Store(request.Header.Get("X-MCP-Secret") == httpSecretCanary)
	response := &stdhttp.Response{
		StatusCode: stdhttp.StatusInternalServerError,
		Header:     make(stdhttp.Header),
		Body:       client.body,
		Request:    request,
	}
	if client.returnError {
		return response, errors.New("upstream failure " + httpSecretCanary)
	}
	return response, nil
}

func (*canaryFailureClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *canaryFailureClient) CloseIdleConnections() { client.closeCalls.Add(1) }

var _ netpolicy.Client = (*canaryFailureClient)(nil)

func containsHTTPSecretMaterial(value string) bool {
	fragments := []string{
		httpSecretCanary,
		httpSecretCanary[:len(httpSecretCanary)/2],
		httpSecretCanary[len(httpSecretCanary)/2:],
	}
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
