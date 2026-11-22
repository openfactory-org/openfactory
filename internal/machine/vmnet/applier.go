package vmnet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Runner runs a command with stdin and returns its combined output.
type Runner func(ctx context.Context, stdin string, name string, args ...string) (string, error)

// ExecRunner runs the command with os/exec, without a shell.
func ExecRunner(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed tool names; arguments are built by the agent
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// HostApplier is the real Applier: nft -f - for rules, ip for taps and
// /proc/sys for sysctls.
type HostApplier struct {
	Run Runner
	// ProcSys is the sysctl root, /proc/sys by default.
	ProcSys string
	// MTU of the taps, 1500 by default.
	MTU int
}

func (h *HostApplier) run(ctx context.Context, stdin, name string, args ...string) error {
	r := h.Run
	if r == nil {
		r = ExecRunner
	}
	_, err := r(ctx, stdin, name, args...)
	return err
}

// ApplyBase applies the base ruleset in one nft transaction.
func (h *HostApplier) ApplyBase(ctx context.Context, rules string) error {
	return h.run(ctx, rules, "nft", "-f", "-")
}

// AddVM creates the tap of vm, sets its sysctls and applies its rules.
func (h *HostApplier) AddVM(ctx context.Context, vm VMNet, rules string) error {
	owner := strconv.Itoa(vm.Owner)
	mtu := h.MTU
	if mtu == 0 {
		mtu = 1500
	}
	_ = h.run(ctx, "", "ip", "link", "del", vm.Tap) // a stale tap of a crashed agent
	steps := [][]string{
		{"ip", "tuntap", "add", "dev", vm.Tap, "mode", "tap", "user", owner, "group", owner},
		{"ip", "addr", "add", vm.HostIP.String() + "/30", "dev", vm.Tap},
		{"ip", "link", "set", vm.Tap, "up", "mtu", strconv.Itoa(mtu)},
	}
	for _, s := range steps {
		if err := h.run(ctx, "", s[0], s[1:]...); err != nil {
			_ = h.run(ctx, "", "ip", "link", "del", vm.Tap)
			return err
		}
	}
	root := h.ProcSys
	if root == "" {
		root = "/proc/sys"
	}
	for k, v := range map[string]string{
		"net/ipv4/conf/" + vm.Tap + "/rp_filter":    "1",
		"net/ipv4/conf/" + vm.Tap + "/proxy_arp":    "0",
		"net/ipv6/conf/" + vm.Tap + "/disable_ipv6": "1",
	} {
		// IPv6 may be disabled on the host; the ipv6 sysctl is then absent.
		if err := os.WriteFile(filepath.Join(root, k), []byte(v), 0o644); err != nil && !strings.Contains(k, "ipv6") { //nolint:gosec // G306: sysctl files
			_ = h.run(ctx, "", "ip", "link", "del", vm.Tap)
			return fmt.Errorf("sysctl %s: %w", k, err)
		}
	}
	if err := h.run(ctx, rules, "nft", "-f", "-"); err != nil {
		_ = h.run(ctx, "", "ip", "link", "del", vm.Tap)
		return err
	}
	return nil
}

// RemoveVM removes the rules and the tap of vm. It is safe to call for a VM
// that has no rules or no tap.
func (h *HostApplier) RemoveVM(ctx context.Context, vm VMNet, rules string) error {
	var first error
	if err := h.run(ctx, rules, "nft", "-f", "-"); err != nil && !strings.Contains(err.Error(), "No such file") {
		first = err
	}
	if err := h.run(ctx, "", "ip", "link", "del", vm.Tap); err != nil && first == nil && !strings.Contains(err.Error(), "Cannot find device") {
		first = err
	}
	return first
}

// RecordingApplier records the scripts it gets. Tests use it.
type RecordingApplier struct {
	mu      sync.Mutex
	Base    []string
	Added   map[string]string
	Removed map[string]string
	Err     error
}

// ApplyBase records rules.
func (r *RecordingApplier) ApplyBase(_ context.Context, rules string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Base = append(r.Base, rules)
	return r.Err
}

// AddVM records the rules of vm.
func (r *RecordingApplier) AddVM(_ context.Context, vm VMNet, rules string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	if r.Added == nil {
		r.Added = map[string]string{}
	}
	r.Added[vm.Tap] = rules
	return nil
}

// RemoveVM records the removal of vm.
func (r *RecordingApplier) RemoveVM(_ context.Context, vm VMNet, rules string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Removed == nil {
		r.Removed = map[string]string{}
	}
	r.Removed[vm.Tap] = rules
	delete(r.Added, vm.Tap)
	return nil
}

// Snapshot returns copies of the added and removed scripts by tap.
func (r *RecordingApplier) Snapshot() (added, removed map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	added, removed = map[string]string{}, map[string]string{}
	for k, v := range r.Added {
		added[k] = v
	}
	for k, v := range r.Removed {
		removed[k] = v
	}
	return added, removed
}
