# JOURNAL

A short running log of what was done and why. Newest at the bottom.

## Foundation milestone

- Read `buildplan.md` (1112 lines) and `kickoff.md` in full. The plan's spine is
  §13's phased plan; the heart of this session is the caption path (Phases 1–3)
  plus network isolation, with audio (Phases 5–6) second and hardware-gated work
  recorded as blockers.
- **Environment recon:** Go absent (installed 1.26.4 locally — no passwordless
  apt/sudo); Node 22 present; GStreamer runtime 1.24.2 present but no dev
  headers / no `webrtcbin`; nft 1.0.9 present; the intended project dir
  `…/captionbridge` is root-owned and unwritable. Decided to build in
  `…/captionbridge-app` (DECISIONS D0) to avoid blocking, recorded the one-line
  relocation command for the operator.
- **Scaffolded** the three-process layout, build system, and CI. Established the
  shared contracts everything else depends on, written by a single author for
  coherence: `internal/model` (all §10 shapes), `internal/logging` (taxonomy +
  redaction invariant), `internal/config` (SQLite store + creds + versioning),
  `internal/ipc` (loopback seam, §3.5).
- **Invariant tests first:** redaction guard proves no content/PII reaches logs;
  config guard proves no content columns; credential test proves no plaintext.
- Verified: `go vet` clean, full test suite green, all four binaries build.
- Wrote STATUS / DECISIONS / BLOCKERS / JOURNAL as durable cross-compaction memory.

Next: the caption path, simulator-and-test-first, starting with the timed-text
parser (§5.4).

## Caption path (Phases 1–3) complete

- Built the caption path breadth-first to get an end-to-end slice early, then
  deepened: timed-text parser → timing engine → fan-out → patron page (a runnable
  Phase-1 offline player, verified live), then RPL + resolver + cache (Phase 2),
  then the ACSP state machine + simulator (Phase 3).
- **Timing engine**: monotonic-clock anchor/predict, re-arming boundary timer
  (no sleep loop), explicit timeline events incl. comms-loss-continue-until-lease.
- **Resolver**: pluggable by locator form; mandatory MXF/ST429-5 unwrap seam
  (PROVISIONAL heuristic; asdcplib noted); bounded LRU cache; windowed reel loader
  that offsets cues to the composition timeline and stamps the ST 429-12 Kind.
- **Controller**: one timing engine per language from a single timeline; a cue-less
  reference engine seeds engines created mid-show (fixes the canonical-order case
  where Update Timeline precedes the RPL).
- **ACSP state machine**: deterministic Handle/Tick API (clock-injectable, so the
  scenario suite is fast + CI-able) plus a production Run loop over a Transport
  seam. Implements every §5.2 robustness behavior.
- **Simulator + 17-scenario quirk suite** (the kickoff's emphasized regression
  backbone): happy-path, mid-playback join, lease near/actual expiry, comms-loss-
  continue + then-expiry, Playout-ID=0/duplicate no-loopback, empty ReelResources,
  output-mode toggle/absent, show-change, rewind, pause/resume/stop, multi-language,
  malformed-asset-degrades, wrong-room fingerprint. Found + fixed two real bugs via
  the suite (mid-show engine seeding; show-change locator collision).
- Everything race-clean; `acsp-simulator` runs the suite standalone.

Next: audio pipeline (§6) — qualifier + mapper pure-Go tested; encode seam.

## Audio, security, admin, deployment, integration — session complete

- **Deployment (§12)** authored while a workflow ran: four profile descriptors +
  Loaner overlay + three systemd units encoding privilege separation and the
  no-core-dump/tmpfs hardening.
- **Admin (§8)**: first-boot setup wizard (forces credential creation, guards
  Complete, auto-expiry), Basic-Auth admin gate, and a dashboard rendering
  caption/audio readiness separately + the loud-warning set.
- **Audio (§6)**: used a **Workflow** to build + adversarially verify four
  independent leaf packages in parallel (qualifier, mapper, firewall, verify).
  The adversarial pass found two genuine MAJOR bugs — the qualifier overclaiming
  "noise normal" on a continuous calibration tone, and verify misreading a
  kernel-disabled IPv6 (missing sysctl file) as "forwarding enabled" — both
  fixed with regression tests, plus the firewall iface-name validation. Added
  the capture/encode seam (synthetic tone → real GStreamer Opus smoke) and the
  capture→map→qualify→encode pipeline.
- **Integration (§3.5, §10.6)**: readiness aggregator (independent verdicts +
  loud warnings); media helper serves audio status over IPC; firewall helper
  generates/verifies (apply gated behind root + flag); core drives a
  self-advancing ACSP demo over a memory-backed resolver and assembles the
  dashboard from live caption state + polled audio + local self-introspection.
  Verified the whole three-process system live.
- **Invariants (§11.6)**: a cross-cutting suite asserting all seven guardrails,
  run in CI.

Final: 28 packages, 119 tests, race-clean, ~71% coverage. All in-scope backlog
done; hardware-gated work recorded in BLOCKERS.md. The build is green and the
appliance runs end-to-end on synthetic/simulated inputs.
