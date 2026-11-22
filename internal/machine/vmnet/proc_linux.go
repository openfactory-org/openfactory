//go:build linux

package vmnet

import (
	"os"
	"os/exec"
	"syscall"
)

// capNetBindService is CAP_NET_BIND_SERVICE: the DNS relay binds port 53.
const capNetBindService = 10

// startAs starts the helper as uid with CAP_NET_BIND_SERVICE as an ambient
// capability, so the DNS relay can bind port 53 without root.
func startAs(args []string, uid int) (*exec.Cmd, error) {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec,noctx // the helper outlives any request context; it is stopped by Helper.Stop
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}, //nolint:gosec // G115: configured uid
		AmbientCaps: []uintptr{capNetBindService},
		Setsid:      true,
	}
	return cmd, cmd.Start()
}
