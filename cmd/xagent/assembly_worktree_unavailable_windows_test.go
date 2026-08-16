//go:build windows

package main

import "testing"

func TestWorktreeUnavailableWindowsInitializerCapabilityFailsClosed(t *testing.T) {
	if assemblyWorktreeInitializerSupported() {
		t.Fatal("Windows initializer capability was reported available")
	}
}
