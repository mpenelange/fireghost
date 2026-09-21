# syntax=docker/dockerfile:1.7
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS test
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
    -ldflags="-s -w -buildid= -X main.version=$VERSION" -o /out/router ./cmd/router

FROM alpine:3.22.6@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS runtime
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
