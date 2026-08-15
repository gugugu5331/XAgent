package hook

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

func TestHTTPURLValidation(t *testing.T) {
	valid := []string{"https://example.invalid/path", "http://localhost/path", "http://127.0.0.1:8080/path", "http://[::1]/path"}
	for _, raw := range valid {
		if _, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: raw, present: map[string]bool{"type": true, "url": true}}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	invalid := []string{"/relative", "https://user:pass@example.com", "https://example.com/#fragment", "https://example.com/${TOKEN}", "https://example.com/{{event}}"}
	for _, raw := range invalid {
		if _, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: raw, present: map[string]bool{"type": true, "url": true}}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestHTTPURLLengthExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	prefix := "https://example.invalid/"
	atLimit := prefix + strings.Repeat("a", limits.HTTPURLBytes-len(prefix))
	if len(atLimit) != limits.HTTPURLBytes {
		t.Fatalf("URL is %d bytes, want %d", len(atLimit), limits.HTTPURLBytes)
	}
	for _, item := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "exact", raw: atLimit},
		{name: "plus one", raw: atLimit + "a", wantErr: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: item.raw, present: map[string]bool{"type": true, "url": true}}, limits, func(string) (string, bool) { return "", false }, nil)
			if (err != nil) != item.wantErr {
				t.Fatalf("compile error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestHTTPHeaderValidation(t *testing.T) {
	config := ActionConfig{Type: ActionHTTP, URL: "https://example.invalid", Headers: map[string]string{"Authorization": "Bearer ${API_TOKEN}"}, present: map[string]bool{"type": true, "url": true, "headers": true}}
	runtimeRedactor := redact.NewRuntimeRedactor()
	const canary = "http-header-component-canary-12345"
	action, err := compileHTTPAction(config, DefaultLimits(), func(name string) (string, bool) { return canary, name == "API_TOKEN" }, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if action.headers.Get("Authorization") != "Bearer "+canary {
		t.Fatalf("header expansion = %q", action.headers.Get("Authorization"))
	}
	if got := runtimeRedactor.Text(canary); got != "[redacted]" {
		t.Fatalf("bare header component was not registered: %q", got)
	}
	if got := runtimeRedactor.Text("Bearer " + canary); got != "Bearer [redacted]" {
		t.Fatalf("registered composite header instead of component: %q", got)
	}
	for _, headers := range []map[string]string{{"Host": "x"}, {"Content-Length": "1"}, {"Bad\nName": "x"}, {"X": "bad\rvalue"}, {"x-a": "1", "X-A": "2"}, {"Content-Type": "application/json"}} {
		config.Headers = headers
		if _, err := compileHTTPAction(config, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err == nil {
			t.Fatalf("accepted headers %#v", headers)
		}
	}
}

func TestHTTPExpandedHeaderSecretRedactedFromDecisionHandoff(t *testing.T) {
	const canary = "http-response-component-canary-12345"
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"decision":"deny","reason":%q}`, canary)
	}))
	defer server.Close()

	runtimeRedactor := redact.NewRuntimeRedactor()
	config := ActionConfig{
		Type:     ActionHTTP,
		URL:      server.URL,
		Headers:  map[string]string{"Authorization": "Bearer ${API_TOKEN}"},
		Decision: true,
		present:  map[string]bool{"type": true, "url": true, "headers": true, "decision": true},
	}
	action, err := compileAction(EventToolBefore, false, Duration{}, config, DefaultLimits(), func(name string) (string, bool) {
		return canary, name == "API_TOKEN"
	}, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(newSnapshot([]Rule{{
		Event:   EventToolBefore,
		Source:  Source{Path: "header-decision.yaml", Ordinal: 1, EffectiveOrdinal: 1},
		Timeout: action.timeout,
		action:  action,
	}}), EngineOptions{ProjectRoot: t.TempDir(), Redactor: runtimeRedactor})
	if err != nil {
		t.Fatal(err)
	}

	decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "session", ExecutionID: "execution", TurnID: "turn"}, NewToolInput("call", "Read", map[string]any{}))
	if got := <-authorization; got != "Bearer "+canary {
		t.Fatalf("Authorization header = %q", got)
	}
	if !decision.IsDeny() || decision.Reason() != "[redacted]" || strings.Contains(decision.Reason(), canary) {
		t.Fatalf("unsafe Provider-facing decision = %#v", decision)
	}
}

func TestHTTPSendEventDefaultAndFalse(t *testing.T) {
	falseValue := false
	defaultAction, err := compileHTTPAction(ActionConfig{
		Type:    ActionHTTP,
		URL:     "http://127.0.0.1/default",
		present: map[string]bool{"type": true, "url": true},
	}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	falseAction, err := compileHTTPAction(ActionConfig{
		Type:      ActionHTTP,
		URL:       "http://127.0.0.1/false",
		SendEvent: &falseValue,
		Headers:   map[string]string{"Content-Type": "text/plain"},
		present:   map[string]bool{"type": true, "url": true, "send_event": true, "headers": true},
	}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !defaultAction.sendEvent || falseAction.sendEvent {
		t.Fatalf("send_event defaults: default=%v false=%v", defaultAction.sendEvent, falseAction.sendEvent)
	}

	type observedRequest struct {
		path        string
		contentType string
		body        string
	}
	var observed []observedRequest
	runner := newTestHTTPRunner(DefaultLimits(), nil, func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			var readErr error
			body, readErr = io.ReadAll(request.Body)
			if readErr != nil {
				return nil, readErr
			}
		}
		observed = append(observed, observedRequest{path: request.URL.Path, contentType: request.Header.Get("Content-Type"), body: string(body)})
		return testHTTPResponse(request, http.StatusOK, nil, "ok"), nil
	})
	payload := []byte(`{"sequence":1}`)
	for _, action := range []*httpAction{defaultAction, falseAction} {
		_, runErr := runner.Run(context.Background(), HTTPRequest{
			URL:       action.url,
			Method:    action.method,
			Headers:   action.headers,
			SendEvent: action.sendEvent,
			EventJSON: payload,
		})
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if len(observed) != 2 {
		t.Fatalf("observed %d requests", len(observed))
	}
	if got := observed[0]; got.path != "/default" || got.contentType != "application/json" || got.body != string(payload) {
		t.Fatalf("default request = %#v", got)
	}
	if got := observed[1]; got.path != "/false" || got.contentType != "text/plain" || got.body != "" {
		t.Fatalf("send_event=false request = %#v", got)
	}
}

func TestHTTPRequestAndResponseBodyLimitsExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	var calls []string
	runner := newTestHTTPRunner(limits, nil, func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Path)
		switch request.URL.Path {
		case "/request-exact":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if len(body) != limits.HTTPRequestBytes {
				return nil, fmt.Errorf("request body = %d bytes", len(body))
			}
			return testHTTPResponse(request, http.StatusOK, nil, "ok"), nil
		case "/response-exact":
			return testHTTPResponse(request, http.StatusOK, nil, strings.Repeat("r", limits.HTTPResponseBytes)), nil
		case "/response-over":
			return testHTTPResponse(request, http.StatusOK, nil, strings.Repeat("r", limits.HTTPResponseBytes+1)), nil
		default:
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
	})

	requestExactURL := mustURL(t, "http://127.0.0.1/request-exact")
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: requestExactURL, Method: http.MethodPost, SendEvent: true, EventJSON: bytes.Repeat([]byte("q"), limits.HTTPRequestBytes)}); err != nil {
		t.Fatalf("request exact: %v", err)
	}
	callsBeforeOver := len(calls)
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/request-over"), Method: http.MethodPost, SendEvent: true, EventJSON: bytes.Repeat([]byte("q"), limits.HTTPRequestBytes+1)}); err == nil {
		t.Fatal("request limit+1 accepted")
	}
	if len(calls) != callsBeforeOver {
		t.Fatal("oversize request reached the transport")
	}

	result, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/response-exact"), Method: http.MethodGet})
	if err != nil || len(result.Body) != limits.HTTPResponseBytes {
		t.Fatalf("response exact = %d bytes, %v", len(result.Body), err)
	}
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/response-over"), Method: http.MethodGet}); err == nil {
		t.Fatal("response limit+1 accepted")
	}
}

func TestHTTPRequestAndResponseBudgetsApplyBeforeFirstByte(t *testing.T) {
	limits := DefaultLimits()
	policy := netpolicy.NewPolicy()
	client := &testPolicyClient{do: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request unexpectedly reached client")
	}}
	factory := &testClientFactory{client: client}
	runner := &DefaultHTTPRunner{Limits: limits, Policy: policy, ClientFactory: factory}

	requests := []HTTPRequest{
		{
			URL:       mustURL(t, "http://127.0.0.1/body"),
			Method:    http.MethodPost,
			SendEvent: true,
			EventJSON: bytes.Repeat([]byte("x"), limits.HTTPRequestBytes+1),
		},
		{
			URL:    mustURL(t, "http://127.0.0.1/headers"),
			Method: http.MethodGet,
			Headers: http.Header{
				"X-Many": make([]string, limits.HTTPHeaderCount+1),
			},
		},
		{
			URL:       mustURL(t, "http://127.0.0.1/generated-header"),
			Method:    http.MethodPost,
			SendEvent: true,
			Headers:   distinctHeaders(limits.HTTPHeaderCount),
			EventJSON: []byte(`{}`),
		},
	}
	for index, request := range requests {
		if _, err := runner.Run(context.Background(), request); err == nil {
			t.Fatalf("oversize request %d accepted", index)
		}
	}
	if got := factory.news.Load(); got != 0 {
		t.Fatalf("request budget created %d policy clients", got)
	}

	body := &countingReadCloser{Reader: strings.NewReader("must not be read")}
	responseRunner := newTestHTTPRunner(limits, nil, func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          body,
			ContentLength: int64(limits.HTTPResponseBytes + 1),
			Request:       request,
		}, nil
	})
	if _, err := responseRunner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/known-over"), Method: http.MethodGet}); err == nil {
		t.Fatal("known oversize response accepted")
	}
	if got := body.reads.Load(); got != 0 {
		t.Fatalf("known oversize response consumed %d body reads", got)
	}
}

func TestHTTPResponseHeaderLimitExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	limits.HTTPResponseBytes = 256
	const name = "X-Boundary"
	baseBytes := 2 + len(name) + 2 + 2
	if baseBytes >= limits.HTTPResponseBytes {
		t.Fatal("invalid test limit")
	}
	for _, item := range []struct {
		name       string
		valueBytes int
		wantErr    bool
	}{
		{name: "exact", valueBytes: limits.HTTPResponseBytes - baseBytes},
		{name: "plus one", valueBytes: limits.HTTPResponseBytes - baseBytes + 1, wantErr: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			runner := newTestHTTPRunner(limits, nil, func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, http.StatusOK, http.Header{name: []string{strings.Repeat("h", item.valueBytes)}}, "ok"), nil
			})
			_, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/header"), Method: http.MethodGet})
			if (err != nil) != item.wantErr {
				t.Fatalf("run error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestHTTPGzipLimitAppliesAfterDecompression(t *testing.T) {
	limits := DefaultLimits()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		size := limits.HTTPResponseBytes
		if request.URL.Path == "/over" {
			size++
		}
		writer.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(writer)
		_, _ = compressed.Write(bytes.Repeat([]byte("g"), size))
		_ = compressed.Close()
	}))
	defer server.Close()

	exact, err := (&DefaultHTTPRunner{}).Run(context.Background(), HTTPRequest{URL: mustURL(t, server.URL+"/exact"), Method: http.MethodGet})
	if err != nil || len(exact.Body) != limits.HTTPResponseBytes {
		t.Fatalf("gzip exact = %d bytes, %v", len(exact.Body), err)
	}
	if _, err := (&DefaultHTTPRunner{}).Run(context.Background(), HTTPRequest{URL: mustURL(t, server.URL+"/over"), Method: http.MethodGet}); err == nil {
		t.Fatal("gzip decompressed limit+1 accepted")
	}
}

func TestHTTPFailures(t *testing.T) {
	sentinel := errors.New("network unavailable")
	expiredContext, cancelExpiredContext := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpiredContext()
	cases := []struct {
		name      string
		ctx       context.Context
		transport roundTripFunc
	}{
		{
			name: "non-2xx",
			ctx:  context.Background(),
			transport: func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, http.StatusServiceUnavailable, nil, "unavailable"), nil
			},
		},
		{
			name: "network",
			ctx:  context.Background(),
			transport: func(*http.Request) (*http.Response, error) {
				return nil, sentinel
			},
		},
		{
			name: "timeout",
			ctx:  expiredContext,
			transport: func(request *http.Request) (*http.Response, error) {
				return nil, request.Context().Err()
			},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			runner := newTestHTTPRunner(DefaultLimits(), nil, item.transport)
			if _, err := runner.Run(item.ctx, HTTPRequest{URL: mustURL(t, "http://127.0.0.1/failure"), Method: http.MethodGet}); err == nil {
				t.Fatal("failure accepted")
			}
		})
	}
}

func TestHTTPHookRejectsUnsafeRedirectWithoutCredentialLeak(t *testing.T) {
	const canary = "http-hook-redirect-credential-canary-12345"
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()

	authorization := make(chan string, 1)
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization <- request.Header.Get("Authorization")
		writer.Header().Set("Location", destination.URL+"/steal?token="+canary)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	runtimeRedactor := redact.NewRuntimeRedactor()
	action, err := compileAction(EventToolBefore, false, Duration{}, ActionConfig{
		Type:    ActionHTTP,
		URL:     redirector.URL,
		Headers: map[string]string{"Authorization": "Bearer ${HOOK_TOKEN}"},
		present: map[string]bool{"type": true, "url": true, "headers": true},
	}, DefaultLimits(), func(name string) (string, bool) {
		return canary, name == "HOOK_TOKEN"
	}, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
	engine, err := NewEngine(newSnapshot([]Rule{{
		Event:  EventToolBefore,
		Source: Source{Path: "redirect.yaml", Ordinal: 1, EffectiveOrdinal: 1},
		action: action,
	}}), EngineOptions{
		ProjectRoot:       t.TempDir(),
		LegacyDiagnostics: collector,
		Redactor:          runtimeRedactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if runner, ok := engine.http.(*DefaultHTTPRunner); ok {
			runner.CloseIdleConnections()
		}
	})

	decision := engine.BeforeTool(context.Background(), ExecutionRef{
		SessionID: "session", ExecutionID: "execution", TurnID: "turn",
	}, NewToolInput("call", "Read", map[string]any{}))
	if decision.IsDeny() {
		t.Fatalf("non-decision HTTP failure changed permission: %#v", decision)
	}
	if got := <-authorization; got != "Bearer "+canary {
		t.Fatalf("intended origin Authorization = %q", got)
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("unsafe redirect reached destination %d times", got)
	}
	diagnosticsText := ""
	for _, item := range collector.List() {
		diagnosticsText += item.Text()
	}
	if !strings.Contains(diagnosticsText, DiagnosticHTTPFailed) {
		t.Fatalf("missing safe HTTP failure diagnostic: %q", diagnosticsText)
	}
	if strings.Contains(diagnosticsText, canary) {
		t.Fatalf("credential leaked through HTTP failure diagnostic: %q", diagnosticsText)
	}
}

func TestHTTPErrorUsesBoundedSafeSummary(t *testing.T) {
	const canary = "http-hook-error-body-canary-12345"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	limits := DefaultLimits()
	limits.HTTPErrorPreviewBytes = 64
	runner := newTestHTTPRunner(limits, runtimeRedactor, func(request *http.Request) (*http.Response, error) {
		response := testHTTPResponse(request, http.StatusBadGateway, http.Header{"Content-Type": []string{"text/plain"}}, canary+strings.Repeat("x", 256))
		return response, nil
	})
	_, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "http://127.0.0.1/error"), Method: http.MethodGet})
	if err == nil {
		t.Fatal("non-success response accepted")
	}
	if strings.Contains(err.Error(), canary) || len(err.Error()) > 512 {
		t.Fatalf("unsafe or unbounded HTTP summary: %q", err)
	}
	if !strings.Contains(err.Error(), "status=502") || !strings.Contains(err.Error(), "truncated=true") {
		t.Fatalf("HTTP summary lacks safe metadata: %q", err)
	}
}

func TestHTTPRunnerOwnsAndClosesPolicyClientOnce(t *testing.T) {
	client := &testPolicyClient{do: func(request *http.Request) (*http.Response, error) {
		return testHTTPResponse(request, http.StatusOK, nil, "ok"), nil
	}}
	policy := netpolicy.NewPolicy()
	runner := &DefaultHTTPRunner{
		Policy:        policy,
		ClientFactory: &testClientFactory{client: client},
	}
	request := HTTPRequest{URL: mustURL(t, "http://127.0.0.1/owned"), Method: http.MethodGet}
	if _, err := runner.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runner.CloseIdleConnections()
	runner.CloseIdleConnections()
	if got := client.closes.Load(); got != 1 {
		t.Fatalf("policy client closed %d times", got)
	}
	if _, err := runner.Run(context.Background(), request); err == nil {
		t.Fatal("closed HTTP runner created or reused a client")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

type testPolicyClient struct {
	do     roundTripFunc
	closes atomic.Int32
}

func (c *testPolicyClient) Do(request *http.Request) (*http.Response, error) {
	return c.do(request)
}

func (*testPolicyClient) SDKHTTPClient() *http.Client { return nil }

func (c *testPolicyClient) CloseIdleConnections() { c.closes.Add(1) }

type testClientFactory struct {
	client netpolicy.Client
	news   atomic.Int32
}

func (f *testClientFactory) New(netpolicy.Endpoint, netpolicy.ClientOptions) (netpolicy.Client, error) {
	f.news.Add(1)
	return f.client, nil
}

type countingReadCloser struct {
	io.Reader
	reads atomic.Int32
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	r.reads.Add(1)
	return r.Reader.Read(buffer)
}

func (*countingReadCloser) Close() error { return nil }

func distinctHeaders(count int) http.Header {
	headers := make(http.Header, count)
	for index := 0; index < count; index++ {
		headers.Set(fmt.Sprintf("X-Budget-%d", index), "value")
	}
	return headers
}

func newTestHTTPRunner(limits Limits, runtimeRedactor *redact.RuntimeRedactor, do roundTripFunc) *DefaultHTTPRunner {
	policy := netpolicy.NewPolicy()
	return &DefaultHTTPRunner{
		Limits:        limits,
		Policy:        policy,
		ClientFactory: &testClientFactory{client: &testPolicyClient{do: do}},
		Redactor:      runtimeRedactor,
	}
}

func testHTTPResponse(request *http.Request, status int, headers http.Header, body string) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
