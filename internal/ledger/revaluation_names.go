package ledger

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
)

// PartnerNames names business partners by number and name (partner
// context): a revaluation's open items show their supplier or customer
// beside its id (T73i2), so a reader needs no call per partner.
type PartnerNames interface {
	PartnerNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PartnerName, error)
}

// PartnerName is what a partner is known by.
type PartnerName struct{ Number, Name string }

// nameItems names a revaluation's open items: each partner by its number
// and name as they are now, and each entry by its document number, read
// once for all the items. An id that names nothing is left unnamed.
func (e *Endpoints) nameItems(ctx context.Context, items []gen.RevaluationItem) error {
	var partners, entries []uuid.UUID
	for _, it := range items {
		if id, ok := it.PartnerID.Get(); ok {
			partners = append(partners, id)
		}
		if id, ok := it.JournalEntryID.Get(); ok {
			entries = append(entries, id)
		}
	}
	if len(entries) == 0 {
		return nil // balances alone: an open item is all that has a partner or an entry
	}
	names, err := e.PartnerNames.PartnerNames(ctx, distinct(partners))
	if err != nil {
		return fmt.Errorf("ledger: names of the revaluation's partners: %w", err)
	}
	numbers, err := OpenItems{}.Numbers(ctx, distinct(entries))
	if err != nil {
		return err
	}
	for i := range items {
		if n, ok := names[items[i].PartnerID.Value]; ok && items[i].PartnerID.Set {
			items[i].PartnerNumber.SetTo(n.Number)
			items[i].PartnerName.SetTo(n.Name)
		}
		if n, ok := numbers[items[i].JournalEntryID.Value]; ok && items[i].JournalEntryID.Set {
			items[i].DocumentNumber.SetTo(n)
		}
	}
	return nil
}

// distinct is ids sorted, each once.
func distinct(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}
