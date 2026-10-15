package nfr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The resilience suite (05-scalability §5.8) breaks the platform's
// dependencies under load and checks the recovery targets of
// 01-requirements §1.3: operations resume, nothing committed is lost (RPO 0
// for posted financial data) and nothing is done twice.

// loadCap bounds a background load whose test fails before it calls stop.
// The tests stop their load themselves once the fault has been injected
// and observed, so on a slow machine the load runs as long as the waits
// take rather than ending at a guessed time.
const loadCap = time.Minute

// running waits until the load has posted entries, so a fault injected
// next meets requests in flight.
func running(t *testing.T, st *Stats) {
	t.Helper()
	pgtest.Eventually(t, 30*time.Second, func() bool { return st.Count("post") >= 20 }, "the load did not get going: %v", st)
}

// background runs the mix until stop is called and returns its statistics.
func background(s *Stack, d time.Duration) (*Stats, func()) {
	st := NewStats()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, d, 0, st); close(done) }()
	return st, func() { cancel(); <-done }
}

// TestResilienceDatabaseConnectionLoss kills every connection to the
// database while the load runs, as a primary failover does: requests fail
// for a moment, then resume; every entry answered 201 is in the journal,
// every retried key posted once, and the audit chain is intact.
func TestResilienceDatabaseConnectionLoss(t *testing.T) {
	s := Start(t, Options{Users: 8, Events: true})
	ctx := context.Background()
	st, stop := background(s, loadCap)
	running(t, st)
	killed := time.Now()
	pgtest.Exec(t, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	// Recovery: the time until a request succeeds again.
	var recovered time.Time
	var last Result
	pgtest.Eventually(t, 20*time.Second, func() bool {
		last = s.Call(ctx, Req{Method: "GET", Path: "/v1/journal-entries?legal_entity_id=" + s.Env.DE.String() + "&fiscal_year=2026&page_size=1", As: s.Env.Tenant.Human})
		if last.OK() {
			recovered = time.Now()
		}
		return last.OK()
	}, "not recovered: %v", &last)
	// The load goes on after recovery, so that the check below that no
	// request fails more than a second after it has requests to judge. This
	// asserts an absence over a window, so it does wait on the clock: the
	// window lasts at least 2 s and until the users have posted another 20
	// entries, however slow the machine.
	posted := st.Count("post")
	pgtest.Eventually(t, 30*time.Second, func() bool {
		return st.Count("post") >= posted+20 && time.Since(recovered) > 2*time.Second
	}, "the load did not resume after recovery: %v", st)
	stop()
	st.Report(t, "load across a connection loss")
	t.Logf("recovered %v after the connections were killed", recovered.Sub(killed).Round(time.Millisecond))
	if rto := recovered.Sub(killed); rto > 10*time.Second {
		t.Errorf("recovery took %v", rto)
	}
	if last := st.LastFailure(); last.After(recovered.Add(time.Second)) {
		t.Errorf("requests still failed %v after recovery: %v", last.Sub(recovered), st.Errors())
	}
	for k, n := range st.Failures() {
		if k[len(k)-3:] == "500" || k[len(k)-3:] == "502" {
			t.Errorf("%s: %d (a lost connection must be a 503 the client retries)", k, n)
			for _, line := range strings.Split(s.Env.API.Logs(), "\n") {
				if strings.Contains(line, "request failed") {
					t.Log(line)
				}
			}
		}
	}
	// Clients retry failed posts with the same key: each is posted once.
	retried := 0
	for _, p := range s.Posted() {
		if p.Status == http.StatusCreated {
			continue
		}
		retried++
		if res := s.Call(ctx, Req{Method: "POST", Path: "/v1/journal-entries", Body: s.Entry(20, p.Key), As: s.Users[0].Actor, Key: p.Key}); !res.OK() &&
			res.Status != http.StatusUnprocessableEntity { // the key belongs to another principal: refused, never posted twice
			t.Errorf("retry of %s: %d %v %s", p.Key, res.Status, res.Err, res.Body)
		}
	}
	t.Logf("%d posts failed during the outage and were retried", retried)
	if err := s.Drain(30 * time.Second); err != nil {
		t.Error(err)
	}
	if err := s.Consistent(ctx); err != nil {
		t.Error(err)
	}
}

// ledgerEvents counts the tenant's ledger events in the stream.
func ledgerEvents(t *testing.T, url string, s *Stack) int {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	stream, err := js.Stream(context.Background(), natsbus.StreamName(Cell, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(context.Background(), jetstream.WithSubjectFilter("erp."+Cell+".ledger."+s.Env.Tenant.ID.String()+".>"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range info.State.Subjects {
		n += int(c)
	}
	return n
}

// TestResilienceEventBusLoss stops the JetStream node under load and starts
// it again: events wait in the outbox meanwhile, and afterwards every
// event is in the stream exactly once.
func TestResilienceEventBusLoss(t *testing.T) {
	s := Start(t, Options{Users: 4, Events: true})
	st, stop := background(s, loadCap)
	countPublished := func() int {
		return s.Count(`SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1 AND published_at IS NOT NULL`, s.Env.Tenant.ID)
	}
	running(t, st)
	pgtest.Eventually(t, 30*time.Second, func() bool { return countPublished() > 0 }, "the relay published nothing while the bus was up")
	s.NATS.Stop()
	// The outage lasts until the load has posted another 20 entries: their
	// events cannot reach a stopped bus, so they wait in the outbox, and the
	// API kept serving meanwhile.
	before := st.Count("post")
	var unpublished int
	pgtest.Eventually(t, 30*time.Second, func() bool {
		_, unpublished = s.Backlogs()
		return st.Count("post") >= before+20 && unpublished >= 20
	}, "nothing waited in the outbox while the bus was down, or the API stopped posting (%d posts before the outage): %v", before, st)
	s.NATS.Start()
	stop()
	st.Report(t, "load across an event bus loss")
	if f := st.Failures(); len(f) > 0 {
		t.Errorf("the API depends on the bus: %v", f)
	}
	if err := s.Drain(60 * time.Second); err != nil {
		t.Fatal(err)
	}
	published := countPublished()
	if published == 0 {
		t.Error("outbox published 0 events, want some")
	}
	if got := ledgerEvents(t, s.NATS.URL, s); got != published {
		t.Errorf("stream ledger events: got %d, want %d (the outbox published count)", got, published)
	}
	t.Logf("%d events waited for the bus; %d published once", unpublished, published)
}

// crashing publishes, then fails as a relay that crashed after publishing
// and before marking the batch: the batch is published again.
type crashing struct {
	outbox.Publisher
	crashes atomic.Int32
}

func (c *crashing) Publish(ctx context.Context, m outbox.Message) error {
	if err := c.Publisher.Publish(ctx, m); err != nil {
		return err
	}
	if c.crashes.Add(-1) >= 0 {
		return errors.New("relay crashed after publishing")
	}
	return nil
}

// TestResilienceWorkerRestarts kills and restarts the workers while they
// work: the projector mid-batch, the relay after publishing, the payment
// proposer mid-build. Each entry is projected once, each event delivered
// once, each proposal built once.
func TestResilienceWorkerRestarts(t *testing.T) {
	nats := natstestServer(t)
	bus, err := natsbus.Connect(nats)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := bus.EnsureStreams(context.Background(), Cell); err != nil {
		t.Fatal(err)
	}
	relay := &crashing{Publisher: bus}
	relay.crashes.Store(5)
	s := Start(t, Options{Users: 4, Events: true, Publisher: relay, NoProjector: true})
	ctx := context.Background()

	// The projector is killed and restarted every 100 ms while entries are posted.
	st, stop := background(s, 3*time.Second)
	projector := &ledger.Projector{Pool: s.Pool, Role: "erp_projector", Batch: 5, Interval: 10 * time.Millisecond, Log: s.Config.Log}
	for range 15 {
		pctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		projector.Run(pctx)
		cancel()
	}
	stop()
	st.Report(t, "load across worker restarts")
	for {
		n, err := projector.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	s.Projector = projector // Consistent compares balances with the journal
	if err := s.Drain(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.Consistent(ctx); err != nil {
		t.Error(err)
	}
	published := s.Count(`SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1 AND published_at IS NOT NULL`, s.Env.Tenant.ID)
	if published == 0 {
		t.Error("outbox published 0 events, want some")
	}
	if got := ledgerEvents(t, nats, s); got != published {
		t.Errorf("stream ledger events: got %d, want %d (the outbox published count)", got, published)
	}
	if left := relay.crashes.Load(); left >= 0 {
		t.Errorf("relay crashes left: got %d, want every crash used (below 0)", left)
	}

	// The proposal worker crashes in the middle of building a run; the next round builds it, once.
	ap := s.Env.Account(t, "2000", "211000")
	pgtest.Exec(t, `WITH c AS (
			INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'PC-NFR', 'create', $2, '{}', 'load', 'applied', $3, now(), $3, $3) RETURNING tenant_id, id, partner_id
		) INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
			bank_country_code, holder_name, valid_from, partner_change_id, created_by)
		SELECT tenant_id, gen_random_uuid(), partner_id, '\x01', md5(partner_id::text) || md5(id::text), '0000', 'iban', 'COBADEFFXXX', 'DE',
			'Holder', '2026-01-01', id, $3 FROM c`, s.Env.Tenant.ID, s.Env.Supplier, s.Env.Tenant.Human.ID)
	for i := range 5 {
		body := fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-01", "posting_date": "2026-03-01", "currency": "EUR", "reference": "INV-%d", "lines": [
			{"account_id": %q, "amount": {"amount": "100.00", "currency": "EUR"}},
			{"account_id": %q, "amount": {"amount": "-100.00", "currency": "EUR"}, "partner_id": %q}]}`, s.Env.DE, i, s.Expense, ap, s.Env.Supplier)
		s.Must(ctx, Req{Method: "POST", Path: "/v1/journal-entries", Body: body, As: s.Env.Tenant.Human})
	}
	var bank string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, s.Env.DE).Scan(&bank); err != nil {
		t.Fatal(err)
	}
	op := s.Must(ctx, Req{Method: "POST", Path: "/v1/payment-runs:propose", As: s.Env.Tenant.Human,
		Body: fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15", "due_by": "2026-04-15"}`, s.Env.DE, bank)}).JSON()
	if op["status"] != "queued" {
		t.Fatalf("proposal: %v", op)
	}
	worker := &payments.Proposer{Endpoints: app.Payments(s.Config), Pool: s.Pool, AppRole: "erp_app", WorkerRole: "erp_worker", Log: s.Config.Log}
	var once sync.Once
	worker.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, sc)
		if err != nil || sc.Role != "erp_app" {
			return tx, err
		}
		crashed := false
		once.Do(func() { crashed = true })
		if crashed {
			return &pgtest.FailingTx{Tx: tx, FailAt: 12}, nil // deep in the build, before the commit
		}
		return tx, nil
	}
	if n, err := worker.RunOnce(ctx); err == nil || n != 0 {
		t.Fatalf("the crash was not simulated: %d jobs, %v", n, err)
	}
	if n, err := worker.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("the restarted worker: %d jobs, %v", n, err)
	}
	if n, err := worker.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a third round found work: %d jobs, %v", n, err)
	}
	run := s.Must(ctx, Req{Method: "GET", Path: op["resource"].(string), As: s.Env.Tenant.Human}).JSON()
	items := s.Count(`SELECT count(*) FROM payments.payment_item WHERE tenant_id = $1`, s.Env.Tenant.ID)
	if run["status"] != "proposed" {
		t.Errorf("run status: got %v, want proposed", run["status"])
	}
	if items != 5 {
		t.Errorf("payment items: got %d, want 5", items)
	}
}

// TestResilienceShutdownUnderLoad stops the server while requests are in
// flight: every request gets a complete answer or a connection error,
// never a half-written one, and what was answered is committed.
func TestResilienceShutdownUnderLoad(t *testing.T) {
	s := Start(t, Options{Users: 8})
	st, stop := background(s, loadCap)
	running(t, st)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Server.Config.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	stop()
	st.Report(t, "load across a shutdown")
	for k, n := range st.Failures() {
		if k[len(k)-2:] != " 0" { // connection errors are the only acceptable failures
			t.Errorf("%s: %d", k, n)
		}
	}
	if st.Total() == 0 {
		t.Fatal("requests before the shutdown: got 0, want some")
	}
	if st.Count("post") == 0 {
		t.Fatal("posts before the shutdown: got 0, want some")
	}
	if err := s.Drain(30 * time.Second); err != nil {
		t.Error(err)
	}
	// The API is gone, so the consistency checks that need it run on the database alone.
	ten := s.Env.Tenant.ID
	for _, problem := range s.missing() { // a 201 cut off by the shutdown is checked by its key
		t.Error(problem)
	}
	if n := s.Count(`SELECT count(*) FROM (SELECT reference FROM ledger.journal_entry WHERE tenant_id = $1 GROUP BY reference HAVING count(*) > 1) d`, ten); n > 0 {
		t.Errorf("%d references posted more than once", n)
	}
}
