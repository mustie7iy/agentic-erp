# Model Context Protocol: schema and the parts T102a needs

Supplied for T102a (`erp mcp`) because build lanes do not fetch (plan-breadth-week3.md,
question 8). Everything below was read from the files named in "Sources" on 2026-11-15;
anything those files do not settle is marked **unconfirmed**.

## Versions and sources

| File | Protocol version | Status (from the repository's releases) |
| --- | --- | --- |
| `2026-07-28/schema.json` | `2026-07-28` | Latest stable: release `2026-07-28`, published 2026-07-28, not a pre-release. |
| `2025-11-25/schema.json` | `2025-11-25` | Previous stable release; the last "legacy" (handshake-based) version. |

- Repository: <https://github.com/modelcontextprotocol/modelcontextprotocol>, files copied
  verbatim from `main` at commit `4414a49c82d7fcc5f2a2ed9ddfc76139a09c1789`
  (2026-11-15):
  - <https://github.com/modelcontextprotocol/modelcontextprotocol/blob/4414a49c82d7fcc5f2a2ed9ddfc76139a09c1789/schema/2026-07-28/schema.json>
    (sha256 `edbb197e…6314a`; last changed by commit `38353251`, 2026-11-06)
  - <https://github.com/modelcontextprotocol/modelcontextprotocol/blob/4414a49c82d7fcc5f2a2ed9ddfc76139a09c1789/schema/2025-11-25/schema.json>
    (sha256 `268a5f82…97e7`)
- Both files differ slightly from their release tags. In `2026-07-28` the only change is the
  description of `Tool.inputSchema`: a paragraph was added advising that type constraints be
  kept out of `oneOf` branches. In `2025-11-25`, `NumberSchema.default/minimum/maximum` became
  `number` instead of `integer`, and a task's `ttl` may now be `null`. Neither change touches a
  message T102a sends or receives.
- The specification pages are cited as `modelcontextprotocol.io/specification/<version>/<page>`.
  They are rendered from `docs/specification/<version>/<page>.mdx` in the same repository and
  commit, which is the text read here.
- The repository states that `schema.ts` is the source of truth and `schema.json` "is
  automatically generated from the TypeScript source of truth" (`/specification/2026-07-28/basic`,
  section "Schema"). The JSON keeps the TypeScript doc comments as `description` values.
- Licence: the task brief said the repository was MIT licensed. Its `LICENSE` file (copied
  here as `LICENSE`) says the project is moving from MIT to Apache-2.0: new specification
  contributions are under Apache-2.0, and older ones whose authors have not agreed to the
  change stay under MIT. Both licences allow this copy, provided the licence text goes with it.

## The decision this leaves for T102a (ADR-0081)

The latest stable version **removes the `initialize` handshake and `ping`**. The plan's
"`initialize`, `tools/list`, `tools/call`" (T102a acceptance, from memory) describes the
legacy versions. What each version requires of a server:

- **2026-07-28 ("modern")**: no handshake. Every request carries its protocol version and
  client capabilities in `params._meta`. The server MUST implement `server/discover`.
- **2025-11-25 and earlier ("legacy")**: `initialize`, then `notifications/initialized`, then
  normal requests. `ping` is available.
- **Dual-era**: a server MAY support both. It is served statelessly when a request carries the
  modern `_meta`, and with legacy semantics for the stdio process when it opens with
  `initialize` (`/specification/2026-07-28/basic/versioning`, "Backward Compatibility with
  Initialization-Based Versions").

**Unconfirmed:** which of these versions the agent hosts T102a targets speak today. Nothing
fetched here says. The compatibility matrix on the versioning page shows a legacy-only client
fails against a modern-only server, and a modern-only client fails against a legacy-only
server. Which versions to implement is ADR-0081's decision. Both schemas are here so either
choice can be built offline.

## stdio transport

Source: `/specification/2026-07-28/basic/transports/stdio`. The legacy rules are the same in
`/specification/2025-11-25/basic/transports`, section "stdio".

- The client launches the server as a subprocess. The server reads JSON-RPC messages from
  `stdin` and writes them to `stdout`.
- Each message is a single JSON-RPC request, notification or response. Messages are
  newline-delimited and **MUST NOT** contain embedded newlines. In Go, `json.Marshal` output
  plus `\n` meets this, because it escapes newlines inside strings.
- The server **MUST NOT** write anything to `stdout` that is not a valid MCP message. It
  **MAY** write UTF-8 logs to `stderr`, and the client **SHOULD NOT** assume `stderr` output
  means an error. So all logging in `erp mcp` goes to `stderr`.
- 2026-07-28 only: the server **MUST NOT** write JSON-RPC *requests* to `stdout`. It writes
  only responses and notifications.
- Shutdown: the client closes the server's `stdin` and waits. The server **SHOULD** exit
  promptly on end-of-file on `stdin`. The server **MAY** shut down by closing its output and
  exiting.
- Cancellation: `notifications/cancelled` names the request's id. After that, the server
  **MUST NOT** send further messages for that request.
- Authorization: the authorization framework is for HTTP. An implementation using the stdio
  transport "**SHOULD NOT** follow this specification, and instead retrieve credentials from
  the environment" (`/specification/2026-07-28/basic`, section "Auth"). That matches T102a
  using the `erp` client's stored sign-in.

## Lifecycle and version negotiation

### 2026-07-28: per-request metadata, no handshake

Source: `/specification/2026-07-28/basic` (sections "Statelessness" and "`_meta`") and
`/specification/2026-07-28/basic/versioning`.

- Every client request **MUST** carry `_meta["io.modelcontextprotocol/protocolVersion"]` (for
  example `"2026-07-28"`) and `_meta["io.modelcontextprotocol/clientCapabilities"]`. It
  **SHOULD** carry `io.modelcontextprotocol/clientInfo`. A request missing a required field is
  malformed, and the server **MUST** reject it with `-32602` (Invalid params).
- If the server does not implement the requested version, it **MUST** answer with
  `UnsupportedProtocolVersionError`: code `-32022`, with `data.supported` (a string array) and
  `data.requested`.
- The server **MUST NOT** rely on earlier requests on the same connection for context. A stdio
  process is not a session.
- The server **SHOULD** put `_meta["io.modelcontextprotocol/serverInfo"]` (`name`, `version`)
  in every result.
- Every result **MUST** include `resultType`. It is `"complete"` for an ordinary result, and
  `"input_required"` for a multi-round-trip interim result, which T102a does not need.
- `server/discover` (`/specification/2026-07-28/server/discover`) **MUST** be implemented. Its
  result has `resultType`, `supportedVersions`, `capabilities`, the optional `instructions`,
  `_meta.serverInfo`, and the caching fields `ttlMs` and `cacheScope`
  (`/specification/2026-07-28/server/utilities/caching`).
- A modern-only server **SHOULD** name the versions it supports in the error it returns to an
  `initialize` request (versioning page).

### 2025-11-25: `initialize` handshake (legacy)

Source: `/specification/2025-11-25/basic/lifecycle`.

1. The client sends `initialize` with `params.protocolVersion`, `params.capabilities` and
   `params.clientInfo`. This **MUST** be the first interaction.
2. The server **MUST** reply with an `InitializeResult`: `protocolVersion`, `capabilities`,
   `serverInfo` (`name`, `version`, with optional `title` and others), and an optional
   `instructions`.
3. The client then sends the notification `notifications/initialized`.
4. Before the server has replied to `initialize`, the client **SHOULD NOT** send requests other
   than `ping`. Before the server receives `initialized`, it **SHOULD NOT** send requests other
   than `ping` and logging.
5. Version negotiation: the client sends the version it supports, which **SHOULD** be its
   latest. If the server supports that version, it **MUST** reply with the same version.
   Otherwise it **MUST** reply with another version it supports, which **SHOULD** be its latest.
   If the client does not support the reply's version, it **SHOULD** disconnect. The page's
   example of an initialization error uses `-32602` with `data.supported` and `data.requested`.

## Capabilities a tools server declares

Source: `/specification/2026-07-28/server/tools`, section "Capabilities", and
`ServerCapabilities` in both schemas.

- A server that supports tools **MUST** declare `"capabilities": {"tools": {}}`. The optional
  `"listChanged": true` means it will emit `notifications/tools/list_changed`. In 2026-07-28
  that notification is only sent on a `subscriptions/listen` stream. A server whose tools do
  not change during the process can leave `listChanged` out.
- Capabilities are returned in `server/discover` (2026-07-28) or in the `initialize` result
  (2025-11-25).
- 2026-07-28: the `tools/list` set **MUST NOT** vary per connection or as a side effect of
  other requests. It **MAY** vary by the authorization presented, "since credentials are
  per-request input". Tools **SHOULD** be returned in a deterministic order.

## tools/list

Sources: `/specification/2026-07-28/server/tools` ("Listing Tools") and
`/specification/2026-07-28/server/utilities/pagination`.

- Request: `{"method": "tools/list", "params": {"cursor": "…"}}`. `cursor` is optional and
  `params` also carries the `_meta` above.
- Result: `tools` (array of `Tool`) and the optional `nextCursor`. In 2026-07-28 the result
  also needs `resultType` and the required `ttlMs` (an integer of 0 or more, in milliseconds)
  and `cacheScope` (`"public"` or `"private"`). `"private"` suits a list that varies per caller:
  caches **MUST NOT** be shared across authorization contexts. 2025-11-25's `ListToolsResult`
  has no caching fields.
- Pagination: the cursor is an opaque string, and the page size is the server's choice. A
  missing `nextCursor` ends the list. An empty-string cursor is valid and does not end the
  list. Servers **SHOULD** provide stable cursors. An invalid cursor **SHOULD** get error
  `-32602`.

### Tool shape

Source: section "Data Types / Tool" on the tools page, and `Tool` and `ToolAnnotations` in
`schema.json`.

| Field | Required | Notes |
| --- | --- | --- |
| `name` | yes | **SHOULD** be 1–128 characters from `A-Z a-z 0-9 _ - .`, case-sensitive, and unique within the server. |
| `title` | no | Display name. Display precedence is `title`, then `annotations.title`, then `name`. |
| `description` | no | Hint to the model. |
| `inputSchema` | yes | A JSON Schema object with `"type": "object"` at the root, never `null`. Without `$schema`, the dialect is JSON Schema 2020-12. 2026-07-28 allows any 2020-12 keyword. In 2025-11-25 the TypeScript type is `{type: "object", properties?, required?}`. For no parameters, `{"type": "object", "additionalProperties": false}` is recommended. |
| `outputSchema` | no | If present, `structuredContent` **MUST** conform to it. |
| `annotations` | no | `ToolAnnotations`, all hints: `title`; `readOnlyHint` (default false); `destructiveHint` (default true, meaningful only when not read-only); `idempotentHint` (default false, meaningful only when not read-only); `openWorldHint` (default true). Clients **MUST** treat annotations from untrusted servers as untrusted. |
| `icons`, `_meta` | no | Not needed by T102a. |

## tools/call and CallToolResult

Sources: tools page ("Calling Tools", "Tool Result", "Error Handling") and `CallToolResult` in
`schema.json`.

- Request: `{"method": "tools/call", "params": {"name": "…", "arguments": {…}}}`. `arguments`
  is an optional object.
- Result (`CallToolResult`):
  - `content`: required array of content blocks. A text block is
    `{"type": "text", "text": "…"}`.
  - `structuredContent`: optional. In 2026-07-28 it may be any JSON value. In 2025-11-25 it
    must be an object.
  - `isError`: optional, and false when absent.
  - In 2026-07-28 the result also has `resultType: "complete"`.
- A tool returning `structuredContent` **SHOULD** also return the serialized JSON in a text
  block, for backward compatibility.

## Errors: protocol error or `isError`

Sources: `/specification/2026-07-28/basic` ("Error Codes"), the tools page ("Error Handling"),
and the `CallToolResult.isError` description in `schema.json`.

JSON-RPC codes in the schema:

| Code | Name |
| --- | --- |
| -32700 | Parse error |
| -32600 | Invalid request |
| -32601 | Method not found |
| -32602 | Invalid params |
| -32603 | Internal error |
| -32020 | HeaderMismatch (HTTP only; 2026-07-28) |
| -32021 | MissingRequiredClientCapability (2026-07-28) |
| -32022 | UnsupportedProtocolVersion (2026-07-28) |

- In 2026-07-28, `-32000` to `-32019` is legacy: new implementations **SHOULD NOT** use it.
  `-32020` to `-32099` is reserved for the specification. Application-defined codes
  **SHOULD** be outside `-32768` to `-32000`.
- An error response carries the request's `id`. The id may be absent only when the request's
  id could not be read because the message was malformed.
- A request id **MUST** be a string or integer, never `null`. A notification **MUST NOT** have
  an id, and the receiver **MUST NOT** answer it.
- **Protocol error** (a JSON-RPC `error`): an unknown tool, a request that does not satisfy the
  `CallToolRequest` schema, or a server error. The page's example of an unknown tool uses
  `-32602`, with the message `Unknown tool: <name>`.
- **Tool execution error** (a result with `isError: true` and the explanation in `content`):
  API failures, input validation errors (for example a date in the wrong format), and business
  logic errors. The schema says errors that originate from the tool "SHOULD be reported inside
  the result object … not as an MCP protocol-level error response", so the model can
  self-correct.
- How this maps to T102a, an inference to be confirmed in ADR-0081 and not a statement of the
  specification:
  - The API's problem responses (4xx and 5xx with `application/problem+json`) are tool
    execution errors: `isError: true`, with the problem in `content` and in
    `structuredContent`.
  - A name not in the caller's tool list (including `x-agent: false` operations) is "Unknown
    tool" (`-32602`).
  - Malformed JSON is `-32700`. A message that is not a valid request is `-32600`.

## ping

- **2025-11-25** (`/specification/2025-11-25/basic/utilities/ping`): either side may send
  `{"jsonrpc": "2.0", "id": …, "method": "ping"}` with no params. The receiver **MUST** respond
  promptly with an empty result, `{"jsonrpc": "2.0", "id": …, "result": {}}`.
- **2026-07-28**: `ping` is removed ("Key Changes", major change 5, in
  `/specification/2026-07-28/changelog`). A modern-only server treats `ping` as an unknown
  method (`-32601`). A dual-era server answers it within a legacy-initialized stdio process.
  The second sentence is an inference, not stated by the specification.
