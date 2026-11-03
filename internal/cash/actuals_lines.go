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

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The lines behind the cash actuals (ADR-0051 §11, T53f2): a currency's
// statement lines in a range of value dates, optionally of one house bank,
// of one liquidity item and its descendants or with no decided match, as
// the actuals classify them. cash.forecast.read shows a line's day, bank,
// amount and shares; its statement, payments and suppliers are the
// payments and partner contexts' data, shown only to a caller who may use
// their read permissions (authz.MayUse), as the forecast's drill-down
// shows its documents, and otherwise left out, never refused.

// statementReadPermission is what a caller must be able to use to see a
// line's statement (payments context; cash does not import it).
const statementReadPermission = "payments.statement.read"

// ActualLinesQuery is what a page of lines lists: the legal entity's lines
// in the currency whose value date is from From to To, of the house bank
// (nil: every bank), of the liquidity item and its descendants, or with
// no decided match (Unclassified), else every line; a page of PageSize
// after the page token's.
type ActualLinesQuery struct {
	LegalEntity  uuid.UUID
	Currency     string
	From, To     time.Time
	HouseBank    *uuid.UUID
	Item         string
	Unclassified bool
	PageSize     int
	PageToken    string
}

// ActualLinePage is a page of lines, with the total of the selected
// shares and the count of the lines over every page.
type ActualLinePage struct {
	LegalEntity uuid.UUID
	CompanyCode string
	Currency    string
	Exponent    int
	From, To    time.Time
	Total       money.Minor
	Count       int
	Lines       []ListedActual
	Next        string // "" on the last page
}

// ListedActual is a line as the page lists it: Share is its share of the
// item asked for (its amount when none was); Statement, Payments and
// Suppliers are what the caller may read, out of PaymentCount payments.
type ListedActual struct {
	ValueDate    time.Time
	HouseBankID  uuid.UUID
	BankCode     string
	Statement    *StatementRef
	Status       string
	Match        string
	PostingRule  string
	Unclassified bool
	Amount       money.Minor
	Share        money.Minor
	Shares       []Share
	PaymentCount int
	Payments     []ListedPayment
	Suppliers    []PartnerName
}

// StatementRef is a bank statement's line by its statement and number,
// with the statement's reference and the line's at the bank.
type StatementRef struct {
	ID            uuid.UUID
	LineNo        int32
	Reference     string
	BankReference string
}

// ListedPayment is a payment a line was matched to, with its run, what
// it paid, signed as the line (negative: money out), and its supplier (nil
// for a caller who may not read suppliers).
type ListedPayment struct {
	ID, RunID uuid.UUID
	RunNumber string
	Amount    money.Minor
	Supplier  *PartnerName
}

// keyedActual is a line the page lists, with its share of the item asked
// for and its key: its value date, then a digest of its statement and
// number, so pages follow each other by day without a token naming a
// statement.
type keyedActual struct {
	a     actual
	share money.Minor
	key   string
}

// ActualLines lists a page of the actuals' lines of a currency. A legal
// entity the caller cannot act in is not found; a range ending before it
// starts or longer than 36 months, or an item asked for with the
// unclassified lines, is refused.
func (e *Endpoints) ActualLines(ctx context.Context, q ActualLinesQuery) (ActualLinePage, error) {
	scope := fmt.Sprintf("listCashActualLines|%s|%s|%s|%s|%s|%s|%t", q.LegalEntity, q.Currency, dayText(&q.From), dayText(&q.To), bankText(q.HouseBank),
		q.Item, q.Unclassified)
	var c flowCursor
	if q.PageToken != "" {
		if err := e.Pages.Decode(scope, q.PageToken, &c); err != nil {
			return ActualLinePage{}, err
		}
	}
	if q.Item != "" && q.Unclassified {
		return ActualLinePage{}, problem.Unprocessable("request.validation_failed", "A liquidity item and the unclassified lines are two different selections.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	}
	if _, err := actualBuckets(q.From, q.To, config.GrainMonth); err != nil {
		return ActualLinePage{}, err
	}
	ar, err := e.readActuals(ctx, q.LegalEntity, clock.Day(q.From), clock.Day(q.To))
	if err != nil {
		return ActualLinePage{}, err
	}
	p := ActualLinePage{LegalEntity: q.LegalEntity, CompanyCode: ar.co.Code, Currency: q.Currency, From: clock.Day(q.From), To: clock.Day(q.To),
		Lines: []ListedActual{}}
	if p.Exponent, err = currencyExponent(ctx, ar.tx, q.Currency); err != nil {
		return ActualLinePage{}, err
	}
	listed := ar.listed(q)
	p.Count = len(listed)
	for _, k := range listed {
		p.Total = ar.r.sums.add(p.Total, k.share)
	}
	if ar.r.sums.overflow {
		return ActualLinePage{}, errActualsTooLarge
	}
	start, _ := slices.BinarySearchFunc(listed, c.After, func(k keyedActual, after string) int { return cmp.Compare(k.key, after) })
	if start < len(listed) && listed[start].key == c.After {
		start++
	}
	size := max(q.PageSize, 1)
	page := listed[start:min(start+size, len(listed))]
	if start+size < len(listed) {
		// Safe to ignore: a cursor of one string always encodes.
		p.Next, _ = e.Pages.Encode(scope, flowCursor{After: page[len(page)-1].key})
	}
	p.Lines, err = e.shownActuals(ctx, page)
	return p, err
}

// bankText is a house bank as the page token's scope names it, "" for
// none.
func bankText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// listed are the lines of the query's currency and bank with a share of
// what it asks for, by key: the item and its descendants, the lines with
// no decided match, or every line whole.
func (ar actualsRead) listed(q ActualLinesQuery) []keyedActual {
	in := ar.r.subtree(q.Item)
	var out []keyedActual
	for _, a := range ar.lines {
		if a.Currency != q.Currency || (q.HouseBank != nil && a.HouseBankID != *q.HouseBank) || (q.Unclassified && !a.unclassified()) {
			continue
		}
		share, has := a.Amount, true
		if q.Item != "" {
			share, has = 0, false
			for _, sh := range a.shares {
				if in[sh.Item] {
					share, has = ar.r.sums.add(share, sh.Amount), true
				}
			}
		}
		if has {
			out = append(out, keyedActual{a: a, share: share, key: actualKey(a)})
		}
	}
	slices.SortFunc(out, func(a, b keyedActual) int { return cmp.Compare(a.key, b.key) })
	return out
}

// actualKey is a line's place in the list: its value date, then a digest
// of its statement and number, unique and stable between two reads.
func actualKey(a actual) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%d", a.StatementID, a.LineNo))
	return a.ValueDate.Format(time.DateOnly) + "|" + hex.EncodeToString(h[:])[:32]
}

// shownActuals are the page's lines as the caller may see them: the
// statement with payments.statement.read usable, the payments with
// payments.run.read, their suppliers with partner.supplier.read; the
// permissions are read once a page, the names only of a page that shows
// suppliers.
func (e *Endpoints) shownActuals(ctx context.Context, page []keyedActual) ([]ListedActual, error) {
	may := mayUse(ctx)
	var suppliers []uuid.UUID
	for _, k := range page {
		for _, p := range k.a.Payments {
			if !slices.Contains(suppliers, p.SupplierID) {
				suppliers = append(suppliers, p.SupplierID)
			}
		}
	}
	names, err := e.partnerNames(ctx, suppliers, may)
	if err != nil {
		return nil, err
	}
	statements, err := may(statementReadPermission)
	if err != nil {
		return nil, err
	}
	payments, err := may(documentReadPermissions[DocPaymentRun])
	if err != nil {
		return nil, err
	}
	out := make([]ListedActual, len(page))
	for i, k := range page {
		out[i] = shownActual(k, names, statements, payments)
	}
	return out, nil
}

// shownActual is a line with what the caller may see of it: its statement
// and payments when they may be shown, each supplier named once.
func shownActual(k keyedActual, names map[uuid.UUID]PartnerName, statements, payments bool) ListedActual {
	a := k.a
	l := ListedActual{ValueDate: a.ValueDate, HouseBankID: a.HouseBankID, BankCode: a.bankCode, Status: a.Status, Match: a.Match, PostingRule: a.PostingRule,
		Unclassified: a.unclassified(), Amount: a.Amount, Share: k.share, Shares: a.shares, PaymentCount: len(a.Payments), Payments: []ListedPayment{},
		Suppliers: []PartnerName{}}
	if statements {
		l.Statement = &StatementRef{ID: a.StatementID, LineNo: a.LineNo, Reference: a.StatementRef, BankReference: a.BankReference}
	}
	var named []uuid.UUID
	for _, p := range a.Payments {
		n, ok := names[p.SupplierID]
		if payments {
			lp := ListedPayment{ID: p.ID, RunID: p.RunID, RunNumber: p.RunNumber, Amount: -p.Amount}
			if ok {
				lp.Supplier = &n
			}
			l.Payments = append(l.Payments, lp)
		}
		if ok && !slices.Contains(named, p.SupplierID) {
			named, l.Suppliers = append(named, p.SupplierID), append(l.Suppliers, n)
		}
	}
	return l
}
