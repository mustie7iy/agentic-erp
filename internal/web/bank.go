package web

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Bank statements and their reconciliation (ADR-0052 §2, ADR-0047,
// T73g): the statements of the house banks by status, company (T73i2) and
// bank, a gap
// flagged where a statement does not follow the one before it, the import
// of a bank's file, a statement's lines with their matches and the
// person's decisions on them (statement.go, statement_actions.go), and
// each house bank's reconciliation report (reconciliation.go).

// A list page shows statementPageSize statements; with
// getCurrentPrincipal, listHouseBanks and listLegalEntities it makes at
// most four calls (ADR-0052 §10: six).
const statementPageSize = 25

// maxHouseBanks is the most house banks a page names, the API's largest
// page.
const maxHouseBanks = 500

// maxStatementFile is the largest file importBankStatements takes (4 MB);
// an upload's body may carry the form's other fields besides.
const (
	maxStatementFile = 4 << 20
	maxUploadBody    = maxStatementFile + maxForm
)

// The statement statuses the catalog names, the list's tabs (every
// status first) and the notices of an import just done.
var (
	statementStatuses = map[string]i18n.Key{"imported": i18n.StatementStatusImported, "gap": i18n.StatementStatusGap,
		"reconciled": i18n.StatementStatusReconciled}
	statementTabs = []string{"", "imported", "gap", "reconciled"}
	bankDone      = map[string]i18n.Key{"imported": i18n.BankDoneImported, "reimported": i18n.BankDoneReimported}
)

// statementPath is a statement's page; reportPath a house bank's
// reconciliation report.
func statementPath(id uuid.UUID) string { return ui.BankPath + "/statements/" + id.String() }

func reportPath(id uuid.UUID) string {
	return ui.BankPath + "/house-banks/" + id.String() + "/reconciliation"
}

// uploaded is what the person sent in the upload form that was refused or
// not valid; the file is never kept.
type uploaded struct {
	bank, format                string
	bankErr, formatErr, fileErr i18n.Key
	refusal                     *ui.Refusal
	sent                        bool // the form was sent: its file is chosen again
}

// statements is the statements' list.
func (s *Server) statements(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showStatements(w, r, p, m, http.StatusOK, uploaded{})
}

// showStatements renders the list as the query asks, with the upload form
// holding what was sent, and its refusal.
func (s *Server) showStatements(w http.ResponseWriter, r *http.Request, p *person, m me, status int, up uploaded) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		q = url.Values{"house_bank_id": {up.bank}}
	}
	params, valid := statementParams(q)
	// A company chosen lists its house banks alone, in the list and the
	// filter, as its statements are.
	banks := s.houseBanks(r.Context(), m, params.LegalEntityID.Or(uuid.Nil))
	companies := s.companies(r.Context(), m)
	l := ui.StatementList{Banks: bankViews(banks, companies), Tabs: bankTabs(q), Done: bankDone[q.Get("done")], UploadRefusal: up.refusal,
		Filter: ui.BankFilter{Status: q.Get("status"), Banks: bankOptions(banks, q.Get("house_bank_id"), false),
			Companies: companyOptions(companies, q.Get(ui.FieldCompany))}}
	if f := uploadForm(p, banks, up); m.may(permImportStatements) && len(f.Banks) > 0 {
		l.Upload = f // none without a bank to import for
	}
	if !valid {
		l.Invalid, status = true, http.StatusBadRequest
	} else {
		res, err := s.API.ListBankStatements(r.Context(), params)
		page, ok := res.(*gen.BankStatementPage)
		if !ok {
			prob, show := s.refusal(w, r, p, "listBankStatements", res, err)
			if !show {
				return
			}
			l.Refusal = explainIn(i18n.BankProblems, i18n.BankRefusedTitle, prob, m)
			if status == http.StatusOK {
				status = int(prob.Status)
			}
		} else {
			l.Rows, l.More = statementRows(*page, banks, companies), nextStatements(q, page.NextPageToken)
		}
	}
	s.render(w, r, status, ui.Statements(s.page(i18n.BankTitle, ui.BankPath, p, m), l))
}

// statementParams are the API's filters the query asks for, and whether
// its company and its bank are ids.
func statementParams(q url.Values) (gen.ListBankStatementsParams, bool) {
	params := gen.ListBankStatementsParams{PageSize: gen.NewOptInt32(statementPageSize), OrderBy: gen.NewOptString("closing_date desc")}
	valid := true
	for _, f := range []struct {
		name string
		set  *gen.OptUUID
	}{{ui.FieldCompany, &params.LegalEntityID}, {"house_bank_id", &params.HouseBankID}} {
		if v := q.Get(f.name); v != "" {
			id, err := uuid.Parse(v)
			valid = valid && err == nil
			*f.set = gen.NewOptUUID(id)
		}
	}
	if v := q.Get("status"); v != "" {
		params.Status = gen.NewOptString(v)
	}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params, valid
}

// nextStatements is the next page's path, "" on the last.
func nextStatements(q url.Values, next gen.OptString) string {
	if v := next.Or(""); v != "" {
		return ui.StatementsPath(with(q, "page", v))
	}
	return ""
}

// bankTabs are the statement statuses, each keeping the bank filter.
func bankTabs(q url.Values) []ui.StatusTab {
	tabs := make([]ui.StatusTab, 0, len(statementTabs))
	for _, st := range statementTabs {
		k := statementStatuses[st]
		if st == "" {
			k = i18n.InvoicesStatusAll
		}
		tabs = append(tabs, ui.StatusTab{Label: k, Path: ui.StatementsPath(with(without(q, "done"), "status", st)), Current: q.Get("status") == st})
	}
	return tabs
}

// without is a copy of q without key.
func without(q url.Values, key string) url.Values {
	out := with(q, key, "")
	out.Del(key)
	return out
}

// statementRows are a page of statements, each with its bank's code and
// masked account, its company, and its status (a gap flagged).
func statementRows(page gen.BankStatementPage, banks map[uuid.UUID]gen.HouseBank, companies map[uuid.UUID]string) []ui.StatementRow {
	out := make([]ui.StatementRow, 0, len(page.Items))
	for _, st := range page.Items {
		out = append(out, ui.StatementRow{Path: statementPath(st.ID), Ref: st.StatementRef, Sequence: st.Sequence, Bank: banks[st.HouseBankID].Code,
			Account: ui.Masked(st.Account), Company: companies[st.LegalEntityID], OpeningDay: st.OpeningDate, ClosingDay: st.ClosingDate,
			Opening: moneyOf(st.OpeningBalance), Closing: moneyOf(st.ClosingBalance), Lines: st.LineCount,
			Status: labelOf(statementStatuses, st.Status), Gap: st.Status == "gap", BankReport: reportPath(st.HouseBankID)})
	}
	return out
}

// houseBanks are the house banks the person may see, of one legal entity
// (uuid.Nil: of every one), by id (listHouseBanks); none when they could
// not be read, logged: a bank's name is no reason to refuse a page.
func (s *Server) houseBanks(ctx context.Context, m me, legalEntity uuid.UUID) map[uuid.UUID]gen.HouseBank {
	if !m.may(permReadStatements) {
		return nil
	}
	params := gen.ListHouseBanksParams{PageSize: gen.NewOptInt32(maxHouseBanks), OrderBy: gen.NewOptString("code")}
	if legalEntity != uuid.Nil {
		params.LegalEntityID = gen.NewOptUUID(legalEntity)
	}
	res, err := s.API.ListHouseBanks(ctx, params)
	page, ok := res.(*gen.HouseBankPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the house banks could not be read", "answer", res, "error", err)
		return nil
	}
	out := make(map[uuid.UUID]gen.HouseBank, len(page.Items))
	for _, b := range page.Items {
		out[b.ID] = b
	}
	return out
}

// bankView is a house bank as the pages name it.
func bankView(b gen.HouseBank, companies map[uuid.UUID]string) ui.HouseBank {
	return ui.HouseBank{Code: b.Code, Account: ui.Masked(b.AccountLast4), Currency: b.Currency, Company: companies[b.LegalEntityID],
		Inactive: b.Status == "inactive", Statements: ui.StatementsPath(url.Values{"house_bank_id": {b.ID.String()}}), Report: reportPath(b.ID)}
}

// bankViews are the house banks by code, then company.
func bankViews(banks map[uuid.UUID]gen.HouseBank, companies map[uuid.UUID]string) []ui.HouseBank {
	out := make([]ui.HouseBank, 0, len(banks))
	for _, b := range banks {
		out = append(out, bankView(b, companies))
	}
	slices.SortFunc(out, func(a, b ui.HouseBank) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.Company, b.Company), cmp.Compare(a.Account, b.Account))
	})
	return out
}

// bankOptions are the house banks to choose, by code and account, the
// active ones alone when active is true; nil when they could not be read.
func bankOptions(banks map[uuid.UUID]gen.HouseBank, chosen string, active bool) []ui.Option {
	if banks == nil {
		return nil
	}
	out := make([]ui.Option, 0, len(banks))
	for id, b := range banks {
		if active && b.Status == "inactive" {
			continue
		}
		out = append(out, ui.Option{Value: id.String(), Text: b.Code + " " + ui.Masked(b.AccountLast4) + " " + b.Currency, Selected: id.String() == chosen})
	}
	sortOptions(out)
	return out
}

// uploadForm is the upload form, fresh (a new idempotency key) and
// holding what was sent.
func uploadForm(p *person, banks map[uuid.UUID]gen.HouseBank, up uploaded) *ui.UploadForm {
	return &ui.UploadForm{CSRF: p.CSRF, Key: uuid.NewString(), Banks: bankOptions(banks, up.bank, true), Format: up.format,
		BankError: up.bankErr, FormatError: up.formatErr, FileError: up.fileErr, Again: up.sent && up.fileErr == ""}
}

// csrfUpload is csrf for the upload's multipart form: its body is read
// up to the largest file the API takes, in memory, and the token is
// checked as csrf checks it. A body too large is answered at once, with
// no API call: nothing is acted on before the token is checked.
func (s *Server) csrfUpload(next func(http.ResponseWriter, *http.Request, *person)) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
		var tooLarge *http.MaxBytesError
		if err := r.ParseMultipartForm(maxUploadBody); errors.As(err, &tooLarge) {
			s.render(w, r, http.StatusRequestEntityTooLarge, ui.Problem(s.chrome(r, i18n.UploadRefusedTitle), i18n.UploadRefusedTitle,
				i18n.UploadFileTooLarge, ui.BankPath, i18n.StatementBack))
			return
		}
		if !tokenOK(r.PostForm.Get(ui.CSRFField), p) {
			s.Log.WarnContext(r.Context(), "web: an upload without the session's CSRF token was refused", "path", r.URL.Path)
			s.refused(w, r)
			return
		}
		next(w, r, p)
	}
}

// tokenOK reports whether token is the session's CSRF token.
func tokenOK(token string, p *person) bool {
	return p.CSRF != "" && subtle.ConstantTimeCompare([]byte(token), []byte(p.CSRF)) == 1
}

// importStatement imports a bank's file for a house bank
// (importBankStatements): done, the person goes to its statement, or to
// the bank's statements when the file held several; refused, the list
// says why and keeps the bank and format chosen.
func (s *Server) importStatement(w http.ResponseWriter, r *http.Request, p *person) {
	key := r.PostForm.Get(ui.FieldIdempotency)
	if _, err := uuid.Parse(key); err != nil {
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody, ui.BankPath, i18n.StatementBack))
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	up := uploaded{bank: r.PostForm.Get(ui.FieldHouseBank), format: r.PostForm.Get(ui.FieldFormat), sent: true}
	bank, err := uuid.Parse(up.bank)
	if err != nil {
		up.bankErr = i18n.UploadBankNeeded
	}
	if !slices.ContainsFunc(ui.UploadFormats, func(o ui.Option) bool { return o.Value == up.format }) {
		up.formatErr = i18n.UploadFormatNeeded
	}
	file, fileErr := statementFile(r)
	if up.fileErr = fileErr; up.bankErr != "" || up.formatErr != "" || up.fileErr != "" {
		s.showStatements(w, r, p, m, http.StatusUnprocessableEntity, up)
		return
	}
	res, err := s.API.ImportBankStatements(r.Context(), &gen.BankStatementUploadMultipart{HouseBankID: bank, Format: up.format, File: string(file)},
		gen.ImportBankStatementsParams{IdempotencyKey: key})
	switch res := res.(type) {
	case *gen.ImportBankStatementsCreated:
		http.Redirect(w, r, importedPath(bank, res.Response, "imported"), http.StatusSeeOther)
		return
	case *gen.ImportBankStatementsOK:
		http.Redirect(w, r, importedPath(bank, res.Response, "reimported"), http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "importBankStatements", res, err); show {
		up.refusal = explainIn(i18n.BankProblems, i18n.UploadRefusedTitle, prob, m)
		up.refusal.ID = "upload-refusal"
		s.showStatements(w, r, p, m, int(prob.Status), up)
	}
}

// statementFile is the upload's file, or what is wrong with it: none
// chosen, empty, or larger than the API takes.
func statementFile(r *http.Request) ([]byte, i18n.Key) {
	return formFile(r, i18n.UploadFileNeeded, i18n.UploadFileTooLarge)
}

// formFile is an upload's file, or what is wrong with it, in the
// upload's words: none chosen or empty (missing), or larger than the
// API takes (tooLarge).
func formFile(r *http.Request, missing, tooLarge i18n.Key) ([]byte, i18n.Key) {
	f, _, err := r.FormFile(ui.FieldFile)
	if err != nil {
		return nil, missing
	}
	defer func() { _ = f.Close() }() // held in memory: closing cannot fail
	data, _ := io.ReadAll(f)         // in memory (csrfUpload): reading cannot fail
	switch {
	case len(data) == 0:
		return nil, missing
	case len(data) > maxStatementFile:
		return nil, tooLarge
	}
	return data, ""
}

// importedPath is where an import goes on to: its one statement's page,
// or the house bank's statements.
func importedPath(bank uuid.UUID, imp gen.BankStatementImport, done string) string {
	if len(imp.Statements) == 1 {
		return statementPath(imp.Statements[0].ID) + "?done=" + done
	}
	return ui.StatementsPath(url.Values{"house_bank_id": {bank.String()}, "done": {done}})
}
