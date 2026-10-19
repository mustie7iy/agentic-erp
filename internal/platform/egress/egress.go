// Package egress is the way out of the cell to addresses a tenant names:
// identity providers (ADR-0031) and webhook endpoints (ADR-0041). Such a
// name may resolve, or re-resolve, to the cell's own network or to an
// instance metadata service, so every connection is checked on the address
// actually dialled, not on the name. The webhook client also connects
// directly (no proxy, ADR-0033), follows no redirect, trusts only the
// system roots, presents no client certificate, and bounds its time and
// what it reads.
package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"syscall"
)

// RefusePrivate allows connections only to public unicast addresses; it is
// a net.Dialer Control, so it judges the address dialled and a name
// resolving elsewhere (DNS rebinding included) is refused too. It fails
// closed: global unicast addresses outside every special-purpose range
// below, IPv4 written as IPv6 judged as IPv4.
func RefusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress: dial %s: %w", address, err)
	}
	ip := ap.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalUnicast6.Contains(ip)) ||
		slices.ContainsFunc(nonPublic, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return fmt.Errorf("egress: address %s is not a public unicast address", ip)
	}
	return nil
}

// globalUnicast6 is the only IPv6 range allocated for global unicast
// (RFC 4291): IPv4-compatible, site-local and discard addresses fall
// outside it.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// nonPublic are the special-purpose ranges (RFC 6890 and successors) that
// IsGlobalUnicast and IsPrivate let through: shared address space (cloud
// metadata services live there too), benchmarking, reserved, and the IPv6
// translation and tunnelling prefixes that would carry a private IPv4
// address.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}
