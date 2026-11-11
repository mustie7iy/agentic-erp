package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestFixedPaymentMethod: what the database holds of an item's fixed
// payment method (000253-000257; ADR-0038, product owner 2026-11-10). A
// supplier invoice may fix a method, one character as every method
// reference; its open item keeps it, once, never changed. A payment of
// items that fix its method names the method, and a run holds one items
// payment per supplier for the items that fix none and one per fixed
// method, never two. A supplier's items fixing a method that a run leaves
// out are excluded once per method, for a reason their group can have.
func TestFixedPaymentMethod(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run := id(), id()
	var je uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,'2026-07-31','2026-07-31','USD',300,'proposed',$4,$4,$4)`, f.tenant, run, f.entity, f.actor); err != nil {
			return err
		}
		var err error
		je, err = f.entry(tx, 4, "2026-04-02", line{account: f.expense, amount: 500}, line{account: f.ap, amount: -500, partner: &f.supplier})
		return err
	})
	type attempt struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string // the constraint it violates, or "" when it is held
	}
	check := func(t *testing.T, cases []attempt) {
		t.Helper()
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				err := asApp(t, f.tenant, c.insert)
				if c.fails == "" {
					if err != nil {
						t.Fatalf("got %v, want it held", err)
					}
					return
				}
				failsOn(t, err, c.fails)
			})
		}
	}

	t.Run("an invoice fixes one method", func(t *testing.T) {
		invoice, ins := f.invoiceOf("invoice", nil, nil, 500)
		mustApp(t, f.tenant, ins)
		fix := func(method any) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `UPDATE payables.supplier_invoice SET payment_method = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
					f.tenant, invoice, method)
			}
		}
		check(t, []attempt{{"a method", fix("S"), ""}, {"none again", fix(nil), ""}, {"a word", fix("wire"), "supplier_invoice_payment_method"},
			{"a lower-case letter", fix("s"), "supplier_invoice_payment_method"}})
	})

	t.Run("an open item keeps its method", func(t *testing.T) {
		keep := func(lineNo int, method string) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO ledger.open_item_payment_method (tenant_id, fiscal_year, journal_entry_id, line_no, payment_method)
					VALUES ($1, 2026, $2, $3, $4)`, f.tenant, je, lineNo, method)
			}
		}
		check(t, []attempt{{"a word", keep(2, "wire"), "open_item_payment_method_payment_method_check"}, {"its method", keep(2, "S"), ""},
			{"a second method", keep(2, "T"), "open_item_payment_method_pkey"}, {"of no line", keep(9, "S"), "open_item_payment_method_line_fkey"}})
		_, err := testDB.Exec(context.Background(), "UPDATE ledger.open_item_payment_method SET payment_method = 'T' WHERE tenant_id = $1", f.tenant)
		failsWith(t, err, "insert-only")
		_, err = testDB.Exec(context.Background(), "DELETE FROM ledger.open_item_payment_method WHERE tenant_id = $1", f.tenant)
		failsWith(t, err, "insert-only")
	})

	t.Run("one items payment per supplier and fixed method", func(t *testing.T) {
		pay := func(kind string, method any, fixed bool) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				var bank any
				if method != nil {
					bank = hb // a payment that names its method names its bank (payment_method_of_bank)
				}
				return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by,
					kind, house_bank_id, payment_method, method_fixed) VALUES ($1, $2, $3, $4, $5, 100, 'USD', left(md5(random()::text), 20), $6, $7, $8, $9, $10)`,
					f.tenant, id(), run, f.supplier, id(), f.actor, kind, bank, method, fixed)
			}
		}
		check(t, []attempt{
			{"its items that fix none, by the run order", pay("items", "T", false), ""},
			{"its items fixing S", pay("items", "S", true), ""},
			{"its items fixing the run order's method", pay("items", "T", true), ""},
			{"a second payment of the items that fix none", pay("items", "S", false), "payment_items_once_unfixed"},
			{"a second payment of the items fixing S", pay("items", "S", true), "payment_items_once_per_method"},
			{"fixed, naming no method", pay("items", nil, true), "payment_method_fixed"},
		})
		request := id()
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount, due_date,
				created_by, updated_by) VALUES ($1, $2, 'DP1', $3, $4, 'USD', 100, '2026-07-31', $5, $5)`, f.tenant, request, f.entity, f.supplier, f.actor)
		})
		t.Run("a down payment fixing a method", func(t *testing.T) {
			failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by,
					kind, down_payment_request_id, house_bank_id, payment_method, method_fixed) VALUES ($1, $2, $3, $4, $5, 100, 'USD', 'E2E-DP', $6, 'down_payment', $7, $8, 'S', true)`,
					f.tenant, id(), run, f.supplier, id(), f.actor, request, hb)
			}), "payment_method_fixed")
		})
	})

	t.Run("items fixing a method are excluded once per method", func(t *testing.T) {
		exclude := func(supplier uuid.UUID, method, reason string) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payments.method_exclusion (tenant_id, id, payment_run_id, supplier_id, payment_method, reason_code, amount)
					VALUES ($1, $2, $3, $4, $5, $6, -20)`, f.tenant, id(), run, supplier, method, reason)
			}
		}
		cases := []attempt{{"a debit balance", exclude(f.supplier, "S", "debit_balance"), ""},
			{"the same method twice", exclude(f.supplier, "S", "method_not_active"), "method_exclusion_once"},
			{"another method of the supplier", exclude(f.supplier, "C", "no_house_bank"), ""},
			{"a word for a method", exclude(f.supplier, "wire", "no_house_bank"), "method_exclusion_payment_method_check"},
			{"a request's reason", exclude(f.otherSupplier, "S", "tax_code_invalid"), "method_exclusion_reason_code_check"},
			{"of no run", func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payments.method_exclusion (tenant_id, id, payment_run_id, supplier_id, payment_method, reason_code, amount)
					VALUES ($1, $2, $3, $4, 'S', 'debit_balance', -20)`, f.tenant, id(), id(), f.otherSupplier)
			}, "method_exclusion_tenant_id_payment_run_id_fkey"}}
		// method_not_allowed is admitted by the check (000253) though no
		// run writes it: the supplier's methods no longer restrict a fixed
		// one (ADR-0038 As built, an item's fixed payment method).
		for _, reason := range []string{"method_not_active", "method_not_allowed", "fixed_method_in_bank_run", "item_reversed", "supplier_blocked"} {
			cases = append(cases, attempt{"the reason " + reason, exclude(id(), "S", reason), ""})
		}
		check(t, cases)
		// The fixed methods' reasons are theirs alone: the exclusion of a
		// supplier's items that fix none never carries them.
		t.Run("a fixed method's reason on items that fix none", func(t *testing.T) {
			failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount) VALUES ($1,$2,$3,'method_not_active',100)`,
					f.tenant, run, id())
			}), "payment_exclusion_reason_code_check")
		})
	})
}
