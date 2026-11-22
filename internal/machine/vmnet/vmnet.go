// Package vmnet builds the host network of the microVMs: one tap and one /30
// subnet per slot, nftables rules that isolate each VM, and the per-VM
// helper process (netguard for restricted tasks, a DNS relay for
// unrestricted tasks).
package vmnet

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Table is the nftables table of the worker.
const Table = "openfactory"

// BaseVersion is the version comment of the base ruleset. Change it when the
// base rules change.
const BaseVersion = "openfactory base v1"

// Ports of the per-VM helper on the host address of the tap.
const (
	ProxyPort  = 3128
	ReportPort = 3129
	DNSPort    = 53
)

// TemplateTap is the tap of a snapshot template VM.
const TemplateTap = "oftpl0"

// Mode is the network mode of a VM.
type Mode int

const (
	// Restricted VMs have no forward path and no DNS: they reach the
	// internet only through netguard.
	Restricted Mode = iota
	// Unrestricted VMs get masquerade NAT and a DNS relay. The always-on
	// drops still apply.
	Unrestricted
)

func (m Mode) String() string {
	if m == Unrestricted {
		return "unrestricted"
	}
	return "restricted"
}

// Private4 is the set of IPv4 ranges that a VM never reaches, with any
// policy: unspecified, RFC 1918, CGNAT, loopback, Azure's platform address,
// link-local (cloud metadata), IETF and documentation ranges, benchmarking,
// multicast and reserved.
var Private4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16/32",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
}

// Private6 is the IPv6 counterpart of Private4. VMs have no IPv6 at all;
// the set guards the proxy uid.
var Private6 = []string{
	"::/127", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
}

// Config is the network configuration that the rules depend on.
type Config struct {
	Subnet   netip.Prefix
	OfnetUID int
	// InternalAllow are ip:port exceptions to the private-range drop.
	InternalAllow []netip.AddrPort
	// Resolvers are the host resolvers that the DNS relay and netguard use.
	Resolvers []netip.Addr
	BlockSMTP bool
	MaxConns  int
	MTU       int
}

// VMNet is the network of one VM.
type VMNet struct {
	Slot     int
	Tap      string
	HostIP   netip.Addr
	GuestIP  netip.Addr
	GuestMAC net.HardwareAddr
	// Owner is the jail uid that owns the tap.
	Owner int
	Mode  Mode
}

// Chain returns the name prefix of the per-VM chains, for example "vm3".
func (v VMNet) Chain() string { return "vm" + strconv.Itoa(v.Slot) }

// GuestPrefix returns the guest address with its /30 prefix.
func (v VMNet) GuestPrefix() string { return v.GuestIP.String() + "/30" }

// Alloc returns the network of slot n: tap ofvm<n>, the /30 at offset 4n,
// host base+4n+1, guest base+4n+2, MAC 06:00 plus the guest IPv4.
func Alloc(subnet netip.Prefix, slot, owner int, mode Mode) (VMNet, error) {
	if !subnet.Addr().Is4() {
		return VMNet{}, fmt.Errorf("subnet %s is not IPv4", subnet)
	}
	if slot < 0 || slot > 255 {
		return VMNet{}, fmt.Errorf("slot %d out of range", slot)
	}
	size := 1 << (32 - subnet.Bits())
	if 4*slot+4 > size {
		return VMNet{}, fmt.Errorf("subnet %s has no /30 for slot %d", subnet, slot)
	}
	b := subnet.Masked().Addr().As4()
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	ip := func(off uint32) netip.Addr {
		v := base + off
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}) //nolint:gosec // G115: bytes of a 32-bit value
	}
	n := uint32(slot) * 4 //nolint:gosec // G115: slot is 0..255
	guest := ip(n + 2)
	g := guest.As4()
	return VMNet{
		Slot: slot, Tap: "ofvm" + strconv.Itoa(slot),
		HostIP: ip(n + 1), GuestIP: guest,
		GuestMAC: net.HardwareAddr{0x06, 0x00, g[0], g[1], g[2], g[3]},
		Owner:    owner, Mode: mode,
	}, nil
}

const baseTmpl = `table inet openfactory {
  comment "{{.Version}}"

  set private4 {
    type ipv4_addr; flags interval
    elements = { {{join .Private4}} }
  }
  set private6 {
    type ipv6_addr; flags interval
    elements = { {{join .Private6}} }
  }
  set internal_allow4 {
    type ipv4_addr . inet_service; flags interval
{{- if .InternalAllow}}
    elements = { {{join .InternalAllow}} }
{{- end}}
  }
  set resolvers4 {
    type ipv4_addr
{{- if .Resolvers}}
    elements = { {{join .Resolvers}} }
{{- end}}
  }
  map vm_in  { type ifname : verdict; }
  map vm_fwd { type ifname : verdict; }

  chain input {
    type filter hook input priority filter - 10; policy accept;
{{template "input" .}}  }
  chain from_vm_input {
{{template "from_vm_input" .}}  }

  chain forward {
    type filter hook forward priority filter - 10; policy accept;
{{template "forward" .}}  }
  chain from_vm_forward {
{{template "from_vm_forward" .}}  }

  chain output {
    type filter hook output priority filter - 10; policy accept;
{{template "output" .}}  }
  chain ofnet_out {
{{template "ofnet_out" .}}  }

  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
{{template "postrouting" .}}  }
}
`

// The rule bodies of the base chains. Both the full ruleset and the refresh
// use them.
var baseRules = map[string]string{
	"input":           `    iifname "ofvm*" jump from_vm_input` + "\n",
	"from_vm_input":   "    iifname vmap @vm_in\n    drop\n",
	"forward":         `    iifname "ofvm*" jump from_vm_forward` + "\n" + `    oifname "ofvm*" ct state established,related accept` + "\n" + `    oifname "ofvm*" drop` + "\n",
	"from_vm_forward": "    meta nfproto ipv6 drop\n    iifname vmap @vm_fwd\n    drop\n",
	"output":          "    meta skuid {{.OfnetUID}} jump ofnet_out\n",
	"ofnet_out": "    ip daddr . tcp dport @internal_allow4 accept\n" +
		"    ip daddr @resolvers4 meta l4proto { tcp, udp } th dport 53 accept\n" +
		"    ip daddr @private4 reject with icmpx admin-prohibited\n" +
		"    ip6 daddr @private6 reject with icmpx admin-prohibited\n",
	"postrouting": `    ip saddr {{.Subnet}} oifname != "ofvm*" masquerade` + "\n",
}

// baseChainOrder is the order of the base chains in a refresh.
var baseChainOrder = []string{"input", "from_vm_input", "forward", "from_vm_forward", "output", "ofnet_out", "postrouting"}

type baseData struct {
	Version       string
	Private4      []string
	Private6      []string
	InternalAllow []string
	Resolvers     []string
	OfnetUID      int
	Subnet        string
}

func data(cfg Config) (baseData, error) {
	if !cfg.Subnet.IsValid() || !cfg.Subnet.Addr().Is4() {
		return baseData{}, fmt.Errorf("subnet %s is not IPv4", cfg.Subnet)
	}
	if cfg.OfnetUID <= 0 {
		return baseData{}, fmt.Errorf("ofnet uid %d", cfg.OfnetUID)
	}
	d := baseData{
		Version: BaseVersion, Private4: Private4, Private6: Private6,
		OfnetUID: cfg.OfnetUID, Subnet: cfg.Subnet.Masked().String(),
	}
	for _, a := range cfg.InternalAllow {
		if !a.Addr().Is4() {
			return d, fmt.Errorf("internal allow %s is not IPv4", a)
		}
		d.InternalAllow = append(d.InternalAllow, a.Addr().String()+" . "+strconv.Itoa(int(a.Port())))
	}
	for _, r := range cfg.Resolvers {
		if r.Is4() {
			d.Resolvers = append(d.Resolvers, r.String())
		}
	}
	sort.Strings(d.InternalAllow)
	sort.Strings(d.Resolvers)
	return d, nil
}

func newTemplate() *template.Template {
	t := template.New("base").Funcs(template.FuncMap{"join": func(s []string) string { return strings.Join(s, ", ") }})
	for name, body := range baseRules {
		template.Must(t.New(name).Parse(body))
	}
	return template.Must(t.Parse(baseTmpl))
}

// RenderBase returns the full base ruleset. The agent applies it when no VM
// runs, after removing the table.
func RenderBase(cfg Config) (string, error) {
	d, err := data(cfg)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := newTemplate().Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderFullApply returns the script that replaces the table: it creates an
// empty table so that the delete never fails, deletes it and declares it
// again. nft -f applies the script as one transaction.
func RenderFullApply(cfg Config) (string, error) {
	base, err := RenderBase(cfg)
	if err != nil {
		return "", err
	}
	return "table inet openfactory\ndelete table inet openfactory\n" + base, nil
}

// RenderRefresh returns the script that refills the base chains and the
// sets in one transaction, and keeps the maps and the per-VM chains. The
// agent uses it at start while adopted VMs run.
func RenderRefresh(cfg Config) (string, error) {
	d, err := data(cfg)
	if err != nil {
		return "", err
	}
	t := newTemplate()
	var b bytes.Buffer
	b.WriteString("add table inet openfactory\n")
	for _, set := range []struct {
		name  string
		items []string
	}{{"internal_allow4", d.InternalAllow}, {"resolvers4", d.Resolvers}} {
		fmt.Fprintf(&b, "flush set inet openfactory %s\n", set.name)
		if len(set.items) > 0 {
			fmt.Fprintf(&b, "add element inet openfactory %s { %s }\n", set.name, strings.Join(set.items, ", "))
		}
	}
	for _, chain := range baseChainOrder {
		fmt.Fprintf(&b, "flush chain inet openfactory %s\n", chain)
		var body bytes.Buffer
		if err := t.ExecuteTemplate(&body, chain, d); err != nil {
			return "", err
		}
		for _, line := range strings.Split(strings.TrimRight(body.String(), "\n"), "\n") {
			fmt.Fprintf(&b, "add rule inet openfactory %s %s\n", chain, strings.TrimSpace(line))
		}
	}
	return b.String(), nil
}

// RenderVM returns the script that adds the rules of one VM and the script
// that removes them.
func RenderVM(cfg Config, vm VMNet) (add, remove string, err error) {
	if !vm.HostIP.Is4() || !vm.GuestIP.Is4() || len(vm.GuestMAC) != 6 || vm.Tap == "" {
		return "", "", fmt.Errorf("incomplete VM network %+v", vm)
	}
	c := vm.Chain()
	in, fwd := c+"_in", c+"_fwd"
	var a, r strings.Builder
	p := func(b *strings.Builder, f string, args ...any) { fmt.Fprintf(b, f+"\n", args...) }
	p(&a, "add chain inet openfactory %s", in)
	p(&a, "add chain inet openfactory %s", fwd)
	if vm.Mode == Unrestricted {
		p(&a, "add set inet openfactory %s_conns { type ipv4_addr; flags dynamic; size 1; }", c)
	}
	p(&a, "add rule inet openfactory %s ether saddr != %s drop", in, vm.GuestMAC)
	p(&a, "add rule inet openfactory %s ip saddr != %s drop", in, vm.GuestIP)
	if vm.Mode == Restricted {
		p(&a, "add rule inet openfactory %s ip daddr %s tcp dport { %d, %d } accept", in, vm.HostIP, ProxyPort, ReportPort)
		p(&a, "add rule inet openfactory %s drop", in)
		p(&a, "add rule inet openfactory %s reject with icmpx admin-prohibited", fwd)
	} else {
		maxConns := cfg.MaxConns
		if maxConns <= 0 {
			maxConns = 2048
		}
		p(&a, "add rule inet openfactory %s ip daddr %s meta l4proto { tcp, udp } th dport %d accept", in, vm.HostIP, DNSPort)
		p(&a, "add rule inet openfactory %s drop", in)
		p(&a, "add rule inet openfactory %s ip saddr != %s drop", fwd, vm.GuestIP)
		p(&a, "add rule inet openfactory %s ct state new add @%s_conns { ip saddr ct count over %d } reject with icmpx admin-prohibited", fwd, c, maxConns)
		p(&a, "add rule inet openfactory %s ip daddr . tcp dport @internal_allow4 accept", fwd)
		p(&a, "add rule inet openfactory %s ip daddr @private4 reject with icmpx admin-prohibited", fwd)
		if cfg.BlockSMTP {
			p(&a, "add rule inet openfactory %s tcp dport 25 reject with tcp reset", fwd)
		}
		p(&a, "add rule inet openfactory %s accept", fwd)
	}
	p(&a, `add element inet openfactory vm_in  { "%s" : jump %s }`, vm.Tap, in)
	p(&a, `add element inet openfactory vm_fwd { "%s" : jump %s }`, vm.Tap, fwd)

	p(&r, `delete element inet openfactory vm_in  { "%s" }`, vm.Tap)
	p(&r, `delete element inet openfactory vm_fwd { "%s" }`, vm.Tap)
	p(&r, "flush chain inet openfactory %s", in)
	p(&r, "flush chain inet openfactory %s", fwd)
	p(&r, "delete chain inet openfactory %s", in)
	p(&r, "delete chain inet openfactory %s", fwd)
	if vm.Mode == Unrestricted {
		p(&r, "delete set inet openfactory %s_conns", c)
	}
	return a.String(), r.String(), nil
}

// Applier changes the host network. The real one runs nft and ip; tests use
// a fake that records the text.
type Applier interface {
	ApplyBase(ctx context.Context, rules string) error
	AddVM(ctx context.Context, vm VMNet, rules string) error
	RemoveVM(ctx context.Context, vm VMNet, rules string) error
}

// ParseResolvers returns the IPv4 name servers of a resolv.conf.
func ParseResolvers(resolvConf string) []netip.Addr {
	var out []netip.Addr
	for _, line := range strings.Split(resolvConf, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() {
				out = append(out, a)
			}
		}
	}
	return out
}

// PrivateAddr reports whether a is in Private4 or Private6.
func PrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	set := Private6
	if a.Is4() {
		set = Private4
	}
	for _, s := range set {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}
