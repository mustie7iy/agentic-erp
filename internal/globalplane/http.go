package globalplane

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Base is the path prefix of the contract.
const Base = "/directory/v1"

// Handler serves api/directory.yaml for operators the verifier accepts.
func Handler(s *Service, v *authn.Verifier, log *slog.Logger) http.Handler {
	h := &handler{s: s, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc(Base+"/tenants", h.tenants)
	mux.HandleFunc(Base+"/tenants/{rest}", h.tenant)
	mux.HandleFunc(Base+"/cells", h.cells)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.NotFound("No such endpoint."))
	})
	return v.OperatorMiddleware(mux)
}

type handler struct {
	s   *Service
	log *slog.Logger
}

type endpoint func(w http.ResponseWriter, r *http.Request) error

// Roles maps each operation to the operator role it needs (ADR-0027).
var Roles = map[string]string{"listDirectoryTenants": authn.RoleRead, "getDirectoryTenant": authn.RoleRead, "listCells": authn.RoleRead, "verifyTenant": authn.RoleRead, "createDirectoryTenant": authn.RoleProvision}

func required(operation string) string {
	if role, ok := Roles[operation]; ok {
		return role
	}
	return authn.RoleOperate
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request, operation string, e endpoint) {
	reqctx.InfoFrom(r.Context()).Operation = operation
	if claims, _ := reqctx.ClaimsFrom(r.Context()); !authn.OperatorAllowed(claims, required(operation)) {
		problem.Write(w, r, problem.Forbidden("authz.operator_role", "This operation needs the operator role "+required(operation)+"."))
		return
	}
	if err := e(w, r); err != nil {
		p := problem.From(err)
		if p.Status >= 500 {
			h.log.ErrorContext(r.Context(), "directory request failed", "operation", operation, "error", err, "trace_id", reqctx.TraceID(r.Context()))
		}
		problem.Write(w, r, p)
	}
}

func notAllowed(w http.ResponseWriter, r *http.Request, allowed string) {
	w.Header().Set("Allow", allowed)
	problem.Write(w, r, problem.New(http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed", "Allowed: "+allowed+"."))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status is sent; a failed write cannot be reported
}

func (h *handler) tenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.serve(w, r, "listDirectoryTenants", h.list)
	case http.MethodPost:
		h.serve(w, r, "createDirectoryTenant", h.create)
	default:
		notAllowed(w, r, "GET, POST")
	}
}

func (h *handler) cells(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		notAllowed(w, r, "GET")
		return
	}
	h.serve(w, r, "listCells", func(w http.ResponseWriter, r *http.Request) error {
		cells, err := h.s.Cells(r.Context(), r.URL.Query().Get("region"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": cells}) // never nil: pgx collects into an empty slice
		return nil
	})
}

func (h *handler) tenant(w http.ResponseWriter, r *http.Request) {
	raw, verb, custom := strings.Cut(r.PathValue("rest"), ":")
	id, err := uuid.Parse(raw)
	if err != nil {
		problem.Write(w, r, problem.BadRequest("request.invalid_parameter", "Parameter id is invalid.",
			problem.FieldError{Field: "id", Code: "invalid", Message: "a UUID"}))
		return
	}
	ifMatch := r.Header.Get("If-Match")
	switch {
	case r.Method == http.MethodGet && !custom:
		h.serve(w, r, "getDirectoryTenant", func(w http.ResponseWriter, r *http.Request) error {
			e, err := h.s.Get(r.Context(), id)
			return h.entry(w, e, err)
		})
	case r.Method == http.MethodPost && custom && verb == "freeze":
		h.serve(w, r, "freezeTenant", func(w http.ResponseWriter, r *http.Request) error {
			e, err := h.s.Freeze(r.Context(), id, ifMatch)
			return h.entry(w, e, err)
		})
	case r.Method == http.MethodPost && custom && verb == "unfreeze":
		h.serve(w, r, "unfreezeTenant", func(w http.ResponseWriter, r *http.Request) error {
			e, err := h.s.Unfreeze(r.Context(), id, ifMatch)
			return h.entry(w, e, err)
		})
	case r.Method == http.MethodPost && custom && verb == "move":
		h.serve(w, r, "moveTenant", func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Cell string `json:"cell"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil || in.Cell == "" {
				return problem.BadRequest("request.invalid_body", "The request body does not match the contract: a cell is required.")
			}
			e, err := h.s.Move(r.Context(), id, ifMatch, in.Cell)
			return h.entry(w, e, err)
		})
	case r.Method == http.MethodPost && custom && verb == "verify":
		h.serve(w, r, "verifyTenant", func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Cell string `json:"cell"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil || in.Cell == "" {
				return problem.BadRequest("request.invalid_body", "The request body does not match the contract: a cell is required.")
			}
			v, err := h.s.Verify(r.Context(), id, in.Cell, r.Header.Get("Authorization"))
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, v)
			return nil
		})
	case custom:
		if r.Method == http.MethodPost {
			problem.Write(w, r, problem.NotFound("No such method on tenants: "+verb+"."))
			return
		}
		notAllowed(w, r, "POST")
	default:
		notAllowed(w, r, "GET")
	}
}

func (h *handler) entry(w http.ResponseWriter, e directory.Entry, err error) error {
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag.Format(e.Version))
	writeJSON(w, http.StatusOK, e)
	return nil
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	page := pagination.Request{OrderBy: q.Get("order_by"), PageToken: q.Get("page_token"), PageTokenSet: q.Has("page_token")}
	if q.Has("page_size") {
		n, err := strconv.ParseInt(q.Get("page_size"), 10, 32)
		if err != nil {
			return problem.BadRequest("request.invalid_parameter", "Parameter page_size is invalid.",
				problem.FieldError{Field: "page_size", Code: "invalid", Message: "an integer"})
		}
		page.PageSize, page.PageSizeSet = int32(n), true
	}
	entries, next, err := h.s.List(r.Context(), directory.Filter{Region: q.Get("region"), Cell: q.Get("cell"), Status: q.Get("status")}, page)
	if err != nil {
		return err
	}
	out := map[string]any{"items": entries} // never nil: pgx collects into an empty slice
	if next != "" {
		out["next_page_token"] = next
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// createBody is the part of TenantCreate the placement needs.
type createBody struct {
	Region string `json:"region"`
	Cell   string `json:"cell"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) error {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return problem.PreconditionRequired("Idempotency-Key")
	}
	if len(key) < 8 || len(key) > 255 {
		return problem.BadRequest("request.invalid_idempotency_key", "Idempotency-Key must be 8 to 255 characters.",
			problem.FieldError{Field: "Idempotency-Key", Code: "invalid_length", Message: "8 to 255 characters"})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, uow.MaxBody))
	if err != nil {
		return problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large", "The request body exceeds 5 MB.").WithCause(err)
	}
	var in createBody
	if err := json.Unmarshal(body, &in); err != nil || in.Region == "" {
		return problem.BadRequest("request.invalid_body", "The request body does not match the contract: a region is required.")
	}
	// The cell's control plane does not know the placement field.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields) // it parsed above
	delete(fields, "cell")
	forwarded, _ := json.Marshal(fields) // raw messages always encode
	placed, err := h.s.Create(r.Context(), key, r.Header.Get("Authorization"), forwarded, in.Region, in.Cell)
	if err != nil {
		return err
	}
	for _, name := range []string{"ETag", "Location", "Idempotent-Replayed", "Content-Type"} {
		if v := placed.Headers.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.Header().Set("X-Erp-Cell", placed.Cell.Name)
	if placed.Status == http.StatusCreated {
		var created map[string]any
		if json.Unmarshal(placed.Body, &created) == nil {
			created["cell"] = placed.Cell.Name
			writeJSON(w, placed.Status, created)
			return nil
		}
	}
	w.WriteHeader(placed.Status)
	_, _ = io.Copy(w, bytes.NewReader(placed.Body))
	return nil
}
