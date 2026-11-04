package fixture

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// CashItems gives the document a liquidity item tree for the cash actuals
// (ADR-0051 §4): under Operating, customer receipts (the inflows' default
// item), supplier payments (the outflows') and bank charges, the item of
// company 2000's posting rule NCHG, which posts charges to 520000.
func CashItems(doc *config.Config) {
	doc.Cash = config.Cash{
		LiquidityItems: []config.LiquidityItem{{Code: "OPERATING", Name: "Operating", Direction: config.DirectionBoth},
			{Code: "REC_CUSTOMERS", Name: "Customer receipts", Direction: config.DirectionInflow, Parent: "OPERATING"},
			{Code: "OP_SUPPLIERS", Name: "Supplier payments", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "BANK_CHARGES", Name: "Bank charges", Direction: config.DirectionBoth, Parent: "OPERATING"}},
		Derivation: config.Derivation{PostingRules: []config.PostingRuleDerivation{{Rule: "NCHG", Item: "BANK_CHARGES"}},
			Defaults: config.DerivationDefaults{Inflow: "REC_CUSTOMERS", Outflow: "OP_SUPPLIERS"}}}
	doc.Company("2000").BankStatements.PostingRules = []config.StatementPostingRule{{TransactionCode: "NCHG", Description: "Bank charges", Account: "520000"}}
}

// StatementLine is a line Statement writes: its value date, its signed
// amount in minor units, and the posting rule a decided match posts it by
// ("": no decided match, the line unmatched).
type StatementLine struct {
	ValueDate string
	Amount    int64
	Rule      string
}

// Statement writes a bank statement of the legal entity's house bank MAIN,
// in the bank's currency, with the lines numbered from 1, as an import and
// a reconciliation would leave them, and returns its id.
func (e Env) Statement(tb testing.TB, legalEntity uuid.UUID, lines ...StatementLine) uuid.UUID {
	tb.Helper()
	var bank uuid.UUID
	var currency string
	pgtest.Scan(tb, `SELECT id, currency FROM payments.house_bank WHERE tenant_id = $1 AND legal_entity_id = $2 AND code = 'MAIN'`,
		[]any{e.Tenant.ID, legalEntity}, &bank, &currency)
	days := make([]string, len(lines))
	var closing int64
	for i, l := range lines {
		days[i], closing = l.ValueDate, closing+l.Amount
	}
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position, file_ref, statement_key,
			statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'camt.053.001.02', sha256($11::bytea), 1, 'statements/' || $11, $11, $11, '1', 'ACCOUNT', $5, 0, $6, $7, $8,
			$9, 'imported', $10, $10)`,
		e.Tenant.ID, id, bank, legalEntity, currency, slices.Min(days), closing, slices.Max(days), len(lines), e.Tenant.Human.ID, id.String())
	for i, l := range lines {
		status := map[bool]string{true: "posted", false: "unmatched"}[l.Rule != ""]
		pgtest.Exec(tb, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code, bank_reference,
				owner_reference, text, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $5, 'NTRF', 'B', 'O', 'T', $6, $7, $7)`, e.Tenant.ID, id, i+1, l.Amount, l.ValueDate, status, e.Tenant.Human.ID)
		if l.Rule == "" {
			continue
		}
		match := uuid.Must(uuid.NewV7())
		pgtest.Exec(tb, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'rule', 'transaction_code', 'By its transaction code', 'matched', $5, $5)`, e.Tenant.ID, match, id, i+1, e.Tenant.Human.ID)
		pgtest.Exec(tb, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, posting_rule, incoming, created_by)
			VALUES ($1, $2, 1, 1, $3, false, $4)`, e.Tenant.ID, match, l.Rule, e.Tenant.Human.ID)
	}
	return id
}

// SnapshotLine is a line Snapshot writes: its value date, the last day of
// its bucket ("": a line at document level), its source and certainty, its
// liquidity item ("": none), its signed amount in minor units, and whether
// it was expected on the house bank MAIN.
type SnapshotLine struct {
	ValueDate, BucketEnd string
	Source, Certainty    string
	Item                 string
	Amount               int64
	Bank                 bool
}

// Snapshot writes a snapshot of the legal entity's liquidity forecast of
// the date, daily or on demand (named by its kind), with the lines
// numbered from 1 in the legal entity's currency, as TakeCashSnapshot
// would leave them, and returns its id.
func (e Env) Snapshot(tb testing.TB, legalEntity uuid.UUID, kind, date string, lines ...SnapshotLine) uuid.UUID {
	tb.Helper()
	var bank uuid.UUID
	var currency string
	pgtest.Scan(tb, `SELECT hb.id, le.local_currency FROM tenancy.legal_entity le JOIN payments.house_bank hb ON hb.tenant_id = le.tenant_id AND hb.legal_entity_id = le.id
		WHERE le.tenant_id = $1 AND le.id = $2 AND hb.code = 'MAIN'`, []any{e.Tenant.ID, legalEntity}, &bank, &currency)
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, name, snapshot_date, time_zone, horizon_end, config_version, taken_at, taken_by)
		VALUES ($1, $2, $3, $4, CASE WHEN $4 = 'on_demand' THEN 'On demand' END, $5, 'UTC', $5::date + 365, 1, clock_timestamp(), $6)`,
		e.Tenant.ID, id, legalEntity, kind, date, e.Tenant.Human.ID)
	for i, l := range lines {
		var onBank *uuid.UUID
		if l.Bank {
			onBank = &bank
		}
		pgtest.Exec(tb, `INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, house_bank_id, currency, source, certainty,
				liquidity_item, amount, flow_count)
			VALUES ($1, $2, $3, $3, $4, nullif($5, '')::date, $6, $7, $8, $9, nullif($10, ''), $11, 1)`,
			e.Tenant.ID, id, i+1, l.ValueDate, l.BucketEnd, onBank, currency, l.Source, l.Certainty, l.Item, l.Amount)
	}
	return id
}
