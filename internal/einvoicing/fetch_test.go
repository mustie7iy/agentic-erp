package einvoicing_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// Participants of the scene's connector: company 1000's and 2000's.
const (
	usParticipant = "0235:100000000000003"
	deParticipant = "0235:200000000000003"
)

// scene is a tenant with an einvoice_provider connector to the stand-in,
// naming the participants of its companies 1000 and 2000.
type scene struct {
	f         fixture.Env
	e         *einvoicing.Endpoints
	cfg       app.Config
	srv       *einvoicetest.Server
	store     blobstore.Dir
	pool      *database.Pool
	connector uuid.UUID
}

func setup(t *testing.T) *scene {
	t.Helper()
	s := &scene{f: fixture.New(t, "einvoicing.einvoice.read", "einvoicing.einvoice.manage", "platform.operation.read"),
		srv: einvoicetest.NewServer(t, "tok"), store: blobstore.Dir{Root: t.TempDir()}, pool: pgtest.Pool(t)}
	client, err := einvoicing.NewClient(egress.LoopbackForTests(s.srv.Roots))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"PROVIDER_TOKEN": "tok"}
	s.cfg = app.Config{Secrets: secrets.Local{Getenv: func(n string) string { return vars[n] }}, Objects: s.store, Providers: client, Pages: pages}
	s.e = app.EInvoicing(s.cfg)
	s.connector = s.conn(t, "Provider", s.srv.URL, "active")
	t.Cleanup(func() { // the test carries out its own fetches, never another package's runner
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, s.f.Tenant.ID, einvoicing.KindFetch)
	})
	return s
}

// conn adds an einvoice_provider connector.
func (s *scene) conn(t *testing.T, name, base, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'einvoice_provider', $3, jsonb_build_object('adapter', 'standin', 'base_url', $4::text, 'format', 'pint_ae',
		'participants', jsonb_build_object('1000', $5::text, '2000', $6::text)), 'env:PROVIDER_TOKEN', $7, $8, $8)`,
		s.f.Tenant.ID, id, name, base, usParticipant, deParticipant, status, s.f.Tenant.Human.ID)
	return id
}

// within is the fetch's Within: a committed transaction of the tenant as
// the fetcher.
func (s *scene) within() einvoicing.Within {
	return func(ctx context.Context, fn func(ctx context.Context) error) error {
		return uow.Job{Begin: s.pool.Begin, Scope: database.Scope{Role: "erp_app", TenantID: s.f.Tenant.ID}, Name: "test fetch"}.As(ctx,
			func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
				return einvoicing.Fetcher(s.f.Tenant.ID), true, nil
			}, fn)
	}
}

// fetch queues a fetch from the connector as the requester and returns
// its Operation.
func (s *scene) fetch(t *testing.T, connector uuid.UUID) uuid.UUID {
	t.Helper()
	var op uuid.UUID
	if err := apitest.Committed(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		res, err := s.e.FetchEInvoices(ctx, &gen.EInvoiceFetch{ConnectorID: connector}, gen.FetchEInvoicesParams{})
		if err == nil {
			op = res.(*gen.OperationHeaders).Response.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1 WHERE id = $1`, op)
	return op
}

// stored is what the tenant holds of the connector's message: its status,
// company, receiver, fetch, size and whether it was acknowledged; "" when
// nothing.
func (s *scene) stored(t *testing.T, message string) string {
	t.Helper()
	var got string
	err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status || ' ' || coalesce(legal_entity_id::text, '-') || ' ' || coalesce(receiver, '-')
		|| ' ' || fetch_operation_id || ' ' || size || ' ' || (acknowledged_at IS NOT NULL) FROM einvoicing.einvoice
		WHERE tenant_id = $1 AND connector_id = $2 AND message_id = $3`, s.f.Tenant.ID, s.connector, message).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestFetchEInvoicesQueues: a fetch from an active einvoice_provider
// connector is queued as its Operation, the connector its resource; one
// that cannot be fetched from is refused before anything is queued
// (ADR-0061 §4), and a server without object storage refuses every fetch.
func TestFetchEInvoicesQueues(t *testing.T) {
	s := setup(t)
	op := s.fetch(t, s.connector)
	var kind, status, resource string
	pgtest.Scan(t, `SELECT kind, status, resource FROM platform.operation WHERE id = $1`, []any{op}, &kind, &status, &resource)
	if got := kind + " " + status + " " + resource; got != "einvoicing.fetch queued /v1/connectors/"+s.connector.String() {
		t.Errorf("the Operation: got %s, want a queued einvoicing.fetch of the connector", got)
	}
	sap := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'sap_file', 'SAP', '{"path": "x.json"}', 'active', $3, $3)`, s.f.Tenant.ID, sap, s.f.Tenant.Human.ID)
	for name, c := range map[string]struct {
		e         *einvoicing.Endpoints
		connector uuid.UUID
		code      string
	}{
		"unknown":           {s.e, uuid.New(), "request.not_found"},
		"not a provider":    {s.e, sap, "integration.not_an_einvoice_provider"},
		"disabled":          {s.e, s.conn(t, "Off", s.srv.URL, "disabled"), "integration.connector_disabled"},
		"no object storage": {app.EInvoicing(app.Config{}), s.connector, "platform.unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
				_, err := c.e.FetchEInvoices(ctx, &gen.EInvoiceFetch{ConnectorID: c.connector}, gen.FetchEInvoicesParams{})
				return err
			})
			if problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	t.Run("failing statements", func(t *testing.T) {
		apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, "fetch", func(ctx context.Context) error {
			_, err := s.e.FetchEInvoices(ctx, &gen.EInvoiceFetch{ConnectorID: s.connector}, gen.FetchEInvoicesParams{})
			return err
		})
	})
}

// TestRecordEInvoice: a fetch lists the provider's pending messages and
// records each once, its bytes kept under their SHA-256, received with the
// company of its participant (none for one it names none of), audited and
// published, then acknowledged after its commit. Run twice, a step records
// nothing twice and publishes nothing again, counting the message as this
// fetch's; a later fetch finds it already recorded and acknowledges it
// again (ADR-0061 §4, run-twice).
func TestRecordEInvoice(t *testing.T) {
	s := setup(t)
	s.srv.Deliver("MSG-1", usParticipant, einvoicetest.Invoice("INV-1"))
	s.srv.Deliver("MSG-2", "0235:999", einvoicetest.Invoice("INV-2"))
	op := s.fetch(t, s.connector)
	ctx := context.Background()
	pending, err := s.e.PendingEInvoices(ctx, s.connector, s.within())
	if err != nil || len(pending) != 2 || pending[0] != (einvoicing.Message{ID: "MSG-1", Receiver: usParticipant}) {
		t.Fatalf("pending: got %v %v, want MSG-1 and MSG-2", pending, err)
	}
	for range 2 {
		for _, m := range pending {
			if got, err := s.e.RecordEInvoice(ctx, s.connector, op, m, s.within()); err != nil || got != einvoicing.Recorded {
				t.Fatalf("record %s: got %q %v, want recorded", m.ID, got, err)
			}
		}
	}
	size := len(einvoicetest.Invoice("INV-1"))
	if got, want := s.stored(t, "MSG-1"), "received "+s.f.US.String()+" "+usParticipant+" "+op.String()+" "+strconv.Itoa(size)+" true"; got != want {
		t.Errorf("MSG-1: got %s, want %s", got, want)
	}
	if got, want := s.stored(t, "MSG-2"), "received - 0235:999 "+op.String()+" "+strconv.Itoa(size)+" true"; got != want {
		t.Errorf("MSG-2: got %s, want %s (addressed to none of the companies)", got, want)
	}
	if got := s.srv.Acknowledged("MSG-1"); got != 2 {
		t.Errorf("acknowledgements of MSG-1: got %d, want 2 (one per run)", got)
	}
	sum := sha256.Sum256(einvoicetest.Invoice("INV-1"))
	kept, _, err := s.store.Get(ctx, blobstore.Key(s.f.Tenant.ID, blobstore.KindDocuments, hex.EncodeToString(sum[:])))
	if err != nil || !bytes.Equal(kept, einvoicetest.Invoice("INV-1")) {
		t.Errorf("the bytes kept: got %q %v, want the document's", kept, err)
	}
	events := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})
	if len(events) != 2 || events[0]["status"] != "received" || events[0]["legal_entity_code"] != "1000" || events[1]["legal_entity_code"] != nil ||
		events[0]["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("EInvoiceChanged: got %v, want one received per message, MSG-1's of 1000", events)
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.received' AND actor_id = $2`,
		[]any{s.f.Tenant.ID, einvoicing.FetcherActor}, &audits)
	if audits != 2 {
		t.Errorf("audit records: got %d, want one per message, as the fetcher", audits)
	}
	later := s.fetch(t, s.connector)
	if got, err := s.e.RecordEInvoice(ctx, s.connector, later, pending[0], s.within()); err != nil || got != einvoicing.AlreadyRecorded {
		t.Errorf("a later fetch: got %q %v, want already_recorded", got, err)
	}
	if got := s.srv.Acknowledged("MSG-1"); got != 3 {
		t.Errorf("acknowledgements after a later fetch: got %d, want 3", got)
	}
	if again, err := s.e.PendingEInvoices(ctx, s.connector, s.within()); err != nil || len(again) != 0 {
		t.Errorf("pending once acknowledged: got %v %v, want none", again, err)
	}
}

// TestRecordEInvoiceFailures: a provider that fails a step fails it, and
// the step run again repairs it: a lost acknowledgement leaves the
// document recorded and unacknowledged until the next run acknowledges it;
// a document above MaxDocument is refused and not recorded; a connector
// refused at use refuses the step (ADR-0061 §4).
func TestRecordEInvoiceFailures(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	op := s.fetch(t, s.connector)
	s.srv.Deliver("MSG-1", usParticipant, einvoicetest.Invoice("INV-1"))
	m := einvoicing.Message{ID: "MSG-1", Receiver: usParticipant}
	s.srv.Fail("POST /messages/MSG-1/acknowledge", 1)
	if _, err := s.e.RecordEInvoice(ctx, s.connector, op, m, s.within()); err == nil || !strings.Contains(err.Error(), "answered 503") {
		t.Errorf("the acknowledgement failing: got %v, want the provider's answer", err)
	}
	if got := s.stored(t, "MSG-1"); !strings.HasSuffix(got, " false") {
		t.Errorf("recorded, not acknowledged: got %q", got)
	}
	if _, err := s.e.RecordEInvoice(ctx, s.connector, op, m, s.within()); err != nil || !strings.HasSuffix(s.stored(t, "MSG-1"), " true") {
		t.Errorf("run again: got %v and %q, want it acknowledged", err, s.stored(t, "MSG-1"))
	}
	s.srv.Fail("GET /messages/MSG-2", 1)
	s.srv.Deliver("MSG-2", usParticipant, einvoicetest.Invoice("INV-2"))
	if _, err := s.e.RecordEInvoice(ctx, s.connector, op, einvoicing.Message{ID: "MSG-2"}, s.within()); err == nil || s.stored(t, "MSG-2") != "" {
		t.Errorf("the fetch failing: got %v and %q, want an error and nothing recorded", err, s.stored(t, "MSG-2"))
	}
	s.srv.Deliver("BIG", usParticipant, bytes.Repeat([]byte("x"), einvoicing.MaxDocument+1))
	if _, err := s.e.RecordEInvoice(ctx, s.connector, op, einvoicing.Message{ID: "BIG"}, s.within()); problem.From(err).Code != "einvoicing.document_too_large" {
		t.Errorf("a document too large: got %v, want einvoicing.document_too_large", err)
	}
	off := s.conn(t, "Off", s.srv.URL, "disabled")
	for name, call := range map[string]func() error{
		"pending": func() error { _, err := s.e.PendingEInvoices(ctx, off, s.within()); return err },
		"record":  func() error { _, err := s.e.RecordEInvoice(ctx, off, op, m, s.within()); return err },
	} {
		t.Run("a disabled connector "+name, func(t *testing.T) {
			if err := call(); problem.From(err).Code != "integration.connector_disabled" {
				t.Errorf("got %v, want integration.connector_disabled", err)
			}
		})
	}
	t.Run("the store failing", func(t *testing.T) {
		e := app.EInvoicing(s.cfg)
		e.Objects = failingStore{}
		if _, err := e.RecordEInvoice(ctx, s.connector, op, m, s.within()); err == nil || !strings.Contains(err.Error(), "store down") {
			t.Errorf("got %v, want the store's error", err)
		}
	})
	t.Run("outside a unit of work", func(t *testing.T) {
		outside := func(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }
		if _, err := s.e.PendingEInvoices(ctx, s.connector, outside); err == nil {
			t.Error("got nil error, want an error")
		}
	})
	t.Run("failing statements", func(t *testing.T) {
		s.srv.Deliver("MSG-3", deParticipant, einvoicetest.Invoice("INV-3"))
		record := func(in einvoicing.Within) error {
			_, err := s.e.RecordEInvoice(ctx, s.connector, op, einvoicing.Message{ID: "MSG-3", Receiver: deParticipant}, in)
			return err
		}
		everyFailure(t, s, record) // until it is recorded
		everyFailure(t, s, record) // recorded: found so, and acknowledged again
		if got := s.stored(t, "MSG-3"); !strings.HasPrefix(got, "received "+s.f.DE.String()) || !strings.HasSuffix(got, " true") {
			t.Errorf("MSG-3 after the failures: got %q, want it recorded for 2000 and acknowledged", got)
		}
	})
}

// failingStore is object storage that refuses every write.
type failingStore struct{ blobstore.Store }

func (failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error {
	return errors.New("store down")
}

// everyFailure runs fn with a Within whose transactions fail their first,
// second, ... statement across fn's transactions, until fn succeeds, and
// fails the test if fn ever hides the failure.
func everyFailure(t *testing.T, s *scene, fn func(in einvoicing.Within) error) {
	t.Helper()
	for n := 1; n < 200; n++ {
		count := 0
		in := func(ctx context.Context, fn func(ctx context.Context) error) error {
			return uow.Job{Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
				tx, err := s.pool.Begin(ctx, scope)
				if err != nil {
					return nil, err
				}
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
			}, Scope: database.Scope{Role: "erp_app", TenantID: s.f.Tenant.ID}, Name: "test fetch"}.As(ctx,
				func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
					return einvoicing.Fetcher(s.f.Tenant.ID), true, nil
				}, fn)
		}
		err := fn(in)
		if err == nil {
			return
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d failed, but the error was %v", n, err)
		}
	}
	t.Fatal("still failing after 200 statements")
}

// TestFinishEInvoiceFetch: a fetch's Operation succeeds with its counts
// and the list of what it recorded, or fails with
// einvoicing.fetch_incomplete naming the messages not fetched; finishing
// one already finished changes nothing; a fetch stopped before any
// message fails with its problem.
func TestFinishEInvoiceFetch(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	state := func(op uuid.UUID) string {
		var got string
		pgtest.Scan(t, `SELECT status || ' ' || coalesce(resource, '-') || ' ' || coalesce(result::text, '-') || ' ' || coalesce(error->>'code', '-')
			FROM platform.operation WHERE id = $1`, []any{op}, &got)
		return got
	}
	done := s.fetch(t, s.connector)
	finish := func(op uuid.UUID, failed []string) error {
		return s.within()(ctx, func(ctx context.Context) error {
			return s.e.FinishEInvoiceFetch(ctx, op, einvoicing.Counts{Recorded: 2, AlreadyRecorded: 1}, failed)
		})
	}
	for range 2 {
		if err := finish(done, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := state(done), `succeeded /v1/e-invoices?fetch_operation_id=`+done.String()+` {"recorded": 2, "already_recorded": 1} -`; got != want {
		t.Errorf("a fetch done: got %s, want %s", got, want)
	}
	incomplete := s.fetch(t, s.connector)
	if err := finish(incomplete, []string{"MSG-9"}); err != nil {
		t.Fatal(err)
	}
	if got := state(incomplete); got != "failed /v1/connectors/"+s.connector.String()+" - einvoicing.fetch_incomplete" {
		t.Errorf("a fetch incomplete: got %s", got)
	}
	var detail string
	pgtest.Scan(t, `SELECT error->>'detail' FROM platform.operation WHERE id = $1`, []any{incomplete}, &detail)
	if !strings.Contains(detail, "MSG-9") || !strings.Contains(detail, "fetch_operation_id="+incomplete.String()) {
		t.Errorf("its detail: got %q, want the message and the fetch's list named", detail)
	}
	stopped := s.fetch(t, s.connector)
	for range 2 {
		if err := s.within()(ctx, func(ctx context.Context) error {
			return s.e.FailEInvoiceFetch(ctx, stopped, problem.Conflict("integration.connector_disabled", "off"))
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := state(stopped); got != "failed /v1/connectors/"+s.connector.String()+" - integration.connector_disabled" {
		t.Errorf("a fetch stopped: got %s", got)
	}
	unknown := uuid.New()
	for name, call := range map[string]func(ctx context.Context) error{
		"finish": func(ctx context.Context) error {
			return s.e.FinishEInvoiceFetch(ctx, unknown, einvoicing.Counts{}, nil)
		},
		"fail": func(ctx context.Context) error {
			return s.e.FailEInvoiceFetch(ctx, unknown, problem.Conflict("x.y", "z"))
		},
	} {
		t.Run("an unknown Operation "+name, func(t *testing.T) {
			if err := s.within()(ctx, call); problem.From(err).Code != "request.not_found" {
				t.Errorf("got %v, want request.not_found", err)
			}
		})
	}
}

// TestStandIn: the stand-in adapter refuses what the provider answers
// wrongly, and the adapters refuse a connector they cannot call.
func TestStandIn(t *testing.T) {
	srv := einvoicetest.NewServer(t, "tok")
	client, err := einvoicing.NewClient(egress.LoopbackForTests(srv.Roots))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := &einvoicing.StandIn{Client: client, BaseURL: srv.URL, Token: "tok"}
	wrong := &einvoicing.StandIn{Client: client, BaseURL: srv.URL, Token: "other"}
	gone := &einvoicing.StandIn{Client: client, BaseURL: "https://127.0.0.1:1/provider", Token: "tok"}
	plain := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) })) // the client's read is checked
	t.Cleanup(plain.Close)
	roots := x509.NewCertPool()
	roots.AddCert(plain.Certificate())
	plainClient, err := einvoicing.NewClient(egress.LoopbackForTests(roots))
	if err != nil {
		t.Fatal(err)
	}
	notJSON := &einvoicing.StandIn{Client: plainClient, BaseURL: plain.URL, Token: "tok"}
	srv.Deliver("X", "", []byte("<x/>"))
	for name, call := range map[string]func() error{
		"listing with the wrong token":     func() error { _, err := wrong.Pending(ctx); return err },
		"listing unreachable":              func() error { _, err := gone.Pending(ctx); return err },
		"a listing not JSON":               func() error { _, err := notJSON.Pending(ctx); return err },
		"fetching unreachable":             func() error { _, err := gone.Fetch(ctx, "X"); return err },
		"fetching an unknown message":      func() error { _, err := p.Fetch(ctx, "Y"); return err },
		"acknowledging unreachable":        func() error { return gone.Acknowledge(ctx, "X") },
		"acknowledging an unknown message": func() error { return p.Acknowledge(ctx, "Y") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("got nil error, want the step failed")
			}
		})
	}
	if err := p.Acknowledge(ctx, "X"); err != nil {
		t.Errorf("acknowledging: got %v, want done", err)
	}
	for name, c := range map[string]struct {
		client  *egress.Client
		adapter string
		code    string
	}{
		"no client":       {nil, "standin", "platform.unavailable"},
		"another adapter": {client, "acme", "integration.invalid_settings"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := einvoicing.Adapters(c.client)(einvoicing.Connector{Name: "P", Adapter: c.adapter}); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
}
