package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The objects a request's object_resource places (T73d3): an order change,
// read through its order, and a bank clearing's match, read among its
// statement's.

// placedData is what placedStack's tenant holds.
type placedData struct {
	change, clearing uuid.UUID    // the pending order_change and bank_clearing requests
	statement        uuid.UUID    // the clearing's statement
	reader           pgtest.Actor // may read approval requests, not their objects
}

// placedStack is the workspace in front of a configured tenant whose
// human asked for approval of an order change and whose house bank's
// statement waits for approval of a line's posting.
func placedStack(t *testing.T) (*stack, *placedData) {
	t.Helper()
	d := &placedData{}
	objects := blobstore.Dir{Root: t.TempDir()}
	s := newStack(t, stackOptions{permissions: append([]string{"procurement.order.read", "procurement.order.create"}, fixture.StatementPermissions...),
		objects: objects, seed: func(f fixture.Env) {
			f.Reconciles(t)
			d.change = f.OrderChange(t, f.NewSupplier(t, "70001"), "4500000901")
			d.statement, d.clearing = f.Clearing(t, objects)
			d.reader = roleHolder(t, f, "reader", "workflow.request.read")
		}})
	return s, d
}

// TestPlacedObjects: an order change's and a bank clearing's requests show
// their objects as their own reads give them, in English and French, the
// clearing linked to its line on the statement's page; a person who may
// not read the objects is told so.
func TestPlacedObjects(t *testing.T) {
	s, d := placedStack(t)
	b := s.browser()
	b.signIn()
	reader := s.as(d.reader)
	for name, tc := range map[string]struct {
		b    *browser
		id   uuid.UUID
		lang string
		want []string
		link string
	}{
		"order change": {b, d.change, "en", []string{"Purchase order change", "Reason given Paper for the whole year", "Total before the change EUR 135.00",
			"Total after the change EUR 1,215.00", "Date 2026-03-01", "Payment terms NT30", "Items 2", "Status pending_approval"}, ""},
		"order change, French": {b, d.change, "fr-CA", []string{"Total avant la modification 135,00 EUR", "Total après la modification 1 215,00 EUR",
			"Conditions de paiement NT30"}, ""},
		"bank clearing": {b, d.clearing, "en", []string{"Bank statement clearing", "Line 1", "How it was matched Matched by a rule",
			"Matching rule Transaction code", "Reason given", "What it clears Posting rule NCHG", "Posting date 2026-04-17", "Status Matched",
			"Open the line on its statement"}, statementPath(d.statement) + "#line-1"},
		"bank clearing, French": {b, d.clearing, "fr-CA", []string{"Ligne 1", "Mode de rapprochement Rapprochée par une règle",
			"Ce qu'il solde Règle de comptabilisation NCHG", "Ouvrir la ligne dans son relevé"}, statementPath(d.statement) + "#line-1"},
		"order change, not readable":  {reader, d.change, "en", []string{"You may not read this object itself"}, ""},
		"bank clearing, not readable": {reader, d.clearing, "en", []string{"You may not read this object itself"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := tc.b.get(requestPath(tc.id), "Accept-Language", tc.lang)
			status(t, name, r, http.StatusOK)
			sections := r.page(t).all("section", "object")
			if len(sections) != 1 {
				t.Fatalf("object sections: got %d, want 1", len(sections))
			}
			holds(t, name, strings.Join(strings.Fields(strings.ReplaceAll(text(sections[0]), " ", " ")), " "), tc.want...)
			var links []string
			walk(sections[0], func(n *html.Node) {
				if n.Data == "a" {
					links = append(links, attrOf(n, "href"))
				}
			})
			if got := strings.Join(links, " "); got != tc.link {
				t.Errorf("links: got %q, want %q", got, tc.link)
			}
		})
	}
}

// placedJSON is a pending request for the object of the kind, placed at
// resource ("" none).
func placedJSON(id uuid.UUID, kind string, object uuid.UUID, resource string) string {
	u := uuid.NewString
	placed := ""
	if resource != "" {
		placed = fmt.Sprintf(`"object_resource":%q,`, resource)
	}
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"object_type":%q,"object_id":%q,%s"title":"Stubbed","requester_id":%q,"status":"pending","current_step":1,
		"steps":[{"step_no":1,"role_id":%q,"required":1,"approvals":0}],"decisions":[]}`, id, u(), kind, object, placed, u(), u())
}

// matchJSON is a statement's match.
func matchJSON(id, statement uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"bank_statement_id":%q,"line_no":3,"kind":"manual","reason":"By hand","status":"matched","targets":[{"candidate":1,"incoming":true}]}`, id, u(), statement)
}

// TestObjectPlacedOrNot: a request's object is read where its
// object_resource places it, and only there: a request without a
// resource, one in another shape, an order change placed under another
// change's id and a match its statement does not list each say the object
// is not placed, and read nothing more; a read the API refuses is shown in
// place, one it cannot answer makes the page unavailable.
func TestObjectPlacedOrNot(t *testing.T) {
	id, order, object, statement := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	changeAt := func(change uuid.UUID) string {
		return "/v1/purchase-orders/" + order.String() + "/changes/" + change.String()
	}
	statementAt := "/v1/bank-statements/" + statement.String()
	matches := `{"items":[` + matchJSON(uuid.New(), statement) + `]}`
	found := `{"items":[` + matchJSON(uuid.New(), statement) + `,` + matchJSON(object, statement) + `]}`
	for name, tc := range map[string]struct {
		kind, resource string
		read           http.HandlerFunc
		status         int
		want           string
	}{
		"change, no resource":      {"order_change", "", nil, http.StatusOK, "does not say where its object is"},
		"change, another change":   {"order_change", changeAt(uuid.New()), nil, http.StatusOK, "does not say where its object is"},
		"change, another shape":    {"order_change", statementAt, nil, http.StatusOK, "does not say where its object is"},
		"change, not found":        {"order_change", changeAt(object), answer(404, problemJSON(404, "request.not_found")), http.StatusOK, "The object does not exist"},
		"clearing, braced id":      {"bank_clearing", "/v1/bank-statements/{" + statement.String() + "}", nil, http.StatusOK, "does not say where its object is"},
		"clearing, another prefix": {"bank_clearing", "/v1/bank-statement/" + statement.String(), nil, http.StatusOK, "does not say where its object is"},
		"clearing, not listed":     {"bank_clearing", statementAt, answer(200, matches), http.StatusOK, "does not say where its object is"},
		"clearing, listed":         {"bank_clearing", statementAt, answer(200, found), http.StatusOK, "An incoming payment"},
		"clearing, not found":      {"bank_clearing", statementAt, answer(404, problemJSON(404, "request.not_found")), http.StatusOK, "The object does not exist"},
		"clearing, unavailable":    {"bank_clearing", statementAt, answer(503, problemJSON(503, "platform.unavailable")), http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, meJSON(permReadApprovals, permReadOrders, permReadStatements)),
				"GET /v1/principal-names":                                    answer(200, `{"items":[]}`),
				"GET /v1/approval-requests/":                                 answer(200, placedJSON(id, tc.kind, object, tc.resource)),
				"GET /v1/purchase-orders/":                                   tc.read,
				"GET /v1/bank-statements/" + statement.String() + "/matches": tc.read}}
			_, b, _ := stubStack(t, st)
			r := b.get(requestPath(id))
			status(t, name, r, tc.status)
			if tc.want != "" {
				holds(t, name, r.page(t).mainText(), tc.want)
			}
			if tc.read == nil && len(st.queries) != 3 {
				t.Errorf("calls: got %d, want the request, its people's names and the person alone", len(st.queries))
			}
		})
	}
}

// TestPlaced: the ids in a resource of a template's shape, each a UUID as
// the API writes it; nothing for any other resource.
func TestPlaced(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		resource string
		want     []uuid.UUID
	}{
		"change":         {"/v1/purchase-orders/" + a.String() + "/changes/" + b.String(), []uuid.UUID{a, b}},
		"none":           {"", nil},
		"too short":      {"/v1/purchase-orders/" + a.String(), nil},
		"other literal":  {"/v1/purchase-orders/" + a.String() + "/lines/" + b.String(), nil},
		"upper case":     {"/v1/purchase-orders/" + strings.ToUpper(a.String()) + "/changes/" + b.String(), nil},
		"not a uuid":     {"/v1/purchase-orders/x/changes/" + b.String(), nil},
		"trailing slash": {"/v1/purchase-orders/" + a.String() + "/changes/" + b.String() + "/", nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := gen.ApprovalRequest{}
			if tc.resource != "" {
				r.ObjectResource = gen.NewOptString(tc.resource)
			}
			got, ok := placed(r, changeResource)
			if ok != (tc.want != nil) || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("placed: got %v %v, want %v", got, ok, tc.want)
			}
		})
	}
}

// TestPlacedSummaries: an order change's and a bank clearing's summaries,
// a match's reason and the reason for its posting date shown as data, and
// what each kind of match clears.
func TestPlacedSummaries(t *testing.T) {
	srv, _ := testServer(t)
	ctx := i18n.With(context.Background(), srv.Catalog.Match("en"))
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	eur := func(a string) gen.Money { return gen.Money{Amount: a, Currency: "EUR"} }
	statement := uuid.New()
	match := func(kind string, targets ...gen.StatementMatchTarget) *gen.StatementMatch {
		return &gen.StatementMatch{BankStatementID: statement, LineNo: 4, Kind: kind, Reason: "<b>Checked</b>", Status: "matched", Targets: targets}
	}
	ruled := match("rule", gen.StatementMatchTarget{Candidate: 1, PostingRule: gen.NewOptString("NCHG")})
	ruled.Rule = gen.NewOptString("transaction_code")
	dated := match("manual", gen.StatementMatchTarget{Candidate: 1, PaymentID: gen.NewOptUUID(uuid.New())},
		gen.StatementMatchTarget{Candidate: 1, PaymentID: gen.NewOptUUID(uuid.New())})
	dated.PostingDate, dated.PostingReason = gen.NewOptDate(day), gen.NewOptString("Booked <i>late</i>")
	for name, tc := range map[string]struct {
		res  any
		want []string
		link string
	}{
		"order change": {&gen.GetPurchaseOrderChangeOKHeaders{Response: gen.PurchaseOrderChange{Reason: "<b>Dearer</b>", OrderDate: day, PaymentTermCode: "NT30",
			Items: make([]gen.PurchaseOrderItem, 3), TotalBefore: eur("100.00"), TotalAfter: eur("1500.00"), Status: "pending_approval"}},
			[]string{"Reason given <b>Dearer</b>", "Total before the change EUR 100.00", "Total after the change EUR 1,500.00", "Payment terms NT30",
				"Items 3"}, ""},
		"by a rule": {ruled, []string{"Line 4", "How it was matched Matched by a rule", "Matching rule Transaction code", "Reason given <b>Checked</b>",
			"What it clears Posting rule NCHG", "Status Matched"}, statementPath(statement) + "#line-4"},
		"payments": {dated, []string{"Matched by hand", "What it clears 2 payments", "Posting date 2026-10-22", "Why on that date Booked <i>late</i>"},
			statementPath(statement) + "#line-4"},
		"one payment": {match("agent", gen.StatementMatchTarget{Candidate: 1, PaymentID: gen.NewOptUUID(uuid.New())}),
			[]string{"The reconciliation agent's proposal", "What it clears 1 payment"}, statementPath(statement) + "#line-4"},
		"incoming": {match("new_kind", gen.StatementMatchTarget{Candidate: 1, Incoming: gen.NewOptBool(true)}),
			[]string{"How it was matched new_kind", "What it clears An incoming payment"}, statementPath(statement) + "#line-4"},
		"nothing": {match("manual"), []string{"What it clears Nothing"}, statementPath(statement) + "#line-4"},
	} {
		t.Run(name, func(t *testing.T) {
			sum, ok := summaryOf(tc.res)
			if !ok {
				t.Fatal("no summary")
			}
			var buf bytes.Buffer
			if err := ui.SummaryList(sum.Rows).Render(ctx, &buf); err != nil {
				t.Fatal(err)
			}
			doc, err := html.Parse(&buf)
			if err != nil {
				t.Fatal(err)
			}
			holds(t, name, text(doc), tc.want...)
			if sum.Link != tc.link {
				t.Errorf("link: got %q, want %q", sum.Link, tc.link)
			}
		})
	}
}

// TestObjectReadsPlacedByTheContract: a read that follows a request's
// object_resource takes it in the shape of a path of the contract, the
// shape the API gives the resource in, and reads at that path or under it
// (a statement's matches under the statement).
func TestObjectReadsPlacedByTheContract(t *testing.T) {
	ops, _ := contract(t)
	paths := map[string]bool{}
	for _, op := range ops {
		paths[op.Path] = true
	}
	for kind, read := range objectReads {
		if read.path == "" {
			continue
		}
		if !paths[read.path] {
			t.Errorf("%s: placed at %s, no path of the contract", kind, read.path)
		}
		if got := ops[read.op.ID].Path; got != read.path && !strings.HasPrefix(got, read.path+"/") {
			t.Errorf("%s: %s reads at %s, want at or under %s", kind, read.op.ID, got, read.path)
		}
	}
}
