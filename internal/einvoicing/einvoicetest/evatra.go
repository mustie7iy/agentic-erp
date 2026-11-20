package einvoicetest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
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
//
// Each call but the login needs a token it issued, unexpired, as R-Auth;
// 401 otherwise. A received document is a message of the store addressed
// to 0235:<tin>; INCOMING lists them all whatever startDate and endDate
// say (the store keeps no dates), OUTGOING none. A token lasts
// TokenLife; Expire makes the issued ones expire now, as Evatra would after
// their expirationDate.
type Evatra struct {
	Store     Store
	Account   EvatraAccount
	TokenLife time.Duration // zero: an hour
	Now       func() time.Time

	mu     sync.Mutex
	tokens map[string]time.Time
	logins int
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

// Handler serves Evatra's calls.
func (e *Evatra) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", e.login)
	mux.Handle("POST /invoice/search-documents", e.authorized(e.search))
	mux.Handle("GET /invoice/get-document/{id}/{format}", e.authorized(e.document))
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
