package procurement_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Expiry alerts (T47g): an active contract without a live renewal that
// ends within purchasing.contract_expiry_notice_days (30 in the fixture's
// configuration) is alerted once, by ContractExpiring.

// ending is an active contract ending in days, by the database's day.
func (e env) ending(t *testing.T, days int) (map[string]any, string) {
	t.Helper()
	c, path := e.activeContract(t)
	pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = current_date - 100, valid_to = current_date + $2::int, version = version + 1 WHERE id = $1`,
		uuidOf(c), days)
	return c, path
}

func expirer(t *testing.T, logs *pgtest.SyncBuffer, settings procurement.Settings) *procurement.ContractExpirer {
	t.Helper()
	return &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil)),
		Settings: settings, Now: func() time.Time { return time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC) }}
}

func TestContractExpiryAlerts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	soon, soonPath := e.ending(t, 10)
	last, _ := e.ending(t, 30) // the last day of the notice
	_, laterPath := e.ending(t, 31)
	_, renewedPath := e.ending(t, 10)
	e.Must(201, "POST", renewedPath+":renew", renewalOf(t, e, renewedPath), "*")
	draft := e.contract(t, e.Supplier.String(), "")
	pgtest.Exec(t, `UPDATE procurement.contract SET valid_to = current_date + 5, valid_from = current_date - 5, version = version + 1 WHERE id = $1`, uuidOf(draft))
	x := expirer(t, &pgtest.SyncBuffer{}, configuration.Settings{})
	round, err := x.Expire(ctx, e.Tenant.ID) // the tenant's round: others' contracts share the database
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.Alerted != 2 || round.Expired != 0 {
		t.Fatalf("round: got %+v, want 2 alerted and none expired", round)
	}
	alerts := events(t, e, "contract_expiring")
	byID := map[any]map[string]any{}
	for _, a := range alerts {
		byID[a["contract_id"]] = a
	}
	if len(alerts) != 2 || byID[soon["id"]] == nil || byID[last["id"]] == nil {
		t.Fatalf("ContractExpiring events: got %v, want one for %v and one for %v", alerts, soon["id"], last["id"])
	}
	want(t, "alert", byID[soon["id"]], map[string]any{"days_left": float64(10), "document_number": soon["document_number"]})
	want(t, "alert target", byID[soon["id"]]["target_value"].(map[string]any), map[string]any{"amount": "1000.00", "currency": "EUR"})
	want(t, "alert on the last day", byID[last["id"]], map[string]any{"days_left": float64(30)})
	got, _ := e.Must(200, "GET", soonPath, "", "")
	if got["expiry_alerted_on"] == nil {
		t.Errorf("alerted contract: got no expiry_alerted_on, want the day of the alert")
	}
	for name, path := range map[string]string{"beyond the notice": laterPath, "renewed": renewedPath} {
		if got, _ := e.Must(200, "GET", path, "", ""); got["expiry_alerted_on"] != nil {
			t.Errorf("%s contract: got alerted on %v, want no alert", name, got["expiry_alerted_on"])
		}
	}
	var actor uuid.UUID
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT actor_id FROM audit.audit_event WHERE tenant_id = $1 AND action = 'procurement.contract.alert_expiry'
		AND object_id = $2`, e.Tenant.ID, uuidOf(soon)).Scan(&actor); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if actor != procurement.ExpirerActor {
		t.Errorf("audit actor: got %v, want %v", actor, procurement.ExpirerActor)
	}
	// Run twice: nothing is alerted again.
	round, err = x.Expire(ctx, e.Tenant.ID)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if round.Alerted != 0 {
		t.Errorf("second round: got %d alerted, want 0", round.Alerted)
	}
	if n := len(events(t, e, "contract_expiring")); n != 2 {
		t.Errorf("ContractExpiring events after the second round: got %d, want 2", n)
	}
}

// renewalOf is a renewal body for the contract at path: a year after it ends.
func renewalOf(t *testing.T, e env, path string) string {
	t.Helper()
	c, _ := e.Must(200, "GET", path, "", "")
	to, err := time.Parse(time.DateOnly, c["valid_to"].(string))
	if err != nil {
		t.Fatalf("valid_to: %v", err)
	}
	return `{"valid_to": "` + to.AddDate(1, 0, 0).Format(time.DateOnly) + `", "target_value": {"amount": "1000.00", "currency": "EUR"}}`
}

// settingsOf is the active configuration as a test sets it.
type settingsOf struct {
	doc *config.Config
	err error
}

func (s settingsOf) Document(context.Context) (*config.Config, int32, error) { return s.doc, 1, s.err }

// TestExpiryAlertsWithoutNotice: a configuration activated before the
// notice existed, or none, alerts nothing and says so; one that cannot be
// read fails the round.
func TestExpiryAlertsWithoutNotice(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.ending(t, 10)
	for name, c := range map[string]struct {
		settings settingsOf
		log      string
	}{
		"without the notice":      {settingsOf{doc: &config.Config{}}, "contract_expiry_notice_days"},
		"without a configuration": {settingsOf{err: problem.Conflict("config.not_bootstrapped", "none")}, "no configuration"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &pgtest.SyncBuffer{}
			round, err := expirer(t, logs, c.settings).Expire(ctx, e.Tenant.ID)
			if err != nil {
				t.Fatalf("round: %v", err)
			}
			if round.Alerted != 0 {
				t.Errorf("round: got %d alerted, want 0", round.Alerted)
			}
			if !strings.Contains(logs.String(), c.log) {
				t.Errorf("log: got %q, want a warning containing %q", logs.String(), c.log)
			}
		})
	}
	broken := errors.New("database gone")
	if _, err := expirer(t, &pgtest.SyncBuffer{}, settingsOf{err: broken}).Expire(ctx, e.Tenant.ID); !errors.Is(err, broken) {
		t.Errorf("round with an unreadable configuration: got %v, want %v", err, broken)
	}
	if got, _ := e.Must(200, "GET", "/v1/contracts/"+id(c), "", ""); got["expiry_alerted_on"] != nil {
		t.Errorf("contract: got alerted on %v, want no alert", got["expiry_alerted_on"])
	}
}

// TestExpiryAlertFailures: every statement and transaction of an alerting
// round failing in turn is reported, and alerts nothing half.
func TestExpiryAlertFailures(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, path := e.ending(t, 10)
	x := expirer(t, &pgtest.SyncBuffer{}, configuration.Settings{})
	pool := pgtest.Pool(t)
	for n := 1; n < 60; n++ {
		count := 0
		x.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := x.Expire(ctx, e.Tenant.ID)
		if err == nil {
			if n == 1 {
				t.Fatal("alerted without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
		if got, _ := e.Must(200, "GET", path, "", ""); got["expiry_alerted_on"] != nil {
			t.Fatalf("statement %d: alerted although the round failed: %v", n, got)
		}
	}
	if got, _ := e.Must(200, "GET", "/v1/contracts/"+id(c), "", ""); got["expiry_alerted_on"] == nil {
		t.Errorf("contract after the rounds: got no alert, want one")
	}
}

// TestRenewalWhileAlerting: a contract renewed while the expirer waits for
// it is not alerted. The renewal locks the contract it renews without
// changing it, so the alert's choice of contracts, made before the
// renewal committed, is checked again as the alert is written.
func TestRenewalWhileAlerting(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, path := e.ending(t, 10)
	renewing, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
	if err != nil {
		t.Fatalf("begin the renewal: %v", err)
	}
	defer renewing.Rollback(context.WithoutCancel(ctx))
	if err := database.SetActor(ctx, renewing, e.Tenant.Human.ID); err != nil {
		t.Fatalf("renewal actor: %v", err)
	}
	if _, err := renewing.Exec(ctx, `SELECT FROM procurement.contract WHERE id = $1 FOR UPDATE`, uuidOf(c)); err != nil {
		t.Fatalf("lock the contract: %v", err)
	}
	if _, err := renewing.Exec(ctx, `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from,
			valid_to, target_value, status, renewal_of_id, created_by, updated_by)
		SELECT tenant_id, gen_random_uuid(), 'R' || document_number, legal_entity_id, supplier_id, currency, valid_to + 1, valid_to + 365, target_value,
			'draft', id, $2, $2 FROM procurement.contract WHERE id = $1`, uuidOf(c), e.Tenant.Human.ID); err != nil {
		t.Fatalf("insert the renewal: %v", err)
	}
	x := expirer(t, &pgtest.SyncBuffer{}, configuration.Settings{})
	type result struct {
		round procurement.Round
		err   error
	}
	done := make(chan result, 1)
	go func() {
		r, err := x.Expire(ctx, e.Tenant.ID)
		done <- result{r, err}
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting bool
		err := pgtest.Admin(t).QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND query LIKE '%c.expiry_alerted_on IS NULL%' AND query LIKE '%FOR UPDATE OF c%')`).Scan(&waiting)
		return err == nil && waiting
	}, "the expirer never waited for the contract being renewed")
	if err := renewing.Commit(ctx); err != nil {
		t.Fatalf("commit the renewal: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("round: %v", got.err)
	}
	if got.round.Alerted != 0 {
		t.Errorf("round: got %d alerted, want 0: the contract was renewed", got.round.Alerted)
	}
	if contract, _ := e.Must(200, "GET", path, "", ""); contract["expiry_alerted_on"] != nil {
		t.Errorf("renewed contract: got alerted on %v, want no alert", contract["expiry_alerted_on"])
	}
}
