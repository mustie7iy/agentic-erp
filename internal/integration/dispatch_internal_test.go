package integration

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// pendingDelivery inserts a subscription of a new tenant and a pending,
// due delivery to it, and returns the delivery as claimed (unleased, at
// version 1).
func pendingDelivery(t *testing.T) claimed {
	t.Helper()
	ten := pgtest.NewTenant(t)
	c := claimed{tenant: ten.ID, id: uuid.Must(uuid.NewV7()), sub: uuid.New(), version: 1, url: "https://93.184.216.34/hook", body: []byte(`{}`),
		sealed: [][]byte{[]byte("sealed")}}
	pgtest.Exec(t, `INSERT INTO integration.webhook_subscription (tenant_id, id, url, event_types, status, secret_cipher, created_by, updated_by)
		VALUES ($1, $2, $3, '{ledger.*}', 'active', '\x00', $4, $4)`, c.tenant, c.sub, c.url, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO integration.webhook_delivery (tenant_id, id, subscription_id, event_id, type, subject, body, status, next_attempt_at)
		VALUES ($1, $2, $3, $4, 'com.example.erp.ledger.journal_entry_posted.v1', $4, '{}', 'pending', now())`, c.tenant, c.id, c.sub, uuid.New())
	return c
}

// TestRecordTwice: recording an attempt again (its first commit's answer
// lost) changes nothing: one attempt, one disabling, one audit record.
func TestRecordTwice(t *testing.T) {
	c := pendingDelivery(t)
	d := &Dispatcher{Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.DiscardHandler)}
	for i := range 2 {
		if err := d.record(context.Background(), c, answer{status: http.StatusGone}); err != nil {
			t.Fatalf("record %d: %v", i+1, err)
		}
	}
	var status, subStatus string
	var attempts, audited int
	pgtest.Scan(t, `SELECT d.status, d.attempts, s.status FROM integration.webhook_delivery d
		JOIN integration.webhook_subscription s ON s.id = d.subscription_id WHERE d.id = $1`, []any{c.id}, &status, &attempts, &subStatus)
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1`, []any{c.sub}, &audited)
	if status != "dead" || attempts != 1 || subStatus != "disabled" || audited != 1 {
		t.Errorf("got delivery %s after %d attempts, subscription %s, %d audit records; want dead after 1, disabled, 1",
			status, attempts, subStatus, audited)
	}
}

type fakeOpener struct {
	plain string
	err   error
}

func (o fakeOpener) Open(context.Context, uuid.UUID, []byte) (string, error) { return o.plain, o.err }

type fakePoster struct {
	t      *testing.T
	cancel context.CancelFunc // nil: the post must not happen
}

func (p fakePoster) Post(context.Context, string, http.Header, []byte) (egress.Response, error) {
	if p.cancel == nil {
		p.t.Error("posted, want no attempt")
		return egress.Response{}, nil
	}
	p.cancel()
	return egress.Response{}, context.Canceled
}

// TestDeliverWithoutAnswer: a secret that cannot be opened or read is
// reported and recorded as a failed attempt, or reported with the
// recording's failure too; a stop while sending records nothing.
func TestDeliverWithoutAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		keys     fakeOpener
		failing  bool
		attempts int
	}{
		"not opened":         {keys: fakeOpener{err: errors.New("no key")}, attempts: 1},
		"not hex":            {keys: fakeOpener{plain: "zz"}, attempts: 1},
		"too short":          {keys: fakeOpener{plain: "00"}, attempts: 1},
		"not opened, failed": {keys: fakeOpener{err: errors.New("no key")}, failing: true},
		"stopped send":       {keys: fakeOpener{plain: strings.Repeat("ab", 32)}},
	} {
		t.Run(name, func(t *testing.T) {
			c := pendingDelivery(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := fakePoster{t: t}
			if name == "stopped send" {
				p.cancel = cancel
			}
			d := &Dispatcher{Pool: pgtest.Pool(t), AppRole: "erp_app", Keys: tc.keys, Client: p, Log: slog.New(slog.DiscardHandler)}
			if tc.failing {
				d.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
			}
			err := d.deliver(ctx, c)
			if err == nil {
				t.Fatal("got nil, want an error")
			}
			if tc.failing && !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want the recording's failure too", err)
			}
			var attempts int
			pgtest.Scan(t, `SELECT attempts FROM integration.webhook_delivery WHERE id = $1`, []any{c.id}, &attempts)
			if attempts != tc.attempts {
				t.Errorf("attempts: got %d, want %d", attempts, tc.attempts)
			}
		})
	}
}

// TestTruncate: an endpoint's error is kept to 1000 characters, whole
// runes.
func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("é", 1001), 1000); got != strings.Repeat("é", 1000) {
		t.Errorf("got %d runes, want 1000", len([]rune(got)))
	}
	if got := truncate("short", 1000); got != "short" {
		t.Errorf("got %q, want it unchanged", got)
	}
}

// TestRecordAfterDelete: a failure answered after the subscription was
// deleted mid-send is recorded on the delivery only.
func TestRecordAfterDelete(t *testing.T) {
	c := pendingDelivery(t)
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'deleted', version = version + 1 WHERE id = $1`, c.sub)
	d := &Dispatcher{Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.DiscardHandler)}
	if err := d.record(context.Background(), c, answer{status: http.StatusInternalServerError}); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var failing bool
	pgtest.Scan(t, `SELECT d.attempts, s.failing_since IS NOT NULL FROM integration.webhook_delivery d
		JOIN integration.webhook_subscription s ON s.id = d.subscription_id WHERE d.id = $1`, []any{c.id}, &attempts, &failing)
	if attempts != 1 || failing {
		t.Errorf("got %d attempts, failing %v; want 1 attempt and the deleted subscription unchanged", attempts, failing)
	}
}
