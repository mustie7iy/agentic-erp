package partner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// InvoiceParties gives a customer as a tax invoice names it: what the
// einvoicing context renders as a PINT AE e-invoice's buyer (ADR-0061 §9,
// As built E2). Keys opens the tax identifier, which a tax invoice prints
// in clear (the UAE's: Executive Regulation Art. 59 as ADR-0049 records
// it).
type InvoiceParties struct {
	Keys *fieldcrypt.Keyring
}

// InvoiceParty is a business partner as a tax invoice names it: its
// number, name (its legal name when it has one), its legal address valid
// on the day (nil for none), its tax identifier of the scheme asked for,
// in clear ("" for none), and whether its personal data is blocked
// (ADR-0079), which keeps it off a new document: the identifier is not
// opened then.
type InvoiceParty struct {
	Number, Name string
	Address      *Address
	TaxID        string
	Blocked      bool
}

// Party returns the partner as a tax invoice to it names it on day, or
// ok = false when there is no partner with the id.
func (r InvoiceParties) Party(ctx context.Context, partnerID uuid.UUID, scheme string, day time.Time) (InvoiceParty, bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return InvoiceParty{}, false, err
	}
	var out InvoiceParty
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT p.number, coalesce(p.legal_name, p.name), p.personal_data_blocked_at IS NOT NULL,
			(SELECT t.value_cipher FROM partner.tax_identifier t WHERE t.tenant_id = p.tenant_id AND t.partner_id = p.id AND t.scheme = $2)
		FROM partner.business_partner p WHERE p.id = $1`, partnerID, scheme).Scan(&out.Number, &out.Name, &out.Blocked, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvoiceParty{}, false, nil
	}
	if err != nil {
		return InvoiceParty{}, false, fmt.Errorf("partner: read business partner %s for a tax invoice: %w", partnerID, err)
	}
	if out.Blocked {
		return InvoiceParty{Number: out.Number, Name: MaskedText, Blocked: true}, true, nil
	}
	if sealed != nil {
		if out.TaxID, err = r.Keys.Open(ctx, p.TenantID, sealed); err != nil {
			return InvoiceParty{}, false, fmt.Errorf("partner: open the %s identifier of business partner %s: %w", scheme, partnerID, err)
		}
	}
	a, ok, err := legalAddress(ctx, tx, partnerID, day)
	if ok {
		out.Address = &a
	}
	return out, true, err
}

// legalAddress is the partner's legal address valid on day, ok = false
// for none.
func legalAddress(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, day time.Time) (Address, bool, error) {
	var a Address
	err := tx.QueryRow(ctx, `SELECT id, purpose, line1, line2, city, postal_code, region, country_code, valid_from
		FROM partner.address WHERE partner_id = $1 AND purpose = 'legal' AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)`,
		partnerID, day).Scan(&a.ID, &a.Purpose, &a.Line1, &a.Line2, &a.City, &a.PostalCode, &a.Region, &a.CountryCode, &a.ValidFrom.Time)
	if errors.Is(err, pgx.ErrNoRows) {
		return Address{}, false, nil
	}
	if err != nil {
		return Address{}, false, fmt.Errorf("partner: read the legal address of business partner %s: %w", partnerID, err)
	}
	return a, true, nil
}
