package ledger

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestGroupClaimed: claimed items are grouped as Unreported groups them,
// by direction, kind, component, jurisdiction (none apart from any),
// account key and, of a prior-period item, its tax date, and listed in key
// order; sums beyond an amount fail rather than wrap.
func TestGroupClaimed(t *testing.T) {
	ca, before := "CA", time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	a, b := uuid.MustParse("00000000-0000-7000-8000-000000000001"), uuid.MustParse("00000000-0000-7000-8000-000000000002")
	item := func(year int16, entry uuid.UUID, no int32, jurisdiction *string, prior *time.Time, tax int64) claimed {
		return claimed{ClaimedItem: ClaimedItem{FiscalYear: year, EntryID: entry, ItemNo: no, TaxLC: money.Minor(tax)},
			group: TaxItemGroup{Direction: "output", Kind: "standard", Component: "gst", Jurisdiction: jurisdiction, AccountKey: "output", PriorTaxDate: prior}}
	}
	groups, items, err := groupClaimed([]claimed{item(2026, b, 1, &ca, nil, -5), item(2026, a, 2, nil, nil, -3), item(2025, b, 1, &ca, nil, -2),
		item(2026, a, 1, &ca, &before, -7), item(2026, a, 3, &ca, nil, -1)})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, it := range items {
		keys = append(keys, fmt.Sprintf("%d/%s/%d", it.FiscalYear, it.EntryID.String()[35:], it.ItemNo))
	}
	if got, want := len(keys), 5; got != want {
		t.Fatalf("items: got %d, want %d", got, want)
	}
	if got, want := keys[0]+" "+keys[1]+" "+keys[2], "2025/2/1 2026/1/1 2026/1/2"; got != want {
		t.Errorf("key order: got %s, want %s", got, want)
	}
	if got, want := len(groups), 3; got != want {
		t.Fatalf("groups: got %d, want %d (CA, none, CA prior)", got, want)
	}
	if g := groups[0]; g.Items != 3 || g.TaxLC != -8 || g.PriorPeriod {
		t.Errorf("CA group: got %+v, want 3 items of -8, not prior", g)
	}
	if g := groups[1]; !g.PriorPeriod || g.Items != 1 {
		t.Errorf("prior group: got %+v, want 1 prior item", g)
	}
	if g := groups[2]; g.Jurisdiction != nil || g.TaxLC != -3 {
		t.Errorf("group without jurisdiction: got %+v, want -3 apart", g)
	}
	big := item(2026, a, 9, nil, nil, math.MaxInt64)
	for name, c := range map[string]claimed{
		"base":        {ClaimedItem: ClaimedItem{BaseLC: math.MaxInt64}, group: big.group},
		"tax":         big,
		"recoverable": {ClaimedItem: ClaimedItem{RecoverableLC: math.MaxInt64}, group: big.group},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := groupClaimed([]claimed{c, c}); err == nil {
				t.Error("got no error, want the sum out of range")
			}
		})
	}
}
