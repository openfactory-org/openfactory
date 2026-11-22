package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
)

// newNetlockCmd returns the hidden commands of the in-sandbox egress lock.
func newNetlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "netlock",
		Short:  "Commands of the in-sandbox egress lock",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "Probe the egress lock as the agent user",
		Long: `Read a probe specification (JSON) from standard input, try the
connections that the lock must block and the ones it must allow, and print the
result as JSON. The runner starts it as the agent user after it applied the
lock. The command exits with 0 when it ran, whatever the result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			if err != nil {
				return err
			}
			var spec netlock.ProbeSpec
			if err := json.Unmarshal(data, &spec); err != nil {
				return fmt.Errorf("netlock probe: decode the specification: %w", err)
			}
			res := netlock.RunProbe(cmd.Context(), spec, nil)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "rules",
		Short: "Print the iptables and ip6tables rules of the lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ports, err := netlock.PortsFromEnv()
			if err != nil {
				return err
			}
			agent := netlock.AgentCredential()
			_, err = fmt.Fprint(cmd.OutOrStdout(), netlock.Script(netlock.RuleParams{
				AgentUID: agent.UID, ProxyUID: netlock.ProxyUserAvoiding(agent.UID), LoopbackDaemonPorts: ports,
			}))
			return err
		},
	})
	return cmd
}
