package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Sign-in through the backend-for-frontend (ADR-0052 §5, P2, P4): the
// browser holds a session cookie and nothing else; the tokens stay in the
// session store. The cookies follow RFC 10017 §6.1.3.2:
//   - the session's, __Host-Http-erp-session: Secure, HttpOnly, Path=/, no
//     Domain, SameSite=Strict, a random 256-bit id, for the browser's
//     session;
//   - the sign-in's, __Host-erp-sign-in: its state, nonce, PKCE verifier
//     and return path, sealed, SameSite=Lax since the provider's redirect
//     back is a cross-site navigation, ten minutes, used once;
//   - the remembered login code, __Host-erp-login-code, so the next visit
//     skips the code (P4); not a secret.
const (
	sessionCookie = "__Host-Http-erp-session"
	signInCookie  = "__Host-erp-sign-in"
	codeCookie    = "__Host-erp-login-code"
	signInTTL     = 10 * time.Minute
	codeTTL       = 365 * 24 * time.Hour
	// refreshAhead renews an access token this long before it expires,
	// so no API call carries one that expires on the way.
	refreshAhead = 30 * time.Second
	// stepUpGrace: a step-up challenge this soon after a step-up sign-in
	// is not asked again, which would loop, but shown (ADR-0052 §5).
	stepUpGrace = 2 * time.Minute
	maxForm     = 64 << 10
)

// signInAAD binds a sealed sign-in to its cookie.
var signInAAD = []byte("erp-web sign-in")

var loginCode = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

func (s *Server) now() time.Time { return clock.Now(s.Now) }

// cookie is one of the workspace's cookies: always Secure, HttpOnly, at
// the root and without Domain (the __Host- prefix's rules).
func cookie(name, value string, maxAge time.Duration, sameSite http.SameSite) *http.Cookie {
	c := &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: sameSite}
	switch {
	case maxAge > 0:
		c.MaxAge = int(maxAge / time.Second)
	case maxAge < 0:
		c.MaxAge = -1
	}
	return c
}

// safeReturn is a same-site path to come back to after sign-in: a path,
// never a URL, so no one can make the sign-in an open redirect.
func safeReturn(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || raw == "" || len(raw) > 2048 || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\t") {
		return "/"
	}
	return u.RequestURI()
}

// signInPath starts a sign-in with the code, returning to ret.
func signInPath(code, ret string, extra url.Values) string {
	q := url.Values{"return": {ret}}
	for k, v := range extra {
		q[k] = v
	}
	return "/login/" + url.PathEscape(code) + "?" + q.Encode()
}

// unavailable is the page of a sign-in that cannot be made now.
func (s *Server) unavailable(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusServiceUnavailable, ui.Problem(s.shell(i18n.SignInTitle, ""), i18n.SignInTitle, i18n.SignInUnavailable, "/login", i18n.SignInAgain))
}

// failed is the page of a sign-in that did not complete.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, status int, why string, err error) {
	s.Log.WarnContext(r.Context(), "web: sign-in failed", "reason", why, "client", r.RemoteAddr, "error", err)
	s.render(w, r, status, ui.Problem(s.shell(i18n.SignInFailedTitle, ""), i18n.SignInFailedTitle, i18n.SignInFailedBody, "/login", i18n.SignInAgain))
}

// loginPage asks for the login code, or goes on with the remembered one.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		s.unavailable(w, r)
		return
	}
	ret := safeReturn(r.URL.Query().Get("return"))
	if c, err := r.Cookie(codeCookie); err == nil && loginCode.MatchString(c.Value) && !r.URL.Query().Has("other") {
		http.Redirect(w, r, signInPath(c.Value, ret, nil), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, ui.SignIn(s.shell(i18n.SignInTitle, ""), ui.SignInForm{Return: ret}))
}

// loginForm takes the typed code; it changes nothing, so it needs no CSRF
// token (CrossOriginProtection still refuses another site's post). It
// answers with the continue page to the sign-in, not a redirect: the CSP's
// form-action 'self' governs a form's redirects too, and the sign-in goes
// on to the provider's origin.
func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	_ = r.ParseForm() // a body that does not parse leaves the code empty, which is answered as invalid
	f := ui.SignInForm{Code: strings.ToLower(strings.TrimSpace(r.PostForm.Get("code"))), Return: safeReturn(r.PostForm.Get("return"))}
	if !loginCode.MatchString(f.Code) {
		f.Error = i18n.SignInInvalid
		s.render(w, r, http.StatusUnprocessableEntity, ui.SignIn(s.shell(i18n.SignInTitle, ""), f))
		return
	}
	s.continueTo(w, r, signInPath(f.Code, f.Return, nil))
}

// login starts a sign-in at the code's provider: it remembers the code,
// seals the sign-in into its cookie and sends the person to the provider.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	code, q := r.PathValue("code"), r.URL.Query()
	if s.Store == nil {
		s.unavailable(w, r)
		return
	}
	f := ui.SignInForm{Code: code, Return: safeReturn(q.Get("return"))}
	if !loginCode.MatchString(code) {
		f.Error = i18n.SignInInvalid
		s.render(w, r, http.StatusNotFound, ui.SignIn(s.shell(i18n.SignInTitle, ""), f))
		return
	}
	client, err := s.Clients.Find(r.Context(), code)
	if errors.Is(err, session.ErrUnknownCode) {
		f.Error = i18n.SignInUnknown
		s.render(w, r, http.StatusNotFound, ui.SignIn(s.shell(i18n.SignInTitle, ""), f))
		return
	}
	t := signIn{Code: code, State: session.NewSecret(), Nonce: session.NewSecret(), Verifier: session.NewSecret(), Return: f.Return,
		ACR: stepUpACR(q.Get("acr")), MaxAge: stepUpMaxAge(q.Get("max_age")), Expires: s.now().Add(signInTTL)}
	if c, cerr := r.Cookie(sessionCookie); cerr == nil && t.stepUp() {
		t.Previous = c.Value
	}
	var target string
	if err == nil {
		target, err = s.OIDC.authorizeURL(r.Context(), client, t)
	}
	if err != nil {
		s.Log.ErrorContext(r.Context(), "web: a sign-in could not start", "login_code", code, "error", err)
		s.unavailable(w, r)
		return
	}
	sealed, _ := json.Marshal(t) // strings, times and a number always encode
	http.SetCookie(w, cookie(codeCookie, code, codeTTL, http.SameSiteLaxMode))
	http.SetCookie(w, cookie(signInCookie, base64.RawURLEncoding.EncodeToString(s.Store.Sealer.Seal(signInAAD, sealed)), signInTTL, http.SameSiteLaxMode))
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// stepUpACR is a step-up's acr_values, as the API's challenge named them:
// printable, space-separated, bounded; anything else asks for none.
func stepUpACR(v string) string {
	if len(v) > 1024 || strings.IndexFunc(v, func(r rune) bool { return r < ' ' || r > '~' }) >= 0 {
		return ""
	}
	return strings.Join(strings.Fields(v), " ")
}

// stepUpMaxAge is a step-up's max_age in seconds; nil for none.
func stepUpMaxAge(v string) *int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 86400 {
		return nil
	}
	return &n
}

// callback completes a sign-in: the provider's answer must match the
// sign-in's cookie (state), its code is exchanged with the PKCE verifier,
// the tokens must prove the sign-in (nonce, issuer, audiences, expiry,
// auth_time), and a new session with a fresh id replaces any other. The
// person then continues on a same-site page (ADR-0052 §5).
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, cookie(signInCookie, "", -1, http.SameSiteLaxMode)) // used once, whatever happens
	if s.Store == nil {
		s.unavailable(w, r)
		return
	}
	t, client, ok := s.answer(w, r)
	if !ok {
		return
	}
	ts, err := s.OIDC.exchange(r.Context(), client, r.URL.Query().Get("code"), t.Verifier)
	var who signedIn
	if err == nil {
		who, err = s.OIDC.verifyTokens(r.Context(), client, ts, t)
	}
	switch {
	case problem.From(err).Status == http.StatusServiceUnavailable, errors.Is(err, errCredential):
		s.Log.ErrorContext(r.Context(), "web: the provider could not complete a sign-in", "issuer", client.Issuer, "error", err)
		s.unavailable(w, r)
		return
	case err != nil:
		s.failed(w, r, http.StatusBadRequest, "tokens", err)
		return
	case t.ACR != "" && !acrAllowed(t.ACR, who.ACR):
		s.render(w, r, http.StatusUnauthorized, ui.Problem(s.shell(i18n.SignInFailedTitle, ""), i18n.SignInFailedTitle, i18n.SignInStepUpBody, "/login", i18n.SignInAgain))
		return
	}
	s.start(w, r, t, client, ts, who)
}

// answer reads the provider's answer against the sign-in's cookie: the
// sign-in must be this browser's, unexpired, with the same state, issued
// by its provider (RFC 9207's iss, where sent), and not an error.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) (signIn, session.Client, bool) {
	q := r.URL.Query()
	var t signIn
	c, err := r.Cookie(signInCookie)
	if err == nil {
		var raw, plain []byte
		if raw, err = base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			if plain, err = s.Store.Sealer.Open(signInAAD, raw); err == nil {
				err = json.Unmarshal(plain, &t)
			}
		}
	}
	switch {
	case err != nil, !s.now().Before(t.Expires):
		s.failed(w, r, http.StatusBadRequest, "no sign-in under way", err)
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(t.State)) != 1:
		s.failed(w, r, http.StatusBadRequest, "state", nil)
	case q.Get("error") != "":
		s.failed(w, r, http.StatusBadRequest, "provider: "+q.Get("error"), nil)
	default:
		client, err := s.Clients.Find(r.Context(), t.Code)
		switch {
		case errors.Is(err, session.ErrUnknownCode):
			s.failed(w, r, http.StatusBadRequest, "login code gone", err)
		case err != nil:
			s.Log.ErrorContext(r.Context(), "web: a sign-in could not complete", "error", err)
			s.unavailable(w, r)
		case q.Has("iss") && q.Get("iss") != client.Issuer:
			s.failed(w, r, http.StatusBadRequest, "iss", nil)
		default:
			return t, client, true
		}
	}
	return signIn{}, session.Client{}, false
}

// start makes the session and continues to the sign-in's return path.
func (s *Server) start(w http.ResponseWriter, r *http.Request, t signIn, client session.Client, ts tokenSet, who signedIn) {
	if t.Previous != "" {
		if err := s.Store.Delete(r.Context(), t.Previous); err != nil {
			s.Log.ErrorContext(r.Context(), "web: the session a step-up replaces was not deleted; the sweep deletes it", "error", err)
		}
	}
	sec := session.Secrets{AccessToken: ts.AccessToken, AccessExpiry: who.AccessExpiry, RefreshToken: ts.RefreshToken, IDToken: ts.IDToken}
	if t.stepUp() {
		sec.SteppedUp = s.now()
	}
	sess, err := s.Store.Create(r.Context(), session.Session{TenantID: client.TenantID, Issuer: client.Issuer, Subject: who.Subject, LoginCode: client.LoginCode, Secrets: sec})
	if err != nil {
		s.Log.ErrorContext(r.Context(), "web: a session could not be stored", "error", err)
		s.unavailable(w, r)
		return
	}
	// Who signed in from where: the client's address is the balancer's
	// PROXY header's, which the cells' audit does not see (ADR-0052,
	// Built in T73b).
	s.Log.InfoContext(r.Context(), "web: signed in", "tenant_id", client.TenantID, "login_code", client.LoginCode, "client", r.RemoteAddr, "step_up", t.stepUp())
	http.SetCookie(w, cookie(sessionCookie, sess.ID, 0, http.SameSiteStrictMode))
	s.continueTo(w, r, t.Return)
}

// continueTo serves the continue page that goes on to path: after the
// provider's cross-site redirect the browser would not send the new
// SameSite=Strict cookie on a redirect, but does on the navigation this
// same-site page starts (ADR-0052 §5).
func (s *Server) continueTo(w http.ResponseWriter, r *http.Request, path string) {
	s.leaveFor(w, r, safeReturn(path))
}

// leaveFor serves the continue page that goes on to target, a path or the
// provider's URL: after a form's POST the browser follows no redirect to
// another origin (the CSP's form-action 'self'), but the navigation this
// page starts is no form's.
func (s *Server) leaveFor(w http.ResponseWriter, r *http.Request, target string) {
	sh := s.shell(i18n.ContinueTitle, "")
	sh.Continue = target
	s.render(w, r, http.StatusOK, ui.Continue(sh))
}

// person is the signed-in person of a request.
type person struct {
	session.Session
}

type personKey struct{}

// signedIn serves next to a signed-in person, with their access token on
// the request's context for the API client (WithToken), renewed first when
// it is about to expire. A request without a session goes to sign in,
// and comes back to where it was.
func (s *Server) signedIn(next func(http.ResponseWriter, *http.Request, *person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil {
			s.unavailable(w, r)
			return
		}
		var id string
		if c, err := r.Cookie(sessionCookie); err == nil {
			id = c.Value
		}
		sess, err := s.Store.Load(r.Context(), id)
		if err == nil {
			sess, err = s.fresh(r.Context(), sess)
		}
		switch {
		case errors.Is(err, session.ErrNoSession), errors.Is(err, errSessionOver):
			s.toSignIn(w, r, id)
			return
		case err != nil:
			s.Log.ErrorContext(r.Context(), "web: a session could not be read", "error", err)
			s.unavailable(w, r)
			return
		}
		p := &person{Session: sess}
		ctx := context.WithValue(WithToken(r.Context(), p.AccessToken), personKey{}, p)
		next(w, r.WithContext(ctx), p)
	}
}

// errSessionOver means the session can go on no more: its access token
// expired and the provider will not renew it.
var errSessionOver = errors.New("web: the session's tokens cannot be renewed")

// fresh renews the session's access token with its refresh token when it
// is about to expire, once for all concurrent requests (session.Store's
// Renew); errSessionOver when it cannot be.
func (s *Server) fresh(ctx context.Context, sess session.Session) (session.Session, error) {
	enough := func(sec session.Secrets) bool { return sec.AccessExpiry.Sub(s.now()) > refreshAhead }
	switch {
	case enough(sess.Secrets):
		return sess, nil
	case sess.RefreshToken == "":
		return sess, s.end(ctx, sess.ID)
	}
	renewed, err := s.Store.Renew(ctx, sess, enough, func(ctx context.Context, sec session.Secrets) (session.Secrets, error) {
		return s.renew(ctx, sess, sec)
	})
	var refused errRefused
	if errors.As(err, &refused) || errors.Is(err, errInvalidTokens) || errors.Is(err, session.ErrUnknownCode) || errors.Is(err, errCredential) {
		s.Log.WarnContext(ctx, "web: a session's tokens could not be renewed; it ends", "error", err)
		return sess, s.end(ctx, sess.ID)
	}
	return renewed, err
}

// renew refreshes sec's tokens at the session's provider (RFC 6749 §6):
// a new access token for the same subject, a rotated refresh token where
// the provider rotates them, and a new ID token where it sends one.
func (s *Server) renew(ctx context.Context, sess session.Session, sec session.Secrets) (session.Secrets, error) {
	client, err := s.Clients.Find(ctx, sess.LoginCode)
	if err != nil {
		return sec, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ts, err := s.OIDC.refresh(ctx, client, sec.RefreshToken)
	if err != nil {
		return sec, err
	}
	access, err := s.OIDC.verifyAccess(ctx, client, ts.AccessToken, sess.Subject)
	if err != nil {
		return sec, err
	}
	sec.AccessToken, sec.AccessExpiry = ts.AccessToken, access.Expiry
	if ts.RefreshToken != "" {
		sec.RefreshToken = ts.RefreshToken
	}
	if ts.IDToken != "" {
		sec.IDToken = ts.IDToken
	}
	return sec, nil
}

// end deletes a session that is over; errSessionOver once it is gone.
func (s *Server) end(ctx context.Context, id string) error {
	if err := s.Store.Delete(ctx, id); err != nil {
		return err
	}
	return errSessionOver
}

// toSignIn sends a request without a session to sign in, and back. A page
// reached from another site comes without the SameSite=Strict cookie, so
// it first gets the continue page, whose own navigation carries it
// (ADR-0052 §5); a write goes to sign in without coming back to it.
func (s *Server) toSignIn(w http.ResponseWriter, r *http.Request, stale string) {
	if stale != "" {
		http.SetCookie(w, cookie(sessionCookie, "", -1, http.SameSiteStrictMode))
	}
	ret := "/"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		ret = r.URL.RequestURI()
		if stale == "" && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			s.continueTo(w, r, ret)
			return
		}
	}
	http.Redirect(w, r, "/login?"+url.Values{"return": {ret}}.Encode(), http.StatusSeeOther)
}

// csrf refuses a state-changing request whose synchronizer token is not
// the session's (OWASP), in the form or htmx's header (ADR-0052 §5).
func (s *Server) csrf(next func(http.ResponseWriter, *http.Request, *person)) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		token := r.Header.Get(ui.CSRFHeader)
		if token == "" {
			_ = r.ParseForm() // a body that does not parse carries no token, which is refused
			token = r.PostForm.Get(ui.CSRFField)
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(p.CSRF)) != 1 {
			s.Log.WarnContext(r.Context(), "web: a request without the session's CSRF token was refused", "path", r.URL.Path)
			s.refused(w, r)
			return
		}
		next(w, r, p)
	}
}

// refused is the page of a request refused as a possible forgery: by the
// CSRF token or by CrossOriginProtection.
func (s *Server) refused(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusForbidden, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.RefusedBody, "/", i18n.NotFoundHome))
}

// logout signs the person out (ADR-0052 §5, G9): the API's session ends
// (endCurrentSession), then this one, then the provider's where it offers
// RP-initiated logout. A failure to end the API's session is logged and
// does not keep the person signed in here.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p *person) {
	s.endAPISession(r.Context())
	if err := s.Store.Delete(r.Context(), p.ID); err != nil {
		s.Log.ErrorContext(r.Context(), "web: a session could not be deleted at sign-out", "error", err)
		s.unavailable(w, r)
		return
	}
	http.SetCookie(w, cookie(sessionCookie, "", -1, http.SameSiteStrictMode))
	s.Log.InfoContext(r.Context(), "web: signed out", "tenant_id", p.TenantID, "login_code", p.LoginCode, "client", r.RemoteAddr)
	if client, err := s.Clients.Find(r.Context(), p.LoginCode); err == nil {
		if u := s.OIDC.logoutURL(r.Context(), client, p.IDToken); u != "" {
			s.leaveFor(w, r, u)
			return
		}
	}
	http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
}

// signedOut says the person is signed out.
func (s *Server) signedOut(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, ui.Problem(s.shell(i18n.SignedOutTitle, ""), i18n.SignedOutTitle, i18n.SignedOutBody, "/login", i18n.SignInAgain))
}
