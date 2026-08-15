package main

import (
	"context"
	"io"

	"xagent/internal/diagnostics"
)

// These aliases keep the pre-cutover unit seams available to historical
// startup tests. They are test-only: the production entry has no reference to
// the legacy factory graph or composite closer.

type startupFactories = legacyStartupFactories
type appCloser = legacyAppCloser
type hookShutdowner = legacyHookShutdowner
type hookShutdownNoticeSource = legacyHookShutdownNoticeSource
type mcpRuntime = legacyMCPRuntime
type shutdownTarget = legacyShutdownTarget
type compositeCloser = legacyCompositeCloser

const (
	shutdownApp            = legacyShutdownApp
	shutdownHook           = legacyShutdownHook
	shutdownMCP            = legacyShutdownMCP
	shutdownProviderClient = legacyShutdownProviderClient
)

func defaultStartupFactories() startupFactories { return legacyStartupFactoriesDefault() }

func runWithFactories(args []string, factories startupFactories) error {
	return legacyRunWithFactories(args, factories)
}

func newCompositeCloser(collector *diagnostics.Collector, redactText func(string) string, stderr io.Writer) *compositeCloser {
	return newLegacyCompositeCloser(collector, redactText, stderr)
}

func newShutdownContext(target shutdownTarget) (context.Context, context.CancelFunc) {
	return newLegacyShutdownContext(target)
}

func shutdownLabel(target shutdownTarget) string { return legacyShutdownLabel(target) }
