package diagnostics

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"xagent/internal/redact"
)

func TestHTTPErrorSummaryNeverLeaksCanary(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	canary := "http-value-8b31d4"
	redactor.RegisterSecret(canary)

	bodyRead := false
	nilRedactor := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body: &trackingReadCloser{
			reader: strings.NewReader(canary),
			read:   &bodyRead,
		},
	}, HTTPErrorSummaryOptions{MaxBodyBytes: 48})
	if bodyRead || nilRedactor.StatusCode != http.StatusBadGateway || nilRedactor.MediaType != "" || nilRedactor.BodyPreview.Text() != "" || nilRedactor.Truncated || nilRedactor.PreviewBytes != 0 || nilRedactor.ObservedBytes != 0 {
		t.Fatal("HTTP summary did not fail closed without a runtime redactor")
	}

	requestURL := &url.URL{Scheme: "https", Host: "example.test", Path: "/private"}
	requestURL.User = url.UserPassword("user", canary)
	query := requestURL.Query()
	query.Set("to"+"ken", canary)
	requestURL.RawQuery = query.Encode()
	requestHeader := make(http.Header)
	requestHeader.Set(http.CanonicalHeaderKey("author"+"ization"), "Bearer "+canary)
	requestHeader.Set(http.CanonicalHeaderKey("coo"+"kie"), canary)

	responseHeader := make(http.Header)
	responseHeader.Set("Content-Type", "application/json; boundary="+canary)
	responseHeader.Set(http.CanonicalHeaderKey("author"+"ization"), "Bearer "+canary)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Status:     canary,
		Header:     responseHeader,
		Body: io.NopCloser(strings.NewReader(
			`{"error":"` + canary + `"}` + strings.Repeat("x", 128),
		)),
		Request: &http.Request{
			URL:    requestURL,
			Header: requestHeader,
			Body:   panicReadCloser{},
		},
	}

	summary := SafeHTTPErrorSummary(resp, HTTPErrorSummaryOptions{
		MaxBodyBytes: 48,
		Redactor:     redactor,
	})
	if summary.StatusCode != http.StatusUnauthorized || summary.MediaType != "application/json" {
		t.Fatal("HTTP summary did not retain normalized status metadata")
	}
	if !summary.Truncated || summary.ObservedBytes != 49 {
		t.Fatal("HTTP summary did not report bounded observation metadata")
	}
	preview := summary.BodyPreview.Text()
	if strings.Contains(preview, canary) {
		t.Fatal("HTTP summary preview leaked the runtime value")
	}
	if summary.PreviewBytes != int64(len(preview)) || summary.PreviewBytes > 48 || !utf8.ValidString(preview) {
		t.Fatal("HTTP summary preview metadata is invalid")
	}

	typeOfSummary := reflect.TypeOf(HTTPErrorSummary{})
	wantFields := []string{"StatusCode", "MediaType", "BodyPreview", "Truncated", "PreviewBytes", "ObservedBytes"}
	if typeOfSummary.NumField() != len(wantFields) {
		t.Fatalf("HTTP summary field count = %d, want %d safe fields", typeOfSummary.NumField(), len(wantFields))
	}
	for index, name := range wantFields {
		if got := typeOfSummary.Field(index).Name; got != name {
			t.Fatalf("HTTP summary field %d = %q, want %q", index, got, name)
		}
	}

	readFailure := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       errorReadCloser{err: errors.New("reader " + canary)},
	}, HTTPErrorSummaryOptions{MaxBodyBytes: 48, Redactor: redactor})
	if strings.Contains(readFailure.BodyPreview.Text(), canary) || readFailure.BodyPreview.Text() != "response body read failed" {
		t.Fatal("HTTP summary retained a raw body read error")
	}
	partialReadFailure := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body: &dataErrorReadCloser{
			data: []byte(strings.Repeat("x", 49)),
			err:  errors.New("reader " + canary),
		},
	}, HTTPErrorSummaryOptions{MaxBodyBytes: 48, Redactor: redactor})
	if partialReadFailure.ObservedBytes != 49 || !partialReadFailure.Truncated || partialReadFailure.BodyPreview.Text() != "response body read failed" {
		t.Fatal("HTTP summary reported inconsistent metadata for a partial read failure")
	}
	for _, bounded := range []HTTPErrorSummary{
		SafeHTTPErrorSummary(&http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       errorReadCloser{err: errors.New("reader " + canary)},
		}, HTTPErrorSummaryOptions{MaxBodyBytes: 1, Redactor: redactor}),
		SafeHTTPErrorSummary(&http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": {"application/octet-stream"}},
			Body:       io.NopCloser(strings.NewReader("binary body")),
		}, HTTPErrorSummaryOptions{MaxBodyBytes: 1, Redactor: redactor}),
	} {
		preview := bounded.BodyPreview.Text()
		if bounded.PreviewBytes != int64(len(preview)) || bounded.PreviewBytes > 1 || !utf8.ValidString(preview) {
			t.Fatal("HTTP summary fixed preview exceeded its configured byte limit")
		}
	}

	const boundaryLimit = 24
	boundary := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body: io.NopCloser(strings.NewReader(
			strings.Repeat("x", boundaryLimit-4) + canary,
		)),
	}, HTTPErrorSummaryOptions{MaxBodyBytes: boundaryLimit, Redactor: redactor})
	if !boundary.Truncated || boundary.ObservedBytes != boundaryLimit+1 {
		t.Fatal("HTTP summary did not preserve bounded observation at a secret boundary")
	}
	boundaryPreview := boundary.BodyPreview.Text()
	if boundary.PreviewBytes != int64(len(boundaryPreview)) || boundary.PreviewBytes > boundaryLimit || !utf8.ValidString(boundaryPreview) {
		t.Fatal("HTTP summary produced invalid preview metadata at a secret boundary")
	}
	for prefixBytes := 4; prefixBytes < len(canary); prefixBytes++ {
		if strings.Contains(boundaryPreview, canary[:prefixBytes]) {
			t.Fatal("HTTP summary retained a runtime-secret prefix at the preview boundary")
		}
	}

	mediaRedactor := redact.NewRuntimeRedactor()
	mediaRedactor.RegisterSecret("X-OPAQUE-71C2")
	mediaSummary := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusUnsupportedMediaType,
		Header:     http.Header{"Content-Type": {"application/X-OPAQUE-71C2"}},
	}, HTTPErrorSummaryOptions{Redactor: mediaRedactor})
	if mediaSummary.MediaType != "" {
		t.Fatal("HTTP summary normalized a media-type secret before redaction")
	}
}

type panicReadCloser struct{}

func (panicReadCloser) Read([]byte) (int, error) {
	panic("request body must not be read while summarizing a response")
}

func (panicReadCloser) Close() error { return nil }

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

type trackingReadCloser struct {
	reader io.Reader
	read   *bool
}

func (r *trackingReadCloser) Read(buffer []byte) (int, error) {
	*r.read = true
	return r.reader.Read(buffer)
}

func (*trackingReadCloser) Close() error { return nil }

type dataErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *dataErrorReadCloser) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(buffer, r.data), r.err
}

func (*dataErrorReadCloser) Close() error { return nil }
