package ui

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	templruntime "github.com/a-h/templ/runtime"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

var errWrite = errors.New("write refused")

// failAfter accepts n writes, then refuses every one.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errWrite
	}
	f.n--
	return len(p), nil
}

func shell() Shell {
	return Shell{Title: i18n.HomeTitle, Current: "/", CSS: "/static/css/workspace.0.css", Script: "/static/vendor/htmx.0.js"}
}

// components are every component, rendered alone.
func components() map[string]templ.Component {
	out := map[string]templ.Component{
		"Layout":          Layout(shell(), Value("content")),
		"Notice":          Notice("n", i18n.HomeNotice, Value("message")),
		"SummaryList":     SummaryList([]Row{{Key: i18n.StatusState, Value: Value("v")}}),
		"Tag":             Tag(i18n.TagPlanned, ""),
		"ButtonLink":      ButtonLink("/", i18n.NotFoundHome),
		"Code":            Code("v1.0.0"),
		"Paragraph":       Paragraph(i18n.HomeJobsCount, 2),
		"Home":            Home(signedIn(), HomePage{Companies: []string{"Acme GmbH"}}),
		"homeContent":     homeContent(HomePage{}),
		"SignOut":         SignOut("token"),
		"SignIn":          SignIn(shell(), SignInForm{Code: "acme", Return: "/"}),
		"signInContent":   signInContent(SignInForm{Code: "Acme", Error: i18n.SignInInvalid}),
		"Problem":         Problem(shell(), i18n.SignInFailedTitle, i18n.SignInFailedBody, "/login", i18n.SignInAgain),
		"problemContent":  problemContent(i18n.RefusedTitle, i18n.RefusedBody, "/", i18n.NotFoundHome),
		"Continue":        Continue(continuing()),
		"continueContent": continueContent("/"),
		"StatusPage":      StatusPage(shell(), Status{Version: "v1.2.3", Revision: "abc"}),
		"statusContent":   statusContent(Status{Version: "devel"}),
		"NotFound":        NotFound(shell()),
		"notFoundContent": notFoundContent(),
		"Inbox":           Inbox(signedIn(), inboxFull()),
		"inbox empty":     inboxContent(InboxPage{}),
		"inbox refused":   inboxContent(InboxPage{Refusal: &Refusal{Title: i18n.InboxRefusedTitle, Message: i18n.ProblemPermission, Code: "identity.permission_missing"}}),
		"inbox last page": inboxContent(InboxPage{Approvals: []Approval{approval(false)}}),
		"Card":            Card(approval(true)),
		"card plain":      Card(Approval{Path: "/approvals/2", Title: "t", Kind: Label{Raw: "new_kind"}}),
		"Fact":            Fact(i18n.ApprovalKind, Value("v")),
		"Amount":          Amount(Money{Amount: "1.00", Currency: "EUR"}),
		"Principal":       Principal(Name{Text: "Avery"}),
		"principal agent": Principal(Name{Text: "Matcher", Agent: true}),
		"principal none":  Principal(Name{}),
		"Date":            Date(day),
		"StatusTag":       StatusTag(Label{Key: i18n.StatusApproved}),
		"status raw":      StatusTag(Label{Raw: "on_hold"}),
		"Barred":          Barred(i18n.InboxBarredBody, []string{"a.b", "c.d"}),
		"RefusalNotice":   RefusalNotice(refusal()),
		"refusal other":   RefusalNotice(Refusal{Title: i18n.RefusalTitle, Fallback: "Gone", Code: "x.y"}),
		"Request":         Request(signedIn(), requestPage(true)),
		"request closed":  requestContent(requestPage(false)),
		"StepItem":        StepItem(Step{No: 1, Required: 2, Approvals: 1, Current: true}),
		"step done":       StepItem(Step{No: 1, Required: 1, Approvals: 1}),
		"DecisionItem":    DecisionItem(Decision{Label: Label{Key: i18n.StatusRejected}, By: Name{Text: "B"}, At: day, Comment: "No"}),
		"decision bare":   DecisionItem(Decision{Label: Label{Key: i18n.StatusApproved}, By: Name{Text: "B"}, At: day}),
		"DecideForm":      DecideForm(forms(), true),
		"decide reject":   DecideForm(forms(), false),
		"decideHidden":    decideHidden("c", `"1"`, "k"),
		"CommentField":    CommentField("f", i18n.DecideComment, i18n.DecideCommentHint, "text", i18n.DecideTooLong),
		"comment plain":   CommentField("f", i18n.DecideComment, i18n.DecideCommentHint, "", ""),
		// The supplier invoices (T73e).
		"Invoices":           Invoices(signedIn(), invoiceList()),
		"queue invalid":      invoicesContent(InvoiceList{Invalid: true}),
		"queue refused":      invoicesContent(InvoiceList{Refusal: &Refusal{Title: i18n.InvoicesRefusedTitle, Code: "x.y", Fallback: "No"}}),
		"queue empty":        invoicesContent(InvoiceList{Filters: InvoiceFilters{From: "x", FromError: i18n.FilterDateInvalid}}),
		"queue last page":    invoicesContent(InvoiceList{Rows: []InvoiceRow{invoiceRow(false)}}),
		"StatusTabs":         StatusTabs([]StatusTab{{Label: i18n.InvoicesStatusAll, Path: "/invoices", Current: true}, {Label: i18n.InvoiceStatusParked, Path: "/invoices?status=parked"}}),
		"FilterForm":         FilterForm(InvoiceFilters{Supplier: Supplier{ID: "s1", Name: "Northwind", Number: "BP1"}, SupplierClear: "/invoices"}, []Option{{Value: "c1", Text: "Acme", Selected: true}}),
		"filters plain":      FilterForm(InvoiceFilters{}, nil),
		"InputField":         InputField("i", "n", "date", i18n.FilterFrom, i18n.FilterDateHint, "x", i18n.FilterDateInvalid),
		"input plain":        InputField("i", "n", "text", i18n.FilterNumber, i18n.FilterNumberHint, "", ""),
		"InvoiceCard":        InvoiceCard(invoiceRow(true)),
		"card plain invoice": InvoiceCard(InvoiceRow{Path: "/invoices/2", Number: "SI2", Kind: Label{Raw: "invoice"}, Status: Label{Raw: "new"}}),
		"supplierFilter":     supplierFilter(invoiceRow(false)),
		"SupplierName":       SupplierName(Supplier{ID: "s1"}),
		"Data":               Data("INV-1"),
		"Day":                Day(day),
		"Number":             Number(10),
		"Quantity":           Quantity("12.5", "KG"),
		"InvoiceStatusTag":   InvoiceStatusTag(Label{Key: i18n.InvoiceStatusOnHold}),
		"Invoice":            Invoice(signedIn(), invoicePage(true)),
		"invoice plain":      invoiceContent(invoicePage(false)),
		"DocumentPanel":      DocumentPanel("/invoices/1/document"),
		"document none":      DocumentPanel(""),
		"LinesTable":         LinesTable(invoicePage(true).Lines),
		"HeaderCell":         HeaderCell(i18n.LineNo),
		"LineRow":            LineRow(Line{No: 1, OrderItem: 10, Quantity: "1", Unit: "EA", Amount: Money{Amount: "1.00", Currency: "EUR"}, TaxCode: "V1"}),
		"line bare":          LineRow(Line{No: 2, Quantity: "1", Unit: "EA", Amount: Money{Amount: "1.00", Currency: "EUR"}}),
		"MatchSection":       MatchSection(invoicePage(true)),
		"match no order":     MatchSection(InvoicePage{}),
		"match refused":      MatchSection(InvoicePage{HasOrder: true, MatchError: &Refusal{ID: "m", Title: i18n.MatchTitle, Code: "x.y"}}),
		"match none":         MatchSection(InvoicePage{HasOrder: true}),
		"MatchResult":        MatchResult(MatchView{When: day, By: Name{Text: "A"}, Config: 1, Clean: true}),
		"match issues":       MatchResult(*invoicePage(true).Match),
		"IssueItem":          IssueItem(Issue{Label: Label{Key: i18n.MatchIssuePrice}, Line: 1, Message: "m"}),
		"issue bare":         IssueItem(Issue{Label: Label{Raw: "new_issue"}, Message: "m"}),
		"OrderLink":          OrderLink("/invoices/1/order"),
		"OrderPage":          OrderPage(signedIn(), orderView(false)),
		"orderPageContent":   orderPageContent(orderView(true)),
		"OrderSection":       OrderSection(orderView(false)),
		"orderBody":          orderBody(OrderView{Header: OrderHeader{Number: "PO1"}}),
		"OrderItemsTable":    OrderItemsTable(orderView(false).Items),
		"OrderItemRow":       OrderItemRow(OrderItemView{No: 1, Ordered: "1", Received: "0", Unit: "EA", Price: Money{Amount: "1.00", Currency: "EUR"}, TaxCode: "V1"}),
		"order item bare":    OrderItemRow(OrderItemView{No: 2, Ordered: "1", Received: "0", Unit: "EA", Price: Money{Amount: "1.00", Currency: "EUR"}}),
		"ReceiptsTable":      ReceiptsTable(orderView(false).Receipts),
		"ReceiptRow":         ReceiptRow(ReceiptView{Number: "GR1", Date: day, OrderItem: 1, Quantity: "1", Unit: "EA"}),
		"RecsSection":        RecsSection(invoicePage(true)),
		"recs refused":       RecsSection(InvoicePage{RecsError: &Refusal{ID: "r", Title: i18n.RecsRefusedTitle, Code: "x.y"}}),
		"recs none":          RecsSection(InvoicePage{}),
		"recs not yours":     RecsSection(InvoicePage{Recs: []RecommendationView{recommendation(false)}, RecsWhy: i18n.RecNotYours}),
		"RecItem":            RecItem(recommendation(true)),
		"rec decided":        RecItem(recommendation(false)),
		"OutcomeTag":         OutcomeTag(Label{Key: i18n.RecAccepted}),
		"RecForm":            RecForm(RecommendationForm{Action: "/x", ID: "r", Outcome: "", OutcomeError: i18n.RecOutcomeNeeded, CommentError: i18n.DecideTooLong}),
		"rec form plain":     RecForm(RecommendationForm{Action: "/x", ID: "r", Outcome: "accepted"}),
		"OutcomeChoice":      OutcomeChoice(RecommendationForm{ID: "r", Outcome: "ignored"}, "ignored"),
		"ActionsSection":     ActionsSection(invoicePage(true)),
		"actions none":       ActionsSection(InvoicePage{Why: i18n.ActionsNone}),
		"ActionFormView":     ActionFormView(ActionForm{Verb: ActReverse, Action: "/x", Text: "t", Date: "d", TextError: i18n.ActionReasonNeeded, DateError: i18n.ActionDateInvalid}),
		"action match":       ActionFormView(ActionForm{Verb: ActMatch, Action: "/x"}),
		"RefusalPage":        RefusalPage(shell(), i18n.DocumentTitle, "/invoices/1", i18n.OrderBack, refusal()),
		"refusalPageContent": refusalPageContent(i18n.DocumentTitle, "/invoices/1", i18n.OrderBack, refusal()),
	}
	for name, c := range bankComponents() {
		out[name] = c
	}
	for name, c := range paymentComponents() {
		out[name] = c
	}
	for name, c := range closeComponents() {
		out[name] = c
	}
	return out
}

// paymentComponents are the payment run screens' components (T73f), with
// the variants their branches need.
func paymentComponents() map[string]templ.Component {
	r := refusal()
	return map[string]templ.Component{
		"Runs":                Runs(signedIn(), runList(true)),
		"runs plain":          runsContent(runList(false)),
		"runs invalid":        runsContent(RunList{Invalid: true}),
		"runs refused":        runsContent(RunList{Refusal: &r}),
		"runs empty":          runsContent(RunList{}),
		"RunFilterForm":       RunFilterForm(RunFilters{Status: "proposed", Number: "PR1", Companies: []Option{{Value: "c1", Text: "Acme", Selected: true}}}),
		"RunCard":             RunCard(runRow(true)),
		"run card plain":      RunCard(runRow(false)),
		"RunStatusTag":        RunStatusTag(Label{Key: i18n.RunStatusReleased}),
		"run status raw":      RunStatusTag(Label{Raw: "on_hold"}),
		"ProposeFormView":     ProposeFormView(proposeForm(true)),
		"propose plain":       ProposeFormView(proposeForm(false)),
		"Run":                 Run(signedIn(), runPage(true)),
		"run plain":           runContent(runPage(false)),
		"run building":        runContent(RunPage{Number: "PR1", Building: true, Progress: "/payments/1/progress"}),
		"run refused lists":   runContent(RunPage{Number: "PR1", PaymentsError: &r, ExclusionsError: &r, FilesShown: true, FilesError: &r}),
		"run empty lists":     runContent(RunPage{Number: "PR1", FilesShown: true}),
		"runLists":            runLists(runPage(true)),
		"RunActions":          RunActions(RunPage{Why: i18n.RunOwn}),
		"RunFormView":         RunFormView(RunForm{Verb: RunReject, ID: "run-reject", Action: "/x", Text: "t", TextError: i18n.ActionReasonNeeded}),
		"run form transmit":   RunFormView(RunForm{Verb: RunTransmit, ID: "file-1", Action: "/x", File: "f1"}),
		"ApprovalBlock":       ApprovalBlock(approvalBlock(true)),
		"approval plain":      ApprovalBlock(approvalBlock(false)),
		"ApprovalStatusTag":   ApprovalStatusTag(Label{Key: i18n.ReleaseApprovalApproved}),
		"PaymentsTable":       PaymentsTable([]PaymentView{paymentView(true), paymentView(false)}),
		"PaymentRow":          PaymentRow(paymentView(true)),
		"optionalAmount":      optionalAmount(nil),
		"ExclusionsTable":     ExclusionsTable([]ExclusionView{exclusionView(true), exclusionView(false)}),
		"ExclusionRow":        ExclusionRow(exclusionView(false)),
		"FilesSection":        FilesSection(RunPage{Files: []BankFileView{fileView(true), fileView(false)}}),
		"FileItem":            FileItem(fileView(true)),
		"FileStatusTag":       FileStatusTag(Label{Key: i18n.FileStatusFailed}),
		"ProgressLink":        ProgressLink("/payments/1/progress"),
		"ProgressSection":     ProgressSection(progressView(true)),
		"progress plain":      ProgressSection(progressView(false)),
		"progressBody":        progressBody(progressView(false)),
		"ProgressStatus":      ProgressStatus(progressView(true)),
		"CancelFormView":      CancelFormView(CancelForm{Action: "/x", CSRF: "c", Key: "k", Operation: "o1"}),
		"ProgressPage":        ProgressPage(signedIn(), progressView(true)),
		"progressPageContent": progressPageContent(progressView(false)),
		"OperationStatusTag":  OperationStatusTag(Label{Key: i18n.OperationRunning}),
	}
}

// runRow is a run of the list: of a named company and house bank, or a
// plain one whose suppliers' payment methods choose the bank.
func runRow(full bool) RunRow {
	r := RunRow{Path: "/payments/1", Number: "PR100001", Status: Label{Key: i18n.RunStatusProposed}, RunDate: day, DueBy: day,
		Total: Money{Amount: "119.00", Currency: "EUR"}, ProposedBy: Name{Text: "Avery"}}
	if full {
		r.Company, r.Bank = "Acme GmbH", "MAIN"
	}
	return r
}

// runList is the list: everything, or a page of one run.
func runList(full bool) RunList {
	if !full {
		return RunList{Rows: []RunRow{runRow(false)}}
	}
	r := refusal()
	return RunList{Tabs: []StatusTab{{Label: i18n.InvoicesStatusAll, Path: "/payments", Current: true}},
		Filters: RunFilters{Companies: []Option{{Value: "c1", Text: "Acme"}}}, Rows: []RunRow{runRow(true), runRow(false)}, More: "/payments?page=t",
		Propose: new(proposeForm(true)), ProposeRefusal: &r}
}

// proposeForm is the propose form, with every error and the banks when
// failed, plain otherwise.
func proposeForm(failed bool) ProposeForm {
	f := ProposeForm{CSRF: "c", Key: "k", Companies: []Option{{Value: "c1", Text: "Acme", Selected: true}}, RunDate: "2026-04-15", DueBy: "2026-04-15"}
	if failed {
		f.Banks = []Option{{Value: "b1", Text: "MAIN", Selected: true}}
		f.Currency, f.CompanyError, f.CurrencyError, f.RunDateError, f.DueByError = "euro", i18n.ProposeCompanyNeeded, i18n.ProposeCurrencyInvalid,
			i18n.ActionDateInvalid, i18n.ActionDateInvalid
	}
	return f
}

// runPage is a run: with everything, or plain.
func runPage(full bool) RunPage {
	h := RunHeader{Status: Label{Key: i18n.RunStatusProposed}, RunDate: day, DueBy: day, Total: Money{Amount: "119.00", Currency: "EUR"}, ProposedBy: Name{Text: "Avery"}}
	if !full {
		return RunPage{Number: "PR1", Header: h, Payments: []PaymentView{paymentView(false)}, Exclusions: []ExclusionView{exclusionView(false)}}
	}
	r := refusal()
	h.DecidedBy, h.DecidedAt = &Name{Text: "Blake"}, &day
	return RunPage{Number: "PR1", Header: h, Approval: new(approvalBlock(true)), Progress: "/payments/1/progress", Payments: []PaymentView{paymentView(true)},
		PaymentsMore: "/payments/1?payments_page=p", Exclusions: []ExclusionView{exclusionView(true)}, ExclusionsMore: "/payments/1?exclusions_page=e",
		FilesShown: true, Files: []BankFileView{fileView(true)}, Actions: []RunForm{{Verb: RunRelease, ID: "run-release", Action: "/x"}},
		Why: i18n.RunApprovalWaits, Done: i18n.RunDoneSubmitted, Refusal: &r}
}

// approvalBlock is a release's approval: approved, or pending.
func approvalBlock(approved bool) ReleaseApproval {
	a := ReleaseApproval{Status: Label{Key: i18n.ReleaseApprovalPending}, Requested: Money{Amount: "119.00", Currency: "EUR"}, Request: "/approvals/1",
		Pending: true}
	if approved {
		a.Status, a.Pending, a.ApprovedBy, a.ApprovedAt = Label{Key: i18n.ReleaseApprovalApproved}, false, &Name{Text: "Casey"}, &day
	}
	return a
}

// paymentView is a payment: a down payment with everything, or plain.
func paymentView(full bool) PaymentView {
	p := PaymentView{Supplier: "s1", Amount: Money{Amount: "119.00", Currency: "EUR"}, EndToEnd: "E2E", Items: 1}
	if full {
		p.Discount, p.Withholding, p.Method, p.DownPayment, p.Posted = &Money{Amount: "1.00", Currency: "EUR"}, &Money{Amount: "2.00", Currency: "EUR"}, "T", true, true
	}
	return p
}

// exclusionView is an exclusion: of a down payment request, or of a
// supplier's items.
func exclusionView(request bool) ExclusionView {
	ex := ExclusionView{Supplier: "s1", Reason: Label{Key: i18n.ExclusionNoBankAccount}, Amount: Money{Amount: "59.50", Currency: "EUR"}}
	if request {
		ex.DownPayment, ex.Reason = "d1", Label{Raw: "sanctions_hit"}
	}
	return ex
}

// fileView is a bank file: one to send with its method, or plain.
func fileView(open bool) BankFileView {
	f := BankFileView{Format: "pain.001.001.03", Status: Label{Key: i18n.FileStatusTransmitted}, Created: day}
	if open {
		f.Method, f.Status, f.Transmit = "T", Label{Key: i18n.FileStatusCreated}, &RunForm{Verb: RunTransmit, ID: "file-1", Action: "/x", File: "f1"}
	}
	return f
}

// progressView is an operation: running with everything, or stopped with
// its total unknown.
func progressView(full bool) ProgressView {
	v := ProgressView{Run: "/payments/1", Self: "/payments/1/progress", Kind: Label{Raw: "payments.other"}, Status: Label{Key: i18n.OperationFailed}, Done: 4}
	if !full {
		v.Resume = &RunForm{Verb: RunRelease, ID: "resume", Action: "/x"}
		return v
	}
	r, total := refusal(), int64(10)
	v.Kind, v.Status, v.Total, v.Running = Label{Key: i18n.OperationRelease}, Label{Key: i18n.OperationRunning}, &total, true
	v.Error, v.Refusal, v.Cancel = &r, &r, &CancelForm{Action: "/x", Operation: "o1"}
	return v
}

// TestPaymentHelpers: each status has its colour, a rejection needs its
// reason, and the list's path drops empty filters.
func TestPaymentHelpers(t *testing.T) {
	for name, tc := range map[string]struct {
		kind func(Label) string
		l    Label
		want string
	}{
		"run released":       {runStatusKind, Label{Key: i18n.RunStatusReleased}, "success"},
		"run releasing":      {runStatusKind, Label{Key: i18n.RunStatusReleasing}, "warning"},
		"run failed":         {runStatusKind, Label{Key: i18n.RunStatusFailed}, "inactive"},
		"run raw":            {runStatusKind, Label{Raw: "x"}, ""},
		"file sent":          {fileStatusKind, Label{Key: i18n.FileStatusAcknowledged}, "success"},
		"file queued":        {fileStatusKind, Label{Key: i18n.FileStatusToSend}, "warning"},
		"file refused":       {fileStatusKind, Label{Key: i18n.FileStatusRejected}, "inactive"},
		"file raw":           {fileStatusKind, Label{Raw: "x"}, ""},
		"approval approved":  {approvalKind, Label{Key: i18n.ReleaseApprovalApproved}, "success"},
		"approval pending":   {approvalKind, Label{Key: i18n.ReleaseApprovalPending}, "warning"},
		"approval withdrawn": {approvalKind, Label{Key: i18n.ReleaseApprovalWithdrawn}, "inactive"},
		"approval raw":       {approvalKind, Label{Raw: "x"}, ""},
		"operation done":     {operationKind, Label{Key: i18n.OperationSucceeded}, "success"},
		"operation queued":   {operationKind, Label{Key: i18n.OperationQueued}, "warning"},
		"operation cancel":   {operationKind, Label{Key: i18n.OperationCancelled}, "inactive"},
		"operation raw":      {operationKind, Label{Raw: "x"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.kind(tc.l); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if !RunRequires(RunReject) || RunRequires(RunRelease) {
		t.Error("RunRequires: a rejection needs its reason, a release none")
	}
	if got := RunsPath(url.Values{"status": {""}, "legal_entity_id": {"c1"}}); got != "/payments?legal_entity_id=c1" {
		t.Errorf("RunsPath: got %q", got)
	}
	if got := RunsPath(url.Values{"status": {""}}); got != PaymentsPath {
		t.Errorf("RunsPath, nothing chosen: got %q", got)
	}
}

// bankComponents are the bank statement screens' components (T73g), with
// the variants their branches need.
func bankComponents() map[string]templ.Component {
	r := refusal()
	return map[string]templ.Component{
		"Statements":               Statements(signedIn(), statementList(true)),
		"statements plain":         statementsContent(statementList(false)),
		"statements invalid":       statementsContent(StatementList{Invalid: true}),
		"statements refused":       statementsContent(StatementList{Refusal: &r}),
		"statements empty":         statementsContent(StatementList{Filter: BankFilter{Banks: []Option{}}}),
		"HouseBankItem":            HouseBankItem(HouseBank{Code: "MAIN", Account: "•••• 3000", Currency: "EUR", Company: "Acme", Inactive: true}),
		"house bank plain":         HouseBankItem(HouseBank{Code: "MAIN", Account: "•••• 3000", Currency: "EUR"}),
		"BankFilterForm":           BankFilterForm(BankFilter{Status: "gap", Banks: []Option{{Value: "b1", Text: "MAIN", Selected: true}}}),
		"StatementCard":            StatementCard(statementRow(true)),
		"statement card plain":     StatementCard(statementRow(false)),
		"StatementStatusTag":       StatementStatusTag(Label{Key: i18n.StatementStatusGap}),
		"statement status raw":     StatementStatusTag(Label{Raw: "in_review"}),
		"amountOn":                 amountOn(Money{Amount: "1.00", Currency: "EUR"}, day),
		"UploadFormView":           UploadFormView(uploadForm(true)),
		"upload plain":             UploadFormView(uploadForm(false)),
		"Statement":                Statement(signedIn(), statementPage(true)),
		"statement plain":          statementContent(statementPage(false)),
		"statement no lines":       statementContent(StatementPage{Why: i18n.StatementReconciledWhy}),
		"doneMessage":              doneMessage(i18n.LineDonePosted, 2),
		"done without a line":      doneMessage(i18n.LineDonePosted, 0),
		"LineCard":                 LineCard(lineView(true)),
		"line decided":             LineCard(lineView(false)),
		"LineStatusTag":            LineStatusTag(Label{Key: i18n.LineStatusPosted}),
		"MatchBlock":               MatchBlock(matchInfo(true)),
		"match block plain":        MatchBlock(matchInfo(false)),
		"TargetItem":               TargetItem(Target{Candidate: 2, Payment: "p1", BankFile: "f1"}, true),
		"target payment":           TargetItem(Target{Candidate: 1, Payment: "p1"}, false),
		"target rule":              TargetItem(Target{Candidate: 1, Rule: "NCHG", RuleText: "Bank charges"}, false),
		"target rule bare":         TargetItem(Target{Candidate: 1, Rule: "NCHG"}, false),
		"target incoming":          TargetItem(Target{Candidate: 1, Incoming: true}, false),
		"target none":              TargetItem(Target{Candidate: 1}, false),
		"LineFormView":             LineFormView(LineForm{Verb: LinePost, Line: 2, ID: "line-2-post", Action: "/x", Text: "t", Date: "d", TextError: i18n.LinePostReasonNeeded, DateError: i18n.ActionDateInvalid, Open: true}),
		"line form plain":          LineFormView(LineForm{Verb: LineReject, Line: 2, ID: "line-2-reject", Action: "/x"}),
		"MatchFormView":            MatchFormView(matchForm(true)),
		"match form plain":         MatchFormView(matchForm(false)),
		"ChoiceItem":               ChoiceItem("c", Choice{Value: "payments:p1,p2", Kind: ChoicePayments, Number: 1, Payments: []string{"p1", "p2"}}, "payments:p1,p2"),
		"choice rule":              ChoiceItem("c", Choice{Value: "rule:NCHG", Kind: ChoiceRule, Rule: "NCHG", RuleText: "Bank charges"}, ""),
		"choice incoming":          ChoiceItem("c", Choice{Value: ChoiceIncoming, Kind: ChoiceIncoming}, ""),
		"Reconciliation":           Reconciliation(signedIn(), reconPage(true)),
		"reconciliation plain":     reconciliationContent(reconPage(false)),
		"reconciliation refused":   reconciliationContent(ReconciliationPage{Refusal: &r}),
		"reconciliation none":      reconciliationContent(ReconciliationPage{Summary: &ReconSummary{}}),
		"ReconFilterForm":          ReconFilterForm(ReconFilters{Path: "/r", From: "x", FromError: i18n.FilterDateInvalid, Statuses: []KeyOption{{Value: "posted", Label: i18n.LineStatusPosted, Selected: true}}}),
		"ReconSummaryView":         ReconSummaryView(*reconPage(true).Summary),
		"recon summary plain":      ReconSummaryView(ReconSummary{Statements: 0}),
		"CountsList":               CountsList([]Count{{Status: Label{Key: i18n.LineStatusUnmatched}, N: 2}}),
		"unexplained":              unexplained(Money{Amount: "-5.00", Currency: "EUR"}),
		"ReconStatementCard":       ReconStatementCard(reconPage(true).Statements[0]),
		"recon statement no lines": ReconStatementCard(ReconStatement{Path: "/s", Ref: "S1", Anchor: "statement-1", Figures: figures(false)}),
		"ReconLinesTable":          ReconLinesTable("s", reconPage(true).Statements[0].Lines),
		"ReconLineRow":             ReconLineRow(reconPage(true).Statements[0].Lines[0]),
		"recon line approval":      ReconLineRow(ReconLine{No: 2, Amount: Money{Amount: "1.00", Currency: "EUR"}, Status: Label{Raw: "x"}, Approval: "/approvals/1"}),
		"recon line bare":          ReconLineRow(ReconLine{No: 3, Amount: Money{Amount: "1.00", Currency: "EUR"}, Status: Label{Raw: "x"}}),
	}
}

// statementRow is a statement of the list: a gap of a named company, or
// a plain one.
func statementRow(gap bool) StatementRow {
	r := StatementRow{Path: "/bank/statements/1", Ref: "S1", Sequence: "1", Account: "•••• 3000", OpeningDay: day, ClosingDay: day,
		Opening: Money{Amount: "1000.00", Currency: "EUR"}, Closing: Money{Amount: "957.25", Currency: "EUR"}, Lines: 5,
		Status: Label{Key: i18n.StatementStatusImported}, BankReport: "/bank/house-banks/1/reconciliation"}
	if gap {
		r.Bank, r.Company, r.Gap, r.Status = "MAIN", "Acme GmbH", true, Label{Key: i18n.StatementStatusGap}
	}
	return r
}

// uploadForm is the upload form, with every error when failed.
func uploadForm(failed bool) UploadForm {
	f := UploadForm{CSRF: "c", Key: "k", Banks: []Option{{Value: "b1", Text: "MAIN", Selected: true}}, Format: "mt940"}
	if failed {
		f.BankError, f.FormatError, f.FileError = i18n.UploadBankNeeded, i18n.UploadFormatNeeded, i18n.UploadFileTooLarge
		return f
	}
	f.Again = true
	return f
}

// statementList is the list: everything, or a page of one statement.
func statementList(full bool) StatementList {
	if !full {
		return StatementList{Rows: []StatementRow{statementRow(false)}}
	}
	r := refusal()
	return StatementList{Banks: []HouseBank{{Code: "MAIN", Account: "•••• 3000", Currency: "EUR", Company: "Acme", Inactive: true}},
		Tabs: []StatusTab{{Label: i18n.InvoicesStatusAll, Path: "/bank", Current: true}}, Filter: BankFilter{Banks: []Option{{Value: "b1", Text: "MAIN"}}},
		Rows: []StatementRow{statementRow(true), statementRow(false)}, More: "/bank?page=t", Upload: new(uploadForm(true)),
		Done: i18n.BankDoneImported, UploadRefusal: &r}
}

// matchInfo is a match: an agent's proposal with everything, or a plain
// one.
func matchInfo(full bool) MatchInfo {
	if !full {
		return MatchInfo{Kind: Label{Raw: "new_kind"}, Status: Label{Key: i18n.MatchStatusMatched}, Reason: "r"}
	}
	rule := Label{Key: i18n.RuleAmount}
	return MatchInfo{Kind: Label{Key: i18n.MatchKindAgent}, Rule: &rule, By: Name{Text: "agent", Agent: true}, Agent: true,
		Proposal: Label{Key: i18n.ProposalMatch}, Status: Label{Key: i18n.MatchStatusOpen}, Reason: "Pay <b>now</b>",
		Targets: []Target{{Candidate: 1, Payment: "p1"}}, Open: true, PostingDay: &day, PostingReason: "Late", Approval: "/approvals/1", Entry: "e / 2026"}
}

// matchForm is a manual match: refused with every kind of choice, or
// plain.
func matchForm(failed bool) MatchForm {
	f := MatchForm{LineForm: LineForm{Verb: LineMatch, Line: 1, ID: "line-1-match", Action: "/x"},
		Choices: []Choice{{Value: "payments:p1", Kind: ChoicePayments, Number: 1, Payments: []string{"p1"}}, {Value: "rule:NCHG", Kind: ChoiceRule, Rule: "NCHG"},
			{Value: ChoiceIncoming, Kind: ChoiceIncoming}}}
	if failed {
		f.Choice, f.ChoiceError, f.TextError, f.Open = "x", i18n.LineChoiceNeeded, i18n.ActionReasonNeeded, true
	}
	return f
}

// lineView is a line: open to every decision, or decided.
func lineView(open bool) LineView {
	l := LineView{No: 1, Anchor: "line-1", Amount: Money{Amount: "-10.50", Currency: "EUR"}, BookingDay: day, ValueDay: day, Code: "NCHG",
		BankRef: "B1", OwnerRef: "NONREF", Status: Label{Key: i18n.LineStatusPosted}}
	if !open {
		return l
	}
	outcome := Label{Key: i18n.OutcomeRejected}
	m := matchInfo(true)
	l.Reversal, l.Text, l.LastOutcome, l.Match, l.Status = true, "CHARGES", &outcome, &m, Label{Key: i18n.LineStatusProposed}
	form := func(verb string) *LineForm { return &LineForm{Verb: verb, Line: 1, ID: "line-1-" + verb, Action: "/x"} }
	l.Accept, l.Reject, l.Exclude, l.Post = form(LineAccept), form(LineReject), form(LineExclude), form(LinePost)
	l.Manual = new(matchForm(false))
	return l
}

// statementPage is a statement: with everything, or plain.
func statementPage(full bool) StatementPage {
	h := StatementHeader{Ref: "S1", Sequence: "1", Format: "mt940", Account: "•••• 3000", OpeningDay: day, ClosingDay: day,
		Opening: Money{Amount: "1.00", Currency: "EUR"}, Closing: Money{Amount: "1.00", Currency: "EUR"}, Lines: 1, Status: Label{Raw: "x"}}
	if !full {
		return StatementPage{Header: h, Report: "/r", Lines: []LineView{lineView(false)}}
	}
	r := refusal()
	h.Bank, h.Previous, h.ReconciledAt, h.ReconciledBy = "MAIN", "/bank/statements/0", &day, &Name{Text: "Avery"}
	return StatementPage{Header: h, Gap: true, Report: "/r", Lines: []LineView{lineView(true)}, Why: i18n.LinesNotYours, Done: i18n.LineDoneMatched,
		DoneLine: 1, Refusal: &r}
}

// figures are a statement's figures: a bank without a ledger balance, or
// with one and nothing unexplained.
func figures(ledger bool) Figures {
	f := Figures{AsOf: day, Closing: Money{Amount: "1.00", Currency: "EUR"}, Excluded: Money{Amount: "0.00", Currency: "EUR"},
		InTransit: Money{Amount: "0.00", Currency: "EUR"}, PostedLater: Money{Amount: "0.00", Currency: "EUR"}}
	if ledger {
		f.Ledger, f.Unexplained = &Money{Amount: "1.00", Currency: "EUR"}, &Money{Amount: "0.00", Currency: "EUR"}
	}
	return f
}

// reconPage is a report: with everything, or a summary alone.
func reconPage(full bool) ReconciliationPage {
	if !full {
		return ReconciliationPage{Bank: HouseBank{Code: "MAIN", Account: "•••• 3000"}, Summary: &ReconSummary{Statements: 1},
			Statements: []ReconStatement{{Path: "/s", Ref: "S1", Anchor: "statement-1", Figures: figures(false)}}}
	}
	rule, kind, outcome := Label{Key: i18n.RuleCode}, Label{Key: i18n.MatchKindRule}, Label{Key: i18n.OutcomeAccepted}
	f := figures(true)
	f.Unexplained = &Money{Amount: "-5.00", Currency: "EUR"}
	return ReconciliationPage{Bank: HouseBank{Code: "MAIN", Account: "•••• 3000"}, Filters: ReconFilters{Path: "/r"},
		Summary: &ReconSummary{Statements: 2, Opening: &Money{Amount: "1.00", Currency: "EUR"}, OpeningDay: &day, Figures: new(figures(true)),
			Counts: []Count{{Status: Label{Key: i18n.LineStatusPosted}, N: 1}}},
		Statements: []ReconStatement{{Path: "/s", Ref: "S1", Sequence: "1", Status: Label{Key: i18n.StatementStatusReconciled}, OpeningDay: day,
			Opening: Money{Amount: "1.00", Currency: "EUR"}, Figures: f, Anchor: "statement-1",
			Lines: []ReconLine{{No: 1, BookingDay: day, Amount: Money{Amount: "1.00", Currency: "EUR"}, Reversal: true, Code: "NCHG",
				Status: Label{Key: i18n.LineStatusPosted}, Match: &kind, Rule: &rule, PostingDay: &day, Outcome: &outcome}}}},
		More: "/r?page=t"}
}

// invoiceRow is a queue's invoice, an agent's capture when agent is true.
func invoiceRow(agent bool) InvoiceRow {
	r := InvoiceRow{Path: "/invoices/1", Number: "SI100001", Reference: "INV-1", Supplier: Supplier{ID: "s1", Number: "BP1", Name: "Northwind"},
		FilterPath: "/invoices?supplier_id=s1", Company: "Acme GmbH", Date: day, Gross: Money{Amount: "119.00", Currency: "EUR"},
		Status: Label{Key: i18n.InvoiceStatusOnHold}, Kind: Label{Key: i18n.InvoiceKindMemo, Raw: "credit_memo"}, HoldReason: "Three-way match"}
	if agent {
		r.Agent = &Name{Text: "Capturer", Agent: true}
	}
	return r
}

func invoiceList() InvoiceList {
	return InvoiceList{Tabs: []StatusTab{{Label: i18n.InvoicesStatusAll, Path: "/invoices", Current: true}},
		Filters: InvoiceFilters{Status: "on_hold", Number: "SI1"}, Companies: []Option{{Value: "c1", Text: "Acme"}},
		Rows: []InvoiceRow{invoiceRow(true), invoiceRow(false)}, More: "/invoices?page=t"}
}

// recommendation is an agent's recommendation, open with its form or
// decided.
func recommendation(open bool) RecommendationView {
	r := RecommendationView{Kind: Label{Key: i18n.RecKindVariance}, Content: "Approve: <b>ok</b>", Agent: Name{Text: "AP agent", Agent: true}, At: day,
		Outcome: Label{Key: i18n.RecOpen}}
	if open {
		r.Decide = &RecommendationForm{Action: "/x", ID: "r1", CSRF: "c", ETag: `"1"`, Key: "k"}
		return r
	}
	r.Outcome, r.DecidedBy = Label{Key: i18n.RecIgnored}, &Name{Text: "Avery"}
	return r
}

// invoicePage is an invoice's page: on hold on an order with everything,
// or on accounts with nothing.
func invoicePage(full bool) InvoicePage {
	if !full {
		return InvoicePage{Number: "SI2", Header: InvoiceHeader{Status: Label{Raw: "new"}, Kind: Label{Raw: "invoice"}, Source: Label{Raw: "edi"},
			NoOrder: true}, Why: i18n.ActionsNone}
	}
	posting := day
	r := refusal()
	return InvoicePage{Number: "SI1", Header: InvoiceHeader{Supplier: Supplier{ID: "s1", Name: "N", Number: "BP1"}, Reference: "INV-1", Company: "Acme",
		Kind: Label{Key: i18n.InvoiceKindInvoice}, Status: Label{Key: i18n.InvoiceStatusOnHold}, HoldReason: "Variance", Date: day, PostingDay: &posting,
		TaxDay: &posting, Gross: Money{Amount: "1.00", Currency: "EUR"}, Tax: Money{Amount: "0.00", Currency: "EUR"},
		Source: Label{Key: i18n.InvoiceSourceCapture}, EnteredBy: Name{Text: "Capturer", Agent: true}, Exception: "/approvals/1"},
		AgentMade: true, Lines: []Line{{No: 1, OrderItem: 1, Quantity: "1", Unit: "EA", Amount: Money{Amount: "1.00", Currency: "EUR"}, TaxCode: "V1"}},
		HasOrder: true, Match: &MatchView{When: day, By: Name{Text: "A"}, Config: 1, Issues: []Issue{{Label: Label{Key: i18n.MatchIssuePrice}, Line: 1, Message: "m"}}},
		Order: "/invoices/1/order", Document: "/invoices/1/document", Recs: []RecommendationView{recommendation(true), recommendation(false)}, RecsShown: true,
		Actions: []ActionForm{{Verb: ActMatch, Action: "/x"}, {Verb: ActRelease, Action: "/y"}}, Pending: true, Done: i18n.InvoiceDoneMatched, Refusal: &r}
}

// orderView is an order with its items and a receipt, or a refusal.
func orderView(refused bool) OrderView {
	if refused {
		return OrderView{Back: "/invoices/1", Refusal: &Refusal{ID: "o", Title: i18n.OrderRefusedTitle, Code: "x.y"}}
	}
	return OrderView{Back: "/invoices/1", Header: OrderHeader{Number: "PO1", Status: Label{Raw: "released"}, Date: day, Total: Money{Amount: "1.00", Currency: "EUR"}},
		Items:    []OrderItemView{{No: 1, Description: "Paper", Ordered: "10", Received: "10", Unit: "EA", Price: Money{Amount: "1.00", Currency: "EUR"}}},
		Receipts: []ReceiptView{{Number: "GR1", Date: day, OrderItem: 1, Quantity: "10", Unit: "EA"}}}
}

var day = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

// approval is a request, an agent's when agent is true.
func approval(agent bool) Approval {
	return Approval{Path: "/approvals/1", Title: "Requisition PR1", Kind: Label{Key: i18n.ObjectRequisition}, Status: Label{Key: i18n.StatusPending},
		Amount: &Money{Amount: "12000.00", Currency: "EUR"}, Requester: Name{Text: "Avery", Agent: agent}, Requested: day, Step: 1, StepCount: 2,
		ObjectID: "0192"}
}

func inboxFull() InboxPage {
	return InboxPage{Approvals: []Approval{approval(true), approval(false)}, More: "/approvals?page=t", Done: i18n.InboxDoneApproved,
		Barred: []string{"workflow.request.decide"}}
}

func refusal() Refusal {
	return Refusal{Title: i18n.RefusalTitle, Message: i18n.ProblemSoD, Code: "identity.sod_conflict", Trace: "abc", Barred: []string{"a.b"}}
}

func forms() DecideForms {
	return DecideForms{Action: "/approvals/1", CSRF: "c", ETag: `"1"`, ApproveKey: "k1", RejectKey: "k2", Comment: "ok", ReasonError: i18n.DecideReasonNeeded}
}

// requestPage is a request's page: open to decide, with a refusal and
// decisions, or decided, with none.
func requestPage(open bool) RequestPage {
	if !open {
		return RequestPage{Approval: approval(false), Why: i18n.DecideClosed}
	}
	r := refusal()
	return RequestPage{Approval: approval(true), Steps: []Step{{No: 1, Required: 1, Approvals: 0, Current: true}},
		Decisions: []Decision{{Label: Label{Key: i18n.StatusApproved}, By: Name{Text: "B"}, At: day}}, Decide: new(forms()), Refusal: &r}
}

func locale(t *testing.T) context.Context {
	t.Helper()
	c, err := i18n.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	return templ.WithNonce(i18n.With(context.Background(), c.Default()), "nonce-value")
}

// TestComponentsRender renders every component as a page would (into a
// plain writer, also with nil children) and then into a writer that refuses its first, second, …
// write until one succeeds, so every write's error path in the generated
// code runs and returns the error; and with a cancelled context, which
// every component refuses before writing.
func TestComponentsRender(t *testing.T) {
	ctx := locale(t)
	size := templruntime.DefaultBufferSize
	// One byte: every write reaches the failing writer at once.
	templruntime.DefaultBufferSize = 1
	t.Cleanup(func() { templruntime.DefaultBufferSize = size })
	for name, c := range components() {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := c.Render(ctx, &out); err != nil || out.Len() == 0 {
				t.Fatalf("render: %v, %d bytes", err, out.Len())
			}
			// templ's generated code replaces nil children with none.
			if err := c.Render(templ.WithChildren(ctx, nil), &out); err != nil {
				t.Errorf("render with nil children: %v", err)
			}
			if err := c.Render(ctx, &failAfter{}); !errors.Is(err, errWrite) {
				t.Errorf("render into a refusing writer: got %v, want the write's error", err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := c.Render(cancelled, &out); !errors.Is(err, context.Canceled) {
				t.Errorf("cancelled: got %v, want context.Canceled", err)
			}
			for n := 0; ; n++ {
				if n > 40000 {
					t.Fatal("no render succeeded within 40000 writes")
				}
				var b templruntime.Buffer
				b.Reset(&failAfter{n: n})
				err := c.Render(ctx, &b)
				if err == nil {
					err = b.Flush()
				}
				if err == nil {
					break
				}
				if !errors.Is(err, errWrite) {
					t.Fatalf("write %d: got %v, want the write's error", n, err)
				}
			}
		})
	}
}

// attr is the value of an element's attribute.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// find is the first element with tag below n.
func find(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, tag); f != nil {
			return f
		}
	}
	return nil
}

func parse(t *testing.T, ctx context.Context, c templ.Component) *html.Node {
	t.Helper()
	var out bytes.Buffer
	if err := c.Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(&out)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// signedIn is a signed-in person's shell.
func signedIn() Shell {
	s := shell()
	s.Person = &Person{CSRF: "token-1"}
	return s
}

// continuing is a continue page's shell.
func continuing() Shell {
	s := shell()
	s.Continue = "/health?x=1"
	return s
}

// TestSignedInLayout: a signed-in person's page has the sign-out form
// with the CSRF token, which htmx also sends as a header; a continue page
// goes on to its path at once; a page before sign-in has neither.
func TestSignedInLayout(t *testing.T) {
	ctx := locale(t)
	doc := parse(t, ctx, Home(signedIn(), HomePage{}))
	form := find(doc, "form")
	if got, _ := attr(form, "action"); got != "/logout" {
		t.Errorf("sign-out form: action %q", got)
	}
	if got, _ := attr(find(form, "input"), "value"); got != "token-1" {
		t.Errorf("sign-out form: token %q", got)
	}
	if got, _ := attr(find(doc, "body"), "hx-headers"); got != `{"X-CSRF-Token":"token-1"}` {
		t.Errorf("htmx headers: got %q", got)
	}
	cont := parse(t, ctx, Continue(continuing()))
	var refresh string
	walkAll(cont, func(n *html.Node) {
		if v, _ := attr(n, "http-equiv"); n.Data == "meta" && v == "refresh" {
			refresh, _ = attr(n, "content")
		}
	})
	if refresh != "0;url=/health?x=1" {
		t.Errorf("continue: refresh %q", refresh)
	}
	plain := parse(t, ctx, SignIn(shell(), SignInForm{}))
	if _, ok := attr(find(plain, "body"), "hx-headers"); ok {
		t.Error("before sign-in: htmx headers set")
	}
	walkAll(parse(t, ctx, SignIn(shell(), SignInForm{Code: "x", Error: i18n.SignInInvalid})), func(n *html.Node) {
		if id, _ := attr(n, "id"); id != "code" {
			return
		}
		invalid, _ := attr(n, "aria-invalid")
		described, _ := attr(n, "aria-describedby")
		if invalid != "true" || described != "code-hint code-error" {
			t.Errorf("the code with an error: aria-invalid %q, aria-describedby %q", invalid, described)
		}
	})
}

func walkAll(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkAll(c, fn)
	}
}

func TestLayout(t *testing.T) {
	doc := parse(t, locale(t), Home(shell(), HomePage{}))
	root := find(doc, "html")
	for key, want := range map[string]string{"lang": "en", "dir": "ltr"} {
		if got, _ := attr(root, key); got != want {
			t.Errorf("html %s: got %q, want %q", key, got, want)
		}
	}
	script := find(doc, "script")
	for key, want := range map[string]string{"src": "/static/vendor/htmx.0.js", "nonce": "nonce-value", "defer": ""} {
		if got, ok := attr(script, key); !ok || got != want {
			t.Errorf("script %s: got %q (%v), want %q", key, got, ok, want)
		}
	}
	nav := find(find(doc, "nav"), "a")
	if got, _ := attr(nav, "aria-current"); got != "page" {
		t.Errorf("the current page's navigation entry: aria-current %q, want page", got)
	}
	if got := find(doc, "title").FirstChild.Data; got != "Your workspace – ERP workspace" {
		t.Errorf("title: got %q", got)
	}
}

// TestNoLocale: outside a request with a locale the page still says its
// language, English, and shows keys rather than failing.
func TestNoLocale(t *testing.T) {
	doc := parse(t, context.Background(), Tag(i18n.TagPlanned, "success"))
	strong := find(doc, "strong")
	if got, _ := attr(strong, "class"); got != "tag tag--success" {
		t.Errorf("class: got %q, want tag tag--success", got)
	}
	if got := strong.FirstChild.Data; got != string(i18n.TagPlanned) {
		t.Errorf("text: got %q, want the key", got)
	}
	if got := htmlAttrs(context.Background()); got["lang"] != "en" || got["dir"] != "ltr" {
		t.Errorf("html attributes without a locale: got %v, want en, ltr", got)
	}
}

func TestStatusRows(t *testing.T) {
	ctx := locale(t)
	for name, tc := range map[string]struct {
		st   Status
		want string
	}{
		"revision":    {Status{Version: "v1", Revision: "abc123"}, "abc123"},
		"no revision": {Status{Version: "v1"}, "Not recorded"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := statusRows(tc.st)[3].Value.Render(ctx, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("revision row: got %q, want %q in it", out.String(), tc.want)
			}
		})
	}
}

// TestLinksSanitised: a link templ cannot trust is replaced.
func TestLinksSanitised(t *testing.T) {
	if got := link("javascript:alert(1)", "")["href"]; got != "about:invalid#TemplFailedSanitizationURL" {
		t.Errorf("href: got %v, want templ's replacement", got)
	}
}

// TestEscaped: values are escaped, never markup.
func TestEscaped(t *testing.T) {
	var out bytes.Buffer
	if err := Value(`<script>alert("x")</script>`).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "<script>") {
		t.Errorf("got %q, want it escaped", got)
	}
}

// TestStatusKind: a status's tag colour.
func TestStatusKind(t *testing.T) {
	for k, want := range map[i18n.Key]string{i18n.StatusApproved: "success", i18n.StatusRejected: "inactive", i18n.StatusCancelled: "inactive",
		i18n.StatusPending: "", "": ""} {
		if got := statusKind(Label{Key: k}); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

// TestAmountWithoutLocale: outside a request with a locale an amount is
// the API's.
func TestAmountWithoutLocale(t *testing.T) {
	var out bytes.Buffer
	if err := amount(Money{Amount: "1.00", Currency: "EUR"}).Render(context.Background(), &out); err != nil || out.String() != "1.00 EUR" {
		t.Errorf("got %q, %v", out.String(), err)
	}
}

// TestSignedInNav: each job's screen joins the navigation of a person
// who may read it: the inbox, the invoices, the bank statements.
func TestSignedInNav(t *testing.T) {
	paths := func(nav []NavItem) string {
		var out []string
		for _, n := range nav {
			out = append(out, n.Path)
		}
		return strings.Join(out, " ")
	}
	for name, tc := range map[string]struct {
		jobs NavJobs
		want string
	}{
		"none":      {NavJobs{}, "/ /health"},
		"approvals": {NavJobs{Approvals: true}, "/ /approvals /health"},
		"invoices":  {NavJobs{Invoices: true}, "/ /invoices /health"},
		"bank":      {NavJobs{Bank: true}, "/ /bank /health"},
		"payments":  {NavJobs{Payments: true}, "/ /payments /health"},
		"close":     {NavJobs{Close: true}, "/ /close /health"},
		"all":       {NavJobs{Approvals: true, Invoices: true, Bank: true, Payments: true, Close: true}, "/ /approvals /invoices /bank /payments /close /health"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := paths(SignedInNav(tc.jobs)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := navOf(Shell{Nav: SignedInNav(NavJobs{Approvals: true, Invoices: true})}); len(got) != 4 {
		t.Errorf("a shell's own: got %v", got)
	}
}

// TestDecideFormFields: a form's comment names its hint and error, and is
// marked invalid with one.
func TestDecideFormFields(t *testing.T) {
	doc := parse(t, locale(t), DecideForm(forms(), false))
	ta := find(doc, "textarea")
	for key, want := range map[string]string{"id": "reject-reason", "name": "comment", "aria-describedby": "reject-reason-hint reject-reason-error", "aria-invalid": "true"} {
		if got, _ := attr(ta, key); got != want {
			t.Errorf("textarea %s: got %q, want %q", key, got, want)
		}
	}
	if got, _ := attr(find(doc, "form"), "action"); got != "/approvals/1/reject" {
		t.Errorf("action: got %q", got)
	}
}

// TestBankHelpers: an account is shown by its last four characters
// alone; the list's path drops empty filters; each status has its colour.
func TestBankHelpers(t *testing.T) {
	for account, want := range map[string]string{"DE89370400440532013000": "•••• 3000", "3000": "•••• 3000", "12": "•••• 12"} {
		if got := Masked(account); got != want {
			t.Errorf("Masked(%q): got %q, want %q", account, got, want)
		}
	}
	if got := StatementsPath(url.Values{"status": {""}, "house_bank_id": {"b1"}}); got != "/bank?house_bank_id=b1" {
		t.Errorf("StatementsPath: got %q", got)
	}
	if got := StatementsPath(url.Values{"status": {""}}); got != BankPath {
		t.Errorf("StatementsPath, nothing chosen: got %q", got)
	}
	for l, want := range map[Label]string{{Key: i18n.LineStatusPosted}: "success", {Key: i18n.LineStatusExcluded}: "inactive",
		{Key: i18n.LineStatusPending}: "warning", {Key: i18n.LineStatusProposed}: "warning", {Raw: "x"}: ""} {
		if got := lineKind(l); got != want {
			t.Errorf("lineKind(%v): got %q, want %q", l, got, want)
		}
	}
	if !LineRequires(LineExclude) || LineRequires(LineAccept) {
		t.Error("LineRequires: an exclusion needs its reason, an acceptance none")
	}
	for amount, want := range map[string]bool{"0.00": true, "-0.00": true, "0": true, "0.01": false, "-5.00": false, "10": false} {
		if got := Zero(amount); got != want {
			t.Errorf("Zero(%q): got %v, want %v", amount, got, want)
		}
	}
}
