package ui

import (
	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// captureComponents are the capture by upload's components (PO 2026-11-01)
// for TestComponentsRender, each rendered on its own, so the write and
// context error paths of the generated code run (batch 38: the upload
// lane's components were rendered only inside its pages, so their error paths never ran).
func captureComponents() map[string]templ.Component {
	companies := []Option{{Value: "1", Text: "Acme"}, {Value: "2", Text: "Beta", Selected: true}}
	form := CaptureForm{CSRF: "c", Key: "k", Companies: companies, CompanyError: i18n.CaptureCompanyNeeded, FileError: i18n.CaptureCompanyNeeded,
		Again: true, Refusal: &Refusal{Title: i18n.CaptureRefusedTitle, Code: "payables.unreadable_document"}, SentBefore: "/invoices/1"}
	running := CaptureWait{Self: CaptureWaitPath("op-1"), Status: Label{Key: i18n.CaptureWaitTitle}, Running: true, Shown: "d"}
	stopped := CaptureWait{Self: CaptureWaitPath("op-1"), Status: Label{Key: i18n.CaptureWaitTitle}, Error: &Refusal{Title: i18n.CaptureRefusedTitle, Code: "x.y"}}
	return map[string]templ.Component{
		"Capture":              Capture(signedIn(), form),
		"queue with capture":   invoicesContent(InvoiceList{Capture: true}),
		"capture sent before":  captureContent(CaptureForm{Companies: companies[:1], SentBefore: CapturePath}),
		"capture no companies": captureContent(CaptureForm{}),
		"CaptureFormView":      CaptureFormView(form),
		"CaptureWaitPage":      CaptureWaitPage(signedIn(), running),
		"captureWaitContent":   captureWaitContent(stopped),
		"capture wait refused": captureWaitContent(CaptureWait{Refusal: &Refusal{Title: i18n.CaptureRefusedTitle, Code: "x.y"}}),
		"CaptureWaitPoll":      CaptureWaitPoll(running),
		"CaptureWaitStatus":    CaptureWaitStatus(stopped),
	}
}
