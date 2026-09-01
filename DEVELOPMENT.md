# Getting started on a new machine

This guide gets OpenAccessBridge building, testing, and running from a clean clone
on a fresh box. Nothing outside this repository is required except a Go
toolchain — the build and the **entire test suite are pure Go** (no cgo, no
system libraries). GStreamer and nftables are only needed to exercise the
hardware-facing runtime seams, and their tests skip honestly when absent.

## 1. Prerequisites

**Required**

- **Go 1.26.x** — the module targets `go 1.26` (see `go.mod`). Install from
  <https://go.dev/dl/> or your package manager. Verify: `go version`.
- **Git**.

That is the whole hard requirement. On the original dev box the toolchain lived
at `/home/libserv-2/go-sdk` (hence references to that path in `STATUS.md` /
`DECISIONS.md`); on a new machine just install Go however you like and make sure
`go` is on your `PATH`. The build does not care where it lives.

**Optional — only for the hardware-facing runtime seams**

- **GStreamer 1.x runtime** (`gst-launch-1.0` + the `opusenc`, `audiotestsrc`,
  `level` elements) — enables the real synthetic-tone → Opus smoke test in
  `internal/audio/encode` and live audio in `openaccessbridge-media`. Without it
  that one test skips and the media helper falls back to its synthetic seam.
  - Debian/Ubuntu: `sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good`
- **nftables + root** — only to apply *real* firewall rules on an appliance.
  Off-appliance, `openaccessbridge-firewall` runs in dry-run mode and verifies via
  self-introspection; rule generation is unit-tested with no privileges.

**Not needed:** a C compiler (SQLite is `modernc.org/sqlite`, pure Go), Node/npm
(the patron and admin pages are server-rendered Go — there is no JS build step).

**Platform:** develop and test on any OS Go supports. The firewall
(nftables), audio runtime (GStreamer), and deployment (systemd) pieces are
Linux-specific, so end-to-end runs and the GStreamer smoke test want Linux; the
pure-Go core builds and tests fine on macOS/Windows too.

## 2. Clone and verify

```sh
git clone git@github.com:simpledcp/openaccessbridge-app.git
cd openaccessbridge-app

make check     # gofmt + go vet + full race test suite — this is what CI gates on
make build     # builds all four binaries into ./bin
```

`make check` passing on a clean clone means the environment is good — it runs the
ST 430-10 simulator scenario suite (the caption-path regression backbone) and the
security/invariant tests, all headless. First run downloads module dependencies
(`go mod download`); every run after that is offline.

Other targets: `make race` (race suite only), `make cover` (coverage summary),
`make sim` (run the ACSP simulator standalone), `make clean`.

### Offline / `GOTOOLCHAIN`

The Makefile and CI set `GOTOOLCHAIN=local` so the build never tries to fetch a
different Go version over the network — important because the production target
is an offline appliance build host. Consequence: your installed Go must be
1.26.x or newer, or the build will refuse rather than auto-download a toolchain.
If you want a fully network-free clone (dependencies included), run
`go mod vendor` once and commit `vendor/`; until then, the first `make check`
needs network to fetch modules (they are pinned and checksum-verified via
`go.sum`).

## 3. Run the whole appliance locally

Three supervised processes over loopback Unix sockets, plus a self-advancing
ACSP demo — no cinema server, phones, or audio hardware required:

```sh
RT=/tmp/oab; mkdir -p "$RT"
./bin/openaccessbridge-firewall --socket-dir="$RT" &      # dry-run unless root + --allow-apply
./bin/openaccessbridge-media    --socket-dir="$RT" &      # synthetic HI/VI-N streams
./bin/openaccessbridge-core --config "$RT/config.db" \
    --patron-bind 127.0.0.1:8080 --admin-bind 127.0.0.1:8090 --socket-dir="$RT" &

# Patron page:  http://127.0.0.1:8080/accessibility
# Admin board:  http://127.0.0.1:8090/admin   (first boot: create credentials)
```

Run the ACSP simulator scenario suite on its own: `./bin/acsp-simulator --list`
then `./bin/acsp-simulator`.

### … or in Docker

The same three processes package as one image (`deploy/docker/`, D45). It needs
Docker Engine 25+ with Compose v2.20+ and BuildKit — on Ubuntu's `docker.io`
package BuildKit's CLI is the separate `docker-buildx` package (or drop the
plugin binary into `~/.docker/cli-plugins/`).

```sh
make docker-build                                             # stamps version + build number from git
docker compose -f deploy/docker/docker-compose.dev.yml up     # three containers, ports 8080/8090
# or, one container:
docker run --rm -p 8080:8080 -p 8090:8090 ghcr.io/simpledcp/openaccessbridge openaccessbridge-demo
make docker-smoke                                             # what CI runs against the image
```

That is this section's recipe as containers: dry-run firewall, synthetic audio,
the self-advancing demo show. The appliance deployment (host network, privileged
helper) is `deploy/docker/docker-compose.yml`; read `deploy/README.md` before
using it — it is not for a laptop.

## 4. Where to look next

- `README.md` — architecture, hard invariants, package layout.
- `buildplan.md` — the full phased engineering spec (read sections on demand).
- `STATUS.md` — what is done and what was verified live at handoff.
- `BLOCKERS.md` — what is hardware/artifact-gated and what clears each gate.
- `DECISIONS.md` — every OPEN-item default and why it was chosen.
- `JOURNAL.md` — running build log.
