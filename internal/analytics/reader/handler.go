package reader

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// QueryPath is where erp-server posts a Request.
const QueryPath = wire.QueryPath

// maxRequest bounds a request's body: a plan names a few columns and
// values.
const maxRequest = 1 << 20

// Handler serves Query at QueryPath. The listener in front of it admits
// erp-server alone (ADR-0033); a refusal is 422 analytics.refused, which
// the semantic layer answers by falling back to the database, and so is a
// query beyond the reader's cap, 503 analytics.busy.
func Handler(r *Reader, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+QueryPath, func(w http.ResponseWriter, req *http.Request) {
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxRequest))
		dec.DisallowUnknownFields()
		var q Request
		if err := dec.Decode(&q); err != nil {
			problem.Write(w, req, problem.BadRequest("analytics.invalid_request", err.Error()))
			return
		}
		res, err := r.Query(req.Context(), q)
		switch {
		case errors.Is(err, ErrRefused):
			log.WarnContext(req.Context(), "analytics query refused", "tenant", q.TenantID, "error", err)
			problem.Write(w, req, problem.Unprocessable(wire.RefusedCode, err.Error()))
			return
		case errors.Is(err, ErrBusy):
			p := problem.New(http.StatusServiceUnavailable, wire.BusyCode, "Analytics busy", err.Error())
			p.RetryAfter = 1
			problem.Write(w, req, p)
			return
		case err != nil:
			problem.Write(w, req, problem.Internal(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res) // a write error is the client gone; nothing is left to tell it
	})
	return mux
}
