# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.8
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/platform-ipam ./cmd/platform-ipam

FROM alpine:3.22.1 AS release
# Open Container Initiative annotations, so a pulled image can be traced back
# to its source and licence without consulting the registry listing.
LABEL org.opencontainers.image.title="platform-ipam" \
      org.opencontainers.image.description="Policy-controlled IP address allocation for AWS networks, with NetBox as the inventory of record" \
      org.opencontainers.image.source="https://github.com/mischapogr/platform-ipam" \
      org.opencontainers.image.documentation="https://github.com/mischapogr/platform-ipam/blob/main/docs/README.md" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.authors="Mischa Pogrebnyak (https://github.com/mischapogr)" \
      org.opencontainers.image.vendor="Mischa Pogrebnyak"
RUN apk add --no-cache ca-certificates tzdata curl && addgroup -S -g 10001 ipam && adduser -S -D -H -u 10001 -G ipam ipam
COPY LICENSE NOTICE /usr/share/doc/platform-ipam/
COPY --from=build /out/platform-ipam /usr/local/bin/platform-ipam
COPY examples/config/pools.yaml /etc/platform-ipam/config/pools.yaml
USER 10001:10001
WORKDIR /app
ENTRYPOINT ["/usr/local/bin/platform-ipam"]
CMD ["api"]

FROM release AS development
