package einvoicing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Delivery and FTA reporting through Evatra (ADR-0061 §9, As built E4;
// Evatra's API, https://evatra.com/api, as the plan read it on
// 2026-11-19, which names the calls and their statuses but not their
// parameters or answers): GET /peppol/invoice-mls-list lists the message
// level responses of a submission, GET /peppol/get-tdd reads its tax data
// document, and POST /peppol/tdd/retry-failed and /peppol/tdd/withdraw act
// on it. The adapter names the submission by companyId and trackingId,
// reads a list's responses under content, each with its status and
// description, and a tax data document's status; the forms are assumed
// (As built E4, for Evatra to confirm).

// mlsAnswer is what the adapter reads of GET /peppol/invoice-mls-list.
type mlsAnswer struct {
	Content []struct {
		Status      string `json:"status"`
		Description string `json:"description"`
	} `json:"content"`
}

// tddRequest is the body of POST /peppol/tdd/retry-failed and
// /peppol/tdd/withdraw.
type tddRequest struct {
	CompanyID  string `json:"companyId"`
	TrackingID string `json:"trackingId"`
}

// taxDataPaths are the calls of a person's requests.
var taxDataPaths = map[string]string{TaxDataRetry: "/peppol/tdd/retry-failed", TaxDataWithdraw: "/peppol/tdd/withdraw"}

// Delivery implements Provider: the submission's message level response
// (the last final one listed, or else the last listed) and its tax data
// document's status (none while Evatra answers 404).
func (e *Evatra) Delivery(ctx context.Context, trackingID string) (Delivery, error) {
	query := "?companyId=" + url.QueryEscape(e.CompanyID) + "&trackingId=" + url.QueryEscape(trackingID)
	resp, err := e.call(ctx, http.MethodGet, "/peppol/invoice-mls-list"+query, nil)
	if err != nil {
		return Delivery{}, err
	}
	if resp.Status != http.StatusOK || resp.Truncated {
		return Delivery{}, fmt.Errorf("einvoicing: evatra invoice-mls-list %s: answered %d (truncated %v)", trackingID, resp.Status, resp.Truncated)
	}
	var list mlsAnswer
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return Delivery{}, fmt.Errorf("einvoicing: evatra invoice-mls-list %s: %w", trackingID, err)
	}
	var d Delivery
	for _, r := range list.Content {
		if !evatraStatus.MatchString(r.Status) {
			return Delivery{}, fmt.Errorf("einvoicing: evatra invoice-mls-list %s: a response without a status", trackingID)
		}
		if !responseFinal(d.Response) || responseFinal(r.Status) {
			d.Response, d.Detail = r.Status, clipBytes(strings.TrimSpace(r.Description), 1000)
		}
	}
	d.TaxData, err = e.taxData(ctx, trackingID, query)
	return d, err
}

// taxData is the submission's tax data document's status; empty while
// Evatra has none (404).
func (e *Evatra) taxData(ctx context.Context, trackingID, query string) (string, error) {
	resp, err := e.call(ctx, http.MethodGet, "/peppol/get-tdd"+query, nil)
	switch {
	case err != nil:
		return "", err
	case resp.Status == http.StatusNotFound:
		return "", nil
	case resp.Status != http.StatusOK:
		return "", fmt.Errorf("einvoicing: evatra get-tdd %s: answered %d", trackingID, resp.Status)
	}
	var got tracked
	if err := json.Unmarshal(resp.Body, &got); err != nil || !evatraStatus.MatchString(got.Status) {
		return "", fmt.Errorf("einvoicing: evatra get-tdd %s: an answer without a status", trackingID)
	}
	return got.Status, nil
}

// TaxData implements Provider: POST /peppol/tdd/retry-failed or
// /peppol/tdd/withdraw for the submission; Evatra refusing it (a 4xx
// other than 401 and 429) is the problem einvoicing.tax_data_refused.
func (e *Evatra) TaxData(ctx context.Context, trackingID, request string) error {
	path, ok := taxDataPaths[request]
	if !ok {
		return fmt.Errorf("einvoicing: evatra: no tax data request %q", request)
	}
	body, _ := json.Marshal(tddRequest{CompanyID: e.CompanyID, TrackingID: trackingID}) // a struct of strings always marshals
	resp, err := e.call(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	return answered(resp.Status, "einvoicing.tax_data_refused", strings.TrimPrefix(path, "/peppol/")+" "+trackingID)
}

// responseFinal reports whether a message level response is final: any
// but ACKNOWLEDGING, or none.
func responseFinal(status string) bool {
	return status != "" && status != ResponseAcknowledging
}

// taxDataFinal reports whether a tax data document's status is final:
// any but none, TDD_SEND and WITHDRAW_SEND, which the authority has not
// answered yet.
func taxDataFinal(status string) bool {
	return !slices.Contains([]string{"", TaxDataSent, TaxDataWithdrawSent}, status)
}
