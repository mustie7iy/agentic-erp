package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// RequirePeer serves next only to the service whose verified SPIFFE ID is
// id: the hop is authorized by the certificate, the request still by its
// token (ADR-0033). Anything else is 403 platform.peer_not_allowed, logged.
func RequirePeer(id string, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peer := tlsconf.PeerID(r.TLS); peer != id {
			// Outside the request's telemetry, so the refusal is logged here.
			log.WarnContext(r.Context(), "peer not allowed", "peer", peer, "want", id, "path", r.URL.Path, "remote", r.RemoteAddr)
			problem.Write(w, r, problem.Forbidden("platform.peer_not_allowed", "Only "+id+" may call this listener."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
