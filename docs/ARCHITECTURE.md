# MCPaw — Architecture

MCPaw exposes HTTP APIs as [Model Context Protocol](https://modelcontextprotocol.io) servers via
declarative connector manifests, configured through a web UI, packaged as one Docker container.
An API becomes a *connector manifest*; a configured deployment of that manifest becomes an
*instance*; an instance is served at `/mcp/{slug}`.

| Goal | Consequence for the design |
| --- | --- |
| **No code execution** | Connector manifests are declarative YAML — no plugin ABI, no `eval`, no scripting host. |
| **Security over speed** | Encrypted secrets at rest, SSRF guard with DNS-rebinding protection, deny-by-default egress, strict input validation, CSRF + session hardening, audit trail. |
| **Operable as one container** | Single static Go binary, embedded SQLite, embedded UI assets, no sidecars. |
| **Maintainable** | Hexagonal layering, explicit constructor wiring, repository interfaces, no global mutable state. |

---

## 1. Domain model

```
Connector  ──(1:N)──►  Instance  ──(1:N)──►  Token
   │                      │
   │                      ├── InstanceSecret   (encrypted, write-only from the UI)
   │                      ├── InstanceVariable (plain config, e.g. Zotero userId)
   └── Tool[]             └── ToolBinding      (per-instance enable/disable + overrides)
```

* **Connector** — an immutable, versioned description of an API: base URL, auth scheme, declared
  variables/secrets, and a list of tools. Ships built-in or is imported by an admin.
* **Tool** — one MCP tool: a JSON Schema for its input, a request template, a response mapping.
* **Instance** — a connector bound to a concrete deployment: resolved base URL, variable values,
  encrypted secrets, egress policy, limits, and which tools are enabled.
* **Token** — a bearer credential an MCP client presents, scoped to one instance or all, stored
  only as a SHA-256 hash.
* **AuditEvent** — an append-only record of every administrative and security-relevant action.

---

## 2. Layering (hexagonal / ports-and-adapters)

| Layer | Packages | Rule |
| --- | --- | --- |
| **Domain** | `internal/domain` | Pure entities, value objects, sentinel errors. Imports nothing from the project. |
| **Ports** | `internal/store`, `internal/upstream` | Interfaces only, expressed in domain terms. |
| **Application** | `internal/service`, `internal/engine`, `internal/mcp` | Use cases and protocol logic. Depends on ports, never on adapters. |
| **Adapters** | `internal/store/sqlitestore`, `internal/httpapi`, `internal/webui` | Concrete I/O. Swappable. |
| **Composition root** | `cmd/mcpaw` | The only place that constructs concrete types and wires them together. |

Every component receives its collaborators as interfaces at construction time — no reflection-based
container, no service locator. Two guard tests enforce this in CI: `internal/service/layering_test.go`
(no `internal/index/source/<connector>` import from `internal/service`) and `internal/archtest`
(no `sqlitestore` import outside `cmd/mcpaw`).

Indexable content sources (Zotero, Gitea, Linkding) each live in their own
`internal/index/source/<name>` subpackage behind a `Crawler` interface — `internal/service` knows
how to run any registered crawler generically, never the specifics of one connector.

---

## 3. Data flow

**Tool invocation (the hot path):** bearer token → SHA-256 lookup, scope check → per-token/instance
rate limit + concurrency gate → JSON-RPC dispatch → arguments validated against the tool's compiled
JSON Schema → request template resolved from `{input, vars, secrets}` with sink-aware escaping →
SSRF guard → circuit breaker → HTTP call with timeout and response-size cap → response mapped to MCP
content blocks → audited (outcome, latency, upstream status — never secrets or arguments).

Secrets are decrypted only at the templating step and never logged, echoed, or returned by the admin
API. Instances are cached in memory as compiled objects (schemas compiled once, templates parsed
once) and invalidated by a version counter on write.

---

## 4. The connector manifest

A manifest is declarative YAML with a deliberately tiny template resolver — not `text/template`,
which is Turing-adjacent and easy to misuse:

```
{{input.query}}        {{vars.userId}}        {{secrets.apiKey}}       {{input.limit|default:25}}
```

Only dotted lookups into three sealed namespaces (`input`, `vars`, `secrets`); no function calls, no
iteration, no arithmetic. Escaping is decided by the sink, not the author: a path segment is
path-escaped, a query value is URL-encoded, a JSON body value is marshalled as JSON — a manifest
author cannot construct an injection by forgetting a filter. An unresolved optional value removes its
field entirely rather than sending an empty string.

Abridged example (from the shipped Zotero connector):

```yaml
apiVersion: mcpaw.dev/v1
kind: Connector
metadata: { id: zotero-local, name: Zotero (Local API), version: 1.0.0 }
spec:
  baseUrl: { default: "http://host.docker.internal:23119", requiresPrivateNetwork: true }
  variables: [ { name: userId, default: "0", pattern: '^[0-9]+$' } ]
  tools:
    - name: zotero_search_items
      description: Full-text/metadata search across the local Zotero library.
      annotations: { readOnlyHint: true }
      inputSchema:
        type: object
        properties: { query: { type: string }, limit: { type: integer, maximum: 100 } }
      request:
        method: GET
        path: /api/users/{{vars.userId}}/items
        query: { q: "{{input.query}}", limit: "{{input.limit|default:25}}" }
      response:
        successCodes: [200]
        includeHeaders: [Total-Results, Link]
```

---

## 5. Security considerations

Threat model: (a) a malicious or compromised MCP client holding a token, (b) a malicious tool
*argument* from a prompt-injected LLM, (c) a hostile upstream response, (d) an attacker on the
network reaching the admin UI.

| Control | Mechanism |
| --- | --- |
| **Secrets at rest** | AES-256-GCM. Master key from `MCPAW_MASTER_KEY` or a `0600` key file generated on first run. AAD binds ciphertext to `instanceID\|secretName`, so a stolen row cannot be replayed into another instance. |
| **Secrets in transit to the UI** | Write-only — the API returns `{"set": true}`, never the value. |
| **Admin authentication** | Argon2id, constant-time verification, a first-run setup flow disabled once an admin exists. |
| **Session security** | 256-bit opaque IDs stored hashed; `HttpOnly`, `SameSite=Lax`, `Secure` when TLS is fronted; idle *and* absolute expiry; server-side revocation. |
| **CSRF** | Per-session token on every non-safe cookie-authenticated request. Bearer-authenticated MCP calls are exempt by construction (no ambient credential). |
| **MCP authentication** | 256-bit bearer tokens shown once, stored as SHA-256, scoped per instance, revocable. |
| **SSRF** | Deny-by-default egress (loopback, RFC1918, link-local, CGNAT, multicast). Enabling it is a per-instance, audited setting. The check runs against the *actual* dialled IP in the socket `Control` hook, defeating DNS rebinding; redirects are re-validated. |
| **Injection** | Arguments validated against a compiled JSON Schema before templating; templating escapes per sink (§4); URLs are rebuilt from parsed components, never string-concatenated. |
| **Hostile upstream** | Responses read through a hard byte cap; errors are normalised, never reflected verbatim into the MCP client. |
| **Resource exhaustion** | Per-token/instance rate limits, concurrency semaphore, timeouts, request body size limits, a circuit breaker that sheds load from a failing upstream. |
| **Browser hardening** | Strict CSP with a per-request nonce, `X-Content-Type-Options`, `X-Frame-Options: DENY`, HSTS when TLS is fronted. |
| **Auditability** | Append-only log of logins, config changes, token issuance/revocation, egress-policy changes. |

Fail closed at every boundary: unknown token → 401; unknown tool → `-32602`; egress not permitted →
refused; an invalid manifest is rejected at import, not at call time; a `panic` in a handler is
recovered, audited, and returned as a generic 500.

---

## 6. Deployment topology

```
docker compose up  ──►  mcpaw:8080   ──►  /            web UI (admin)
                        volume /data       /api/v1/…    admin JSON API
                        (SQLite + key)     /mcp/{slug}  MCP Streamable HTTP endpoint
                                           /healthz /readyz /metrics
```

The Zotero connector's local API listens on `127.0.0.1:23119` **on the host**, not the container's
own loopback, so MCPaw reaches it via `host.docker.internal` and needs private-network egress opened
for that instance. Zotero also validates the request's `Host` header as a DNS-rebinding defense,
accepting only `127.0.0.1`/`localhost`/`[::1]` — which `host.docker.internal` is not — so
`domain.Instance.HostHeaderOverride` exists to connect via one address while presenting another; the
web UI pre-fills it whenever a connector's default base URL points at `host.docker.internal`.

---

## 7. Repository layout

```
cmd/mcpaw/                  composition root + CLI (serve, healthcheck, keygen)
internal/domain/            entities, value objects, sentinel errors
internal/store/              repository interfaces
internal/store/sqlitestore/  SQLite adapter + embedded migrations
internal/secrets/            keyring, AES-GCM sealer, argon2id, token hashing
internal/connector/          manifest schema, validation, registry, builtin manifests
internal/connector/openapi/  OpenAPI 3 → manifest translator (not yet wired into the admin UI)
internal/index/              chunking, HTML stripping, embedder client (connector-agnostic)
internal/index/source/       one Crawler per connector (zotero, gitea, linkding)
internal/engine/             template resolver, request builder, response mapper, executor
internal/upstream/           SSRF-guarded HTTP client, circuit breaker, rate limiter
internal/mcp/                JSON-RPC types, MCP server, Streamable HTTP transport
internal/service/            application services (use cases)
internal/httpapi/            admin REST handlers + middleware
internal/webui/              server-rendered templates and static assets
internal/platform/           config, logging, metrics, IDs
```
