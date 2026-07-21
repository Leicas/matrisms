# --- Builder stage (Debian bookworm) ---
FROM golang:1.25-bookworm AS builder

# Install build dependencies including libolm
RUN apt-get update -y \
    && apt-get install -y --no-install-recommends git ca-certificates build-essential libolm-dev \
    && rm -rf /var/lib/apt/lists/*

# TARGETARCH is provided by buildx for multi-platform builds. We use it to
# scope the Go build cache per-arch so cross-arch builds (linux/amd64 +
# linux/arm64 in release.yml) don't corrupt each other's compile artifacts.
# Single-arch builds (docker.yml) just get a stable, arch-specific cache key.
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
# BuildKit cache mount on the module cache: persists across builds via the
# GHA cache (cache-to: type=gha,mode=max in the workflows). go.mod/go.sum
# changes still bust the layer because the COPY above is what triggers
# re-execution; the mount only kicks in when this RUN actually runs.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Build with CGO to link against libolm. Cache mounts:
#   - /root/.cache/go-build: Go's compile cache; arch-scoped because compiled
#     object files are platform-specific (sharing across arm64+amd64 corrupts).
#   - /go/pkg/mod: module source cache; arch-agnostic.
# These persist across builds (combined with cache-to: type=gha,mode=max),
# turning cold ~5-8 min builds into ~30-90s incremental ones.
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 go build -o matrisms ./cmd/matrisms

# Prepare a data directory we can chown in final image via COPY --chown
RUN mkdir -p /runtime-data

# --- Runtime dependencies stage (Debian bookworm-slim) ---
# Stage layout: install libolm + ca-certs into a known prefix, then COPY that
# whole prefix into the distroless final stage. This avoids hardcoding the
# arch-specific multiarch path (x86_64-linux-gnu vs aarch64-linux-gnu) and
# lets buildx produce a working image for both linux/amd64 and linux/arm64.
FROM debian:bookworm-slim AS runtime-deps
RUN apt-get update -y \
    && apt-get install -y --no-install-recommends ca-certificates libolm3 tzdata \
    && rm -rf /var/lib/apt/lists/*

# Stage the libolm shared object under an arch-agnostic path so the COPY in
# the final stage doesn't need to know the target arch.
RUN mkdir -p /matrisms-runtime/lib /matrisms-runtime/etc/ssl/certs /matrisms-runtime/usr/share \
    && libolm_path="$(find /usr/lib -name 'libolm.so.3*' \( -type f -o -type l \) | head -n1)" \
    && test -n "$libolm_path" || (echo "libolm.so.3 not found under /usr/lib"; ls -lR /usr/lib | grep -i olm; exit 1) \
    && cp -L "$libolm_path" /matrisms-runtime/lib/libolm.so.3 \
    && cp /etc/ssl/certs/ca-certificates.crt /matrisms-runtime/etc/ssl/certs/ca-certificates.crt \
    && cp -r /usr/share/zoneinfo /matrisms-runtime/usr/share/zoneinfo

# --- Final minimal runtime (Distroless) ---
# Distroless base matching Debian 12
FROM gcr.io/distroless/cc-debian12:nonroot

# Copy the compiled binary
COPY --from=builder /build/matrisms /usr/bin/matrisms
# Copy required shared libraries and data from runtime-deps. We place libolm
# at /usr/lib (not /usr/lib/<arch>-linux-gnu) so the dynamic linker finds it
# regardless of architecture — distroless's /etc/ld.so.conf.d already
# searches /usr/lib.
COPY --from=runtime-deps /matrisms-runtime/lib/libolm.so.3 /usr/lib/libolm.so.3
COPY --from=runtime-deps /matrisms-runtime/etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=runtime-deps /matrisms-runtime/usr/share/zoneinfo /usr/share/zoneinfo

# Use a path owned by the nonroot user in distroless
WORKDIR /home/nonroot/app
# Ensure a writable data directory owned by the nonroot user exists
COPY --from=builder --chown=nonroot:nonroot /runtime-data /home/nonroot/app/data
# Expose a writable volume for data (mount a host volume here)
VOLUME ["/home/nonroot/app/data"]

EXPOSE 29331
ENTRYPOINT ["/usr/bin/matrisms"]
