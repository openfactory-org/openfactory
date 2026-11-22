//go:build unix

package netlock

import (
	"os"
	"os/exec"
	"syscall"
)

// Apply makes cmd run as the user of the credential, with no supplementary
// groups. It keeps the other process attributes of cmd (the process group).
// It does nothing when the process already runs as that user.
func (c Credential) Apply(cmd *exec.Cmd) {
	if c.UID == os.Getuid() && c.GID == os.Getgid() {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID), Groups: []uint32{}} //nolint:gosec // G115: uids are small
}
