package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPersonalDataBlocked (T100b, migration 000331; ADR-0079 §11): a
// partner whose personal data is blocked is blocked itself, and the
// permissions to block it and to read it while blocked are seeded and are
// no approval permissions.
func TestPersonalDataBlocked(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct{ name, status, want string }{
		{"blocked", "blocked", ""},
		{"active", "active", "business_partner_personal_data_blocked_check"},
		{"inactive", "inactive", "business_partner_personal_data_blocked_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `UPDATE partner.business_partner SET status = $2, personal_data_blocked_at = now(), version = version + 1 WHERE id = $1`, f.supplier, c.status)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'partner.privacy.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "partner.privacy.block partner.privacy.read_blocked"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}
