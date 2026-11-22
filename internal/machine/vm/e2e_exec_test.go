//go:build e2e_firecracker

package vm

import "os/exec"

func osExec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
