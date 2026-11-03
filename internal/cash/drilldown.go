package cash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The forecast's drill-down (ADR-0051 §9, T53e4): the flows of a currency
// in a range of days, optionally of one liquidity item and its
// descendants, as the forecast reads them, with the documents each pays.
// cash.forecast.read shows the flows and their amounts; a flow's supplier
// and documents are other contexts' data, shown only to a caller who may
// use their own read permission (authz.MayUse) and otherwise left out, as
// an expansion is (04 §4.4): the drill-down is never refused for them.

// Partners names business partners (partner context).
type Partners interface {
	// Names are the partners' numbers and names, keyed by id; an id that
	// names no partner is left out.
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PartnerName, error)
}

// PartnerName is a business partner by its number and name.
type PartnerName struct {
	ID           uuid.UUID
	Number, Name string
}

// supplierReadPermission is what a caller must be able to use to see a
// flow's supplier (partner.ReadPermission, which cash does not import).
const supplierReadPermission = "partner.supplier.read"

// documentReadPermissions are the read permissions of the contexts that
// own each kind of document, as their own reads require them.
var documentReadPermissions = map[string]string{
	DocJournalEntry:       "ledger.entry.read",
	DocSupplierInvoice:    "payables.invoice.read",
	DocDownPaymentRequest: "payables.invoice.read",
	DocPaymentRun:         "payments.run.read",
	DocMemoRecord:         "cash.memo.read",
}

// FlowsQuery is what a drill-down lists: the legal entity's flows in the
// currency from From (nil: the read date) to To (nil: no end); of the
// liquidity item and its descendants, of what no rule assigns
// (Unclassified), or every flow; the certainty cut and whether blocked
// suppliers' flows count in the total, as the forecast takes them; and a
// page of PageSize after the page token's.
type FlowsQuery struct {
	LegalEntity    uuid.UUID
	Currency       string
	From, To       *time.Time
	Item           string
	Unclassified   bool
	MinCertainty   string
	IncludeBlocked bool
	PageSize       int
	PageToken      string
}

// FlowPage is a page of a drill-down, with the total of the shares the
// forecast counts and the count of the flows over every page.
type FlowPage struct {
	LegalEntity  uuid.UUID
	ReadDate     time.Time
	TimeZone     string
	Currency     string
	Exponent     int
	From         time.Time
	MinCertainty string
	Total        money.Minor
	Count        int
	Flows        []ListedFlow
	Next         string // "" on the last page
}

// ListedFlow is a flow as the drill-down lists it: its day, source,
// certainty and lateness, whether the totals count it, its amount and the
// share of the item asked for (its amount when none was), its supplier
// (nil for a memo record or a caller who may not read suppliers), and the
// documents it pays that the caller may read, out of DocumentCount.
type ListedFlow struct {
	Date              time.Time
	Source, Certainty string
	Late, Counted     bool
	Amount, Share     money.Minor
	Supplier          *PartnerName
	DocumentCount     int
	Documents         []ListedDocument
}

// ListedDocument is a document a listed flow pays and what it pays of it.
type ListedDocument struct {
	Document
	Amount money.Minor
}

// flowCursor is the last flow of a page, by its key.
type flowCursor struct {
	After string `json:"after"`
}

// keyed is a flow the drill-down lists, with the share of the item asked
// for and its key: its day, then a digest of what it is, so pages follow
// each other by day without a token naming a document.
type keyed struct {
	f     datedFlow
	share money.Minor
	key   string
}

// Flows lists a page of the forecast's flows of a currency in a range of
// days (ADR-0051 §9 drill-down, T53e4). A legal entity the caller cannot
// act in is not found; a currency the reference data does not know, or a
// range ending before it starts, is refused.
func (e *Endpoints) Flows(ctx context.Context, q FlowsQuery) (FlowPage, error) {
	scope := fmt.Sprintf("listLiquidityForecastFlows|%s|%s|%s|%s|%s|%v|%s|%v", q.LegalEntity, q.Currency, dayText(q.From), dayText(q.To), q.Item,
		q.Unclassified, q.MinCertainty, q.IncludeBlocked)
	var c flowCursor
	if q.PageToken != "" {
		if err := e.Pages.Decode(scope, q.PageToken, &c); err != nil {
			return FlowPage{}, err
		}
	}
	fr, err := e.readForecast(ctx, q.LegalEntity, q.MinCertainty, func(r reader) (time.Time, error) { return drillHorizon(r, q) })
	if err != nil {
		return FlowPage{}, err
	}
	exp, err := currency.Exponent(ctx, fr.tx, q.Currency, problem.Unprocessable("cash.unknown_currency", "The currency is not known.",
		problem.FieldError{Field: "currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"}))
	if err != nil {
		return FlowPage{}, err
	}
	p := FlowPage{LegalEntity: q.LegalEntity, ReadDate: fr.r.read, TimeZone: fr.zone, Currency: q.Currency, Exponent: exp, From: fr.r.read,
		MinCertainty: fr.minCertainty, Flows: []ListedFlow{}}
	if q.From != nil {
		p.From = clock.Day(*q.From)
	}
	listed := fr.r.drill(fr.flows, q, p.From)
	p.Count = len(listed)
	for _, k := range listed {
		if counted(k.f, q.IncludeBlocked) {
			p.Total = fr.r.sums.add(p.Total, k.share)
		}
	}
	if fr.r.sums.overflow {
		return FlowPage{}, errForecastTooLarge
	}
	start, _ := slices.BinarySearchFunc(listed, c.After, func(k keyed, after string) int { return cmp.Compare(k.key, after) })
	if start < len(listed) && listed[start].key == c.After {
		start++
	}
	size := max(q.PageSize, 1)
	page := listed[start:min(start+size, len(listed))]
	if start+size < len(listed) {
		// Safe to ignore: a cursor of one string always encodes.
		p.Next, _ = e.Pages.Encode(scope, flowCursor{After: page[len(page)-1].key})
	}
	p.Flows, err = e.shown(ctx, page, q.IncludeBlocked)
	return p, err
}

// dayText is a day as the page token's scope names it, "" for none.
func dayText(d *time.Time) string {
	if d == nil {
		return ""
	}
	return clock.Day(*d).Format(time.DateOnly)
}

// drillHorizon is the last day a drill-down expands memo series to: its
// last day, else the company's last bucket's. A range ending before it
// starts, or an item asked for with what no rule assigns, is refused.
func drillHorizon(r reader, q FlowsQuery) (time.Time, error) {
	switch {
	case q.Item != "" && q.Unclassified:
		return time.Time{}, problem.Unprocessable("request.validation_failed", "A liquidity item and the unclassified shares are two different rows.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	case q.To == nil:
		bs := buckets(r.read, r.settings.BucketsOrDefault())
		return bs[len(bs)-1].End, nil
	case q.From != nil && clock.Day(*q.To).Before(clock.Day(*q.From)):
		return time.Time{}, problem.Unprocessable("request.validation_failed", "The range ends before it starts.",
			problem.FieldError{Field: "to", Code: "invalid", Message: "on or after from"})
	}
	return clock.Day(*q.To), nil
}

// counted reports whether the forecast's totals count the flow: a blocked
// supplier's only when asked.
func counted(f datedFlow, includeBlocked bool) bool {
	return includeBlocked || f.source != SourcePayableBlocked
}

// drill are the flows of the query's currency in its range with a share
// of what it asks for, by key: the item and its descendants, what no rule
// assigns, or the whole flow.
func (r reader) drill(flows []datedFlow, q FlowsQuery, from time.Time) []keyed {
	in := r.subtree(q.Item)
	var out []keyed
	for _, f := range flows {
		if f.currency != q.Currency || f.date.Before(from) || (q.To != nil && f.date.After(clock.Day(*q.To))) {
			continue
		}
		if share, ok := r.shareOf(f, q, in); ok {
			out = append(out, keyed{f: f, share: share, key: flowKey(f)})
		}
	}
	// Keys are unique: a supplier payment's documents are its own, and every
	// other flow carries its identity (a payment, a payment's line in
	// transit, a series' occurrence), so a key stays with its flow between
	// two reads and the next page starts after it.
	slices.SortFunc(out, func(a, b keyed) int { return cmp.Compare(a.key, b.key) })
	return out
}

// shareOf is the flow's share of what the query asks for, and whether it
// has one: its whole amount when the query names no row; else the sum of
// its shares of the unclassified row, or of the item's subtree (in).
func (r reader) shareOf(f datedFlow, q FlowsQuery, in map[string]bool) (money.Minor, bool) {
	if q.Item == "" && !q.Unclassified {
		return f.amount, true
	}
	var share money.Minor
	has := false
	for _, sh := range f.shares {
		if (q.Unclassified && sh.Item == "") || (!q.Unclassified && in[sh.Item]) {
			has, share = true, r.sums.add(share, sh.Amount)
		}
	}
	return share, has
}

// subtree is the item and its descendants in the active document's tree:
// a code it does not have is itself alone, which no flow names.
func (r reader) subtree(item string) map[string]bool {
	in := map[string]bool{item: true}
	for grew := true; grew; {
		grew = false
		for _, it := range r.doc.Cash.LiquidityItems {
			if in[it.Parent] && !in[it.Code] {
				in[it.Code], grew = true, true
			}
		}
	}
	return in
}

// flowKey is a flow's place in the drill-down: its day, then a digest of
// its source, certainty, identity and documents, which tells flows of a
// day apart without naming a document a page token would show.
func flowKey(f datedFlow) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%v|%s", f.source, f.certainty, f.late, f.identity)
	for _, d := range f.documents {
		fmt.Fprintf(h, "|%s:%s", d.Kind, d.ID)
	}
	return f.date.Format(time.DateOnly) + "|" + hex.EncodeToString(h.Sum(nil))[:32]
}

// shown are the page's flows as the caller may see them: each supplier
// only with partner.supplier.read usable, each document only with its
// context's read permission usable; the permissions are read once a
// page, and the names only of a page that shows suppliers.
func (e *Endpoints) shown(ctx context.Context, page []keyed, includeBlocked bool) ([]ListedFlow, error) {
	may := map[string]bool{}
	mayUse := func(permission string) (bool, error) {
		if v, ok := may[permission]; ok {
			return v, nil
		}
		v, err := authz.MayUse(ctx, permission)
		if err != nil {
			return false, fmt.Errorf("cash: may the caller use %s: %w", permission, err)
		}
		may[permission] = v
		return v, nil
	}
	names, err := e.supplierNames(ctx, page, mayUse)
	if err != nil {
		return nil, err
	}
	out := make([]ListedFlow, len(page))
	for i, k := range page {
		f := k.f
		lf := ListedFlow{Date: f.date, Source: f.source, Certainty: f.certainty, Late: f.late, Counted: counted(f, includeBlocked), Amount: f.amount,
			Share: k.share, DocumentCount: len(f.documents), Documents: []ListedDocument{}}
		if f.supplier != nil {
			if n, ok := names[*f.supplier]; ok {
				lf.Supplier = &n
			}
		}
		for _, d := range f.documents {
			v, err := mayUse(documentReadPermissions[d.Kind])
			if err != nil {
				return nil, err
			}
			if v {
				lf.Documents = append(lf.Documents, ListedDocument{Document: d.Document, Amount: d.amount})
			}
		}
		out[i] = lf
	}
	return out, nil
}

// supplierNames are the page's suppliers by id, read only when the page
// names one and the caller may read suppliers; none otherwise.
func (e *Endpoints) supplierNames(ctx context.Context, page []keyed, mayUse func(string) (bool, error)) (map[uuid.UUID]PartnerName, error) {
	var ids []uuid.UUID
	for _, k := range page {
		if k.f.supplier != nil && !slices.Contains(ids, *k.f.supplier) {
			ids = append(ids, *k.f.supplier)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if v, err := mayUse(supplierReadPermission); err != nil || !v {
		return nil, err
	}
	names, err := e.Partners.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("cash: names of the forecast's suppliers: %w", err)
	}
	return names, nil
}
