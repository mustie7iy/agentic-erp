package ledger

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
)

// expandPartners is what an entry read with expand=partners embeds
// (T73h3): the business partners its lines name, once each in the order
// of the lines that first name them, so a reader shows a line's partner
// by number and name without another call. An expansion never crosses
// permissions (04 §4.4): a caller who may not read business partners
// gets none, as do an entry whose lines name none and a read that did
// not ask. The names are as they are now, not the entry's, so the
// entry's ETag is taken before they are added.
func (e *Endpoints) expandPartners(ctx context.Context, expand []string, ids []gen.OptUUID) (gen.OptJournalEntryExpanded, error) {
	var none gen.OptJournalEntryExpanded
	if !slices.Contains(expand, "partners") {
		return none, nil
	}
	ordered := firstSeen(ids)
	if len(ordered) == 0 {
		return none, nil
	}
	held, err := authz.Permissions(ctx)
	if err != nil {
		return none, fmt.Errorf("ledger: may the caller read an entry's partners: %w", err)
	}
	if !held[permReadPartners] {
		return none, nil
	}
	names, err := e.PartnerNames.PartnerNames(ctx, ordered)
	if err != nil {
		return none, fmt.Errorf("ledger: names of an entry's partners: %w", err)
	}
	partners := make([]gen.PartnerName, 0, len(ordered))
	for _, id := range ordered {
		if n, ok := names[id]; ok {
			partners = append(partners, gen.PartnerName{ID: id, Number: n.Number, Name: n.Name})
		}
	}
	return gen.NewOptJournalEntryExpanded(gen.JournalEntryExpanded{Partners: partners}), nil
}

// permReadPartners is the permission listBusinessPartners requires, the
// one an expansion of partners requires too.
const permReadPartners = "partner.supplier.read"

// firstSeen is the ids set, each once, in the order first given.
func firstSeen(ids []gen.OptUUID) []uuid.UUID {
	out, seen := []uuid.UUID{}, map[uuid.UUID]bool{}
	for _, id := range ids {
		if v, ok := id.Get(); ok && !seen[v] {
			out, seen[v] = append(out, v), true
		}
	}
	return out
}
