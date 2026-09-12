# syntax=docker/dockerfile:1.7
FROM golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c AS test
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./...

FROM test AS build
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -buildid=" -o /out/router ./cmd/router

FROM alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 AS runtime
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S -g 10001 router \
    && adduser -S -D -H -u 10001 -G router router \
    && mkdir -p /data/cache \
    && chown -R router:router /data
COPY --from=build --chown=router:router /out/router /usr/local/bin/router
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
ARG SOURCE=unknown
LABEL org.opencontainers.image.title="Fireghost Router" \
      org.opencontainers.image.description="Local-first Firecrawl-compatible router" \
      org.opencontainers.image.source=$SOURCE \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.created=$BUILD_DATE \
      org.opencontainers.image.licenses="NOASSERTION"
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/health"]
ENTRYPOINT ["/usr/local/bin/router"]
