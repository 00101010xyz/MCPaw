# MCPaw — Architecture

> **MCPaw** turns ordinary HTTP/REST APIs into [Model Context Protocol](https://modelcontextprotocol.io)
> servers. You describe an API once (declaratively, or by importing an OpenAPI document), configure an
> instance of it through a web UI, and MCPaw exposes it at a stable MCP endpoint that any MCP client
> can connect to. The first shipped connector targets the **Zotero Desktop local API**.

---

## 1. Problem statement and design goals

Every team that wants an LLM to talk to an internal or local API today writes a bespoke MCP server:
a small program that re-implements auth, retries, schema validation, and error mapping. That work is
almost entirely mechanical and almost entirely duplicated.

MCPaw replaces the bespoke program with **data**. An API becomes a *connector manifest*; a configured
deployment of that manifest becomes an *instance*; an instance is served over MCP.

| Goal | Consequence for the design |
| --- | --- |
| **Zero-code onboarding of an API** | Declarative connector manifests + OpenAPI import; no plugin ABI, no user-supplied code execution. |
| **Security over speed** | Encrypted secrets at rest, SSRF guard with DNS-rebinding protection, deny-by-default egress, strict input validation, CSRF + session hardening, audit trail. |
| **Operable as one container** | Single static Go binary, embedded SQLite, embedded UI assets, no sidecars. |
| **Maintainable** | Hexagonal layering, dependency injection by explicit constructor wiring, repository interfaces, no global mutable state. |
| **Scalable when it needs to be** | Stateless request path, per-instance concurrency/rate/breaker controls, storage behind an interface so SQLite → Postgres is a swap, not a rewrite. |

### Explicit non-goals

* Not an API gateway for browsers or general traffic — MCPaw only speaks MCP inbound.
* Not a scripting host. Manifests are data. There is no `eval`, no Lua, no WASM plugin surface.
* Not multi-tenant SaaS in v1. It is a single-team appliance with role-based admin accounts.

---

## 2. Domain model

```
Connector  ──(1:N)──►  Instance  ──(1:N)──►  Token
   │                      │
   │                      ├── InstanceSecret   (encrypted, write-only from the UI)
   │                      ├── InstanceVariable (plain config, e.g. Zotero userId)
   └── Tool[]             └── ToolBinding      (per-instance enable/disable + overrides)
```

* **Connector** — an immutable, versioned *description* of an API: base URL, auth scheme, declared
  variables and secrets, and a list of tools. Ships built-in (embedded in the binary) or is imported
  by an admin (manifest upload / OpenAPI import).
* **Tool** — one MCP tool: a JSON Schema for its input, a request template, and a response mapping.
* **Instance** — a connector bound to a concrete deployment: resolved base URL, variable values,
  encrypted secrets, egress policy, limits, and which tools are enabled. Served at `/mcp/{slug}`.
* **Token** — a bearer credential an MCP client presents. Scoped to one instance or to all; stored
  only as a SHA-256 hash.
* **AuditEvent** — an append-only record of every administrative and security-relevant action.

---

## 3. Component architecture

```
                    ┌──────────────────────── MCPaw container ───────────────────────┐
                    │                                                                │
  MCP client        │   ┌──────────────┐        ┌───────────────────────────────┐    │
 (Claude, IDE, …) ──┼──►│  MCP         │        │  Application services          │   │
   Bearer token     │   │  transport   │───────►│  (instance, connector, token,  │   │
                    │   │ (Streamable  │        │   user, audit)                 │   │
                    │   │  HTTP + SSE) │        └───────────────┬───────────────┘    │
                    │   └──────────────┘                        │                    │
                    │          │                                │                    │
  Admin browser     │   ┌──────▼───────┐                ┌───────▼────────┐           │
 ─────────────────┼─┼──►│  Web UI +    │───────────────►│  Execution      │          │
  Session cookie   │   │  Admin API   │                │  engine         │          │
                    │   └──────────────┘                └───────┬────────┘           │
                    │          │                                │                    │
                    │   ┌──────▼──────────────────────────┐     │                    │
                    │   │ Middleware chain                │     │                    │
                    │   │ reqID▸recover▸secHdrs▸log▸authN │     │                    │
                    │   │ ▸authZ▸CSRF▸rate-limit          │     │                    │
                    │   └─────────────────────────────────┘     │                    │
                    │                                           │                    │
                    │   ┌────────────┐   ┌──────────┐   ┌───────▼─────────┐          │
                    │   │  Store     │   │ Secrets  │   │ Safe HTTP       │          │
                    │   │ (SQLite)   │   │ keyring  │   │ egress client   │          │
                    │   └────────────┘   └──────────┘   └───────┬─────────┘          │
                    └───────────────────────────────────────────┼────────────────────┘
                                                                │ SSRF guard,
                                                                │ breaker, limits
                                                                ▼
                                                     Upstream API (e.g. Zotero
                                                     http://host.docker.internal:23119)
```

### 3.1 Layering (hexagonal / ports-and-adapters)

| Layer | Packages | Rule |
| --- | --- | --- |
| **Domain** | `internal/domain` | Pure entities, value objects, sentinel errors. Imports nothing from the project. |
| **Ports** | `internal/store` (repository interfaces), `internal/upstream` (Doer) | Interfaces only, expressed in domain terms. |
| **Application** | `internal/service`, `internal/engine`, `internal/mcp` | Use cases and protocol logic. Depends on ports, never on adapters. |
| **Adapters** | `internal/store/sqlitestore`, `internal/httpapi`, `internal/webui`, `internal/upstream` | Concrete I/O. Swappable. |
| **Composition root** | `cmd/mcpaw` | The *only* place that constructs concrete types and wires them together. |

**Dependency injection** is explicit constructor injection — every component receives its
collaborators as interfaces at construction time. There is no reflection-based container and no
service locator, so the full dependency graph is readable in one file (`cmd/mcpaw/main.go`) and every
component is trivially testable with a fake.

---

## 4. Data flow

### 4.1 Tool invocation (the hot path)

```
1. POST /mcp/{slug}                     Authorization: Bearer mcpaw_…
2. authN     token → SHA-256 → lookup → not revoked, not expired, scope covers {slug}
3. authZ     instance enabled? tool enabled on this instance?
4. limits    per-token + per-instance token bucket; per-instance concurrency semaphore
5. protocol  JSON-RPC 2.0 decode → method dispatch (initialize | tools/list | tools/call | ping)
6. validate  tools/call arguments validated against the tool's compiled JSON Schema
7. render    request template resolved from {input, vars, secrets} with context-aware escaping
8. egress    SSRF guard → circuit breaker → HTTP call with timeout + response size cap
9. map       status → success/error, optional JSON projection, header allowlist
10. reply    MCP content blocks (+ structuredContent when an outputSchema is declared)
11. audit    outcome, latency, upstream status — never arguments containing secrets
```

Steps 2–4 are middleware; 5–10 live in `internal/mcp` and `internal/engine`. **Secrets are only
decrypted at step 7 and never leave the process** — they are not logged, not echoed in errors, and
not returned by the admin API.

### 4.2 Configuration flow

```
Admin browser ──session+CSRF──► Admin API ──► InstanceService ──► Store (SQLite)
                                                   │
                                                   ├──► Sealer (AES-256-GCM) for secrets
                                                   └──► Registry cache invalidation
```

Instances are cached in memory as compiled objects (schemas compiled once, templates parsed once)
and invalidated by version counter on write, so the hot path never re-parses a manifest.

---

## 5. The connector manifest

A manifest is declarative YAML. The interesting design decision is the **template language**: rather
than embedding `text/template` (Turing-adjacent, easy to misuse, hard to escape correctly), MCPaw
uses a deliberately tiny resolver:

```
{{input.query}}        {{vars.userId}}        {{secrets.apiKey}}       {{input.limit|default:25}}
```

* Only dotted lookups into three sealed namespaces. No function calls, no iteration, no arithmetic.
* **Escaping is decided by the sink, not the author**: values interpolated into a path are
  path-segment escaped, values into a query are URL-encoded by `url.Values`, values into a JSON body
  are marshalled as JSON. A manifest author cannot construct an injection by forgetting a filter.
* An unresolved optional value removes its query parameter / body field entirely rather than sending
  an empty string.

Example (abridged, from the shipped Zotero connector):

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

**OpenAPI import** is a *translator into this format*, not a second runtime: an imported document is
converted to a manifest, shown to the admin for review, and stored. One execution path, one security
model.

---

## 6. Security considerations

Threat model: the adversary is (a) a malicious or compromised MCP client holding a token, (b) a
malicious tool *argument* supplied by an LLM that has been prompt-injected, (c) a hostile upstream
response, (d) an attacker on the network reaching the admin UI.

| Control | Mechanism |
| --- | --- |
| **Secrets at rest** | AES-256-GCM. Master key from `MCPAW_MASTER_KEY` (base64, 32 bytes) or a `0600` key file generated on first run. Per-purpose subkeys via HKDF. AAD binds ciphertext to `instanceID\|secretName`, so a stolen row cannot be replayed into another instance. |
| **Secrets in transit to the UI** | Write-only. The API returns `{"set": true}`, never the value. |
| **Admin authentication** | Argon2id (OWASP params: 19 MiB, t=2, p=1), constant-time verification, first-run setup flow that is disabled once an admin exists. |
| **Session security** | 256-bit opaque IDs stored hashed; `HttpOnly`, `SameSite=Lax`, `Secure` when TLS is fronted; idle *and* absolute expiry; ID rotation on privilege change; server-side revocation. |
| **CSRF** | Per-session token, required on every non-safe cookie-authenticated request, compared in constant time. Bearer-authenticated MCP calls are exempt by construction (no ambient credential). |
| **MCP authentication** | 256-bit bearer tokens shown exactly once, stored as SHA-256, scoped per instance, revocable, with expiry and last-used tracking. |
| **SSRF** | Deny-by-default egress: loopback, RFC1918, link-local, CGNAT, ULA, multicast and unspecified ranges are blocked. Enabling them is a **per-instance, explicit, audited opt-in** (required for Zotero on the host). The check runs in the socket `Control` hook against the *actual* dialled IP, which defeats DNS rebinding; redirects are re-validated and capped. |
| **Injection** | Tool arguments are validated against a compiled JSON Schema *before* templating; templating escapes per sink (§5); the upstream URL is rebuilt from parsed components, never string-concatenated. |
| **Hostile upstream** | Response bodies are read through a hard byte cap, content types are checked, decompression bombs bounded, and upstream errors are normalised — never reflected verbatim into the MCP client. |
| **Resource exhaustion** | Per-token and per-instance token buckets, per-instance concurrency semaphore, per-request timeouts, server read/write/idle timeouts, request body size limits, and a circuit breaker that sheds load from a failing upstream. |
| **Browser hardening** | Strict CSP with per-request nonce (no inline handlers, no CDN), `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy`, HSTS when TLS is fronted. |
| **Auditability** | Append-only audit log of logins, config changes, token issuance/revocation, and egress-policy changes, with actor, IP, and outcome. |
| **Supply chain / runtime** | Four direct dependencies, all pure Go. `CGO_ENABLED=0` static binary on a distroless base, non-root UID, read-only root filesystem, dropped capabilities, `no-new-privileges`. |

### Defensive-programming posture

Fail closed at every boundary: unknown token → 401 without a timing side channel; unknown tool →
`-32602`; egress not explicitly permitted → refused; manifest that fails validation → rejected at
import, not at call time; every `context` carries a deadline; every goroutine has an owner and an
exit path; `panic` in a handler is recovered, audited, and returned as a generic 500.

---

## 7. Scalability decisions

The design deliberately starts as a **single-container appliance** and leaves exactly the seams
needed to grow:

1. **Stateless request path.** Everything a request needs is in the DB or the request itself. MCP
   sessions are advisory (the spec permits stateless servers), so replicas need no stickiness for
   tool calls.
2. **Storage behind an interface.** `store.Repositories` is the only door to persistence; the SQLite
   adapter is one implementation. Postgres becomes an adapter, not a refactor. SQLite runs in WAL
   mode with a single writer connection and a pool of readers — the correct configuration for an
   embedded workload rather than the default footgun.
3. **Bounded work, not unbounded queues.** Concurrency semaphores and token buckets shed load
   deterministically instead of accumulating latency.
4. **Cheap hot path.** JSON Schemas compile once; manifests parse once; instances are served from an
   in-memory cache keyed by a version counter.
5. **Horizontal scale caveat, documented rather than hidden.** Rate limiting and the breaker are
   in-process. Multi-replica deployments need a shared limiter (Redis) — the `Limiter` interface is
   already the injection point.
6. **Observability first.** Structured logs with request IDs, Prometheus metrics (request rate,
   latency histogram, upstream status, breaker state), `/healthz` and `/readyz` — so scaling
   decisions are driven by data.

---

## 8. Deployment topology

```
docker compose up  ──►  mcpaw:8080   ──►  /            web UI (admin)
                        volume /data       /api/v1/…    admin JSON API
                        (SQLite + key)     /mcp/{slug}  MCP Streamable HTTP endpoint
                                           /healthz /readyz /metrics
```

The Zotero case has one wrinkle worth stating plainly: the Zotero local API listens on
`127.0.0.1:23119` **on the host**, which is not the container's loopback. MCPaw therefore ships with
`host.docker.internal` as the connector default and requires the operator to consciously enable
private-network egress for that instance — the security control and the deployment reality are
surfaced together in the UI rather than silently defaulted.

---

## 9. Repository layout

```
cmd/mcpaw/                 composition root + CLI (serve, healthcheck, keygen)
internal/domain/           entities, value objects, sentinel errors
internal/store/            repository interfaces
internal/store/sqlitestore/ SQLite adapter + embedded migrations
internal/secrets/          keyring, AES-GCM sealer, argon2id, token hashing
internal/connector/        manifest schema, validation, registry, builtin manifests
internal/connector/openapi/ OpenAPI 3 → manifest translator
internal/engine/           template resolver, request builder, response mapper, executor
internal/upstream/         SSRF-guarded HTTP client, circuit breaker, rate limiter
internal/mcp/              JSON-RPC types, MCP server, Streamable HTTP transport
internal/service/          application services (use cases)
internal/httpapi/          admin REST handlers + middleware
internal/webui/            server-rendered templates and static assets
internal/platform/         config, logging, metrics, IDs
```
