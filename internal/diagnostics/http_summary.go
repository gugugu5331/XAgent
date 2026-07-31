package diagnostics

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"xagent/internal/budget"
	"xagent/internal/redact"
)

const (
	DefaultHTTPBodyPreviewBytes = 1024
	maxHTTPMediaTypeBytes       = 256
)

type HTTPErrorSummary struct {
	StatusCode    int
	MediaType     string
	BodyPreview   redact.SafeText
	Truncated     bool
	PreviewBytes  int64
	ObservedBytes int64
}

type HTTPErrorSummaryOptions struct {
	MaxBodyBytes int
	Redactor     *redact.RuntimeRedactor
}

func SafeHTTPErrorSummary(resp *http.Response, options HTTPErrorSummaryOptions) HTTPErrorSummary {
	if resp == nil {
		return HTTPErrorSummary{}
	}
	summary := HTTPErrorSummary{StatusCode: resp.StatusCode}
	if options.Redactor == nil {
		return summary
	}
	limit := options.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultHTTPBodyPreviewBytes
	}
	limit = capHTTPBodyPreviewBytes(limit)
	summary.MediaType = normalizedMediaType(resp.Header.Get("Content-Type"), options.Redactor)
	if resp.Body == nil {
		return summary
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	summary.ObservedBytes = int64(len(data))
	summary.Truncated = len(data) > limit
	if err != nil {
		summary.BodyPreview = finalizeHTTPBodyPreview("response body read failed", limit, options.Redactor)
		summary.PreviewBytes = int64(len(summary.BodyPreview.Text()))
		return summary
	}
	if summary.Truncated {
		data = data[:limit]
	}
	if !isTextContent(summary.MediaType, data) {
		summary.BodyPreview = finalizeHTTPBodyPreview("binary or unsupported response body", limit, options.Redactor)
		summary.PreviewBytes = int64(len(summary.BodyPreview.Text()))
		return summary
	}
	// Drop invalid byte fragments before redaction so an incomplete UTF-8 rune
	// at the observation boundary cannot expand and defeat the trailing safety
	// window below.
	preview := strings.ToValidUTF8(string(data), "")
	preview = sanitizeTerminalText(options.Redactor.Redact(preview).Text())
	if summary.Truncated {
		// A registered secret split by the read boundary can leave at most
		// MaxSecretBytes-1 raw bytes at the end. Complete secrets have already
		// been replaced, so removing this suffix cannot expose a new prefix.
		preview = dropTrailingUTF8Bytes(preview, options.Redactor.MaxSecretBytes()-1)
	}
	summary.BodyPreview = finalizeHTTPBodyPreview(preview, limit, options.Redactor)
	summary.PreviewBytes = int64(len(summary.BodyPreview.Text()))
	return summary
}

func finalizeHTTPBodyPreview(value string, limit int, redactor *redact.RuntimeRedactor) redact.SafeText {
	if limit <= 0 {
		return redactor.Redact("")
	}
	value = truncateString(strings.TrimSpace(sanitizeTerminalText(value)), limit)
	safe := redactor.Redact(value)
	if len(safe.Text()) <= limit {
		return safe
	}
	// A redaction marker can be longer than the value it replaced. Truncate
	// only already-redacted text, then cross the SafeText boundary again.
	safe = redactor.Redact(truncateString(safe.Text(), limit))
	if len(safe.Text()) <= limit {
		return safe
	}
	// Fail closed if a newly exposed token boundary expands a second time.
	return redactor.Redact("")
}

func capHTTPBodyPreviewBytes(requested int) int {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope != budget.DiagnosticsMaxItemBytes {
			continue
		}
		if int64(requested) > spec.HardCap {
			return int(spec.HardCap)
		}
		return requested
	}
	// The budget contract is compiled into this package. If it is ever
	// removed, fail closed with an empty preview instead of becoming unbounded.
	return 0
}

func dropTrailingUTF8Bytes(value string, count int) string {
	if count <= 0 {
		return value
	}
	if count >= len(value) {
		return ""
	}
	return truncateString(value, len(value)-count)
}

func normalizedMediaType(contentType string, redactor *redact.RuntimeRedactor) string {
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return ""
	}
	rawMediaType := contentType
	if separator := strings.IndexByte(rawMediaType, ';'); separator >= 0 {
		rawMediaType = rawMediaType[:separator]
	}
	rawMediaType = redactor.Redact(strings.TrimSpace(rawMediaType)).Text()
	mediaType, _, err := mime.ParseMediaType(rawMediaType)
	if err != nil {
		return ""
	}
	mediaType = strings.ToLower(mediaType)
	return truncateString(sanitizeTerminalText(mediaType), maxHTTPMediaTypeBytes)
}

func isTextContent(mediaType string, data []byte) bool {
	if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return true
	}
	if mediaType != "" {
		return false
	}
	return utf8.Valid(data) && !bytes.Contains(data, []byte{0})
}
