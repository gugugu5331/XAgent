package transport

import (
	"context"
	"encoding/json"
	"io"

	"xagent/internal/mcpclient/protocol"
)

// TransportEvent contains only one framed protocol payload. Business-level
// MCP values are decoded above the transport boundary.
type TransportEvent struct {
	Frame    json.RawMessage
	Captured *protocol.CapturedCallToolResponse
	// CapturedBytes preserves cumulative frame-budget accounting when the raw
	// frame is intentionally not forwarded upward.
	CapturedBytes int64
}

// OutputCaptureBinder associates one outbound tools/call request with the
// operation-local output writer before any response bytes are read.  It is an
// optional extension so test and migration transports can retain the minimal
// Transport interface while production transports enforce the wire boundary.
type OutputCaptureBinder interface {
	BindOutputCapture(frame json.RawMessage, destination io.Writer) error
}

// OutputCaptureUnbinder releases a borrowed writer on send failure,
// cancellation, or connection shutdown. It is intentionally separate from
// the binder so compatibility transports do not gain a new required method.
type OutputCaptureUnbinder interface {
	UnbindOutputCapture(frame json.RawMessage)
}

// Transport owns framing and physical I/O resources. Connection is its sole
// receiver and owns the single Receive loop.
type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, frame json.RawMessage) error
	Receive(ctx context.Context) (TransportEvent, error)
	Close(ctx context.Context) error
}
