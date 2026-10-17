package httpapi

import (
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// ClientCertHeader is tlsconf.ClientCertHeader, the router's forwarded
// thumbprint.
const ClientCertHeader = tlsconf.ClientCertHeader

// ClientCertificate puts the forwarded thumbprint in the request's
// context when trusted (the peer verified as the router), and removes the
// header either way, so nothing past it reads an unverified one.
func ClientCertificate(trusted bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thumbprint := r.Header.Get(ClientCertHeader)
		r.Header.Del(ClientCertHeader)
		if trusted && thumbprint != "" {
			r = r.WithContext(reqctx.WithClientCert(r.Context(), thumbprint))
		}
		next.ServeHTTP(w, r)
	})
}
