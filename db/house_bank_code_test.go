package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestHouseBankCodePerAccount: a house bank's code is up to ten upper-case
// letters or digits (000490-000491; ADR-0038 As built, a house bank's
// account by payment method): SAP's house bank and account ID (HBKID +
// HKTID) for an account of a house bank of several (product owner
// 2026-11-10).
func TestHouseBankCodePerAccount(t *testing.T) {
	need(t)
	f := newFixture(t)
	bank := func(code string, account uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$4,'\x00','5555','BUKBGB22','USD',$5,$5,'active',$6,$6)`, f.tenant, id(), f.entity, code, account, f.actor)
		}
	}
	for name, c := range map[string]struct {
		code    string
		account uuid.UUID // one per bank held (house_bank_account_once)
		fails   bool
	}{
		"a house bank's code":       {"MAIN", f.bank, false},
		"house bank and account ID": {"DEUTB00002", f.expense, false},
		"eleven characters":         {"DEUTB000002", f.ap, true},
		"lower case":                {"main", f.ap, true},
		"empty":                     {"", f.ap, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := asApp(t, f.tenant, bank(c.code, c.account))
			if !c.fails {
				if err != nil {
					t.Fatalf("code %q: got %v, want it held", c.code, err)
				}
				return
			}
			failsOn(t, err, "house_bank_code_check")
		})
	}
}

// TestHouseBankCodePerAccountDown: 000490's down refuses, rather than
// drops, a house bank coded longer than the previous version holds (06
// §6.4), and a role held to row-level security that would see none fails
// rather than narrows the check over it.
func TestHouseBankCodePerAccountDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000490_house_bank_code_per_account").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	seed := `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id,
		active_gl_account_id, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $1, 'DEUTB00002', 'DEUTB00002', '\x00', '5555', 'BUKBGB22', 'USD', $1, $1, 'active', $1, $1)`
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
		step(seed, tenant), step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`),
		step(`SAVEPOINT migrator`)); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "house banks coded by account")
}
