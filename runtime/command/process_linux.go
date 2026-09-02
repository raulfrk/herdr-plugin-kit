//go:build linux

package command

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
func killProcessGroup(cmd *exec.Cmd) error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
func processGroupExists(cmd *exec.Cmd) bool {
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
