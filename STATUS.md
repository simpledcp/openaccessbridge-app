# STATUS

_Last updated: foundation milestone._

## How to build and test

```sh
export PATH=/home/libserv-2/go-sdk/bin:$PATH   # local Go 1.26 toolchain
cd /home/libserv-2/captionbridge-app
make check     # go vet + full test suite
make build     # all four binaries → ./bin
make race      # race detector
make cover     # coverage summary
```

## Done

- **Full caption path (Phases 1–3) — the heart of this session — is complete and
  green end-to-end:**
  - timed-text parser (ST 428-7/428-10/Interop), monotonic timing engine,
    RPL parser + pluggable resolver + MXF unwrap + bounded cache + reel loader,
    multi-language Controller, WS/SSE fan-out, patron page.
  - **ST 430-10 state machine** (`internal/caption/acsp`): lease-and-timeline
    machine; half-lease request interval; continue-output-on-comms-loss-until-
    lease-expiry; Set Output Mode incl. absence; Playout-ID=0/duplicate
    disambiguation by timeline (no false loop-back); empty ReelResources; new-
    show Terminate-Lease drop; wrong-room fingerprint guard. Deterministic
    Handle/Tick API + production Run loop over a Transport seam (PROVISIONAL,
    Phase-0A-swappable).
  - **ST 430-10 simulator + 17-scenario quirk suite** (`internal/sim`) wired
    into CI as the regression backbone (§11.1); standalone `acsp-simulator`
    binary runs them too. All race-clean.

- **Foundation** (task 1):
  - Repo scaffold, `go.mod` (Go 1.26), Makefile, CI (`.github/workflows/ci.yml`),
    `.gitignore`, README, the four living docs.
  - `internal/model` — all shared data shapes (§10): cue, audio stream state,
    caption-link/ACSP state + patron-status mapping, patron session, auditorium
    profile, system readiness + loud-warning set, system config/versioning.
  - `internal/logging` — structured event taxonomy (§9.4) + **redaction guard**
    (denied-key scrubbing + opaque HashID); tests assert no caption text / audio /
    SDP / tokens / client IPs / title-bearing paths can reach a log.
  - `internal/config` — SQLite (pure-Go) config store: auditorium profiles,
    system config/versioning, admin credentials (PBKDF2, no plaintext, must-change
    default flag), settings; tests incl. a "no content columns" structural guard.
  - `internal/ipc` — loopback Unix-socket IPC between the three processes (§3.5),
    newline-delimited JSON, owner-only socket perms; round-trip test.
  - Four runnable binaries: `captionbridge-core` (opens config, logs, waits),
    `captionbridge-media` + `captionbridge-firewall` (loopback ping/pong helpers),
    `acsp-simulator` (placeholder until `internal/sim`).
  - **All green:** `go vet` clean, all tests pass, all binaries build.

## In progress

- (none — between milestones)

## Next (in plan order)

8. **Audio pipeline** (§6) — captionbridge-media: four-state qualifier + channel
   mapper (pure-Go, unit-tested) + capture/encode seam (gst-launch synthetic tone;
   webrtcbin hardware-gated).
9. **Firewall manager + verification + security tests** (§9) — nftables Variant
   A/B rule-gen; forwarding/isolation self-introspection; security tests in CI.
10. **Admin UI + setup wizard + loud warnings** (§8).
11. **Deployment profiles + systemd + Loaner overlay** (§12).
12. **Final integration + invariant tests + handoff.**

Tasks 2–7 (timed-text, RPL/resolver, timing, ACSP+fan-out, simulator, web) are
DONE.

## Key facts for a future session (post-compaction)

- Build host has **no passwordless sudo**; Go is at `/home/libserv-2/go-sdk`.
- Project is at `/home/libserv-2/captionbridge-app` (NOT the root-owned
  `…/captionbridge`) — see DECISIONS D0.
- GStreamer runtime present but **no dev headers / no `webrtcbin`** — media encode
  uses `gst-launch` subprocess behind a seam (DECISIONS D5, BLOCKERS B4).
- ACSP behavior is **PROVISIONAL** (ISDCF notes), Variant A default — see
  DECISIONS D6/D7, BLOCKERS B1.
- Read `buildplan.md` sections on demand; do not rely on memory for protocol detail.
