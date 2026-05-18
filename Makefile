# Build targets for the toolyard gateway.
#
# Native build is direct (go build). Linux cross-build runs inside the
# golang Docker image so we don't have to install a cross-compiler
# toolchain on the Mac — toolyard depends on mattn/go-sqlite3 which
# requires CGO, and CGO cross-builds are a pain to set up otherwise.
#
# Usage:
#   make build           # native binary -> gateway-darwin (or -linux on Linux)
#   make linux           # linux/amd64   -> gateway-linux         (needs docker)
#   make linux-arm64     # linux/arm64   -> gateway-linux-arm64   (needs docker)
#   make test            # go test ./...
#   make clean           # remove built binaries
#
# After cross-build, deploy with:
#   scp gateway-linux user@server:/path/to/toolyard/
#   ssh user@server 'cd /path/to/toolyard && systemctl restart toolyard'

PKG := ./cmd/gateway
# Strip DWARF + symbol table so the binary is ~30% smaller. Panic
# traces still resolve because Go embeds func names regardless.
LDFLAGS := -s -w

# Pin to a Go version that matches go.mod. Bump in lock-step when you
# bump the go directive in go.mod or you'll hit "go directive too new".
GO_IMG := golang:1.25-bookworm

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

ifeq ($(UNAME_S),Darwin)
  NATIVE := gateway-darwin
else ifeq ($(UNAME_S),Linux)
  ifeq ($(UNAME_M),aarch64)
    NATIVE := gateway-linux-arm64
  else
    NATIVE := gateway-linux
  endif
else
  NATIVE := gateway
endif

.PHONY: build linux linux-arm64 all test clean

build:
	go build -ldflags="$(LDFLAGS)" -o $(NATIVE) $(PKG)

# Cross-build inside a Linux container. Mounts the repo as /src and uses
# a named volume for the module cache so repeat builds don't re-download
# every dep. apt-get only pulls gcc on the first run because it's
# layered into the same volume on subsequent rebuilds... actually it
# isn't (image layers are immutable), but gcc-12 is ~50 MB so it's not
# the bottleneck. The build itself is what's slow.
linux:
	docker run --rm \
	  -v "$(CURDIR)":/src \
	  -v toolyard-gomod-linux-amd64:/go/pkg/mod \
	  -w /src \
	  -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=1 \
	  $(GO_IMG) \
	  sh -c 'apt-get update -qq && apt-get install -y -qq gcc >/dev/null && \
	         go build -ldflags="$(LDFLAGS)" -o gateway-linux $(PKG)'

linux-arm64:
	docker run --rm \
	  -v "$(CURDIR)":/src \
	  -v toolyard-gomod-linux-arm64:/go/pkg/mod \
	  -w /src \
	  -e GOOS=linux -e GOARCH=arm64 -e CGO_ENABLED=1 \
	  -e CC=aarch64-linux-gnu-gcc \
	  $(GO_IMG) \
	  sh -c 'dpkg --add-architecture arm64 && apt-get update -qq && \
	         apt-get install -y -qq gcc-aarch64-linux-gnu libc6-dev-arm64-cross >/dev/null && \
	         go build -ldflags="$(LDFLAGS)" -o gateway-linux-arm64 $(PKG)'

all: build linux linux-arm64

test:
	go test ./... -count=1

clean:
	rm -f gateway-darwin gateway-linux gateway-linux-arm64
