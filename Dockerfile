# syntax=docker/dockerfile:1
#
# Multi-stage build. The builder stage has the Go toolchain and nothing else
# that matters; the final stage is distroless — no shell, no package manager,
# no coreutils an attacker who reaches the container could use. `mcpaw
# healthcheck` (cmd/mcpaw/healthcheck.go) exists specifically because a
# distroless image has no curl for the HEALTHCHECK to shell out to.

FROM golang:1.24-bookworm AS builder
WORKDIR /src

# Dependency layers first so `docker build` reuses them until go.mod/go.sum
# actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mcpaw ./cmd/mcpaw

# distroless/static: no libc, no shell — matches CGO_ENABLED=0 and the
# cgo-free sqlite driver (modernc.org/sqlite) this build already depends on.
FROM gcr.io/distroless/static-debian12:nonroot AS final

WORKDIR /
COPY --from=builder /out/mcpaw /mcpaw

# The distroless nonroot image already runs as uid/gid 65532; /data is the
# one writable path the process needs (the SQLite database and, absent an
# explicit MCPAW_MASTER_KEY, the generated key file).
VOLUME ["/data"]
ENV MCPAW_DATA_DIR=/data
ENV MCPAW_ADDR=:8080

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/mcpaw", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/mcpaw"]
CMD ["serve"]
