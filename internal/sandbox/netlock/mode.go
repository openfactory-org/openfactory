package netlock

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Mode is how the runner locks the network of the agent.
type Mode string

const (
	// ModeNone applies no lock. The provider enforces the policy, or nothing does.
	ModeNone Mode = "none"
	// ModeIPTables applies iptables and ip6tables owner rules.
	ModeIPTables Mode = "iptables"
	// ModeNetns runs the agent in a private network namespace. Reserved: not
	// implemented yet.
	ModeNetns Mode = "netns"
)

// Environment variables that the provider sets for the runner.
const (
	// EnvMode selects the Mode. Empty means ModeNone.
	EnvMode = "OF_EGRESS_LOCK"
	// EnvLoopbackDaemonPorts lists the loopback TCP ports of provider daemons
	// that the agent must not reach, separated by commas.
	EnvLoopbackDaemonPorts = "OF_LOOPBACK_DAEMON_PORTS"
)

// ParseMode parses the value of OF_EGRESS_LOCK. An empty value is ModeNone.
func ParseMode(v string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(v))); m {
	case "":
		return ModeNone, nil
	case ModeNone, ModeIPTables, ModeNetns:
		return m, nil
	}
	return "", fmt.Errorf("%w: %s %q: want %q, %q or %q", rtcore.ErrInvalid, EnvMode, v, ModeNone, ModeIPTables, ModeNetns)
}

// ModeFromEnv reads OF_EGRESS_LOCK.
func ModeFromEnv() (Mode, error) { return ParseMode(os.Getenv(EnvMode)) }

// Enabled reports whether the mode locks the network.
func (m Mode) Enabled() bool { return m != "" && m != ModeNone }

// ParsePorts parses a comma-separated list of TCP ports.
func ParsePorts(v string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: port %q", rtcore.ErrInvalid, f)
		}
		out = append(out, n)
	}
	return out, nil
}

// PortsFromEnv reads OF_LOOPBACK_DAEMON_PORTS.
func PortsFromEnv() ([]int, error) { return ParsePorts(os.Getenv(EnvLoopbackDaemonPorts)) }
