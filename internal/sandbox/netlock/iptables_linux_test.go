//go:build linux && netlock_iptables

package netlock

// Run as root in a throwaway Linux machine or container with NET_ADMIN:
//
//	go test -tags netlock_iptables -run TestRealIPTables ./internal/sandbox/netlock/
//
// It changes the OUTPUT chain of the network namespace and removes its chain
// at the end.

import (
	"context"
	"os"
	"testing"
)

func TestRealIPTables(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	ex := OSExecutor{}
	fw := &Firewall{Exec: ex}
	p := RuleParams{LoopbackDaemonPorts: []int{8911}}
	t.Cleanup(func() {
		for _, prog := range []string{"iptables", "ip6tables"} {
			_, _ = ex.Run(ctx, prog, "-w", "5", "-D", "OUTPUT", "-j", Chain)
			_, _ = ex.Run(ctx, prog, "-w", "5", "-F", Chain)
			_, _ = ex.Run(ctx, prog, "-w", "5", "-X", Chain)
		}
	})
	if err := fw.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	// A second apply is idempotent: one jump, the same rules.
	if err := fw.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := fw.Verify(ctx, p); err != nil {
		t.Fatal(err)
	}
}
