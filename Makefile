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

.PHONY: all build test vet race cover sim soak clean tidy fmt check

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

# Run the ACSP simulator standalone (dev convenience).
sim: acsp-simulator
	$(BIN)/acsp-simulator

clean:
	rm -rf $(BIN) coverage.txt coverage.html
