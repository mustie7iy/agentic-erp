package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
)

// TestSignIn is the sign-in of ADR-0052 §5 step by step: the code flow
// with PKCE (S256), state and nonce, the exact redirect URI, the cookies'
// attributes, a session with a fresh id, the continue page, and the
// person's own token on the API call the home page makes.
func TestSignIn(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.setCookie(sessionCookie, "planted-by-someone") // no fixation: never kept
	start := b.get("/login/" + loginCodeOf + "?return=/")
	if start.status != http.StatusSeeOther {
		t.Fatalf("start: got %d %s", start.status, start.body)
	}
	auth, err := url.Parse(start.location())
	if err != nil {
		t.Fatal(err)
	}
	q := auth.Query()
	for name, want := range map[string]string{"response_type": "code", "client_id": "erp-web", "redirect_uri": s.public + "/callback",
		"scope": "openid erp-api offline_access", "code_challenge_method": "S256"} {
		if got := q.Get(name); got != want {
			t.Errorf("authorization request %s: got %q, want %q", name, got, want)
		}
	}
	if len(q.Get("state")) != 43 || len(q.Get("nonce")) != 43 || len(q.Get("code_challenge")) != 43 || q.Has("acr_values") {
		t.Errorf("authorization request: got %v, want 256-bit state and nonce, a challenge and no acr_values", q)
	}
	checkCookie(t, start.header, signInCookie, http.SameSiteLaxMode, int(signInTTL/time.Second))
	if strings.Contains(strings.Join(start.header.Values("Set-Cookie"), " "), codeCookie) {
		t.Error("start: the login code remembered before the sign-in completed")
	}
	back := b.get(start.location())
	if back.status != http.StatusFound || !strings.HasPrefix(back.location(), s.public+"/callback?") {
		t.Fatalf("provider: got %d to %s, want the callback", back.status, back.location())
	}
	done := b.get(back.location())
	if done.status != http.StatusOK || continueTarget(t, done.body) != "/" {
		t.Fatalf("callback: got %d %s, want the continue page to /", done.status, done.body)
	}
	checkCookie(t, done.header, sessionCookie, http.SameSiteStrictMode, 0)
	checkCookie(t, done.header, codeCookie, http.SameSiteLaxMode, int(codeTTL/time.Second))
	if !strings.Contains(done.header.Values("Set-Cookie")[0], signInCookie+"=;") {
		t.Errorf("callback: got %v, want the sign-in cookie deleted", done.header.Values("Set-Cookie"))
	}
	id := b.cookie(sessionCookie)
	if id == "planted-by-someone" || len(id) != 43 {
		t.Errorf("session id: got %q, want a fresh 256-bit id", id)
	}
	home := b.get("/")
	if home.status != http.StatusOK || !strings.Contains(home.body, "Acme GmbH") || !strings.Contains(home.body, "Sign out") {
		t.Fatalf("home: got %d %s", home.status, home.body)
	}
	if got := s.cellSessions(); !slices.Equal(got, []string{""}) {
		t.Errorf("the cell's sessions: got %v, want one live one, the person's token's", got)
	}
	if strings.Contains(s.logs.String(), id) {
		t.Error("logs: hold the session id")
	}
	if !strings.Contains(s.logs.String(), `msg="web: signed in"`) || !strings.Contains(s.logs.String(), "client=127.0.0.1:") {
		t.Errorf("logs: got %s, want the sign-in with the client's address", s.logs.String())
	}
}

// checkCookie checks a Set-Cookie of the response: Secure, HttpOnly,
// Path=/, no Domain, the SameSite mode and Max-Age (0: a session cookie).
func checkCookie(t *testing.T, h http.Header, name string, sameSite http.SameSite, maxAge int) {
	t.Helper()
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name != name {
			continue
		}
		if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != sameSite || c.MaxAge != maxAge {
			t.Errorf("%s: got %+v, want Secure, HttpOnly, Path=/, no Domain, SameSite %v, Max-Age %d", name, c, sameSite, maxAge)
		}
		return
	}
	t.Errorf("%s: not set (%v)", name, h.Values("Set-Cookie"))
}

// callbackOf runs a sign-in up to the provider's redirect back and
// returns its URL and the sign-in cookie, without completing it.
func callbackOf(t *testing.T, b *browser) (string, string) {
	t.Helper()
	start := b.get("/login/" + loginCodeOf + "?return=/")
	back := b.get(start.location())
	if back.status != http.StatusFound {
		t.Fatalf("provider: got %d %s", back.status, back.body)
	}
	return back.location(), b.cookie(signInCookie)
}

// TestCallbackRefusals: a callback the sign-in's cookie does not match,
// or whose tokens do not prove the sign-in, makes no session.
func TestCallbackRefusals(t *testing.T) {
	s := newStack(t, stackOptions{})
	for name, c := range map[string]struct {
		change func(t *testing.T, u *url.URL, b *browser)
		status int
	}{
		"no sign-in cookie":   {func(_ *testing.T, _ *url.URL, b *browser) { b.setCookie(signInCookie, "") }, 400},
		"a cookie not sealed": {func(_ *testing.T, _ *url.URL, b *browser) { b.setCookie(signInCookie, "AAAA") }, 400},
		"a cookie not base64": {func(_ *testing.T, _ *url.URL, b *browser) { b.setCookie(signInCookie, "%%%") }, 400},
		"another state": {func(t *testing.T, u *url.URL, _ *browser) {
			t.Helper()
			q := u.Query()
			q.Set("state", otherFirst(t, q.Get("state")))
			u.RawQuery = q.Encode()
		}, 400},
		"the provider's error": {func(_ *testing.T, u *url.URL, _ *browser) {
			q := u.Query()
			q.Del("code")
			q.Set("error", "access_denied")
			u.RawQuery = q.Encode()
		}, 400},
		"another issuer (RFC 9207)": {func(_ *testing.T, u *url.URL, _ *browser) {
			q := u.Query()
			q.Set("iss", "https://evil.example.com")
			u.RawQuery = q.Encode()
		}, 400},
		"no iss from a provider that sends it": {func(_ *testing.T, u *url.URL, _ *browser) {
			q := u.Query()
			q.Del("iss")
			u.RawQuery = q.Encode()
		}, 400},
		"another code": {func(_ *testing.T, u *url.URL, _ *browser) {
			q := u.Query()
			q.Set("code", "forged")
			u.RawQuery = q.Encode()
		}, 400},
		"expired": {func(*testing.T, *url.URL, *browser) { s.clock.Advance(signInTTL) }, 400},
	} {
		t.Run(name, func(t *testing.T) {
			b := s.browser()
			raw, _ := callbackOf(t, b)
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			before := s.sessions()
			c.change(t, u, b)
			if r := b.get(u.String()); r.status != c.status || !strings.Contains(r.body, "Sign-in could not be completed") {
				t.Errorf("got %d %s, want %d and the failed sign-in", r.status, r.body, c.status)
			}
			if b.cookie(sessionCookie) != "" || s.sessions() != before {
				t.Error("got a session, want none")
			}
			s.clock.Advance(-time.Until(s.clock.Now()))
		})
	}
	t.Run("a replayed code", func(t *testing.T) {
		b := s.browser()
		raw, sealed := callbackOf(t, b)
		if r := b.get(raw); r.status != http.StatusOK {
			t.Fatalf("first: got %d %s", r.status, r.body)
		}
		replay := s.browser()
		replay.setCookie(signInCookie, sealed)
		if r := replay.get(raw); r.status != http.StatusBadRequest || replay.cookie(sessionCookie) != "" {
			t.Errorf("replay: got %d, want 400 and no session", r.status)
		}
	})
}

// otherFirst is s with its first character changed. The state is random
// base64url, so overwriting it with a fixed character would leave it
// unchanged one time in 64; this always picks a different one.
func otherFirst(t *testing.T, s string) string {
	t.Helper()
	if s == "" {
		t.Fatal("no state to tamper with")
	}
	c := byte('x')
	if s[0] == c {
		c = 'y'
	}
	other := string(c) + s[1:]
	if other == s {
		t.Fatalf("tampered state %q equals the original", s)
	}
	return other
}

// TestOtherFirst: the tamper differs from the original even when the
// original already starts with the character it would write.
func TestOtherFirst(t *testing.T) {
	for _, s := range []string{"xAbc", "yAbc", "AAbc", "x"} {
		t.Run(s, func(t *testing.T) {
			if got := otherFirst(t, s); got == s || got[1:] != s[1:] {
				t.Errorf("got %q, want %q with only its first character changed", got, s)
			}
		})
	}
}

// TestTokenRefusals: tokens that fail any check of OIDC Core §3.1.3.7, or
// an access token not the registration's, make no session.
func TestTokenRefusals(t *testing.T) {
	s := newStack(t, stackOptions{})
	for name, hooks := range map[string]authntest.Hooks{
		"another nonce":                        {IDToken: func(c map[string]any) { c["nonce"] = "other" }},
		"no nonce":                             {IDToken: func(c map[string]any) { delete(c, "nonce") }},
		"another issuer":                       {IDToken: func(c map[string]any) { c["iss"] = "https://evil.example.com" }},
		"another audience":                     {IDToken: func(c map[string]any) { c["aud"] = "other-client" }},
		"several audiences, no azp":            {IDToken: func(c map[string]any) { c["aud"] = []string{"erp-web", "other"} }},
		"expired":                              {IDToken: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		"an access token for another audience": {AccessToken: func(c map[string]any) { c["aud"] = "other-api" }},
		"an access token of another subject":   {AccessToken: func(c map[string]any) { c["sub"] = "mallory" }},
		"an expired access token":              {AccessToken: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
	} {
		t.Run(name, func(t *testing.T) {
			s.is.SetHooks(hooks)
			defer s.is.SetHooks(authntest.Hooks{})
			b := s.browser()
			if r := b.follow("/login/" + loginCodeOf + "?return=/"); r.status != http.StatusBadRequest || b.cookie(sessionCookie) != "" {
				t.Errorf("got %d %s, want 400 and no session", r.status, r.body)
			}
		})
	}
	t.Run("several audiences with azp", func(t *testing.T) {
		s.is.SetHooks(authntest.Hooks{IDToken: func(c map[string]any) { c["aud"], c["azp"] = []string{"erp-web", "other"}, "erp-web" }})
		defer s.is.SetHooks(authntest.Hooks{})
		s.browser().signIn()
	})
	t.Run("a client key the provider does not hold", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		c := s.client
		c.Key = &other.PublicKey
		s.is.RegisterClient(c)
		defer s.is.RegisterClient(s.client)
		if r := s.browser().follow("/login/" + loginCodeOf + "?return=/"); r.status != http.StatusBadRequest {
			t.Errorf("got %d, want 400", r.status)
		}
	})
	t.Run("the provider denies", func(t *testing.T) {
		c := s.client
		c.RedirectURI = s.public + "/elsewhere"
		s.is.RegisterClient(c)
		defer s.is.RegisterClient(s.client)
		if r := s.browser().follow("/login/" + loginCodeOf + "?return=/"); r.status != http.StatusBadRequest {
			t.Errorf("an unregistered redirect URI: got %d, want the provider's 400", r.status)
		}
	})
}

// TestSignInCodes: the login code page, the remembered code (P4), and
// codes no one uses.
func TestSignInCodes(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	if r := b.get("/login?return=/health"); r.status != http.StatusOK || !strings.Contains(r.body, `name="code"`) {
		t.Fatalf("first visit: got %d, want the form", r.status)
	}
	for _, code := range []string{" ACME ", "acme"} {
		want := "/login/acme?return=%2Fhealth"
		// The continue page, not a redirect: the CSP's form-action 'self'
		// would refuse the form's redirect on to the provider.
		if r := b.do(http.MethodPost, "/login", url.Values{"code": {code}, "return": {"/health"}}); r.status != http.StatusOK || continueTarget(t, r.body) != want {
			t.Errorf("post %q: got %d to %s, want %s", code, r.status, continueTarget(t, r.body), want)
		}
	}
	if r := b.do(http.MethodPost, "/login", url.Values{"code": {"a"}}); r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "2 to 32") {
		t.Errorf("an invalid code: got %d %s", r.status, r.body)
	}
	for path, want := range map[string]string{"/login/nobody": "No company uses", "/login/Not_A_Code": "2 to 32"} {
		if r := b.get(path); r.status != http.StatusNotFound || !strings.Contains(r.body, want) {
			t.Errorf("%s: got %d, want 404 with %q", path, r.status, want)
		}
	}
	b.signIn()
	if r := b.get("/login?return=/health"); r.status != http.StatusSeeOther || r.location() != "/login/acme?return=%2Fhealth" {
		t.Errorf("remembered: got %d to %s, want the remembered code", r.status, r.location())
	}
	if r := b.get("/login?other"); r.status != http.StatusOK {
		t.Errorf("another code: got %d, want the form", r.status)
	}
}

// TestSafeReturn: the return path is a same-site path, never a URL (an
// open redirect).
func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/": "/", "/health?x=1": "/health?x=1", "": "/", "//evil.example.com/x": "/", "https://evil.example.com/": "/",
		"/\\evil.example.com": "/", "javascript:alert(1)": "/", "health": "/", "/x\r\nSet-Cookie: a=b": "/", "%zz": "/",
		"/" + strings.Repeat("a", 1024): "/", "/login/evil": "/", "/login": "/", "/callback?code=x": "/", "/logout": "/", "/signed-out": "/",
		"/loginx": "/loginx", "/./login/x": "/", "/a/../login/x": "/", "/%2e/login/x": "/", "/a/./b": "/", "/health/": "/health/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q): got %q, want %q", in, got, want)
		}
	}
}

// TestCrossSiteArrival: a page reached from another site comes without
// the Strict cookie; it gets the continue page, whose own navigation
// carries it, rather than a sign-in it does not need.
func TestCrossSiteArrival(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	r := b.get("/?from=mail", "Sec-Fetch-Site", "cross-site")
	if r.status != http.StatusOK || continueTarget(t, r.body) != "/?from=mail" {
		t.Errorf("cross-site: got %d %s, want the continue page", r.status, r.body)
	}
	if r := b.get("/?from=mail", "Sec-Fetch-Site", "same-origin"); r.status != http.StatusSeeOther || r.location() != "/login?return=%2F%3Ffrom%3Dmail" {
		t.Errorf("same site, signed out: got %d to %s, want sign-in", r.status, r.location())
	}
}

// TestCSRF: a state-changing request needs the session's synchronizer
// token, and CrossOriginProtection refuses another site's whatever it
// carries (ADR-0052 §5).
func TestCSRF(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	token := b.csrf()
	for name, c := range map[string]struct {
		form   url.Values
		header []string
	}{
		"no token":       {url.Values{}, nil},
		"another token":  {url.Values{"csrf_token": {session.NewSecret()}}, nil},
		"another header": {url.Values{}, []string{"X-CSRF-Token", "x"}},
		"another site":   {url.Values{"csrf_token": {token}}, []string{"Sec-Fetch-Site", "cross-site"}},
		"another origin": {url.Values{"csrf_token": {token}}, []string{"Sec-Fetch-Site", "", "Origin", "https://evil.example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			if r := b.do(http.MethodPost, "/logout", c.form, c.header...); r.status != http.StatusForbidden || !strings.Contains(r.body, "This request was refused") {
				t.Errorf("got %d %s, want 403", r.status, r.body)
			}
			if s.sessions() != 1 {
				t.Error("signed out, want the session kept")
			}
		})
	}
	if r := b.do(http.MethodPost, "/logout", url.Values{}, "X-CSRF-Token", token); r.status != http.StatusOK {
		t.Errorf("htmx's header: got %d, want the sign-out", r.status)
	}
}

// TestSafeMethodsChangeNothing walks the routes: GET, HEAD and OPTIONS,
// which CrossOriginProtection lets through from anywhere, never sign out
// or change a session.
func TestSafeMethodsChangeNothing(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	before, err := s.srv.Store.Load(context.Background(), b.cookie(sessionCookie))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/health", "/login", "/signed-out", "/logout", "/no/such/page", s.srv.Assets.CSS} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			b.do(method, path, nil, "Sec-Fetch-Site", "cross-site")
		}
	}
	after, err := s.srv.Store.Load(context.Background(), b.cookie(sessionCookie))
	if err != nil || after.Version != before.Version || after.AccessToken != before.AccessToken || s.sessions() != 1 {
		t.Errorf("after: got %+v, %v, want the session unchanged", after.Version, err)
	}
	if r := b.get("/logout"); r.status != http.StatusNotFound {
		t.Errorf("GET /logout: got %d, want no such page", r.status)
	}
}

// TestSignOut: the API's session ends (G9), then erp-web's, then the
// provider's (RP-initiated logout), which returns to the signed-out page.
func TestSignOut(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	id := b.cookie(sessionCookie)
	r := b.do(http.MethodPost, "/logout", url.Values{"csrf_token": {b.csrf()}})
	next := continueTarget(t, r.body)
	if r.status != http.StatusOK || !strings.HasPrefix(next, s.is.URL+authntest.EndSessionPath+"?") {
		t.Fatalf("sign-out: got %d to %s, want the continue page to the provider's logout", r.status, next)
	}
	if done := b.follow(next); done.status != http.StatusOK || !strings.Contains(done.body, "You are signed out") {
		t.Errorf("back: got %d %s", done.status, done.body)
	}
	logouts := s.is.Logouts()
	if len(logouts) != 1 || logouts[0].Get("id_token_hint") == "" || logouts[0].Get("post_logout_redirect_uri") != s.public+"/signed-out" || logouts[0].Get("client_id") != "erp-web" {
		t.Errorf("logouts: got %v", logouts)
	}
	if got := s.cellSessions(); !slices.Equal(got, []string{"revoked"}) {
		t.Errorf("the cell's sessions: got %v, want it revoked", got)
	}
	if s.sessions() != 0 || b.cookie(sessionCookie) != "" {
		t.Errorf("erp-web's session: got %d stored, cookie %q, want none", s.sessions(), b.cookie(sessionCookie))
	}
	again := s.browser()
	again.setCookie(sessionCookie, id)
	if r := again.get("/"); r.status != http.StatusSeeOther {
		t.Errorf("the old cookie: got %d, want sign-in", r.status)
	}
}

// TestThePersonsAddressInTheAudit: the API's audit trail records the
// signed-in person's address as erp-web's load balancer named it, not
// erp-web's own (ADR-0011 as amended by T73c): ending the session at
// sign-out is audited with it.
func TestThePersonsAddressInTheAudit(t *testing.T) {
	s := newStack(t, stackOptions{remote: "198.51.100.23:50211"})
	b := s.browser()
	b.signIn()
	b.do(http.MethodPost, "/logout", url.Values{"csrf_token": {b.csrf()}})
	var got []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(host(client_address)), '{}') FROM audit.audit_event WHERE tenant_id = $1 AND action = 'identity.session.end'`,
		[]any{s.ten.ID}, &got)
	if !slices.Equal(got, []string{"198.51.100.23"}) {
		t.Errorf("the session's end in the audit: got addresses %v, want the person's 198.51.100.23", got)
	}
}

// TestSignOutWithoutTheProvidersLogout: a provider without an end
// session endpoint, or an API that cannot be reached, still signs the
// person out here.
func TestSignOutWithoutTheProvidersLogout(t *testing.T) {
	doc := []byte(`{"issuer": "ISSUER", "jwks_uri": "ISSUER/keys", "authorization_endpoint": "ISSUER/auth", "token_endpoint": "ISSUER/token"}`)
	s := newStack(t, stackOptions{discovery: doc})
	b := s.browser()
	b.signIn()
	token := b.csrf()
	s.api.Close()
	if r := b.do(http.MethodPost, "/logout", url.Values{"csrf_token": {token}}); r.status != http.StatusSeeOther || r.location() != "/signed-out" {
		t.Errorf("got %d to %s, want the signed-out page", r.status, r.location())
	}
	if s.sessions() != 0 || !strings.Contains(s.logs.String(), "the API's session could not be ended") {
		t.Errorf("got %d sessions, logs %s", s.sessions(), s.logs.String())
	}
}

// TestSessionExpiry: erp-web's own idle timeout and lifetime end a
// session, and so does an access token that expires without a refresh
// token: the person signs in again, coming back to the page.
func TestSessionExpiry(t *testing.T) {
	s := newStack(t, stackOptions{})
	for name, c := range map[string]struct {
		refresh bool
		wait    []time.Duration
	}{
		"idle":                          {true, []time.Duration{session.DefaultIdle}},
		"past the lifetime":             {true, []time.Duration{7 * time.Hour, 7 * time.Hour, 7 * time.Hour, 7 * time.Hour}},
		"a token without refresh token": {false, []time.Duration{time.Hour}},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := s.client
			c2.Refresh = c.refresh
			s.is.RegisterClient(c2)
			defer s.is.RegisterClient(s.client)
			b := s.browser()
			b.signIn()
			var r response
			for _, d := range c.wait {
				s.clock.Advance(d)
				r = b.get("/")
			}
			if r.status != http.StatusSeeOther || r.location() != "/login?return=%2F" {
				t.Errorf("got %d to %s, want sign-in and back", r.status, r.location())
			}
			s.clock.Advance(-time.Until(s.clock.Now()))
		})
	}
}

// TestRefresh: an access token about to expire is renewed with the
// refresh token, once for all concurrent requests (the provider rotates
// refresh tokens, so the second use would be refused); a refresh the
// provider refuses ends the session.
func TestRefresh(t *testing.T) {
	s := newStack(t, stackOptions{})
	s.is.SetTokenTTL(2 * time.Minute)
	b := s.browser()
	b.signIn()
	s.is.SetTokenTTL(time.Hour)
	s.clock.Advance(2 * time.Minute)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() { codes[i] = b.get("/").status })
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i, code)
		}
	}
	if n := s.is.TokenRequests("refresh_token"); n != 1 {
		t.Errorf("refreshes: got %d, want 1", n)
	}
	t.Run("refused", func(t *testing.T) {
		s.is.SetTokenTTL(2 * time.Minute)
		b := s.browser()
		b.signIn()
		s.is.SetTokenTTL(time.Hour)
		s.clock.Advance(4 * time.Minute)
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		c := s.client
		c.Key = &other.PublicKey
		s.is.RegisterClient(c)
		defer s.is.RegisterClient(s.client)
		if r := b.get("/"); r.status != http.StatusSeeOther || !strings.HasPrefix(r.location(), "/login") {
			t.Errorf("got %d to %s, want sign-in", r.status, r.location())
		}
	})
}

// TestTheAPIsAnswerDecides: the API's 401 for an ended session ends this
// one too; its RFC 9470 step-up challenge signs the person in again with
// the acr it asks for and comes back (ADR-0052 §5).
func TestTheAPIsAnswerDecides(t *testing.T) {
	t.Run("an ended session", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		b := s.browser()
		b.signIn()
		if r := b.get("/"); r.status != http.StatusOK {
			t.Fatalf("home: got %d", r.status)
		}
		s.clock.Advance(time.Hour + time.Minute) // past the cell's idle timeout, within erp-web's
		if r := b.get("/"); r.status != http.StatusSeeOther || r.location() != "/login?return=%2F" {
			t.Errorf("got %d to %s, want sign-in", r.status, r.location())
		}
		if s.sessions() != 0 {
			t.Errorf("sessions: got %d, want it ended", s.sessions())
		}
	})
	t.Run("step-up", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		s.reg.set(func(r *authn.Registration) { r.MFARequired, r.ACRValues = true, []string{"urn:erp:mfa"} })
		s.is.SignIn(authntest.User{Tenant: s.ten.ID, Subject: s.ten.Human.Subject, StepUp: map[string]any{"acr": "urn:erp:mfa"}})
		b := s.browser()
		b.signIn()
		r := b.get("/")
		if r.status != http.StatusSeeOther || !strings.Contains(r.location(), "acr=urn%3Aerp%3Amfa") {
			t.Fatalf("challenged: got %d to %s, want a step-up sign-in", r.status, r.location())
		}
		if done := b.follow(r.location()); done.status != http.StatusOK || continueTarget(t, done.body) != "/" {
			t.Fatalf("step-up: got %d %s", done.status, done.body)
		}
		if r := b.get("/"); r.status != http.StatusOK {
			t.Errorf("after the step-up: got %d %s", r.status, r.body)
		}
		if s.sessions() != 1 {
			t.Errorf("sessions: got %d, want the step-up's alone", s.sessions())
		}
	})
	t.Run("a step-up the provider does not prove", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		s.reg.set(func(r *authn.Registration) { r.MFARequired, r.ACRValues = true, []string{"urn:erp:mfa"} })
		b := s.browser()
		b.signIn()
		if r := b.follow("/"); r.status != http.StatusUnauthorized || !strings.Contains(r.body, "second factor") {
			t.Errorf("got %d %s, want the step-up refused", r.status, r.body)
		}
	})
	t.Run("a challenge again right after a step-up", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		s.reg.set(func(r *authn.Registration) { r.MFARequired, r.AMRValues = true, []string{"mfa"} })
		b := s.browser()
		b.signIn()
		r := b.get("/")
		if r.status != http.StatusSeeOther || !strings.Contains(r.location(), "max_age=0") {
			t.Fatalf("challenged: got %d to %s, want a fresh sign-in (max_age=0)", r.status, r.location())
		}
		b.follow(r.location())
		if r := b.get("/"); r.status != http.StatusUnauthorized || !strings.Contains(r.body, "second factor") {
			t.Errorf("again: got %d %s, want it said rather than looped", r.status, r.body)
		}
	})
}

// TestClientSecretBasic: a provider that offers only a client secret.
func TestClientSecretBasic(t *testing.T) {
	s := newStack(t, stackOptions{method: "client_secret_basic"})
	b := s.browser()
	b.signIn()
	if r := b.get("/"); r.status != http.StatusOK {
		t.Errorf("home: got %d", r.status)
	}
}

// TestSignInUnavailable: a provider that cannot be reached, a credential
// outside erp-web's prefix, and a store that fails are 503s, never a
// session.
func TestSignInUnavailable(t *testing.T) {
	t.Run("the provider", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		s.is.FailNext(20)
		if r := s.browser().get("/login/" + loginCodeOf); r.status != http.StatusServiceUnavailable {
			t.Errorf("got %d, want 503", r.status)
		}
	})
	t.Run("a credential outside the prefix", func(t *testing.T) {
		s := newStack(t, stackOptions{credential: "file:" + filepath.Join(t.TempDir(), "elsewhere.pem")})
		if r := s.browser().follow("/login/" + loginCodeOf + "?return=/"); r.status != http.StatusServiceUnavailable {
			t.Errorf("got %d, want 503", r.status)
		}
	})
	t.Run("the store", func(t *testing.T) {
		s := newStack(t, stackOptions{})
		signedIn := s.browser()
		signedIn.signIn()
		b := s.browser()
		raw, _ := callbackOf(t, b)
		pool := s.srv.Store.Pool
		s.srv.Store.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
				return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1})
			})
		}
		if r := b.get(raw); r.status != http.StatusServiceUnavailable {
			t.Errorf("callback: got %d, want 503", r.status)
		}
		if r := signedIn.get("/"); r.status != http.StatusServiceUnavailable {
			t.Errorf("a page: got %d, want 503", r.status)
		}
	})
}

// deleteFails is a transaction whose deletions of sessions fail.
type deleteFails struct{ pgx.Tx }

func (d deleteFails) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(strings.TrimSpace(sql), "DELETE FROM web.session") {
		return pgconn.CommandTag{}, pgtest.ErrInjected
	}
	return d.Tx.Exec(ctx, sql, args...)
}

// failDeletes makes the store's deletions fail.
func (s *stack) failDeletes() {
	pool := s.srv.Store.Pool
	s.srv.Store.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, deleteFails{tx}) })
	}
}

// TestDeletionsThatFail: a session that cannot be deleted is reported:
// at sign-out the person is told (the session would otherwise live on);
// elsewhere it is logged, and the sweep deletes it.
func TestDeletionsThatFail(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	token := b.csrf()
	s.failDeletes()
	if r := b.do(http.MethodPost, "/logout", url.Values{"csrf_token": {token}}); r.status != http.StatusServiceUnavailable {
		t.Errorf("sign-out: got %d, want 503", r.status)
	}
	s.clock.Advance(time.Hour + time.Minute) // the cell's session idles out
	if r := b.get("/"); r.status != http.StatusSeeOther || !strings.Contains(s.logs.String(), "the sweep deletes it") {
		t.Errorf("an ended session: got %d, want sign-in and the failure logged", r.status)
	}
	s.srv.Store.InTx = nil
	t.Run("an expired token without refresh", func(t *testing.T) {
		c := s.client
		c.Refresh = false
		s.is.RegisterClient(c)
		defer s.is.RegisterClient(s.client)
		b := s.browser()
		b.signIn()
		s.failDeletes()
		defer func() { s.srv.Store.InTx = nil }()
		s.clock.Advance(time.Hour)
		if r := b.get("/"); r.status != http.StatusServiceUnavailable {
			t.Errorf("got %d, want 503", r.status)
		}
	})
	t.Run("a step-up's previous session", func(t *testing.T) {
		s.clock.Advance(-time.Until(s.clock.Now())) // the provider's auth_time is now
		b := s.browser()
		b.signIn()
		s.failDeletes()
		defer func() { s.srv.Store.InTx = nil }()
		if r := b.follow("/login/" + loginCodeOf + "?return=/&max_age=0"); r.status != http.StatusOK || !strings.Contains(s.logs.String(), "a sign-in replaces was not deleted") {
			t.Errorf("got %d, want the step-up done and the failure logged", r.status)
		}
	})
}

// stubAPI answers every API call with status, a problem with code, and
// the challenge, if any.
func stubAPI(t *testing.T, status int, code, challenge string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"t","status":` + strconv.Itoa(status) + `,"code":"` + code + `","detail":"d","trace_id":"x"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTheAPIsOtherAnswers: a person who may see no company sees that, not
// an error (§12); an API that cannot be reached, or limits the rate, is a
// 503; a step-up challenge with max_age signs in again with it; a sign-out
// the API refuses to end its session for goes on.
func TestTheAPIsOtherAnswers(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	token := b.csrf()
	real := s.srv.API
	use := func(srv *httptest.Server) {
		api, err := NewAPI(srv.URL, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		s.srv.API = api
	}
	defer func() { s.srv.API = real }()
	use(stubAPI(t, http.StatusForbidden, "identity.permission_missing", ""))
	if r := b.get("/"); r.status != http.StatusOK || !strings.Contains(r.body, "no company to work in") {
		t.Errorf("forbidden: got %d %s", r.status, r.body)
	}
	use(stubAPI(t, http.StatusTooManyRequests, "platform.rate_limited", ""))
	if r := b.get("/"); r.status != http.StatusServiceUnavailable {
		t.Errorf("rate limited: got %d, want 503", r.status)
	}
	down := stubAPI(t, 200, "", "")
	down.Close()
	use(down)
	if r := b.get("/"); r.status != http.StatusServiceUnavailable {
		t.Errorf("down: got %d, want 503", r.status)
	}
	use(stubAPI(t, http.StatusUnauthorized, "identity.mfa_required", `Bearer error="insufficient_user_authentication", max_age=300`))
	if r := b.get("/"); r.status != http.StatusSeeOther || !strings.Contains(r.location(), "max_age=300") {
		t.Errorf("step-up with max_age: got %d to %s", r.status, r.location())
	}
	use(stubAPI(t, http.StatusForbidden, "identity.permission_missing", ""))
	if r := b.do(http.MethodPost, "/logout", url.Values{"csrf_token": {token}}); r.status != http.StatusOK || !strings.Contains(s.logs.String(), "did not end its session") {
		t.Errorf("sign-out: got %d, want it done and the refusal logged", r.status)
	}
}

// TestSweeper deletes ended sessions in rounds.
func TestSweeper(t *testing.T) {
	s := newStack(t, stackOptions{})
	s.browser().signIn()
	s.clock.Advance(session.DefaultIdle)
	if again, err := s.srv.sweep(context.Background()); again || err != nil || s.sessions() != 0 {
		t.Errorf("sweep: got %v, %v, %d sessions, want none left", again, err, s.sessions())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.srv.RunSweeper(ctx) // one round, then the cancelled context ends it
}

// TestRenewalsThatFail: a session whose web client was removed, or whose
// renewed token is not the registration's, ends; one whose store fails
// meanwhile is a 503.
func TestRenewalsThatFail(t *testing.T) {
	for name, c := range map[string]struct {
		change func(s *stack)
		status int
	}{
		"the web client removed": {func(s *stack) {
			if err := s.dir.Pool.InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE directory.issuer SET login_code = NULL, client_id = NULL, auth_method = NULL, credential_ref = NULL, scope = NULL`)
				return err
			}); err != nil {
				s.t.Fatal(err)
			}
			s.clock.Advance(session.CacheTTL)
		}, http.StatusSeeOther},
		"a renewed token of another audience": {func(s *stack) {
			s.is.SetHooks(authntest.Hooks{AccessToken: func(c map[string]any) { c["aud"] = "other" }})
		}, http.StatusSeeOther},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t, stackOptions{})
			s.is.SetTokenTTL(2 * time.Minute)
			b := s.browser()
			b.signIn()
			s.is.SetTokenTTL(time.Hour)
			s.clock.Advance(2 * time.Minute)
			c.change(s)
			if r := b.get("/"); r.status != c.status {
				t.Errorf("got %d, want %d", r.status, c.status)
			}
		})
	}
}

// TestSignInStartFailures: the store failing to find the code's client.
func TestSignInStartFailures(t *testing.T) {
	s := newStack(t, stackOptions{})
	pool := s.srv.Store.Pool
	s.srv.Store.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}) })
	}
	if r := s.browser().get("/login/" + loginCodeOf); r.status != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", r.status)
	}
}

// TestCallbackWithoutTheClient: a sign-in whose code's client went away
// meanwhile, or cannot be read, completes no session.
func TestCallbackWithoutTheClient(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	raw, _ := callbackOf(t, b)
	cookie := b.cookie(signInCookie)
	if err := s.dir.ReleaseIssuer(context.Background(), directoryIssuer(s)); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(session.CacheTTL)
	if r := b.get(raw); r.status != http.StatusBadRequest {
		t.Errorf("gone: got %d, want 400", r.status)
	}
	b.setCookie(signInCookie, cookie)
	pool := s.srv.Store.Pool
	s.srv.Store.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}) })
	}
	s.clock.Advance(session.CacheTTL)
	if r := b.get(raw); r.status != http.StatusServiceUnavailable {
		t.Errorf("unreadable: got %d, want 503", r.status)
	}
}

// directoryIssuer is the stack's claim, for its release.
func directoryIssuer(s *stack) directory.Issuer {
	var is directory.Issuer
	if err := s.dir.Pool.InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT issuer, audience, provider_id FROM directory.issuer`).Scan(&is.Issuer, &is.Audience, &is.ProviderID)
	}); err != nil {
		s.t.Fatal(err)
	}
	return is
}

// TestALoginCodeReclaimed: a login code freed by one tenant's provider and
// claimed by another's never receives the first's sessions' tokens: the
// renewal, the sign-out and a sign-in under way end rather than call the
// other provider.
func TestALoginCodeReclaimed(t *testing.T) {
	s := newStack(t, stackOptions{})
	s.is.SetTokenTTL(2 * time.Minute)
	b := s.browser()
	b.signIn()
	s.is.SetTokenTTL(time.Hour)
	pending := s.browser()
	raw, _ := callbackOf(t, pending)
	other := authntest.NewSeparate(t)
	is := directoryIssuer(s)
	if err := s.dir.ReleaseIssuer(context.Background(), is); err != nil {
		t.Fatal(err)
	}
	taken := directory.Issuer{Issuer: other.URL, Audience: authntest.Audience, TenantID: uuid.New(), Cell: "us-1", ProviderID: uuid.New(),
		Web: &directory.WebClient{LoginCode: loginCodeOf, ClientID: "erp-web", AuthMethod: "client_secret_basic", CredentialRef: "env:X", Scope: "openid"}}
	if err := s.dir.ClaimIssuer(context.Background(), taken); err != nil {
		t.Fatal(err)
	}
	if err := s.dir.ClaimWebClient(context.Background(), taken); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(session.CacheTTL + 2*time.Minute)
	if r := pending.get(raw); r.status != http.StatusBadRequest {
		t.Errorf("the sign-in under way: got %d, want 400", r.status)
	}
	if r := b.get("/"); r.status != http.StatusSeeOther {
		t.Errorf("the renewal: got %d, want the session ended", r.status)
	}
	if n := other.TokenRequests("refresh_token") + other.TokenRequests("authorization_code"); n != 0 || len(other.Logouts()) != 0 {
		t.Errorf("the other provider: got %d token requests and %d logouts, want none", n, len(other.Logouts()))
	}
}

// TestSignInOverASession: a sign-in replaces the session the browser had,
// whatever the kind, and a refreshed ID token that is not the provider's
// for this person ends the session.
func TestSignInOverASession(t *testing.T) {
	s := newStack(t, stackOptions{})
	b := s.browser()
	b.signIn()
	b.signIn()
	if n := s.sessions(); n != 1 {
		t.Errorf("sessions: got %d, want the new one alone", n)
	}
	defer s.is.SetHooks(authntest.Hooks{})
	for name, hook := range map[string]func(map[string]any){
		"of another subject": func(claims map[string]any) { claims["sub"] = "mallory" },
		"for another client": func(claims map[string]any) { claims["aud"] = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			s.is.SetTokenTTL(2 * time.Minute)
			s.is.SetHooks(authntest.Hooks{})
			c := s.browser()
			c.signIn()
			s.is.SetTokenTTL(time.Hour)
			s.is.SetHooks(authntest.Hooks{IDToken: hook})
			s.clock.Advance(2 * time.Minute)
			if r := c.get("/"); r.status != http.StatusSeeOther {
				t.Errorf("a refreshed ID token %s: got %d, want the session ended", name, r.status)
			}
		})
	}
}
