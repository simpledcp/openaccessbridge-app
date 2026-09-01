# OpenAccessBridge build system.
# Local Go toolchain is installed at /home/libserv-2/go-sdk on this dev machine;
# CI uses actions/setup-go. GOTOOLCHAIN=local avoids surprise network toolchain
# downloads on an offline appliance build host.

GO ?= go
GOTOOLCHAIN ?= local
export GOTOOLCHAIN

BIN := bin
BINARIES := openaccessbridge-core openaccessbridge-media openaccessbridge-firewall acsp-simulator
# BUILD_NUMBER is the monotonic ordering the §9.4 update path compares (D32). The
# commit count is monotonic on this repo's linear history; a release process with
# its own numbering overrides it. A binary built WITHOUT it refuses to judge an
# update rather than guessing — see buildinfo.BuildNumber.
BUILD_NUMBER ?= $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -X github.com/simpledcp/openaccessbridge/internal/buildinfo.Version=$(VERSION) \
           -X github.com/simpledcp/openaccessbridge/internal/buildinfo.Build=$(BUILD_NUMBER)

.PHONY: all build test vet race cover sim soak latency qr-verify clean tidy fmt check \
        docker-build docker-save docker-smoke docker-up docker-down docker-dev-up docker-dev-down

all: check build

build: $(BINARIES)

# Static and path-stripped, exactly as the container image builds them: the
# README's "no cgo, no system libraries" is a build setting here, not an
# observation. (The test suite is unaffected — the race detector needs cgo and
# keeps it.)
$(BINARIES): export CGO_ENABLED=0
$(BINARIES): stamp-check
	@mkdir -p $(BIN)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$@ ./cmd/$@

# A shallow clone counts ONE commit, which stamps a build number that is known
# and wrong — the failure D32's refusal exists to prevent, and one CI produced
# for a while. Refuse to stamp from one unless BUILD_NUMBER was given outright.
.PHONY: stamp-check
stamp-check:
	@if [ "$(origin BUILD_NUMBER)" = file ] && [ "$$(git rev-parse --is-shallow-repository 2>/dev/null)" = true ]; then \
	  echo "refusing to stamp a build number from a shallow clone (it would count 1 commit); run 'git fetch --unshallow' or pass BUILD_NUMBER=<n>" >&2; exit 1; fi

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.txt -covermode=atomic ./...
	$(GO) tool cover -func=coverage.txt | tail -1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@unformatted=$$(gofmt -l internal/ cmd/); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

tidy:
	$(GO) mod tidy

# check is what CI gates on: gofmt + vet + the full test suite (incl. the ACSP
# simulator scenario suite, the caption-path regression backbone — §11.1).
check: fmt-check vet test

# Long soak (§11.6): many show cycles with reconnects, language switches, and
# output-mode toggles, asserting that retention is FLAT and that caption content
# purges at every show boundary. `make check` already runs the short soak as part
# of the suite; this is the pre-pilot run.
SOAK_CYCLES ?= 2000
soak:
	OPENACCESSBRIDGE_SOAK_CYCLES=$(SOAK_CYCLES) $(GO) test ./internal/soak/ -run Soak -v -timeout 30m

# §11.4 latency pass. Reports the APPLIANCE-SIDE contributors separately —
# channel map, level analysis, Opus encode transit, WebRTC fan-out transit — as
# p50/p95 distributions over a long run.
#
# READ THE HEADER IT PRINTS. What comes out is a FLOOR for the appliance's own
# contribution, measured Go-peer-to-Go-peer on one machine. It is NOT a
# glass-to-ear figure and must not be compared to §6.7's 100-150 ms HI target:
# the dominant term of that budget is the phone-side jitter buffer (40-300+ ms)
# and there is no phone and no browser here (BLOCKER B2). `make check` already
# runs a short version of this as a regression guard.
LATENCY_SECONDS ?= 60
latency:
	OPENACCESSBRIDGE_LATENCY_SECONDS=$(LATENCY_SECONDS) $(GO) test ./internal/audio/latency/ -run Latency -v -count=1 -timeout 20m

# §10.4: decode the QR images this appliance really serves, with an INDEPENDENT
# decoder (zxing-cpp, the engine behind many phone scanner apps).
#
# `make check` already pins every module of every symbol against reference
# matrices from an unrelated encoder, all of which were decoded before being
# committed — see internal/qr/testdata/generate_vectors.py. What those fixtures
# cannot cover is the last step: that the rendered PNG, at the scales the admin
# surface serves, is an image a scanner acquires. This target closes that, and it
# lives outside CI because CI has no Python and must not need any.
#
#     python3 -m venv .venv && .venv/bin/pip install qrcode segno zxing-cpp pillow
#     make qr-verify PY=.venv/bin/python
# A fixed path rather than a temp dir, so the images stay on disk for an operator
# to look at (and because a recursively-expanded mktemp would hand the two halves
# of this target two different directories).
PY ?= python3
# Absolute: `go test` runs in the package's own directory, so a relative path
# would put the images under internal/qr/ and the verifier would look elsewhere.
QR_PNG_OUT ?= $(CURDIR)/$(BIN)/qr-verify
qr-verify:
	QR_PNG_OUT=$(QR_PNG_OUT) $(GO) test ./internal/qr/ -run TestWritePNGCorpus -v -count=1
	$(PY) internal/qr/testdata/verify_png.py $(QR_PNG_OUT)

# Run the ACSP simulator standalone (dev convenience).
sim: acsp-simulator
	$(BIN)/acsp-simulator

# --- Docker (deploy/docker, D45) ------------------------------------------------
# One image carrying all four binaries; the appliance runs it as three
# containers (deploy/docker/docker-compose.yml), mirroring the systemd units.
# The image gets the SAME version and build-number stamps as the native build:
# the Dockerfile refuses to build without them, for the reason above (D32).
IMAGE ?= ghcr.io/simpledcp/openaccessbridge
COMPOSE     := docker compose -f deploy/docker/docker-compose.yml
COMPOSE_DEV := docker compose -f deploy/docker/docker-compose.dev.yml

docker-build: stamp-check
	@docker buildx version >/dev/null 2>&1 || { \
	  echo "docker buildx is required: the Dockerfile is a BuildKit one (Ubuntu: apt install docker-buildx; or drop the plugin into ~/.docker/cli-plugins/)" >&2; exit 1; }
	docker build -f deploy/docker/Dockerfile \
	    --build-arg VERSION=$(VERSION) --build-arg BUILD_NUMBER=$(BUILD_NUMBER) \
	    -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# Ship an image to an appliance as a file: the appliance's own output chain
# drops `docker pull` (§9.1), so images travel offline (deploy/README.md).
docker-save: docker-build
	@mkdir -p dist
	docker save -o dist/openaccessbridge-$(VERSION).tar $(IMAGE):$(VERSION)
	@ls -la dist/openaccessbridge-$(VERSION).tar

# Build the image, then prove it: the three-container dev stack comes up
# healthy, the patron and admin surfaces answer, captions and audio are
# offered, the single-container demo works, and nothing logged an error.
# Ports 18080/18090 so a natively running appliance on 8080/8090 is not in the way.
docker-smoke: docker-build
	OPENACCESSBRIDGE_IMAGE=$(IMAGE):$(VERSION) deploy/docker/smoke.sh

# Appliance stack (host network, privileged helper) — read deploy/README.md first.
docker-up:
	$(COMPOSE) up -d
docker-down:
	$(COMPOSE) down

# Laptop/CI stack (bridge network, published ports, dry-run helper).
docker-dev-up:
	$(COMPOSE_DEV) up -d
docker-dev-down:
	$(COMPOSE_DEV) down -v

clean:
	rm -rf $(BIN) coverage.txt coverage.html
