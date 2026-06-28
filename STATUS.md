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

2. **Timed-text parser** (§5.4) — ST 428-7 + ST 428-10 closed-caption profile +
   Interop; ST 429-12 track-type; edit-rate/timecode→ms; normalize to cue model.
3. **RPL parser + resolver** (§5.3) — ST 430-11 parse; pluggable resolver + MXF
   unwrap seam; bounded transient cache.
4. **Timing engine** (§5.5) — monotonic anchor/predict; active-cue resolver.
5. **ACSP state machine + fan-out** (§5.2/§5.6) — lease+timeline machine; WS/SSE.
6. **ACSP simulator + scenario suite** (§11.1) — the CI regression backbone.
7. **Web server + patron page** (§7/§8.4).
8. **Audio pipeline** (§6) — qualifier + mapper (pure-Go tested) + encode seam.
9. **Firewall manager + verification + security tests** (§9).
10. **Admin UI + setup wizard + loud warnings** (§8).
11. **Deployment profiles + systemd + Loaner overlay** (§12).
12. **Final integration + invariant tests + handoff.**

## Key facts for a future session (post-compaction)

- Build host has **no passwordless sudo**; Go is at `/home/libserv-2/go-sdk`.
- Project is at `/home/libserv-2/captionbridge-app` (NOT the root-owned
  `…/captionbridge`) — see DECISIONS D0.
- GStreamer runtime present but **no dev headers / no `webrtcbin`** — media encode
  uses `gst-launch` subprocess behind a seam (DECISIONS D5, BLOCKERS B4).
- ACSP behavior is **PROVISIONAL** (ISDCF notes), Variant A default — see
  DECISIONS D6/D7, BLOCKERS B1.
- Read `buildplan.md` sections on demand; do not rely on memory for protocol detail.
