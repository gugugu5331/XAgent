//go:build !unix

package hook

import (
	"os/exec"
	"time"
)

func setupCommand(*exec.Cmd) {}
func terminateCommand(cmd *exec.Cmd, _ time.Duration) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
