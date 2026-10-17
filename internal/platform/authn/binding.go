package authn

import (
	"crypto/subtle"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CheckBinding applies certificate-bound tokens (RFC 8705, ADR-0033): a
// token bound to a certificate (cnf.x5t#S256) is accepted only with that
// certificate, presented being the thumbprint of the one the request was
// made with; and with requireBound (an agent's token in production) only
// a bound token of a provider that binds tokens is accepted.
func CheckBinding(c reqctx.Claims, presented string, requireBound bool) error {
	if c.CertThumbprint != "" && subtle.ConstantTimeCompare([]byte(c.CertThumbprint), []byte(presented)) != 1 {
		return problem.Unauthorized("The token is bound to a client certificate this request was not made with.")
	}
	if requireBound && (!c.CertificateBound || c.CertThumbprint == "") {
		return problem.Unauthorized("An agent's token must be bound to its client certificate (RFC 8705) by an identity provider that binds tokens.")
	}
	return nil
}
