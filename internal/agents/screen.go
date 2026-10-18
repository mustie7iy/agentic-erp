package agents

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/redact"
)

// Countries reads the countries of the tenant's legal entities in tx
// (tenancy.Countries): whose tax identifiers are redacted.
type Countries func(ctx context.Context, tx pgx.Tx) ([]string, error)

// screened is what a request carries out of the cell once screened.
type screened struct {
	redactions int
	documents  bool
}

// screen redacts the request's text before it leaves the cell, IBANs,
// card numbers and the tax identifiers of the tenant's countries
// (ADR-0036), and refuses documents, which cannot be redacted, while the
// tenant's policy withholds them. Only text is redacted: what the
// request's prompt, system prompt and tool results say. Tool definitions
// and the model's own earlier turns' tool input carry no data the model was
// not already given.
func (g *Gateway) screen(ctx context.Context, tx pgx.Tx, pol Policy, req *request) error {
	var countries []string
	if g.Countries != nil {
		var err error
		if countries, err = g.Countries(ctx, tx); err != nil {
			return err
		}
	}
	s := &screener{r: redact.For(countries)}
	for _, field := range []string{"system", "messages"} {
		if raw, ok := req.fields[field]; ok {
			req.fields[field] = s.content(raw, field == "messages")
		}
	}
	if s.documents && !pol.DocumentsToModels {
		return problem.Unprocessable("agents.documents_withheld", "This tenant's AI policy withholds documents from models; the call carries one.")
	}
	req.redactions = s.n
	req.body, _ = json.Marshal(req.fields) // raw messages of a decoded object always encode
	return nil
}

// screener walks a request's content, redacting text and noting
// documents.
type screener struct {
	r         *redact.Redactor
	n         int
	documents bool
}

// content screens a system prompt or the messages: a string, or a list of
// blocks; with messages, a list of messages whose content is either. What
// is neither passes as it is, for the provider to judge.
func (s *screener) content(raw json.RawMessage, messages bool) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return s.text(text)
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw
	}
	for _, item := range items {
		if messages {
			if c, ok := item["content"]; ok {
				item["content"] = s.content(c, false)
			}
			continue
		}
		s.block(item)
	}
	out, _ := json.Marshal(items) // decoded raw messages always encode
	return out
}

// block screens one content block in place.
func (s *screener) block(b map[string]json.RawMessage) {
	var kind string
	_ = json.Unmarshal(b["type"], &kind) // a block without a string type is the provider's to refuse
	switch kind {
	case "text":
		var text string
		if json.Unmarshal(b["text"], &text) == nil {
			b["text"] = s.text(text)
		}
	case "tool_result":
		if c, ok := b["content"]; ok {
			b["content"] = s.content(c, false)
		}
	case "document", "image":
		s.documents = true
	}
}

func (s *screener) text(text string) json.RawMessage {
	out, n := s.r.Text(text)
	s.n += n
	raw, _ := json.Marshal(out) // a string always encodes
	return raw
}
