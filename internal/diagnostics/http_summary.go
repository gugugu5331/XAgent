package diagnostics

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

const DefaultHTTPBodyPreviewBytes = 1024

type HTTPErrorSummary struct {
	StatusCode  int
	ContentType string
	BodyPreview string
	Truncated   bool
	BodyBytes   int64
}

type HTTPErrorSummaryOptions struct {
	MaxBodyBytes int
	Redactor     Redactor
}

func SafeHTTPErrorSummary(resp *http.Response, options HTTPErrorSummaryOptions) HTTPErrorSummary {
	if resp == nil {
		return HTTPErrorSummary{}
	}
	limit := options.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultHTTPBodyPreviewBytes
	}
	summary := HTTPErrorSummary{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	if resp.Body == nil {
		return summary
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		summary.BodyPreview = "failed to read response body"
		return summary
	}
	summary.Truncated = len(data) > limit
	if summary.Truncated {
		data = data[:limit]
	}
	summary.BodyBytes = int64(len(data))
	if !isTextContent(summary.ContentType, data) {
		summary.BodyPreview = "binary or unsupported response body"
		return summary
	}
	preview := safeUTF8Prefix(string(data), limit)
	if options.Redactor != nil {
		preview = options.Redactor(preview)
	}
	summary.BodyPreview = strings.TrimSpace(preview)
	return summary
}

func isTextContent(contentType string, data []byte) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
			return true
		}
		if mediaType != "" {
			return false
		}
	}
	return utf8.Valid(data) && !bytes.Contains(data, []byte{0})
}

func safeUTF8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
