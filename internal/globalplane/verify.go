package globalplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Cross-cell verification (T25, ADR-0030): step 3 of a move (05 §5.4). The
// global plane asks the tenant's cell and the destination for the tenant's
// fingerprint (every table's rows and the audit chain head, taken by each
// cell's control plane in one snapshot) and names every difference. Equal
// fingerprints are the operator's evidence that the copy is complete before
// the entry is switched.

// Side is one cell's view of the tenant.
type Side struct {
	Cell        string                   `json:"cell"`
	Fingerprint *offboarding.Fingerprint `json:"fingerprint"` // null when the cell has no such tenant
}

// Verification compares the tenant's data in two cells.
type Verification struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	Source      Side      `json:"source"`
	Destination Side      `json:"destination"`
	Equal       bool      `json:"equal"`
	Differences []string  `json:"differences"`
}

// Verify compares the tenant's fingerprint in its cell with the one in the
// cell named, both fetched with the operator's token. The tenant's entry
// may be in any status: an operator verifies before freezing (to see how
// far the copy is) and after (to switch).
func (s *Service) Verify(ctx context.Context, tenant uuid.UUID, cell, authorization string) (Verification, error) {
	e, err := s.Get(ctx, tenant)
	if err != nil {
		return Verification{}, err
	}
	if cell == e.Cell {
		return Verification{}, problem.Unprocessable("tenancy.same_cell", "The tenant is in "+cell+"; name the cell it is being copied to.",
			problem.FieldError{Field: "cell", Code: "same_cell", Message: "another cell of the region"})
	}
	cells, err := s.Directory.ListCells(ctx, e.Region)
	if err != nil {
		return Verification{}, err
	}
	var source, destination *directory.Cell
	for i := range cells {
		switch cells[i].Name {
		case e.Cell:
			source = &cells[i]
		case cell:
			destination = &cells[i]
		}
	}
	if destination == nil {
		return Verification{}, problem.Unprocessable("tenancy.unknown_cell", "No cell named "+cell+" in "+e.Region+" is registered.",
			problem.FieldError{Field: "cell", Code: "unknown", Message: "a registered cell of the region"})
	}
	if source == nil {
		return Verification{}, problem.Unavailable("The tenant's cell "+e.Cell+" has not registered itself; its control plane is unknown.", 60)
	}
	v := Verification{TenantID: tenant, Source: Side{Cell: e.Cell}, Destination: Side{Cell: cell}, Differences: []string{}}
	if v.Source.Fingerprint, err = s.fingerprint(ctx, *source, tenant, authorization); err != nil {
		return Verification{}, err
	}
	if v.Destination.Fingerprint, err = s.fingerprint(ctx, *destination, tenant, authorization); err != nil {
		return Verification{}, err
	}
	v.Differences = differences(v.Source, v.Destination)
	v.Equal = len(v.Differences) == 0
	return v, nil
}

// fingerprint fetches the tenant's fingerprint from a cell's control plane;
// nil when the cell has no such tenant.
func (s *Service) fingerprint(ctx context.Context, cell directory.Cell, tenant uuid.UUID, authorization string) (*offboarding.Fingerprint, error) {
	status, body, _, err := s.call(ctx, cell, http.MethodGet, "/control/v1/tenants/"+tenant.String()+"/fingerprint", nil, map[string]string{"Authorization": authorization})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var answer struct {
			Fingerprint *offboarding.Fingerprint `json:"fingerprint"`
		}
		if json.Unmarshal(body, &answer) != nil || answer.Fingerprint == nil {
			return nil, problem.New(http.StatusBadGateway, "platform.cell_unreachable", "Cell unreachable", "Cell "+cell.Name+" did not answer with a fingerprint.")
		}
		return answer.Fingerprint, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, problem.New(http.StatusBadGateway, "platform.cell_refused", "Cell refused", fmt.Sprintf("Cell %s answered %d to the fingerprint request.", cell.Name, status))
}

// differences names what differs between two sides: a missing tenant, a
// table's rows, the audit chain head.
func differences(a, b Side) []string {
	out := []string{}
	switch {
	case a.Fingerprint == nil && b.Fingerprint == nil:
		return []string{"neither " + a.Cell + " nor " + b.Cell + " has the tenant"}
	case a.Fingerprint == nil:
		return []string{a.Cell + " has no tenant with this id"}
	case b.Fingerprint == nil:
		return []string{b.Cell + " has no tenant with this id"}
	}
	rows := map[string][2]*int64{}
	var names []string
	for _, t := range a.Fingerprint.Tables {
		n := t.Rows
		rows[t.Name] = [2]*int64{&n, nil}
		names = append(names, t.Name)
	}
	for _, t := range b.Fingerprint.Tables {
		n := t.Rows
		r, seen := rows[t.Name]
		if !seen {
			names = append(names, t.Name)
		}
		r[1] = &n
		rows[t.Name] = r
	}
	for _, name := range names {
		r := rows[name]
		switch {
		case r[0] == nil:
			out = append(out, fmt.Sprintf("%s: no table in %s, %d rows in %s", name, a.Cell, *r[1], b.Cell))
		case r[1] == nil:
			out = append(out, fmt.Sprintf("%s: %d rows in %s, no table in %s", name, *r[0], a.Cell, b.Cell))
		case *r[0] != *r[1]:
			out = append(out, fmt.Sprintf("%s: %d rows in %s, %d in %s", name, *r[0], a.Cell, *r[1], b.Cell))
		}
	}
	if h := headDifference(a, b); h != "" {
		out = append(out, h)
	}
	return out
}

func headDifference(a, b Side) string {
	x, y := a.Fingerprint.ChainHead, b.Fingerprint.ChainHead
	switch {
	case x == nil && y == nil:
		return ""
	case x == nil:
		return fmt.Sprintf("audit chain head: none in %s, seq %d in %s", a.Cell, y.Seq, b.Cell)
	case y == nil:
		return fmt.Sprintf("audit chain head: seq %d in %s, none in %s", x.Seq, a.Cell, b.Cell)
	case x.Seq != y.Seq || x.Hash != y.Hash:
		return fmt.Sprintf("audit chain head: seq %d (%s) in %s, seq %d (%s) in %s", x.Seq, short(x.Hash), a.Cell, y.Seq, short(y.Hash), b.Cell)
	}
	return ""
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}

// call sends one request to a cell's control plane and reads its answer
// whole; the cell not answering is reported as 502, a control URL that
// cannot be used as 503.
func (s *Service) call(ctx context.Context, cell directory.Cell, method, path string, body []byte, headers map[string]string) (int, []byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cell.ControlURL, "/")+path, reader) // #nosec G704 -- the cell's control URL comes from the directory, registered by the cells themselves; the path is the contract's with a parsed UUID
	if err != nil {
		return 0, nil, nil, problem.Unavailable("Cell "+cell.Name+" has no usable control URL.", 60).WithCause(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req) // #nosec G704 -- see above
	if err != nil {
		return 0, nil, nil, problem.New(http.StatusBadGateway, "platform.cell_unreachable", "Cell unreachable", "Cell "+cell.Name+" did not answer; try again.").WithCause(err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, uow.MaxBody))
	if err != nil {
		return 0, nil, nil, problem.New(http.StatusBadGateway, "platform.cell_unreachable", "Cell unreachable", "Cell "+cell.Name+" did not finish its answer; try again.").WithCause(err)
	}
	return resp.StatusCode, answer, resp.Header, nil
}
