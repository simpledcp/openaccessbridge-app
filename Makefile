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

.PHONY: all build test vet race cover sim soak latency clean tidy fmt check

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

# Run the ACSP simulator standalone (dev convenience).
sim: acsp-simulator
	$(BIN)/acsp-simulator

clean:
	rm -rf $(BIN) coverage.txt coverage.html
