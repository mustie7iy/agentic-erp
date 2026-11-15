// Package mcp serves the API's operations open to agents as Model Context
// Protocol tools over standard input and output (T102a, ADR-0081): an
// agent host launches erp mcp and lists and calls the tools apitools
// builds from the contract, each call going to the API with the agent's
// token and recorded in its session (ADR-0036), so permissions, company
// access, autonomy and segregation of duties apply unchanged. It serves
// the protocol version 2026-07-28 (no handshake, the version in each
// request's _meta) and answers 2025-11-25's initialize and ping for hosts
// not on it yet, with the same tools. There is no listener on the
// network. The messages are those of the specification's schema,
// docs/design/specs/mcp.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// The protocol versions served: the current one, stateless, and the
// legacy one a host reaches through initialize.
const (
	Modern = "2026-07-28"
	Legacy = "2025-11-25"
)

// supported is what server/discover and a refused version name.
var supported = []string{Modern, Legacy}

// The _meta keys of 2026-07-28 (RequestMetaObject, ResultMetaObject).
const (
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo   = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC and protocol error codes (the schema's).
const (
	codeParse         = -32700
	codeInvalid       = -32600
	codeNoMethod      = -32601
	codeInvalidParams = -32602
	codeInternal      = -32603
	codeUnsupported   = -32022
)

// Instructions tell a host's model what the server is.
const Instructions = "The ERP's public API as tools, one per operation open to agents, named by its operationId. " +
	"Each call is made with the agent's token and recorded in its session: the API's permissions, company access, " +
	"autonomy level and segregation of duties decide it. A result is the API's answer: its status, ETag and body, " +
	"problems included (isError)."

// Server answers one host's messages. Tools are those the agent is
// offered (apitools.Offered at its session's level).
type Server struct {
	Tools   []*apitools.Tool
	Name    string // the implementation's name and version (serverInfo)
	Version string

	byName map[string]*apitools.Tool
	legacy bool // a 2025-11-25 host initialized this process
}

// Serve reads newline-delimited JSON-RPC messages from in until its end
// and writes each answer to out, one per line (the stdio transport). It
// returns when in ends, or with the first failed read or write.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.byName = map[string]*apitools.Tool{}
	for _, t := range s.Tools {
		s.byName[t.Name()] = t
	}
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if answer := s.handle(ctx, line); answer != nil {
				if werr := write(out, answer); werr != nil {
					return werr
				}
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("mcp: read: %w", err)
		}
	}
}

// write sends one message on its own line: json.Marshal escapes every
// newline inside strings, so a message never spans two.
func write(w io.Writer, msg *response) error {
	// A response holds strings, numbers, maps of them, valid JSON (the
	// API's bodies, checked), bytes and the tools' schemas: it always
	// encodes.
	b, _ := json.Marshal(msg)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

// message is a JSON-RPC message as received.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is a JSON-RPC response: a result or an error.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func fail(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// handle answers one line: a response to a request, nil for a
// notification or a response from the host.
func (s *Server) handle(ctx context.Context, line []byte) *response {
	if !json.Valid(line) {
		return &response{JSONRPC: "2.0", Error: fail(codeParse, "Parse error")}
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil { // valid JSON that is not an object: a batch, a scalar
		return &response{JSONRPC: "2.0", Error: fail(codeInvalid, "Invalid request: a message is one JSON object")}
	}
	id, hasID := requestID(m.ID)
	switch {
	case m.ID != nil && !hasID:
		return &response{JSONRPC: "2.0", Error: fail(codeInvalid, "Invalid request: an id is a string or an integer")}
	case m.JSONRPC != "2.0" || (m.Method == nil && !hasID):
		return &response{JSONRPC: "2.0", ID: id, Error: fail(codeInvalid, "Invalid request: not a JSON-RPC 2.0 request")}
	case m.Method == nil: // a response to a request the server never sends
		return nil
	case !hasID:
		// A notification is never answered: initialized ends a legacy
		// handshake and a cancellation needs nothing, as each request is
		// answered before the next is read.
		return nil
	}
	result, err := s.call(ctx, *m.Method, m.Params)
	if err != nil {
		return &response{JSONRPC: "2.0", ID: id, Error: err}
	}
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

// requestID is the message's id when it is one a request may carry: a
// string or an integer, never null (absent: none).
func requestID(raw json.RawMessage) (json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	var v any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	_ = d.Decode(&v) // part of a message json.Valid accepted
	switch x := v.(type) {
	case string:
		return raw, true
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return raw, true
		}
	}
	return nil, false
}

// params are a request's parameters as the server reads them.
type params struct {
	Meta            map[string]json.RawMessage `json:"_meta"`
	ProtocolVersion *string                    `json:"protocolVersion"`
	Cursor          *string                    `json:"cursor"`
	Name            *string                    `json:"name"`
	Arguments       json.RawMessage            `json:"arguments"`
}

// call answers a request by its method.
func (s *Server) call(ctx context.Context, method string, raw json.RawMessage) (any, *rpcError) {
	var p params
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fail(codeInvalidParams, "Invalid params: %v", err)
		}
	}
	if method == "initialize" {
		return s.initialize(p)
	}
	modern, err := s.version(p)
	if err != nil {
		return nil, err
	}
	switch method {
	case "server/discover":
		if !modern {
			break
		}
		return s.complete(map[string]any{"supportedVersions": supported, "capabilities": capabilities(),
			"instructions": Instructions, "ttlMs": 0, "cacheScope": "public"}), nil
	case "ping":
		if modern {
			break
		}
		return map[string]any{}, nil
	case "tools/list":
		return s.list(p, modern)
	case "tools/call":
		return s.toolCall(ctx, p, modern)
	}
	return nil, fail(codeNoMethod, "Method not found: %s", method)
}

// version says whether a request is served as 2026-07-28 (its _meta names
// the version) or as 2025-11-25 (no _meta, after initialize); any other
// request is malformed or asks for a version not served.
func (s *Server) version(p params) (bool, *rpcError) {
	raw, ok := p.Meta[metaVersion]
	if !ok {
		if s.legacy {
			return false, nil
		}
		return false, fail(codeInvalidParams, "Invalid params: _meta[%q] is required (or initialize first, for %s)", metaVersion, Legacy)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v != Modern {
		e := fail(codeUnsupported, "Unsupported protocol version: %s", raw)
		e.Data = map[string]any{"supported": supported, "requested": strings.Trim(string(raw), `"`)}
		return false, e
	}
	if _, ok := p.Meta[metaCapabilities]; !ok {
		return false, fail(codeInvalidParams, "Invalid params: _meta[%q] is required", metaCapabilities)
	}
	return true, nil
}

// initialize is 2025-11-25's handshake: the version is the host's when
// served, else the legacy one.
func (s *Server) initialize(p params) (any, *rpcError) {
	if p.ProtocolVersion == nil {
		return nil, fail(codeInvalidParams, "Invalid params: protocolVersion is required")
	}
	s.legacy = true
	return map[string]any{"protocolVersion": Legacy, "capabilities": capabilities(), "serverInfo": s.info(), "instructions": Instructions}, nil
}

func capabilities() map[string]any { return map[string]any{"tools": map[string]any{}} }

func (s *Server) info() map[string]string {
	return map[string]string{"name": s.Name, "version": s.Version}
}

// complete adds what a 2026-07-28 result carries: its type and the
// server's identity.
func (s *Server) complete(result map[string]any) map[string]any {
	result["resultType"] = "complete"
	result["_meta"] = map[string]any{metaServerInfo: s.info()}
	return result
}

// list is every tool, in the order of their names; the list is one page,
// so no cursor names another.
func (s *Server) list(p params, modern bool) (any, *rpcError) {
	if p.Cursor != nil {
		return nil, fail(codeInvalidParams, "Invalid params: unknown cursor %q", *p.Cursor)
	}
	tools := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		tools = append(tools, map[string]any{"name": t.Name(), "description": t.Description(), "inputSchema": t.InputSchema(),
			"annotations": map[string]any{"readOnlyHint": t.Reads()}})
	}
	if !modern {
		return map[string]any{"tools": tools}, nil
	}
	// The list follows the caller's autonomy level: never shared across
	// tokens.
	return s.complete(map[string]any{"tools": tools, "ttlMs": 0, "cacheScope": "private"}), nil
}

// toolCall calls a tool: a name not offered and arguments that are not
// an object holding the required ones are refused before the API is
// called; the API's answer, problems included, is the result.
func (s *Server) toolCall(ctx context.Context, p params, modern bool) (any, *rpcError) {
	if p.Name == nil {
		return nil, fail(codeInvalidParams, "Invalid params: name is required")
	}
	t, ok := s.byName[*p.Name]
	if !ok {
		return nil, fail(codeInvalidParams, "Unknown tool: %s", *p.Name)
	}
	input := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &input); err != nil {
			return nil, fail(codeInvalidParams, "Invalid params: arguments are an object")
		}
	}
	for _, r := range t.Required() {
		if _, ok := input[r]; !ok {
			return nil, fail(codeInvalidParams, "Invalid params: argument %s is required", r)
		}
	}
	resp, err := t.Invoke(ctx, input)
	if errors.Is(err, apitools.ErrNotRecorded) {
		return nil, fail(codeInternal, "Internal error: %v", err)
	}
	result := toolResult(resp, err)
	if !modern {
		return result, nil
	}
	return s.complete(result), nil
}

// toolResult is the API's answer as a tool's result: its status, ETag and
// body (JSON as it is, other text as a string, other bytes in base64) as
// structured content and as its text; a problem or a call that could not
// be made is an error the model sees.
func toolResult(resp apitools.Response, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	answer := map[string]any{"status": resp.Status, "etag": resp.ETag}
	switch {
	case len(resp.Body) == 0:
		answer["body"] = nil
	case json.Valid(resp.Body):
		answer["body"] = json.RawMessage(resp.Body)
	case utf8.Valid(resp.Body):
		answer["body"] = string(resp.Body)
	default:
		answer["body_base64"] = []byte(resp.Body) // a []byte, not the json.RawMessage, which encoding/json writes in base64
	}
	text, _ := json.Marshal(answer) // values that encode: a number, strings, valid JSON, bytes
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "structuredContent": answer,
		"isError": !resp.OK()}
}
