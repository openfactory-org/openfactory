package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
	"github.com/openfactory-org/openfactory/internal/server"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Open Factory server (API, dashboard, engine and dispatcher)",
		Long: `Run the control plane: REST API, dashboard, Factory MCP, webhook and
integration ingress, the engine and the dispatcher. The dispatcher starts one
sandbox for each session of a run in the configured sandbox backend: kubernetes (the
default, and the only production backend) or local processes (development and
tests; the default with OF_DEV=true). The runner in each sandbox runs the agent and
talks to this server.

Configuration comes from (lowest to highest precedence) built-in defaults, the
YAML file named by --config (env OF_CONFIG) and OF_* environment variables.
--config is the only flag. Probes: /healthz, /readyz; Prometheus metrics:
/metrics.

Quick start:
  OF_DEV=true OF_DATABASE_URL=sqlite://:memory: openfactory serve`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags())
			if err != nil {
				return err
			}
			if w := cfg.LocalBackendWarning(); w != "" {
				cfg.Logger().Warn(w, "sandbox_backend", cfg.SandboxBackend)
			}
			srv, err := server.New(cmd.Context(), cfg, server.WithVersion(version), server.WithBuildInfo(commit, date), server.WithStdout(cmd.OutOrStdout()))
			if err != nil {
				return err
			}
			return srv.Run(cmd.Context())
		},
	}
	config.BindFlags(cmd.Flags())
	return cmd
}

func newWorkerCmd(g *globals) *cobra.Command {
	var (
		backend, id, name, metricsAddr string
		oneShot                        bool
		capacity                       int
		labels                         []string
	)
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a remote worker that runs agent sandboxes for a server",
		Long: `Connect to an Open Factory server and run agent sandboxes in this worker's
Kubernetes cluster. Agents select the worker with workerHost: <id>. The worker
connects outbound, authenticates with a team API key and reconnects
automatically. The server assigns a sandbox, and the runner in each sandbox talks to
the server directly.

--server and --api-key (OF_SERVER, OF_API_KEY) identify the server. Each
flag has an environment variable, shown in the flag list. Sandbox settings are
read from the environment: OF_K8S_NAMESPACE, OF_K8S_POD_TEMPLATE,
OF_KUBECONFIG, OF_SANDBOX_IMAGE, OF_RUNNER_IMAGE and the other OF_K8S_*
settings of the server. The local backend
(--backend local, with OF_SANDBOX_ROOT) runs agents on the host without
isolation and is for development and tests only.

--one-shot takes one sandbox and exits when it exited. The exit code is 0 only for a sandbox that
completed.

Example:
  openfactory worker --server https://factory.example.com --api-key $KEY \
    --backend kubernetes --id cluster-eu-1 --capacity 4 --label region=eu`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.apiKey == "" {
				return errors.New("--api-key (or OF_API_KEY) is required")
			}
			if err := direct.ValidateWorkerID(id); err != nil {
				return err
			}
			if v := strings.TrimSpace(os.Getenv("OF_WORKER_CAPACITY")); v != "" && !cmd.Flags().Changed("capacity") {
				if n, err := strconv.Atoi(v); err != nil || n < 0 {
					return fmt.Errorf("OF_WORKER_CAPACITY %q: want a whole number, 0 or more", v)
				}
			}
			lbls := map[string]string{}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok || k == "" {
					return fmt.Errorf("invalid --label %q (want key=value)", l)
				}
				lbls[k] = v
			}
			backend = strings.ToLower(strings.TrimSpace(backend))
			switch backend {
			case config.BackendKubernetes, config.BackendLocal:
			default:
				return fmt.Errorf("unknown --backend %q (want kubernetes or local)", backend)
			}
			return runWorkerV2(cmd.Context(), g, workerV2Flags{
				backend: backend, id: id, name: name, metricsAddr: metricsAddr,
				oneShot: oneShot, capacity: capacity, labels: lbls,
			})
		},
	}
	host, _ := os.Hostname()
	f := cmd.Flags()
	f.StringVar(&backend, "backend", envOr("OF_SANDBOX_BACKEND", config.DefaultBackend(envBool("OF_DEV"))), "sandbox backend: kubernetes (production) or local (development and tests only; default when OF_DEV=true) (env OF_SANDBOX_BACKEND)")
	f.StringVar(&id, "id", envOr("OF_WORKER_ID", sanitizeID(host)), "worker id agents reference as workerHost (env OF_WORKER_ID)")
	f.StringVar(&name, "name", os.Getenv("OF_WORKER_NAME"), "display name (env OF_WORKER_NAME; default: the id)")
	f.IntVar(&capacity, "capacity", envInt("OF_WORKER_CAPACITY"), "maximum concurrent sandboxes, 0 = unlimited (env OF_WORKER_CAPACITY)")
	f.StringArrayVar(&labels, "label", envList("OF_WORKER_LABELS"), "worker label key=value, repeatable (env OF_WORKER_LABELS, comma-separated)")
	f.BoolVar(&oneShot, "one-shot", envBool("OF_WORKER_ONE_SHOT"), "take exactly one sandbox and exit when it exited; the exit code is 0 only for the reason completed (env OF_WORKER_ONE_SHOT)")
	f.StringVar(&metricsAddr, "metrics-addr", os.Getenv("OF_METRICS_ADDR"), "serve worker Prometheus metrics on this address, e.g. :9090 (env OF_METRICS_ADDR; default off)")
	cmd.AddCommand(newWorkerListCmd(g))
	return cmd
}

// envInt returns the environment variable as an integer, or 0 when it is
// empty or not a number. The command checks the value with the flag.
func envInt(key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	return n
}

// envList splits a comma-separated environment variable. Empty items are skipped.
func envList(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// envBool reports whether the environment variable holds a true boolean.
func envBool(key string) bool {
	b, _ := strconv.ParseBool(os.Getenv(key))
	return b
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "worker"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
