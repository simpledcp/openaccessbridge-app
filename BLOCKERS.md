# BLOCKERS

Each hardware/external dependency that cannot be cleared in software on this dev
machine, and exactly what is needed to clear it. Per the kickoff: these are
**stubbed or simulated behind a seam** and the build continues — never faked.

## Hardware / external dependencies (out of scope this run)

### B1. Phase 0A — ACSP wire contract (real cinema server / IMB) [§13 Phase 0A, OPEN #1–4]
- **Needed:** one session against a real cinema server/IMB of the chosen first
  target family, capturing a pcap and answering: TCP-initiation direction; ACSP
  port; endpoint-registration model (auto-discover vs operator-configured IP);
  request framing/PDU structure; RPL locator form; timed-text fetch mechanism;
  whether inbound theater-side firewall rules are needed.
- **Seam in code:** `internal/caption/acsp` wire codec + `model.ACSPInitiation`
  (default Variant A) + the pluggable resource resolver. The simulator
  (`internal/sim`) models PROVISIONAL ISDCF behavior; swapping in the real
  contract is a codec/config change.
- **Clears:** firewall variant choice (§9.1), simulator fidelity (§11.1),
  resolver handlers (§5.3), gate G1.

### B2. Phase 0B — browser local-appliance reality (real iOS/Android phones) [§13 Phase 0B, OPEN #13]
- **Needed:** iOS Safari + Android Chrome + desktop Chrome tested against the
  appliance for HTTP-vs-HTTPS, self-signed-cert warnings, mDNS `*.local` ICE
  candidate handling, captive-portal behavior, QR onboarding, autoplay gesture,
  Wake Lock, and host-only WebRTC connect.
- **Seam in code:** WebRTC signaling/transport is behind the media encode seam;
  secure-context strategy is configurable (D12). Patron page handles tap-to-start.
- **Clears:** primary audio path proof, secure-context decision (Phase 7), G3.
- **What the §6.7 build DID prove here (2026-08-20):** the appliance now really
  serves Opus over WebRTC. A Go subscriber speaking the same protocol as
  `assets/patron.js` joins the running three-process appliance, offers, applies
  the answer, and receives Opus RTP
  (`cmd/captionbridge-core/audio_e2e_test.go`). What it does NOT prove is
  anything phone-shaped: no iOS/Android browser, no venue AP, no mDNS candidate
  from a real browser, no autoplay-gesture behaviour, and — decisively — **no
  measurement of the phone-side jitter buffer**, which §6.7 names as the
  dominant term in the latency budget. Every one of those still needs B2.

### B3. Phase 0C — audio hardware + HI glass-to-ear latency (real ADCs/jacks/phones) [§13 Phase 0C, OPEN #7–10]
- **Needed:** mini-PC mic jacks, USB dongles/interfaces, TRS/TRRS adapters,
  RCA→3.5 wiring, attenuator pads, ground-loop isolators; real glass-to-ear
  latency measurement (wired vs Bluetooth) on a phone fleet.
- **Seam in code:** capture is an interface with a synthetic-tone source; the
  qualifier/mapper are unit-tested on synthetic fixtures. No real-device latency
  is asserted.
- **Clears:** known-good hardware kit (§6.2), compatibility matrix (§11.3),
  gates G2a/G2b.
- **Now audible, which raises the stakes (2026-08-20):** the §6.7 path delivers
  this stand-in tone to a real subscriber, so the tone is no longer only a
  number on a dashboard — it is what a listener hears. The stand-in is therefore
  declared on the PATRON surface as well as the admin one
  (`PatronStream.test_tone` → "You are hearing a test tone, not this
  auditorium's audio"), and the synthetic source is paced at 1x
  (`capture.SyntheticTone.Realtime`) so what is delivered is 10 ms of audio per
  10 ms of clock rather than a generator running at whatever speed the CPU
  allows.
- **What the §11.4 latency pass did and did NOT clear (2026-08-20).** Cleared in
  software: the appliance-side contributors are measured as distributions rather
  than quoted from §6.7's table — channel map, level analysis, Opus encode
  transit, WebRTC fan-out transit, and an end-to-end appliance floor, plus a
  measured Opus pre-skip of 120 samples (2.5 ms) read off the RTP clock. Still
  gated by B3, and unchanged: **§11.4's actual deliverable, glass-to-ear
  latency**, which needs a known marker injected into a real ANALOG input and
  the phone's headphone output captured back — a loopback mic or splitter, real
  ADCs, and representative phones under good and degraded RF. Also still gated:
  §6.7 row 1 (analog capture + ALSA buffer, 5–20 ms), because there is no ADC
  here and the harness's 10 ms framing term is a synthetic frame size, not a
  real ALSA period; and §11.4's "continuous in-product latency estimate …
  calibrated against the loopback ground truth", which cannot be calibrated
  against a ground truth that does not exist yet. The dashboard therefore still
  reports `latency_measured:false`.

### B4. GStreamer dev libs + `webrtcbin` not installable (no passwordless apt) — **CLEARED IN SOFTWARE (D20)**
> **Status:** no longer blocking. The appliance-side WebRTC peer is now Pion
> (pure Go, no dev headers, no cgo) — see DECISIONS D20. GStreamer remains the
> Opus ENCODER via `gst-launch-1.0`, which needs only the runtime plugins that
> are present. The text below is kept because it still describes why the
> `webrtcbin` route was rejected.
- **Present:** GStreamer runtime 1.24.2 with `opusenc`, `audiotestsrc`,
  `alsasrc`, `level`.
- **Missing:** `gstreamer-1.0.pc` dev headers (so no go-gst cgo bindings) and the
  `webrtcbin` element (gst-plugins-bad / gst-plugins-rs).
- **Needed to clear:** `sudo apt install libgstreamer1.0-dev
  gstreamer1.0-plugins-bad gstreamer1.0-nice` (or the Rust webrtc plugin).
- **Workaround in place → now the shipped design:** the Opus encoder is a
  `gst-launch-1.0` subprocess per named stream, fed the mapped lane on stdin and
  emitting RTP on loopback UDP, which Pion fans out to the subscribers
  (`internal/audio/encode/stream.go`, `internal/audio/webrtcpeer`). No
  `webrtcbin`, no dev headers, no cgo.

### B5. nftables apply + sysctl forwarding toggle need root at runtime
- **Needed to clear:** the `captionbridge-firewall` helper runs as root (systemd
  unit) on the real appliance to apply rules and set `net.ipv4.ip_forward=0` /
  `net.ipv6.conf.all.forwarding=0`.
- **Workaround in place:** the firewall manager **generates and verifies** the
  ruleset and introspects forwarding/routes; `apply` is gated behind a dry-run
  mode when not root, and unit-tested by asserting generated rules, not by
  mutating this machine's live firewall.

### B6. Real cinema-server lab test (Phase 4, gate G1) and field pilot (Phase 9, G5)
- Hardware/venue/legal-gated by definition; out of scope. The caption path is
  validated against the simulator scenario suite instead (§11.1), which is the
  documented stand-in until lab time is available.

### B7. Real ST 429-5 MXF timed-text demux (asdcplib)
- **Present:** asdcplib source at `/home/libserv-2/asdcplib`.
- **Needed to clear:** build/link asdcplib (cgo) and replace the PROVISIONAL
  heuristic in `internal/caption/resolver/unwrap.go` with a proper Generic
  Container essence extractor. Only needed for servers that return MXF-wrapped
  track files (a Phase 0A finding, B1); servers returning plain XML don't hit it.
- **Workaround in place:** heuristic XML-essence extraction (DECISIONS D15),
  behind the same `Unwrap` seam.

## None of the above block the in-scope software work

The caption path (parser → resolver → timing → state machine → fan-out → patron
page), the audio qualifier/mapper, the firewall rule generation + verification
logic, the admin/setup surfaces, and the deployment-profile configuration are all
buildable and testable in software now, behind the seams above.
