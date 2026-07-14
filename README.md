# CaptionBridge

A locked-down, dual-interface cinema **accessibility appliance**: it consumes a
cinema server's standardized off-screen timed-text feed (SMPTE ST 430-10 sync +
ST 430-11 resource lists + referenced ST 428-7/428-10 timed text) and serves
captions to patron smartphones over a local web page, plus HI / VI-N assistive
audio captured from low-cost analog inputs — **without ever routing packets
between the theater network and the patron network.**

This repository implements the engineering plan in [`buildplan.md`](./buildplan.md).
See [`kickoff.md`](./kickoff.md) for the autonomous-build brief.

> **Status:** in active autonomous software build on a dev machine (no real cinema
> server, phones, or audio hardware). See [`STATUS.md`](./STATUS.md),
> [`DECISIONS.md`](./DECISIONS.md), [`BLOCKERS.md`](./BLOCKERS.md), and
> [`JOURNAL.md`](./JOURNAL.md).

## Architecture (three supervised processes, two physical interfaces)

| Process | Role |
|---|---|
| `captionbridge-core` | Go: ACSP state machine, RPL/timed-text parsing, timing engine, caption fan-out, web/admin server. Supervises the others. |
| `captionbridge-media` | Audio pipeline: capture → four-state qualifier → channel mapper → Opus → WebRTC. Isolated so a media crash can't drop the caption lease. |
| `captionbridge-firewall` | Privileged helper: nftables apply + forwarding/isolation verification. The only component that needs root. |

The processes talk only over local loopback Unix sockets (`internal/ipc`). No
plane forwards packets between the theater (`eth0`) and patron (`wlan0`/`eth1`)
interfaces; data crosses only as fully terminated, re-originated application
traffic.

## Hard invariants (tested from day one)

1. No persistence of caption text or HI/VI audio (memory/tmpfs only).
2. No IPv4/IPv6 forwarding; default-deny firewall; no bridges.
3. Three-process privilege separation.
4. Independent caption/audio readiness state.
5. No silent degradation — surface a loud staff warning *and* a patron-facing state.
6. Nothing sensitive in logs (no caption text, audio, SDP, or client IDs — `internal/logging`).

## Build & test

**New machine? See [`DEVELOPMENT.md`](./DEVELOPMENT.md) for full setup.** The
build and the entire test suite are pure Go — the only hard requirement is a
**Go 1.26.x** toolchain on your `PATH` (no cgo, no system libraries; GStreamer
and nftables are optional runtime seams).

```sh
make check     # gofmt + go vet + full race test suite (incl. ACSP simulator scenario suite)
make build     # build all four binaries into ./bin
make race      # tests under the race detector
make cover     # coverage summary
```

CI (`.github/workflows/ci.yml`) runs vet, the race-enabled test suite (the ACSP
simulator scenario suite is the caption-path regression backbone, §11.1), and
builds all binaries on every push/PR.

## Layout

```
cmd/                     process entrypoints + the acsp-simulator
internal/
  model/                 shared transient + persisted data shapes (§10)
  logging/               structured observability + redaction guard (§9.4)
  config/                SQLite config store + versioning (§10/§10.8)
  ipc/                   loopback IPC between the three processes (§3.5)
  caption/               timed-text, RPL, resolver, timing, ACSP, fan-out (§5)
  audio/                 qualifier, mapper, capture, encode (§6)
  web/                   patron + admin HTTP surfaces (§7, §8)
  security/              firewall rule generation + verification (§9)
  sim/                   ST 430-10 simulator + scenario suite (§11.1)
deploy/                  systemd units + deployment profiles (§12)
testdata/                RPL + timed-text fixtures
```
