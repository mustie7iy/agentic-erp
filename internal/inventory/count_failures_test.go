package inventory_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestCountDatabaseFailures: every database error of the count
// documents' operations is answered as an error, none outside a unit of
// work.
func TestCountDatabaseFailures(t *testing.T) {
	e := countSetup(t)
	h := endpoints(t)
	e.post(t, "561", e.item("10", "EA", nil))
	other := mustMaterial(t, e)
	d := e.count(t, e.bracket)
	id := uuid.MustParse(d["id"].(string))
	_, tag := e.Must(200, "GET", counts+"/"+id.String(), "", "")
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreatePhysicalInventoryDocument(ctx, &gen.PhysicalInventoryDocumentCreate{PlantID: e.p1, StorageLocationID: e.s1,
				Items: []gen.PhysicalInventoryItemInput{{MaterialID: uuid.MustParse(other)}}}, gen.CreatePhysicalInventoryDocumentParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPhysicalInventoryDocument(ctx, gen.GetPhysicalInventoryDocumentParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListPhysicalInventoryDocuments(ctx, gen.ListPhysicalInventoryDocumentsParams{})
			return err
		},
		"enter a count": func(ctx context.Context) error {
			_, err := h.EnterPhysicalInventoryCount(ctx, &gen.PhysicalInventoryCountEntry{Items: []gen.PhysicalInventoryCountInput{{ItemNo: 1,
				Quantity: gen.Quantity{Value: "9", Unit: "EA"}}}}, gen.EnterPhysicalInventoryCountParams{ID: id, IfMatch: tag})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListPhysicalInventoryDocuments(ctx, gen.ListPhysicalInventoryDocumentsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
