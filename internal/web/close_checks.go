package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The pre-close checks of a period (ADR-0052 §2): before a period is
// closed, the GR/IR positions still open, the ledger's reconciliation
// with another ledger the company keeps (ADR-0045) and the company's bank
// statements not reconciled (ADR-0047), each with its result; a statement
// links to its page and its bank's reconciliation report, the screens
// that exist. The GR/IR and ledger reports are T73j's, so their checks
// show their results in place. It is a section htmx loads into the
// period's page (HX-Request), or a page of its own for a browser without
// htmx; with getCurrentPrincipal and listLedgers it makes six calls, the
// statements read once per status not reconciled.

// checkSize is how many positions, differences and statements a check
// shows; more are said to be there.
const checkSize = 10

// checkQuery is the company, ledger, year and period a check is of.
type checkQuery struct {
	company, ledger uuid.UUID
	year, period    int32
}

// parseChecks is the query's company, ledger, year and period, and
// whether it names them.
func parseChecks(q url.Values) (checkQuery, bool) {
	company, err1 := uuid.Parse(q.Get(ui.FieldCompany))
	ledger, err2 := uuid.Parse(q.Get(ui.FieldLedger))
	year, err3 := strconv.ParseInt(q.Get(ui.FieldYear), 10, 32)
	period, err4 := strconv.ParseInt(q.Get(ui.FieldPeriod), 10, 32)
	return checkQuery{company: company, ledger: ledger, year: int32(year), period: int32(period)},
		err1 == nil && err2 == nil && err3 == nil && err4 == nil
}

// checks is the pre-close checks the query names.
func (s *Server) checks(w http.ResponseWriter, r *http.Request, p *person) {
	w.Header().Add("Vary", "HX-Request") // its refusals too: a page or a section
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	var v ui.ChecksView
	if id, err := uuid.Parse(q.Get("period_id")); err == nil {
		v.Period = periodPath(id)
	}
	status := http.StatusOK
	if cq, valid := parseChecks(q); !valid {
		v.Invalid, status = true, http.StatusBadRequest
	} else if !s.runChecks(w, r, p, m, cq, &v) {
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, status, ui.ChecksSection(v))
		return
	}
	s.render(w, r, status, ui.ChecksPage(s.page(i18n.ChecksTitle, ui.ClosePath, p, m), v))
}

// runChecks runs the three checks. ok is false when the answer is already
// written; a refusal is shown in its check's place.
func (s *Server) runChecks(w http.ResponseWriter, r *http.Request, p *person, m me, cq checkQuery, v *ui.ChecksView) bool {
	refused := func(operation string, res any, err error, id string) (*ui.Refusal, bool) {
		return s.inPlace(w, r, p, m, i18n.PeriodProblems, operation, res, err, i18n.ChecksRefusedTitle, id)
	}
	ctx := r.Context()
	res, err := s.API.ListGrIrBalances(ctx, gen.ListGrIrBalancesParams{LegalEntityID: cq.company, OpenOnly: gen.NewOptBool(true),
		PageSize: gen.NewOptInt32(checkSize), OrderBy: gen.NewOptString("last_posting_date")})
	var ok bool
	if page, isPage := res.(*gen.GrIrBalancePage); isPage {
		v.GRIR = grirCheck(*page)
	} else if v.GRIR.Refusal, ok = refused("listGrIrBalances", res, err, "grir-refusal"); !ok {
		return false
	}
	if !s.ledgerCheck(r, cq, s.ledgers(ctx, m), v, refused) {
		return false
	}
	for _, st := range []string{"imported", "gap"} {
		sres, err := s.API.ListBankStatements(ctx, gen.ListBankStatementsParams{LegalEntityID: gen.NewOptUUID(cq.company), Status: gen.NewOptString(st),
			PageSize: gen.NewOptInt32(checkSize), OrderBy: gen.NewOptString("closing_date desc")})
		page, isPage := sres.(*gen.BankStatementPage)
		if !isPage {
			v.Statements.Refusal, ok = refused("listBankStatements", sres, err, "statements-refusal")
			return ok
		}
		v.Statements.Rows = append(v.Statements.Rows, statementRows(*page, nil, nil)...)
		v.Statements.More = v.Statements.More || page.NextPageToken.Set
	}
	v.Statements.Imported = ui.StatementsPath(url.Values{"status": {"imported"}})
	v.Statements.Gap = ui.StatementsPath(url.Values{"status": {"gap"}})
	return true
}

// grirCheck is a page of the open GR/IR positions.
func grirCheck(page gen.GrIrBalancePage) ui.GRIRCheck {
	c := ui.GRIRCheck{More: page.NextPageToken.Set}
	for _, b := range page.Items {
		c.Positions = append(c.Positions, ui.GRIRPosition{Order: b.PurchaseOrderID.String(), Item: b.PoItemNo, Received: b.ReceivedQuantity.Value,
			Invoiced: b.InvoicedQuantity.Value, Unit: b.ReceivedQuantity.Unit, Balance: moneyOf(b.Balance), Last: b.LastPostingDate})
	}
	return c
}

// ledgerCheck reconciles the period's ledger with another the company
// keeps: a parallel ledger with the leading one, the leading ledger with
// the company's first parallel one by code. ok is false when the answer
// is already written.
func (s *Server) ledgerCheck(r *http.Request, cq checkQuery, ledgers []gen.Ledger, v *ui.ChecksView,
	refused func(string, any, error, string) (*ui.Refusal, bool)) bool {
	kept := slices.DeleteFunc(slices.Clone(ledgers), func(l gen.Ledger) bool { return !slices.Contains(l.LegalEntityIds, cq.company) })
	ledger, compare, found := compared(kept, cq.ledger)
	switch {
	case ledgers == nil:
		v.Ledgers.Unknown = true
		return true
	case !found:
		v.Ledgers.Single = true
		return true
	}
	v.Ledgers.Ledger, v.Ledgers.Compare = ledger.Code, compare.Code
	res, err := s.API.ListLedgerReconciliation(r.Context(), gen.ListLedgerReconciliationParams{LegalEntityID: cq.company, LedgerID: gen.NewOptUUID(ledger.ID),
		CompareLedgerID: compare.ID, FiscalYear: cq.year, FiscalPeriod: gen.NewOptInt32(cq.period), PageSize: gen.NewOptInt32(checkSize)})
	page, ok := res.(*gen.LedgerReconciliationPage)
	if !ok {
		v.Ledgers.Refusal, ok = refused("listLedgerReconciliation", res, err, "ledgers-refusal")
		return ok
	}
	codes := map[uuid.UUID]string{ledger.ID: ledger.Code, compare.ID: compare.Code}
	for _, tb := range page.TrialBalances {
		v.Ledgers.Trials = append(v.Ledgers.Trials, ui.TrialView{Ledger: codes[tb.LedgerID], Debit: moneyOf(tb.DebitLc), Credit: moneyOf(tb.CreditLc),
			Balanced: tb.Balanced})
	}
	for _, d := range page.Items {
		v.Ledgers.Differences = append(v.Ledgers.Differences, ui.DifferenceView{Account: d.AccountID.String(), Currency: d.Currency,
			Balance: moneyOf(d.Balance), Compare: moneyOf(d.CompareBalance), Difference: moneyOf(d.Difference)})
	}
	v.Ledgers.More = page.NextPageToken.Set
	return true
}

// compared are the ledger reconciled and the one it is compared with, of
// the ledgers the company keeps (the leading one first): the period's
// parallel ledger with the leading one, or the leading ledger with the
// first parallel one; found is false when the company keeps only one.
func compared(kept []gen.Ledger, period uuid.UUID) (ledger, compare gen.Ledger, found bool) {
	i := slices.IndexFunc(kept, func(l gen.Ledger) bool { return l.ID == period })
	if len(kept) < 2 || i < 0 || !kept[0].IsLeading {
		return gen.Ledger{}, gen.Ledger{}, false
	}
	if i == 0 {
		return kept[0], kept[1], true
	}
	return kept[i], kept[0], true
}
