package ledger

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Parallel ledgers (T52a, ADR-0045): the configuration's ledgers, the
// ledgers each company keeps and the ledger groups. What is activated
// stays: a ledger keeps its accounting principle and is never removed, a
// company keeps its ledgers, and a group keeps its members and
// representative; names change and members are added.

// checkLedgers reports a proposal that removes an activated ledger or
// changes its accounting principle, or that removes an activated group,
// changes its representative or takes a ledger out of it.
func checkLedgers(ctx context.Context, tx pgx.Tx, proposed *config.Config, out *findings) error {
	type activated struct{ Code, Principle string }
	rows, _ := tx.Query(ctx, `SELECT code, accounting_principle FROM ledger.ledger ORDER BY code`)
	ledgers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[activated])
	if err != nil {
		return fmt.Errorf("ledger: check ledgers: %w", err)
	}
	list := proposed.LedgerList()
	for _, l := range ledgers {
		i := slices.IndexFunc(list, func(x config.Ledger) bool { return x.Code == l.Code })
		switch {
		case i < 0:
			out.add("ledgers["+l.Code+"]", "a ledger cannot be removed once activated")
		case list[i].AccountingPrinciple != l.Principle:
			out.add("ledgers["+l.Code+"].accounting_principle", "a ledger's accounting principle cannot change once activated")
		}
	}
	return checkLedgerGroups(ctx, tx, proposed, out)
}

func checkLedgerGroups(ctx context.Context, tx pgx.Tx, proposed *config.Config, out *findings) error {
	type activated struct {
		Code, Representative string
		Ledgers              []string
	}
	rows, _ := tx.Query(ctx, `SELECT g.code, r.code, array_agg(l.code ORDER BY l.code) FROM ledger.ledger_group g
		JOIN ledger.ledger r ON r.tenant_id = g.tenant_id AND r.id = g.representative_ledger_id
		JOIN ledger.ledger_group_member m ON m.tenant_id = g.tenant_id AND m.group_id = g.id
		JOIN ledger.ledger l ON l.tenant_id = m.tenant_id AND l.id = m.ledger_id
		GROUP BY g.code, r.code ORDER BY g.code`)
	groups, err := pgx.CollectRows(rows, pgx.RowToStructByPos[activated])
	if err != nil {
		return fmt.Errorf("ledger: check ledger groups: %w", err)
	}
	for _, was := range groups {
		p := "ledger_groups[" + was.Code + "]"
		i := slices.IndexFunc(proposed.LedgerGroups, func(g config.LedgerGroup) bool { return g.Code == was.Code })
		if i < 0 {
			out.add(p, "a ledger group cannot be removed once activated")
			continue
		}
		g := proposed.LedgerGroups[i]
		if g.Representative != was.Representative {
			out.add(p+".representative", "a ledger group's representative ledger cannot change once activated")
		}
		for _, code := range was.Ledgers {
			if !slices.Contains(g.Ledgers, code) {
				out.add(p+".ledgers["+code+"]", "a ledger cannot leave a group once activated")
			}
		}
	}
	return nil
}

// checkCompanyLedgers reports ledgers the proposal takes from a company
// that keeps them: their postings and periods stay the company's.
func checkCompanyLedgers(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, out *findings) error {
	rows, _ := tx.Query(ctx, `SELECT l.code FROM ledger.company_ledger c JOIN ledger.ledger l ON l.tenant_id = c.tenant_id AND l.id = c.ledger_id
		WHERE c.legal_entity_id = $1 AND NOT l.code = ANY($2) ORDER BY 1`, le, co.LedgerCodes())
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("ledger: check company %s: ledgers: %w", co.Code, err)
	}
	for _, code := range taken {
		out.add(p+".ledgers["+code+"]", "a company keeps a ledger once assigned")
	}
	return nil
}

// applyLedgers creates the document's ledgers and renames changed ones;
// 0L is the leading ledger.
func applyLedgers(ctx context.Context, tx pgx.Tx, p reqctx.Principal, doc *config.Config) error {
	list := doc.LedgerList()
	ids, codes, names, principles := make([]uuid.UUID, len(list)), make([]string, len(list)), make([]string, len(list)), make([]string, len(list))
	for i, l := range list {
		ids[i], codes[i], names[i], principles[i] = uuid.Must(uuid.NewV7()), l.Code, l.Name, l.AccountingPrinciple
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger AS l (tenant_id, id, code, name, is_leading, accounting_principle, created_by)
		SELECT $1, i, c, n, c = $7, a, $2 FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[]) AS x(i, c, n, a)
		ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name, version = l.version + 1 WHERE l.name <> EXCLUDED.name`,
		p.TenantID, p.ID, ids, codes, names, principles, config.LeadingLedger); err != nil {
		return fmt.Errorf("ledger: apply ledgers: %w", err)
	}
	return nil
}

// applyCompanyLedgers assigns the company the ledgers it keeps.
func applyCompanyLedgers(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, co config.Company) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by)
		SELECT $1, $2, l.id, l.is_leading, $3 FROM ledger.ledger l WHERE l.code = ANY($4) ON CONFLICT DO NOTHING`,
		p.TenantID, le, p.ID, co.LedgerCodes()); err != nil {
		return fmt.Errorf("ledger: apply company %s: ledgers: %w", co.Code, err)
	}
	return nil
}

// applyLedgerGroups creates the document's groups, renames changed ones
// and adds their new members.
func applyLedgerGroups(ctx context.Context, tx pgx.Tx, p reqctx.Principal, doc *config.Config) error {
	n := len(doc.LedgerGroups)
	ids, codes, names, representatives := make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
	var memberGroups, members []string
	for i, g := range doc.LedgerGroups {
		ids[i], codes[i], names[i], representatives[i] = uuid.Must(uuid.NewV7()), g.Code, g.Name, g.Representative
		for _, l := range g.Ledgers {
			memberGroups, members = append(memberGroups, g.Code), append(members, l)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_group AS g (tenant_id, id, code, name, representative_ledger_id, created_by, updated_by)
		SELECT $1, x.i, x.c, x.n, l.id, $2, $2 FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[]) AS x(i, c, n, r)
		JOIN ledger.ledger l ON l.code = x.r
		ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name, version = g.version + 1, updated_by = $2 WHERE g.name <> EXCLUDED.name`,
		p.TenantID, p.ID, ids, codes, names, representatives); err != nil {
		return fmt.Errorf("ledger: apply ledger groups: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_group_member (tenant_id, group_id, ledger_id, created_by)
		SELECT $1, g.id, l.id, $2 FROM unnest($3::text[], $4::text[]) AS x(g, l)
		JOIN ledger.ledger_group g ON g.code = x.g JOIN ledger.ledger l ON l.code = x.l
		ON CONFLICT DO NOTHING`, p.TenantID, p.ID, memberGroups, members); err != nil {
		return fmt.Errorf("ledger: apply ledger group members: %w", err)
	}
	return nil
}
