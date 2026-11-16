package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

// mcpRoutes are an agent's session at autonomy level 0 and one legal
// entity; ending the session answers end.
func mcpRoutes(end reply) routes {
	return routes{
		"POST /v1/agent-sessions":               {{201, `{"id": "s1", "autonomy_level": 0}`}},
		"POST /v1/agent-sessions/s1/tool-calls": {{201, `{}`}},
		"GET /v1/legal-entities":                {ok(`{"items": [{"code": "3000"}]}`)},
		"POST /v1/agent-sessions/s1:end":        {end},
	}
}

// TestMCPCommand (T102a): erp mcp opens a session with the agent's token,
// offers the tools of its level, answers its host on standard output and
// ends the session when the host closes its input.
func TestMCPCommand(t *testing.T) {
	const meta = `"_meta": {"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {}}`
	host := func(lines ...string) {
		in = strings.NewReader(strings.Join(lines, "\n"))
		t.Cleanup(func() { in = os.Stdin })
	}
	fakeAPI(t, mcpRoutes(ok(`{}`)))
	host(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {`+meta+`}}`,
		`{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "listLegalEntities", `+meta+`}}`,
		`{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "createLegalEntity", `+meta+`}}`)
	got := must(t, "mcp")
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("answers: got %d lines, want 3:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], `"name":"listLegalEntities"`) || strings.Contains(lines[0], `"name":"createLegalEntity"`) {
		t.Errorf("tools at level 0: got %.300s, want the reads alone", lines[0])
	}
	if !strings.Contains(lines[1], `\"code\":\"3000\"`) || !strings.Contains(lines[1], `"isError":false`) {
		t.Errorf("call: got %s", lines[1])
	}
	if !strings.Contains(lines[2], `"Unknown tool: createLegalEntity"`) {
		t.Errorf("a write at level 0: got %s, want an unknown tool", lines[2])
	}
	// The session not ending fails the command.
	fakeAPI(t, mcpRoutes(reply{409, `{"code": "agents.session_ended"}`}))
	host()
	fails(t, "end session", "mcp")
	// A host whose input fails ends the session as failed.
	fakeAPI(t, mcpRoutes(ok(`{}`)))
	in = iotest.ErrReader(errors.New("input broken"))
	fails(t, "input broken", "mcp")
	fails(t, "flag provided", "mcp", "-x")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "mcp")
	fakeAPI(t, routes{})
	fails(t, "start session", "mcp")
}
