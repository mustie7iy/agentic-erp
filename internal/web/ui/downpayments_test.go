package ui

import (
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// downPaymentComponents are the clearing of down payments' components
// (T73e4) for TestComponentsRender, each rendered on its own, so the write
// and context error paths of the generated code run.
func downPaymentComponents() map[string]templ.Component {
	paid, tax := Money{Amount: "119.00", Currency: "EUR"}, Money{Amount: "19.00", Currency: "EUR"}
	offered := DownPaymentChoice{Value: "2026:e:1", Request: "DP1", Entry: "100001", Reference: "AB-1", Posting: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
		Amount: &paid, Tax: &tax, Chosen: true}
	refused := DownPaymentChoice{Entry: "100002", Posting: offered.Posting, Why: i18n.DownPaymentsNoRequest}
	page := DownPaymentsPage{Back: "/invoices/1", Number: "SI1", Supplier: Supplier{ID: "s", Number: "BP1", Name: "Acme"}, Gross: paid,
		Action: "/invoices/1/down-payments", CSRF: "c", ETag: `"2"`, Key: "k", Page: "t1", Date: "2026-03-20", DateError: i18n.ActionDateInvalid,
		ChooseError: i18n.DownPaymentsChooseNeeded, Items: []DownPaymentChoice{offered, refused}, More: "/invoices/1/down-payments?page=t2",
		Refusal: &Refusal{Title: i18n.DownPaymentsRefusedTitle, Code: "payables.down_payments_exceed_invoice"}}
	shown := page
	shown.Items = []DownPaymentChoice{refused}
	return map[string]templ.Component{
		"DownPayments":                 DownPayments(signedIn(), page),
		"downPaymentsContent":          downPaymentsContent(page),
		"down payments closed":         downPaymentsContent(DownPaymentsPage{Closed: true}),
		"down payments refused":        downPaymentsContent(DownPaymentsPage{ItemsRefusal: &Refusal{Title: i18n.DownPaymentsRefusedTitle, Code: "x.y"}}),
		"down payments none":           downPaymentsContent(DownPaymentsPage{}),
		"down payments none offered":   downPaymentsContent(shown),
		"down payments one page":       downPaymentsContent(DownPaymentsPage{Items: []DownPaymentChoice{refused}}),
		"downPaymentsForm":             downPaymentsForm(page),
		"down payments form, one page": downPaymentsForm(DownPaymentsPage{Items: []DownPaymentChoice{{Value: "v", Posting: offered.Posting}}}),
		"DownPaymentsTable":            DownPaymentsTable(page.Items, ""),
		"downPaymentRow":               downPaymentRow(0, offered, i18n.DownPaymentsChooseNeeded),
		"down payment not offered":     downPaymentRow(1, refused, ""),
		"actions with down payments":   ActionsSection(InvoicePage{DownPayments: "/invoices/1/down-payments", Why: i18n.ActionsNone}),
	}
}
