package ledger

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
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
// once for all the items. An id that names nothing is left unnamed, and
// so is every partner for a caller who may not read business partners
// (partnerNames).
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
	names, err := e.partnerNames(ctx, partners)
	if err != nil {
		return err
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

// partnerNames are the partners' numbers and names, none for a caller who
// may not read business partners: a name embedded in another context's
// resource is a copy of the partner, and a copy never crosses permissions
// (04 §4.4). The item keeps its partner_id, which is the revaluation's
// own; the run is answered, not refused, as it is the caller's to read.
func (e *Endpoints) partnerNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PartnerName, error) {
	may, err := authz.Holds(ctx, permReadPartners)
	if err != nil {
		return nil, fmt.Errorf("ledger: may the caller read the revaluation's partners: %w", err)
	}
	if !may {
		return nil, nil
	}
	names, err := e.PartnerNames.PartnerNames(ctx, distinct(ids))
	if err != nil {
		return nil, fmt.Errorf("ledger: names of the revaluation's partners: %w", err)
	}
	return names, nil
}

// permReadPartners is the permission reading business partners requires
// (the partner context's ReadPermission, which the ledger may not import).
const permReadPartners = "partner.supplier.read"

// distinct is ids sorted, each once.
func distinct(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}
