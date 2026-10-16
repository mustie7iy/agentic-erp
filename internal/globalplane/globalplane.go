// Package globalplane is the operators' control plane across cells
// (ADR-0023): the directory's view of every tenant and cell, provisioning
// that places a tenant in a cell of its region and forwards the request to
// that cell's control plane, and the directory steps of a tenant move
// (freeze, move, unfreeze). It is served by erp-router on its admin
// listener under /directory/v1 (api/directory.yaml) and holds no state but
// the directory.
package globalplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Service is the global control plane over the directory.
type Service struct {
	Directory *directory.Postgres
	Pages     *pagination.Codec
	Client    *http.Client // reaches the cells' control planes; nil: a client with a 30 s timeout
	// RequireTLS refuses a cell whose control URL is not https
	// (production, ADR-0033).
	RequireTLS bool
	// PeerOf is the SPIFFE ID a cell must present; nil: the client's own
	// rule.
	PeerOf func(cell string) string
}

func (s *Service) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// List returns one page of the directory's tenants.
func (s *Service) List(ctx context.Context, f directory.Filter, page pagination.Request) ([]directory.Entry, string, error) {
	if f.Status != "" && !slices.Contains([]string{directory.Active, directory.Suspended, directory.Offboarded, directory.ReadOnly}, f.Status) {
		return nil, "", problem.BadRequest("request.invalid_parameter", "Parameter status is invalid.",
			problem.FieldError{Field: "status", Code: "invalid", Message: "one of active, suspended, offboarded, read_only"})
	}
	ks, err := s.Pages.Keyset(directory.Sort, "listDirectoryTenants|"+f.Region+"|"+f.Cell+"|"+f.Status, page)
	if err != nil {
		return nil, "", err
	}
	return s.Directory.ListTenants(ctx, f, ks)
}

// Get returns one entry.
func (s *Service) Get(ctx context.Context, tenant uuid.UUID) (directory.Entry, error) {
	e, err := s.Directory.Lookup(ctx, tenant)
	if errors.Is(err, directory.ErrUnknown) {
		return directory.Entry{}, problem.NotFound("No cell serves this tenant.")
	}
	return e, err
}

// Cells lists the cells of a region (or all), fewest tenants first.
func (s *Service) Cells(ctx context.Context, region string) ([]directory.Cell, error) {
	return s.Directory.ListCells(ctx, region)
}

// Freeze marks a tenant read-only for a move: the router lets reads through
// and refuses writes. Only an active tenant freezes.
func (s *Service) Freeze(ctx context.Context, tenant uuid.UUID, ifMatch string) (directory.Entry, error) {
	return s.transition(ctx, tenant, ifMatch, directory.Active, directory.ReadOnly, "freeze applies to active tenants")
}

// Unfreeze ends a move: the tenant is active again, in whichever cell the
// directory now names.
func (s *Service) Unfreeze(ctx context.Context, tenant uuid.UUID, ifMatch string) (directory.Entry, error) {
	return s.transition(ctx, tenant, ifMatch, directory.ReadOnly, directory.Active, "unfreeze applies to read-only tenants")
}

func (s *Service) transition(ctx context.Context, tenant uuid.UUID, ifMatch, from, to, rule string) (directory.Entry, error) {
	e, err := s.checked(ctx, tenant, ifMatch)
	if err != nil {
		return directory.Entry{}, err
	}
	if e.Status != from {
		return directory.Entry{}, problem.Conflict("tenancy.invalid_transition", fmt.Sprintf("The tenant is %s; %s.", e.Status, rule))
	}
	return s.updated(s.Directory.SetStatus(ctx, tenant, e.Version, to))
}

// Move points a frozen tenant at another cell of the directory, the step of
// 05 §5.4 after its rows were copied and verified; the cell must be known.
func (s *Service) Move(ctx context.Context, tenant uuid.UUID, ifMatch, cell string) (directory.Entry, error) {
	e, err := s.checked(ctx, tenant, ifMatch)
	if err != nil {
		return directory.Entry{}, err
	}
	if e.Status != directory.ReadOnly {
		return directory.Entry{}, problem.Conflict("tenancy.invalid_transition", fmt.Sprintf("The tenant is %s; freeze it before moving it.", e.Status))
	}
	moved, err := s.Directory.Move(ctx, tenant, e.Version, cell)
	switch {
	case errors.Is(err, directory.ErrUnknownCell):
		return directory.Entry{}, problem.Unprocessable("tenancy.unknown_cell", "No cell named "+cell+" is registered.",
			problem.FieldError{Field: "cell", Code: "unknown", Message: "a registered cell"})
	case errors.Is(err, directory.ErrRegion):
		return directory.Entry{}, problem.Unprocessable("tenancy.region_mismatch", fmt.Sprintf("Cell %s is in another region; the tenant's data stays in %s.", cell, e.Region))
	}
	return s.updated(moved, err)
}

// checked reads the entry and checks If-Match against its version.
func (s *Service) checked(ctx context.Context, tenant uuid.UUID, ifMatch string) (directory.Entry, error) {
	e, err := s.Get(ctx, tenant)
	if err != nil {
		return directory.Entry{}, err
	}
	if err := etag.Check(ifMatch, e.Version); err != nil {
		return directory.Entry{}, err
	}
	return e, nil
}

// updated maps the directory's errors of an update to problems. The entry
// was read moments before, and entries are never deleted, so ErrUnknown
// cannot happen here; a version mismatch can, when another operator wrote
// between the read and the update.
func (s *Service) updated(e directory.Entry, err error) (directory.Entry, error) {
	if errors.Is(err, directory.ErrVersion) {
		return directory.Entry{}, problem.Conflict("request.version_mismatch", "The entry changed while the request ran; read it again and retry.")
	}
	return e, err
}

// Placed is what a global provisioning answers: the cell's answer, with the cell.
type Placed struct {
	Status  int
	Body    []byte
	Headers http.Header
	Cell    directory.Cell
}

// Create provisions a tenant in a cell: the request (the control plane's
// TenantCreate, with Idempotency-Key and the operator's token) is placed in
// a cell of its region and forwarded to that cell's control plane
// unchanged; the cell's answer comes back with the cell named, and a new
// tenant is entered in the directory at once.
func (s *Service) Create(ctx context.Context, key, authorization string, body []byte, region, cell string) (Placed, error) {
	chosen, err := s.Directory.Place(ctx, key, region, cell)
	if errors.Is(err, directory.ErrUnknownCell) {
		if cell != "" {
			return Placed{}, problem.Unprocessable("tenancy.unknown_cell", "No cell named "+cell+" in "+region+" is registered.",
				problem.FieldError{Field: "cell", Code: "unknown", Message: "a registered cell of the region"})
		}
		return Placed{}, problem.Unprocessable("tenancy.no_cell", "No cell of "+region+" takes new tenants.",
			problem.FieldError{Field: "region", Code: "no_cell", Message: "a region with an accepting cell"})
	}
	if err != nil {
		return Placed{}, err
	}
	status, answer, headers, err := s.call(ctx, chosen, http.MethodPost, "/control/v1/tenants", body, map[string]string{"Content-Type": "application/json", "Idempotency-Key": key, "Authorization": authorization})
	if err != nil {
		return Placed{}, err
	}
	if status == http.StatusCreated {
		var created struct {
			ID     uuid.UUID `json:"id"`
			Region string    `json:"region"`
			Status string    `json:"status"`
		}
		if json.Unmarshal(answer, &created) == nil && created.ID != uuid.Nil {
			// The cell registers the tenant too; this makes it routable before its next round.
			_ = s.Directory.Upsert(ctx, directory.Entry{TenantID: created.ID, Region: created.Region, Cell: chosen.Name, APIURL: chosen.APIURL, Status: created.Status})
		}
	}
	return Placed{Status: status, Body: answer, Headers: headers, Cell: chosen}, nil
}
