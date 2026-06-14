# syntax=docker/dockerfile:1

# ---- Build stage -----------------------------------------------------------
# All non-GUI commands are pure Go (modernc.org/sqlite needs no CGO), so we
# build fully static binaries with CGO disabled.
FROM golang:1.26.3-alpine AS build

WORKDIR /src

# Mirror the Makefile build flags.
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath
ARG LDFLAGS="-s -w"

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the four non-GUI commands into /out.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    for cmd in quorum-server quorum-client quorum-admin quorum-gencert; do \
        go build -ldflags "${LDFLAGS}" -o "/out/${cmd}" "./cmd/${cmd}"; \
    done

# ---- Runtime stage ---------------------------------------------------------
FROM alpine:3.20

# TLS roots (the client/admin/server speak gRPC over TLS).
RUN apk add --no-cache ca-certificates

# Put every binary on PATH so `docker run quorum quorum-server ...` just works.
COPY --from=build /out/ /usr/local/bin/

# Working dir for generated certs and the SQLite database; mount a volume here
# to persist them across container restarts.
WORKDIR /data
VOLUME ["/data"]

# Default gRPC listen port used by quorum-server.
EXPOSE 8443

# Drop to a shell by default; run a specific binary with e.g.
#   docker run --rm quorum quorum-gencert -out certs
CMD ["sh"]
