package materials_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDuplicates: a material with another's description, white space and
// case aside, is refused unless confirmed; confirmed, its create change
// lists the others. A material whose create change was rejected is no
// duplicate, and stays pending.
func TestDuplicates(t *testing.T) {
	e := setup(t)
	first := e.active(t, nil)
	got := e.Problem(409, "materials.duplicate_description", "POST", path, e.bracket(t, map[string]any{"description": "  machined   BRACKET m10 "}), "")
	if detail := got["detail"].(string); !strings.Contains(detail, "MA2000-100000 Machined bracket M10") {
		t.Errorf("detail: got %q, want it to name MA2000-100000", detail)
	}
	second := e.create(t, map[string]any{"description": "Machined bracket  M10", "confirm_duplicate": true})
	c, _ := e.Must(200, "GET", "/v1/material-changes/"+second["pending_change_id"].(string), "", "")
	dups := rows(t, c, "possible_duplicates")
	if len(dups) != 1 || dups[0]["material_id"] != first["id"] || dups[0]["number"] != "MA2000-100000" {
		t.Errorf("possible duplicates: got %v, want the first material", dups)
	}
	r := e.Pending(t, second["pending_change_id"].(string))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "A duplicate"}`, `"1"`)
	rejected, _ := e.Must(200, "GET", path+"/"+second["id"].(string), "", "")
	fields(t, rejected, map[string]any{"status": "pending", "pending_change_id": nil})
	e.Problem(409, "materials.invalid_status", "PATCH", path+"/"+second["id"].(string), `{"description": "Other"}`, `"1"`)
	third := e.create(t, map[string]any{"description": "Machined bracket m10", "confirm_duplicate": true})
	c, _ = e.Must(200, "GET", "/v1/material-changes/"+third["pending_change_id"].(string), "", "")
	if dups := rows(t, c, "possible_duplicates"); len(dups) != 1 || dups[0]["number"] != "MA2000-100000" {
		t.Errorf("after the rejection: got %v, want the first material alone", dups)
	}
}

// TestDuplicatesAtOnce: two materials of one description created at once
// wait for one another, so the second finds the first and is refused.
func TestDuplicatesAtOnce(t *testing.T) {
	e := setup(t)
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", path, e.bracket(t, map[string]any{"description": "Spacer ring"}), "")
		}()
	}
	wg.Wait()
	if created, refused := statuses[0] == 201 && statuses[1] == 409, statuses[0] == 409 && statuses[1] == 201; !created && !refused {
		t.Errorf("statuses: got %v, want one created and one refused", statuses)
	}
}

// TestDecideOnce: a change applied, rejected or withdrawn is decided
// once; deciding it again is refused and changes nothing (the approvals
// context asking twice).
func TestDecideOnce(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	m := e.active(t, nil)
	page, _ := e.Must(200, "GET", "/v1/material-changes?material_id="+m["id"].(string), "", "")
	change := uuid.MustParse(rows(t, page, "items")[0]["id"].(string))
	decide := materials.Approvals{Endpoints: h}
	for name, call := range map[string]func(ctx context.Context) error{
		"approved again":         func(ctx context.Context) error { return decide.Approved(ctx, change, uuid.Nil) },
		"rejected once applied":  func(ctx context.Context) error { return decide.Rejected(ctx, change, uuid.Nil, "No") },
		"withdrawn once applied": func(ctx context.Context) error { return decide.Cancelled(ctx, change, uuid.Nil, "No") },
		"no such change":         func(ctx context.Context) error { return decide.Approved(ctx, uuid.New(), uuid.Nil) },
	} {
		t.Run(name, func(t *testing.T) {
			err := e.inRequest(t, call)
			want := "materials.invalid_status"
			if name == "no such change" {
				want = "request.not_found"
			}
			if p := problem.From(err); p == nil || p.Code != want {
				t.Errorf("got %v, want %s", err, want)
			}
		})
	}
	again, _ := e.Must(200, "GET", path+"/"+m["id"].(string), "", "")
	fields(t, again, map[string]any{"status": "active", "version": 2})
	paths, err := decide.Resources(context.Background(), []uuid.UUID{change})
	if err != nil || paths[change] != "/v1/material-changes/"+change.String() {
		t.Errorf("resource: got %v %v, want the change's path", paths, err)
	}
}

// noApproval opens no request: the configuration needs no approval.
type noApproval struct{}

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

// TestNoApprovalNeeded: a change no approval is needed for is applied when
// it is requested.
func TestNoApprovalNeeded(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	h.Approvals = noApproval{}
	err := e.inRequest(t, func(ctx context.Context) error {
		m, err := h.Create(ctx, materials.Input{LegalEntityID: e.DE, Description: "Washer", BaseUnit: "EA", Type: "trading_good", Group: "B"})
		if err != nil {
			return err
		}
		if m.Status != materials.Active || m.PendingChangeID != nil {
			t.Errorf("created: got %s, pending %v, want active and nothing pending", m.Status, m.PendingChangeID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestChangeList: changes newest first, filtered by material, kind and
// status, paged.
func TestChangeList(t *testing.T) {
	e := setup(t)
	a := e.active(t, nil)["id"].(string)
	b := e.create(t, map[string]any{"description": "Copper wire"})["id"].(string)
	e.Must(200, "PATCH", path+"/"+a, `{"status": "blocked"}`, `"2"`)
	numbers := func(page map[string]any) string {
		var out []string
		for _, c := range rows(t, page, "items") {
			out = append(out, c["kind"].(string)+":"+c["status"].(string))
		}
		return strings.Join(out, " ")
	}
	for name, tc := range map[string]struct {
		query string
		want  string
	}{
		"all":         {"", "block:applied create:pending create:applied"},
		"a material":  {"material_id=" + a, "block:applied create:applied"},
		"by kind":     {"kind=create", "create:pending create:applied"},
		"by status":   {"status=pending&material_id=" + b, "create:pending"},
		"an old kind": {"kind=delete", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.Must(200, "GET", "/v1/material-changes?"+tc.query, "", "")
			if n := numbers(got); n != tc.want {
				t.Errorf("got %q, want %q", n, tc.want)
			}
		})
	}
	first, _ := e.Must(200, "GET", "/v1/material-changes?page_size=2", "", "")
	next, _ := e.Must(200, "GET", "/v1/material-changes?page_size=2&page_token="+url.QueryEscape(first["next_page_token"].(string)), "", "")
	if got := numbers(first) + " | " + numbers(next); got != "block:applied create:pending | create:applied" {
		t.Errorf("pages: got %q, want two and one", got)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/material-changes/"+uuid.NewString(), "", "")
}
