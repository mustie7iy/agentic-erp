package web

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A decision's text kept across a step-up sign-in (T73d2). When the API
// answers a decision with the RFC 9470 step-up challenge, the person is
// sent to sign in again and comes back to the request's page (ADR-0052
// §5); what they wrote would be lost on the way. It travels in the
// person's own browser, never in the web store (§6: no business data
// there, and the directory is global, not the tenant's region): sealed
// with the sessions' key (AES-256-GCM, so the browser can neither read nor
// change it), bound to the tenant and the person (a different person
// signing in on the same browser never sees it), for the sign-in's ten
// minutes, in SameSite=Strict, HttpOnly cookies, read once by the
// request's page, which shows it in its form, not sent: the person sends
// it again, one decision one click (§7).

// draftCookie is the prefix of the draft's cookies, draftChunk the most a
// cookie holds of it and draftChunks how many: a comment's 2,000
// characters are at most 8,000 bytes, sealed and encoded under 10,800,
// within three cookies of 3,800 (each under a browser's 4,096 with its
// name and attributes).
const (
	draftCookie = "__Host-erp-draft-"
	draftChunk  = 3800
	draftChunks = 3
)

// draftAAD binds a sealed draft to its cookies, its tenant and its person.
func draftAAD(p *person) []byte {
	return []byte("erp-web draft\x00" + p.TenantID.String() + "\x00" + p.Subject)
}

// draft is a decision not sent: on which request, which decision (a
// ui.Verb…) and what the person wrote.
type draft struct {
	Request uuid.UUID
	Verb    string
	Text    string
}

// keepDraft keeps the draft in the browser for the step-up sign-in the
// answer is about to start, replacing any other.
func (s *Server) keepDraft(w http.ResponseWriter, r *http.Request, p *person, d draft) {
	plain := strings.Join([]string{d.Request.String(), d.Verb, strconv.FormatInt(s.now().Add(signInTTL).Unix(), 10), d.Text}, "\n")
	value := base64.RawURLEncoding.EncodeToString(s.Store.Sealer.Seal(draftAAD(p), []byte(plain)))
	for i := range draftChunks {
		chunk := value[min(i*draftChunk, len(value)):min((i+1)*draftChunk, len(value))]
		if chunk == "" {
			http.SetCookie(w, cookie(draftCookie+strconv.Itoa(i), "", -1, http.SameSiteStrictMode))
			continue
		}
		http.SetCookie(w, cookie(draftCookie+strconv.Itoa(i), chunk, signInTTL, http.SameSiteStrictMode))
	}
	s.Log.InfoContext(r.Context(), "web: a decision's text is kept across a step-up sign-in", "request", d.Request)
}

// keptDraft is what the person wrote on the request before a step-up
// sign-in, in its form's field, and true; used once. A draft for another
// request is left for its own page; one that does not open as this
// person's, or is past its ten minutes, is deleted.
func (s *Server) keptDraft(w http.ResponseWriter, r *http.Request, p *person, id uuid.UUID) (ui.DecideForms, bool) {
	var value strings.Builder
	for i := range draftChunks {
		c, err := r.Cookie(draftCookie + strconv.Itoa(i))
		if err != nil {
			break
		}
		value.WriteString(c.Value)
	}
	if value.Len() == 0 {
		return ui.DecideForms{}, false
	}
	d, ok := s.openDraft(p, value.String())
	if ok && d.Request != id {
		return ui.DecideForms{}, false
	}
	for i := range draftChunks {
		http.SetCookie(w, cookie(draftCookie+strconv.Itoa(i), "", -1, http.SameSiteStrictMode))
	}
	if !ok {
		return ui.DecideForms{}, false
	}
	switch d.Verb {
	case ui.VerbReject:
		return ui.DecideForms{Reason: d.Text}, true
	case ui.VerbWithdraw:
		return ui.DecideForms{Withdrawal: d.Text}, true
	}
	return ui.DecideForms{Comment: d.Text}, true
}

// openDraft opens a kept draft: ok is false for one that is not this
// person's, not whole, or past its time.
func (s *Server) openDraft(p *person, value string) (draft, bool) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return draft{}, false
	}
	plain, err := s.Store.Sealer.Open(draftAAD(p), sealed)
	if err != nil {
		return draft{}, false
	}
	// Sealed by keepDraft alone, so its four parts are always there.
	parts := strings.SplitN(string(plain), "\n", 4)
	request, _ := uuid.Parse(parts[0])             // keepDraft wrote a request's id
	until, _ := strconv.ParseInt(parts[2], 10, 64) // and a Unix time
	if !s.now().Before(time.Unix(until, 0)) {
		return draft{}, false
	}
	return draft{Request: request, Verb: parts[1], Text: parts[3]}, true
}
