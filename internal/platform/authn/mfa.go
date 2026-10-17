package authn

import (
	"net/http"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CheckMFA applies the tenant's multi-factor rule to a person's token
// (ADR-0031): where it is required, the token's acr must be one of the
// provider's acr values or its amr must hold one of the provider's amr
// values. Otherwise the answer is 401 identity.mfa_required with the step-up
// challenge of RFC 9470, naming the acr values to ask the provider for.
func CheckMFA(c reqctx.Claims) error {
	rule := c.MFA
	if !rule.Required || (c.ACR != "" && slices.Contains(rule.ACRValues, c.ACR)) ||
		slices.ContainsFunc(c.AMR, func(m string) bool { return slices.Contains(rule.AMRValues, m) }) {
		return nil
	}
	challenge := `Bearer error="insufficient_user_authentication", error_description="Multi-factor authentication is required."`
	if len(rule.ACRValues) > 0 {
		challenge += `, acr_values="` + quoted.Replace(strings.Join(rule.ACRValues, " ")) + `"`
	}
	p := problem.New(http.StatusUnauthorized, "identity.mfa_required", "Multi-factor authentication required",
		"The tenant requires multi-factor authentication; sign in again at the identity provider with a second factor.")
	p.Header = http.Header{"Www-Authenticate": {challenge}}
	return p
}

// quoted escapes a value for an HTTP quoted-string (RFC 9110 §5.6.4).
var quoted = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
