// Package spiffeauth authorizes calls to a cell's Temporal by the SPIFFE
// identity of the caller's verified client certificate (ADR-0033,
// ADR-0035): the cell's server may use its namespace, the engine's own
// nodes administer it, and nothing else may do anything but the health
// checks. A certificate is never a principal of the platform: these
// claims say which service may call, not which tenant or user.
//
// The identities are the platform's (internal/platform/tlsconf.ID and
// PeerID, the same rule): this module cannot import the platform's, whose
// dependency versions Temporal's server does not build with (ADR-0035).
package spiffeauth

import (
	"crypto/tls"
	"net/url"

	"go.temporal.io/server/common/authorization"
)

// ID is a service's SPIFFE ID, spiffe://<trustDomain>/<scope>/<service>,
// scope being the cell (tlsconf.ID).
func ID(trustDomain, scope, service string) string {
	return "spiffe://" + trustDomain + "/" + scope + "/" + service
}

// PeerID is the SPIFFE ID of the leaf a connection verified, or "" when it
// verified none or the leaf names no SPIFFE ID (tlsconf.PeerID).
func PeerID(cs *tls.ConnectionState) string {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return ""
	}
	return spiffeID(cs.VerifiedChains[0][0].URIs)
}

// spiffeID is the one spiffe: URI among uris; none or several give ""
// (stricter than tlsconf, which takes the first: the platform's
// certificates name one, erp-cluster's policy requires it).
func spiffeID(uris []*url.URL) string {
	id := ""
	for _, u := range uris {
		if u.Scheme != "spiffe" {
			continue
		}
		if id != "" {
			return ""
		}
		id = u.String()
	}
	return id
}

// Mapper maps a verified client certificate's SPIFFE identity to claims.
type Mapper struct {
	// Server is the identity of the cell's server, the one client.
	Server string
	// Nodes is the identity of the engine's own services, which call each
	// other and their frontend (the system worker) as it.
	Nodes string
	// Namespace is the one namespace the server may use.
	Namespace string
}

// GetClaims implements authorization.ClaimMapper. A connection without a
// verified certificate, or with another identity, gets no role: Temporal's
// default authorizer then allows it the health checks alone.
func (m Mapper) GetClaims(info *authorization.AuthInfo) (*authorization.Claims, error) {
	id := ""
	if info != nil && info.TLSConnection != nil {
		id = PeerID(&info.TLSConnection.State)
	}
	switch id {
	case "":
		return &authorization.Claims{}, nil
	case m.Nodes:
		return &authorization.Claims{Subject: id, System: authorization.RoleAdmin, AuthType: "mtls"}, nil
	case m.Server:
		return &authorization.Claims{
			Subject:    id,
			Namespaces: map[string]authorization.Role{m.Namespace: authorization.RoleWriter},
			AuthType:   "mtls",
		}, nil
	default:
		return &authorization.Claims{Subject: id, AuthType: "mtls"}, nil
	}
}

// Authorizer is Temporal's role-based authorizer, which decides on the
// claims Mapper gives.
func Authorizer() authorization.Authorizer { return authorization.NewDefaultAuthorizer() }
