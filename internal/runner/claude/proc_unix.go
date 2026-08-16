//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a process group of its own, which is what
// makes cancellation actually cancel.
//
// Without it there is nothing to signal but the direct child: a worker forty
// seconds into `go test ./...` would leave the test running, holding its
// worktree, long after the run it belonged to was abandoned.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalGroup sends sig to the whole group led by the child. The negative pid
// is what turns one kill into a group kill; with Setpgid the child's pid is
// also its group id.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// terminateProcess asks the group to exit.
func terminateProcess(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGTERM) }

// killProcess ends the group, for whatever ignored the ask.
func killProcess(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }

// processAlive reports whether pid still exists. Signal 0 checks for the
// process without disturbing it.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
