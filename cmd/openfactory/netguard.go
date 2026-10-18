package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

// newNetguardCmd returns the command that runs the egress proxy of a task.
// The Kubernetes backend runs it as a sidecar of a restricted task Pod.
func newNetguardCmd() *cobra.Command {
	var (
		listen       string
		reportListen string
		allow, deny  []string
		alwaysAllow  []string
		unrestricted bool
	)
	cmd := &cobra.Command{
		Use:   "netguard",
		Short: "Run the egress proxy that enforces a network policy",
		Long: `Run an HTTP forward proxy (plain HTTP and CONNECT tunnels) that enforces
one network policy. A request to a host that the policy does not allow gets a
403 answer. The Kubernetes backend runs one proxy as a sidecar of each
restricted sandbox Pod, with the policy of that sandbox.

--report-listen serves the blocked hosts at GET /blocked. The report has no
secret, so it must stay on a loopback address.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			policy := core.EffectiveNetworkPolicy{Restricted: !unrestricted, AllowedHosts: allow, DeniedHosts: deny}
			p := netguard.New(policy, alwaysAllow)
			p.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			addr, err := p.Start(listen)
			if err != nil {
				return fmt.Errorf("netguard: listen on %s: %w", listen, err)
			}
			if reportListen != "" {
				if !loopbackAddr(reportListen) {
					p.Logger.Warn("the report listens on an address that is not loopback: every client can read it",
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
	cmd.Flags().StringVar(&reportListen, "report-listen", "", "address for the report of blocked hosts (GET /blocked), for example 127.0.0.1:3129; use a loopback address")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "allowed host (repeat the flag or separate values with commas)")
	cmd.Flags().StringSliceVar(&deny, "deny", nil, "denied host (repeat the flag or separate values with commas)")
	cmd.Flags().StringSliceVar(&alwaysAllow, "always-allow", nil, "host that is always reachable; a leading \".\" matches subdomains")
	cmd.Flags().BoolVar(&unrestricted, "unrestricted", false, "allow every host that is not denied")
	return cmd
}

// loopbackAddr reports whether a listen address is a loopback IP address.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
