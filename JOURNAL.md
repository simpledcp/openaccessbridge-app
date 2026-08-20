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

## Backend wiring pass (§3.5, §8.3, §9.3, §10.5, §10.8)

The previous wave built real capability that nothing called. This pass made the
running system use it:

- **cmd/captionbridge-core** grew a supervisor (`status.go`, `media.go`,
  `firewall.go`): it applies the profile-derived firewall ruleset through the
  privileged helper, runs the §9.3 verification pass through that helper (with a
  local fallback that fails loud rather than passing), pushes the §10.5
  auditorium input profile to the media process, reports per-process liveness,
  and persists a §10.8 readiness result from a check that actually ran.
- **Caption warnings reach the dashboard.** `acsp.Options.OnWarning/OnState` are
  wired to `readiness.CaptionWarnings`, a live set with measured clearing rules;
  the §8.5 wrong-room guard is armed from the stored fingerprint, and the
  dashboard now shows the CONFIGURED allowed server plus the connected one and
  the §10.5 identity-strength label.
- **cmd/captionbridge-media** implements `media.configure/start/stop`: the stored
  `channel_mapping` is parsed through `mapper.ParseMode`/`PlanFor` and rebuilds
  the streams. A real capture device is REFUSED (B3/B4) rather than served by a
  synthetic tone.
- **cmd/captionbridge-firewall**'s watchdog now re-asserts the posture on a flip
  (sysctls + the last ruleset core asked for), re-measures, and says plainly when
  it is not permitted to fix anything.
- **internal/readiness** fixed the `reasonHas("requires")` over-match, added the
  disconnected-device and unavailable-encoder warnings, surfaced process
  liveness, client counts vs the D11 cap, and the §10.8 record.
- **internal/caption/resolver** propagates timed-text parse warnings through
  `Loader.OnParseWarning` (classes and counts only), so parser degradation is no
  longer silent.
- **internal/config** reads and reconciles `config_schema_version` on open.

Verified live: three processes up, media reconfigured from the stored profile
(mono → L/R), firewall applied through the helper (dry-run, no root), the
dashboard reporting measured facts, and §10.5/§10.8 written from real passes.

### Review pass on the wiring lane

Re-verified each claim by mutation (revert, watch the test fail, restore). Two
real defects were found and repaired:

- **The media process was labelling a synthetic tone with the booth's hardware
  identity.** The B3/B4 refusal was gated on the `--synthetic` flag rather than
  on the profile's `input_device_id`, so with the flag at its default (what the
  systemd unit runs) a stored device id produced a tone reported AS that device —
  and core then persisted the resulting verdict as §10.5 `last_qualification` and
  §10.8 `audio_device_compat_profile`. The gate is now the profile's device.
- **Caption content warnings were cleared before anyone could read them.** They
  are raised from inside the window load, which happens while handling Set RPL —
  which can precede the first Update Timeline — so the playout-ID comparison filed
  them under the previous show and the next observation dropped them. Clearing is
  now a measurement: `Loader.OnLoadComplete` reports what every load found, and a
  load that finds nothing wrong is what retires the warning.

Verified live on this host: three processes up; `media.configure` refuses
`usb-Focusrite_Scarlett_2i2-00` by name and honours a device-less
`stereo:L=HI,R=VI-N` (HI←left, VI-N←right reaching the dashboard); the firewall
posture reports `firewall_introspected=false` with "nft list failed: Operation
not permitted", i.e. loud rather than green (B5); §10.8 records a code-only
failure reason from a pass that really ran. The firewall APPLY path was skipped
on this run — the store has no interfaces or allowlist — which is the documented
"awaiting setup" branch, not a confirmation that apply works end to end.

## Audit-driven correctness pass (picking the project back up)

The previous entry closed with "all in-scope backlog done." That claim did not
survive contact with the spec. A section-by-section audit of `buildplan.md`
against the shipped code — every section, with an adversarial verifier trying to
refute each finding by locating the code that already implemented it — confirmed
**78 gaps**. The verifiers did not merely read; several REPRODUCED the failure by
running the code. Two were showstoppers in the heart of the product:

- `loadReels(0)` was hard-coded, so the fetch-ahead window never followed the
  timeline: captions went blank after reel 1, and a mid-show reconnect fetched
  reels the show had already played.
- RPLs were "latest wins" rather than keyed by timeline segment, so a second
  `Set RPL Location` made the FEATURE's captions render during the trailer.

Neither was visible in the 17-scenario suite, because every existing scenario
sent exactly one RPL and one reel's worth of timeline.

**How the work was run.** Four disjoint package lanes in isolated git worktrees,
each followed by an adversarial review-and-repair pass whose job was to disbelieve
the fixer. That structure paid for itself immediately: the repair pass on the
caption lane found that the fix for "periodic status ignores its timeline state"
had introduced a live silent-wrong-content bug — a status carrying only
"playing" with no position re-anchored the show at 0, jumping every timing engine
to the head mid-show. It reproduced it (timeline 20500 → 0, caption "Late" → "")
before fixing it. Servers push play state more often than position, so that
would have been a field failure.

Then a second wave wired the new capability into the running processes, because
all four lanes independently reported the same thing: their work had no
production caller. Its review pass caught two more:

- **A fabrication.** The media helper gated its B3/B4 hardware refusal on the
  `--synthetic` FLAG rather than on the profile's device id, so with the flag at
  its default — what the systemd unit actually runs — a stored `input_device_id`
  produced a synthetic tone reported AS that device, and core persisted the
  resulting verdict as §10.5 `last_qualification`. A measurement of hardware
  nothing had opened. A test had actively locked the behavior in.
- **A warning nobody could ever see.** Caption content warnings were raised
  during the window load (which happens while handling Set RPL, before the first
  Update Timeline), so they were filed under the previous playout and cleared by
  the very next observation. Clearing is now a measurement, not an inference.

**Fixed directly, in packages no lane owned:**

- The privileged helper's loopback socket was unreachable as deployed: the
  firewall runs as root, core as `captionbridge`, and `ipc.Listen` created an
  owner-only 0600 socket — so core's dial always got EACCES and the dashboard
  would have shown a permanent "Firewall INACTIVE". Privilege separation
  silently disabling the security surface it exists to prove. Socket ownership is
  now explicit, with an SO_PEERCRED admission policy that fails closed.
- Setup mode never closed. `requireAdmin` opened the ENTIRE admin mux whenever no
  credential existed, with no reference to the window, so an appliance that
  booted and was never finished served `/admin` and `/api/admin/status`
  unauthenticated forever (§8.1 step 14).
- Two fail-open defaults: an un-provisioned appliance came up as *Lab*, the most
  permissive profile, and `Production()` returned false for an unrecognized
  profile name, which made `validateBindSeparation` skip the §8.4 check entirely.
- Setup completion stamped §10.8's `LastReadinessCheckOK` without running any
  check. It now gates on the readiness the supervisor measured, and leaves the
  stamp to whoever actually ran a pass.

**Built new:** the §6.4 calibration engine (verdict, plain-language summary,
concrete tier-aware fixes) and the §11.6 soak layer. Writing the calibration
engine produced a bug of exactly the kind the audit kept finding: comparing the
estimated noise floor to a threshold directly meant a QUIET TEST TONE — precisely
what §6.4 step 4 asks the installer to play, and which has no quiet windows for
the percentile estimator to see — was blamed on background noise and told every
such installer to fit a ground-loop isolator. It now requires a real
dynamic-range gap before attributing a floor to noise.

The soak's first two "leaks" were the harness, not the appliance: an accumulating
fixture map and unbounded per-locator fetch counters. Both were fixed rather than
tolerated — a soak that reports its own bookkeeping as a product leak is worse
than no soak. With them gone: 1500 show cycles, heap 399 KiB → 473 KiB,
goroutines flat, 1500/1500 verified content purges.

**Where it landed:** 31 packages, 374 tests, ~82% coverage, 30 ACSP scenarios,
green under `-race -count=2`, three processes verified running end to end.
Remaining software work is listed in STATUS.md under "Known remaining work" —
chiefly that the admin UI has not caught up with the backend facts now available
to it, and the §6.4 wizard has an engine but no screen.
