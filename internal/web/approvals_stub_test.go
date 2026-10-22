package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The answers the real API gives only in states hard to reach from a
// test (a page the owning context thinned, a server error, a rate limit),
// from a stub: the stack's sign-in is real, its API client is pointed at
// the stub.

// stubbed answers each call by "METHOD /path" (the longest prefix that
// matches) and records the query of each.
type stubbed struct {
	mu      sync.Mutex
	queries []url.Values
	routes  map[string]http.HandlerFunc
}

func (st *stubbed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.queries = append(st.queries, r.URL.Query())
	st.mu.Unlock()
	key, best := r.Method+" "+r.URL.Path, ""
	for k := range st.routes {
		if strings.HasPrefix(key, k) && len(k) > len(best) {
			best = k
		}
	}
	h, ok := st.routes[best]
	if !ok {
		http.Error(w, "no stub for "+key, http.StatusTeapot)
		return
	}
	h(w, r)
}

// answer writes a JSON answer.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ct := "application/json"
		if status >= 400 {
			ct = "application/problem+json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's own stub
	}
}

func problemJSON(status int, code string) string {
	return fmt.Sprintf(`{"type":"about:blank","title":"Stubbed","status":%d,"code":%q}`, status, code)
}

func meJSON(permissions ...string) string {
	return fmt.Sprintf(`{"id":%q,"tenant_id":%q,"kind":"human","display_name":"x","email":null,"all_companies":true,"legal_entity_ids":[],
		"permissions":[%s],"barred_permissions":[],"autonomy_level":null}`, uuid.NewString(), uuid.NewString(), quoted(permissions))
}

func quoted(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ",")
}

// requestJSON is a request of kinds, a status and a decision the
// workspace does not know (open enumerations).
func requestJSON(id uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"object_type":"new_kind","object_id":%q,"title":"Stubbed request","requester_id":%q,"status":"on_hold","current_step":1,
		"steps":[{"step_no":1,"role_id":%q,"required":1,"approvals":0}],
		"decisions":[{"id":%q,"step_no":1,"actor_id":%q,"decision":"escalate","decided_at":"2026-10-22T11:00:00Z"}]}`,
		id, u(), u(), u(), u(), u(), u())
}

func pageJSON(next string, ids ...uuid.UUID) string {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = requestJSON(id)
	}
	if next != "" {
		return fmt.Sprintf(`{"items":[%s],"next_page_token":%q}`, strings.Join(items, ","), next)
	}
	return fmt.Sprintf(`{"items":[%s]}`, strings.Join(items, ","))
}

// stubStack is a signed-in browser whose workspace calls st, and the
// browser's CSRF token.
func stubStack(t *testing.T, st *stubbed) (*stack, *browser, string) {
	t.Helper()
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	token := b.csrf()
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	api, err := NewAPI(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	s.srv.API = api
	return s, b, token
}

// TestInboxReadsOn: a page of the inbox the owning context thinned (fewer
// requests than asked, with a next page) is read on, up to three pages;
// the next page's link carries the API's token; names that cannot be read
// are said to be unknown; values the workspace does not know are shown as
// they are.
func TestInboxReadsOn(t *testing.T) {
	r1, r2 := uuid.New(), uuid.New()
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                 answer(200, meJSON(permReadApprovals)),
		"GET /v1/principal-names":    answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/approval-requests/": answer(200, requestJSON(r1)),
		"GET /v1/approval-requests": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page_token") {
			case "":
				answer(200, pageJSON("t2", r1))(w, r)
			case "t2":
				answer(200, pageJSON("t3", r2))(w, r)
			default:
				answer(200, pageJSON("t4"))(w, r)
			}
		},
	}}
	s, b, _ := stubStack(t, st)
	r := b.get("/approvals")
	if r.status != http.StatusOK {
		t.Fatalf("inbox: got %d %s", r.status, r.body)
	}
	p := r.page(t)
	if got := len(p.all("li", "card")); got != 2 {
		t.Errorf("cards: got %d, want 2 from three pages", got)
	}
	if got := p.mainText(); !strings.Contains(got, "Unknown") || !strings.Contains(got, "new_kind") {
		t.Errorf("inbox: got %q, want the unknown name and kind as they are", got)
	}
	more := ""
	for _, a := range p.all("a", "button") {
		more = attrOf(a, "href")
	}
	if more != "/approvals?page=t4" {
		t.Errorf("next page: got %q", more)
	}
	if !strings.Contains(s.logs.String(), "principals' names could not be read") {
		t.Error("the names' failure is not logged")
	}
	st.queries = nil
	b.get(more)
	if got := st.queries[1].Get("page_token"); got != "t4" {
		t.Errorf("the next page: asked with token %q, want t4", got)
	}
	detail := b.get(requestPath(r1)).page(t).mainText()
	for _, want := range []string{"on_hold", "escalate", "new_kind", "Nothing is left to decide"} {
		if !strings.Contains(detail, want) {
			t.Errorf("request: no %q in %q", want, detail)
		}
	}
}

// TestApprovalsUnavailable: an API that cannot answer, limits the rate or
// fails makes the page unavailable, never a half page; its 401 signs the
// person in again, back to the request a decision was sent from.
func TestApprovalsUnavailable(t *testing.T) {
	id := uuid.New()
	me := answer(200, meJSON(permReadApprovals, permDecide))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	abort := func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }
	form := func(token string) url.Values {
		return url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}, "comment": {"ok"}}
	}
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"home: the companies unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/legal-entities": answer(503, problemJSON(503, "platform.unavailable"))},
			http.MethodGet, "/", http.StatusServiceUnavailable, ""},
		"inbox: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/approvals", http.StatusSeeOther, "return=%2Fapprovals"},
		"inbox: rate limited": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/approval-requests": answer(429, problemJSON(429, "request.rate_limited"))},
			http.MethodGet, "/approvals", http.StatusServiceUnavailable, ""},
		"request: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, requestPath(id), http.StatusSeeOther, "return=%2Fapprovals%2F" + id.String()},
		"request: unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/approval-requests/": answer(503, problemJSON(503, "platform.unavailable"))},
			http.MethodGet, requestPath(id), http.StatusServiceUnavailable, ""},
		"decision: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, requestPath(id) + "/approve", http.StatusSeeOther,
			"return=%2Fapprovals%2F" + id.String()},
		"decision: rate limited": {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/approval-requests/": answer(429, problemJSON(429, "request.rate_limited"))},
			http.MethodPost, requestPath(id) + "/reject", http.StatusServiceUnavailable, ""},
		"decision: the API gone": {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/approval-requests/": abort},
			http.MethodPost, requestPath(id) + "/approve", http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(tc.path)
			} else {
				r = b.do(tc.method, tc.path, form(token))
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// TestProblemOf: only an answer holding a problem as its Response is one.
func TestProblemOf(t *testing.T) {
	for name, tc := range map[string]struct {
		res  any
		want bool
	}{
		"a refusal":           {&gen.ForbiddenHeaders{Response: gen.Problem{Code: "x.y"}}, true},
		"a value":             {gen.ConflictHeaders{Response: gen.Problem{Code: "x.y"}}, true},
		"nothing":             {nil, false},
		"another response":    {&gen.ApprovalRequestHeaders{}, false},
		"no response at all":  {&struct{ X int }{}, false},
		"not a struct":        {"text", false},
		"a refusal of a list": {&gen.BadRequestHeaders{Response: gen.Problem{Code: "x.y"}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			p, ok := problemOf(tc.res)
			if ok != tc.want || (ok && p.Code != "x.y") {
				t.Errorf("got %v %v, want %v", p, ok, tc.want)
			}
		})
	}
}

// TestCommentError: a reason is needed to reject, and a comment has at
// most 2,000 characters (not bytes).
func TestCommentError(t *testing.T) {
	for name, tc := range map[string]struct {
		text     string
		required bool
		want     i18n.Key
	}{
		"no comment to approve": {"", false, ""},
		"no reason to reject":   {" \n", true, i18n.DecideReasonNeeded},
		"a reason":              {"Duplicate", true, ""},
		"2,000 characters":      {strings.Repeat("é", 2000), false, ""},
		"2,001 characters":      {strings.Repeat("x", 2001), true, i18n.DecideTooLong},
	} {
		t.Run(name, func(t *testing.T) {
			if got := commentError(tc.text, tc.required); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNameCache: names last five minutes per session; a full cache drops
// the expired sessions, and empties when none has expired.
func TestNameCache(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	id := uuid.New()
	c := &nameCache{max: 2}
	if got := c.get("a", now); len(got) != 0 {
		t.Errorf("empty: got %v", got)
	}
	c.put("a", now, map[uuid.UUID]ui.Name{id: {Text: "Ann"}})
	c.put("a", now.Add(time.Minute), map[uuid.UUID]ui.Name{uuid.New(): {Text: "Bo"}})
	if got := c.get("a", now.Add(4*time.Minute)); len(got) != 2 || got[id].Text != "Ann" {
		t.Errorf("within five minutes: got %v", got)
	}
	if got := c.get("a", now.Add(nameTTL)); len(got) != 0 {
		t.Errorf("after five minutes: got %v, want none", got)
	}
	if got := c.get("b", now); len(got) != 0 {
		t.Errorf("another session: got %v, want none of a's", got)
	}
	c.put("b", now, nil) // full: a and b
	c.put("c", now.Add(nameTTL), map[uuid.UUID]ui.Name{id: {Text: "Ann"}})
	if _, ok := c.sessions["c"]; !ok || len(c.sessions) != 1 {
		t.Errorf("a full cache: got %v, want the expired sessions dropped for c", c.sessions)
	}
	c.put("d", now.Add(nameTTL), nil)
	c.put("e", now.Add(nameTTL), nil) // full of live sessions: emptied, then e
	if _, ok := c.sessions["e"]; !ok || len(c.sessions) != 1 {
		t.Errorf("a full cache of live sessions: got %v, want it emptied for e", c.sessions)
	}
	c.put("e", now.Add(2*nameTTL), map[uuid.UUID]ui.Name{id: {Text: "Ann"}}) // e expired: started again
	if got := c.get("e", now.Add(2*nameTTL)); got[id].Text != "Ann" {
		t.Errorf("an expired session's names again: got %v", got)
	}
	var def nameCache // the default limit
	def.put("x", now, nil)
	if len(def.sessions) != 1 {
		t.Errorf("the default cache: got %v", def.sessions)
	}
}
