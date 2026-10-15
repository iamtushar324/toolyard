# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# The syntax line (docker/dockerfile:1.7, pinned by digest) makes BuildKit
# use a Dockerfile frontend that knows HEALTHCHECK --start-interval, also
# when the builder's built-in frontend is older. BuildKit is required
# (the default since Docker 23).
#
# Multi-stage build for toolyard.
#
# Single Go binary; the dashboard is plain HTML/JS embedded via go:embed, so
# the image needs no Node toolchain. CGO is on (mattn/go-sqlite3), so each
# architecture is built natively: the publish workflow
# (.github/workflows/publish-image.yml) builds linux/amd64 and linux/arm64
# on their own runners and merges them into one multi-arch index.
#
# Both base images are pinned by digest (the multi-arch index digest, with
# the tag it was read from). Bump the Go image in lock-step with the `go`
# line in go.mod: CI fails when they differ, and GOTOOLCHAIN=local stops the
# build from downloading a different toolchain.

# golang:1.25.14-bookworm (index digest read 2026-10-15). Go 1.26 raises the
# code-mode worker's memory use past its 256 MiB cap on large results
# (TestWorkerDecodesLargeResultsAtDefaultCap), so the move to 1.26/1.27 waits for
# that to be measured and fixed.
FROM golang:1.25.14-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS build
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=1
WORKDIR /src

# Cache module downloads separately from the sources.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is stamped into the binary (`toolyard version`, /v1/health and
# toolyard_build_info). The publish workflow passes sha-<commit>.
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/toolyard ./cmd/gateway \
 && /out/toolyard version

# debian:bookworm-slim (index digest read 2026-10-15). Same glibc as the
# build stage, and it keeps a shell for debugging.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata tini \
 && rm -rf /var/lib/apt/lists/*

# A fixed numeric uid/gid, so orchestrators that require a non-root user
# (Kubernetes runAsNonRoot) can verify it, and secret files can be owned by
# it (Swarm secrets uid/gid 10001). The data directory exists and belongs to
# that user, so a fresh named volume mounted there is writable.
RUN groupadd --system --gid 10001 toolyard \
 && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/toolyard \
      --no-create-home --shell /usr/sbin/nologin toolyard \
 && install -d -o 10001 -g 10001 -m 0750 /var/lib/toolyard \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/toolyard/data

COPY --from=build /out/toolyard /usr/local/bin/toolyard
COPY LICENSE /usr/share/doc/toolyard/LICENSE

USER 10001:10001
WORKDIR /var/lib/toolyard

# The root filesystem can be read-only: toolyard writes only to the data
# directory (crash dumps, the model-price cache, temporary upload files on
# Postgres; the database itself on SQLite). Mount a volume there.
ENV TOOLYARD_DATA_DIR=/var/lib/toolyard/data \
    TOOLYARD_ADDR=:8787 \
    TOOLYARD_CREDENTIALS_DIR=/run/secrets
# Key files (identity.key, oauth.key, secrets.key, session.key), database-url,
# ops-token and an optional toolyard.env are read from TOOLYARD_CREDENTIALS_DIR
# (where Docker and Swarm mount secrets) before the data directory. See
# docs/self-hosting.md, "Keys and credentials" and "Containers".

# 8787: dashboard, /mcp and /v1/*. 9090: the ops listener (/metrics and the
# probes) when -ops-addr / TOOLYARD_OPS_ADDR enables it.
EXPOSE 8787 9090

# Liveness, not readiness: a database outage makes /v1/readyz fail, and a
# restart would not fix it. Route traffic on /v1/readyz in the load balancer
# instead. The image has no curl or wget, so the binary checks its own probe
# (exit 0 when the status matches; 3 s timeout). Engines older than
# Docker 25 ignore --start-interval at run time.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --start-interval=5s --retries=3 \
  CMD ["/usr/local/bin/toolyard", "healthcheck", "-url", "http://127.0.0.1:8787/v1/livez", "-expect", "live"]

# SIGTERM starts the drain (up to -drain-timeout, 60 s, plus ~10 s of
# bounded closes): give the container a stop grace period of 75 s.
STOPSIGNAL SIGTERM

# tini runs as PID 1: it forwards signals to toolyard and reaps any orphaned
# grandchildren (stdio upstreams such as npx or uv can leave some).
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/toolyard"]
CMD ["serve"]

ARG VERSION=dev
LABEL org.opencontainers.image.title="toolyard" \
      org.opencontainers.image.description="Self-hosted MCP gateway for tool access, bounded human permission and agent questions" \
      org.opencontainers.image.source="https://github.com/iamtushar324/toolyard" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
