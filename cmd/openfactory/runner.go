package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/harness/claude"
	"github.com/openfactory-org/openfactory/internal/harness/codex"
	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
)

const envExecCommand = "OF_RUNNER_EXEC_COMMAND"

func newRunnerCmd() *cobra.Command {
	var workdir, stateDir string
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Run one session of a run inside a sandbox",
		Long: `Run the runner of one session. It reads OF_CONTROL_URL, OF_RUN_ID,
OF_SESSION and OF_BOOTSTRAP_TOKEN from the environment, prepares the
workspace and talks to the control plane. The first SIGTERM or SIGINT stops the
session in a graceful way. A second signal exits at once.

The harnesses "claude" and "codex" run the matching CLI inside the sandbox.
The harness "open" runs its loop in this process. It calls the model through
the control plane and holds no provider key.
The harness "exec" is available if OF_RUNNER_EXEC_COMMAND is set. The runner
runs its value with "sh -c" for each turn.

OF_EGRESS_LOCK=iptables locks the network of the agent: the runner must run as
root. It creates the users agent and ofproxy, starts netguard, applies
iptables and ip6tables rules, proves them with a probe and runs every
user-controlled process as agent. OF_LOOPBACK_DAEMON_PORTS lists loopback
ports of provider daemons that the agent must not reach. When the lock cannot
be proven, the runner exits with code 75. When the control plane serves
another runner version, it exits with code 65.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := runnerConfig(workdir, stateDir)
			if err != nil {
				return err
			}
			if cfg.GitCredentialHelper, err = gitCredentialHelper(stateDir); err != nil {
				return err
			}
			if cfg.CloudTokenCommand, err = cloudTokenCommand(stateDir); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			sigs := make(chan os.Signal, 2)
			signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
			defer signal.Stop(sigs)
			go func() {
				<-sigs
				cancel()
				<-sigs
				os.Exit(1)
			}()
			return runnerExit(runner.Run(ctx, cfg))
		},
	}
	cmd.Flags().StringVar(&workdir, "workdir", envOr("OF_WORKSPACE", "./workspace"), "workspace directory (env OF_WORKSPACE)")
	cmd.PersistentFlags().StringVar(&stateDir, "state-dir", envOr("OF_RUNNER_STATE_DIR", "./.openfactory-runner"), "directory for the event log and the session file (env OF_RUNNER_STATE_DIR)")
	cmd.AddCommand(newGitCredentialCmd(&stateDir))
	cmd.AddCommand(newCloudTokenCmd(&stateDir))
	return cmd
}

// runnerExit turns the errors that have their own exit code into an exit.
func runnerExit(err error) error {
	switch {
	case errors.Is(err, netlock.ErrNotProven):
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(netlock.ExitCode)
	case errors.Is(err, runner.ErrRunnerVersionMismatch):
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(runner.ExitVersionMismatch)
	}
	return err
}

// newGitCredentialCmd makes the git credential helper command. Git calls it
// with the operation as the last argument.
func newGitCredentialCmd(stateDir *string) *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:    "git-credential <get|store|erase>",
		Short:  "Git credential helper for the runner",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if endpoint != "" {
				return runner.GitCredentialVia(cmd.Context(), endpoint, args[0], os.Stdin, os.Stdout)
			}
			return runner.GitCredential(cmd.Context(), *stateDir, args[0], os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "loopback URL of the helper endpoint of the runner (with the egress lock)")
	return cmd
}

// newCloudTokenCmd makes the cloud token helper command. It prints only the
// token, for SDKs that read a web identity token from a command.
func newCloudTokenCmd(stateDir *string) *cobra.Command {
	var audience, format, endpoint string
	var ttl int
	cmd := &cobra.Command{
		Use:    "cloud-token --audience <audience> [--ttl <seconds>]",
		Short:  "Print a cloud identity token for the run",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if endpoint != "" {
				return runner.CloudTokenVia(cmd.Context(), endpoint, audience, ttl, format, os.Stdout)
			}
			return runner.CloudTokenAs(cmd.Context(), *stateDir, audience, ttl, format, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "loopback URL of the helper endpoint of the runner (with the egress lock)")
	cmd.Flags().StringVar(&audience, "audience", "", "audience of the token")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "life of the token in seconds (0 means the default)")
	cmd.Flags().StringVar(&format, "format", runner.CloudTokenFormatToken, "output: token (the token only) or gcp (an executable-source response)")
	_ = cmd.MarkFlagRequired("audience")
	return cmd
}

// gitCredentialHelper returns the value of git's credential.helper: this
// program with the state directory, run by a shell.
func gitCredentialHelper(stateDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the runner program: %w", err)
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	return "!" + shellQuote(exe) + " runner git-credential --state-dir " + shellQuote(abs), nil
}

// cloudTokenCommand returns the shell command of the cloud token helper,
// without --audience.
func cloudTokenCommand(stateDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the runner program: %w", err)
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	return shellQuote(exe) + " runner cloud-token --state-dir " + shellQuote(abs), nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runnerConfig reads the task variables and makes the runner configuration.
func runnerConfig(workdir, stateDir string) (runner.Config, error) {
	cfg := runner.Config{
		ControlURL:     os.Getenv(rtcore.TaskEnvControlURL),
		RunID:          os.Getenv(rtcore.TaskEnvRunID),
		BootstrapToken: os.Getenv(rtcore.TaskEnvBootstrapToken),
		WorkDir:        workdir,
		StateDir:       stateDir,
		Version:        version,
		// OF_SNAPSHOT_DECLARATIONS names the file with extra snapshot paths.
		SnapshotDeclarations: os.Getenv("OF_SNAPSHOT_DECLARATIONS"),
		// The proxy serves the blocked hosts here.
		EgressReportURL: os.Getenv(netlock.ReportEnv),
		Harnesses: map[string]runner.Harness{
			string(rtcore.HarnessClaude): &runner.CoreHarness{H: claude.New(claude.Options{Logger: slog.Default()})},
			string(rtcore.HarnessCodex):  &runner.CoreHarness{H: codex.New(codex.Options{Logger: slog.Default()})},
		},
	}
	for name, v := range map[string]string{
		rtcore.TaskEnvControlURL:     cfg.ControlURL,
		rtcore.TaskEnvRunID:          cfg.RunID,
		rtcore.TaskEnvBootstrapToken: cfg.BootstrapToken,
		rtcore.TaskEnvSession:        os.Getenv(rtcore.TaskEnvSession),
	} {
		if v == "" {
			return cfg, fmt.Errorf("missing required environment variable %s", name)
		}
	}
	n, err := strconv.Atoi(os.Getenv(rtcore.TaskEnvSession))
	if err != nil {
		return cfg, errors.New(rtcore.TaskEnvSession + " is not a number")
	}
	cfg.Session = n
	if cfg.EgressLock, err = netlock.ModeFromEnv(); err != nil {
		return cfg, err
	}
	if cfg.LoopbackDaemonPorts, err = netlock.PortsFromEnv(); err != nil {
		return cfg, err
	}
	if exe, err := os.Executable(); err == nil {
		cfg.Executable = exe
	} else if cfg.EgressLock.Enabled() {
		return cfg, fmt.Errorf("find the runner program: %w", err)
	}
	if c := os.Getenv(envExecCommand); c != "" {
		cfg.Harnesses["exec"] = runner.ExecHarness{Command: []string{"sh", "-c", c}}
	}
	return cfg, nil
}
