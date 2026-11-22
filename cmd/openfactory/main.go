// Command openfactory-runtime is the Open Factory runtime: the in-sandbox
// runner, the egress proxy, the dev process worker, the microVM worker host
// and the microVM init. The control plane and the client commands are
// TypeScript (apps/control-plane and the openfactory npm CLI).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// Set with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// errSilent signals a failure whose details were already printed.
var errSilent = errors.New("")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	root := newRootCmd()
	err := root.ExecuteContext(ctx)
	stop()
	if err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

// globals holds the flags that several commands share.
type globals struct {
	server string
	apiKey string
	output string
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   RuntimeName,
		Short: "Open Factory runtime: runner, egress proxy and workers",
		Long: RuntimeName + ` runs inside sandboxes and on worker hosts (runner, netguard,
worker, vm-init, version). The control plane and the client commands are part
of the openfactory command-line tool, which forwards the runtime commands to
this program.

The dev process worker reads the control plane URL and API key from
--server/--api-key or the OF_SERVER and OF_API_KEY environment variables.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// The help text must never show the value of OF_API_KEY, and OF_SERVER can
	// hold a password in the URL. So the flags have fixed defaults, and the
	// environment sets the variable after the flags are defined.
	root.PersistentFlags().StringVar(&g.server, "server", "http://localhost:8787", "control plane base URL (env OF_SERVER)")
	root.PersistentFlags().StringVar(&g.apiKey, "api-key", "", "API key of the dev process worker (env OF_API_KEY)")
	g.server = envOr("OF_SERVER", g.server)
	g.apiKey = os.Getenv("OF_API_KEY")
	root.PersistentFlags().StringVarP(&g.output, "output", "o", "text", "output format of worker status and doctor: text or json")

	root.AddCommand(newRunnerCmd(), newNetguardCmd(), newWorkerCmd(g), newVMInitCmd(), newVersionCmd(), newNetlockCmd())
	return root
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (g *globals) json() bool { return g.output == "json" }

// emit prints v as indented JSON.
func emit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints rows with aligned columns.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}
