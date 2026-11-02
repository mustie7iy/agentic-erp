package ui

import (
	"net/url"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// CapturePath is where a supplier invoice is captured from its document
// (ADR-0052 §7, PO 2026-11-01): the form, posted to the same path, and
// with OperationParam the capture's progress until it names the invoice.
// A path of its own under the invoices would collide with an invoice's
// actions (/invoices/{id}/{verb}).
const CapturePath = InvoicesPath + "/capture"

// OperationParam names the capture's operation on CapturePath.
const OperationParam = "operation"

// CaptureWaitPath is the progress of a capture's operation.
func CaptureWaitPath(operation string) string {
	return CapturePath + "?" + url.Values{OperationParam: {operation}}.Encode()
}

// CaptureForm is the capture form: the company the invoice is addressed to
// and the document, posted as a multipart form with the CSRF token and a
// fresh idempotency key, in that order, the document last, so erp-web
// checks the token before a byte of the document arrives and streams the
// document on to the API as it does.
type CaptureForm struct {
	CSRF         string
	Key          string
	Companies    []Option // nil: the companies could not be listed, so the form cannot be sent
	CompanyError i18n.Key
	FileError    i18n.Key
	Again        bool     // the form was sent and is shown again: its file is chosen again
	Refusal      *Refusal // the API's refusal of the upload or of the capture
	// SentBefore is set when this form's key was sent before: the invoice
	// captured then, or CapturePath when none was.
	SentBefore string
}

// CaptureWait is a capture's operation while it runs, or why it stopped.
type CaptureWait struct {
	Self    string // the wait's own path
	Status  Label
	Running bool
	Shown   string   // while it runs, the digest of the status shown, which the poll sends back
	Error   *Refusal // why the operation failed
	Refusal *Refusal // the API's refusal to read it
}

// capturePoll asks for the wait's status again every two seconds, with
// the digest of the status shown.
func capturePoll(self, shown string) templ.Attributes {
	u, _ := url.Parse(self) // CaptureWaitPath's, which parses
	q := u.Query()
	q.Set("part", "status")
	q.Set(ShownParam, shown)
	u.RawQuery = q.Encode()
	return templ.Attributes{"hx-get": string(templ.URL(u.String())), "hx-trigger": "every 2s", "hx-swap": "outerHTML"}
}

// captureCompany is the form's company select.
func captureCompany(err i18n.Key) templ.Attributes {
	return uploadSelect("capture-company", FieldCompany, err)
}
