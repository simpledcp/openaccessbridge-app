# STATUS — final handoff

CaptionBridge is built as far as it can go in software on this dev machine. All
in-scope backlog items from `kickoff.md` are **done, integrated, and green**;
everything hardware-gated is stubbed behind a seam and recorded in `BLOCKERS.md`.

- **28 packages · 119 tests · race-clean · ~71% coverage · ~11.8K LOC of Go.**
- The full three-process system runs end-to-end and was verified live (below).

## How to build, test, and run

```sh
export PATH=/home/libserv-2/go-sdk/bin:$PATH      # local Go 1.26 toolchain
cd /home/libserv-2/captionbridge-app

make check        # gofmt + go vet + full race test suite (CI gates on this)
make build        # all four binaries → ./bin
make cover        # coverage summary

# Run the whole appliance locally (three processes + self-advancing ACSP demo):
RT=/tmp/cb; mkdir -p $RT
./bin/captionbridge-firewall --socket-dir=$RT &      # dry-run unless root + --allow-apply
./bin/captionbridge-media    --socket-dir=$RT &      # synthetic HI/VI-N streams
./bin/captionbridge-core --config $RT/config.db \
    --patron-bind 127.0.0.1:8080 --admin-bind 127.0.0.1:8090 --socket-dir=$RT &
# Patron page:  http://127.0.0.1:8080/accessibility
# Admin board:  http://127.0.0.1:8090/admin   (first boot: create credentials)
# Run the ACSP simulator scenario suite standalone:
./bin/acsp-simulator --list ; ./bin/acsp-simulator
```

**Verified live (all three processes):** the dashboard reports captions
`PLAYING` (lease active, RPL loaded, languages en + es-419) AND audio HI/VI-N
`excellent` (via the media helper over loopback IPC) AND
forwarding-disabled / firewall-active / admin-isolated (via self-introspection)
— each computed **independently**.

## Done (all in-scope backlog)

| # | Item | Where |
|---|---|---|
| 1 | Foundation: 3-process scaffold, models (§10), logging+redaction (§9.4), SQLite config+versioning (§10/§10.8), loopback IPC (§3.5), CI | `internal/{model,logging,config,ipc}`, `.github/` |
| 2 | Timed-text parser: ST 428-7 + 428-10 closed profile + Interop; edit-rate/timecode→ms (§5.4) | `internal/caption/timedtext` |
| 3 | RPL parser + pluggable resolver + MXF/ST429-5 unwrap seam + bounded cache + reel loader (§5.3) | `internal/caption/{rpl,resolver}` |
| 4 | Monotonic timing engine — anchor/predict, no sleep loops (§5.5) | `internal/caption/timing` |
| 5 | ST 430-10 state machine + WS/SSE caption fan-out (§5.2, §5.6) | `internal/caption/{acsp,fanout}` |
| 6 | ST 430-10 simulator + 17-scenario quirk suite (CI regression backbone, §11.1) | `internal/sim` |
| 7 | Web server + patron page + route/bind separation (§7, §8.4) | `internal/web` |
| 8 | Audio: 4-state qualifier + channel mapper (pure-Go, tested) + capture/encode seam + synthetic-tone GStreamer Opus smoke (§6) | `internal/audio/*`, `captionbridge-media` |
| 9 | Firewall nftables Variant A/B rule-gen + self-introspection verify + security/invariant tests in CI (§9) | `internal/security/*`, `captionbridge-firewall` |
| 10 | Admin dashboard (separate caption/audio readiness + loud warnings) + first-boot setup wizard + admin auth (§8) | `internal/web`, `internal/readiness` |
| 11 | Four deployment profiles + Loaner overlay + systemd units (§12, §3.5, §9.4) | `internal/deploy`, `deploy/` |
| 12 | Three-process integration + cross-cutting invariant tests + this handoff | `cmd/*`, `internal/invariants` |

## Invariants (asserted in CI — `internal/invariants`)

No caption/audio persistence (config has no content columns; units use tmpfs +
`LimitCORE=0`); no v4/v6 forwarding + default-deny firewall with zero
forward-accepts; three-process privilege separation (only firewall is root; core
*Wants* media); independent caption/audio readiness; no-silent-degradation (a
failed feed raises a critical warning); nothing sensitive in logs (redaction).

## What is NOT done (hardware/external-gated — see BLOCKERS.md)

Phase 0A ACSP wire capture (real server), Phase 0B browser/WebRTC/secure-context
(real phones), Phase 0C audio-hardware + glass-to-ear latency, real `webrtcbin`
peer, live nftables apply on the appliance, and gates G1–G5. The ACSP protocol is
**PROVISIONAL** (ISDCF notes, Variant A default) behind a swappable seam; the
caption path is validated against the simulator scenario suite instead.

## Key facts for a future session (post-compaction)

- Project is at `/home/libserv-2/captionbridge-app` (the intended
  `…/captionbridge` is root-owned/unwritable — DECISIONS D0). Go at
  `/home/libserv-2/go-sdk`. No passwordless sudo.
- Read `buildplan.md` sections on demand; do not rely on memory for protocol detail.
- DECISIONS.md records every OPEN-item default (configurable); BLOCKERS.md records
  what hardware/artifact clears each gate.
