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

### B3. Phase 0C — audio hardware + HI glass-to-ear latency (real ADCs/jacks/phones) [§13 Phase 0C, OPEN #7–10]
- **Needed:** mini-PC mic jacks, USB dongles/interfaces, TRS/TRRS adapters,
  RCA→3.5 wiring, attenuator pads, ground-loop isolators; real glass-to-ear
  latency measurement (wired vs Bluetooth) on a phone fleet.
- **Seam in code:** capture is an interface with a synthetic-tone source; the
  qualifier/mapper are unit-tested on synthetic fixtures. No real-device latency
  is asserted.
- **Clears:** known-good hardware kit (§6.2), compatibility matrix (§11.3),
  gates G2a/G2b.

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
- **Workaround in place:** media encode via `gst-launch-1.0` subprocess for the
  synthetic-tone smoke test (D5); real WebRTC peer deferred to B2.

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
