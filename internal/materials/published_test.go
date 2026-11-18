package materials_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/materials"
)

// TestPublished: the published API reads materials by id in the caller's
// transaction, whatever companies the caller may act in, and answers
// whether one is kept in stock and at a storage location.
func TestPublished(t *testing.T) {
	e := setup(t)
	m := e.create(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}})
	id := uuid.MustParse(m["id"].(string))
	if _, err := (materials.Materials{}).Materials(context.Background(), []uuid.UUID{id}); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	reader := e.Reader(t, "Reader of 2000", e.DE)
	var got map[uuid.UUID]materials.Material
	if err := apitest.InRequest(t, e.Tenant, reader, func(ctx context.Context) error {
		var err error
		got, err = materials.Materials{}.Materials(ctx, []uuid.UUID{id, uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b, ok := got[id]
	switch {
	case len(got) != 1 || !ok:
		t.Fatalf("got %v, want the material alone", got)
	case !b.Stocked():
		t.Error("a trading good: got not stocked, want stocked")
	case !b.KeptAt(e.p2, e.s3) || b.KeptAt(e.p2, e.s1) || b.KeptAt(e.p1, e.s1):
		t.Errorf("kept at: got %v, want plant P2's S3 alone, though the reader may not act in company 1000", b.Plants)
	}
	b.Type = "service"
	if b.Stocked() {
		t.Error("a service: got stocked, want not stocked")
	}
}

// TestConvert: a quantity in an alternative unit is its numerator /
// denominator base units, exactly, and refused when it is not exact in
// six decimals or too large; the base unit converts to itself.
func TestConvert(t *testing.T) {
	m := materials.Material{Number: "MA1", BaseUnit: "EA", Units: []materials.Unit{{Code: "BOX", Numerator: 20, Denominator: 1},
		{Code: "KG", Numerator: 1, Denominator: 3}, {Code: "G", Numerator: 1, Denominator: 1000000}}}
	for _, c := range []struct {
		name, qty, unit, want string
		err                   error
	}{
		{"the base unit", "2.5", "EA", "2.5", nil},
		{"boxes", "1.5", "BOX", "30", nil},
		{"thirds", "3", "KG", "1", nil},
		{"a third", "1", "KG", "", materials.ErrInexact},
		{"a millionth", "1", "G", "0.000001", nil},
		{"less than a millionth", "0.5", "G", "", materials.ErrInexact},
		{"more than fourteen digits", "99999999999999", "BOX", "", materials.ErrInexact},
		{"a unit it has not", "1", "PC", "", materials.ErrUnknownUnit},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, _ := new(big.Rat).SetString(c.qty)
			got, err := m.Convert(q, c.unit)
			if !errors.Is(err, c.err) {
				t.Fatalf("error: got %v, want %v", err, c.err)
			}
			if err == nil && got.Cmp(mustRat(t, c.want)) != 0 {
				t.Errorf("got %s, want %s", got.FloatString(6), c.want)
			}
		})
	}
}

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("%q is no decimal", s)
	}
	return r
}
