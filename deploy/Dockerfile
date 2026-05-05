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

EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -q -O- http://localhost:8787/v1/health || exit 1

ENTRYPOINT ["/usr/local/bin/toolyard"]
CMD ["serve", "-addr", ":8787", "-data", "/var/lib/toolyard/data"]
