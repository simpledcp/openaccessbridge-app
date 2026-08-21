# CaptionBridge build system.
# Local Go toolchain is installed at /home/libserv-2/go-sdk on this dev machine;
# CI uses actions/setup-go. GOTOOLCHAIN=local avoids surprise network toolchain
# downloads on an offline appliance build host.

GO ?= go
GOTOOLCHAIN ?= local
export GOTOOLCHAIN

BIN := bin
BINARIES := captionbridge-core captionbridge-media captionbridge-firewall acsp-simulator
LDFLAGS := -X github.com/simpledcp/captionbridge/internal/buildinfo.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)

.PHONY: all build test vet race cover sim soak latency qr-verify clean tidy fmt check

all: check build

build: $(BINARIES)

$(BINARIES):
	@mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/$@ ./cmd/$@

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
	CAPTIONBRIDGE_SOAK_CYCLES=$(SOAK_CYCLES) $(GO) test ./internal/soak/ -run Soak -v -timeout 30m

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
	CAPTIONBRIDGE_LATENCY_SECONDS=$(LATENCY_SECONDS) $(GO) test ./internal/audio/latency/ -run Latency -v -count=1 -timeout 20m

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

clean:
	rm -rf $(BIN) coverage.txt coverage.html
