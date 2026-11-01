package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestRequestExclusionNamesTheTax: a request left out with
// tax_code_invalid names its code and rates (000180, ADR-0049 §4): only
// that reason names a code, rates only with a code, each a percentage of
// 0 to below 100; exclusions recorded before name none.
func TestRequestExclusionNamesTheTax(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,$4,'2026-07-31','2026-07-31','USD',0,'proposed',$5,$5,$5)`, f.tenant, run, f.entity, hb, f.actor)
	})
	exclude := func(reason string, code, requested, onRun any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.request_exclusion (tenant_id, payment_run_id, down_payment_request_id, supplier_id, reason_code, amount,
				tax_code, request_tax_rate, run_tax_rate) VALUES ($1,$2,$3,$4,$5,100,$6,$7,$8)`, f.tenant, run, id(), id(), reason, code, requested, onRun)
		}
	}
	mustApp(t, f.tenant, exclude("tax_code_invalid", "V1", "19", "16"))
	mustApp(t, f.tenant, exclude("tax_code_invalid", "V1", "19", nil))
	mustApp(t, f.tenant, exclude("tax_code_invalid", nil, nil, nil))
	mustApp(t, f.tenant, exclude("supplier_blocked", nil, nil, nil))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
	}{
		{"a code for another reason", exclude("supplier_blocked", "V1", nil, nil)},
		{"a rate without a code", exclude("tax_code_invalid", nil, "19", nil)},
		{"a negative rate", exclude("tax_code_invalid", "V1", "-1", nil)},
		{"a rate of 100", exclude("tax_code_invalid", "V1", "19", "100")},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), "request_exclusion_tax_named") })
	}
}

// TestDocumentaryWarningsAreAList: an invoice's documentary warnings are a
// JSON array, empty by default (000180, ADR-0049 §6).
func TestDocumentaryWarningsAreAList(t *testing.T) {
	need(t)
	f := newFixture(t)
	invoice := func(warnings any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
				invoice_date, currency, gross_amount, tax_amount, status, source, created_by, updated_by, documentary_warnings)
				VALUES ($1,$2,$3,$4,$5,$3,'2026-06-01','USD',100,0,'parked','manual',$6,$6,coalesce($7::jsonb, '[]'))`,
				f.tenant, id(), id().String()[24:], f.entity, f.supplier, f.actor, warnings)
		}
	}
	mustApp(t, f.tenant, invoice(nil))
	mustApp(t, f.tenant, invoice(`[{"code": "supplier_tax_id_missing"}]`))
	failsOn(t, asApp(t, f.tenant, invoice(`{"code": "supplier_tax_id_missing"}`)), "supplier_invoice_documentary_warnings_array")
}
