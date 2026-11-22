//go:build !unix

package netlock

import "os/exec"

// Apply does nothing on this platform: the lock needs a unix system.
func (c Credential) Apply(cmd *exec.Cmd) {}
