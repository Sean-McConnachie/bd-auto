//go:build unix

package codex

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}

func terminateProcess(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGTERM) }
func killProcess(cmd *exec.Cmd) error      { return signalGroup(cmd, syscall.SIGKILL) }
func processAlive(pid int) bool            { return syscall.Kill(pid, 0) == nil }
