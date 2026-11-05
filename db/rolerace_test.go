package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigrateBesideWinsARoleRace replays what failed PR #100's check on a
// new server: another database's migration has created the role and not
// committed when this one, past its IF NOT EXISTS, creates it too. The
// CREATE waits for the other, then fails on pg_authid's unique index; the
// retry finds the role and the migration is applied.
func TestMigrateBesideWinsARoleRace(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	role := "erp_race_" + strings.ReplaceAll(uuid.NewString(), "-", "") // roles are the server's: unique across binaries
	ident := pgx.Identifier{role}.Sanitize()
	other := connectAdmin(t)
	t.Cleanup(func() {
		if _, err := other.Exec(ctx, "DROP ROLE IF EXISTS "+ident); err != nil {
			t.Errorf("drop %s: %v", role, err)
		}
	})
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed; otherwise the failure is reported already
	if _, err := tx.Exec(ctx, "CREATE ROLE "+ident+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	ms := []Migration{{Name: "000001_role", Up: fmt.Sprintf(
		"DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s NOLOGIN; END IF; END $$",
		"'"+role+"'", ident)}} // role is uuid hex: a literal needs no escaping
	type result struct {
		applied []string
		runs    int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.applied, r.err = retryRoleRaces(func() ([]string, error) { r.runs++; return migrate(ctx, conn, ms) })
		done <- r
	}()
	waitForLockWait(t, role)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("migrate beside the other: got %v, want nil", r.err)
	}
	if r.runs != 2 {
		t.Errorf("runs: got %d, want 2 (one lost race, one retry)", r.runs)
	}
	if got := strings.Join(r.applied, ","); got != "000001_role" {
		t.Errorf("applied: got %q, want 000001_role", got)
	}
}

func connectAdmin(t *testing.T) *pgx.Conn {
	t.Helper()
	need(t)
	c, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) }) // the test is over; closing loses nothing
	return c
}

// waitForLockWait polls until a session's CREATE ROLE of role waits on a
// lock: the other transaction's, which it must outlast.
func waitForLockWait(t *testing.T, role string) {
	t.Helper()
	c := connectAdmin(t)
	deadline := time.After(30 * time.Second)
	for {
		var waiting bool
		if err := c.QueryRow(context.Background(), `SELECT EXISTS (SELECT FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%CREATE ROLE %' || $1 || '%')`, role).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the migration's CREATE ROLE never waited for the other transaction")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestLostRoleRace(t *testing.T) {
	need(t)
	// duplicate_object as the server raises it: the other committed
	// between this one's IF NOT EXISTS and its CREATE.
	_, committed := testDB.Exec(context.Background(), "CREATE ROLE erp_app NOLOGIN")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"no error":                         {nil, false},
		"not from the server":              {errors.New("boom"), false},
		"the other committed first":        {committed, true},
		"the other committed during":       {fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505", ConstraintName: "pg_authid_rolname_index"}), true},
		"another unique index":             {&pgconn.PgError{Code: "23505", ConstraintName: "tenant_pkey"}, false},
		"another object already exists":    {&pgconn.PgError{Code: "42710", Routine: "CreateTrigger"}, false},
		"a role refused for another cause": {&pgconn.PgError{Code: "42501", Routine: "CreateRole"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lostRoleRace(tc.err); got != tc.want {
				t.Errorf("lostRoleRace(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryRoleRaces(t *testing.T) {
	race := &pgconn.PgError{Code: "23505", ConstraintName: "pg_authid_rolname_index"}
	t.Run("gives up after maxRoleRaces", func(t *testing.T) {
		runs := 0
		applied, err := retryRoleRaces(func() ([]string, error) { runs++; return []string{"a"}, race })
		if runs != maxRoleRaces {
			t.Errorf("runs: got %d, want %d", runs, maxRoleRaces)
		}
		if len(applied) != maxRoleRaces {
			t.Errorf("applied: got %d, want %d", len(applied), maxRoleRaces)
		}
		if !errors.Is(err, race) {
			t.Errorf("error: got %v, want it to wrap the last race", err)
		}
	})
	t.Run("another failure is not retried", func(t *testing.T) {
		runs := 0
		boom := errors.New("boom")
		if _, err := retryRoleRaces(func() ([]string, error) { runs++; return nil, boom }); !errors.Is(err, boom) {
			t.Errorf("error: got %v, want boom", err)
		}
		if runs != 1 {
			t.Errorf("runs: got %d, want 1", runs)
		}
	})
}

func TestMigrateBesideOnAMigratedDatabase(t *testing.T) {
	need(t)
	applied, err := MigrateBeside(context.Background(), testDB)
	if err != nil {
		t.Fatalf("migrate: got %v, want nil", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied: got %v, want none", applied)
	}
}
