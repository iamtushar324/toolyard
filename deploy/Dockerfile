# Multi-stage build for toolyard.
#
# Single Go binary; the React-style PWA is plain HTML/JS embedded via go:embed
# so we don't need a Node toolchain in the image.

FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache module deps separately from sources.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /out/toolyard ./cmd/gateway

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --system --uid 10001 --home /var/lib/toolyard --create-home toolyard

COPY --from=build /out/toolyard /usr/local/bin/toolyard

USER toolyard
WORKDIR /var/lib/toolyard
ENV TOOLYARD_DATA_DIR=/var/lib/toolyard/data
# Key files (identity.key, oauth.key, secrets.key, session.key) and an
# optional toolyard.env are read from Docker secrets here first, then from
# the data directory. See docs/self-hosting.md, "Keys and credentials".
ENV TOOLYARD_CREDENTIALS_DIR=/run/secrets

EXPOSE 8787
# The image has no curl or wget: the binary checks its own readiness probe
# (exit 0 when /v1/readyz reports "ready", 1 otherwise; 3 s timeout).
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/toolyard", "healthcheck", "-url", "http://127.0.0.1:8787/v1/readyz", "-expect", "ready"]

ENTRYPOINT ["/usr/local/bin/toolyard"]
CMD ["serve", "-addr", ":8787", "-data", "/var/lib/toolyard/data"]
