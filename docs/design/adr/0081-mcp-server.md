# ADR-0081: MCP server for agent hosts

**Status:** accepted (T102a, 2026-11-15; decided by default under ADR-0053 d,
the protocol versions decided by the coordinator on 2026-11-15, accepted
once `go-reviewer` had no blocking finding). It decides how an agent host
outside the platform reaches the API as tools, and records what T102a
built under "As built".

## Context

The product owner asked for MCP servers so that agents use the same
governed APIs (tracker: "MCP servers so agents use the same governed
APIs"; `plan-breadth-week3.md` §T102a). The platform already gives its own
agents the API as tools: `internal/apitools` builds one tool per contract
operation open to agents (none marked `x-agent: false`), with its
parameters and body as the input schema and `x-autonomy` deciding the
level that performs it, and records every call in the agent's session
(ADR-0036, 04 §4.13). A host such as a desktop assistant or an IDE speaks
the Model Context Protocol instead of the Anthropic SDK's tool runner.

The specification was supplied as files, since build lanes do not fetch
(the plan's question 8): `docs/design/specs/mcp/` holds the schemas of
2026-07-28 (the latest stable version, without the `initialize`
handshake: each request names its version and capabilities in `_meta`,
and `server/discover` is required) and 2025-11-25 (the last version with
the handshake), with a summary of what a tools server needs. Which
versions hosts speak today is not settled there.

## Decision

1. **`erp mcp`, over standard input and output.** The command line's
   `erp mcp` is the server: a host launches it as a subprocess with the
   agent's token in `ERP_AGENT_TOKEN` (the stdio transport takes its
   credentials from the environment, as the specification's "Auth" section
   says), and it reads newline-delimited JSON-RPC 2.0 messages from its
   input and writes its answers, and nothing else, to its output. There is
   no listener on the network, and no sign-in of its own: the token is the
   agent's, as for `erp agent`.
2. **The tools are apitools'.** The tools are every operation open to
   agents (`apitools.Agent`), narrowed to the agent's autonomy level as its
   own runs are (`apitools.Offered`), named by `operationId`, in the order
   of their names, with apitools' input schema and the hint `readOnlyHint`
   for an operation that changes no business data. An operation closed to
   agents is never listed, and a call to a name not listed (one closed to
   agents, one above the level, one that does not exist) is the protocol's
   "Unknown tool" (-32602). The list is fixed for the process.
3. **A call is the API's call, recorded.** `erp mcp` opens an agent
   session when it starts (`startAgentSession`; only agents may) and ends
   it when the host closes its input (completed) or the input fails
   (failed). Each call goes to the API with the token and is recorded in
   the session (`recordAgentToolCall`), so permissions, company access,
   autonomy and segregation of duties apply unchanged. The result is the
   API's answer: its status, ETag and body as structured content and the
   same JSON as text, a problem (4xx, 5xx) marked `isError` so the model
   can correct itself; a body that is not JSON is a string, or base64 when
   it is not UTF-8. A call the API cannot be reached for is `isError` with
   the reason; a call the session fails to record is an internal error
   (-32603), since the API may have acted.
4. **Malformed input changes nothing.** A line that is not JSON is -32700;
   a message that is not one JSON-RPC 2.0 request object (a batch, a null
   or fractional id) is -32600; parameters that are not an object, a call
   without a name, arguments that are not an object or that lack a
   required property are -32602. None of them reaches the API. A
   notification and a response from the host are not answered.
5. **Versions: 2026-07-28, and 2025-11-25 for hosts not on it yet**
   (coordinator, 2026-11-15). A request whose `_meta` names
   `io.modelcontextprotocol/protocolVersion` is served as 2026-07-28: the
   version must be 2026-07-28 (else -32022 with `supported` and
   `requested`), `clientCapabilities` must be there (else -32602), and the
   result carries `resultType` and `_meta.serverInfo`; `server/discover`
   names both versions and the tools capability; `tools/list` is one page
   (`ttlMs` 0, `cacheScope` private, since it follows the caller's level).
   A host on 2025-11-25 opens with `initialize`, answered with that
   version whatever version it asked for, and its requests without `_meta`
   are then served with 2025-11-25's results, `ping` among them. Before
   `initialize`, a request without `_meta` is -32602. The tools are the
   same set in both.

## Alternatives

- A server over HTTP with its own sign-in: a network listener and an
  authorization server for one more client; left for later (the plan's
  "Later").
- 2026-07-28 alone: a host still on the handshake would fail against it
  (the versioning page's compatibility matrix); 2025-11-25 alone: a host
  on the current version would.
- Tools of the operations the agent's permissions allow: the contract's
  `x-permission` is not the whole decision (company access, segregation of
  duties, an object's own rules), so the API decides each call and the
  list follows only what apitools decides, as for the platform's agents.
- A tool's result as the API's body alone: a problem's status would be
  lost, and the model could not tell an empty answer from a refusal.

## Consequences

- An agent's tool calls through a host are recorded and governed as the
  platform's own agents' are; its tokens and cost are not, since the host
  runs its model outside the cell's gateway (the session ends with no
  usage).
- The list of tools is long (every operation open to agents at the
  level): a host that wants fewer narrows it itself until tool lists by
  agent type are built.
- What hosts make of `readOnlyHint` and `isError` is theirs; the API stays
  the backstop.

## Defaults chosen (for the product owner's digest)

1. `erp mcp` over stdio with `ERP_AGENT_TOKEN`; no network listener.
2. Tools are every operation open to agents at the agent's level.
3. Serves MCP 2026-07-28 and, after `initialize`, 2025-11-25.
4. A session per process, ended when the host closes the input.

## As built

### T102a MCP server

- `internal/mcp`: `Server.Serve` (§1, §3 to §5); its row in
  `internal/layout` (it imports `apitools` alone).
- `internal/apitools`: `Agent` (every operation open to agents, in the
  order of their names), `Offered`, `Tool.Invoke` (call and record, which
  `Execute` now uses), `ErrNotRecorded`, `Tool.Reads`, `Tool.Required`.
- `cmd/erp`: `erp mcp [-purpose TEXT] [-model NAME]`; the session's model
  is the flag's (default `agent-host`), since the host's model is not
  known to the server.
- Walkthrough `step_mcp` (step 98): a scripted host, as the support agent
  of step 88 (level 1), discovers the server, lists the tools (none closed
  to agents, no write above its level, a proposal at it), calls
  `listConfigVersions` and gets the API's answer, is refused
  `createPrincipal` (closed to agents) as an unknown tool; a 2025-11-25
  host after `initialize` lists the same tools; the session holds the one
  call and ended completed.
- Decided by default while building: §3's body encodings, §4's checks of
  required arguments before the call, `ttlMs` 0, one page of tools, the
  session's purpose and model as flags.
- Not checked: a real agent host against `erp mcp`. None is available to
  the build; the messages follow the supplied schemas, and the tests and
  the walkthrough drive the server with scripted messages.
