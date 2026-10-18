//go:build !unix

package tasklocal

import "os/exec"

// setProcessGroup does nothing on this platform.
func setProcessGroup(cmd *exec.Cmd) {}

// terminateGroup stops the process, because this platform has no process group signal.
func terminateGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killGroup stops the process, because this platform has no process group signal.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
