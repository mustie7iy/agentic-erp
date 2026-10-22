package httpapi

import (
	"net/http"
	"net/netip"

	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ClientAddress puts where the request came from in its context, for the
// audit trail (ADR-0011 as amended by T73c). Trusted (the peer verified as
// the router), it is the router's Erp-Client-Address, which the router
// always sets: the person's address in the web workspace, otherwise the
// client's connection; none when the header is missing or malformed, as
// the connection is the router's own. Untrusted (no trust domain:
// development, reached directly), it is the connection's. The header is
// removed either way, so nothing past it reads an unverified one.
func ClientAddress(trusted bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded := r.Header.Get(proxyproto.ClientAddressHeader)
		r.Header.Del(proxyproto.ClientAddressHeader)
		var addr netip.Addr
		var ok bool
		if trusted {
			addr, ok = proxyproto.ParseClientAddress(forwarded)
		} else {
			addr, ok = proxyproto.RemoteIP(r)
		}
		if ok {
			r = r.WithContext(reqctx.WithClientAddress(r.Context(), addr))
		}
		next.ServeHTTP(w, r)
	})
}
