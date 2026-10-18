package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

// newNetprobeCmd returns the hidden command that the probe Pods of the
// Kubernetes backend run, to find out whether the cluster enforces
// NetworkPolicy (package egress).
func newNetprobeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "netprobe",
		Short:  "Test commands for the NetworkPolicy check of the Kubernetes backend",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	var listen string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Accept TCP connections and close them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return egress.ServeProbe(ctx, listen)
		},
	}
	serve.Flags().StringVar(&listen, "listen", ":8080", "address to listen on")

	var (
		addr                     string
		delay, interval, timeout time.Duration
		attempts                 int
	)
	dial := &cobra.Command{
		Use:   "dial",
		Short: "Try to open a TCP connection. Exit code 0 means that it worked.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			if egress.DialProbe(ctx, addr, delay, attempts, interval, timeout) {
				return nil
			}
			return errors.New("could not connect to " + addr)
		},
	}
	dial.Flags().StringVar(&addr, "addr", "", "host:port to connect to")
	dial.Flags().DurationVar(&delay, "delay", 0, "time to wait before the first attempt")
	dial.Flags().IntVar(&attempts, "attempts", 1, "number of attempts")
	dial.Flags().DurationVar(&interval, "interval", time.Second, "time between two attempts")
	dial.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "time of one attempt")
	_ = dial.MarkFlagRequired("addr")

	var (
		waitAddr, waitHost string
		waitPort           int
		waitTimeout        time.Duration
	)
	wait := &cobra.Command{
		Use:   "wait-blocked",
		Short: "Wait until a direct connection fails (the NetworkPolicy of the Pod is active)",
		Long: "Dials a target that the NetworkPolicy of a restricted sandbox never allows, until the dial fails.\n" +
			"A sandbox Pod runs this in an init container before the sandbox starts. Exit code 0 means that the\n" +
			"connection is blocked. Exit code " + strconv.Itoa(egress.WaitExitCode) + " means that it still works when the time is over.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr := waitAddr
			if addr == "" {
				if waitHost == "" {
					return errors.New("set --addr, or --host and --port")
				}
				addr = net.JoinHostPort(waitHost, strconv.Itoa(waitPort))
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			took, err := egress.WaitBlocked(ctx, addr, egress.WaitOptions{Timeout: waitTimeout})
			switch {
			case err == nil:
				fmt.Fprintf(cmd.OutOrStdout(), "egress-wait: direct connections to %s are blocked after %s\n", addr, took.Round(time.Millisecond))
				return nil
			case errors.Is(err, egress.ErrNotBlocked):
				fmt.Fprintln(os.Stderr, egress.WaitMessage(addr, waitTimeout))
				os.Exit(egress.WaitExitCode)
			}
			return err
		},
	}
	wait.Flags().StringVar(&waitAddr, "addr", "", "host:port of the target")
	wait.Flags().StringVar(&waitHost, "host", "", "host of the target (with --port)")
	wait.Flags().IntVar(&waitPort, "port", egress.WaitPort, "port of the target (with --host)")
	wait.Flags().DurationVar(&waitTimeout, "timeout", egress.DefaultWaitTimeout, "time to wait")
	cmd.AddCommand(serve, dial, wait)
	return cmd
}
