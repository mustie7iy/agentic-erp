package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// OpenCompanies returns the legal entities in which the partner has an
// open item: a line of the leading ledger naming it on a supplier or
// customer reconciliation account, neither cleared nor of a reversed entry
// or a reversal, special G/L items (down payments) included. It is what
// the partner context asks before a partner's personal data is blocked
// (ADR-0079 §11, T100b): nothing may be open for it. The companies are
// read one by one through the index by company and partner; the order is
// by legal entity id.
func (OpenItems) OpenCompanies(ctx context.Context, partnerID uuid.UUID) ([]uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT le.legal_entity_id
		FROM (SELECT DISTINCT legal_entity_id FROM ledger.company_account) le
		WHERE EXISTS (SELECT FROM ledger.journal_line l
			JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
			JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind IN ('supplier', 'customer')
			WHERE l.legal_entity_id = le.legal_entity_id AND l.partner_id = $1
				AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
					AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
				AND NOT `+reversedLine+`)
		ORDER BY le.legal_entity_id`, partnerID)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("ledger: companies with open items of business partner %s: %w", partnerID, err)
	}
	return ids, nil
}
