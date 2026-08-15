package transport

import (
	"errors"
	"time"

	"xagent/internal/diagnostics"
)

const MaxCleanupTimeout = 2 * time.Second

type Options struct {
	CleanupTimeout time.Duration
	Diagnostics    diagnostics.BoundedSink
}

type resolvedOptions struct {
	cleanupTimeout time.Duration
	diagnostics    diagnostics.BoundedSink
}

func resolveOptions(options Options) (resolvedOptions, error) {
	if options.Diagnostics == nil {
		return resolvedOptions{}, errors.New("MCP transport diagnostics sink is required")
	}
	timeout := options.CleanupTimeout
	if timeout < 0 {
		return resolvedOptions{}, errors.New("MCP transport cleanup timeout is invalid")
	}
	if timeout == 0 {
		timeout = MaxCleanupTimeout
	}
	if timeout > MaxCleanupTimeout {
		return resolvedOptions{}, errors.New("MCP transport cleanup timeout exceeds hard limit")
	}
	return resolvedOptions{cleanupTimeout: timeout, diagnostics: options.Diagnostics}, nil
}
