package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// EvatraHosts are the base URLs of Evatra's two environments, the only
// hosts its adapter calls (ADR-0061 §3 As built E1; Evatra's API reference,
// https://evatra.com/api, read 2026-11-19): the connector names the
// environment, never a URL. Only the walkthrough replaces them, with its
// stand-in (ERP_WALKTHROUGH_PROVIDER, development only). Each call answers
// a copy, so no caller changes the hosts the adapter calls.
func EvatraHosts() map[string]string {
	return map[string]string{
		"stage": "https://api-stage.evatrauae.com",
		"prod":  "https://api.evatrauae.com",
	}
}

// Evatra's bounds as the adapter reads them: pages of 200 documents (its
// largest), and the received documents of the last 60 days listed on each
// fetch (EvatraLookback; ADR-0061 As built E1: Evatra keeps no
// acknowledgement, so each fetch lists what it still holds and leaves out
// what was recorded and acknowledged).
const (
	EvatraPage     = 200
	EvatraLookback = 60 * 24 * time.Hour
	// evatraSkew is how long before its expirationDate a token is no
	// longer used, so a call never carries one about to expire.
	evatraSkew = time.Minute
	// evatraMaxPages bounds one company's listing: 25 pages of 200, five
	// thousand documents in the lookback, far above a pilot company's, and
	// within what one fetch workflow's history holds (an activity per
	// document, about six events each, under the engine's 51,200).
	evatraMaxPages = 25
)

// evatraCredentials are an Evatra connector's secret: the API key and the
// account's email, as its login takes them (po-2026-11-16 H2: the
// tenant-scoped connector secret).
type evatraCredentials struct {
	APIKey string `json:"apiKey"`
	Email  string `json:"email"`
}

// Evatra is the adapter of Evatra (ADR-0061 §3, po:2026-11-19), one
// connector's: it logs in with the connector's credentials (POST
// /auth/login), keeps the token (jwt) until its expirationDate in Sessions,
// sends it as the R-Auth header, and logs in once again when a call is
// answered 401. Pending lists each company's received documents (POST
// /invoice/search-documents, documentType INCOMING, by the TIN of the
// company's participant), Fetch reads one (GET
// /invoice/get-document/{id}/XML, Base64), and Acknowledge is local only:
// Evatra has no acknowledgement. The credentials and the token are never
// logged, audited or put in an error.
type Evatra struct {
	Client       *egress.Client
	BaseURL      string            // EvatraHosts()' for the connector's environment
	CompanyID    string            // the company's id at Evatra (companyId)
	Participants map[string]string // company code -> 0235:<TIN>
	Sessions     *EvatraSessions
	Now          func() time.Time

	key         sessionKey
	credentials evatraCredentials
}

// newEvatra is the Evatra adapter of a connector: refused when its
// environment has no host or its secret is not Evatra's credentials.
func newEvatra(client *egress.Client, hosts map[string]string, sessions *EvatraSessions, now func() time.Time, c Connector) (*Evatra, error) {
	base, ok := hosts[c.Environment]
	if !ok {
		return nil, problem.Unprocessable("integration.invalid_settings", "Connector "+c.Name+" names Evatra's environment "+c.Environment+", which has no host.")
	}
	var creds evatraCredentials
	if err := json.Unmarshal([]byte(c.Secret), &creds); err != nil || creds.APIKey == "" || creds.Email == "" {
		// the decoder's error may quote the secret: it is not kept
		return nil, problem.Unprocessable("integration.secret_unavailable",
			"The credentials of connector "+c.Name+" are not Evatra's: a JSON object with apiKey and email.")
	}
	return &Evatra{Client: client, BaseURL: base, CompanyID: c.CompanyID, Participants: c.Participants, Sessions: sessions, Now: now,
		key: sessionKey{connector: c.ID, credentials: sha256.Sum256([]byte(c.Secret))}, credentials: creds}, nil
}

// EvatraSessions keeps Evatra's tokens, one per connector and credentials,
// until they expire: a rotated secret is another key, so it logs in again.
// It is shared by the adapters of every connector the server calls, and
// safe for concurrent use; no lock is held across a call to Evatra.
type EvatraSessions struct {
	mu     sync.Mutex
	tokens map[sessionKey]evatraSession
}

type sessionKey struct {
	connector   uuid.UUID
	credentials [sha256.Size]byte
}

type evatraSession struct {
	jwt     string
	expires time.Time
}

// get is the connector's token, if one is kept and not about to expire.
func (s *EvatraSessions) get(key sessionKey, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[key]
	if !ok || !now.Add(evatraSkew).Before(t.expires) {
		return "", false
	}
	return t.jwt, true
}

// put keeps a token until it expires, dropping the tokens expired since.
func (s *EvatraSessions) put(key sessionKey, t evatraSession, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[sessionKey]evatraSession{}
	}
	for k, old := range s.tokens {
		if !now.Before(old.expires) {
			delete(s.tokens, k)
		}
	}
	s.tokens[key] = t
}

// drop forgets the token Evatra refused, unless another call has kept a
// newer one meanwhile.
func (s *EvatraSessions) drop(key sessionKey, jwt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[key].jwt == jwt {
		delete(s.tokens, key)
	}
}

// AcknowledgesLocally implements LocalAcknowledgement: Evatra has no
// acknowledgement, so its listing holds documents already kept.
func (*Evatra) AcknowledgesLocally() {}

// Acknowledge implements Provider: local only, the fetch's acknowledged_at.
func (*Evatra) Acknowledge(context.Context, string) error { return nil }

// credentialsRefused is Evatra refusing the connector's credentials.
func credentialsRefused() error {
	return problem.Unprocessable("integration.secret_unavailable", "Evatra refused the connector's credentials (its API key and email); replace the secret.")
}

// login asks Evatra for a token and keeps it until its expirationDate (an
// expirationDate it cannot read, or one already past, is used for this
// call alone).
func (e *Evatra) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(e.credentials) // #nosec G117 -- the login's own body, to Evatra over TLS alone; a struct of two strings always marshals
	resp, err := e.Client.Post(ctx, e.BaseURL+"/auth/login", http.Header{"Content-Type": {"application/json"}}, body)
	switch {
	case err != nil:
		return "", fmt.Errorf("einvoicing: evatra login: %w", err)
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
		return "", credentialsRefused()
	case resp.Status != http.StatusOK:
		return "", fmt.Errorf("einvoicing: evatra login: answered %d", resp.Status)
	}
	var got struct {
		JWT            string          `json:"jwt"`
		ExpirationDate json.RawMessage `json:"expirationDate"` // its form is not documented: read below
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil || got.JWT == "" {
		return "", fmt.Errorf("einvoicing: evatra login: an answer without a token")
	}
	if expires, ok := rfc3339(got.ExpirationDate); ok {
		e.Sessions.put(e.key, evatraSession{jwt: got.JWT, expires: expires}, e.Now())
	}
	return got.JWT, nil
}

// rfc3339 reads an expirationDate that is an RFC 3339 string; any other
// form (a number, none) is not read, and the token is not kept.
func rfc3339(raw json.RawMessage) (time.Time, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, text)
	return t, err == nil
}

// token is the kept token, or a new one.
func (e *Evatra) token(ctx context.Context) (string, error) {
	if jwt, ok := e.Sessions.get(e.key, e.Now()); ok {
		return jwt, nil
	}
	return e.login(ctx)
}

// call sends one request with the token as R-Auth; answered 401, it logs in
// once again and sends it once more.
func (e *Evatra) call(ctx context.Context, method, path string, body []byte) (egress.Response, error) {
	var resp egress.Response
	for attempt := 0; attempt < 2; attempt++ {
		jwt, err := e.token(ctx)
		if err != nil {
			return egress.Response{}, err
		}
		header := http.Header{"R-Auth": {jwt}}
		if body != nil {
			header.Set("Content-Type", "application/json")
		}
		if resp, err = e.Client.Do(ctx, method, e.BaseURL+path, header, body); err != nil {
			return egress.Response{}, fmt.Errorf("einvoicing: evatra %s: %w", path, err)
		}
		if resp.Status != http.StatusUnauthorized {
			return resp, nil
		}
		e.Sessions.drop(e.key, jwt)
	}
	return resp, nil
}

// searchRequest is POST /invoice/search-documents' body, as the adapter
// sends it.
type searchRequest struct {
	CompanyID    string `json:"companyId"`
	TIN          string `json:"tin"`
	DocumentType string `json:"documentType"`
	StartDate    string `json:"startDate"`
	EndDate      string `json:"endDate"`
	Page         int    `json:"page"`
	Size         int    `json:"size"`
}

// searchAnswer is what the adapter reads of its answer: the documents'
// count and ids (documentId, the name get-document's path gives it, a
// string or a number: its type is not documented).
type searchAnswer struct {
	TotalCount  int `json:"totalCount"`
	InvoiceList []struct {
		DocumentID json.RawMessage `json:"documentId"`
	} `json:"invoiceList"`
}

// documentID is a listed document's id: a non-empty string, or an integer
// as Evatra wrote it; false otherwise.
func documentID(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, text != ""
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&n); err != nil {
		return "", false
	}
	if _, err := n.Int64(); err != nil {
		return "", false
	}
	return n.String(), true
}

// Pending implements Provider: each company's received documents of the
// lookback, page by page, in the order of its company codes; each
// document's receiver is the participant it was listed for. A document
// listed again (pages shifted by a document received meanwhile) is
// listed once, where it was first.
func (e *Evatra) Pending(ctx context.Context) ([]Message, error) {
	codes := make([]string, 0, len(e.Participants))
	for code := range e.Participants {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	var out []Message
	seen := map[string]bool{}
	for _, code := range codes {
		ms, err := e.received(ctx, e.Participants[code])
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// received lists the documents Evatra received for one participant in the
// lookback (its TIN, after the scheme 0235:), each once: a document a
// later page lists again (the pages shifted by one received meanwhile)
// is not counted twice against Evatra's count, so the listing reads on
// to the page holding the document the shift pushed there.
func (e *Evatra) received(ctx context.Context, participant string) ([]Message, error) {
	now := e.Now().UTC()
	req := searchRequest{CompanyID: e.CompanyID, TIN: strings.TrimPrefix(participant, "0235:"), DocumentType: "INCOMING",
		StartDate: now.Add(-EvatraLookback).Format(time.DateOnly), EndDate: now.AddDate(0, 0, 1).Format(time.DateOnly), Size: EvatraPage}
	var out []Message
	seen := map[string]bool{}
	for req.Page = 1; req.Page <= evatraMaxPages; req.Page++ {
		page, err := e.search(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, d := range page.InvoiceList {
			id, ok := documentID(d.DocumentID)
			if !ok {
				return nil, fmt.Errorf("einvoicing: evatra search-documents: a document without its documentId")
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, Message{ID: id, Receiver: participant})
			}
		}
		if len(page.InvoiceList) == 0 || len(out) >= page.TotalCount {
			return out, nil
		}
	}
	return nil, fmt.Errorf("einvoicing: evatra search-documents: more than %d documents received for %s in %v", evatraMaxPages*EvatraPage, participant, EvatraLookback)
}

// search asks for one page of received documents.
func (e *Evatra) search(ctx context.Context, req searchRequest) (searchAnswer, error) {
	body, _ := json.Marshal(req) // a struct of strings and ints always marshals
	resp, err := e.call(ctx, http.MethodPost, "/invoice/search-documents", body)
	if err != nil {
		return searchAnswer{}, err
	}
	if resp.Status != http.StatusOK || resp.Truncated {
		return searchAnswer{}, fmt.Errorf("einvoicing: evatra search-documents: answered %d (truncated %v)", resp.Status, resp.Truncated)
	}
	var page searchAnswer
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return searchAnswer{}, fmt.Errorf("einvoicing: evatra search-documents: %w", err)
	}
	return page, nil
}

// Fetch implements Provider: the document's XML, which Evatra answers in
// Base64 (as text, or as a JSON string); above MaxDocument once decoded it
// is refused as the stand-in's is, and content that is not Base64 is
// refused as unreadable (it is not recorded; the next fetch meets it
// again).
func (e *Evatra) Fetch(ctx context.Context, id string) ([]byte, error) {
	resp, err := e.call(ctx, http.MethodGet, "/invoice/get-document/"+url.PathEscape(id)+"/XML", nil)
	switch {
	case err != nil:
		return nil, err
	case resp.Status != http.StatusOK:
		return nil, fmt.Errorf("einvoicing: evatra get-document %s: answered %d", id, resp.Status)
	case resp.Truncated:
		return nil, tooLarge(id)
	}
	text := bytes.TrimSpace(resp.Body)
	if len(text) > 0 && text[0] == '"' {
		var s string
		if err := json.Unmarshal(text, &s); err != nil {
			return nil, notBase64(id)
		}
		text = []byte(s)
	}
	content, err := base64.StdEncoding.DecodeString(string(text))
	switch {
	case err != nil:
		return nil, notBase64(id)
	case len(content) > MaxDocument:
		return nil, tooLarge(id)
	}
	return content, nil
}

// notBase64 refuses a document Evatra answered in a form that is not
// Base64.
func notBase64(id string) error {
	return problem.Unprocessable("einvoicing.unreadable", "Evatra answered message "+id+" with content that is not Base64.")
}
