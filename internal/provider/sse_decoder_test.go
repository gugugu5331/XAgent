package provider

import (
	"errors"
	"io"
	"strings"
	"testing"

	"xagent/internal/budget"
	"xagent/internal/config"
)

func TestSSEDecoderReportsScannerError(t *testing.T) {
	wantErr := errors.New("injected stream read failure")
	limits := testSSELimits(t, 1024, 256, 8)
	decoder, err := newSSEDecoder(&failingSSEReader{
		data: []byte("data: partial"),
		err:  wantErr,
	}, limits)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}

	if decoder.Next() {
		t.Fatalf("decoder emitted partial event: %#v", decoder.Event())
	}
	if !errors.Is(decoder.Err(), wantErr) {
		t.Fatalf("decoder error = %v, want injected read error", decoder.Err())
	}
	if decoder.Termination() != sseTerminationScanError {
		t.Fatalf("termination = %v, want scanner error", decoder.Termination())
	}
}

func TestSSEDecoderRejectsOversizedEvent(t *testing.T) {
	limits := testSSELimits(t, 1024, 16, 8)
	decoder, err := newSSEDecoder(strings.NewReader("data: 12345\ndata: 67890\n\n"), limits)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}

	if decoder.Next() {
		t.Fatalf("decoder emitted oversized event: %#v", decoder.Event())
	}
	var limitErr *budget.LimitError
	if !errors.As(decoder.Err(), &limitErr) {
		t.Fatalf("decoder error = %v (%T), want *budget.LimitError", decoder.Err(), decoder.Err())
	}
	if limitErr.Scope != string(budget.ProviderMaxEventBytes) || limitErr.Dimension != budget.Bytes || limitErr.Limit != 16 || limitErr.Observed <= limitErr.Limit {
		t.Fatalf("unexpected event limit error: %+v", limitErr)
	}
	if decoder.Termination() != sseTerminationLimitExceeded {
		t.Fatalf("termination = %v, want limit exceeded", decoder.Termination())
	}
	if strings.Contains(decoder.Err().Error(), "12345") || strings.Contains(decoder.Err().Error(), "67890") {
		t.Fatal("event limit error retained SSE payload")
	}
}

func TestSSEDecoderDistinguishesProtocolTerminationAndUnexpectedEOF(t *testing.T) {
	t.Run("protocol termination", func(t *testing.T) {
		limits := testSSELimits(t, 1024, 256, 8)
		decoder, err := newSSEDecoder(strings.NewReader("data:\n\ndata: {\"ok\":true}\n\ndata: [DONE]"), limits)
		if err != nil {
			t.Fatalf("new decoder: %v", err)
		}
		if !decoder.Next() || decoder.Event().Data != "" {
			t.Fatalf("empty data event = %#v, want an emitted empty event", decoder.Event())
		}
		if !decoder.Next() || decoder.Event().Data != `{"ok":true}` {
			t.Fatalf("first event = %#v, want data event", decoder.Event())
		}
		if !decoder.Next() || decoder.Event().Data != "[DONE]" {
			t.Fatalf("terminal event = %#v, want [DONE]", decoder.Event())
		}
		if decoder.Next() {
			t.Fatal("decoder emitted event after protocol termination")
		}
		if decoder.Err() != nil || decoder.Termination() != sseTerminationProtocol {
			t.Fatalf("protocol termination err=%v state=%v", decoder.Err(), decoder.Termination())
		}
	})

	t.Run("unexpected EOF", func(t *testing.T) {
		limits := testSSELimits(t, 1024, 256, 8)
		decoder, err := newSSEDecoder(strings.NewReader("data: {\"ok\":true}\n\n"), limits)
		if err != nil {
			t.Fatalf("new decoder: %v", err)
		}
		if !decoder.Next() || decoder.Event().Data != `{"ok":true}` {
			t.Fatalf("first event = %#v, want data event", decoder.Event())
		}
		if decoder.Next() {
			t.Fatal("decoder emitted event at unexpected EOF")
		}
		if !errors.Is(decoder.Err(), io.ErrUnexpectedEOF) || decoder.Termination() != sseTerminationUnexpectedEOF {
			t.Fatalf("unexpected EOF err=%v state=%v", decoder.Err(), decoder.Termination())
		}
	})
}

func testSSELimits(t *testing.T, responseBytes, eventBytes, events int64) *streamLimits {
	t.Helper()
	limits, err := newStreamLimits(config.StreamConfig{
		MaxResponseBytes: responseBytes,
		MaxEventBytes:    eventBytes,
		MaxEvents:        events,
	})
	if err != nil {
		t.Fatalf("new stream limits: %v", err)
	}
	return limits
}

type failingSSEReader struct {
	data []byte
	err  error
}

func (r *failingSSEReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}
