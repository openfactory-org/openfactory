package netlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// ExitCode is the exit code of the runner when the lock is not proven. The
// provider maps it to the failure network_policy_timeout.
const ExitCode = 75

// ErrNotProven wraps every failure of Setup. The runner exits with ExitCode.
var ErrNotProven = errors.New("netlock: egress lock not proven")

// Credential is a user that the runner starts processes as.
type Credential struct {
	UID, GID int
	Home     string
}

// AgentCredential is the credential of the agent user.
func AgentCredential() Credential {
	return Credential{UID: AgentUID, GID: AgentUID, Home: AgentHome}
}

// Params are the inputs of Setup.
type Params struct {
	// Mode is the lock mode. ModeNone makes Setup return a nil lock.
	Mode Mode
	// Policy is the network policy of the run, from the session start.
	Policy rtcore.EffectiveNetworkPolicy
	// ControlURL is the URL of the control plane. Its host is always allowed.
	ControlURL string
	// AlwaysAllow are more hosts that are always reachable (the run data
	// store, for example). A leading "." matches subdomains.
	AlwaysAllow []string
	// PrivateAllow are hosts that may resolve to a private address (a control
	// plane on a private network).
	PrivateAllow []string
	// LoopbackDaemonPorts are the loopback ports of provider daemons.
	LoopbackDaemonPorts []int
	// Workspace is the workspace directory. It is given to the agent.
	Workspace string
	// AgentDirs are more directories that are given to the agent.
	AgentDirs []string
	// Executable is the runtime binary; it runs netguard and the probe.
	Executable string
	Logger     *slog.Logger
}

// Locker sets up the lock. The zero value uses the real system; tests
// replace the parts.
type Locker struct {
	Exec     Executor
	Users    *Users
	Firewall *Firewall
	// StartProxy starts netguard as the proxy user and returns a function
	// that stops it. Default: Executable netguard ...
	StartProxy func(ctx context.Context, p Params, proxyUID int) (stop func(), err error)
	// Probe runs the probe as the agent. Default: Executable netlock probe.
	Probe func(ctx context.Context, p Params, spec ProbeSpec) (ProbeResult, error)
	// Chown gives a directory tree to the agent. Default ChownTree.
	Chown func(dir string, uid, gid int) error
	// Agent replaces AgentCredential, for tests that cannot change the user.
	Agent *Credential
}

// Lock is an applied lock.
type Lock struct {
	// Agent is the credential that every user-controlled process runs as.
	Agent Credential
	// ProxyURL is the URL of netguard.
	ProxyURL string
	// ReportURL is the URL of the report of blocked hosts.
	ReportURL string
	stop      func()
}

// Env returns the proxy variables of the agent processes.
func (l *Lock) Env() map[string]string {
	if l == nil {
		return nil
	}
	return Env(l.ProxyURL)
}

// Close stops netguard. The rules stay until the sandbox ends.
func (l *Lock) Close() {
	if l != nil && l.stop != nil {
		l.stop()
		l.stop = nil
	}
}

// Setup applies the lock of p.Mode. For ModeNone it returns nil, nil. Every
// error wraps ErrNotProven: the caller must not run agent code.
func (lk *Locker) Setup(ctx context.Context, p Params) (*Lock, error) {
	switch p.Mode {
	case "", ModeNone:
		return nil, nil
	case ModeIPTables:
	case ModeNetns:
		return nil, fmt.Errorf("%w: mode %q is not implemented", ErrNotProven, p.Mode)
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrNotProven, p.Mode)
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	ex := lk.Exec
	if ex == nil {
		ex = OSExecutor{}
	}
	agent := AgentCredential()
	if lk.Agent != nil {
		agent = *lk.Agent
	}
	proxyUID := ProxyUserAvoiding(agent.UID)
	users := lk.Users
	if users == nil {
		users = &Users{Exec: ex}
	}
	if err := users.Ensure(ctx, agent.UID, proxyUID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotProven, err)
	}
	chown := lk.Chown
	if chown == nil {
		chown = ChownTree
	}
	for _, d := range append([]string{p.Workspace}, p.AgentDirs...) {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("%w: create %s: %w", ErrNotProven, d, err)
		}
		if err := chown(d, agent.UID, agent.GID); err != nil {
			return nil, fmt.Errorf("%w: give %s to the agent: %w", ErrNotProven, d, err)
		}
	}
	start := lk.StartProxy
	if start == nil {
		start = startNetguard
	}
	stop, err := start(ctx, p, proxyUID)
	if err != nil {
		return nil, fmt.Errorf("%w: start netguard: %w", ErrNotProven, err)
	}
	l := &Lock{Agent: agent, ProxyURL: ProxyURL(), ReportURL: ReportURL(), stop: stop}
	fw := lk.Firewall
	if fw == nil {
		fw = &Firewall{Exec: ex}
	}
	rp := RuleParams{AgentUID: agent.UID, ProxyUID: proxyUID, LoopbackDaemonPorts: p.LoopbackDaemonPorts}
	if err := fw.Apply(ctx, rp); err != nil {
		l.Close()
		return nil, fmt.Errorf("%w: %w", ErrNotProven, err)
	}
	probe := lk.Probe
	if probe == nil {
		probe = probeAsAgent
	}
	res, err := probe(ctx, p, ProbeSpecFor(p))
	if err == nil {
		err = res.Err()
	}
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("%w: %w", ErrNotProven, err)
	}
	p.Logger.Info("egress lock applied", "mode", p.Mode, "checks", len(res.Checks), "daemon_ports", len(p.LoopbackDaemonPorts))
	return l, nil
}

// controlHost returns host:port of a URL, with the default port of its scheme.
func controlHost(raw string) (host, hostport string) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return u.Hostname(), net.JoinHostPort(u.Hostname(), port)
}

// ProbeSpecFor returns the probe of a lock.
func ProbeSpecFor(p Params) ProbeSpec {
	spec := ProbeSpec{
		ProxyURL:      ProxyURL(),
		DirectTargets: append([]string{}, DefaultDirectTargets...),
		DaemonPorts:   p.LoopbackDaemonPorts,
		Restricted:    p.Policy.Restricted,
	}
	if host, hp := controlHost(p.ControlURL); host != "" {
		// The agent reaches the control plane only through the proxy.
		spec.DirectTargets = append(spec.DirectTargets, hp)
		spec.ControlHost = hp
	}
	return spec
}

// NetguardArgs returns the arguments of the netguard process of a lock.
func NetguardArgs(p Params) []string {
	args := []string{"netguard",
		"--listen", "127.0.0.1:" + strconv.Itoa(ProxyPort),
		"--report-listen", "127.0.0.1:" + strconv.Itoa(ReportPort),
	}
	if !p.Policy.Restricted {
		args = append(args, "--unrestricted")
	}
	for _, h := range p.Policy.AllowedHosts {
		args = append(args, "--allow", h)
	}
	for _, h := range p.Policy.DeniedHosts {
		args = append(args, "--deny", h)
	}
	always := append([]string{}, p.AlwaysAllow...)
	private := append([]string{}, p.PrivateAllow...)
	if host, _ := controlHost(p.ControlURL); host != "" {
		always = append(always, host)
		// The control plane is a platform host: it may be on a private network.
		private = append(private, host)
	}
	for _, h := range always {
		if h != "" {
			args = append(args, "--always-allow", h)
		}
	}
	for _, h := range private {
		if h != "" {
			args = append(args, "--allow-private-host", h)
		}
	}
	return args
}

func startNetguard(ctx context.Context, p Params, proxyUID int) (func(), error) {
	if p.Executable == "" {
		return nil, errors.New("no runtime executable")
	}
	// netguard outlives the setup: it must not stop with ctx.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), p.Executable, NetguardArgs(p)...) //nolint:gosec // G204: our own binary
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	Credential{UID: proxyUID, GID: proxyUID}.Apply(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop := func() {
		_ = cmd.Process.Kill()
		<-done
	}
	addr := "127.0.0.1:" + strconv.Itoa(ProxyPort)
	// Stop waiting when netguard exits before it listens.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-wctx.Done():
		}
	}()
	err := rtcore.WaitDial(wctx, "tcp", addr, 10*time.Second)
	if err == nil {
		return stop, nil
	}
	select {
	case <-done:
		return nil, errors.New("netguard exited at start")
	default:
	}
	stop()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("netguard does not listen on %s: %w", addr, err)
}

func probeAsAgent(ctx context.Context, p Params, spec ProbeSpec) (ProbeResult, error) {
	var res ProbeResult
	if p.Executable == "" {
		return res, errors.New("no runtime executable")
	}
	in, err := json.Marshal(spec)
	if err != nil {
		return res, err
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(pctx, p.Executable, "netlock", "probe") //nolint:gosec // G204: our own binary
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + AgentHome}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	AgentCredential().Apply(cmd)
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("run the probe as %s: %w: %s", AgentUser, err, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return res, fmt.Errorf("decode the probe result: %w", err)
	}
	return res, nil
}
