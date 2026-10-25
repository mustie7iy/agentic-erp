package fixture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// JournalPermissions are the permissions of the GL accountant who enters
// manual journal entries (T35, T73h), besides reading the companies and
// the partners a reconciliation line names: reading, parking and posting
// entries, the accounts and centers a line names, and the change of
// centers through which Centers sets them up.
var JournalPermissions = []string{"ledger.entry.read", "ledger.entry.park", "ledger.entry.post", "ledger.account.read", "ledger.cost_center.read",
	"ledger.cost_center.change"}

// JournalCenters are the centers Centers gives company 2000.
type JournalCenters struct {
	Segment, Profit, Cost uuid.UUID
}

// Centers gives company 2000, through the API as a controller's changes
// are (ADR-0050), segment SEG1, profit center PC10 of SEG1 and cost
// center CC100 (ADMIN) of PC10, each valid from 1 January 2026: the
// tenant's human requests each change and the approver approves it. The
// tenant needs ledger.cost_center.change and .read.
func (e Env) Centers(tb testing.TB) JournalCenters {
	tb.Helper()
	var c JournalCenters
	node := func(kind string) string {
		h, tag := e.Must(201, "POST", "/v1/hierarchies", fmt.Sprintf(`{"kind": %q, "code": "STANDARD", "name": "Standard", "is_standard": true}`, kind), "")
		ch, _ := e.Must(200, "POST", fmt.Sprintf("/v1/hierarchies/%v/nodes/%v:add", h["id"], h["root_node_id"]), `{"code": "LEAF", "name": "Leaf", "reason": "r"}`, tag)
		e.approveChange(tb, ch["id"])
		return ch["payload"].(map[string]any)["node_id"].(string)
	}
	ccNode, pcNode := node("cost_center"), node("profit_center")
	seg, _ := e.Must(201, "POST", "/v1/segments", `{"code": "SEG1", "name": "Segment one"}`, "")
	c.Segment = uuid.MustParse(seg["id"].(string))
	pc, _ := e.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC10", "name": "Profit center ten", "responsible": "Jane Doe",
		"segment_id": %q, "legal_entity_ids": [%q], "valid_from": "2026-01-01", "standard_node_id": %q, "reason": "New region"}`, c.Segment, e.DE, pcNode), "")
	e.approveChange(tb, pc["pending_change_id"])
	c.Profit = uuid.MustParse(pc["id"].(string))
	cc, _ := e.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "CC100", "name": "Cost center hundred",
		"responsible": "Jane Doe", "category": "ADMIN", "profit_center_id": %q, "valid_from": "2026-01-01", "standard_node_id": %q, "reason": "New office"}`,
		e.DE, c.Profit, ccNode), "")
	e.approveChange(tb, cc["pending_change_id"])
	c.Cost = uuid.MustParse(cc["id"].(string))
	return c
}

// approveChange has the approver approve a center change's pending
// request.
func (e Env) approveChange(tb testing.TB, change any) {
	tb.Helper()
	r := e.Pending(tb, fmt.Sprint(change))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, `"`+jsonNumber(r["version"])+`"`)
}

// Park parks, as the client's principal, a manual entry of company 2000
// in EUR on the day with the lines given, each an account number of
// company 2000's chart and a signed amount, and returns its id.
func (e Env) Park(tb testing.TB, day, header string, lines ...[2]string) uuid.UUID {
	tb.Helper()
	body := ""
	for i, l := range lines {
		if i > 0 {
			body += ", "
		}
		body += fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}`, e.Account(tb, "2000", l[0]), l[1])
	}
	pe, _ := e.Must(201, "POST", "/v1/parked-journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q,
		"currency": "EUR", "header_text": %q, "lines": [%s]}`, e.DE, day, day, header, body), "")
	return uuid.MustParse(pe["id"].(string))
}

// PostedBy posts, as a context does through ledger.Postings, an entry of
// company 2000 of amount minor units of EUR on the day from office
// supplies 520000 to bank 100000, with source the context's and a source
// document of its own, and returns the entry's id: an entry the journal's
// reversal refuses when another context owns it.
func (e Env) PostedBy(tb testing.TB, source string, day string, amount money.Minor) uuid.UUID {
	tb.Helper()
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		tb.Fatal(err)
	}
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	var id uuid.UUID
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		entry, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: d, PostingDate: d, Currency: "EUR",
			SourceContext: source, SourceDocumentID: uuid.Must(uuid.NewV7()),
			Lines: []ledger.PostingLine{{AccountID: e.Account(tb, "2000", "520000"), Amount: amount}, {AccountID: e.Account(tb, "2000", "100000"), Amount: -amount}}})
		id = entry.ID
		return err
	}); err != nil {
		tb.Fatal(err)
	}
	return id
}
