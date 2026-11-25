FROM --platform=$BUILDPLATFORM docker.io/library/caddy:2.11.9-builder AS builder

ARG TARGETOS
ARG TARGETARCH
ARG CADDY_VERSION=v2.11.9
ARG PLUGINS=""

COPY . /src

RUN set -e; \
    with="--with github.com/m-vinc/caddy-bgp=/src"; \
    for plugin in $PLUGINS; do with="$with --with $plugin"; done; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build "$CADDY_VERSION" $with --output /out/caddy

FROM docker.io/library/caddy:2.11.9

COPY --from=builder /out/caddy /usr/bin/caddy
