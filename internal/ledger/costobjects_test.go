package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// TestCostObjectReferences: a cost object custom field's center is judged
// for the document's company and date as a posting judges it, without its
// locks (ADR-0046 As built T56k).
func TestCostObjectReferences(t *testing.T) {
	a := setupAssigned(t)
	pending, _ := a.Must(201, "POST", "/v1/profit-centers", a.pcBody(fmt.Sprintf(`{"code": "PCPEND", "name": "PC pending", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, a.de)), "")
	pcPending := uuid.MustParse(pending["id"].(string))
	cc, pc := extensions.CostCenter, extensions.ProfitCenter
	for name, c := range map[string]struct {
		kind extensions.Kind
		id   uuid.UUID
		le   uuid.UUID
		day  string
		want extensions.CostObjectState
	}{
		"a cost center valid on the date":       {cc, a.cc1000, a.de, "2026-05-10", extensions.CostObjectValid},
		"a cost center locked for costs":        {cc, a.cc2000, a.de, "2026-02-10", extensions.CostObjectValid},
		"a cost center not begun":               {cc, a.cc3000, a.de, "2026-05-31", extensions.CostObjectNotValidOnDate},
		"a cost center from its first day":      {cc, a.cc3000, a.de, "2026-06-01", extensions.CostObjectValid},
		"a pending cost center":                 {cc, a.ccPending, a.de, "2026-05-10", extensions.CostObjectNotActive},
		"another company's cost center":         {cc, a.ccUS, a.de, "2026-05-10", extensions.CostObjectNotInCompany},
		"an unknown cost center":                {cc, uuid.New(), a.de, "2026-05-10", extensions.CostObjectNotFound},
		"a profit center's id as a cost center": {cc, a.pc1, a.de, "2026-05-10", extensions.CostObjectNotFound},
		"a profit center valid on the date":     {pc, a.pc1, a.de, "2026-05-10", extensions.CostObjectValid},
		"a locked profit center":                {pc, a.pc1, a.de, "2026-03-10", extensions.CostObjectValid},
		"a profit center ended":                 {pc, a.pcOld, a.de, "2026-04-01", extensions.CostObjectNotValidOnDate},
		"a profit center on its last day":       {pc, a.pcOld, a.de, "2026-03-31", extensions.CostObjectValid},
		"a pending profit center":               {pc, pcPending, a.de, "2026-05-10", extensions.CostObjectNotActive},
		"a profit center of another company":    {pc, a.pcUS, a.de, "2026-05-10", extensions.CostObjectNotInCompany},
		"a cost center's id as a profit center": {pc, a.cc1000, a.de, "2026-05-10", extensions.CostObjectNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			var got extensions.CostObjectState
			err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
				got, err = ledger.CostObjectReferences{}.CostObject(ctx, c.kind, c.le, date(c.day), c.id)
				return err
			})
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCostObjectReferencesFailures: a kind the ledger holds no record of,
// a question outside a request and every failed read are errors, never a
// valid reference.
func TestCostObjectReferencesFailures(t *testing.T) {
	a := setupAssigned(t)
	refs := ledger.CostObjectReferences{}
	if _, err := refs.CostObject(context.Background(), extensions.CostCenter, a.de, date("2026-05-10"), a.cc1000); err == nil {
		t.Error("outside a request: got no error, want one")
	}
	if err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		_, err := refs.CostObject(ctx, extensions.Partner, a.de, date("2026-05-10"), a.cc1000)
		return err
	}); err == nil {
		t.Error("a partner: got no error, want one")
	}
	for kind, id := range map[extensions.Kind]uuid.UUID{extensions.CostCenter: a.cc1000, extensions.ProfitCenter: a.pc1} {
		apitest.EveryFailure(t, a.ten, a.ten.Human, string(kind), func(ctx context.Context) error {
			_, err := refs.CostObject(ctx, kind, a.de, date("2026-05-10"), id)
			return err
		})
	}
}
