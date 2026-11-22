//go:build !linux

package guest

import (
	"fmt"
	"os"
)

// Main refuses to run: vm-init is PID 1 of a Linux microVM.
func Main() int {
	fmt.Fprintln(os.Stderr, "vm-init runs only as PID 1 of a Linux worker microVM")
	return 2
}
