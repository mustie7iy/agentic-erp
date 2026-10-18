package agents_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// redacting is a call whose text carries an IBAN in the prompt, a card in
// a tool result and a German VAT id in the system prompt.
const redacting = `{"model": "erp-default", "max_tokens": 8,
	"system": [{"type": "text", "text": "Supplier VAT DE123456789."}],
	"messages": [
		{"role": "user", "content": "Pay DE89 3704 0044 0532 0130 00 for Anna Schmidt <anna@example.com>"},
		{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "getCard", "input": {"q": "4111111111111111"}}]},
		{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "card 4111 1111 1111 1111"}]},
			{"type": "tool_result", "tool_use_id": "t2", "content": "EIN 12-3456789"},
			{"type": "search_result", "source": "s", "title": "t", "content": [{"type": "text", "text": "IBAN NL91 ABNA 0417 1643 00"}]}]}
	]}`

// TestRedaction: the gateway forwards a call's text with its IBANs, card
// numbers and the tax ids of the tenant's countries replaced, names and
// e-mail addresses as they were, and records how many it replaced, never
// what (ADR-0036).
func TestRedaction(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)}
	g := gateway(t, p)
	g.Countries = func(context.Context, pgx.Tx) ([]string, error) { return []string{"DE"}, nil }
	rec := post(t, g, principal(ten), redacting, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d %s, want 200", rec.Code, rec.Body)
	}
	sent := string(p.sent)
	for _, gone := range []string{"DE89", "4111 1111", "4111111111111111", "DE123456789", "NL91"} {
		if strings.Contains(sent, gone) {
			t.Errorf("forwarded %q: %s", gone, sent)
		}
	}
	for _, kept := range []string{"[IBAN]", "[CARD]", "[TAX_ID]", "Anna Schmidt", "anna@example.com", "12-3456789", `"q":"[CARD]"`} {
		if !strings.Contains(sent, kept) {
			t.Errorf("not forwarded %q: %s", kept, sent)
		}
	}
	if n := count(t, ten, `SELECT redactions FROM agents.model_call WHERE tenant_id = $1`); n != 5 {
		t.Errorf("redactions: got %d, want 5", n)
	}
}

// TestDocumentsWithheld: documents cannot be redacted, so while the
// tenant's policy withholds them a call carrying one, in a message or a
// tool result, is refused with 422 agents.documents_withheld and not
// forwarded; by default they go (ADR-0036).
func TestDocumentsWithheld(t *testing.T) {
	priced(t, "claude-opus-5-5")
	for _, c := range []struct {
		name, block string
	}{
		{"a PDF", `{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBE"}}`},
		{"an image in a tool result", `{"type": "tool_result", "tool_use_id": "t", "content": [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBO"}}]}`},
	} {
		body := `{"model": "erp-default", "max_tokens": 8, "messages": [{"role": "user", "content": [` + c.block + `]}]}`
		t.Run(c.name+", withheld", func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id, documents_to_models) VALUES ($1, false)`, ten.ID)
			p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)}
			rec := post(t, gateway(t, p), principal(ten), body, nil)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "agents.documents_withheld") {
				t.Errorf("got %d %s, want 422 agents.documents_withheld", rec.Code, rec.Body)
			}
			if p.sent != nil {
				t.Error("the call was forwarded")
			}
		})
		t.Run(c.name+", allowed", func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)}
			if rec := post(t, gateway(t, p), principal(ten), body, nil); rec.Code != http.StatusOK {
				t.Errorf("status: got %d %s, want 200", rec.Code, rec.Body)
			}
		})
	}
}

// TestScreenPassesWhatItCannotRead: content in a shape the gateway does
// not know is passed for the provider to judge; an unreadable list of
// countries fails the call.
func TestScreenPassesWhatItCannotRead(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	p := &provider{status: 400, body: `{"type": "error"}`}
	g := gateway(t, p)
	if rec := post(t, g, principal(ten), `{"model": "erp-default", "max_tokens": 8, "system": 7, "messages": [{"role": "user", "content": [{"type": 1}]}]}`, nil); rec.Code != 400 {
		t.Errorf("status: got %d, want the provider's 400", rec.Code)
	}
	if !strings.Contains(string(p.sent), `"system":7`) {
		t.Errorf("forwarded: got %s, want the system prompt as it was", p.sent)
	}
	g.Countries = func(context.Context, pgx.Tx) ([]string, error) { return nil, errors.New("tenancy unreadable") }
	if rec := post(t, g, principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
}

// TestToolCallsMinimized: a tool call is recorded with its input's and its
// summary's IBANs and card numbers redacted, as the gateway redacts what
// leaves (ADR-0036); other values are kept as they were.
func TestToolCallsMinimized(t *testing.T) {
	_, agent := setup(t)
	s, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "p", "model": "m"}`, "")
	c, _ := agent.Must(201, "POST", "/v1/agent-sessions/"+s["id"].(string)+"/tool-calls",
		`{"tool": "payInvoice", "input": {"iban": "DE89 3704 0044 0532 0130 00", "cards": [{"pan": "4111 1111 1111 1111"}], "n": 5, "ok": true,
			"amount": 12345678901234567890, "price": 1234.50},
		"result_summary": "paid to DE89370400440532013000"}`, "")
	if got := fmt.Sprint(c["input"]); got != "map[amount:1.2345678901234567e+19 cards:[map[pan:[CARD]]] iban:[IBAN] n:5 ok:true price:1234.5]" {
		t.Errorf("input: got %s", got)
	}
	var amount, price string
	pgtest.Scan(t, `SELECT input->>'amount', input->>'price' FROM agents.tool_call WHERE session_id = $1`, []any{s["id"]}, &amount, &price)
	if amount != "12345678901234567890" {
		t.Errorf("recorded amount: got %s, want every digit", amount)
	}
	if price != "1234.50" {
		t.Errorf("recorded price: got %s, want 1234.50 as written", price)
	}
	if c["result_summary"] != "paid to [IBAN]" {
		t.Errorf("summary: got %v, want paid to [IBAN]", c["result_summary"])
	}
}

// TestToolCallInputNotJSON: input that is not JSON, which the API's
// decoder never passes, is refused by the database rather than recorded.
func TestToolCallInputNotJSON(t *testing.T) {
	_, agent := setup(t)
	s, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "p", "model": "m"}`, "")
	h := &agents.Endpoints{}
	err := apitest.InRequest(t, agent.Tenant, agent.As, func(ctx context.Context) error {
		_, err := h.RecordAgentToolCall(ctx, &gen.AgentToolCallCreate{Tool: "x", Input: gen.AgentToolCallCreateInput{"a": []byte("{")}},
			gen.RecordAgentToolCallParams{ID: uuid.MustParse(s["id"].(string))})
		return err
	})
	if err == nil {
		t.Error("got no error, want the database's refusal")
	}
}
