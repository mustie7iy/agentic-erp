package partner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Personal data blocked (ADR-0079 §11, T100b): the privacy officer blocks
// a partner's personal data once nothing is open for it. The partner is
// blocked with it, so the posting checks in place refuse new documents,
// and the partner context's reads, and the names other contexts take from
// it, answer its personal data masked to a caller without
// ReadBlockedPermission. Masking is at the source only: what another
// context stored on its own document, and events and exports written
// before, stay as they are.

// ReadBlockedPermission is the permission that reads a partner's
// personal data while it is blocked.
const ReadBlockedPermission = "partner.privacy.read_blocked"

// The markers masked personal data reads as: text, and the last four
// characters of an account or a tax identifier (their pattern allows
// capitals and digits only).
const (
	MaskedText  = "[blocked]"
	MaskedLast4 = "XXXX"
)

// OpenItems is what the partner context asks the ledger about a partner's
// open items (ledger.OpenItems).
type OpenItems interface {
	// OpenCompanies returns the legal entities in which the partner has an
	// open item, by id.
	OpenCompanies(ctx context.Context, partnerID uuid.UUID) ([]uuid.UUID, error)
}

// personalDataBlocked refuses a change of a partner whose personal data
// is blocked.
func personalDataBlocked(id uuid.UUID) error {
	return problem.Conflict("partner.personal_data_blocked", "The personal data of business partner "+id.String()+
		" is blocked; it is not changed or unblocked until the block is lifted (unblockPersonalData).")
}

// openForChange holds the partner FOR NO KEY UPDATE to the end of the
// transaction and refuses one whose personal data is blocked: a block,
// which takes the partner FOR UPDATE and refuses a partner with a pending
// change, then waits for this change and finds it (ADR-0008). Not FOR
// SHARE: a change applied at once, needing no approval, takes the partner
// FOR UPDATE in the same transaction, and two requests holding a share
// lock would each wait for the other's.
func openForChange(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var blocked *time.Time
	err := tx.QueryRow(ctx, `SELECT personal_data_blocked_at FROM partner.business_partner WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&blocked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return problem.NotFoundOf("business partner")
	case err != nil:
		return fmt.Errorf("partner: is the personal data of business partner %s blocked: %w", id, err)
	case blocked != nil:
		return personalDataBlocked(id)
	}
	return nil
}

// BlockPersonalData blocks an active or blocked partner's personal data,
// and the partner with it, once nothing is open for it: no open item in
// any company's ledger and no pending partner change. It records an
// applied change of kind block and returns the partner.
func BlockPersonalData(ctx context.Context, items OpenItems, les LegalEntities, id uuid.UUID, ifMatch func(int32) error, reason string) (Partner, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Partner{}, err
	}
	bp, err := getPartner(ctx, tx, id, true)
	if err != nil {
		return Partner{}, err
	}
	if err := ifMatch(bp.Version); err != nil {
		return Partner{}, err
	}
	if err := blockable(ctx, tx, items, les, bp); err != nil {
		return Partner{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET personal_data_blocked_at = now(), status = 'blocked', version = version + 1, updated_by = $2
		WHERE id = $1`, id, p.ID); err != nil {
		return Partner{}, fmt.Errorf("partner: block the personal data of business partner %s: %w", id, err)
	}
	c, err := insertChange(ctx, tx, p, KindBlock, id, map[string]any{"scope": "all", "personal_data": true}, reason, nil, nil, true)
	if err != nil {
		return Partner{}, err
	}
	after, err := getPartner(ctx, tx, id, false)
	if err != nil {
		return Partner{}, err
	}
	events := []outbox.Event{privacyUpdated(after, c)}
	if bp.Status == Active {
		events = append(events, event("partner_blocked", id, "partner.business_partner", map[string]any{
			"partner_id": id, "scope": "all", "legal_entity_id": nil, "partner_change_id": c.ID, "reason": reason}))
	}
	return after, audit.Record(ctx, "partner.business_partner.block_personal_data", "partner.business_partner", id,
		map[string]any{"partner_change_id": c.ID, "reason": reason, "previous_status": bp.Status}, events...)
}

// blockable refuses a partner whose personal data may not be blocked: one
// blocked already, one neither active nor blocked, one with a pending
// change or with an open item, naming its companies.
func blockable(ctx context.Context, tx pgx.Tx, items OpenItems, les LegalEntities, bp Partner) error {
	if bp.PersonalDataBlockedAt != nil {
		return problem.Conflict("partner.personal_data_blocked", "The personal data of business partner "+bp.Number+" is blocked already.")
	}
	if bp.Status != Active && bp.Status != Blocked {
		return problem.Conflict("partner.invalid_status", "The business partner is "+bp.Status+"; blocking its personal data requires status active or blocked.")
	}
	var pending string
	err := tx.QueryRow(ctx, `SELECT number FROM partner.partner_change WHERE partner_id = $1 AND status = 'pending' ORDER BY number LIMIT 1`, bp.ID).Scan(&pending)
	if err == nil {
		return problem.Conflict("partner.change_pending", "Partner change "+pending+" of business partner "+bp.Number+
			" is pending; it is decided or cancelled before the partner's personal data is blocked.")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("partner: pending changes of business partner %s: %w", bp.ID, err)
	}
	open, err := items.OpenCompanies(ctx, bp.ID)
	if err != nil {
		return fmt.Errorf("partner: open items of business partner %s: %w", bp.ID, err)
	}
	if len(open) == 0 {
		return nil
	}
	codes := make([]string, len(open))
	for i, le := range open {
		entity, found, err := les.Entity(ctx, le)
		if err != nil {
			return fmt.Errorf("partner: company %s of an open item: %w", le, err)
		}
		codes[i] = le.String()
		if found {
			codes[i] = entity.Code
		}
	}
	return problem.Conflict("partner.open_items", "Business partner "+bp.Number+" has open items in company "+strings.Join(codes, ", ")+
		"; its personal data is blocked once they are cleared.")
}

// UnblockPersonalData lifts the block of a partner's personal data; the
// partner stays blocked until its unblock change is approved. It records
// an applied change of kind unblock and returns the partner.
func UnblockPersonalData(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Partner, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Partner{}, err
	}
	bp, err := getPartner(ctx, tx, id, true)
	if err != nil {
		return Partner{}, err
	}
	if err := ifMatch(bp.Version); err != nil {
		return Partner{}, err
	}
	if bp.PersonalDataBlockedAt == nil {
		return Partner{}, problem.Conflict("partner.personal_data_not_blocked", "The personal data of business partner "+bp.Number+" is not blocked.")
	}
	if bp.PersonalDataDeletedAt != nil {
		return Partner{}, problem.Conflict("partner.personal_data_deleted", "The personal data of business partner "+bp.Number+
			" was deleted by a deletion run; its block is not lifted.")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET personal_data_blocked_at = NULL, version = version + 1, updated_by = $2 WHERE id = $1`,
		id, p.ID); err != nil {
		return Partner{}, fmt.Errorf("partner: unblock the personal data of business partner %s: %w", id, err)
	}
	c, err := insertChange(ctx, tx, p, KindUnblock, id, map[string]any{"scope": "personal_data"}, reason, nil, nil, true)
	if err != nil {
		return Partner{}, err
	}
	after, err := getPartner(ctx, tx, id, false)
	if err != nil {
		return Partner{}, err
	}
	return after, audit.Record(ctx, "partner.business_partner.unblock_personal_data", "partner.business_partner", id,
		map[string]any{"partner_change_id": c.ID, "reason": reason}, privacyUpdated(after, c))
}

// privacyUpdated is BusinessPartnerUpdated naming the block of the
// partner's personal data, set or lifted.
func privacyUpdated(bp Partner, c Change) outbox.Event {
	var at any
	if bp.PersonalDataBlockedAt != nil {
		at = bp.PersonalDataBlockedAt.UTC().Format(time.RFC3339Nano)
	}
	return event("business_partner_updated", bp.ID, "partner.business_partner", map[string]any{"partner_id": bp.ID, "partner_change_id": c.ID,
		"changed_fields": []string{"personal_data_blocked_at"}, "personal_data_blocked_at": at})
}

// readsBlocked reports whether the caller reads blocked personal data in
// full (authz.MayUse of ReadBlockedPermission), asked only when blocked
// is true: a read of no blocked partner reads no permission.
func readsBlocked(ctx context.Context, blocked bool) (bool, error) {
	if !blocked {
		return true, nil
	}
	may, err := authz.MayUse(ctx, ReadBlockedPermission)
	if err != nil {
		return false, fmt.Errorf("partner: may the caller read blocked personal data: %w", err)
	}
	return may, nil
}

// maskable reports whether the partner's personal data is masked to a
// caller who may not read it: blocked, and not blanked since.
func (p Partner) maskable() bool {
	return p.PersonalDataBlockedAt != nil && p.PersonalDataDeletedAt == nil
}

// masked is the partner with its personal data masked: its names, its
// addresses' lines, city, postal code and region, and its tax
// identifiers' last four characters.
func masked(p Partner) Partner {
	p.Name = MaskedText
	if p.LegalName != nil {
		p.LegalName = maskedPtr(p.LegalName)
	}
	addresses := make([]Address, len(p.Addresses))
	for i, a := range p.Addresses {
		a.Line1, a.City = MaskedText, MaskedText
		a.Line2, a.PostalCode, a.Region = maskedPtr(a.Line2), maskedPtr(a.PostalCode), maskedPtr(a.Region)
		addresses[i] = a
	}
	p.Addresses = addresses
	taxes := make([]TaxIdentifier, len(p.TaxIdentifiers))
	for i, t := range p.TaxIdentifiers {
		t.ValueLast4 = MaskedLast4
		taxes[i] = t
	}
	p.TaxIdentifiers = taxes
	return p
}

// maskedPtr is MaskedText for a value, nil for none.
func maskedPtr(v *string) *string {
	if v == nil {
		return nil
	}
	m := MaskedText
	return &m
}

// maskedBank is the bank account with its holder, its last four
// characters and its bank's identifiers masked.
func maskedBank(b BankAccount) BankAccount {
	b.HolderName, b.AccountLast4 = MaskedText, MaskedLast4
	b.BIC, b.InstitutionNumber, b.TransitNumber = nil, nil, nil
	return b
}

// personalKeys are the keys of a change's payload that hold personal
// data: names, addresses and bank details.
var personalKeys = map[string]bool{"name": true, "legal_name": true, "line1": true, "line2": true, "city": true, "postal_code": true, "region": true,
	"holder_name": true, "account_last4": true, "value_last4": true, "bic": true, "institution_number": true, "transit_number": true}

// maskedPayload is a change's payload with every string value under a
// personal key masked.
func maskedPayload(v any) any { return personalReplaced(v, MaskedText) }

// personalReplaced is v with every non-empty string value under a
// personal key replaced by marker.
func personalReplaced(v any, marker string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if s, ok := val.(string); ok && personalKeys[k] && s != "" {
				val = marker
			}
			out[k] = personalReplaced(val, marker)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = personalReplaced(val, marker)
		}
		return out
	}
	return v
}

// blockedAmong returns those of the partners whose personal data is
// blocked and not deleted: what is masked.
func blockedAmong(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT id FROM partner.business_partner WHERE id = ANY($1) AND personal_data_blocked_at IS NOT NULL
		AND personal_data_deleted_at IS NULL`, ids)
	blocked, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("partner: which of %d partners have their personal data blocked: %w", len(ids), err)
	}
	out := make(map[uuid.UUID]bool, len(blocked))
	for _, id := range blocked {
		out[id] = true
	}
	return out, nil
}

// maskChanges masks, for a caller who may not read blocked personal data,
// the payload of each change of a partner whose personal data is blocked
// and the name of each possible duplicate that is such a partner.
func maskChanges(ctx context.Context, tx pgx.Tx, cs []Change) error {
	var ids []uuid.UUID
	for _, c := range cs {
		ids = append(ids, c.PartnerID)
		for _, d := range c.PossibleDuplicates {
			ids = append(ids, d.PartnerID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	blocked, err := blockedAmong(ctx, tx, ids)
	if err != nil {
		return err
	}
	may, err := readsBlocked(ctx, len(blocked) > 0)
	if err != nil || may {
		return err
	}
	for i := range cs {
		c := &cs[i]
		if blocked[c.PartnerID] {
			c.Payload, _ = maskedPayload(c.Payload).(map[string]any) // a payload is a JSON object
		}
		dups := make([]Duplicate, len(c.PossibleDuplicates))
		for j, d := range c.PossibleDuplicates {
			if blocked[d.PartnerID] {
				d.Name = MaskedText
			}
			dups[j] = d
		}
		c.PossibleDuplicates = dups
	}
	return nil
}
