//go:build unix

package hook

import (
	"os/exec"
	"syscall"
	"time"
)

func setupCommand(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminateCommand(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	<-timer.C
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	// Also target the process handle directly. The group leader may have changed
	// its process group after Start; a direct kill still guarantees cmd.Wait can
	// reap the root command while inherited pipe holders are closed separately.
	_ = cmd.Process.Kill()
}
