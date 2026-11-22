package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/machine/agent"
	"github.com/openfactory-org/openfactory/internal/machine/cache"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/hostcheck"
	"github.com/openfactory-org/openfactory/internal/machine/identity"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
)

// newWorkerHostCmds returns the commands of a microVM worker host.
func newWorkerHostCmds(g *globals) []*cobra.Command {
	return []*cobra.Command{
		newWorkerEnrollCmd(), newWorkerRunCmd(), newWorkerStatusCmd(g), newWorkerDrainCmd(g),
		newWorkerDoctorCmd(g), newWorkerDNSRelayCmd(), newWorkerGitCredentialCmd(),
	}
}

func configFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "config", envOr("OF_WORKER_CONFIG", config.DefaultPath), "worker configuration file (env OF_WORKER_CONFIG)")
}

func newWorkerEnrollCmd() *cobra.Command {
	var cfgPath, url, token, name string
	var labels []string
	var slots int
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll this Linux host as a microVM worker",
		Long: `Enroll this host with the control plane. The command checks /dev/kvm, makes an
Ed25519 key pair, sends the public key and the facts of the host (architecture,
kernel, CPUs, memory, disk, Firecracker version, runtime version, KVM) with the
single-use enrollment token, and stores the worker ID. The private key stays
on the host. It writes control_url and name to the configuration file.

An admin creates the token with POST /api/v1/workers/enrollments. A host
without a usable /dev/kvm cannot enroll.

Example:
  sudo ` + RuntimeName + ` worker enroll --url https://factory.example.com --token ofe_... --name build-01`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if url == "" || token == "" {
				return errors.New("--url and --token are required")
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if name != "" {
				cfg.Name = name
			}
			if cfg.Name == "" {
				h, _ := os.Hostname()
				cfg.Name = sanitizeID(h)
			}
			if cmd.Flags().Changed("slots") {
				cfg.Slots = slots
			}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok || k == "" {
					return fmt.Errorf("invalid --label %q (want key=value)", l)
				}
				if cfg.Labels == nil {
					cfg.Labels = map[string]string{}
				}
				cfg.Labels[k] = v
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			host := hostcheck.Host()
			if err := host.KVM(); err != nil {
				return fmt.Errorf("%w; a worker needs KVM (run worker doctor)", err)
			}
			key, err := identity.NewKey(cfg.IdentityDir())
			if err != nil {
				return err
			}
			facts := hostcheck.Facts(cmd.Context(), host, cfg, version)
			pub, _ := key.Public().(ed25519.PublicKey)
			out, err := identity.Enroll(cmd.Context(), nil, url, identity.EnrollRequest{
				EnrollmentToken: token, Name: cfg.Name, PublicKey: identity.PublicKeyString(pub),
				Facts: identity.Facts{WorkerFacts: facts, DiskGiB: facts.DiskTotalBytes >> 30}, Labels: cfg.Labels,
				Slots: cfg.Slots, SlotSize: identity.SlotSize{VCPUs: cfg.Slot.VCPUs, MemoryMiB: cfg.Slot.MemoryMiB, ScratchGiB: cfg.Slot.ScratchGiB},
			})
			if err != nil {
				return err
			}
			if err := identity.Save(cfg.IdentityDir(), identity.Identity{WorkerID: out.WorkerID, ControlURL: out.ControlURL, EnrolledAt: time.Now().UTC()}); err != nil {
				return err
			}
			cfg.ControlURL = out.ControlURL
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enrolled as worker %s (%s)\nnext: sudo systemctl enable --now openfactory-worker\n", out.WorkerID, cfg.Name)
			return nil
		},
	}
	configFlag(cmd, &cfgPath)
	cmd.Flags().StringVar(&url, "url", "", "base URL of the control plane, for example https://factory.example.com")
	cmd.Flags().StringVar(&token, "token", "", "single-use enrollment token (prefix ofe_)")
	cmd.Flags().StringVar(&name, "name", "", "worker name (default: the configured name, or the host name)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "worker label key=value, repeatable")
	cmd.Flags().IntVar(&slots, "slots", 0, "sessions that run at once (default: the configured slots)")
	return cmd
}

func newWorkerRunCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the microVM worker agent (the openfactory-worker service)",
		Long: `Run the worker agent: connect to the control plane, take sessions and run each
one in a Firecracker microVM. The openfactory-worker systemd unit runs this
command. On SIGTERM the agent disconnects and exits; running VMs keep running
and the next start adopts them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			return agent.Run(cmd.Context(), agent.Options{ConfigPath: cfgPath, Version: version, Exe: exe, Logger: log})
		},
	}
	configFlag(cmd, &cfgPath)
	return cmd
}

func printStatus(w io.Writer, st agent.Status, asJSON bool) error {
	if asJSON {
		return emit(w, st)
	}
	state := "stopped"
	switch {
	case st.Running && st.Draining:
		state = "draining"
	case st.Running:
		state = "running"
	}
	fmt.Fprintf(w, "worker:     %s (%s)\ncontrol:    %s\nagent:      %s\nslots:      %d of %d in use\n", st.WorkerID, st.Name, st.ControlURL, state, st.SlotsUsed, st.Slots)
	if st.LastWelcome != nil {
		fmt.Fprintf(w, "connected:  %s\n", st.LastWelcome.Format(time.RFC3339))
	}
	if len(st.Tasks) > 0 {
		rows := make([][]string, 0, len(st.Tasks))
		for _, t := range st.Tasks {
			rows = append(rows, []string{t.TaskID, string(t.State), string(t.Reason), t.SandboxID})
		}
		fmt.Fprintln(w)
		table(w, []string{"SESSION", "STATE", "REASON", "VM"}, rows)
	}
	return nil
}

func newWorkerStatusCmd(g *globals) *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the state of this worker host and its sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			st, err := agent.NewClient(cfg.ControlSocket()).Status(ctx)
			if err != nil {
				st, err = agent.OfflineStatus(cfg)
				if err != nil {
					return err
				}
			}
			return printStatus(cmd.OutOrStdout(), st, g.json())
		},
	}
	configFlag(cmd, &cfgPath)
	return cmd
}

func newWorkerDrainCmd(g *globals) *cobra.Command {
	var cfgPath string
	var resume bool
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Finish the running sessions of this host and take no new ones",
		Long: `Tell the running agent to refuse new sessions. Running sessions finish. --resume
takes new sessions again. The drain lasts until the agent restarts; use
POST /api/v1/workers/{id}/drain for a drain that the control plane keeps.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			st, err := agent.NewClient(cfg.ControlSocket()).Drain(ctx, !resume)
			if err != nil {
				return err
			}
			return printStatus(cmd.OutOrStdout(), st, g.json())
		},
	}
	configFlag(cmd, &cfgPath)
	cmd.Flags().BoolVar(&resume, "resume", false, "take new sessions again")
	return cmd
}

func newWorkerDoctorCmd(g *globals) *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this host can run microVM workers",
		Long: `Check the host: /dev/kvm, CPU architecture, cgroup v2, the host kernel, nft,
ip, mkfs.ext4 and git, Firecracker and the jailer, the guest kernel, the VM
subnet, the jail uids, firewall tools (Docker, ufw, firewalld) whose rules would
drop VM traffic, SMT, KSM, swap and the memory, vCPU and disk budget of the
slots. Each problem comes with a fix. The exit code is 1 when a check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			r := hostcheck.Doctor(cmd.Context(), hostcheck.Host(), cfg)
			out := cmd.OutOrStdout()
			if g.json() {
				if err := emit(out, r); err != nil {
					return err
				}
			} else {
				for _, c := range r.Checks {
					line := fmt.Sprintf("%-5s %s", strings.ToUpper(string(c.Status)), c.Name)
					if c.Detail != "" {
						line += ": " + c.Detail
					}
					fmt.Fprintln(out, line)
					if c.Fix != "" && c.Status != hostcheck.OK {
						fmt.Fprintln(out, "      fix: "+c.Fix)
					}
				}
			}
			if r.Failed() {
				return errSilent
			}
			return nil
		},
	}
	configFlag(cmd, &cfgPath)
	return cmd
}

func newWorkerDNSRelayCmd() *cobra.Command {
	var listen string
	var upstreams []string
	cmd := &cobra.Command{
		Use:    "dns-relay",
		Short:  "Relay DNS of one unrestricted microVM to the host resolvers (started by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ups []netip.Addr
			for _, u := range upstreams {
				a, err := netip.ParseAddr(u)
				if err != nil {
					return fmt.Errorf("--upstream %q: %w", u, err)
				}
				ups = append(ups, a)
			}
			r := &vmnet.DNSRelay{Listen: listen, Upstreams: ups}
			err := r.Run(cmd.Context())
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to listen on (UDP and TCP)")
	cmd.Flags().StringArrayVar(&upstreams, "upstream", nil, "resolver address, repeatable")
	return cmd
}

func newWorkerGitCredentialCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-credential <get|store|erase>",
		Short:  "Git credential helper of the worker git mirror cache (run by git)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cache.HelperMain(cmd.Context(), args[0], cmd.InOrStdin(), cmd.OutOrStdout(), os.Getenv)
		},
	}
}
