package vmnet

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

func testConfig() Config {
	return Config{
		Subnet:        netip.MustParsePrefix("10.213.0.0/16"),
		OfnetUID:      899999,
		InternalAllow: []netip.AddrPort{netip.MustParseAddrPort("10.1.2.3:443")},
		Resolvers:     []netip.Addr{netip.MustParseAddr("127.0.0.53")},
		BlockSMTP:     true,
		MaxConns:      2048,
	}
}

func TestAlloc(t *testing.T) {
	vm, err := Alloc(netip.MustParsePrefix("10.213.0.0/16"), 3, 900003, Restricted)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Tap != "ofvm3" || vm.HostIP.String() != "10.213.0.13" || vm.GuestIP.String() != "10.213.0.14" || vm.GuestMAC.String() != "06:00:0a:d5:00:0e" {
		t.Fatalf("alloc = %+v", vm)
	}
	if _, err := Alloc(netip.MustParsePrefix("10.0.0.0/30"), 1, 1, Restricted); err == nil {
		t.Fatal("slot beyond the subnet")
	}
}

// The per-VM rules must equal section 10.4 of the design.
const wantRestricted = `add chain inet openfactory vm3_in
add chain inet openfactory vm3_fwd
add rule inet openfactory vm3_in ether saddr != 06:00:0a:d5:00:0e drop
add rule inet openfactory vm3_in ip saddr != 10.213.0.14 drop
add rule inet openfactory vm3_in ip daddr 10.213.0.13 tcp dport { 3128, 3129 } accept
add rule inet openfactory vm3_in drop
add rule inet openfactory vm3_fwd reject with icmpx admin-prohibited
add element inet openfactory vm_in  { "ofvm3" : jump vm3_in }
add element inet openfactory vm_fwd { "ofvm3" : jump vm3_fwd }
`

const wantUnrestricted = `add chain inet openfactory vm3_in
add chain inet openfactory vm3_fwd
add set inet openfactory vm3_conns { type ipv4_addr; flags dynamic; size 1; }
add rule inet openfactory vm3_in ether saddr != 06:00:0a:d5:00:0e drop
add rule inet openfactory vm3_in ip saddr != 10.213.0.14 drop
add rule inet openfactory vm3_in ip daddr 10.213.0.13 meta l4proto { tcp, udp } th dport 53 accept
add rule inet openfactory vm3_in drop
add rule inet openfactory vm3_fwd ip saddr != 10.213.0.14 drop
add rule inet openfactory vm3_fwd ct state new add @vm3_conns { ip saddr ct count over 2048 } reject with icmpx admin-prohibited
add rule inet openfactory vm3_fwd ip daddr . tcp dport @internal_allow4 accept
add rule inet openfactory vm3_fwd ip daddr @private4 reject with icmpx admin-prohibited
add rule inet openfactory vm3_fwd tcp dport 25 reject with tcp reset
add rule inet openfactory vm3_fwd accept
add element inet openfactory vm_in  { "ofvm3" : jump vm3_in }
add element inet openfactory vm_fwd { "ofvm3" : jump vm3_fwd }
`

const wantRemoveUnrestricted = `delete element inet openfactory vm_in  { "ofvm3" }
delete element inet openfactory vm_fwd { "ofvm3" }
flush chain inet openfactory vm3_in
flush chain inet openfactory vm3_fwd
delete chain inet openfactory vm3_in
delete chain inet openfactory vm3_fwd
delete set inet openfactory vm3_conns
`

func TestRenderVM(t *testing.T) {
	cfg := testConfig()
	vm, _ := Alloc(cfg.Subnet, 3, 900003, Restricted)
	add, rm, err := RenderVM(cfg, vm)
	if err != nil {
		t.Fatal(err)
	}
	if add != wantRestricted {
		t.Errorf("restricted add:\n%s\nwant:\n%s", add, wantRestricted)
	}
	if strings.Contains(rm, "vm3_conns") || !strings.HasPrefix(rm, `delete element inet openfactory vm_in  { "ofvm3" }`) {
		t.Errorf("restricted remove:\n%s", rm)
	}
	vm.Mode = Unrestricted
	add, rm, err = RenderVM(cfg, vm)
	if err != nil {
		t.Fatal(err)
	}
	if add != wantUnrestricted {
		t.Errorf("unrestricted add:\n%s\nwant:\n%s", add, wantUnrestricted)
	}
	if rm != wantRemoveUnrestricted {
		t.Errorf("unrestricted remove:\n%s", rm)
	}
	cfg.BlockSMTP = false
	add, _, _ = RenderVM(cfg, vm)
	if strings.Contains(add, "dport 25") {
		t.Error("smtp rule without block_smtp")
	}
	// Restricted: no forward path, no DNS.
	vm.Mode = Restricted
	add, _, _ = RenderVM(cfg, vm)
	if strings.Contains(add, "dport 53") || strings.Contains(add, "vm3_fwd accept") {
		t.Error("restricted rules allow DNS or forwarding")
	}
}

func TestRenderBase(t *testing.T) {
	base, err := RenderBase(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`comment "openfactory base v2"`,
		"elements = { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 168.63.129.16/32, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.0.2.0/24, 192.88.99.0/24, 192.168.0.0/16, 198.18.0.0/15, 198.51.100.0/24, 203.0.113.0/24, 224.0.0.0/4, 240.0.0.0/4 }",
		"elements = { ::/96, ::ffff:0:0/96, 64:ff9b:1::/48, 100::/64, 2001::/32, 2001:db8::/32, fc00::/7, fe80::/10, fec0::/10, ff00::/8 }",
		"type ipv4_addr . inet_service; flags interval\n    elements = { 10.1.2.3 . 443 }",
		"elements = { 127.0.0.53 }",
		"map vm_in  { type ifname : verdict; }",
		"type filter hook input priority filter - 10; policy accept;\n    iifname \"ofvm*\" jump from_vm_input\n",
		"iifname vmap @vm_in\n    drop\n",
		"    meta nfproto ipv6 drop\n    iifname vmap @vm_fwd\n    drop\n",
		`    oifname "ofvm*" ct state established,related accept`,
		"meta skuid 899999 jump ofnet_out",
		"ip daddr @resolvers4 meta l4proto { tcp, udp } th dport 53 accept\n    ip daddr @private4 reject with icmpx admin-prohibited\n    ip6 daddr @private6 reject with icmpx admin-prohibited",
		"type nat hook postrouting priority srcnat; policy accept;\n    ip saddr 10.213.0.0/16 oifname != \"ofvm*\" masquerade",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base lacks %q\n%s", want, base)
		}
	}
	full, _ := RenderFullApply(testConfig())
	if !strings.HasPrefix(full, "table inet openfactory\ndelete table inet openfactory\ntable inet openfactory {") {
		t.Errorf("full apply:\n%s", full)
	}
	// Empty sets render without elements.
	cfg := testConfig()
	cfg.InternalAllow, cfg.Resolvers = nil, nil
	base, _ = RenderBase(cfg)
	if strings.Contains(base, "elements = {  }") {
		t.Errorf("empty elements:\n%s", base)
	}
}

func TestRenderRefresh(t *testing.T) {
	r, err := RenderRefresh(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"flush set inet openfactory internal_allow4\nadd element inet openfactory internal_allow4 { 10.1.2.3 . 443 }",
		"flush set inet openfactory private4\nadd element inet openfactory private4 { 0.0.0.0/8, ",
		"flush set inet openfactory private6\nadd element inet openfactory private6 { ::/96, ",
		"flush chain inet openfactory output\nadd rule inet openfactory output meta skuid 899999 jump ofnet_out",
		`add rule inet openfactory postrouting ip saddr 10.213.0.0/16 oifname != "ofvm*" masquerade`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("refresh lacks %q\n%s", want, r)
		}
	}
	if strings.Contains(r, "vm_in") && strings.Contains(r, "flush map") {
		t.Error("refresh must keep the maps")
	}
}

func TestHex6(t *testing.T) {
	for in, want := range map[string]string{
		"::":                   "::",
		"::1":                  "::1",
		"::ffff:0.0.0.0":       "::ffff:0:0",
		"64:ff9b:1::":          "64:ff9b:1::",
		"2001:db8::":           "2001:db8::",
		"fe80::":               "fe80::",
		"2001:0:0:1:0:0:0:1":   "2001:0:0:1::1",
		"1:2:3:4:5:6:7:8":      "1:2:3:4:5:6:7:8",
		"100::":                "100::",
		"2001:db8:0:1:1:1:1:1": "2001:db8:0:1:1:1:1:1",
	} {
		a := netip.MustParseAddr(in)
		got := hex6(a)
		if got != want {
			t.Errorf("hex6(%s) = %q, want %q", in, got, want)
		}
		if netip.MustParseAddr(got) != a {
			t.Errorf("hex6(%s) = %q parses to another address", in, got)
		}
	}
}

// TestPrivateSetsMatchNetguard checks that the nftables sets hold exactly the
// reserved table of netguard, so the two guards cannot drift.
func TestPrivateSetsMatchNetguard(t *testing.T) {
	got := map[string]bool{}
	for _, p := range append(slices.Clone(Private4), Private6...) {
		got[netip.MustParsePrefix(p).String()] = true
	}
	if len(got) != len(netguard.ReservedPrefixes) {
		t.Errorf("sets have %d prefixes, netguard has %d", len(got), len(netguard.ReservedPrefixes))
	}
	for _, p := range netguard.ReservedPrefixes {
		if !got[p.String()] {
			t.Errorf("sets lack %s", p)
		}
	}
	for _, p := range Private4 {
		if !netip.MustParsePrefix(p).Addr().Is4() {
			t.Errorf("private4 holds %s", p)
		}
	}
}

func TestHostApplier(t *testing.T) {
	var calls []string
	proc := t.TempDir()
	for _, d := range []string{"net/ipv4/conf/ofvm3", "net/ipv6/conf/ofvm3"} {
		_ = os.MkdirAll(filepath.Join(proc, d), 0o755)
	}
	h := &HostApplier{ProcSys: proc, Run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " ")+"|"+stdin)
		return "", nil
	}}
	vm, _ := Alloc(netip.MustParsePrefix("10.213.0.0/16"), 3, 900003, Restricted)
	if err := h.AddVM(context.Background(), vm, "RULES"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip link del ofvm3|",
		"ip tuntap add dev ofvm3 mode tap user 900003 group 900003|",
		"ip addr add 10.213.0.13/30 dev ofvm3|",
		"ip link set ofvm3 up mtu 1500|",
		"nft -f -|RULES",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(proc, "net/ipv4/conf/ofvm3/rp_filter")); string(b) != "1" {
		t.Errorf("rp_filter %q", b)
	}
}

func TestHelperArgs(t *testing.T) {
	vm, _ := Alloc(netip.MustParsePrefix("10.213.0.0/16"), 3, 900003, Restricted)
	s := HelperSpec{VM: vm, Exe: "/usr/local/bin/openfactory-runtime", Allowed: []string{"github.com", "api.anthropic.com"}, AlwaysAllow: []string{"factory.example.com"}}
	got := strings.Join(s.Args(), " ")
	want := "/usr/local/bin/openfactory-runtime netguard --listen 10.213.0.13:3128 --report-listen 10.213.0.13:3129 --allow github.com,api.anthropic.com --always-allow factory.example.com"
	if got != want {
		t.Fatalf("args:\n%s\nwant\n%s", got, want)
	}
	vm.Mode = Unrestricted
	s = HelperSpec{VM: vm, Exe: "x", Upstreams: []netip.Addr{netip.MustParseAddr("127.0.0.53")}}
	if got := strings.Join(s.Args(), " "); got != "x worker dns-relay --listen 10.213.0.13:53 --upstream 127.0.0.53" || s.ListenAddr() != "10.213.0.13:53" {
		t.Fatalf("relay args %q", got)
	}
}

func TestParseResolvers(t *testing.T) {
	got := ParseResolvers("# c\nnameserver 127.0.0.53\nnameserver ::1\nnameserver 9.9.9.9\nsearch x\n")
	if len(got) != 2 || got[0].String() != "127.0.0.53" || got[1].String() != "9.9.9.9" {
		t.Fatalf("resolvers %v", got)
	}
}

func TestDNSRelayUDPAndTCP(t *testing.T) {
	up, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer up.Close()
	port := up.LocalAddr().(*net.UDPAddr).Port
	tl, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Skip(err)
	}
	defer tl.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := up.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = up.WriteTo(append([]byte("re:"), buf[:n]...), from)
		}
	}()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err == nil {
					_, _ = c.Write(append([]byte("tcp:"), buf[:n]...))
				}
			}()
		}
	}()
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	listen := pc.LocalAddr().String()
	_ = pc.Close()
	r := &DNSRelay{Listen: listen, Upstreams: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, port: port, Timeout: 2 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	if err := rtcore.WaitDial(ctx, "tcp", listen, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp4", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte("q1"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "re:q1" {
		t.Fatalf("udp answer %q, %v", buf[:n], err)
	}
	tc, err := net.Dial("tcp4", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = tc.Write([]byte("q2"))
	n, err = tc.Read(buf)
	if err != nil || string(buf[:n]) != "tcp:q2" {
		t.Fatalf("tcp answer %q, %v", buf[:n], err)
	}
}
