package einvoicing_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

// Participants of the Evatra connector: 0235 and the TIN of companies 1000
// and 2000 (ADR-0061 §5 C6).
const (
	usTIN = "0235:1000000000"
	deTIN = "0235:2000000000"
)

// evatraSecret is the Evatra connector's credentials, Server's account.
var evatraSecret = `{"apiKey": "tok", "email": "` + einvoicetest.EvatraEmail + `"}`

// evatraScene is the scene with Evatra's hosts replaced by the stand-in's
// Evatra API and an evatra connector in environment env.
func evatraScene(t *testing.T, env, secret string) *scene {
	t.Helper()
	s := setup(t)
	s.cfg.Secrets = vaulttest.ByPath{"provider": "tok", "evatra": secret}
	s.cfg.EvatraHosts = map[string]string{"stage": s.srv.EvatraURL, "prod": s.srv.EvatraURL}
	s.e = app.EInvoicing(s.cfg)
	s.connector = s.evatraConn(t, "Evatra", env, "evatra")
	return s
}

// evatraConn adds an evatra connector whose secret is at path.
func (s *scene) evatraConn(t *testing.T, name, env, path string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'einvoice_provider', $3, jsonb_build_object('adapter', 'evatra', 'environment', $4::text, 'company_id', $5::text,
		'format', 'pint_ae', 'participants', jsonb_build_object('1000', $6::text, '2000', $7::text)), $8, 'active', $9, $9)`,
		s.f.Tenant.ID, id, name, env, einvoicetest.EvatraCompany, usTIN, deTIN, vaulttest.TenantRef(s.f.Tenant.ID, path), s.f.Tenant.Human.ID)
	return id
}

// TestEvatraFetch: an evatra connector's fetch lists each company's
// received documents at Evatra by its TIN, records each with its bytes as
// Evatra gave them in Base64, acknowledged locally; Evatra keeps no
// acknowledgement, so the next fetch leaves out what is recorded and
// acknowledged, and takes again one whose acknowledgement was lost, found
// already recorded. One login serves every call while its token lives
// (ADR-0061 As built E1).
func TestEvatraFetch(t *testing.T) {
	s := evatraScene(t, "prod", evatraSecret)
	ctx := context.Background()
	s.srv.Deliver("DOC-1", usTIN, einvoicetest.Invoice("INV-1"))
	s.srv.Deliver("DOC-2", deTIN, einvoicetest.Invoice("INV-2"))
	s.srv.Deliver("DOC-3", "0235:9999999999", einvoicetest.Invoice("INV-3")) // another company's, not listed for this connector
	op := s.fetch(t, s.connector)
	ms, err := s.e.PendingEInvoices(ctx, s.connector, s.within())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ms); got != "[{DOC-1 "+usTIN+"} {DOC-2 "+deTIN+"}]" {
		t.Fatalf("pending: got %s, want DOC-1 for 1000 and DOC-2 for 2000", got)
	}
	for run := range 2 { // run twice: recorded, then found recorded by this fetch
		if outcome, err := s.e.RecordEInvoice(ctx, s.connector, op, ms[0], s.within()); err != nil || outcome != einvoicing.Recorded {
			t.Errorf("DOC-1 run %d: got %q %v, want recorded", run, outcome, err)
		}
	}
	if _, err := s.e.RecordEInvoice(ctx, s.connector, op, ms[1], losingAcknowledgement(s.within())); !errors.Is(err, errLost) {
		t.Errorf("DOC-2 with its acknowledgement lost: got %v, want it lost", err)
	}
	if got := s.stored(t, "DOC-1"); !strings.HasPrefix(got, "received "+s.f.US.String()+" "+usTIN+" "+op.String()) || !strings.HasSuffix(got, " true") {
		t.Errorf("DOC-1: got %q, want it received for 1000 and acknowledged", got)
	}
	if got := s.content(t, "DOC-1"); !bytes.Equal(got, einvoicetest.Invoice("INV-1")) {
		t.Errorf("DOC-1's bytes: got %q, want Evatra's decoded", got)
	}
	second := s.fetch(t, s.connector)
	again, err := s.e.PendingEInvoices(ctx, s.connector, s.within())
	if err != nil || fmt.Sprint(again) != "[{DOC-2 "+deTIN+"}]" {
		t.Fatalf("pending with an acknowledgement lost: got %v %v, want DOC-2", again, err)
	}
	if outcome, err := s.e.RecordEInvoice(ctx, s.connector, second, again[0], s.within()); err != nil || outcome != einvoicing.AlreadyRecorded {
		t.Errorf("DOC-2 again: got %q %v, want already recorded", outcome, err)
	}
	if got := s.stored(t, "DOC-2"); !strings.HasSuffix(got, " true") {
		t.Errorf("DOC-2 after the second fetch: got %q, want it acknowledged", got)
	}
	if again, err := s.e.PendingEInvoices(ctx, s.connector, s.within()); err != nil || len(again) != 0 {
		t.Errorf("pending once recorded and acknowledged: got %v %v, want none", again, err)
	}
	if got := s.srv.Evatra.Logins(); got != 1 {
		t.Errorf("logins: got %d, want 1", got)
	}
	t.Run("failing statements", func(t *testing.T) {
		s.srv.Deliver("DOC-4", usTIN, einvoicetest.Invoice("INV-4"))
		if _, err := s.e.RecordEInvoice(ctx, s.connector, second, einvoicing.Message{ID: "DOC-4", Receiver: usTIN}, losingAcknowledgement(s.within())); !errors.Is(err, errLost) {
			t.Fatalf("DOC-4: got %v, want its acknowledgement lost", err)
		}
		everyFailure(t, s, func(in einvoicing.Within) error {
			ms, err := s.e.PendingEInvoices(ctx, s.connector, in)
			if err == nil && fmt.Sprint(ms) != "[{DOC-4 "+usTIN+"}]" {
				t.Errorf("pending: got %v, want DOC-4", ms)
			}
			return err
		})
	})
}

// errLost is the acknowledgement's commit lost.
var errLost = errors.New("the acknowledgement's commit lost")

// losingAcknowledgement is in, but its third transaction, a recording's
// acknowledgement (after the connector's and the record's), is lost.
func losingAcknowledgement(in einvoicing.Within) einvoicing.Within {
	n := 0
	return func(ctx context.Context, fn func(ctx context.Context) error) error {
		if n++; n == 3 {
			return errLost
		}
		return in(ctx, fn)
	}
}

// content is the bytes kept of the connector's message.
func (s *scene) content(t *testing.T, message string) []byte {
	t.Helper()
	var ref string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT content_ref FROM einvoicing.einvoice WHERE tenant_id = $1 AND connector_id = $2
		AND message_id = $3`, s.f.Tenant.ID, s.connector, message).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestEvatraPages: a company's received documents are read page by page,
// 200 at a time, until Evatra's count is reached.
func TestEvatraPages(t *testing.T) {
	s := evatraScene(t, "stage", evatraSecret)
	for i := range einvoicing.EvatraPage + 1 {
		s.srv.Deliver("DOC-"+strconv.Itoa(i), usTIN, einvoicetest.Invoice("INV"))
	}
	ms, err := s.e.PendingEInvoices(context.Background(), s.connector, s.within())
	if err != nil || len(ms) != einvoicing.EvatraPage+1 || ms[einvoicing.EvatraPage].ID != "DOC-200" {
		t.Errorf("got %d messages, %v; want 201, the last DOC-200", len(ms), err)
	}
}

// TestEvatraSessions: a token Evatra refused is dropped and the call sent
// once more after a new login; one about to expire is not used; wrong
// credentials, or a secret that is not Evatra's, are refused as the
// credentials' problem, and an environment without a host as the
// settings'; a fetch from a connector whose secret is not Evatra's is
// refused before it is queued.
func TestEvatraSessions(t *testing.T) {
	s := evatraScene(t, "prod", evatraSecret)
	ctx := context.Background()
	s.srv.Deliver("DOC-1", usTIN, einvoicetest.Invoice("INV-1"))
	pending := func(connector uuid.UUID) error {
		_, err := s.e.PendingEInvoices(ctx, connector, s.within())
		return err
	}
	if err := pending(s.connector); err != nil {
		t.Fatal(err)
	}
	s.srv.Evatra.Expire()
	if err := pending(s.connector); err != nil || s.srv.Evatra.Logins() != 2 {
		t.Errorf("a token expired at Evatra: got %v after %d logins, want listed after 2", err, s.srv.Evatra.Logins())
	}
	s.srv.Evatra.Expire()
	s.srv.Evatra.SetTokenLife(30 * time.Second) // below the minute a token is kept short of its expiry
	for range 2 {                               // each search of each listing logs in: the token is not kept
		if err := pending(s.connector); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.srv.Evatra.Logins(); got != 6 {
		t.Errorf("a token about to expire: got %d logins, want 6 (one per search)", got)
	}
	s.cfg.Secrets = vaulttest.ByPath{"evatra": evatraSecret, "wrong": `{"apiKey": "other", "email": "x@example.test"}`, "plain": "tok",
		"partial": `{"apiKey": "tok"}`}
	s.e = app.EInvoicing(s.cfg)
	for name, c := range map[string]struct {
		connector uuid.UUID
		code      string
	}{
		"wrong credentials":     {s.evatraConn(t, "Wrong", "prod", "wrong"), "integration.secret_unavailable"},
		"a plain token":         {s.evatraConn(t, "Plain", "prod", "plain"), "integration.secret_unavailable"},
		"no email":              {s.evatraConn(t, "Partial", "prod", "partial"), "integration.secret_unavailable"},
		"an unknown connector":  {uuid.New(), "request.not_found"},
		"the connector's valid": {s.connector, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := pending(c.connector); problem.From(err).Code != c.code && (c.code != "" || err != nil) {
				t.Errorf("got %v, want %q", err, c.code)
			}
		})
	}
	t.Run("a fetch with a secret not Evatra's", func(t *testing.T) {
		plain := s.evatraConn(t, "Plain again", "prod", "plain")
		err := apitest.Committed(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := s.e.FetchEInvoices(ctx, &gen.EInvoiceFetch{ConnectorID: plain}, gen.FetchEInvoicesParams{})
			return err
		})
		if problem.From(err).Code != "integration.secret_unavailable" {
			t.Errorf("got %v, want integration.secret_unavailable", err)
		}
	})
	t.Run("an environment without a host", func(t *testing.T) {
		adapter := einvoicing.Adapters(s.cfg.Providers, map[string]string{"prod": s.srv.EvatraURL}, nil)
		if _, err := adapter(einvoicing.Connector{Name: "E", Adapter: "evatra", Environment: "stage", Secret: evatraSecret}); problem.From(err).Code != "integration.invalid_settings" {
			t.Errorf("got %v, want integration.invalid_settings", err)
		}
		if _, err := einvoicing.Adapters(s.cfg.Providers, nil, nil)(einvoicing.Connector{Name: "E", Adapter: "evatra", Environment: "prod", Secret: evatraSecret}); err != nil {
			t.Errorf("Evatra's own hosts: got %v, want an adapter", err)
		}
	})
}

// fakeEvatra is an Evatra adapter of a server answering each path as
// routes say (and 404 otherwise), its login answering a token.
func fakeEvatra(t *testing.T, routes map[string]http.HandlerFunc) einvoicing.Provider {
	t.Helper()
	mux := http.NewServeMux()
	if _, ok := routes["POST /auth/login"]; !ok {
		routes["POST /auth/login"] = func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"jwt": "j", "expirationDate": "` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)) // the client's read is checked
		}
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client, err := einvoicing.NewClient(egress.LoopbackForTests(roots))
	if err != nil {
		t.Fatal(err)
	}
	p, err := einvoicing.Adapters(client, map[string]string{"prod": srv.URL}, nil)(einvoicing.Connector{Name: "E", Adapter: "evatra", Environment: "prod",
		CompanyID: "C", Participants: map[string]string{"1000": usTIN}, Secret: evatraSecret})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// answer answers status with body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // the client's read is checked
	}
}

// TestEvatraAnswers: the adapter refuses what Evatra answers wrongly, each
// call failing rather than guessing: a login without a token or not 200,
// a listing not 200 or not JSON or naming no documentId, more documents
// than its pages hold, a token refused again after a new login; a
// document not found, not Base64 (unreadable) or above MaxDocument once
// decoded (too large); a document in Base64 as text or as a JSON string
// is the document. A login whose expirationDate it cannot read serves its
// call and is not kept. Acknowledging does nothing.
func TestEvatraAnswers(t *testing.T) {
	ctx := context.Background()
	doc := base64.StdEncoding.EncodeToString([]byte("<Invoice/>"))
	quoted, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	huge := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), einvoicing.MaxDocument+1))
	full := `{"totalCount": 1000000, "invoiceList": [` + strings.Repeat(`{"documentId": "D"},`, einvoicing.EvatraPage-1) + `{"documentId": "D"}]}`
	for name, c := range map[string]struct {
		routes map[string]http.HandlerFunc
		call   func(p einvoicing.Provider) error
		code   string // "": an error without a problem code
	}{
		"a login without a token": {map[string]http.HandlerFunc{"POST /auth/login": answer(200, `{}`)}, pending, ""},
		"a login failing":         {map[string]http.HandlerFunc{"POST /auth/login": answer(500, ``)}, pending, ""},
		"a login refused":         {map[string]http.HandlerFunc{"POST /auth/login": answer(403, ``)}, pending, "integration.secret_unavailable"},
		"a listing failing":       {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(503, ``)}, pending, ""},
		"a listing not JSON":      {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(200, `not json`)}, pending, ""},
		"a document id neither a string nor an integer": {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(200, `{"totalCount": 1, "invoiceList": [{"documentId": 1.5}]}`)},
			pending, ""},
		"a document id an object": {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(200, `{"totalCount": 1, "invoiceList": [{"documentId": {}}]}`)},
			pending, ""},
		"a document without its id": {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(200, `{"totalCount": 1, "invoiceList": [{"id": "D"}]}`)},
			pending, ""},
		"more documents than the pages": {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(200, full)}, pending, ""},
		"a token refused after a login": {map[string]http.HandlerFunc{"POST /invoice/search-documents": answer(401, ``)}, pending, ""},
		"a document not found":          {map[string]http.HandlerFunc{}, fetch, ""},
		"a document not Base64":         {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, `<Invoice/>`)}, fetch, "einvoicing.unreadable"},
		"a JSON string not closed":      {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, `"PEl`)}, fetch, "einvoicing.unreadable"},
		"a document too large":          {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, huge)}, fetch, "einvoicing.document_too_large"},
		"an answer beyond the client's": {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, huge+strings.Repeat("A", 8<<10))}, fetch, "einvoicing.document_too_large"},
		"a document in Base64":          {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, doc+"\n")}, fetchIs("<Invoice/>"), "-"},
		"a document as a JSON string":   {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": answer(200, string(quoted))}, fetchIs("<Invoice/>"), "-"},
		"a listing cut off":             {map[string]http.HandlerFunc{"POST /invoice/search-documents": cutOff}, pending, ""},
		"a document cut off":            {map[string]http.HandlerFunc{"GET /invoice/get-document/D/XML": cutOff}, fetch, ""},
		"acknowledging, locally": {map[string]http.HandlerFunc{}, func(p einvoicing.Provider) error {
			p.(einvoicing.LocalAcknowledgement).AcknowledgesLocally()
			return p.Acknowledge(ctx, "D")
		}, "-"},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.call(fakeEvatra(t, c.routes))
			switch {
			case c.code == "-" && err != nil:
				t.Errorf("got %v, want done", err)
			case c.code == "" && (err == nil || !strings.HasPrefix(problem.From(err).Code, "platform.")): // the machinery's: internal or unavailable
				t.Errorf("got %v, want the call failed without a problem of its own", err)
			case c.code != "" && c.code != "-" && problem.From(err).Code != c.code:
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		client, err := einvoicing.NewClient(egress.LoopbackForTests(x509.NewCertPool()))
		if err != nil {
			t.Fatal(err)
		}
		p, err := einvoicing.Adapters(client, map[string]string{"prod": "https://127.0.0.1:1"}, nil)(einvoicing.Connector{Name: "E", Adapter: "evatra",
			Environment: "prod", Participants: map[string]string{"1000": usTIN}, Secret: evatraSecret})
		if err != nil {
			t.Fatal(err)
		}
		if err := pending(p); err == nil || !strings.Contains(err.Error(), "evatra login") {
			t.Errorf("got %v, want the login failed", err)
		}
	})
}

// TestEvatraTokensKept: a token is kept until a minute before its
// expirationDate when that is an RFC 3339 string; one dated otherwise (a
// number, a word) or already past serves its call and is not kept, so the
// next call logs in again.
func TestEvatraTokensKept(t *testing.T) {
	for name, c := range map[string]struct {
		expiration string
		logins     int
	}{
		"an hour ahead":     {`"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"`, 1},
		"a number":          {`1763600000000`, 2},
		"not RFC 3339":      {`"tomorrow"`, 2},
		"already past":      {`"2026-01-01T00:00:00Z"`, 2},
		"no expirationDate": {`null`, 2},
	} {
		t.Run(name, func(t *testing.T) {
			var logins atomic.Int32
			p := fakeEvatra(t, map[string]http.HandlerFunc{
				"POST /auth/login": func(w http.ResponseWriter, _ *http.Request) {
					logins.Add(1)
					_, _ = w.Write([]byte(`{"jwt": "j", "expirationDate": ` + c.expiration + `}`)) // the client's read is checked
				},
				"POST /invoice/search-documents": answer(200, `{"totalCount": 0, "invoiceList": []}`)})
			if err := twice(pending)(p); err != nil {
				t.Fatal(err)
			}
			if got := int(logins.Load()); got != c.logins {
				t.Errorf("logins for two listings: got %d, want %d", got, c.logins)
			}
		})
	}
}

// TestEvatraListing: documentId is read as a string or an integer, and a
// document a later page lists again (the pages shifted by a document
// received meanwhile) is listed once, where it was first, and is not
// counted twice: the listing reads on to the page holding the document
// the shift pushed there (batch 84 integration).
func TestEvatraListing(t *testing.T) {
	var page atomic.Int32
	pages := []string{`{"totalCount": 4, "invoiceList": [{"documentId": "A"}, {"documentId": 17}]}`,
		`{"totalCount": 4, "invoiceList": [{"documentId": 17}, {"documentId": "C"}]}`, // 17 again
		`{"totalCount": 4, "invoiceList": [{"documentId": "D"}]}`, `{"totalCount": 4, "invoiceList": []}`}
	p := fakeEvatra(t, map[string]http.HandlerFunc{"POST /invoice/search-documents": func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(pages[min(int(page.Add(1)), len(pages))-1])) // the client's read is checked
	}})
	ms, err := p.Pending(context.Background())
	if got := fmt.Sprint(ms); err != nil || got != "[{A "+usTIN+"} {17 "+usTIN+"} {C "+usTIN+"} {D "+usTIN+"}]" {
		t.Errorf("got %s %v, want A, 17, C and D once each", got, err)
	}
	if got := page.Load(); got != 3 {
		t.Errorf("pages read: got %d, want 3 (the count reached on the third)", got)
	}
}

// cutOff closes the connection without an answer.
func cutOff(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }

// twice calls call twice.
func twice(call func(p einvoicing.Provider) error) func(p einvoicing.Provider) error {
	return func(p einvoicing.Provider) error { return errors.Join(call(p), call(p)) }
}

func pending(p einvoicing.Provider) error {
	_, err := p.Pending(context.Background())
	return err
}

func fetch(p einvoicing.Provider) error {
	_, err := p.Fetch(context.Background(), "D")
	return err
}

// fetchIs fetches D and checks its bytes are want.
func fetchIs(want string) func(p einvoicing.Provider) error {
	return func(p einvoicing.Provider) error {
		got, err := p.Fetch(context.Background(), "D")
		if err == nil && string(got) != want {
			return fmt.Errorf("got %q, want %q", got, want)
		}
		return err
	}
}
