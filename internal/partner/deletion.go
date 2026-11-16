package partner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Deleted personal data (ADR-0079 §12, T100c): a deletion run of the
// records context blanks a partner whose personal data has been blocked
// longer than the retention rule for personal data, through Privacy. Each
// inventory field of the partner is replaced by a fixed marker, its bank
// accounts are ended, and the names other partners' changes copied of it
// are blanked too; no row is removed, and nothing outside the partner
// context changes. It is not reversible.

// The markers blanked personal data holds: text, and the last four
// characters of an account or a tax identifier. A Canadian account's
// institution and transit numbers, which its check requires, are zeros.
const (
	DeletedText        = "[deleted]"
	DeletedLast4       = "XXXX"
	deletedInstitution = "000"
	deletedTransit     = "00000"
)

// Privacy answers the records context's deletion runs, in the request's
// transaction: which partners' personal data is blocked, and blanking
// one. Keys seal the marker in place of an account or a tax identifier.
// Settings and Now tell partner's day, on which a blanked account ends
// (partnerDay; Now nil: the transaction's start).
type Privacy struct {
	Keys      *fieldcrypt.Keyring
	OpenItems OpenItems // the ledger's open items: a partner with one is not blanked
	Settings  config.Documents
	Now       func() time.Time
}

// BlockedSubject is a partner whose personal data is blocked and not deleted:
// its number, when the block began, and the companies it has supplier or
// customer data in, whose retention rules hold it.
type BlockedSubject struct {
	ID        uuid.UUID
	Number    string
	BlockedAt time.Time
	Companies []uuid.UUID
}

// Blocked returns the partners whose personal data is blocked and not
// deleted, by number, each with its companies by id.
func (Privacy) Blocked(ctx context.Context) ([]BlockedSubject, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.number, p.personal_data_blocked_at,
			ARRAY(SELECT s.legal_entity_id FROM partner.supplier_company s WHERE s.tenant_id = p.tenant_id AND s.partner_id = p.id
				UNION SELECT c.legal_entity_id FROM partner.customer_company c WHERE c.tenant_id = p.tenant_id AND c.partner_id = p.id ORDER BY 1)
		FROM partner.business_partner p
		WHERE p.personal_data_blocked_at IS NOT NULL AND p.personal_data_deleted_at IS NULL
		ORDER BY p.number`)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BlockedSubject, error) {
		var b BlockedSubject
		return b, r.Scan(&b.ID, &b.Number, &b.BlockedAt, &b.Companies)
	})
	if err != nil {
		return nil, fmt.Errorf("partner: partners whose personal data is blocked: %w", err)
	}
	return out, nil
}

// Blank blanks the partner's personal data, in the caller's transaction,
// and reports whether it did: a partner whose personal data is no longer
// blocked since blockedAt (the block lifted, or lifted and set again, or
// the data blanked already), or that has an open item, is left as it is.
// It holds the partner FOR UPDATE first, as a block and its lifting do,
// then writes its own rows and the possible duplicates other changes
// copied of it.
func (y Privacy) Blank(ctx context.Context, id uuid.UUID, blockedAt time.Time) (bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var at *time.Time
	var deleted bool
	err = tx.QueryRow(ctx, `SELECT personal_data_blocked_at, personal_data_deleted_at IS NOT NULL FROM partner.business_partner WHERE id = $1 FOR UPDATE`,
		id).Scan(&at, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("partner: blank business partner %s: there is none", id)
	}
	if err != nil {
		return false, fmt.Errorf("partner: lock business partner %s to blank it: %w", id, err)
	}
	if deleted || at == nil || !at.Equal(blockedAt) {
		return false, nil
	}
	// A posting that passed its partner check before the block committed
	// after it may have left an item open (ADR-0079 §11): such a partner
	// keeps its data until the item is cleared.
	open, err := y.OpenItems.OpenCompanies(ctx, id)
	if err != nil {
		return false, fmt.Errorf("partner: open items of business partner %s before blanking it: %w", id, err)
	}
	if len(open) > 0 {
		return false, nil
	}
	if err := y.blankRows(ctx, tx, p, id); err != nil {
		return false, err
	}
	return true, blankChanges(ctx, tx, id)
}

// blankRows blanks the partner's own rows: its names, addresses, bank
// accounts (ended too) and tax identifiers.
func (y Privacy) blankRows(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET name = $2, legal_name = CASE WHEN legal_name IS NOT NULL THEN $2 END,
			personal_data_deleted_at = now(), version = version + 1, updated_by = $3 WHERE id = $1`, id, DeletedText, p.ID); err != nil {
		return fmt.Errorf("partner: blank business partner %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.address SET line1 = $2, city = $2, line2 = CASE WHEN line2 IS NOT NULL THEN $2 END,
			postal_code = CASE WHEN postal_code IS NOT NULL THEN $2 END, region = CASE WHEN region IS NOT NULL THEN $2 END WHERE partner_id = $1`,
		id, DeletedText); err != nil {
		return fmt.Errorf("partner: blank the addresses of business partner %s: %w", id, err)
	}
	cipher, err := y.Keys.Seal(ctx, p.TenantID, DeletedText)
	if err != nil {
		return fmt.Errorf("partner: seal the marker of business partner %s: %w", id, err)
	}
	print, err := y.Keys.Fingerprint(ctx, p.TenantID, DeletedText)
	if err != nil {
		return fmt.Errorf("partner: fingerprint the marker of business partner %s: %w", id, err)
	}
	day, err := partnerDay(ctx, tx, y.Settings, y.Now)
	if err != nil {
		return err
	}
	// An account still valid ends today, partner's day, or the day after it
	// starts if it starts today or later: validity is never empty, nor ended
	// in the past.
	if _, err := tx.Exec(ctx, `UPDATE partner.bank_account SET holder_name = $2, account_last4 = $3, bic = NULL,
			institution_number = CASE WHEN institution_number IS NOT NULL THEN $4 END, transit_number = CASE WHEN transit_number IS NOT NULL THEN $5 END,
			account_cipher = $6, account_fingerprint = $7,
			valid_to = CASE WHEN valid_to IS NULL OR valid_to > greatest($9::date, valid_from + 1) THEN greatest($9::date, valid_from + 1) ELSE valid_to END,
			ended_by = CASE WHEN valid_to IS NULL OR valid_to > greatest($9::date, valid_from + 1) THEN $8 ELSE ended_by END
		WHERE partner_id = $1`, id, DeletedText, DeletedLast4, deletedInstitution, deletedTransit, cipher, print, p.ID, day); err != nil {
		return fmt.Errorf("partner: blank the bank accounts of business partner %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.tax_identifier SET value_cipher = $2, value_fingerprint = $3, value_last4 = $4, version = version + 1
		WHERE partner_id = $1`,
		id, cipher, print, DeletedLast4); err != nil {
		return fmt.Errorf("partner: blank the tax identifiers of business partner %s: %w", id, err)
	}
	return nil
}

// blankChanges blanks the payloads of the partner's own changes (every
// personal value the marker, sealed values and fingerprints left out) and
// its name where another partner's change listed it as a possible
// duplicate.
func blankChanges(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	rows, _ := tx.Query(ctx, `SELECT id, payload FROM partner.partner_change WHERE partner_id = $1 ORDER BY id FOR UPDATE`, id)
	type payload struct {
		id   uuid.UUID
		body map[string]any
	}
	payloads, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (payload, error) {
		var p payload
		return p, r.Scan(&p.id, &p.body)
	})
	if err != nil {
		return fmt.Errorf("partner: changes of business partner %s to blank: %w", id, err)
	}
	for _, c := range payloads {
		blank, _ := personalReplaced(mask(c.body), DeletedText).(map[string]any) // a payload is a JSON object
		if _, err := tx.Exec(ctx, `UPDATE partner.partner_change SET payload = $2, version = version + 1 WHERE id = $1`, c.id, blank); err != nil {
			return fmt.Errorf("partner: blank partner change %s: %w", c.id, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.partner_change c SET possible_duplicates = (
			SELECT jsonb_agg(CASE WHEN d->>'partner_id' = $1::text THEN jsonb_set(d, '{name}', to_jsonb($2::text)) ELSE d END ORDER BY n)
			FROM jsonb_array_elements(c.possible_duplicates) WITH ORDINALITY AS x(d, n)), version = c.version + 1
		WHERE c.possible_duplicates @> jsonb_build_array(jsonb_build_object('partner_id', $1::text))`, id, DeletedText); err != nil {
		return fmt.Errorf("partner: blank business partner %s where it is a possible duplicate: %w", id, err)
	}
	return nil
}
