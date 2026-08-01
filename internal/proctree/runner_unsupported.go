package proctree

import "context"

// unsupportedRunner is the explicit fail-closed implementation for platforms
// without a protected process-tree backend. It is also kept buildable on
// supported platforms so the refusal path can be exercised by required tests.
type unsupportedRunner struct{}

func newUnsupportedRunner(Options) (*unsupportedRunner, error) {
	return &unsupportedRunner{}, nil
}

func (r *unsupportedRunner) Start(_ context.Context, request Request) (Process, error) {
	if request.Protection.validForStart() {
		_ = request.Protection.cleanupScratch()
	}
	return nil, newStartError(startCodeProtectedExecUnavailable)
}
