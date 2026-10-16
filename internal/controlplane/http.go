package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// Base is the path prefix of the control plane on the admin listener.
const Base = "/control/v1"

// Handler serves the contract api/control.yaml for operators the verifier
// accepts (VerifyOperator). Errors are RFC 9457 problems like the public
// API's; failures of the platform are logged.
func Handler(s *Service, v *authn.Verifier, log *slog.Logger) http.Handler {
	h := &handler{s: s, op: httpapi.Operator{Roles: Roles, Log: log, Name: "control"}}
	mux := http.NewServeMux()
	mux.HandleFunc(Base+"/tenants", h.collection)
	mux.HandleFunc(Base+"/tenants/{rest}", h.item)
	mux.HandleFunc(Base+"/tenants/{id}/fingerprint", h.fingerprintItem)
	mux.HandleFunc(Base+"/tenants/{id}/identity-providers", h.providers)
	mux.HandleFunc(Base+"/tenants/{id}/identity-providers/{rest}", h.provider)
	mux.HandleFunc(Base+"/tenants/{id}/auth-policy", h.policy)
	mux.HandleFunc(Base+"/tenants/{id}/principals", h.principals)
	mux.HandleFunc(Base+"/tenants/{id}/principals/{rest}", h.principal)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.NotFound("No such endpoint."))
	})
	return v.OperatorMiddleware(mux)
}

type handler struct {
	s  *Service
	op httpapi.Operator
}

// Roles maps each operation to the operator role it needs (ADR-0027); any
// other needs authn.RoleOperate.
var Roles = map[string]string{"listTenants": authn.RoleRead, "getTenant": authn.RoleRead, "getTenantFingerprint": authn.RoleRead, "createTenant": authn.RoleProvision,
	// A tenant's identity providers and policy decide who signs in as its
	// principals: changing them needs erp:admin (ADR-0027 as amended by ADR-0031).
	"listIdentityProviders": authn.RoleRead, "getIdentityProvider": authn.RoleRead, "getAuthPolicy": authn.RoleRead,
	"registerIdentityProvider": authn.RoleAdmin, "updateIdentityProvider": authn.RoleAdmin, "retireIdentityProvider": authn.RoleAdmin, "updateAuthPolicy": authn.RoleAdmin,
	// Binding a principal to a subject hands its access to whoever holds the subject.
	"listTenantPrincipals": authn.RoleRead, "bindPrincipal": authn.RoleAdmin}

func (h *handler) collection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.op.Serve(w, r, "listTenants", h.list)
	case http.MethodPost:
		h.op.Serve(w, r, "createTenant", h.create)
	default:
		httpapi.NotAllowed(w, r, "GET, POST")
	}
}

func (h *handler) item(w http.ResponseWriter, r *http.Request) {
	raw, verb, custom := strings.Cut(r.PathValue("rest"), ":")
	id, err := uuid.Parse(raw)
	if err != nil {
		problem.Write(w, r, problem.BadRequest("request.invalid_parameter", "Parameter id is invalid.",
			problem.FieldError{Field: "id", Code: "invalid", Message: "a UUID"}))
		return
	}
	switch {
	case r.Method == http.MethodGet && !custom:
		h.op.Serve(w, r, "getTenant", func(w http.ResponseWriter, r *http.Request) error { return h.get(w, r, id) })
	case r.Method == http.MethodPost && custom && verb == "rotateKey":
		h.op.Serve(w, r, "rotateTenantKey", func(w http.ResponseWriter, r *http.Request) error { return h.rotateKey(w, r, id) })
	case r.Method == http.MethodPost && custom && verb == "erase":
		h.op.Serve(w, r, "eraseTenant", func(w http.ResponseWriter, r *http.Request) error { return h.erase(w, r, id) })
	case r.Method == http.MethodPost && custom:
		h.op.Serve(w, r, verb+"Tenant", func(w http.ResponseWriter, r *http.Request) error { return h.transition(w, r, id, verb) })
	case custom:
		httpapi.NotAllowed(w, r, "POST")
	default:
		httpapi.NotAllowed(w, r, "GET")
	}
}

func operator(ctx context.Context) Operator {
	claims, _ := reqctx.ClaimsFrom(ctx) // the middleware put them there
	return OperatorFrom(claims)
}

// pageRequest reads a listing's page parameters.
func pageRequest(q url.Values) (pagination.Request, error) {
	page := pagination.Request{OrderBy: q.Get("order_by"), PageToken: q.Get("page_token"), PageTokenSet: q.Has("page_token")}
	if q.Has("page_size") {
		n, err := strconv.ParseInt(q.Get("page_size"), 10, 32)
		if err != nil {
			return page, problem.BadRequest("request.invalid_parameter", "Parameter page_size is invalid.",
				problem.FieldError{Field: "page_size", Code: "invalid", Message: "an integer"})
		}
		page.PageSize, page.PageSizeSet = int32(n), true
	}
	return page, nil
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	page, err := pageRequest(q)
	if err != nil {
		return err
	}
	tenants, next, err := h.s.List(r.Context(), q.Get("status"), page)
	if err != nil {
		return err
	}
	out := map[string]any{"items": tenants}
	if next != "" {
		out["next_page_token"] = next
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, id uuid.UUID) error {
	t, err := h.s.Get(r.Context(), id)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(t.Version))
	httpapi.WriteJSON(w, http.StatusOK, t)
	return nil
}

// fingerprintItem serves the tenant's one sub-resource, {id}/fingerprint.
func (h *handler) fingerprintItem(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	switch {
	case err != nil:
		problem.Write(w, r, problem.BadRequest("request.invalid_parameter", "Parameter id is invalid.",
			problem.FieldError{Field: "id", Code: "invalid", Message: "a UUID"}))
	case r.Method != http.MethodGet:
		httpapi.NotAllowed(w, r, "GET")
	default:
		h.op.Serve(w, r, "getTenantFingerprint", func(w http.ResponseWriter, r *http.Request) error { return h.fingerprint(w, r, id) })
	}
}

// fingerprinted is the TenantFingerprint schema of the contract.
type fingerprinted struct {
	Tenant
	Fingerprint offboarding.Fingerprint `json:"fingerprint"`
}

func (h *handler) fingerprint(w http.ResponseWriter, r *http.Request, id uuid.UUID) error {
	t, fp, err := h.s.Fingerprint(r.Context(), id)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(t.Version))
	httpapi.WriteJSON(w, http.StatusOK, fingerprinted{Tenant: t, Fingerprint: fp})
	return nil
}

// createBody is the TenantCreate schema of the contract.
type createBody struct {
	Name          string `json:"name"`
	Region        string `json:"region"`
	GroupCurrency string `json:"group_currency"`
	Administrator struct {
		Subject     string  `json:"subject"`
		DisplayName string  `json:"display_name"`
		Email       *string `json:"email"`
	} `json:"administrator"`
	Configuration    json.RawMessage `json:"configuration"`
	IdentityProvider *providerBody   `json:"identity_provider"`
}

// readBody enforces the Idempotency-Key and body rules of 04 §4.3 and §4.11
// and hashes the request as the unit of work does.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *provisioning.Idempotency, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, nil, problem.PreconditionRequired("Idempotency-Key")
	}
	if len(key) < 8 || len(key) > 255 {
		return nil, nil, problem.BadRequest("request.invalid_idempotency_key", "Idempotency-Key must be 8 to 255 characters.",
			problem.FieldError{Field: "Idempotency-Key", Code: "invalid_length", Message: "8 to 255 characters"})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, uow.MaxBody))
	if err != nil {
		return nil, nil, problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large",
			"The request body exceeds 5 MB.").WithCause(err)
	}
	sum := sha256.New()
	sum.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n"))
	sum.Write(body)
	return body, &provisioning.Idempotency{Key: key, RequestHash: sum.Sum(nil)}, nil
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) error {
	body, key, err := readBody(w, r)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var in createBody
	if err := dec.Decode(&in); err != nil {
		return problem.BadRequest("request.invalid_body", "The request body does not match the contract.").WithCause(err)
	}
	req := provisioning.Request{Name: in.Name, Region: in.Region, GroupCurrency: in.GroupCurrency, Idempotency: key,
		Administrator: provisioning.Administrator{Subject: in.Administrator.Subject, DisplayName: in.Administrator.DisplayName, Email: in.Administrator.Email}}
	if ip := in.IdentityProvider; ip != nil {
		req.IdentityProvider = &identity.NewIdentityProvider{Issuer: ip.Issuer, Audience: ip.Audience, TenantClaim: ip.TenantClaim, ACRValues: ip.ACRValues, AMRValues: ip.AMRValues,
			CertificateBound: ip.CertificateBound}
	}
	if len(in.Configuration) > 0 {
		doc, err := config.Parse(in.Configuration)
		if err != nil {
			return problem.Unprocessable("tenancy.invalid_tenant", "The tenant request is invalid.",
				problem.FieldError{Field: "configuration", Code: "invalid", Message: err.Error()})
		}
		req.Document = doc
	}
	created, replayed, err := h.s.Create(r.Context(), operator(r.Context()), req)
	if err != nil {
		return err
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("ETag", etag.Format(created.Version))
	w.Header().Set("Location", Base+"/tenants/"+created.ID.String())
	httpapi.WriteJSON(w, http.StatusCreated, created)
	return nil
}

func (h *handler) transition(w http.ResponseWriter, r *http.Request, id uuid.UUID, verb string) error {
	t, err := h.s.Transition(r.Context(), operator(r.Context()), id, verb, r.Header.Get("If-Match"))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(t.Version))
	httpapi.WriteJSON(w, http.StatusOK, t)
	return nil
}

// rotated is the TenantKeyRotated schema: the tenant and the rotation.
type rotated struct {
	Tenant
	Rotation fieldcrypt.Rotation `json:"rotation"`
}

func (h *handler) rotateKey(w http.ResponseWriter, r *http.Request, id uuid.UUID) error {
	t, rot, err := h.s.RotateKey(r.Context(), operator(r.Context()), id, r.Header.Get("If-Match"))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(t.Version))
	httpapi.WriteJSON(w, http.StatusOK, rotated{Tenant: t, Rotation: rot})
	return nil
}

// erased is the answer of :erase.
type erased struct {
	Tenant
	Erasure offboarding.Erasure `json:"erasure"`
}

func (h *handler) erase(w http.ResponseWriter, r *http.Request, id uuid.UUID) error {
	t, er, err := h.s.Erase(r.Context(), operator(r.Context()), id, r.Header.Get("If-Match"))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(t.Version))
	httpapi.WriteJSON(w, http.StatusOK, erased{Tenant: t, Erasure: er})
	return nil
}
