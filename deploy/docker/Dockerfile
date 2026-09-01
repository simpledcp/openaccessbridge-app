# syntax=docker/dockerfile:1.7
#
# OpenAccessBridge container image: one image carrying all four binaries. The
# three supervised processes (buildplan §3.5) run as three containers from this
# one image — see deploy/docker/docker-compose.yml — so the privilege
# separation the systemd units enforce is enforced here too: only the firewall
# container is privileged, and core/media run unprivileged from a read-only
# root filesystem.
#
# Build through `make docker-build`, not `docker build` directly. The Makefile
# stamps VERSION and BUILD_NUMBER from git; a binary built without a build
# number refuses to judge an update rather than guessing (D32), and this
# Dockerfile keeps that failure loud rather than defaulting it away.

ARG GO_IMAGE=golang:1.26-bookworm
ARG BASE_IMAGE=ubuntu:24.04

# ---------------------------------------------------------------------------
# Build stage: pure Go, no cgo, statically linked — the same flags `make build`
# uses, so the image's binaries are the native install's binaries.
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build

ARG VERSION
ARG BUILD_NUMBER

# GOTOOLCHAIN=local: never fetch a different toolchain over the network; the
# image's Go must satisfy go.mod on its own (DEVELOPMENT.md "Offline").
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOFLAGS=-trimpath

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    # A positive integer, or nothing: "0", "", "abc" all become "build number
    # unknown" in buildinfo.BuildNumber, an image that cannot judge an update.
    case "${BUILD_NUMBER:-}" in ''|0|*[!0-9]*) \
      echo >&2 "BUILD_NUMBER must be a positive integer (got '${BUILD_NUMBER:-}'); use 'make docker-build':"; \
      echo >&2 "  a binary without a build number refuses to judge updates (D32, buildinfo.BuildNumber)"; \
      exit 1;; \
    esac; \
    if [ -z "${VERSION:-}" ]; then echo >&2 "VERSION build arg is required (use 'make docker-build')"; exit 1; fi; \
    ldflags="-X github.com/simpledcp/openaccessbridge/internal/buildinfo.Version=${VERSION}"; \
    ldflags="${ldflags} -X github.com/simpledcp/openaccessbridge/internal/buildinfo.Build=${BUILD_NUMBER}"; \
    mkdir -p /out; \
    for bin in openaccessbridge-core openaccessbridge-media openaccessbridge-firewall acsp-simulator; do \
      go build -ldflags "${ldflags}" -o "/out/${bin}" "./cmd/${bin}"; \
    done

# ---------------------------------------------------------------------------
# Runtime stage. Ubuntu 24.04 is the base the appliance was verified on
# (GStreamer 1.24, nftables 1.0.9 — STATUS.md, BLOCKERS B4).
# ---------------------------------------------------------------------------
FROM ${BASE_IMAGE} AS runtime

ARG VERSION
ARG BUILD_NUMBER

# gstreamer1.0-tools:        gst-launch-1.0 / gst-inspect-1.0 (the Opus encoder is a gst-launch subprocess, D20)
# gstreamer1.0-plugins-base: opusenc, audiotestsrc, audioconvert, audioresample, rawaudioparse
# gstreamer1.0-plugins-good: level, rtpopuspay, udpsink (and most of the image's size — its dependency chain)
# nftables:                  nft, applied AND read back by the firewall helper (§9.1, §9.3)
# ca-certificates:           core fetches http(s) caption locators with the system trust store
#                            (internal/caption/resolver); the native install has the host's
# libcap2-bin:               setcap, below
# No gstreamer1.0-alsa: nothing in this build opens a capture device (B3); it
# joins the image with the capture backend that needs it.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        gstreamer1.0-tools \
        gstreamer1.0-plugins-base \
        gstreamer1.0-plugins-good \
        nftables \
        ca-certificates \
        libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    # Every element the encoder and its smoke test build a pipeline from must be
    # here, or the build fails now rather than the first show failing later.
    && for e in fdsrc rawaudioparse audioconvert audioresample capsfilter opusenc rtpopuspay udpsink audiotestsrc fakesink level; do \
         gst-inspect-1.0 --exists "$e" || { echo >&2 "GStreamer element '$e' is missing from the runtime image"; exit 1; }; \
       done \
    # The base image ships a spare interactive account (uid 1000); nothing here uses one.
    && userdel -r ubuntu

# The unprivileged service account, with a FIXED uid/gid so that volume
# ownership and the SO_PEERCRED admission checks (§3.5) line up across the
# three containers regardless of the host's own account database. `audio` is
# for /dev/snd on a real appliance (deploy/systemd/openaccessbridge-media.service).
ARG OAB_UID=1901
ARG OAB_GID=1901
RUN groupadd --system --gid "${OAB_GID}" openaccessbridge \
    && useradd --system --uid "${OAB_UID}" --gid openaccessbridge --groups audio \
        --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin openaccessbridge \
    # Only the SQLite CONFIG store is persistent (configuration only — never content).
    && install -d -m 0700 -o openaccessbridge -g openaccessbridge /var/lib/openaccessbridge \
    # The shared loopback-socket directory, in the posture the compose files
    # mount a tmpfs over it with: root's, group-writable for the service
    # account (so core and media can create their socket and the debug-capture
    # sink), sticky (so they cannot remove the root helper's socket). The
    # processes leave a directory they did not create exactly as provisioned
    # (internal/ipc prepareSocketDir). Never content on disk (§9.4).
    && install -d -m 1770 -o root -g openaccessbridge /run/openaccessbridge

# Installed and given its file capability in ONE layer: setcap on a file from
# an earlier layer would carry a second copy of the 20 MB binary.
#
# core binds the patron surface on :80 as the service account. The unit gives
# it AmbientCapabilities=CAP_NET_BIND_SERVICE; a container runtime hands a
# non-root user no ambient set, so a `cap_add` alone is lost at exec and the
# bind fails on the host network (where ports below 1024 are still
# privileged). A file capability on the binary is what survives: with
# `cap_add: [NET_BIND_SERVICE]` keeping it in the bounding set, this is the
# whole of what core can do that the service account cannot.
RUN --mount=type=bind,from=build,source=/out,target=/mnt/out \
    install -m0755 /mnt/out/* /usr/local/bin/ \
    && setcap cap_net_bind_service=+ep /usr/local/bin/openaccessbridge-core
COPY --chmod=0755 deploy/docker/openaccessbridge-demo /usr/local/bin/openaccessbridge-demo

ENV OPENACCESSBRIDGE_STATE_DIR=/var/lib/openaccessbridge \
    OPENACCESSBRIDGE_RUNTIME_DIR=/run/openaccessbridge

LABEL org.opencontainers.image.title="OpenAccessBridge" \
      org.opencontainers.image.description="Cinema accessibility appliance: ST 430-10/-11 captions and HI/VI-N audio to patron phones, with the theater and patron networks never joined" \
      org.opencontainers.image.source="https://github.com/simpledcp/openaccessbridge-app" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${BUILD_NUMBER}" \
      org.opencontainers.image.licenses="UNLICENSED"

# No ENTRYPOINT: each compose service names its own binary. The default is the
# core process; `openaccessbridge-demo` runs all three in one container.
CMD ["openaccessbridge-core"]
