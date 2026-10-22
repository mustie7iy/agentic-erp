package proxyproto

import (
	"net/http"
	"net/netip"
)

// ClientAddressHeader carries the IP address a request came from, for the
// API's audit trail (ADR-0011 as amended by T73c): from erp-web to the
// router, the address of the person using the workspace, which the router
// believes only from erp-web's verified identity; from the router to the
// cell, the address it settled on, which the cell believes only from the
// router's (ADR-0033). Anyone else's is dropped.
const ClientAddressHeader = "Erp-Client-Address"

// RemoteIP is the IP address of the connection a request came on: the
// client's, when the load balancer's PROXY header named it (Conn); ok is
// false when RemoteAddr is no address.
func RemoteIP(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap().WithZone(""), true // a zone names a host's interface, not where a client is
}

// ParseClientAddress reads ClientAddressHeader's value: one IP address,
// without a zone or a port.
func ParseClientAddress(v string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}
