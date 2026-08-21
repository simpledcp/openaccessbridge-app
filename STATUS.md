# STATUS — handoff

CaptionBridge is built as far as it can go in software on this dev machine.
Everything hardware-gated is behind a seam and recorded in `BLOCKERS.md`.

- **31 packages · 472 tests · race-clean · ~82% coverage · ~36K LOC of Go.**
- **32 ACSP scenarios** (`./bin/acsp-simulator`) as the caption-path regression backbone.
- A **soak layer** (§11.6): 1500 show cycles with reconnects and language
  switches, retention flat, content purged at every show boundary.
- The full three-process system runs end-to-end and was verified live (below).

## Where the work stands

An earlier session declared the in-scope backlog complete. A later spec-vs-code
audit of every `buildplan.md` section against the shipped code found **78
confirmed gaps**, several reproduced by RUNNING the code — captions went blank
after reel 1, and a second `Set RPL Location` rendered the feature during the
trailer. Those are fixed, along with the wiring that left the new capability
with no production caller. See JOURNAL.md for the narrative and
`## Known remaining work` below for what is still open.

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

make soak         # §11.6 long soak (default 2000 show cycles)
make latency      # §11.4 appliance-side latency pass (default 60 s sampling window)
```

**Verified live (all three processes):** captions `PLAYING` (lease active, RPL
loaded, languages en + es-419) AND audio HI/VI-N `excellent` over loopback IPC
AND per-process liveness for all three AND the D11 client cap in force (0/25 on
Booth Diagnostic) — each computed **independently**. Audio delivery is verified
the same way: with the three processes running,
`curl /api/audio/streams` reports `playback_available:true`, and
`TestAudioPathEndToEndAcrossTheThreeProcesses` joins as a patron would and
receives real Opus RTP (see §6.7 note below). On this dev host the
firewall row reads `INACTIVE / introspected=false` with "nft list failed:
Operation not permitted", which is the CORRECT loud answer: reading the ruleset
needs root (B5), so the posture is reported as unverified rather than assumed
green.

## Done

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
| 13 | Caption path made correct: timeline-driven reel window, RPL keyed by timeline SEGMENT, reel-level durations, `<StartTime>`/EntryPoint cue origin, output-mode suppression, bounded fetch with a lease-clamped timeout, content purge at reel + show boundaries (§5.2–§5.5) | `internal/caption/*` |
| 14 | Audio made honest: stereo mappings qualify (the pipeline hard-coded 1 channel), sustained-silence hysteresis, device enumeration with stable by-id/by-path identity, dead-device reporting, no fabricated latency (§6.1–§6.6) | `internal/audio/*` |
| 15 | Security posture MEASURED, not asserted: the loaded nftables ruleset is read, route/DNS-leak and allowlist-scope introspection, profile-aware SSH, multi-IP allowlist, debug-capture controls (§9.1, §9.3, §9.4) | `internal/security/*` |
| 16 | Backend wiring: caption warnings reach the dashboard, D11 client caps enforced, firewall applied+verified through the privileged helper, per-process liveness, §10.5/§10.8 written from checks that actually ran (§3.5, §8.3, §10.8) | `cmd/*`, `internal/readiness` |
| 17 | Patron surface (§7): the three routes differ, HI/VI-N picker + volume + the §6.7 "add a little buffer" control, per-show sessions and join codes (§10.4), browser-language matching, reachable SSE fallback, show-ended state | `internal/web` |
| 18 | §6.4 audio calibration engine: feasibility, verdict, plain-language summary, concrete tier-aware fixes, saved auditorium profile | `internal/audio/calibration` |
| 19 | §11.6 soak/reliability layer: retention must be FLAT across many shows | `internal/soak` |
| 20 | §6.7 audio delivery, end to end: streaming Opus/RTP encode per named stream, a Pion peer hub that fans ONE encoder out to N subscribers, the SDP/ICE relay across all three processes, and patron availability driven by a measured encoder state (D20) | `internal/audio/{encode,webrtcpeer,pipeline}`, `internal/web/audio.go`, `cmd/*` |
| 21 | ICE gathering confined to the configured patron interface, refused by name when that interface is absent, and the networks a patron was really offered MEASURED off the emitted SDP and shown on the dashboard (§6.7, §9.1, §8.2, D28) | `internal/audio/webrtcpeer`, `internal/ipc/network.go`, `cmd/*` |
| 22 | §10.4 QR onboarding: an in-tree pure-Go QR encoder verified module-for-module against independent implementations across all 160 version/level combinations and decoded by zxing-cpp, served per-show on the admin surface with a computed minimum print size (D29) | `internal/qr`, `internal/web/session_admin.go` |
| 23 | §11.5 pre-show QA checklist codified as a staff procedure: explicit three-state pass/fail per item, a remedy on every item, staff steps printed but never recorded, and a printable booth sheet (D30) | `internal/model/checklist.go`, `internal/readiness/checklist.go`, `internal/web/checklist.go` |
| 24 | §9.4 support mode: a bounded, audited, outbound-only WINDOW with an absolute deadline that does not survive a restart, one scoped egress rule, an audit trail beside the switch — plus the fix for an nftables ruleset that could never remove a rule (D31, D31a) | `internal/security/support`, `internal/security/firewall`, `internal/config/support.go`, `cmd/captionbridge-core` |

## §6.7 audio delivery — what is real, and what a patron would actually get

A patron on a phone can now **really receive audio**. The path is: capture →
channel mapper → one `gst-launch-1.0` Opus encoder per named stream (mapped PCM
in on stdin, RTP out on loopback UDP) → one Pion `TrackLocalStaticRTP` per named
stream → one peer connection per subscriber, negotiated over the patron page's
own WebSocket and relayed to `captionbridge-media` over the loopback socket.
Host ICE candidates only; no STUN, no TURN.

**What they would hear on this host is a 440 Hz (HI) / 660 Hz (VI-N) test tone**,
because there is no capture hardware (B3). That is stated on the patron surface
(`test_tone` → "You are hearing a test tone, not this auditorium's audio") as
well as on the dashboard. Availability is measured, not assumed: a stream is
offered only while the media process reports `encoder_state:"streaming"`, which
is itself set from a counted RTP packet — not from "the encoder was launched".

**Measured here:** Opus RTP arrives at a subscriber over the real three-process
path; RTP timestamps advance exactly 480 ticks per packet (the §6.7 10 ms
frame); the encode path holds ~0 frames of backlog in steady state. **Not
measured here:** anything glass-to-ear. No phone, no venue AP, no jitter buffer
(B2). The dashboard still renders audio latency as "not measured" with the
encode-path estimate beside it under its own name.

## §11.4 latency — the appliance-side FLOOR, and only that

`internal/audio/latency` (`make latency`) drives the production path — the real
`pipeline.Stream`, the real `gst-launch` Opus encoder, the real
`webrtcpeer.Hub` — and times each appliance-side contributor separately, so a
real figure can later be COMPOSED with the phone-side terms instead of guessed.
Timestamps are taken from a tap on the `encode.RTPStream` seam; each packet is
joined between the encoder's socket and the subscriber by its RTP timestamp, and
to the PCM block that produced it by an ordinal that resynchronises on any RTP
sequence gap rather than drifting a frame.

Measured on this dev host, 30 s window, 3000 joined packets, 0 dropped blocks,
0 sequence resyncs (p50 / p95):

| Contributor | p50 | p95 | what the two timestamps span |
|---|---|---|---|
| `channel_map` | 0.8 µs | 4.4 µs | one `mapper.Extract` of one capture frame, on the delivery path |
| `level_analysis` | 119 µs | 234 µs | one `Extract` + one `qualifier.Analyze` of a §6.3 block — runs BESIDE the delivery path, so it is not in a patron's latency at all |
| `opus_encode_transit` | 416 µs | 524 µs | a complete 10 ms block handed to the encoder → the RTP packet carrying it, off the encoder's own socket |
| `webrtc_fanout_transit` | 111 µs | 143 µs | that packet → the same packet parsed by a Pion subscriber, through the fan-out, a real SRTP session and an ICE host-candidate hop |
| `appliance_floor` | 531 µs | 651 µs | both of the above on ONE packet (a measurement, not a sum of percentiles) |

Also **measured**, from the RTP clock: the encoder's Opus **pre-skip is 120
samples (2.5 ms)** — its first timestamp stride was 360 ticks where every later
stride was exactly 480.

**Composed appliance-side floor: ~10.7 ms** = 10 ms of structural framing (a
10 ms Opus frame cannot be emitted before 10 ms of audio exists) + the 651 µs
measured p95, for the oldest sample in a block.

**This is a FLOOR, and it is NOT a glass-to-ear figure.** It was measured
Go-peer-to-Go-peer on one machine. §6.7's own honesty flag names the phone-side
jitter buffer (40–300+ ms) as the dominant, least-controllable term, and it is
not in these numbers at all — a Pion receiver buffers nothing and hands packets
up as they arrive, which is precisely why this bounds the appliance from below
and says nothing about a patron's ear. **Nothing here may be read as meeting (or
missing) §6.7's 100–150 ms HI target.** Still unmeasured, each printed by name in
every report the harness produces: analog capture + ALSA buffer (B3), local
Wi-Fi transport, the phone-side jitter buffer (B2), Opus **decode** (nothing here
decodes a payload), and phone audio output (B2). On real hardware the 10 ms
framing term is replaced by the ALSA capture period (§6.7 row 1: 5–20 ms), which
is not this synthetic frame.

## Invariants (asserted in CI — `internal/invariants`)

No caption/audio persistence (config has no content columns; units use tmpfs +
`LimitCORE=0`); no v4/v6 forwarding + default-deny firewall with zero
forward-accepts; three-process privilege separation (only firewall is root; core
*Wants* media); independent caption/audio readiness; no-silent-degradation (a
failed feed raises a critical warning); nothing sensitive in logs (redaction).

## Known remaining work (software, not hardware-gated)

Recorded honestly rather than closed off. Fullest detail is in JOURNAL.md and in
the audit lanes' own reports.

- **The §6.4 wizard has no hardware path to measure.** `calibration.SourceHardware`
  is implemented and tested, but nothing on this image can emit it — the single
  line that becomes a real-device open is the synthetic-device-id refusal (B3/B4).
- **A multi-device room is not representable in one §10.5 profile record**
  (`AuditoriumProfile` holds a single `input_device_id`). The wizard now refuses
  the pairing rather than calibrating one device and reporting two; the show path
  in `cmd/captionbridge-media` has the same shape and is masked only by the B3/B4
  refusal. Worth a decision before the hardware path lands.
- **Support mode opens the WINDOW, not a tunnel.** §9.4's session is built —
  bounded, audited, outbound-only, visibly indicated — but this appliance has no
  tunnel client and no support endpoint of its own, so nothing dials out. Every
  surface reports that distinction (`tunnel_established` is a field, always
  false). A real support transport is a separate piece of work and needs a
  service to connect to.
- **The nftables table-replacement fix is not demonstrated on a live ruleset.**
  Applying needs root (B5) and this host has user namespaces disabled, so the
  append-vs-replace behaviour is established from nft's documented semantics and
  the fix is asserted in the generated script text. Confirm it against a real
  ruleset the first time the helper runs as root (D31a).
- **Signed offline updates + A/B rollback (§9.4)** are not built.
- **The ACSP supervisor has no production caller**: `cmd/captionbridge-core`
  still runs the self-advancing demo, because the real transport is B1-gated.
- **The ICE scope row is as fresh as the last posture reconcile** (15 s), not as
  fresh as the last patron's answer. That matches every other posture row on the
  dashboard, but it means a booth watching the page sees a confinement change one
  reconcile after it takes effect. The confinement itself applies to the very
  next answer.
- **§6.7's latency table now has an appliance-side floor and nothing else.**
  Rows 2 and 3 (analysis/map, Opus encode) are measured with distributions, and
  the appliance's own transport leg is measured. Rows 1, 4, 5 and 6 — ALSA
  buffer, Wi-Fi, the DOMINANT phone jitter buffer, phone output — are still
  quoted typicals: they need the §11.4 loopback rig (B3) and a phone fleet (B2).
  §15 named HI latency as the plan's largest risk and it remains one.
- **`pipeline.EncoderPathLatencyMS` double-counts one accumulation.** It reports
  capture period + Opus frame (20 ms shipped), but at the shipped settings the
  10 ms capture frame IS the 10 ms Opus frame — one accumulation, not two. The
  §11.4 harness measures the whole appliance path at ~10.7 ms. It errs
  conservative, so it was left alone in the latency pass; it needs a decision
  alongside the real ALSA period, where a period longer than one frame does add
  a genuine second term.
- **There is no in-product latency estimate calibrated against ground truth**
  (§11.4's last bullet). The dashboard shows a structural encode-path budget and
  says "not measured"; calibrating it needs the loopback rig.

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
- **Do not trust a "done" claim without the test that fails without it.** The
  §6.7 review pass mutated every claimed behaviour: three passed with the
  behaviour removed, and one hid a real mid-show fault (a stale peer callback
  keyed by id closed the patron's live peer when they switched feeds). Two
  fabrications were caught by adversarial review, not by the test suite: a
  synthetic tone reported under the booth's real hardware identity and persisted
  as §10.5 `last_qualification`, and a §10.8 readiness stamp written by pressing
  Complete rather than by a check that ran. Both looked green.
- The three processes are named longer than 15 characters, so `pkill -x
  captionbridge-core` silently matches nothing. Kill them by PID.
