package einvoicing_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// changes is the connector's message as kept: the SHA-256 of its first
// bytes, of the change last recorded (or -), and its version.
func (s *scene) changes(t *testing.T, message string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT encode(sha256, 'hex') || ' ' || coalesce(encode(changed_hash, 'hex'), '-') || ' ' || version FROM einvoicing.einvoice
		WHERE tenant_id = $1 AND connector_id = $2 AND message_id = $3`, []any{s.f.Tenant.ID, s.connector, message}, &got)
	return got
}

// record records the message for the fetch and says the outcome.
func (s *scene) record(t *testing.T, op uuid.UUID, m einvoicing.Message) string {
	t.Helper()
	got, err := s.e.RecordEInvoice(context.Background(), s.connector, op, m, s.within())
	if err != nil {
		t.Fatalf("record %s: %v", m.ID, err)
	}
	return got
}

// TestRedeliveredEInvoice: a message delivered again under its id with
// other bytes keeps its first bytes and records the new bytes' hash as
// its change, with one audit record and one EInvoiceChanged; run twice,
// the step changes and publishes nothing again and counts the change as
// this fetch's; a later fetch finding the same changed bytes, or the first
// bytes again, finds it already recorded; other bytes again are a second
// change (ADR-0061 §4, run-twice).
func TestRedeliveredEInvoice(t *testing.T) {
	s := setup(t)
	first, second, third := einvoicetest.Invoice("INV-1"), einvoicetest.Invoice("INV-1 corrected"), einvoicetest.Invoice("INV-1 again")
	m := einvoicing.Message{ID: "MSG-1", Receiver: usParticipant}
	s.srv.Deliver(m.ID, m.Receiver, first)
	if got := s.record(t, s.fetch(t, s.connector), m); got != einvoicing.Recorded {
		t.Fatalf("the first delivery: got %s, want recorded", got)
	}
	s.srv.Redeliver(m.ID, second)
	op := s.fetch(t, s.connector)
	for run := range 2 {
		if got := s.record(t, op, m); got != einvoicing.Changed {
			t.Errorf("run %d of the redelivery: got %s, want changed", run+1, got)
		}
	}
	// Version 1 recorded, 2 acknowledged, 3 changed; the acknowledgement
	// of the change finds acknowledged_at set and leaves the version.
	if got, want := s.changes(t, m.ID), hash(first)+" "+hash(second)+" 3"; got != want {
		t.Errorf("after the redelivery: got %s, want %s", got, want)
	}
	if got := s.srv.Acknowledged(m.ID); got != 2 {
		t.Errorf("acknowledgements after the redelivery: got %d, want 2 (one since)", got)
	}
	events := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})
	if len(events) != 2 {
		t.Fatalf("EInvoiceChanged: got %d events, want the receipt and one change", len(events))
	}
	for field, c := range map[string]struct{ got, want any }{
		"status":            {events[1]["status"], "received"},
		"status_before":     {events[1]["status_before"], "received"},
		"sha256":            {events[1]["sha256"], hash(first)},
		"changed_sha256":    {events[1]["changed_sha256"], hash(second)},
		"legal_entity_code": {events[1]["legal_entity_code"], "1000"},
		"receipt's change":  {events[0]["changed_sha256"], nil},
	} {
		if c.got != c.want {
			t.Errorf("the change's EInvoiceChanged, %s: got %v, want %v", field, c.got, c.want)
		}
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.bytes_changed' AND actor_id = $2`,
		[]any{s.f.Tenant.ID, einvoicing.FetcherActor}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the change: got %d, want one", audits)
	}
	later := s.fetch(t, s.connector)
	for name, body := range map[string][]byte{"the changed bytes again": second, "the first bytes again": first} {
		t.Run(name, func(t *testing.T) {
			s.srv.Redeliver(m.ID, body)
			if got := s.record(t, later, m); got != einvoicing.AlreadyRecorded {
				t.Errorf("got %s, want already_recorded", got)
			}
		})
	}
	s.srv.Redeliver(m.ID, third)
	if got := s.record(t, later, m); got != einvoicing.Changed {
		t.Errorf("other bytes again: got %s, want changed", got)
	}
	if got, want := s.changes(t, m.ID), hash(first)+" "+hash(third)+" 4"; got != want {
		t.Errorf("after a second change: got %s, want %s", got, want)
	}
	if n := len(apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})); n != 3 {
		t.Errorf("EInvoiceChanged after a second change: got %d events, want 3", n)
	}
}

// TestRedeliveredEInvoiceFailures: the change of a document addressed to
// none of the companies names none; a failing statement on the way fails
// the step, and the step run again records the change once.
func TestRedeliveredEInvoiceFailures(t *testing.T) {
	s := setup(t)
	m := einvoicing.Message{ID: "MSG-2", Receiver: "0235:999"}
	s.srv.Deliver(m.ID, m.Receiver, einvoicetest.Invoice("INV-2"))
	s.record(t, s.fetch(t, s.connector), m)
	s.srv.Redeliver(m.ID, einvoicetest.Invoice("INV-2 corrected"))
	op := s.fetch(t, s.connector)
	record := func(in einvoicing.Within) error {
		_, err := s.e.RecordEInvoice(context.Background(), s.connector, op, m, in)
		return err
	}
	everyFailure(t, s, record)
	events := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})
	if len(events) != 2 {
		t.Fatalf("EInvoiceChanged: got %d events, want the receipt and one change", len(events))
	}
	if got := events[1]["legal_entity_code"]; got != nil {
		t.Errorf("the change's company: got %v, want none", got)
	}
	m = einvoicing.Message{ID: "MSG-1", Receiver: usParticipant}
	s.srv.Deliver(m.ID, m.Receiver, einvoicetest.Invoice("INV-1"))
	s.record(t, op, m)
	s.srv.Redeliver(m.ID, einvoicetest.Invoice("INV-1 corrected"))
	later := s.fetch(t, s.connector)
	everyFailure(t, s, func(in einvoicing.Within) error {
		_, err := s.e.RecordEInvoice(context.Background(), s.connector, later, m, in)
		return err
	})
	if got := s.changes(t, m.ID); got != hash(einvoicetest.Invoice("INV-1"))+" "+hash(einvoicetest.Invoice("INV-1 corrected"))+" 3" {
		t.Errorf("MSG-1 after the failures: got %s, want its change recorded once", got)
	}
}

// failingGets is object storage whose reads fail.
type failingGets struct{ blobstore.Store }

func (failingGets) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}

// TestEInvoiceContent: the document's bytes are its first, as received,
// after a change too, through the API; one the caller may not see, or
// that does not exist, is not found; bytes missing from the store are not
// found, bytes that differ from the hash recorded are refused with
// einvoicing.document_changed, and a store that fails or a server without
// one answers 503 (ADR-0061 §8).
func TestEInvoiceContent(t *testing.T) {
	s, _ := listScene(t)
	s.srv.Redeliver("MSG-1", einvoicetest.Invoice("INV-1 corrected"))
	s.record(t, s.fetch(t, s.connector), einvoicing.Message{ID: "MSG-1", Receiver: usParticipant})
	var id, other uuid.UUID
	pgtest.Scan(t, `SELECT id FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'MSG-1'`, []any{s.f.Tenant.ID}, &id)
	pgtest.Scan(t, `SELECT id FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'MSG-3'`, []any{s.f.Tenant.ID}, &other)
	s.f.API = apitest.NewWith(t, func(c *app.Config) { c.Objects, c.Providers, c.Secrets = s.cfg.Objects, s.cfg.Providers, s.cfg.Secrets })
	path := "/v1/e-invoices/" + id.String() + "/content"
	r := s.f.API.Do(apitest.Req{Method: http.MethodGet, Path: path, Tenant: s.f.Tenant.ID, As: s.f.Tenant.Human})
	if r.Code != http.StatusOK || r.Body.String() != string(einvoicetest.Invoice("INV-1")) {
		t.Errorf("the content: got %d %q, want the first bytes as received", r.Code, r.Body)
	}
	s.f.Tenant.AllCompanies(t, s.f.Tenant.Agent)
	if r := s.f.API.Do(apitest.Req{Method: http.MethodGet, Path: path, Tenant: s.f.Tenant.ID, As: s.f.Tenant.Agent}); r.Code != http.StatusForbidden {
		t.Errorf("an agent: got %d %s, want 403 (x-agent: false)", r.Code, r.Body)
	}
	doc, _ := s.f.Must(http.StatusOK, http.MethodGet, "/v1/e-invoices/"+id.String(), "", "")
	if got, want := doc["changed_sha256"], hash(einvoicetest.Invoice("INV-1 corrected")); got != want || doc["changed_at"] == nil {
		t.Errorf("the document's change: got %v at %v, want %s with its time", got, doc["changed_at"], want)
	}
	content := func(e *einvoicing.Endpoints, doc uuid.UUID, as pgtest.Actor) ([]byte, error) {
		var out []byte
		err := apitest.InRequest(t, s.f.Tenant, as, func(ctx context.Context) error {
			res, err := e.GetEInvoiceContent(ctx, gen.GetEInvoiceContentParams{ID: doc})
			if err != nil {
				return err
			}
			out, err = io.ReadAll(res.(*gen.GetEInvoiceContentOK).Data)
			return err
		})
		return out, err
	}
	noStore := app.EInvoicing(app.Config{Pages: s.cfg.Pages})
	failing := app.EInvoicing(s.cfg)
	failing.Objects = failingGets{}
	for name, c := range map[string]struct {
		e    *einvoicing.Endpoints
		doc  uuid.UUID
		code string
	}{
		"unknown":           {s.e, uuid.New(), "request.not_found"},
		"no object storage": {noStore, id, "platform.unavailable"},
		"the store failing": {failing, id, "platform.unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := content(c.e, c.doc, s.f.Tenant.Human); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	t.Run("a caller of company 1000 alone", func(t *testing.T) {
		clerk := s.f.Tenant.Service // holds the read permission, and access to no company yet
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`, s.f.Tenant.ID, clerk.ID, s.f.US, s.f.Tenant.Human.ID)
		if got, err := content(s.e, id, clerk); err != nil || string(got) != string(einvoicetest.Invoice("INV-1")) {
			t.Errorf("1000's document: got %q %v, want its bytes", got, err)
		}
		if _, err := content(s.e, other, clerk); problem.From(err).Code != "request.not_found" {
			t.Errorf("a document addressed to no company: got %v, want request.not_found", err)
		}
	})
	kept := filepath.Join(s.store.Root, filepath.FromSlash(blobstore.Key(s.f.Tenant.ID, blobstore.KindDocuments, hash(einvoicetest.Invoice("INV-1")))))
	t.Run("bytes other than those received", func(t *testing.T) {
		if err := os.WriteFile(kept, []byte("<Invoice/>"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := content(s.e, id, s.f.Tenant.Human); problem.From(err).Code != "einvoicing.document_changed" {
			t.Errorf("got %v, want einvoicing.document_changed", err)
		}
	})
	t.Run("bytes missing from the store", func(t *testing.T) {
		if err := os.Remove(kept); err != nil {
			t.Fatal(err)
		}
		if _, err := content(s.e, id, s.f.Tenant.Human); problem.From(err).Code != "request.not_found" {
			t.Errorf("got %v, want request.not_found", err)
		}
	})
	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		call := func(ctx context.Context) error {
			_, err := s.e.GetEInvoiceContent(ctx, gen.GetEInvoiceContentParams{ID: other})
			return err
		}
		if err := call(context.Background()); err == nil {
			t.Error("outside a unit of work: got nil error, want an error")
		}
		apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, "content", call)
	})
}

// TestQueueEInvoiceFetch: a scheduled fetch's Operation is recorded once
// under the id its workflow chose, queued, of the connector, as the
// fetcher, naming its workflow; recorded again, nothing changes; outside a
// unit of work it fails (ADR-0061 §4, run-twice).
func TestQueueEInvoiceFetch(t *testing.T) {
	s := setup(t)
	op := uuid.Must(uuid.NewV7())
	queue := func(ctx context.Context) error {
		return s.e.QueueEInvoiceFetch(ctx, op, s.connector, "the-scheduled-workflow")
	}
	for range 2 {
		if err := s.within()(context.Background(), queue); err != nil {
			t.Fatalf("queue: %v", err)
		}
	}
	var got string
	pgtest.Scan(t, `SELECT count(*) || ' ' || min(kind) || ' ' || min(status) || ' ' || min(resource) || ' ' || min(workflow_id) || ' ' || min(created_by::text)
		|| ' ' || min(request->>'connector_id') FROM platform.operation WHERE tenant_id = $1 AND id = $2`, []any{s.f.Tenant.ID, op}, &got)
	if want := "1 einvoicing.fetch queued /v1/connectors/" + s.connector.String() + " the-scheduled-workflow " + einvoicing.FetcherActor.String() + " " +
		s.connector.String(); got != want {
		t.Errorf("the Operation: got %s, want %s", got, want)
	}
	if err := queue(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
}
