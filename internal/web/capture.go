package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Capture of a supplier invoice from its document (T73e3's upload, PO
// 2026-11-01, ADR-0052 §7): the person chooses the company and the
// document; erp-web checks the form's CSRF token, starts an upload on the
// API's own signed path (createUpload, via api), streams the document
// there as it arrives (capture_relay.go), captures the invoice from the
// upload (captureSupplierInvoice) and takes the person to it, or, while
// the capture's operation runs, to its progress, polled until it names the
// invoice. The form's page reads getCurrentPrincipal and
// listLegalEntities; the progress getCurrentPrincipal and getOperation.

// maxCaptureDocument is the largest document an upload takes
// (payables.MaxUpload, the API's 50 MB: PO 2026-11-01); the API holds it
// to the upload's max_bytes again while it streams in.
const maxCaptureDocument = 50 << 20

// maxCaptureForm bounds a capture's whole form: the document and the
// form's few fields.
const maxCaptureForm = maxCaptureDocument + maxForm

// The capture's deadlines (PO 2026-11-02): the cell gives the document's
// bytes api.UploadTimeout (five minutes) on its signed path, where every
// other request keeps its listener's 30 seconds, and each hop in front
// allows api.UploadMargin (a minute) more than the one behind it, so the
// cell is the one to cut an upload that overruns. erp-web links no api
// package (ADR-0052 §7), so the values are written out here and
// TestCaptureDeadlines holds them to api's and the router's.
const (
	// captureReadTimeout: the cell's five minutes, from after createUpload
	// (30 seconds at most) and the router's checks: two margins.
	captureReadTimeout = 7 * time.Minute
	// relayTimeout is the relay's whole call, past the router's write
	// deadline (router.UploadWriteTimeout, seven minutes) by a margin.
	relayTimeout = 8 * time.Minute
	// captureWriteTimeout: createUpload, the relay, then
	// captureSupplierInvoice (30 seconds at most) and the page, a margin
	// past them all.
	captureWriteTimeout = 10 * time.Minute
)

// allowTime gives the capture's document captureReadTimeout to arrive from
// now and its answer captureWriteTimeout to be written, where every other
// request keeps the listener's 30 and 60 seconds. Only a capture about to
// read its document gets it: the person signed in, the form's CSRF token
// the session's (csrfStream), the fields before the document read under
// the listener's deadlines. The header's own timeout is the listener's,
// unchanged. A writer without deadlines (a test's recorder) has none to
// lift; the listener's are lifted through the telemetry middleware
// (Unwrap).
func allowTime(w http.ResponseWriter, now time.Time) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(now.Add(captureReadTimeout))
	_ = rc.SetWriteDeadline(now.Add(captureWriteTimeout))
}

// captureTypes are the documents a capture takes, as their first bytes
// show them (http.DetectContentType), and the upload's content type each
// is sent as.
var captureTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "text/xml": true}

// captureProblems are the codes the capture explains in its own words,
// before the invoice screens' and the general ones.
var captureProblems = func() map[string]i18n.Key {
	out := map[string]i18n.Key{
		"payables.unreadable_document": i18n.ProblemUnreadable,
		"payables.already_captured":    i18n.ProblemCaptured,
		"payables.capture_needs_order": i18n.ProblemCaptureOrder,
		"payables.document_too_large":  i18n.ProblemDocumentLarge,
		"payables.upload_expired":      i18n.ProblemUploadExpired,
		"payables.upload_url_expired":  i18n.ProblemUploadExpired,
		"agents.ai_disabled":           i18n.ProblemAIDisabled,
		"agents.documents_withheld":    i18n.ProblemWithheld,
		"agents.budget_exhausted":      i18n.ProblemAIBudget,
	}
	for code, k := range i18n.InvoiceProblems {
		if _, ok := out[code]; !ok {
			out[code] = k
		}
	}
	return out
}()

// captured is what the person sent in the capture form, and what is wrong
// with it; the form is rendered again with it.
type captured struct {
	company             string
	companyErr, fileErr i18n.Key
	sent                bool
	refusal             *ui.Refusal
	sentBefore          string // the form was sent before: the invoice captured then, or the capture's own path
}

// capture is the capture's page: its form, or with ?operation= the
// progress of a capture sent.
func (s *Server) capture(w http.ResponseWriter, r *http.Request, p *person) {
	if op := r.URL.Query().Get(ui.OperationParam); op != "" {
		s.captureWait(w, r, p, op)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showCapture(w, r, p, m, http.StatusOK, captured{})
}

// showCapture renders the form, fresh (a new idempotency key), with what
// was sent and why it was refused.
func (s *Server) showCapture(w http.ResponseWriter, r *http.Request, p *person, m me, status int, c captured) {
	f := ui.CaptureForm{CSRF: p.CSRF, Key: uuid.NewString(), Companies: companyOptions(s.companies(r.Context(), m), c.company),
		CompanyError: c.companyErr, FileError: c.fileErr, Again: c.sent && c.fileErr == "", Refusal: c.refusal, SentBefore: c.sentBefore}
	s.render(w, r, status, ui.Capture(s.page(i18n.CaptureTitle, ui.InvoicesPath, p, m), f))
}

// streamedForm is a multipart form read up to its file: the fields before
// it, and the file's part, unread (nil when none was chosen).
type streamedForm struct {
	fields url.Values
	file   *multipart.Part
}

// maxStreamedFields bounds the fields before the file: the form has three.
const maxStreamedFields = 8

// errStreamedForm is a form that is no capture form.
var errStreamedForm = errors.New("web: the form is not the capture's")

// csrfStream is csrf for a form whose file is streamed rather than held:
// its fields are read up to the file, which comes last, and the token is
// checked as csrf checks it before the file is read or anything is acted
// on. The whole body is bounded by the largest form; a form that is no
// such multipart form, or whose token is not the session's, is refused.
// csrfUpload, which holds a form of up to 4 MB in memory, stays for the
// statement and entry files.
func (s *Server) csrfStream(next func(http.ResponseWriter, *http.Request, *person, streamedForm)) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		r.Body = http.MaxBytesReader(w, r.Body, maxCaptureForm)
		form, err := readStreamed(r)
		if err != nil || !tokenOK(form.fields.Get(ui.CSRFField), p) {
			s.Log.WarnContext(r.Context(), "web: a streamed form without the session's CSRF token was refused", "path", r.URL.Path, "error", err)
			s.refused(w, r)
			return
		}
		next(w, r, p, form)
	}
}

// readStreamed reads a multipart form's fields up to its file part.
func readStreamed(r *http.Request) (streamedForm, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return streamedForm{}, errStreamedForm
	}
	form := streamedForm{fields: url.Values{}}
	for range maxStreamedFields {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}
		if err != nil {
			return streamedForm{}, errStreamedForm
		}
		if part.FormName() == ui.FieldFile {
			if part.FileName() != "" {
				form.file = part
			}
			return form, nil
		}
		value, err := io.ReadAll(io.LimitReader(part, maxForm+1))
		if err != nil || len(value) > maxForm {
			return streamedForm{}, errStreamedForm
		}
		form.fields.Add(part.FormName(), string(value))
	}
	return streamedForm{}, errStreamedForm
}

// captureDocument captures an invoice from the document the form streams:
// checked, uploaded to the API's signed path, captured; done, the person
// goes to the invoice or to the capture's progress; refused, the form says
// why.
func (s *Server) captureDocument(w http.ResponseWriter, r *http.Request, p *person, form streamedForm) {
	key, err := uuid.Parse(form.fields.Get(ui.FieldIdempotency))
	if err != nil {
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody, ui.CapturePath, i18n.CaptureTitle))
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	c := captured{company: form.fields.Get(ui.FieldCompany), sent: true}
	company, err := uuid.Parse(c.company)
	if err != nil {
		c.companyErr = i18n.CaptureCompanyNeeded
	}
	allowTime(w, clock.Now(s.Now))
	doc, contentType, fileErr := sniff(form.file)
	if c.fileErr = fileErr; c.companyErr != "" || c.fileErr != "" {
		s.showCapture(w, r, p, m, http.StatusUnprocessableEntity, c)
		return
	}
	up, ok := s.startUpload(w, r, p, m, key, contentType, &c)
	if !ok {
		return
	}
	switch s.relayDocument(w, r, p, m, up, doc, &c) {
	case relayed:
		s.captureUpload(w, r, p, m, key, company, up.ID, &c)
	case closed:
		s.sentBefore(w, r, p, m, up.ID, &c)
	}
}

// What became of a document relayed: its bytes received, the upload
// holding them already (the form sent again), or the answer written.
const (
	relayed = iota
	closed
	answered
)

// sniff is the document and the content type its first bytes show, or
// what is wrong with it: none chosen or empty, or not a document a
// capture takes.
func sniff(file *multipart.Part) (io.Reader, string, i18n.Key) {
	if file == nil {
		return nil, "", i18n.CaptureFileNeeded
	}
	br := bufio.NewReaderSize(file, 512)
	head, _ := br.Peek(512) // a shorter document, or one cut short, shows what it has; the relay reports a reader's failure
	if len(head) == 0 {
		return nil, "", i18n.CaptureFileNeeded
	}
	contentType, _, _ := mime.ParseMediaType(http.DetectContentType(head)) // DetectContentType names a media type that parses
	if !captureTypes[contentType] {
		return nil, "", i18n.CaptureFileType
	}
	return br, contentType, ""
}

// startUpload starts an upload of the document on the API's own signed
// path (createUpload, via api), with a key derived from the form's, so a
// form sent again finds the same upload. ok is false when the answer is
// already written.
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request, p *person, m me, key uuid.UUID, contentType string, c *captured) (gen.Upload, bool) {
	res, err := s.API.CreateUpload(r.Context(), &gen.UploadCreate{Purpose: "supplier_invoice", ContentType: contentType, Via: gen.NewOptString("api")},
		gen.CreateUploadParams{IdempotencyKey: uploadKey(key)})
	if got, ok := res.(*gen.CreateUploadCreatedHeaders); ok {
		return got.Response, true
	}
	if prob, show := s.refusal(w, r, p, "createUpload", res, err); show {
		c.refusal = explainIn(captureProblems, i18n.CaptureRefusedTitle, prob, m)
		s.showCapture(w, r, p, m, int(prob.Status), *c)
	}
	return gen.Upload{}, false
}

// relayDocument streams the document to the upload's signed path: relayed,
// closed when the upload already holds its document, else answered: the
// document too large, the API's refusal, or the relay failing.
func (s *Server) relayDocument(w http.ResponseWriter, r *http.Request, p *person, m me, up gen.Upload, doc io.Reader, c *captured) int {
	prob, err := s.relay.put(r.Context(), up, doc)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, errTooLarge), errors.As(err, &tooLarge):
		c.fileErr = i18n.CaptureFileTooLarge
		s.showCapture(w, r, p, m, http.StatusRequestEntityTooLarge, *c)
	case err != nil:
		s.apiFailed(w, r, p, "the upload's signed path", nil, err)
	case prob == nil:
		return relayed
	case prob.Code == "payables.upload_closed":
		return closed
	case prob.Status == http.StatusUnauthorized:
		s.apiFailed(w, r, p, "the upload's signed path", &gen.UnauthorizedHeaders{Response: *prob}, nil)
	default:
		if refused, show := s.refusal(w, r, p, "the upload's signed path", prob, nil); show {
			c.refusal = explainIn(captureProblems, i18n.CaptureRefusedTitle, refused, m)
			s.showCapture(w, r, p, m, int(refused.Status), *c)
		}
	}
	return answered
}

// sentBefore answers a form sent again, whose upload already holds a
// document: the form is shown again, fresh, saying so, with a link to the
// invoice captured from it when there is one. The document sent now may
// not be the one the upload holds (a form restored by Back and sent with
// another file), and erp-web cannot tell without reading it, so it is
// never taken for it (go-reviewer, round 1): the person opens the invoice
// or sends the document again with the fresh form. The capture's own key
// could not replay the first answer either: a multipart call's body
// differs by its boundary each time, so the API takes it for another
// request.
func (s *Server) sentBefore(w http.ResponseWriter, r *http.Request, p *person, m me, upload uuid.UUID, c *captured) {
	res, err := s.API.GetUpload(r.Context(), gen.GetUploadParams{ID: upload})
	got, ok := res.(*gen.GetUploadOKHeaders)
	if !ok {
		if prob, show := s.refusal(w, r, p, "getUpload", res, err); show {
			c.refusal = explainIn(captureProblems, i18n.CaptureRefusedTitle, prob, m)
			s.showCapture(w, r, p, m, int(prob.Status), *c)
		}
		return
	}
	c.sentBefore = ui.CapturePath
	if invoice, used := got.Response.UsedBy.Get(); used {
		c.sentBefore = invoicePath(invoice)
	}
	s.showCapture(w, r, p, m, http.StatusConflict, *c)
}

// captureUpload captures the invoice from the upload with the form's key:
// done, the person goes to the invoice, or to the capture's progress while
// it runs; refused, the form says why.
func (s *Server) captureUpload(w http.ResponseWriter, r *http.Request, p *person, m me, key, company, upload uuid.UUID, c *captured) {
	res, err := s.API.CaptureSupplierInvoice(r.Context(), &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: company, UploadID: gen.NewOptUUID(upload)},
		gen.CaptureSupplierInvoiceParams{IdempotencyKey: key.String()})
	if op, ok := res.(*gen.OperationHeaders); ok {
		http.Redirect(w, r, captureNext(op.Response), http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "captureSupplierInvoice", res, err); show {
		c.refusal = explainIn(captureProblems, i18n.CaptureRefusedTitle, prob, m)
		s.showCapture(w, r, p, m, int(prob.Status), *c)
	}
}

// captureNext is where a capture goes on to: its invoice once the
// operation names it, else its progress.
func captureNext(op gen.Operation) string {
	if id, ok := capturedInvoice(op); ok {
		return invoicePath(id)
	}
	return ui.CaptureWaitPath(op.ID.String())
}

// capturedInvoice is the invoice a capture's operation entered, once it
// has succeeded.
func capturedInvoice(op gen.Operation) (uuid.UUID, bool) {
	resource, ok := op.Resource.Get()
	raw, found := strings.CutPrefix(resource, "/v1/supplier-invoices/")
	id, err := uuid.Parse(raw)
	return id, op.Status == "succeeded" && ok && found && err == nil
}

// captureWait is a capture's progress (?operation=): the page, or under
// htmx its status alone (part=status), asked again every two seconds
// until the operation names the invoice, where the person then goes; a
// status the poll already shows is answered 204, as a run's progress is
// (T73f2).
func (s *Server) captureWait(w http.ResponseWriter, r *http.Request, p *person, operation string) {
	id, err := uuid.Parse(operation)
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Add("Vary", "HX-Request")
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	v := ui.CaptureWait{Self: ui.CaptureWaitPath(id.String())}
	res, err := s.API.GetOperation(r.Context(), gen.GetOperationParams{ID: id})
	got, ok := res.(*gen.GetOperationOKHeaders)
	if !ok {
		if v.Refusal, ok = s.inPlace(w, r, p, m, captureProblems, "getOperation", res, err, i18n.CaptureWaitRefused, "capture-refusal"); ok {
			s.render(w, r, refusedStatus(res), ui.CaptureWaitPage(s.page(i18n.CaptureWaitTitle, ui.InvoicesPath, p, m), v))
		}
		return
	}
	op := got.Response
	if op.Kind != "payables.capture" {
		s.notFound(w, r)
		return
	}
	v.Status, v.Running = labelOf(operationStatuses, op.Status), running(op)
	if prob, ok := op.Error.Get(); ok {
		v.Error = explainIn(captureProblems, i18n.CaptureWaitStopped, prob, m)
		v.Error.ID = "capture-error"
	}
	s.answerWait(w, r, p, m, op, v)
}

// answerWait answers the progress as the request asks: the invoice once
// the operation names it (HX-Redirect under htmx), the status alone to the
// poll, or the page.
func (s *Server) answerWait(w http.ResponseWriter, r *http.Request, p *person, m me, op gen.Operation, v ui.CaptureWait) {
	poll := r.Header.Get("HX-Request") == "true" && r.URL.Query().Get("part") == "status"
	if v.Running {
		v.Shown = captureShown(r.Context(), v)
	}
	invoice, done := capturedInvoice(op)
	switch {
	case done && poll:
		w.Header().Set("HX-Redirect", invoicePath(invoice))
		s.render(w, r, http.StatusOK, ui.CaptureWaitStatus(v))
	case done:
		http.Redirect(w, r, invoicePath(invoice), http.StatusSeeOther)
	case poll && !v.Running:
		w.Header().Set("HX-Redirect", v.Self) // the page says why it stopped
		s.render(w, r, http.StatusOK, ui.CaptureWaitStatus(v))
	case poll && r.URL.Query().Get(ui.ShownParam) == v.Shown:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case poll:
		s.render(w, r, http.StatusOK, ui.CaptureWaitPoll(v))
	default:
		s.render(w, r, http.StatusOK, ui.CaptureWaitPage(s.page(i18n.CaptureWaitTitle, ui.InvoicesPath, p, m), v))
	}
}

// captureShown is a digest of the status as the person reads it, as a
// run's progress digests its own (shown).
func captureShown(ctx context.Context, v ui.CaptureWait) string {
	h := sha256.New()
	// A status that does not render answers the page's own render with a
	// 500; its digest then matches no poll's, so the status is sent again.
	_ = ui.CaptureWaitStatus(v).Render(ctx, h)
	return hex.EncodeToString(h.Sum(nil)[:12])
}
