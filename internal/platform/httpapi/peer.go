package httpapi

import (
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// RequirePeer serves next only to the service whose verified SPIFFE ID is
// id: the hop is authorized by the certificate, the request still by its
// token (ADR-0033). Anything else is 403 platform.peer_not_allowed.
func RequirePeer(id string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tlsconf.PeerID(r.TLS) != id {
			problem.Write(w, r, problem.Forbidden("platform.peer_not_allowed", "Only "+id+" may call this listener."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
