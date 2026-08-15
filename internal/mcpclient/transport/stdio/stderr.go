package stdio

import (
	"errors"
	"io"

	"xagent/internal/diagnostics"
)

const (
	stderrReadBufferBytes          = 32 * 1024
	maxConsecutiveEmptyStderrReads = 100
)

var (
	errStderrObserved = errors.New("MCP stdio server produced stderr output")
	errStderrRead     = errors.New("MCP stdio stderr could not be drained")
)

// stderrWorker owns the only read loop for one borrowed stderr pipe. Process
// remains the pipe owner: this worker never closes the reader, waits for the
// process, or retains any stderr bytes.
type stderrWorker struct {
	done <-chan struct{}
}

func startStderrWorker(reader io.Reader, sink diagnostics.BoundedSink) (*stderrWorker, error) {
	if isNilInterface(reader) || isNilInterface(sink) {
		return nil, ErrInvalidConfig
	}
	done := make(chan struct{})
	worker := &stderrWorker{done: done}
	go drainStderr(reader, sink, done)
	return worker, nil
}

func (worker *stderrWorker) Done() <-chan struct{} {
	if worker == nil {
		return nil
	}
	return worker.done
}

func drainStderr(reader io.Reader, sink diagnostics.BoundedSink, done chan<- struct{}) {
	defer close(done)
	buffer := make([]byte, stderrReadBufferBytes)
	emptyReads := 0
	reportedOutput := false
	for {
		read, readErr := reader.Read(buffer)
		if read < 0 || read > len(buffer) {
			sink.Add(stderrReadFailureDiagnostic())
			return
		}
		if read > 0 {
			clear(buffer[:read])
			emptyReads = 0
			if !reportedOutput {
				sink.Add(stderrObservedDiagnostic())
				reportedOutput = true
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyStderrReads {
				sink.Add(stderrReadFailureDiagnostic())
				return
			}
		}

		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return
		default:
			sink.Add(stderrReadFailureDiagnostic())
			return
		}
	}
}

func stderrObservedDiagnostic() diagnostics.SanitizeInput {
	return diagnostics.SanitizeInput{
		Code:     "mcp_stdio_stderr",
		Source:   "mcp.transport.stdio",
		Hint:     "content_omitted",
		Severity: diagnostics.SeverityWarning,
		Err:      errStderrObserved,
	}
}

func stderrReadFailureDiagnostic() diagnostics.SanitizeInput {
	return diagnostics.SanitizeInput{
		Code:     "mcp_stdio_stderr",
		Source:   "mcp.transport.stdio",
		Hint:     "read_failed",
		Severity: diagnostics.SeverityError,
		Err:      errStderrRead,
	}
}
