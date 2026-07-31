package proctree

import (
	"context"
	"io"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/safefs"
)

type ProtectionMode uint8

const ProtectionRequired ProtectionMode = 1

type ProtectionPlan struct {
	seal    *protectionSeal
	roots   []*safefs.Root
	scratch *safefs.Root
}

type protectionSeal struct{}

type Pipes struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

type StartError struct {
	Code          string
	TargetStarted bool
}

const (
	startCodeProtectedExecUnavailable = "protected_exec_unavailable"
	startCodeProcessStartFailed       = "process_start_failed"
)

func (e *StartError) Error() string {
	if e == nil || e.Code == "" {
		return "proctree start failed"
	}
	return e.Code
}

func newStartError(code string) *StartError {
	if code != startCodeProtectedExecUnavailable && code != startCodeProcessStartFailed {
		code = startCodeProcessStartFailed
	}
	return &StartError{Code: code, TargetStarted: false}
}

type Request struct {
	Executable string
	Args       []string
	WorkingDir *safefs.Root
	Env        []string
	Mode       ProtectionMode
	Protection ProtectionPlan
}

type Result struct {
	ExitCode  int
	Cancelled bool
	TimedOut  bool
}

type Process interface {
	Pipes() Pipes
	Wait(ctx context.Context) (Result, error)
	Terminate(ctx context.Context) error
	Close(ctx context.Context) error
}

type Runner interface {
	Start(ctx context.Context, request Request) (Process, error)
}

type Options struct {
	CleanupTimeout time.Duration
	Diagnostics    diagnostics.BoundedSink
}
