# syntax=docker/dockerfile:1
#
# Multi-stage build. The builder stage has the Go toolchain and nothing else
# that matters; the final stage is distroless — no shell, no package manager,
# no coreutils an attacker who reaches the container could use. `mcpaw
# healthcheck` (cmd/mcpaw/healthcheck.go) exists specifically because a
# distroless image has no curl for the HEALTHCHECK to shell out to.

FROM golang:1.27-bookworm AS builder
WORKDIR /src

# Dependency layers first so `docker build` reuses them until go.mod/go.sum
# actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mcpaw ./cmd/mcpaw

# Pre-create /data owned by the distroless nonroot uid/gid here, in the builder
# stage — the final stage has no shell to run mkdir/chown in. Docker seeds a
# fresh named/anonymous volume from whatever the image already has at that
# mount point (content *and* ownership) the first time it's mounted; without
# this, the volume gets created root-owned and the nonroot process can't
# write master.key or the sqlite database into it.
RUN mkdir -p /data && chown -R 65532:65532 /data

# distroless/static: no libc, no shell — matches CGO_ENABLED=0 and the
# cgo-free sqlite driver (modernc.org/sqlite) this build already depends on.
FROM gcr.io/distroless/static-debian12:nonroot AS final

WORKDIR /
COPY --from=builder /out/mcpaw /mcpaw
COPY --from=builder --chown=65532:65532 /data /data

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
