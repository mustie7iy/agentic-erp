package uow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestJob: As commits fn's work and audit entries as the actor; nothing to
// do, a failing actor read, a failing fn or a failing commit keep nothing;
// a failed begin is returned as it is.
func TestJob(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	boom := errors.New("boom")
	human := reqctx.Principal{ID: ten.Human.ID, Kind: ten.Human.Kind, TenantID: ten.ID}
	failCommit := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
	}
	job := func(begin database.BeginFunc) uow.Job {
		return uow.Job{Begin: begin, Scope: database.Scope{Role: "erp_app", TenantID: ten.ID}, Name: "test job"}
	}
	step := func(action string, err error) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			if err := audit.Add(ctx, audit.Entry{Action: action, ObjectType: "test", Outcome: "succeeded"}); err != nil {
				return err
			}
			return err
		}
	}
	as := func(ok bool, err error) func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
		return func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) { return human, ok, err }
	}
	for name, c := range map[string]struct {
		begin   database.BeginFunc
		actor   func(context.Context, pgx.Tx) (reqctx.Principal, bool, error)
		fnErr   error
		wantErr error
		want    int
	}{
		"committed":     {pool.Begin, as(true, nil), nil, nil, 1},
		"nothing to do": {pool.Begin, as(false, nil), nil, nil, 0},
		"actor fails":   {pool.Begin, as(true, boom), nil, boom, 0},
		"step fails":    {pool.Begin, as(true, nil), boom, boom, 0},
		"commit fails":  {failCommit, as(true, nil), nil, pgtest.ErrInjected, 0},
		"begin fails":   {func(context.Context, database.Scope) (pgx.Tx, error) { return nil, boom }, as(true, nil), nil, boom, 0},
	} {
		t.Run(name, func(t *testing.T) {
			action := "test.job_" + strings.ReplaceAll(name, " ", "_")
			err := job(c.begin).As(context.Background(), c.actor, step(action, c.fnErr))
			if c.wantErr == nil && err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
			if n := audited(t, ten, action); n != c.want {
				t.Errorf("audit entries: got %d, want %d", n, c.want)
			}
		})
	}
	t.Run("a failed commit names the job", func(t *testing.T) {
		err := job(failCommit).Tx(context.Background(), func(context.Context, pgx.Tx) (bool, error) { return true, nil })
		if got, want := err.Error(), "test job: commit: "+pgtest.ErrInjected.Error(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
