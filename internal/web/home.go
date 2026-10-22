package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// home is the signed-in person's home: the companies the API lets them
// see, read as them (ADR-0052 §7). A person without access to any sees
// that, not an error (§12).
func (s *Server) home(w http.ResponseWriter, r *http.Request, p *person) {
	res, err := s.API.ListLegalEntities(r.Context(), gen.ListLegalEntitiesParams{})
	if err != nil {
		s.Log.ErrorContext(r.Context(), "web: the API could not be reached", "operation", "listLegalEntities", "error", err)
		s.unavailable(w, r)
		return
	}
	var h ui.HomePage
	switch res := res.(type) {
	case *gen.LegalEntityPageHeaders:
		for _, e := range res.Response.Items {
			h.Companies = append(h.Companies, e.Name)
		}
	case *gen.UnauthorizedHeaders:
		s.unauthorized(w, r, p, res)
		return
	case *gen.ForbiddenHeaders:
		// No permission to list companies: none to show (§12).
	default:
		s.Log.WarnContext(r.Context(), "web: the API refused a page's read", "operation", "listLegalEntities", "answer", res)
		s.unavailable(w, r)
		return
	}
	s.render(w, r, http.StatusOK, ui.Home(s.page(i18n.HomeTitle, "/", p), h))
}

// page is the shell of a signed-in person's page.
func (s *Server) page(title i18n.Key, current string, p *person) ui.Shell {
	sh := s.shell(title, current)
	sh.Person = &ui.Person{CSRF: p.CSRF}
	return sh
}

// unauthorized answers the API's 401 to a person's call (ADR-0052 §5):
// the RFC 9470 step-up challenge signs the person in again for the
// authentication it asks, coming back to the page, unless they did just
// that (then it says so rather than loop); any other 401 means the API's
// session ended, and so does this one.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request, p *person, res *gen.UnauthorizedHeaders) {
	challenge := params(res.WWWAuthenticate.Or(""))
	if challenge["error"] != "insufficient_user_authentication" {
		if err := s.Store.Delete(r.Context(), p.ID); err != nil {
			s.Log.ErrorContext(r.Context(), "web: a session the API ended could not be deleted; the sweep deletes it", "error", err)
		}
		s.toSignIn(w, r, p.ID)
		return
	}
	if !p.SteppedUp.IsZero() && s.now().Sub(p.SteppedUp) < stepUpGrace {
		s.render(w, r, http.StatusUnauthorized, ui.Problem(s.page(i18n.SignInFailedTitle, "", p), i18n.SignInFailedTitle, i18n.SignInStepUpBody, "/", i18n.NotFoundHome))
		return
	}
	extra := url.Values{}
	if v := challenge["acr_values"]; v != "" {
		extra.Set("acr", v)
	}
	if v := challenge["max_age"]; v != "" {
		extra.Set("max_age", v)
	}
	if len(extra) == 0 {
		extra.Set("max_age", "0") // no value named: a fresh authentication at least
	}
	ret := "/"
	if r.Method == http.MethodGet {
		ret = r.URL.RequestURI()
	}
	http.Redirect(w, r, signInPath(p.LoginCode, ret, extra), http.StatusSeeOther)
}

// params reads a Bearer challenge's auth-params (RFC 9110 §11.2), quoted
// or not.
func params(challenge string) map[string]string {
	out := map[string]string{}
	_, rest, _ := strings.Cut(challenge, " ")
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimLeft(strings.TrimSpace(rest), ",") {
		rest = strings.TrimSpace(rest)
		name, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var value string
		value, rest = paramValue(after)
		out[strings.ToLower(strings.TrimSpace(name))] = value
	}
	return out
}

// paramValue reads one auth-param value, a quoted-string with its escapes
// or a token, and returns what follows it.
func paramValue(s string) (value, rest string) {
	if !strings.HasPrefix(s, `"`) {
		value, rest, _ = strings.Cut(s, ",")
		return strings.TrimSpace(value), rest
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), ""
}

// endAPISession ends the person's session at the API (endCurrentSession,
// ADR-0052 G9). Its failure is logged: sign-out goes on.
func (s *Server) endAPISession(ctx context.Context) {
	res, err := s.API.EndCurrentSession(ctx, gen.EndCurrentSessionParams{IdempotencyKey: uuid.NewString()})
	if err != nil {
		s.Log.ErrorContext(ctx, "web: the API's session could not be ended at sign-out", "error", err)
		return
	}
	if _, ok := res.(*gen.SessionEndHeaders); !ok {
		s.Log.WarnContext(ctx, "web: the API did not end its session at sign-out", "answer", res)
	}
}
