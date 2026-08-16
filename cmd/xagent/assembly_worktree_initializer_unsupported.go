//go:build !darwin && !linux

package main

func assemblyWorktreeInitializerSupported() bool { return false }
