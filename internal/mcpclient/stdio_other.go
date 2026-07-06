//go:build !unix

package mcpclient

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
