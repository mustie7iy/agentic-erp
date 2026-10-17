package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// pathID parses the path parameter name as a UUID; anything else is 400.
func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, problem.BadRequest("request.invalid_parameter", "Parameter "+name+" is invalid.",
			problem.FieldError{Field: name, Code: "invalid", Message: "a UUID"})
	}
	return id, nil
}

// serveIn serves an operation on the tenant named in the path.
func (h *handler) serveIn(w http.ResponseWriter, r *http.Request, operation string, body func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error) {
	h.op.Serve(w, r, operation, func(w http.ResponseWriter, r *http.Request) error {
		tenant, err := pathID(r, "id")
		if err != nil {
			return err
		}
		return body(w, r, tenant)
	})
}

// providers serves {id}/identity-providers.
func (h *handler) providers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.serveIn(w, r, "listIdentityProviders", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			ps, err := h.s.ListProviders(r.Context(), tenant)
			if err == nil {
				httpapi.WriteJSON(w, http.StatusOK, map[string]any{"items": ps})
			}
			return err
		})
	case http.MethodPost:
		h.serveIn(w, r, "registerIdentityProvider", h.register)
	default:
		httpapi.NotAllowed(w, r, "GET, POST")
	}
}

// providerBody is the IdentityProviderCreate schema of the contract.
type providerBody struct {
	Issuer      string   `json:"issuer"`
	Audience    string   `json:"audience"`
	TenantClaim *string  `json:"tenant_claim"`
	ACRValues   []string `json:"acr_values"`
	AMRValues   []string `json:"amr_values"`
	// CertificateBound is certificate_bound_tokens (ADR-0033).
	CertificateBound bool `json:"certificate_bound_tokens"`
}

func (h *handler) register(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
	body, key, err := readBody(w, r)
	if err != nil {
		return err
	}
	var in providerBody
	if err := strictJSON(body, &in); err != nil {
		return err
	}
	p, replayed, err := h.s.RegisterProvider(r.Context(), operator(r.Context()), tenant,
		identity.NewIdentityProvider{Issuer: in.Issuer, Audience: in.Audience, TenantClaim: in.TenantClaim, ACRValues: in.ACRValues, AMRValues: in.AMRValues,
			CertificateBound: in.CertificateBound}, key)
	if err != nil {
		return err
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("ETag", etag.Format(p.Version))
	w.Header().Set("Location", Base+"/tenants/"+tenant.String()+"/identity-providers/"+p.ID.String())
	httpapi.WriteJSON(w, http.StatusCreated, p)
	return nil
}

// strictJSON decodes a body that must match the contract's schema.
func strictJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return problem.BadRequest("request.invalid_body", "The request body does not match the contract.").WithCause(err)
	}
	return nil
}

// patchBody reads a merge-patch body of at most uow.MaxBody bytes. Like
// the public API, a PATCH body is application/merge-patch+json (RFC 7396)
// and nothing else.
func patchBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/merge-patch+json" {
		return nil, problem.BadRequest("request.invalid_body", "The body must be application/merge-patch+json.")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, uow.MaxBody))
	if err != nil {
		return nil, problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large",
			"The request body exceeds 5 MB.").WithCause(err)
	}
	return body, nil
}

// provider serves {id}/identity-providers/{provider_id} and its :retire.
func (h *handler) provider(w http.ResponseWriter, r *http.Request) {
	raw, verb, custom := strings.Cut(r.PathValue("rest"), ":")
	run := func(operation string, fn func(w http.ResponseWriter, r *http.Request, tenant, id uuid.UUID) error) {
		h.serveIn(w, r, operation, func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			id, err := uuid.Parse(raw)
			if err != nil {
				return problem.BadRequest("request.invalid_parameter", "Parameter provider_id is invalid.",
					problem.FieldError{Field: "provider_id", Code: "invalid", Message: "a UUID"})
			}
			return fn(w, r, tenant, id)
		})
	}
	switch {
	case custom && verb != "retire":
		problem.Write(w, r, problem.NotFound("No such method on identity providers: "+verb+"."))
	case custom && r.Method == http.MethodPost:
		run("retireIdentityProvider", func(w http.ResponseWriter, r *http.Request, tenant, id uuid.UUID) error {
			return answer(w, http.StatusOK)(h.s.RetireProvider(r.Context(), operator(r.Context()), tenant, id, r.Header.Get("If-Match")))
		})
	case custom:
		httpapi.NotAllowed(w, r, "POST")
	case r.Method == http.MethodGet:
		run("getIdentityProvider", func(w http.ResponseWriter, r *http.Request, tenant, id uuid.UUID) error {
			return answer(w, http.StatusOK)(h.s.GetProvider(r.Context(), tenant, id))
		})
	case r.Method == http.MethodPatch:
		run("updateIdentityProvider", h.updateProvider)
	default:
		httpapi.NotAllowed(w, r, "GET, PATCH")
	}
}

// answer writes a provider with its ETag, or returns the error.
func answer(w http.ResponseWriter, status int) func(IdentityProvider, error) error {
	return func(p IdentityProvider, err error) error {
		if err != nil {
			return err
		}
		w.Header().Set("ETag", etag.Format(p.Version))
		httpapi.WriteJSON(w, status, p)
		return nil
	}
}

func (h *handler) updateProvider(w http.ResponseWriter, r *http.Request, tenant, id uuid.UUID) error {
	body, err := patchBody(w, r)
	if err != nil {
		return err
	}
	ch, err := providerChange(body)
	if err != nil {
		return err
	}
	return answer(w, http.StatusOK)(h.s.UpdateProvider(r.Context(), operator(r.Context()), tenant, id, r.Header.Get("If-Match"), ch))
}

// providerChange reads the IdentityProviderUpdate merge patch: null clears
// the tenant claim or empties a list; certificate_bound_tokens is true or
// false, never null.
func providerChange(body []byte) (identity.IdentityProviderChange, error) {
	var patch struct {
		TenantClaim json.RawMessage `json:"tenant_claim"`
		ACRValues   json.RawMessage `json:"acr_values"`
		AMRValues   json.RawMessage `json:"amr_values"`
		Bound       json.RawMessage `json:"certificate_bound_tokens"`
	}
	var ch identity.IdentityProviderChange
	if err := strictJSON(body, &patch); err != nil {
		return ch, err
	}
	if patch.TenantClaim != nil {
		var claim *string
		if err := json.Unmarshal(patch.TenantClaim, &claim); err != nil {
			return ch, problem.BadRequest("request.invalid_body", "tenant_claim must be a string or null.").WithCause(err)
		}
		ch.TenantClaim = &claim
	}
	if patch.Bound != nil {
		var bound *bool
		// null is refused rather than read as false: a security flag is
		// set or cleared by name.
		err := json.Unmarshal(patch.Bound, &bound)
		if err != nil || bound == nil {
			return ch, problem.BadRequest("request.invalid_body", "certificate_bound_tokens must be true or false.").WithCause(err)
		}
		ch.CertificateBound = bound
	}
	for _, f := range []struct {
		raw json.RawMessage
		to  **[]string
	}{{patch.ACRValues, &ch.ACRValues}, {patch.AMRValues, &ch.AMRValues}} {
		if f.raw == nil {
			continue
		}
		values := []string{} // null empties the list
		if err := json.Unmarshal(f.raw, &values); err != nil {
			return ch, problem.BadRequest("request.invalid_body", "acr_values and amr_values must be lists of strings or null.").WithCause(err)
		}
		if values == nil {
			values = []string{}
		}
		*f.to = &values
	}
	return ch, nil
}

// policy serves {id}/auth-policy.
func (h *handler) policy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.serveIn(w, r, "getAuthPolicy", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			return policyAnswer(w)(h.s.AuthPolicy(r.Context(), tenant))
		})
	case http.MethodPatch:
		h.serveIn(w, r, "updateAuthPolicy", h.updatePolicy)
	default:
		httpapi.NotAllowed(w, r, "GET, PATCH")
	}
}

func policyAnswer(w http.ResponseWriter) func(AuthPolicy, error) error {
	return func(p AuthPolicy, err error) error {
		if err != nil {
			return err
		}
		w.Header().Set("ETag", etag.Format(p.Version))
		httpapi.WriteJSON(w, http.StatusOK, p)
		return nil
	}
}

func (h *handler) updatePolicy(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
	body, err := patchBody(w, r)
	if err != nil {
		return err
	}
	var patch struct {
		MFARequired   *bool  `json:"mfa_required"`
		SessionMaxAge *int64 `json:"session_max_age_seconds"`
		IdleTimeout   *int64 `json:"idle_timeout_seconds"`
	}
	if err := strictJSON(body, &patch); err != nil {
		return err
	}
	if err := noNulls(body); err != nil {
		return err
	}
	ch := identity.AuthPolicyChange{MFARequired: patch.MFARequired, SessionMaxAge: seconds(patch.SessionMaxAge), IdleTimeout: seconds(patch.IdleTimeout)}
	p, err := h.s.UpdateAuthPolicy(r.Context(), operator(r.Context()), tenant, r.Header.Get("If-Match"), ch)
	return policyAnswer(w)(p, contractFields(err))
}

// noNulls refuses null values in a policy patch: none of its fields can be
// cleared (AuthPolicyUpdate), and null would otherwise read as unchanged.
func noNulls(body []byte) error {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields) // strictJSON decoded the same body
	for name, v := range fields {
		if string(v) == "null" {
			return problem.BadRequest("request.invalid_body", name+" cannot be null.",
				problem.FieldError{Field: name, Code: "not_nullable", Message: "a value, or leave the field out"})
		}
	}
	return nil
}

// contractFields names a policy problem's fields as the contract does: the
// durations are in seconds there.
func contractFields(err error) error {
	if err == nil {
		return nil
	}
	p := problem.From(err)
	for i := range p.Errors {
		if p.Errors[i].Field == "session_max_age" || p.Errors[i].Field == "idle_timeout" {
			p.Errors[i].Field += "_seconds"
		}
	}
	return p
}

// seconds is a duration of whole seconds, nil for nil. Out-of-range values
// are the policy's to refuse.
func seconds(s *int64) *time.Duration {
	if s == nil {
		return nil
	}
	d := time.Duration(min(max(*s, 0), int64(time.Duration(1<<62)/time.Second))) * time.Second
	return &d
}

// principals serves {id}/principals.
func (h *handler) principals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpapi.NotAllowed(w, r, "GET")
		return
	}
	h.serveIn(w, r, "listTenantPrincipals", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
		q := r.URL.Query()
		page, err := pageRequest(q)
		if err != nil {
			return err
		}
		ps, next, err := h.s.ListPrincipals(r.Context(), tenant, q.Get("binding"), page)
		if err != nil {
			return err
		}
		out := map[string]any{"items": ps}
		if next != "" {
			out["next_page_token"] = next
		}
		httpapi.WriteJSON(w, http.StatusOK, out)
		return nil
	})
}

// principal serves {id}/principals/{principal_id}:bind.
func (h *handler) principal(w http.ResponseWriter, r *http.Request) {
	raw, verb, custom := strings.Cut(r.PathValue("rest"), ":")
	switch {
	case !custom || verb != "bind":
		problem.Write(w, r, problem.NotFound("No such endpoint."))
	case r.Method != http.MethodPost:
		httpapi.NotAllowed(w, r, "POST")
	default:
		h.serveIn(w, r, "bindPrincipal", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			id, err := uuid.Parse(raw)
			if err != nil {
				return problem.BadRequest("request.invalid_parameter", "Parameter principal_id is invalid.",
					problem.FieldError{Field: "principal_id", Code: "invalid", Message: "a UUID"})
			}
			return h.bind(w, r, tenant, id)
		})
	}
}

func (h *handler) bind(w http.ResponseWriter, r *http.Request, tenant, id uuid.UUID) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		return problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large", "The request body exceeds 4 KB.").WithCause(err)
	}
	var in struct {
		IdentityProviderID uuid.UUID `json:"identity_provider_id"`
		Subject            string    `json:"subject"`
	}
	if err := strictJSON(body, &in); err != nil {
		return err
	}
	p, err := h.s.BindPrincipal(r.Context(), operator(r.Context()), tenant, id, r.Header.Get("If-Match"), in.IdentityProviderID, in.Subject)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(p.Version))
	httpapi.WriteJSON(w, http.StatusOK, p)
	return nil
}
