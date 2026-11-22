package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// RuntimeName is the name of the runtime binary. The runtime commands
// (runner, netguard, worker, version) use it in their help. The binary is
// renamed to it when the client commands move to the TypeScript CLI.
const RuntimeName = "openfactory-runtime"

// versionString returns the version line: name, version, commit, date and
// platform.
func versionString(name string) string {
	s := name + " " + version
	if commit != "" {
		s += " (" + commit
		if date != "" {
			s += ", " + date
		}
		s += ")"
	}
	return s + " " + runtime.GOOS + "/" + runtime.GOARCH
}

func newVersionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long: `Print the version of ` + RuntimeName + `, its commit, its build date and its
platform. --short prints only the version: the runner sends it as
runner_version, and the control plane refuses a session of another version.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			if short {
				fmt.Fprintln(cmd.OutOrStdout(), version)
				return
			}
			fmt.Fprintln(cmd.OutOrStdout(), versionString(RuntimeName))
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version")
	return cmd
}
