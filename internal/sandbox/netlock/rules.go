package netlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// Chain is the name of the iptables chain of the lock.
	Chain = "OF_LOCK"
	// AgentUser is the user of the harness and of every process that the
	// agent controls.
	AgentUser = "agent"
	// AgentUID is the uid (and gid) of AgentUser.
	AgentUID = 1001
	// ProxyUser is the user of netguard.
	ProxyUser = "ofproxy"
	// DefaultProxyUID is the uid of ProxyUser: the "nobody" user of many
	// distroless images.
	DefaultProxyUID = 65532
	// AgentHome is the home directory of AgentUser.
	AgentHome = "/home/agent"
)

// ProxyUserAvoiding returns the first uid from DefaultProxyUID up that is
// none of the given uids: the users that agent code can run as. The rules
// tell the proxy from the agent by uid, so the two must differ.
func ProxyUserAvoiding(uids ...int) int {
	used := map[int]bool{}
	for _, u := range uids {
		used[u] = true
	}
	u := DefaultProxyUID
	for used[u] {
		u++
	}
	return u
}

// Command is one command of the lock. When it fails and Fallback is set,
// Fallback runs instead; the step fails only when Fallback fails too.
type Command struct {
	Args     []string
	Fallback []string
}

// String renders the command as a shell line.
func (c Command) String() string {
	s := strings.Join(c.Args, " ")
	if len(c.Fallback) > 0 {
		s += " 2>/dev/null || " + strings.Join(c.Fallback, " ")
	}
	return s
}

// Executor runs a program and returns its combined output.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// OSExecutor runs programs with os/exec.
type OSExecutor struct{}

// Run runs name with args.
func (OSExecutor) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RuleParams are the inputs of the rules.
type RuleParams struct {
	// AgentUID is the uid of the agent. Default AgentUID.
	AgentUID int
	// ProxyUID is the uid of netguard. Default DefaultProxyUID.
	ProxyUID int
	// LoopbackDaemonPorts are the loopback ports of provider daemons that the
	// agent must not reach.
	LoopbackDaemonPorts []int
}

func (p *RuleParams) defaults() {
	if p.AgentUID == 0 {
		p.AgentUID = AgentUID
	}
	if p.ProxyUID == 0 {
		p.ProxyUID = DefaultProxyUID
	}
}

// chainRules returns the rules of the chain, without the program name, in
// order. The same rules go in both families: loopback is "-o lo".
func chainRules(p RuleParams) [][]string {
	p.defaults()
	agent := strconv.Itoa(p.AgentUID)
	proxy := strconv.Itoa(p.ProxyUID)
	rules := [][]string{
		// netguard may go out.
		{"-A", Chain, "-m", "owner", "--uid-owner", proxy, "-j", "RETURN"},
	}
	// The daemons of the provider on loopback are closed to the agent.
	for _, port := range p.LoopbackDaemonPorts {
		ps := strconv.Itoa(port)
		rules = append(rules,
			[]string{"-A", Chain, "-o", "lo", "-m", "owner", "--uid-owner", agent, "-p", "tcp", "--dport", ps, "-j", "REJECT", "--reject-with", "tcp-reset"},
			[]string{"-A", Chain, "-o", "lo", "-m", "owner", "--uid-owner", agent, "-p", "udp", "--dport", ps, "-j", "REJECT"},
		)
	}
	rules = append(rules,
		// Loopback stays open to the agent: its own servers, netguard, the MCP forwarder.
		[]string{"-A", Chain, "-o", "lo", "-m", "owner", "--uid-owner", agent, "-j", "RETURN"},
		// Everything else of the agent is rejected.
		[]string{"-A", Chain, "-m", "owner", "--uid-owner", agent, "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		[]string{"-A", Chain, "-m", "owner", "--uid-owner", agent, "-j", "REJECT"},
	)
	return rules
}

// Commands returns the commands of the lock for one program (iptables or
// ip6tables).
func Commands(program string, p RuleParams) []Command {
	w := []string{program, "-w", "5"}
	cmds := []Command{{
		Args:     append(append([]string{}, w...), "-N", Chain),
		Fallback: append(append([]string{}, w...), "-F", Chain),
	}}
	for _, r := range chainRules(p) {
		cmds = append(cmds, Command{Args: append(append([]string{}, w...), r...)})
	}
	// First in OUTPUT, before the rules of the provider (ufw and others).
	cmds = append(cmds, Command{
		Args:     append(append([]string{}, w...), "-C", "OUTPUT", "-j", Chain),
		Fallback: append(append([]string{}, w...), "-I", "OUTPUT", "1", "-j", Chain),
	})
	return cmds
}

// Script renders the commands of both families as a shell script.
func Script(p RuleParams) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, prog := range []string{"iptables", "ip6tables"} {
		for _, c := range Commands(prog, p) {
			b.WriteString(c.String())
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Firewall applies and checks the rules.
type Firewall struct {
	Exec Executor
	// HasProgram reports whether a program is installed. Default exec.LookPath.
	HasProgram func(name string) bool
	// IPv6Interfaces returns the names of the interfaces with an IPv6
	// address. Default: read /proc/net/if_inet6.
	IPv6Interfaces func() ([]string, error)
}

// ErrNoIPv6Lock means that ip6tables is missing while an interface other
// than loopback has an IPv6 address: IPv6 egress would stay open.
var ErrNoIPv6Lock = errors.New("netlock: ip6tables is missing and the sandbox has IPv6")

func (f *Firewall) hasProgram(name string) bool {
	if f.HasProgram != nil {
		return f.HasProgram(name)
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func (f *Firewall) ipv6Interfaces() ([]string, error) {
	if f.IPv6Interfaces != nil {
		return f.IPv6Interfaces()
	}
	data, err := os.ReadFile("/proc/net/if_inet6")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // no IPv6 in the kernel
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if fs := strings.Fields(line); len(fs) >= 6 {
			out = append(out, fs[5])
		}
	}
	return out, nil
}

// programs returns the programs to apply the rules with.
func (f *Firewall) programs() ([]string, error) {
	if !f.hasProgram("iptables") {
		return nil, errors.New("netlock: iptables is not installed")
	}
	progs := []string{"iptables"}
	if f.hasProgram("ip6tables") {
		return append(progs, "ip6tables"), nil
	}
	ifs, err := f.ipv6Interfaces()
	if err != nil {
		return nil, fmt.Errorf("netlock: read IPv6 interfaces: %w", err)
	}
	for _, name := range ifs {
		if name != "lo" {
			return nil, ErrNoIPv6Lock
		}
	}
	return progs, nil
}

// Apply applies the rules with iptables and ip6tables, then checks them.
func (f *Firewall) Apply(ctx context.Context, p RuleParams) error {
	progs, err := f.programs()
	if err != nil {
		return err
	}
	for _, prog := range progs {
		for _, c := range Commands(prog, p) {
			if _, err := f.Exec.Run(ctx, c.Args[0], c.Args[1:]...); err != nil {
				if len(c.Fallback) == 0 {
					return fmt.Errorf("netlock: %w", err)
				}
				if _, err := f.Exec.Run(ctx, c.Fallback[0], c.Fallback[1:]...); err != nil {
					return fmt.Errorf("netlock: %w", err)
				}
			}
		}
	}
	return f.Verify(ctx, p)
}

// Verify checks that the chain holds the expected rules and that OUTPUT
// jumps to it first. A provider rule or an agent that changed the rules
// makes it fail.
func (f *Firewall) Verify(ctx context.Context, p RuleParams) error {
	progs, err := f.programs()
	if err != nil {
		return err
	}
	want := chainRules(p)
	for _, prog := range progs {
		out, err := f.Exec.Run(ctx, prog, "-w", "5", "-S", Chain)
		if err != nil {
			return fmt.Errorf("netlock: list %s rules: %w", prog, err)
		}
		got := ruleLines(out, "-A "+Chain+" ")
		if len(got) != len(want) {
			return fmt.Errorf("netlock: %s chain %s has %d rules, want %d", prog, Chain, len(got), len(want))
		}
		for i, line := range got {
			if !sameRule(line, want[i]) {
				return fmt.Errorf("netlock: %s rule %d is %q, want %q", prog, i+1, line, strings.Join(want[i], " "))
			}
		}
		out, err = f.Exec.Run(ctx, prog, "-w", "5", "-S", "OUTPUT")
		if err != nil {
			return fmt.Errorf("netlock: list %s OUTPUT: %w", prog, err)
		}
		first := ruleLines(out, "-A OUTPUT ")
		if len(first) == 0 || strings.TrimSpace(first[0]) != "-A OUTPUT -j "+Chain {
			return fmt.Errorf("netlock: %s OUTPUT does not jump to %s first", prog, Chain)
		}
	}
	return nil
}

func ruleLines(out, prefix string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, prefix) {
			lines = append(lines, l)
		}
	}
	return lines
}

// sameRule compares a line of "iptables -S" with a rule. iptables adds
// implicit matches ("-m tcp") to its output, so the check is that every
// token of the rule appears in the line, in order.
func sameRule(line string, rule []string) bool {
	toks := strings.Fields(line)
	i := 0
	for _, want := range rule {
		for i < len(toks) && toks[i] != want {
			i++
		}
		if i == len(toks) {
			return false
		}
		i++
	}
	return true
}
