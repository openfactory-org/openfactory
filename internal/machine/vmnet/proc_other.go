//go:build !linux

package vmnet

import (
	"os"
	"os/exec"
)

// startAs starts the helper as the current user. Worker hosts are Linux
// only; this keeps the package building elsewhere for tests.
func startAs(args []string, _ int) (*exec.Cmd, error) {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec,noctx // the helper outlives any request context; it is stopped by Helper.Stop
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd, cmd.Start()
}
