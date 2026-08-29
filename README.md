# MCPaw

MCPaw runs a [Model Context Protocol](https://modelcontextprotocol.io) server for Zotero
Desktop, Gitea and Linkding, packaged as one Docker container with a small web UI for
configuring instances, tokens, tool access and semantic search.

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

Once signed in, **create an instance** (pick a built-in connector — Zotero, Gitea or
Linkding). The base URL, egress and Host header override are pre-filled with sensible
defaults for each; expand **Advanced** to review or change them. Creating an instance
already runs a connection test and, once an embedder is configured (see below), starts
indexing automatically — so the common path is: pick a connector, fill in what it asks
for, submit.

For Zotero specifically: leave the `userId` variable at its default (`0`), and leave
**Host header override** at its pre-filled `127.0.0.1:23119` including the port — Zotero's
local API checks the request's `Host` header as a DNS-rebinding defense and rejects
anything else, even `host.docker.internal`, with `400 Bad Request`. If Test connection
400s, confirm `curl -i http://127.0.0.1:23119/api/users/0/items?limit=1` works directly on
the machine running Zotero first.

Once the instance exists: **issue a token** (Tokens → Issue a token, scoped to this
instance) and point your MCP client at `http://localhost:8080/mcp/<slug>` with
`Authorization: Bearer <token>`.

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

A Zotero, Gitea or Linkding instance can index its content and expose a
`semantic_search` tool that returns short, relevant excerpts instead of whole
documents — advertised to clients as the first and preferred tool once it's
active.

Configure the embedder once, under **Search** (not per instance): point it at
a local sidecar such as [Ollama](https://ollama.com) — the form is pre-filled
with the common default, `http://host.docker.internal:11434` running
`nomic-embed-text`. Saving a URL there automatically starts indexing every
existing instance that doesn't have one yet, and every new instance from then
on; **Update index** and **Rebuild from scratch** on an instance's own page
are there for a manual re-run, not a required step. Results below a relevance
floor are dropped rather than returned as noise, and a run's stats line
reports documents with no extractable text or that failed to embed, so a
misconfigured embedder is visible on the page instead of silently indexing
nothing.
