package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/machine/guest"
)

// vmInitArg is the argument that the guest kernel passes to init: the boot
// arguments end with "init=/openfactory -- vm-init".
const vmInitArg = "vm-init"

// In a microVM the runtime binary is PID 1 with the single argument
// vm-init. It runs vm-init before the command tree exists, so a PID 1 never
// depends on flag parsing.
func init() {
	if len(os.Args) == 2 && os.Args[1] == vmInitArg && os.Getpid() == 1 {
		os.Exit(guest.Main())
	}
}

// newVMInitCmd returns the vm-init command. It only explains itself: vm-init
// runs from init() when the program is PID 1 in a microVM.
func newVMInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   vmInitArg,
		Short: "Init process of a microVM (runs only as PID 1 in the guest)",
		Long: `vm-init is the first process of a microVM worker guest. The guest kernel
starts this program as PID 1 with the single argument vm-init. It mounts the
file systems, reads the session from the metadata service, starts the runner
and powers the VM off when the runner exits. Outside a microVM it refuses to
run.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("vm-init runs only as PID 1 in a microVM")
		},
	}
}
