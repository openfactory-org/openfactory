package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

// newNetguardCmd returns the command that runs the egress proxy of a task.
// The runner of a restricted sandbox and the microVM worker host start it.
func newNetguardCmd() *cobra.Command {
	var (
		listen       string
		reportListen string
		allow, deny  []string
		alwaysAllow  []string
		privateHosts []string
		unrestricted bool
	)
	cmd := &cobra.Command{
		Use:   "netguard",
		Short: "Run the egress proxy that enforces a network policy",
		Long: `Run an HTTP forward proxy (plain HTTP and CONNECT tunnels) that enforces
one network policy. A request to a host that the policy does not allow gets a
403 answer. A sandbox with a restricted policy runs one proxy, with the
policy of that sandbox; the in-sandbox lock lets only the proxy reach the
network. On a microVM worker host the proxy of each VM listens on the tap
address of the VM.

The proxy never dials a private, loopback, link-local, carrier-grade NAT,
cloud metadata or other reserved address, even when the policy allows the host
name that resolves to it. --allow-private-host names the exact hosts that may
resolve to such an address, for example a control plane on a private network.

--report-listen serves the blocked hosts at GET /blocked. The report has no
secret, but every client that reaches the address can read it: use a loopback
address, or the tap address of the VM on a microVM worker host.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			policy := rtcore.EffectiveNetworkPolicy{Restricted: !unrestricted, AllowedHosts: allow, DeniedHosts: deny}
			p := netguard.New(policy, alwaysAllow)
			p.AllowPrivateHosts(privateHosts)
			p.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			// main cancels the context on SIGTERM or SIGINT.
			ctx := cmd.Context()
			addr, err := p.Start(listen)
			if err != nil {
				return fmt.Errorf("netguard: listen on %s: %w", listen, err)
			}
			if reportListen != "" {
				if !specificAddr(reportListen) {
					p.Logger.Warn("the report listens on a wildcard address: every client can read it",
						"addr", reportListen)
				}
				// The runner of the task reads the blocked hosts here.
				rs := &http.Server{Addr: reportListen, Handler: p.ReportHandler(), ReadHeaderTimeout: 10 * time.Second}
				ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", reportListen)
				if err != nil {
					return fmt.Errorf("netguard: listen for the report on %s: %w", reportListen, err)
				}
				go func() { _ = rs.Serve(ln) }()
				defer rs.Close()
			}
			p.Logger.Info("netguard listening", "addr", addr.String(), "restricted", policy.Restricted, "allowed_hosts", len(allow))
			<-ctx.Done()
			return p.Close()
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:3128", "address to listen on")
	cmd.Flags().StringVar(&reportListen, "report-listen", "", "address for the report of blocked hosts (GET /blocked), for example 127.0.0.1:3129; use a loopback or tap address")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "allowed host (repeat the flag or separate values with commas)")
	cmd.Flags().StringSliceVar(&deny, "deny", nil, "denied host (repeat the flag or separate values with commas)")
	cmd.Flags().StringSliceVar(&alwaysAllow, "always-allow", nil, "host that is always reachable; a leading \".\" matches subdomains")
	cmd.Flags().StringSliceVar(&privateHosts, "allow-private-host", nil, "exact host that may resolve to a private or other reserved address (repeat the flag or separate values with commas)")
	cmd.Flags().BoolVar(&unrestricted, "unrestricted", false, "allow every host that is not denied")
	return cmd
}

// specificAddr reports whether a listen address names one IP address, for
// example a loopback address or the tap address of a microVM. A host name, an
// empty host or a wildcard address (0.0.0.0, ::) gives false.
func specificAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsUnspecified()
}
