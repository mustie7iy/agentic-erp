package agents

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/redact"
)

// Countries reads the countries of the tenant's legal entities in tx
// (tenancy.Countries): whose tax identifiers are redacted.
type Countries func(ctx context.Context, tx pgx.Tx) ([]string, error)

// screen redacts the request's text before it leaves the cell, IBANs,
// card numbers and the tax identifiers of the tenant's countries
// (ADR-0036), and refuses documents, which cannot be redacted, while the
// tenant's policy withholds them. Every block's text and nested content is
// redacted, whatever its type, and so is the input of the earlier turns'
// tool calls, which the client sends back as it likes. Tool definitions are
// the caller's code, not data, and pass.
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

// block screens one content block in place. A type the gateway does not
// know is screened all the same: its text, its nested content and its
// input; fail-closed for data, the provider judges the shape.
func (s *screener) block(b map[string]json.RawMessage) {
	var kind string
	_ = json.Unmarshal(b["type"], &kind) // a block without a string type is the provider's to refuse
	if kind == "document" || kind == "image" {
		s.documents = true
	}
	var text string
	if t, ok := b["text"]; ok && json.Unmarshal(t, &text) == nil {
		b["text"] = s.text(text)
	}
	if c, ok := b["content"]; ok {
		b["content"] = s.content(c, false)
	}
	if in, ok := b["input"]; ok {
		var n int
		b["input"], n = redactJSON(s.r, in)
		s.n += n
	}
}

func (s *screener) text(text string) json.RawMessage {
	out, n := s.r.Text(text)
	s.n += n
	raw, _ := json.Marshal(out) // a string always encodes
	return raw
}

// minimize redacts a tool call's input and summary before they are
// recorded, as what leaves for a model is (ADR-0036), for the tenant's
// countries in tx; nil countries: IBANs and card numbers only.
func minimize(ctx context.Context, tx pgx.Tx, countries Countries, input map[string]json.RawMessage, summary *string) (map[string]json.RawMessage, *string, error) {
	var cs []string
	if countries != nil {
		var err error
		if cs, err = countries(ctx, tx); err != nil {
			return nil, nil, err
		}
	}
	r := redact.For(cs)
	out := make(map[string]json.RawMessage, len(input))
	for k, v := range input {
		out[k], _ = redactJSON(r, v) // the record keeps no count
	}
	if summary != nil {
		s, _ := r.Text(*summary)
		summary = &s
	}
	return out, summary, nil
}

// redactJSON is a JSON value with every string in it redacted, and how
// many replacements that made. Numbers are kept as they were written
// (UseNumber): an amount or an id never passes through a float.
func redactJSON(r *redact.Redactor, raw json.RawMessage) (json.RawMessage, int) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw, 0 // not JSON: passed as it is, for whoever reads it next to refuse
	}
	n := 0
	out, _ := json.Marshal(redactValue(r, v, &n)) // decoded JSON always encodes
	return out, n
}

func redactValue(r *redact.Redactor, v any, n *int) any {
	switch v := v.(type) {
	case string:
		out, k := r.Text(v)
		*n += k
		return out
	case []any:
		for i := range v {
			v[i] = redactValue(r, v[i], n)
		}
	case map[string]any:
		for k := range v {
			v[k] = redactValue(r, v[k], n)
		}
	}
	return v
}
