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

// TestByNumber: the published API reads materials by number, keyed by
// it; a number of no material has no entry.
func TestByNumber(t *testing.T) {
	e := setup(t)
	m := e.create(t, nil)
	number := m["number"].(string)
	var got map[string]materials.Material
	if err := e.inRequest(t, func(ctx context.Context) error {
		var err error
		got, err = materials.Materials{}.ByNumber(ctx, []string{number, "NONE"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[number].ID.String() != m["id"] {
		t.Errorf("got %v, want material %s alone", got, number)
	}
}

// TestForItem: an item naming a material is keyed by its number, takes
// its description when it gives none, and is in one of its units, a
// quantity converting exactly; a material not active is refused on a new
// item, not on a kept one.
func TestForItem(t *testing.T) {
	m := materials.Material{Number: "MA1", Description: "Bracket", Status: materials.Active, BaseUnit: "EA",
		Units: []materials.Unit{{Code: "KG", Numerator: 1, Denominator: 3}}}
	blocked := m
	blocked.Status = materials.Blocked
	other, own := "OTHER", "MA1"
	for _, c := range []struct {
		name                 string
		m                    materials.Material
		key                  *string
		description, q, unit string
		added                bool
		want                 materials.ItemText
		err                  error
	}{
		{"by its material alone", m, nil, "", "3", "KG", true, materials.ItemText{Key: "MA1", Description: "Bracket"}, nil},
		{"with its own key and text", m, &own, "Loose", "1", "EA", true, materials.ItemText{Key: "MA1", Description: "Loose"}, nil},
		{"a unit alone", m, nil, "", "", "KG", true, materials.ItemText{Key: "MA1", Description: "Bracket"}, nil},
		{"a quantity no decimal", m, nil, "", "x", "KG", true, materials.ItemText{Key: "MA1", Description: "Bracket"}, nil},
		{"another key", m, &other, "", "1", "EA", true, materials.ItemText{}, materials.ErrOtherKey},
		{"blocked, new", blocked, nil, "", "1", "EA", true, materials.ItemText{}, materials.ErrNotUsable},
		{"blocked, kept", blocked, nil, "", "1", "EA", false, materials.ItemText{Key: "MA1", Description: "Bracket"}, nil},
		{"inexact", m, nil, "", "1", "KG", true, materials.ItemText{}, materials.ErrInexact},
		{"a unit it has not", m, nil, "", "", "M", true, materials.ItemText{}, materials.ErrUnknownUnit},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.m.ForItem(c.key, c.description, c.q, c.unit, c.added)
			if !errors.Is(err, c.err) {
				t.Fatalf("error: got %v, want %v", err, c.err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	for _, c := range []struct{ name, q, unit, want string }{
		{"inexact", "1", "KG", "1 KG does not convert exactly to material MA1's base unit EA in six decimals."},
		{"a unit it has not", "", "M", "M is no unit of material MA1 (its base unit EA or an alternative unit)."},
	} {
		t.Run("why "+c.name, func(t *testing.T) {
			_, err := m.ForItem(nil, "", c.q, c.unit, true)
			if got := m.UnitRefusal(c.q, c.unit, err); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
