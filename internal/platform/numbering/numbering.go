// Package numbering issues the numbers a context gives its own records
// itself, outside the configuration's number ranges (ADR-0004 As built
// review:T73g2): a business partner's and a partner change's (BP, PC), a
// material change's (MC, ADR-0082 §9). A number is the prefix and eight
// digits, one more than the table's highest of the prefix.
package numbering

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Next issues the tenant's next number with the prefix in the table's
// number column, in the transaction. Numbering is serialized per tenant and
// table by an advisory lock held until the transaction ends, so two
// requests never issue one number and a rolled-back one gives its number
// back.
func Next(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, schema, table, prefix string) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, schema+"."+table+"."+tenant.String()); err != nil {
		return "", fmt.Errorf("numbering: lock the %s.%s numbering: %w", schema, table, err)
	}
	var n string
	err := tx.QueryRow(ctx, `SELECT $1 || lpad((coalesce(substr(max(number), length($1) + 1)::bigint, 0) + 1)::text, 8, '0')
		FROM `+pgx.Identifier{schema, table}.Sanitize()+` WHERE number LIKE $1 || '%'`, prefix).Scan(&n)
	if err != nil {
		return n, fmt.Errorf("numbering: issue the next %s.%s number: %w", schema, table, err)
	}
	return n, nil
}
