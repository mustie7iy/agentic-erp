package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
)

// router finds the operation a method and path reach (the generated server).
type router interface {
	FindPath(method string, u *url.URL) (gen.Route, bool)
}

// notice is the headers that announce a deprecated operation (ADR-0041).
type notice struct{ deprecation, sunset, link string }

// deprecations is the notice of each deprecated operation by operationId,
// built from the contract so that no list is kept by hand: Deprecation as
// an RFC 9745 date (@<unix seconds>), Sunset as an RFC 8594 HTTP date and a
// Link to the migration notes with rel="deprecation".
func deprecations(ops map[string]api.Operation) map[string]notice {
	out := map[string]notice{}
	for id, op := range ops {
		if !op.Lifecycle.Deprecated {
			continue
		}
		out[id] = notice{
			deprecation: "@" + strconv.FormatInt(op.Lifecycle.DeprecatedAt.Unix(), 10),
			sunset:      op.Lifecycle.Sunset.UTC().Format(http.TimeFormat),
			link:        "<" + op.Lifecycle.Link + `>; rel="deprecation"; type="text/html"`,
		}
	}
	return out
}

// announce sets the notice of a deprecated operation on every answer to
// it, errors included (an unauthenticated call learns of the sunset too),
// before the request goes on. A handler that set a Link of its own would
// replace this one (the unit of work copies headers by name); none does.
func announce(routes router, table map[string]notice, next http.Handler) http.Handler {
	if len(table) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route, ok := routes.FindPath(r.Method, r.URL); ok {
			if n, ok := table[route.OperationID()]; ok {
				h := w.Header()
				h.Set("Deprecation", n.deprecation)
				h.Set("Sunset", n.sunset)
				h.Add("Link", n.link)
			}
		}
		next.ServeHTTP(w, r)
	})
}
