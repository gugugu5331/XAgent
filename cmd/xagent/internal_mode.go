package main

import "errors"

const protectedLaunchFailure = "受保护进程启动失败"

// runInternalMode deliberately accepts no CLI arguments, environment payload,
// or input stream. The platform launcher owns the complete hidden protocol and
// may obtain a versioned launch plan only from its fixed inherited handles.
func runInternalMode() (bool, error) {
	handled, err := runPlatformInternalMode()
	if handled && err != nil {
		return true, errors.New(protectedLaunchFailure)
	}
	return handled, err
}
