package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The award of an RFQ (T82d, migration 000297): what the database keeps
// of an award (ADR-0069 §7). An RFQ is awarded from published, once, to
// one of its quotations and to a contract of its company, in its
// currency, with that quotation's supplier; a quotation's outcome is set
// once, while its RFQ is published.

const (
	awardSQL   = `UPDATE procurement.rfq SET status = 'awarded', contract_id = $2, version = version + 1 WHERE id = $1`
	outcomeSQL = `UPDATE procurement.supplier_quotation SET status = $2, version = version + 1 WHERE id = $1`
)

// awardFixture is newQuotations with a quotation of each supplier for its
// other published RFQ (second the other supplier's, third the
// supplier's) and a draft contract in EUR with each supplier.
type awardFixture struct {
	quotationFixture
	second, third, contract, otherContract uuid.UUID
}

// contractIn inserts a draft contract of the legal entity with the
// supplier in the currency.
func (f fixture) contractIn(tx pgx.Tx, c, entity, supplier uuid.UUID, currency string) error {
	return exec(tx, `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from,
		valid_to, target_value, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-01-01', '2026-12-31', 100000, 'draft', $7, $7)`,
		f.tenant, c, c.String()[24:], entity, supplier, currency, f.actor)
}

func newAwards(t *testing.T) awardFixture {
	t.Helper()
	f := awardFixture{quotationFixture: newQuotations(t), second: id(), third: id(), contract: id(), otherContract: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, quotationSQL, f.tenant, f.second, f.other, f.otherSupplier, "Q-2", "recorded", f.actor); err != nil {
			return err
		}
		if err := exec(tx, quotationSQL, f.tenant, f.third, f.other, f.supplier, "Q-3", "recorded", f.actor); err != nil {
			return err
		}
		if err := f.contractIn(tx, f.contract, f.entity, f.supplier, "EUR"); err != nil {
			return err
		}
		return f.contractIn(tx, f.otherContract, f.entity, f.otherSupplier, "EUR")
	})
	return f
}

// awarding marks the quotation awarded, then awards its RFQ to the
// contract, as the application does.
func awarding(quotation, rfq, contract uuid.UUID) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, outcomeSQL, quotation, "awarded"); err != nil {
			return err
		}
		return exec(tx, awardSQL, rfq, contract)
	}
}

// twice sets a quotation's outcome and then sets it again.
func twice(quotation uuid.UUID, first, then string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, outcomeSQL, quotation, first); err != nil {
			return err
		}
		return exec(tx, outcomeSQL, quotation, then)
	}
}

// TestRFQAwardRules: what the database refuses of an award, as the
// application role.
func TestRFQAwardRules(t *testing.T) {
	need(t)
	f := newAwards(t)
	usd, elsewhere := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := f.contractIn(tx, usd, f.entity, f.supplier, "USD"); err != nil {
			return err
		}
		return f.contractIn(tx, elsewhere, id(), f.supplier, "EUR")
	})
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	const notOfQuotation = "is awarded to one of its quotations"
	for name, c := range map[string]struct {
		do   func(tx pgx.Tx) error
		want string
	}{
		"a draft RFQ awarded":            {one(awardSQL, f.draft, f.contract), "only a published RFQ is awarded"},
		"a contract without an award":    {one(`UPDATE procurement.rfq SET contract_id = $2, version = version + 1 WHERE id = $1`, f.published, f.contract), "rfq_awarded_to_contract"},
		"an award to no quotation":       {one(awardSQL, f.published, f.contract), notOfQuotation},
		"an award without a contract":    {awarding(f.quotation, f.published, uuid.Nil), notOfQuotation},
		"another supplier's contract":    {awarding(f.quotation, f.published, f.otherContract), notOfQuotation},
		"a contract in another currency": {awarding(f.quotation, f.published, usd), notOfQuotation},
		"a contract of another company":  {awarding(f.quotation, f.published, elsewhere), notOfQuotation},
		"an unknown RFQ status":          {one(`UPDATE procurement.rfq SET status = 'closed', version = version + 1 WHERE id = $1`, f.draft), "rfq_status_check"},
		"a quotation born awarded":       {one(quotationSQL, f.tenant, id(), f.other, f.supplier, "Q-9", "awarded", f.actor), "starts as recorded"},
		"an unknown outcome":             {one(outcomeSQL, f.quotation, "withdrawn"), "its outcome is set once"},
		"an outcome taken back":          {twice(f.quotation, "not_awarded", "recorded"), "its outcome is set once"},
		"an outcome changed":             {twice(f.quotation, "not_awarded", "awarded"), "its outcome is set once"},
		"two quotations of an RFQ awarded": {one(`UPDATE procurement.supplier_quotation SET status = 'awarded', version = version + 1 WHERE rfq_id = $1`, f.other),
			"supplier_quotation_one_awarded"},
		"a quotation's reference changed": {one(`UPDATE procurement.supplier_quotation SET supplier_reference = 'Q-9' WHERE id = $1`, f.quotation), "permission denied"},
		"a quotation's validity changed":  {one(`UPDATE procurement.supplier_quotation SET valid_until = '2027-12-31' WHERE id = $1`, f.quotation), "permission denied"},
		"a quotation deleted":             {one(`DELETE FROM procurement.supplier_quotation WHERE id = $1`, f.quotation), "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
		})
	}
}

// TestRFQAwarded: an award as the application makes it, and what the
// database refuses of an awarded RFQ and its quotations afterwards. The
// RFQ is awarded to the supplier's quotation and contract; the other
// supplier's quotation is left recorded, which the application never
// does, to show that its outcome can no longer be set.
func TestRFQAwarded(t *testing.T) {
	need(t)
	f := newAwards(t)
	mustApp(t, f.tenant, awarding(f.third, f.other, f.contract))
	var status string
	var contract *uuid.UUID
	if err := testDB.QueryRow(context.Background(), `SELECT status, contract_id FROM procurement.rfq WHERE id = $1`, f.other).Scan(&status, &contract); err != nil {
		t.Fatal(err)
	}
	if status != "awarded" {
		t.Fatalf("the RFQ's status: got %s, want awarded", status)
	}
	if contract == nil || *contract != f.contract {
		t.Fatalf("the RFQ's contract: got %v, want %s", contract, f.contract)
	}
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	for name, c := range map[string]struct {
		do   func(tx pgx.Tx) error
		want string
	}{
		"the award taken back": {one(`UPDATE procurement.rfq SET status = 'published', contract_id = NULL, version = version + 1 WHERE id = $1`, f.other),
			"stays awarded to its contract"},
		"the award moved to another contract": {one(`UPDATE procurement.rfq SET contract_id = $2, version = version + 1 WHERE id = $1`, f.other, f.otherContract),
			"stays awarded to its contract"},
		"a quotation for an awarded RFQ":     {one(quotationSQL, f.tenant, id(), f.other, f.supplier, "Q-9", "recorded", f.actor), "is not published"},
		"an outcome once the RFQ is awarded": {one(outcomeSQL, f.second, "not_awarded"), "is not published"},
		"a contract awarded by a second RFQ": {awarding(f.quotation, f.published, f.contract), "rfq_contract_once"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
		})
	}
}
