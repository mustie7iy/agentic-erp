package taxfiling

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

// InvoiceNumbers gives a company's registration number as its tax
// invoices print it: what the einvoicing context renders as a PINT AE
// e-invoice's seller TRN (ADR-0061 §9, As built E2). Keys opens the
// sealed number, which a tax invoice prints in clear (the UAE's:
// Executive Regulation Art. 59 as ADR-0049 records it); nothing else
// shows it.
type InvoiceNumbers struct {
	Keys *fieldcrypt.Keyring
}

// Number returns the number of the company's registration with the
// authority in force on day (from its first period's start to its last
// period's end): its own, or the tax group's it is a member
// of (member true, its VAT number not its own). ok is false when it has
// none. Of several, its own comes first, then by code.
func (r InvoiceNumbers) Number(ctx context.Context, legalEntity uuid.UUID, authority string, day time.Time) (number string, member, ok bool, err error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return "", false, false, err
	}
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT number_cipher, legal_entity_id <> $1 FROM tax.registration
		WHERE authority = $2 AND is_active AND (legal_entity_id = $1 OR $1 = ANY (group_member_ids))
			AND first_period_start <= $3 AND (last_period_end IS NULL OR last_period_end >= $3)
		ORDER BY legal_entity_id = $1 DESC, code LIMIT 1`, legalEntity, authority, day).Scan(&sealed, &member)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("taxfiling: read the %s registration of legal entity %s: %w", authority, legalEntity, err)
	}
	if number, err = r.Keys.Open(ctx, p.TenantID, sealed); err != nil {
		return "", false, false, fmt.Errorf("taxfiling: open the %s registration number of legal entity %s: %w", authority, legalEntity, err)
	}
	return number, member, true, nil
}
