package einvoicing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Sending through Evatra (ADR-0061 §3, §9, As built E3; Evatra's API,
// https://evatra.com/api, as the plan read it on 2026-11-19): POST
// /invoice/send-document-async takes the XML in Base64 with the invoice
// and business types and answers a tracking id; GET
// /invoice/document-status/{trackingId} its processing status; POST
// /invoice/document-process/search finds the company's outgoing documents
// by filename, which the adapter asks before sending after a call that may
// have reached Evatra, since Evatra has no idempotency key. The answers'
// forms beyond trackingId and status are not documented: the adapter reads
// those two fields, and a search's documents under content (a page
// numbered from 0, as its pageNumber is).

// evatraStatus is the form of a processing status the adapter reads.
var evatraStatus = regexp.MustCompile(`^[A-Z_]{1,40}$`)

// sendRequest is POST /invoice/send-document-async's body.
type sendRequest struct {
	CompID         string `json:"compId"`
	Base64Document string `json:"base64Document"`
	InvoiceType    string `json:"invoiceType"`
	BusinessType   string `json:"businessType"`
	InvoiceSource  string `json:"invoiceSource"`
}

// processRequest is POST /invoice/document-process/search's body: the
// company's outgoing documents of one filename, the first page.
type processRequest struct {
	CompanyID  string `json:"companyId"`
	Direction  string `json:"direction"`
	Filename   string `json:"filename"`
	PageNumber int    `json:"pageNumber"`
}

// tracked is a submission as Evatra answers it.
type tracked struct {
	TrackingID string `json:"trackingId"`
	Status     string `json:"status"`
	Filename   string `json:"filename,omitempty"`
}

// Send implements Provider: after a call that may have reached Evatra
// (LookUp) it first looks the document up by its filename and takes the
// submission found, unless Evatra failed to send it (SENDING_FAILED, or one
// of the tracking ids given as failed); otherwise it sends the XML.
// Evatra refusing it (a 4xx other than 401 and 429) is the problem
// einvoicing.send_refused, which another call would meet again.
func (e *Evatra) Send(ctx context.Context, d Outgoing) (Submission, error) {
	if d.LookUp {
		found, ok, err := e.lookUp(ctx, d)
		if err != nil || ok {
			return found, err
		}
	}
	body, _ := json.Marshal(sendRequest{CompID: e.CompanyID, Base64Document: base64.StdEncoding.EncodeToString(d.Content), InvoiceType: "UAE_PINT",
		BusinessType: d.BusinessType, InvoiceSource: "ERP"}) // a struct of strings always marshals
	resp, err := e.call(ctx, http.MethodPost, "/invoice/send-document-async", body)
	if err != nil {
		return Submission{}, err
	}
	if err := answered(resp.Status, "send-document-async "+d.Filename); err != nil {
		return Submission{}, err
	}
	var got tracked
	if err := json.Unmarshal(resp.Body, &got); err != nil || got.TrackingID == "" || len(got.TrackingID) > 200 {
		return Submission{}, fmt.Errorf("einvoicing: evatra send-document-async %s: an answer without a tracking id", d.Filename)
	}
	if !evatraStatus.MatchString(got.Status) {
		got.Status = ProviderPending
	}
	return Submission{TrackingID: got.TrackingID, Status: got.Status}, nil
}

// answered is nil for a 200 (or 201, 202), the refusal for a 4xx other than
// 401 (the token, retried by call) and 429, and an error to try again for
// any other status.
func answered(status int, what string) error {
	switch {
	case status == http.StatusOK || status == http.StatusCreated || status == http.StatusAccepted:
		return nil
	case status >= 400 && status < 500 && status != http.StatusUnauthorized && status != http.StatusTooManyRequests:
		return problem.Unprocessable("einvoicing.send_refused", fmt.Sprintf("Evatra refused %s: answered %d.", what, status))
	}
	return fmt.Errorf("einvoicing: evatra %s: answered %d", what, status)
}

// lookUp finds the company's outgoing document of the filename at Evatra:
// a submission it did not fail to send, found true.
func (e *Evatra) lookUp(ctx context.Context, d Outgoing) (Submission, bool, error) {
	body, _ := json.Marshal(processRequest{CompanyID: e.CompanyID, Direction: "OUTGOING", Filename: d.Filename}) // as above
	resp, err := e.call(ctx, http.MethodPost, "/invoice/document-process/search", body)
	if err != nil {
		return Submission{}, false, err
	}
	if resp.Status != http.StatusOK || resp.Truncated {
		return Submission{}, false, fmt.Errorf("einvoicing: evatra document-process/search %s: answered %d (truncated %v)", d.Filename, resp.Status, resp.Truncated)
	}
	var page struct {
		Content []tracked `json:"content"`
	}
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return Submission{}, false, fmt.Errorf("einvoicing: evatra document-process/search %s: %w", d.Filename, err)
	}
	for _, t := range page.Content {
		if t.Filename == d.Filename && t.TrackingID != "" && len(t.TrackingID) <= 200 && evatraStatus.MatchString(t.Status) &&
			!slices.Contains(d.Failed, t.TrackingID) && !slices.Contains([]string{ProviderSendingFailed, ProviderFailed, ProviderPermanentFailed}, t.Status) {
			return Submission{TrackingID: t.TrackingID, Status: t.Status, Found: true}, true, nil
		}
	}
	return Submission{}, false, nil
}

// Status implements Provider: the submission's processing status.
func (e *Evatra) Status(ctx context.Context, trackingID string) (Report, error) {
	resp, err := e.call(ctx, http.MethodGet, "/invoice/document-status/"+url.PathEscape(trackingID), nil)
	if err != nil {
		return Report{}, err
	}
	if resp.Status != http.StatusOK {
		return Report{}, fmt.Errorf("einvoicing: evatra document-status %s: answered %d", trackingID, resp.Status)
	}
	var got tracked
	if err := json.Unmarshal(resp.Body, &got); err != nil || !evatraStatus.MatchString(got.Status) {
		return Report{}, fmt.Errorf("einvoicing: evatra document-status %s: an answer without a status", trackingID)
	}
	return Report{Status: got.Status}, nil
}
