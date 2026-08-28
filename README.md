# MCPaw

MCPaw runs a [Model Context Protocol](https://modelcontextprotocol.io) server for the
**Zotero Desktop** local API, packaged as one Docker container with a small web UI for
configuring instances, tokens and tool access.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for how it's put together and
[`SECURITY.md`](SECURITY.md) to report a vulnerability.

## Quickstart

```sh
docker compose up --build
```

Then open <http://localhost:8080>. On first run you'll land on `/setup` to create the
administrator account — that form is only reachable until the first administrator exists,
after which it is closed permanently.

> **Upgrading from an image built before the `/data` ownership fix?** An earlier version
> of the `Dockerfile` didn't pre-create `/data` with the right ownership, so Docker
> materialized the named volume as `root`-owned and the container failed with
> `permission denied` writing `master.key`. Rebuilding the image alone won't fix an
> already-created volume — run `docker compose down -v` first (this deletes the named
> volume, so only do it if you have nothing in it worth keeping yet) and then
> `docker compose up --build` again.

Once signed in:

1. **Create an instance.** Pick the built-in **Zotero (Local API)** connector. The base
   URL defaults to `http://host.docker.internal:23119`, which is how a container reaches
   the Zotero desktop app's local API running on your machine (`compose.yml` already maps
   `host.docker.internal` for you on Linux; Docker Desktop on macOS/Windows resolves it
   automatically).
2. **Enable private-network egress** on the instance. MCPaw refuses to reach loopback and
   private-network addresses by default — that's intentional, not a bug, since Zotero's
   local API lives on your machine's loopback interface. Cloud instance-metadata addresses
   (`169.254.169.254` and friends) stay blocked regardless.
3. Leave the `userId` variable at its default (`0`) — that's what the local API always
   uses. No secret is required for the local API.
4. Leave **Host header override** at its pre-filled `127.0.0.1:23119` — **including the
   port**; `127.0.0.1` alone is a different Host header and Zotero will still reject it.
   Zotero's local API checks the request's `Host` header as a DNS-rebinding defense and
   only accepts `127.0.0.1`, `localhost`, or `[::1]` on the exact port it's listening on
   — rejecting anything else, including `host.docker.internal`, with `400 Bad Request`.
   This field lets MCPaw connect via `host.docker.internal` (the only address that
   actually reaches your host from inside the container) while still presenting a `Host`
   header Zotero accepts.
5. Click **Test connection** to confirm MCPaw can actually reach a running Zotero desktop
   app before handing the endpoint to a client.
6. **Issue a token** (Tokens → Issue a token), scoped to this one instance rather than to
   every instance.
7. Point your MCP client at `http://localhost:8080/mcp/<slug>` with
   `Authorization: Bearer <token>`.

**Seeing `the upstream API returned 400 Bad Request` on Test connection?** This is almost
always the Host header check above. Confirm with `curl -i http://127.0.0.1:23119/api/users/0/items?limit=1`
directly on the machine running Zotero — if that succeeds but MCPaw still 400s, check the
instance's **Host header override** field is set to `127.0.0.1:23119`.

## Running without Docker

```sh
go build -o mcpaw ./cmd/mcpaw
MCPAW_MASTER_KEY="$(./mcpaw keygen)" ./mcpaw serve
```

`mcpaw keygen` prints a fresh base64-encoded master key. If you omit `MCPAW_MASTER_KEY`,
MCPaw generates one on first boot and stores it as `master.key` (mode `0600`) under
`MCPAW_DATA_DIR`. **Back that file up** — every stored credential is encrypted with a key
derived from it, and losing it makes them permanently unrecoverable. Pinning the key
explicitly via the environment variable, in your own secret store, is the recommended path
for anything beyond local trial.

`mcpaw healthcheck` queries `/healthz` and exits non-zero on failure — it's what the
container's `HEALTHCHECK` shells out to, since the final image is distroless and has no
`curl`.

## Configuration

All configuration is read from the environment. Every variable is optional except where
noted; `mcpaw` refuses to start rather than run with an invalid combination.

| Variable | Default | Notes |
| --- | --- | --- |
| `MCPAW_ADDR` | `:8080` | HTTP listen address. |
| `MCPAW_DATA_DIR` | `/data` | Holds the SQLite database and the generated master key file. |
| `MCPAW_PUBLIC_URL` | *(request host)* | Externally reachable base URL, used to render the `/mcp/<slug>` endpoint shown in the UI. Set this behind a reverse proxy. |
| `MCPAW_MASTER_KEY` | *(generated)* | Base64 (std or raw-url) or hex, 32 bytes. See above. |
| `MCPAW_TRUST_PROXY_HEADERS` | `false` | Honour `X-Forwarded-For`/`X-Real-IP` for the audit log and login rate limiting. Only enable this behind a proxy you actually trust — otherwise a client can spoof its own address. |
| `MCPAW_SECURE_COOKIES` | auto | Forces the `Secure` cookie attribute. Auto-enabled when `MCPAW_PUBLIC_URL` is `https://`. |
| `MCPAW_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `MCPAW_LOG_FORMAT` | `json` | `json` \| `text`. |
| `MCPAW_MAX_REQUEST_BYTES` | `1048576` | Caps admin API and MCP request bodies. |
| `MCPAW_SESSION_IDLE_TIMEOUT` | `2h` | Web session idle window. |
| `MCPAW_SESSION_ABSOLUTE_TIMEOUT` | `24h` | Hard ceiling on a web session regardless of activity. |
| `MCPAW_LOGIN_RATE_LIMIT_PER_MIN` | `10` | Failed sign-in attempts allowed per client address per minute. |
| `MCPAW_METRICS_ENABLED` | `true` | Exposes Prometheus text format at `/metrics`. |

Per-instance settings (base URL, variables, secrets, egress policy, timeouts, rate limits,
which tools are enabled) are configured through the web UI, not the environment — they
belong to the database, not the process.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

`internal/webui` and `internal/httpapi` boot the real service layer against a temp-file
SQLite database and exercise it through actual HTTP requests — including the CSRF and
bearer-token-scoping behaviour — rather than mocking the stack away.

## Semantic search

An instance of the Zotero, Gitea or Linkding connector can index its content
— Zotero's PDF and snapshot attachments, a Gitea repository's markdown and
typst files, or a Linkding bookmark's archived HTML snapshot — and expose a
`semantic_search` tool that returns short, relevant excerpts instead of
whole documents. It needs a local embeddings sidecar
(e.g. [Ollama](https://ollama.com) running an embedding model such as
`nomic-embed-text`, exposed at `http://host.docker.internal:11434`) — set
that as the **embedder URL** in the instance's "Semantic search" panel (an
instance-level setting, separate from the connector's own configuration),
then use **Build index**. The tool is only advertised to clients once the
index holds at least one chunk; leaving the embedder URL empty leaves the
feature off entirely, with no other effect on the instance.

Once an index exists, **Update index** re-fetches everything but only
re-embeds a document whose content actually changed, and removes any
document no longer found upstream — the routine action for keeping an index
current. **Rebuild from scratch** re-embeds every document regardless,
which is only needed after changing the embedder model (mixing vectors from
two models silently breaks search rather than erroring, so switching models
always requires a rebuild) or to recover from a suspect index.
