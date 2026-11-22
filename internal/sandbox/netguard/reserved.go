package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ReservedPrefixes are the address ranges that egress never reaches,
// whatever the policy allows: private networks, loopback, link-local,
// carrier-grade NAT, cloud metadata, benchmarking, documentation, multicast
// and the reserved ranges. A host name that the policy allows can resolve to
// one of these (DNS rebinding, or a self-managed service). Without this check
// the proxy would reach the network of the machine it runs on.
//
// netguard checks each dialed address against this table (ReservedAddr). The
// microVM worker loads the same table into its nftables sets as a second
// guard (internal/machine/vmnet), so the prefixes must not overlap: an
// nftables interval set refuses overlapping elements. The NAT64 (64:ff9b::/96)
// and 6to4 (2002::/16) ranges are not in the table: a public IPv4 address
// can hide behind them, so ReservedAddr checks the embedded IPv4 address
// instead, which nftables cannot do.
var ReservedPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8",        // "this network"
	"10.0.0.0/8",       // RFC 1918
	"100.64.0.0/10",    // carrier-grade NAT (RFC 6598)
	"127.0.0.0/8",      // loopback
	"169.254.0.0/16",   // link-local, includes 169.254.169.254 (cloud metadata)
	"172.16.0.0/12",    // RFC 1918
	"192.0.0.0/24",     // IETF protocol assignments
	"192.0.2.0/24",     // documentation (TEST-NET-1)
	"192.88.99.0/24",   // 6to4 relay anycast
	"192.168.0.0/16",   // RFC 1918
	"198.18.0.0/15",    // benchmarking
	"198.51.100.0/24",  // documentation (TEST-NET-2)
	"203.0.113.0/24",   // documentation (TEST-NET-3)
	"224.0.0.0/4",      // multicast
	"240.0.0.0/4",      // reserved, includes 255.255.255.255
	"168.63.129.16/32", // Azure platform address (WireServer, DNS)
	// IPv6
	"::/96",          // unspecified, loopback and IPv4-compatible (deprecated)
	"::ffff:0:0/96",  // IPv4-mapped; ReservedAddr checks the IPv4 address first, nftables cannot
	"64:ff9b:1::/48", // local-use NAT64 (RFC 8215)
	"100::/64",       // discard
	"2001::/32",      // Teredo: reaches any IPv4 address; a sandbox has no use for it
	"2001:db8::/32",  // documentation
	"fc00::/7",       // unique local (ULA), includes fd00:ec2::254 (cloud metadata)
	"fe80::/10",      // link-local
	"fec0::/10",      // site-local (deprecated)
	"ff00::/8",       // multicast
)

// Prefixes that embed an IPv4 address. The embedded address is checked.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// ReservedAddr reports whether netguard refuses to dial addr. IPv4-mapped
// IPv6 addresses (::ffff:a.b.c.d) and the IPv6 forms that embed an IPv4
// address (NAT64, 6to4) are checked by their IPv4 address. An invalid
// address counts as reserved.
func ReservedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap().WithZone("")
	for _, p := range ReservedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	if addr.Is6() {
		b := addr.As16()
		switch {
		case nat64Prefix.Contains(addr):
			return ReservedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case sixToFour.Contains(addr):
			return ReservedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}

// reservedAddrError is returned when a dial would reach a reserved address.
type reservedAddrError struct {
	host string
	addr netip.Addr
}

func (e *reservedAddrError) Error() string {
	return fmt.Sprintf("egress to %s blocked: it resolves to the reserved address %s", e.host, e.addr)
}

// dialControl returns a net.Dialer Control function that refuses reserved
// addresses. The dialer calls it for each resolved address just before the
// connect, so the check covers the address that is really dialed.
func dialControl(host string) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			ip, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				return &reservedAddrError{host: host}
			}
			a, perr := netip.ParseAddr(ip)
			if perr != nil {
				return &reservedAddrError{host: host}
			}
			ap = netip.AddrPortFrom(a, 0)
		}
		if ReservedAddr(ap.Addr()) {
			return &reservedAddrError{host: host, addr: ap.Addr().Unmap()}
		}
		return nil
	}
}

func isReservedAddrError(err error) (*reservedAddrError, bool) {
	var re *reservedAddrError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
