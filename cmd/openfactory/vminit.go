package main

import (
	"os"

	"github.com/openfactory-org/openfactory/internal/machine/guest"
)

// vmInitArg is the argument that the guest kernel passes to init: the boot
// arguments end with "init=/openfactory -- vm-init".
const vmInitArg = "vm-init"

// In a microVM the runtime binary is PID 1 with the single argument
// vm-init. It runs vm-init before the command tree exists, so a PID 1 never
// depends on flag parsing. Anywhere else the argument goes to cobra, which
// does not know it.
func init() {
	if len(os.Args) == 2 && os.Args[1] == vmInitArg && os.Getpid() == 1 {
		os.Exit(guest.Main())
	}
}
