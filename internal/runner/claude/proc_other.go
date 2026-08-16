//go:build !unix

package claude

import "os/exec"

// Everywhere without process groups the best available answer is the direct
// child, so cancellation is correct but not thorough: a grandchild can outlive
// the run. bd-auto targets unix, and this file exists so the package still
// builds elsewhere rather than to make that guarantee portable.

func setProcessGroup(*exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killProcess(cmd *exec.Cmd) error { return terminateProcess(cmd) }

func processAlive(int) bool { return false }
