# Cross-compiling multi-arch build.
#
# The builder runs on the NATIVE build platform (`--platform=$BUILDPLATFORM`)
# and cross-compiles to the requested target arch. Previously the builder ran
# under QEMU for linux/arm64, which emulated the *entire* Rust compile and took
# ~2h per release. Cross-compiling on the native runner brings arm64 back to
# minutes; only the tiny runtime layer (ca-certificates) still touches QEMU.
FROM --platform=$BUILDPLATFORM rust:1.98.1-bookworm@sha256:93ce27a88655056a51dbdd8f5f2d7ddc071c7b0070fb288a37b5a285fc83971e AS chef

# Provided automatically by buildx: amd64 | arm64. BUILDARCH is the native arch
# of the build host, so `$TARGETARCH != $BUILDARCH` ⇒ we are cross-compiling and
# need the target's cross toolchain (the native rustc image only ships its own).
ARG TARGETARCH
ARG BUILDARCH

WORKDIR /app

# Install the Rust target + (when cross-compiling) the target cross toolchain,
# and record the rustc target triple for the build step. crossbuild-essential-*
# = the target gcc/g++ AND the target libc dev headers (the bare cross gcc alone
# lacks sys/types.h etc., which broke aws-lc-sys's C build).
# cmake + clang/libclang are for btls-sys, the vendored BoringSSL behind wreq
# (the `impersonated` feature): it builds with CMake and generates its bindings
# with bindgen, which dlopens libclang at build time. Both arches need them;
# the aarch64 leg cross-compiles BoringSSL with the same host clang.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends cmake clang libclang-dev; \
    case "$TARGETARCH" in \
      amd64) RUST_TARGET=x86_64-unknown-linux-gnu; \
             if [ "$BUILDARCH" != "amd64" ]; then \
               apt-get install -y --no-install-recommends crossbuild-essential-amd64; \
             fi ;; \
      arm64) RUST_TARGET=aarch64-unknown-linux-gnu; \
             if [ "$BUILDARCH" != "arm64" ]; then \
               apt-get install -y --no-install-recommends crossbuild-essential-arm64; \
             fi ;; \
      *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    rm -rf /var/lib/apt/lists/*; \
    rustup target add "$RUST_TARGET"; \
    echo "$RUST_TARGET" > /rust_target

# cargo-chef compiles only the dependency graph, described by a recipe derived
# from the manifests and Cargo.lock, so dependencies land in their own image
# layer. That layer is reused (locally or from a registry build cache) until
# the recipe changes, and only the workspace crates recompile per commit.
RUN cargo install cargo-chef --locked --version 0.1.78

# Cross linker per target (each var is consulted only when building that target,
# and resolves to the native gcc when building that arch natively).
# btls-sys applies its own CMAKE_TOOLCHAIN_FILE for aarch64, which sets only
# CMAKE_SYSTEM_NAME/PROCESSOR and relies on environment variables for the
# compiler. Because a toolchain file is present the cmake crate does not
# inject CMAKE_C_COMPILER either, so without these CMake picks the HOST cc and
# the aarch64 link dies with "Relocations in generic ELF (EM: 62)". The names
# are target-suffixed, so the amd64 leg ignores them.
ENV CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc \
    CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=x86_64-linux-gnu-gcc \
    CC_aarch64_unknown_linux_gnu=aarch64-linux-gnu-gcc \
    CXX_aarch64_unknown_linux_gnu=aarch64-linux-gnu-g++

# The workspace release profile uses fat LTO + codegen-units=1, whose final
# link of crw-server (aws-lc-sys + the full dep graph) needs several GB and
# OOM-killed the docker build (see #90's 4 GB OOM warning). The container
# binary doesn't need max LTO, so use thin LTO across more codegen units —
# far lower peak memory and a faster link, negligible runtime difference.
# Set before cooking so dependencies are compiled with the final profile.
ENV CARGO_PROFILE_RELEASE_LTO=thin \
    CARGO_PROFILE_RELEASE_CODEGEN_UNITS=16

FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS builder
COPY --from=planner /app/recipe.json recipe.json
RUN set -eux; \
    RUST_TARGET="$(cat /rust_target)"; \
    cargo chef cook --locked --release --target "$RUST_TARGET" --recipe-path recipe.json \
      -p crw-server --features cdp,camofox,impersonated -p crw-mcp -p crw-cli

# Per-build identity is declared only now: these values change every commit,
# and declaring them earlier would invalidate the cached dependency layer.
ARG CRW_VERSION=1.5.0-local
ARG CRW_REVISION=unknown
ARG CRW_BUILD_DATE=unknown

# Embedded by rustc through `option_env!`; these identify the binary even when
# it is copied out of the container or the registry metadata is unavailable.
ENV CRW_BUILD_VERSION=$CRW_VERSION \
    CRW_BUILD_REVISION=$CRW_REVISION \
    CRW_BUILD_DATE=$CRW_BUILD_DATE

COPY . .

RUN set -eux; \
    RUST_TARGET="$(cat /rust_target)"; \
    cargo build --locked --release --target "$RUST_TARGET" \
      -p crw-server --features cdp,camofox,impersonated -p crw-mcp -p crw-cli; \
    mkdir -p /out; \
    cp "target/${RUST_TARGET}/release/crw" \
       "target/${RUST_TARGET}/release/crw-server" \
       "target/${RUST_TARGET}/release/crw-mcp" /out/

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

# curl is included for the Compose service healthcheck; the runtime otherwise
# remains minimal and does not expose any browser ports.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Declared after the package layer so per-build values don't invalidate it.
ARG CRW_VERSION=1.5.0-local
ARG CRW_REVISION=unknown
ARG CRW_BUILD_DATE=unknown
ARG CRW_SOURCE=https://git.firewire.cc/michael/fireghost

COPY --from=builder /out/crw /usr/local/bin/crw
COPY --from=builder /out/crw-server /usr/local/bin/crw-server
COPY --from=builder /out/crw-mcp /usr/local/bin/crw-mcp
COPY config.default.toml /app/config.default.toml
COPY config.docker.toml /app/config.docker.toml

WORKDIR /app

LABEL org.opencontainers.image.title="Fireghost CRW" \
      org.opencontainers.image.source="$CRW_SOURCE" \
      org.opencontainers.image.version="$CRW_VERSION" \
      org.opencontainers.image.revision="$CRW_REVISION" \
      org.opencontainers.image.created="$CRW_BUILD_DATE" \
      org.opencontainers.image.description="Firecrawl-compatible CRW server with Camofox rendering" \
      io.modelcontextprotocol.server.name="io.github.us/crw"

ENV CRW_BUILD_VERSION=$CRW_VERSION \
    CRW_BUILD_REVISION=$CRW_REVISION \
    CRW_BUILD_DATE=$CRW_BUILD_DATE

EXPOSE 3000

CMD ["crw-server"]
