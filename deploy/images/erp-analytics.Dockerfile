# The erp-analytics image (ADR-0044, ADR-0032): the one cgo binary, which
# links DuckDB, so ko (static Go only, no cgo cross-compilation) cannot
# build it. Built with docker buildx from the repository's root, by
# scripts/images.sh (`make images`), with the other four images.
#
# - Both bases are pinned by the digest of their multi-platform index, as
#   .ko.yaml pins ko's: a base that moves under a tag would change the
#   image without a change here.
# - The builder runs on the build machine's platform and cross-compiles
#   (no emulation): Debian 12's cross gcc for the other architecture, its
#   own gcc for the same one. DuckDB comes prebuilt and static from
#   duckdb-go-bindings (go.sum pins it); glibc stays dynamic, so the
#   runtime base is distroless cc (glibc, libgcc, libstdc++, CA
#   certificates, time zones, no shell, uid 65532). The base is Debian
#   13, the builder Debian 12 (ADR-0044 amendment of 2026-11-01): glibc,
#   libgcc_s and libstdc++ keep every older symbol version, so a binary
#   linked against Debian 12's runs on Debian 13's, never the reverse.
#   The builder may never be newer than the runtime base.
# - Reproducible: -trimpath, no build id, no VCS stamp (the revision is in
#   the signed provenance), stripped, and buildx dates every layer at
#   SOURCE_DATE_EPOCH (rewrite-timestamp), so the digest depends on the
#   source, this file and the builder's architecture.
# - The pod runs it as uid 65532 with a read-only root file system; the
#   verified-file cache is an emptyDir (ERP_ANALYTICS_CACHE).
ARG GO_IMAGE=docker.io/library/golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0
ARG BASE_IMAGE=gcr.io/distroless/cc-debian13:nonroot@sha256:08476c244888bfb425bae1ae4dd7c17f093e972aab8426883d97f3c6f280f7c1

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG BUILDARCH
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in amd64) triple=x86-64-linux-gnu ;; arm64) triple=aarch64-linux-gnu ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac; \
    if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      apt-get update; \
      apt-get install -y --no-install-recommends "gcc-$triple" "g++-$triple"; \
      rm -rf /var/lib/apt/lists/*; \
    fi
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
ARG ERP_VERSION=
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build set -eu; \
    case "$TARGETARCH" in amd64) cc=x86_64-linux-gnu ;; arm64) cc=aarch64-linux-gnu ;; esac; \
    if [ "$TARGETARCH" = "$BUILDARCH" ]; then export CC=gcc CXX=g++; else export CC="$cc-gcc" CXX="$cc-g++"; fi; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -extldflags=-Wl,--build-id=none -X github.com/mustie7iy/agentic-erp/internal/platform/buildinfo.version=${ERP_VERSION}" \
      -o /out/erp-analytics ./cmd/erp-analytics

FROM ${BASE_IMAGE}
COPY --from=build /out/erp-analytics /erp-analytics
USER 65532:65532
ENTRYPOINT ["/erp-analytics"]
