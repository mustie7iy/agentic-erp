package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/middleware"
	"github.com/ogen-go/ogen/openapi"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestAuditRefusals: a listed write refused with a business outcome is
// recorded on its {id}, with the operation; a write that succeeds, one
// not listed, a body refused (perhaps before the document was read) and
// one whose {id} is no uuid record nothing, and every
// answer is the handler's.
func TestAuditRefusals(t *testing.T) {
	id := uuid.New()
	refused := problem.Conflict("test.closed", "Closed.")
	mw := auditRefusals(map[string]audit.Entry{"postThing": {Action: "test.thing.post", ObjectType: "test.thing"}})
	for _, c := range []struct {
		name, op string
		param    any
		err      error
		want     int
	}{
		{"refused", "postThing", id, refused, 1},
		{"succeeded", "postThing", id, nil, 0},
		{"not listed", "postOther", id, refused, 0},
		{"a body refused", "postThing", id, problem.Unprocessable("request.validation_failed", "Bad body."), 0},
		{"an id that is no uuid", "postThing", "x", refused, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, buf := audit.WithBuffer(context.Background())
			req := middleware.Request{Context: ctx, OperationID: c.op,
				Params: middleware.Parameters{{Name: "id", In: openapi.LocationPath}: c.param}}
			_, err := mw(req, func(middleware.Request) (middleware.Response, error) { return middleware.Response{}, c.err })
			if !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
				t.Errorf("answer: got %v, want %v", err, c.err)
			}
			if buf.Len() != c.want {
				t.Errorf("refusals recorded: got %d, want %d", buf.Len(), c.want)
			}
		})
	}
}

// TestRefusalsAuditedNameWrites: every operation listed is a write of the
// contract on a document named by its {id}, recorded under an action of
// its object type's context.
func TestRefusalsAuditedNameWrites(t *testing.T) {
	ops := api.MustOperations()
	for id, e := range RefusalsAudited {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("not in the contract")
			}
			if op.Method == http.MethodGet || op.Reads || !strings.Contains(op.Path, "/{id}") {
				t.Errorf("%s %s: want a write on a document's {id}", op.Method, op.Path)
			}
			ctx, _, _ := strings.Cut(e.ObjectType, ".")
			if !strings.HasPrefix(e.Action, ctx+".") || strings.Count(e.Action, ".") != 2 {
				t.Errorf("action %s of %s: want context.object.verb in %s", e.Action, e.ObjectType, ctx)
			}
		})
	}
}
