package einvoicetest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"
)

// EvatraAccount is what the Evatra stand-in accepts: the credentials its
// login takes and the company id its search takes.
type EvatraAccount struct {
	APIKey, Email, CompanyID string
}

// The test account of Server's Evatra stand-in: its API key is the token
// NewServer was given.
const (
	EvatraEmail   = "erp@example.test"
	EvatraCompany = "COMP-1"
)

// Evatra emulates the calls of Evatra's API the adapter makes (ADR-0061 As
// built E1; https://evatra.com/api as the plan read it on 2026-11-19),
// over store:
//
//	POST /auth/login {apiKey, email}              {"jwt": ..., "expirationDate": RFC 3339}; 401 otherwise
//	POST /invoice/search-documents {companyId, tin, documentType, page (from 1), size (≤ 200), ...}
//	                                              {"totalCount": n, "currencyTotals": [], "invoiceList": [{"documentId": ...}]}
//	GET  /invoice/get-document/{id}/XML           the document in Base64
//	POST /invoice/send-document-async {compId, base64Document, invoiceType UAE_PINT, businessType B2B|B2G, invoiceSource ERP}
//	                                              {"trackingId": ..., "status": "PENDING"} (As built E3)
//	GET  /invoice/document-status/{trackingId}    {"trackingId": ..., "status": ...}
//	POST /invoice/document-process/search {companyId, direction OUTGOING, filename, pageNumber (from 0)}
//	                                              {"content": [{"trackingId", "status", "filename"}], "totalElements": n}
//
// Each call but the login needs a token it issued, unexpired, as R-Auth;
// 401 otherwise. A received document is a message of the store addressed
// to 0235:<tin>; INCOMING lists them all whatever startDate and endDate
// say (the store keeps no dates), OUTGOING none. A sent document is kept
// in Outbox under the number its XML names first (cbc:ID), the filename;
// each status read of a submission answers the next status the outbox's
// script gives for that send of the filename (the last one again once
// read), COMPLETED when it gives none. A token lasts
// TokenLife; Expire makes the issued ones expire now, as Evatra would after
// their expirationDate.
type Evatra struct {
	Store     Store
	Account   EvatraAccount
	TokenLife time.Duration // zero: an hour
	Now       func() time.Time
	// Outbox keeps what is sent and scripts its statuses; nil: kept in
	// memory, scripted by Script.
	Outbox Outbox

	mu     sync.Mutex
	tokens map[string]time.Time
	logins int
	sent   []Sent
	reads  map[string]int // tracking id -> status reads answered
	lose   int            // sends still to accept and answer 503, as if the answer were lost
	memory memoryOutbox
}

// Sent is a document Evatra's stand-in was sent: its tracking id, the
// filename its XML names, which send of that filename it is (from 0), the
// request's types and the document.
type Sent struct {
	TrackingID, Filename      string
	Send                      int
	InvoiceType, BusinessType string
	InvoiceSource, CompanyID  string
	Document                  []byte
}

// Outbox keeps the documents Evatra's stand-in is sent and gives each
// send's statuses: Keep records one, Statuses the status reads' answers
// for the send-th send of a filename, in order (none: COMPLETED).
type Outbox interface {
	Keep(s Sent) error
	Statuses(filename string, send int) ([]string, error)
}

// memoryOutbox is the Outbox Script fills.
type memoryOutbox struct {
	scripts map[string][][]string
}

func (m *memoryOutbox) Keep(Sent) error { return nil }

func (m *memoryOutbox) Statuses(filename string, send int) ([]string, error) {
	sends := m.scripts[filename]
	if len(sends) == 0 {
		return nil, nil
	}
	return sends[min(send, len(sends)-1)], nil
}

func (e *Evatra) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// SetTokenLife makes the tokens it issues from now on last d.
func (e *Evatra) SetTokenLife(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.TokenLife = d
}

// Logins is how many tokens it issued.
func (e *Evatra) Logins() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logins
}

// Expire makes every token it issued expire, so the next call is answered
// 401.
func (e *Evatra) Expire() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tokens = nil
}

// Script gives the statuses of the sends of a filename, in memory: the
// first send's status reads answer sends[0] in order, the second's
// sends[1], and so on (the last list again for later sends).
func (e *Evatra) Script(filename string, sends ...[]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.memory.scripts == nil {
		e.memory.scripts = map[string][][]string{}
	}
	e.memory.scripts[filename] = sends
}

// LoseAnswers makes the next n sends be accepted and answered 503, as if
// their answers were lost on the way.
func (e *Evatra) LoseAnswers(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lose = n
}

// Sends are the documents sent so far, in order.
func (e *Evatra) Sends() []Sent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.sent)
}

func (e *Evatra) outbox() Outbox {
	if e.Outbox != nil {
		return e.Outbox
	}
	return &e.memory
}

// Handler serves Evatra's calls.
func (e *Evatra) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", e.login)
	mux.Handle("POST /invoice/search-documents", e.authorized(e.search))
	mux.Handle("GET /invoice/get-document/{id}/{format}", e.authorized(e.document))
	mux.Handle("POST /invoice/send-document-async", e.authorized(e.send))
	mux.Handle("GET /invoice/document-status/{trackingId}", e.authorized(e.status))
	mux.Handle("POST /invoice/document-process/search", e.authorized(e.processes))
	return mux
}

func (e *Evatra) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey string `json:"apiKey"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.APIKey != e.Account.APIKey || in.Email != e.Account.Email {
		http.Error(w, `{"message": "invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails (Go 1.24)
	jwt := hex.EncodeToString(raw)
	e.mu.Lock()
	life := e.TokenLife
	if life == 0 {
		life = time.Hour
	}
	expires := e.now().Add(life)
	if e.tokens == nil {
		e.tokens = map[string]time.Time{}
	}
	e.tokens[jwt] = expires
	e.logins++
	e.mu.Unlock()
	writeJSON(w, map[string]any{"jwt": jwt, "expirationDate": expires.UTC().Format(time.RFC3339)})
}

// authorized serves next to a request bearing a live token as R-Auth.
func (e *Evatra) authorized(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		expires, ok := e.tokens[r.Header.Get("R-Auth")]
		e.mu.Unlock()
		if !ok || !e.now().Before(expires) {
			http.Error(w, `{"message": "unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

func (e *Evatra) search(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CompanyID    string `json:"companyId"`
		TIN          string `json:"tin"`
		DocumentType string `json:"documentType"`
		Page         int    `json:"page"`
		Size         int    `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Page < 1 || in.Size < 1 || in.Size > 200 || in.CompanyID != e.Account.CompanyID {
		http.Error(w, `{"message": "bad request"}`, http.StatusBadRequest)
		return
	}
	ms, err := e.Store.Pending()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type listed struct {
		DocumentID string `json:"documentId"`
	}
	var all []listed
	for _, m := range ms {
		if in.DocumentType == "INCOMING" && m.Receiver == "0235:"+in.TIN {
			all = append(all, listed{DocumentID: m.ID})
		}
	}
	page := []listed{}
	if from := (in.Page - 1) * in.Size; from < len(all) {
		page = all[from:min(from+in.Size, len(all))]
	}
	writeJSON(w, map[string]any{"totalCount": len(all), "currencyTotals": []any{}, "invoiceList": page})
}

func (e *Evatra) document(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("format") != "XML" {
		http.Error(w, `{"message": "unsupported format"}`, http.StatusBadRequest)
		return
	}
	body, ok, err := e.Store.Get(r.PathValue("id"))
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case !ok:
		http.NotFound(w, r)
	default:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(body))) // a client gone gets nothing; the stand-in does not care
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) // as Handler's: a client that went away gets nothing
}

// invoiceID is the first cbc:ID of a UBL document: its number (IBT-001).
var invoiceID = regexp.MustCompile(`<cbc:ID>([^<]+)</cbc:ID>`)

func (e *Evatra) send(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CompID, Base64Document, InvoiceType, BusinessType, InvoiceSource string
	}
	err := json.NewDecoder(r.Body).Decode(&in)
	doc, decodeErr := base64.StdEncoding.DecodeString(in.Base64Document)
	m := invoiceID.FindSubmatch(doc)
	if err != nil || decodeErr != nil || m == nil || in.CompID != e.Account.CompanyID || in.InvoiceType != "UAE_PINT" || in.InvoiceSource != "ERP" ||
		(in.BusinessType != "B2B" && in.BusinessType != "B2G") {
		http.Error(w, `{"message": "bad request"}`, http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	s := Sent{TrackingID: "TRK-" + hex.EncodeToString(randomBytes(6)), Filename: string(m[1]), InvoiceType: in.InvoiceType, BusinessType: in.BusinessType,
		InvoiceSource: in.InvoiceSource, CompanyID: in.CompID, Document: doc}
	for _, earlier := range e.sent {
		if earlier.Filename == s.Filename {
			s.Send++
		}
	}
	lost := e.lose > 0
	if lost {
		e.lose--
	}
	err = e.outbox().Keep(s)
	if err == nil {
		e.sent = append(e.sent, s)
	}
	e.mu.Unlock()
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case lost:
		http.Error(w, "the answer was lost", http.StatusServiceUnavailable)
	default:
		writeJSON(w, map[string]any{"trackingId": s.TrackingID, "status": "PENDING"})
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24)
	return b
}

// current is a submission's status after reads reads: PENDING before the
// first, then the script's in order, its last one again; COMPLETED with no
// script. Called with mu held.
func (e *Evatra) current(s Sent, reads int) (string, error) {
	if reads == 0 {
		return "PENDING", nil
	}
	script, err := e.outbox().Statuses(s.Filename, s.Send)
	if err != nil || len(script) == 0 {
		return "COMPLETED", err
	}
	return script[min(reads, len(script))-1], nil
}

func (e *Evatra) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("trackingId")
	e.mu.Lock()
	i := slices.IndexFunc(e.sent, func(s Sent) bool { return s.TrackingID == id })
	var status string
	var err error
	if i >= 0 {
		if e.reads == nil {
			e.reads = map[string]int{}
		}
		e.reads[id]++
		status, err = e.current(e.sent[i], e.reads[id])
	}
	e.mu.Unlock()
	switch {
	case i < 0:
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, map[string]any{"trackingId": id, "status": status})
	}
}

func (e *Evatra) processes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CompanyID, Direction, Filename string
		PageNumber                     int
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.CompanyID != e.Account.CompanyID || in.Direction != "OUTGOING" || in.PageNumber != 0 {
		http.Error(w, `{"message": "bad request"}`, http.StatusBadRequest)
		return
	}
	type listed struct {
		TrackingID string `json:"trackingId"`
		Status     string `json:"status"`
		Filename   string `json:"filename"`
	}
	content := []listed{}
	e.mu.Lock()
	var err error
	for _, s := range e.sent {
		if s.Filename != in.Filename || err != nil {
			continue
		}
		var status string
		status, err = e.current(s, e.reads[s.TrackingID])
		content = append(content, listed{TrackingID: s.TrackingID, Status: status, Filename: s.Filename})
	}
	e.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"content": content, "totalElements": len(content)})
}
