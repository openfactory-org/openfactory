// Command openfactory is the Open Factory server, worker and command-line client.
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
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/pkg/sdk"
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

// globals holds client connection flags.
type globals struct {
	server string
	apiKey string
	output string
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "openfactory",
		Short: "Open Factory: teams of AI coding agents that turn issues, chats and webhooks into reviewed pull requests",
		Long: `openfactory runs the Open Factory control plane (serve), remote workers
(worker), validates factory definitions locally (validate, schema, init), and
talks to a running server through its REST API (bootstrap, factory, run,
work-item, inbox, secret, webhook, benchmark, mcp-registry).

Client commands read the server URL and API key from --server/--api-key or the
OF_SERVER and OF_API_KEY environment variables.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// The help text must never show the value of OF_API_KEY, and OF_SERVER can
	// hold a password in the URL. So the flags have fixed defaults, and the
	// environment sets the variable after the flags are defined.
	root.PersistentFlags().StringVar(&g.server, "server", "http://localhost:8080", "server base URL (env OF_SERVER)")
	root.PersistentFlags().StringVar(&g.apiKey, "api-key", "", "API key (env OF_API_KEY)")
	g.server = envOr("OF_SERVER", g.server)
	g.apiKey = os.Getenv("OF_API_KEY")
	root.PersistentFlags().StringVarP(&g.output, "output", "o", "text", "output format for client commands: text or json")

	root.AddGroup(
		&cobra.Group{ID: "run", Title: "Running Open Factory:"},
		&cobra.Group{ID: "local", Title: "Local definition tools:"},
		&cobra.Group{ID: "client", Title: "Client commands (talk to a server):"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("run", newServeCmd(), newWorkerCmd(g), newMCPCmd(g), newRunnerCmd(), newNetguardCmd())
	add("local", newValidateCmd(), newSchemaCmd(), newInitCmd())
	add("client", newBootstrapCmd(g), newFactoryCmd(g), newRunCmd(g), newWorkItemCmd(g),
		newInboxCmd(g), newSecretCmd(g), newMCPRegistryCmd(g), newWebhookCmd(g), newBenchmarkCmd(g))
	root.AddCommand(newVersionCmd(), newNetlockCmd())
	return root
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (g *globals) client() (*sdk.Client, error) {
	if g.apiKey == "" {
		return nil, errors.New("no API key: pass --api-key or set OF_API_KEY")
	}
	return sdk.NewClient(strings.TrimRight(g.server, "/"), g.apiKey, sdk.WithUserAgent("openfactory/"+version)), nil
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

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String() + " ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
