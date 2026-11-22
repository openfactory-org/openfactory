package main

import (
	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/config"
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
