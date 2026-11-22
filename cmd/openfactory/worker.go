package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

// backendLocal is the only sandbox backend of the dev process worker.
const backendLocal = "local"

func newWorkerCmd(g *globals) *cobra.Command {
	var (
		backend, id, name, metricsAddr, keyFile string
		oneShot, process                        bool
		capacity                                int
		labels                                  []string
	)
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a microVM worker host (enroll, run, status, drain, doctor), or a dev process worker (--process)",
		Long: `A microVM worker host runs each session in a Firecracker microVM. Set it up
with the subcommands: worker doctor, worker enroll, then worker run (the
openfactory-worker service); worker status and worker drain operate it.

Without a subcommand, the dev process worker (--process) connects to an Open
Factory control plane and runs each assigned session as a
runner process on this host. Agents select the worker with workerHost: <id>.
The worker connects outbound, authenticates with a team API key and reconnects
automatically. The runner of each session talks to the control plane directly.

The process worker gives no isolation: it is for development and tests only.
The control plane accepts it only when OF_DEV_PROCESS_WORKERS is true.
OF_SANDBOX_ROOT is the directory of the task directories (default
~/.openfactory/sandboxes). OF_RUNNER_COMMAND is the command that starts the
runner (default: this program with the argument runner).

--server and --api-key (OF_SERVER, OF_API_KEY) identify the control plane.
--key-file (OF_WORKER_KEY_FILE) names the Ed25519 private key of an enrolled
worker; the worker signs the challenge of the control plane with it.

--one-shot takes one session and exits when it exited. The exit code is 0 only
for a session that completed.

Example:
  ` + RuntimeName + ` worker --process --server https://factory.example.com --api-key $KEY \
    --id dev-box-1 --capacity 2 --label region=eu`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.apiKey == "" && keyFile == "" {
				return errors.New("--api-key (or OF_API_KEY) or --key-file (or OF_WORKER_KEY_FILE) is required")
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
			if process {
				backend = backendLocal
			}
			if backend != backendLocal {
				return fmt.Errorf("unknown --backend %q (want local, or use --process)", backend)
			}
			return runWorkerV2(cmd.Context(), g, workerV2Flags{
				backend: backend, id: id, name: name, metricsAddr: metricsAddr, keyFile: keyFile,
				oneShot: oneShot, capacity: capacity, labels: lbls,
			})
		},
	}
	host, _ := os.Hostname()
	f := cmd.Flags()
	f.BoolVar(&process, "process", false, "run each session as a runner process on this host (development and tests only)")
	f.StringVar(&backend, "backend", envOr("OF_SANDBOX_BACKEND", backendLocal), "sandbox backend: local (the same as --process) (env OF_SANDBOX_BACKEND)")
	f.StringVar(&id, "id", envOr("OF_WORKER_ID", sanitizeID(host)), "worker id agents reference as workerHost (env OF_WORKER_ID)")
	f.StringVar(&name, "name", os.Getenv("OF_WORKER_NAME"), "display name (env OF_WORKER_NAME; default: the id)")
	f.IntVar(&capacity, "capacity", envInt("OF_WORKER_CAPACITY"), "maximum concurrent sandboxes, 0 = unlimited (env OF_WORKER_CAPACITY)")
	f.StringArrayVar(&labels, "label", envList("OF_WORKER_LABELS"), "worker label key=value, repeatable (env OF_WORKER_LABELS, comma-separated)")
	f.BoolVar(&oneShot, "one-shot", envBool("OF_WORKER_ONE_SHOT"), "take exactly one sandbox and exit when it exited; the exit code is 0 only for the reason completed (env OF_WORKER_ONE_SHOT)")
	f.StringVar(&metricsAddr, "metrics-addr", os.Getenv("OF_METRICS_ADDR"), "serve worker Prometheus metrics on this address, e.g. :9090 (env OF_METRICS_ADDR; default off)")
	f.StringVar(&keyFile, "key-file", os.Getenv("OF_WORKER_KEY_FILE"), "Ed25519 private key file of an enrolled worker, for the challenge of the control plane (env OF_WORKER_KEY_FILE)")
	cmd.AddCommand(newWorkerListCmd(g))
	cmd.AddCommand(newWorkerHostCmds(g)...)
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
