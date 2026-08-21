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

## Admin surface catches up (§8.2, §8.3, §8.4, §6.4)

Two waves had made the backend measure honestly; almost none of it was visible.
The dashboard rendered about half of §8.2's rows and none of the facts added
since — most sharply `firewall_introspected`, which meant the page printed
"Firewall: Active" identically whether the ruleset had been READ or merely
assumed. Two lanes: the dashboard, and a real screen for the §6.4 calibration
engine.

**The fabrication check earned its keep three times over.** All three defects
were live on the shipped page, all three passed their tests, and all three are
the same shape — a value that was never measured rendered as though it had been:

- **A synthetic tone shown as the booth's feed.** `captionbridge-media` marks
  generated inputs with a `synthetic-` device-id prefix, but the dashboard
  dropped `input_device_id` entirely, so the page rendered "HI · excellent ·
  -13.5 dBFS" and an audio verdict of READY with nothing saying the source was a
  generated tone. `--synthetic` defaults to true, so that was what every
  dashboard showed. Attribution now appears at all four places a verdict is read.
- **Drift 0 printed as a measurement.** `drift_ms_estimate` is an int64 with no
  `omitempty`, so the "not measured" branch was dead code; `MaxDriftMS` ranges
  over per-language engines and returns 0 by vacuity when no show is loaded. A
  DISCONNECTED caption path rendered "Drift at last re-anchor: 0 ms" — flawless
  sync for a path that is not running.
- **A cleared level printed as a reading.** `pipeline.Stream.fail()` clears
  `LevelDBFS` to the -120.0 sentinel, so a stream whose only reason was "not yet
  started" rendered as "-120.0 dBFS": a sentinel dressed as a measurement of
  digital silence.

Also found: `verify.Report.AdminIsolated` answers "does this binding meet the
ACTIVE profile's requirement", and Lab and Booth Diagnostic impose none — so it
read `true` for an admin surface bound to every interface, exactly the case §8.4
wrote its banner for. The dashboard now carries the raw reachability measurement.

**Fixed at the integration seam afterwards:** core never passed `--socket-dir`
through to the web layer, so the wizard reported "the media process is not
answering" for every device on any non-default socket dir. And both listeners
shared one embedded asset handler, so the PATRON bind served admin.html,
admin.js and the calibration wizard to the untrusted BYOD network — no measured
values, but the surface, and this pass had just grown admin.js from 3KB to 29KB
of booth diagnostics. Assets are now partitioned by surface with an allowlist,
and a test asserts every file in `assets/` is classified to exactly one of them.

**Also this session:** the §8.5 wrong-room guard was completed. Only the
fingerprint was compared; the screen name was received and discarded, which
matters because §10.5 notes the fingerprint may not exist on a given server
family — where identity is the weaker composite, the screen name is the only
wrong-room signal there is. The check now reports five outcomes rather than a
boolean, because "nothing pinned" and "pinned but unanswered" are both
UNVERIFIED and neither may render as a match.

**A process note worth keeping.** Agent worktrees were three times created from
the session's ORIGINAL commit rather than current HEAD. The first time cost a
merge conflict; the third time both lanes would have been building against a
tree where `internal/audio/calibration` did not exist and `firewall_introspected`
had never been added — the entire premise of their task absent. Caught it before
any work was wasted, and the workflow script now verifies its base commit as step
zero and resets if stale.

Where it landed: 31 packages, 472 tests, ~82% coverage, 32 scenarios, green
under `-race`, three processes verified running with the dashboard and the
wizard both exercised live.

## §6.7: the audio actually reaches a listener

Everything before this session ended at the encode seam. `internal/audio/encode`
could prove an Opus pipeline existed by running a 200 ms smoke test into
`fakesink` — audio encoded and thrown away — and `Deps.Signaler` was nil, so the
patron page correctly told everyone that listening was unavailable. The halves
were real and each had tests. Nothing connected them.

**What the shape had to be.** §6.7 states the topology as a requirement: one
capture/analysis/Opus source per NAMED STREAM, fanned out to one peer connection
per subscriber, and "the design must avoid re-running capture/analysis per
client". So: `pipeline.Stream` starts exactly one `encode.RTPStream` and
publishes it to a `DeliverySink`; `webrtcpeer.Hub` holds one
`TrackLocalStaticRTP` per stream keyed in a map, and every `Answer` binds
another peer to that same track. Adding the tenth subscriber adds an SRTP
session and nothing else. The test that defends it measures encoder reads and
delivered packets over the same window: with one source they match, and a
per-subscriber source would read the encoder about twice per delivered packet.

**How PCM reaches the encoder, and why.** The §6.7 spike ran `audiotestsrc`
inside GStreamer, but the real source is the MAPPED output of the pipeline —
capture, then the channel mapper that §6.5 makes the single place deciding which
captured channel feeds which named stream. Letting GStreamer reopen the device
and split channels again would put a second, untested mapping in the path, and
the qualifier's verdict would then describe a different lane from the one a
patron hears. So the source element is `fdsrc` on stdin and the appliance writes
the mapped lane in as S16LE, with `rawaudioparse` stating the caps explicitly
rather than letting a byte stream be guessed at.

Two things that cost real time and are worth remembering:

- **`perfect-timestamp=true` on `opusenc`.** Without it the RTP clock advanced in
  irregular steps (360 ticks, then 480, then 360) because `rtpopuspay` derives
  the RTP timestamp from the buffer PTS and a short read on the stdin pipe made
  those PTS ragged. With it, timestamps come from the sample count: exactly 480
  ticks per 10 ms frame, forever. Ragged RTP timestamps are precisely the jitter
  §6.7's dominant phone-side term has to absorb.
- **The synthetic tone had to be paced.** `capture.SyntheticTone` generated
  frames as fast as the consumer read them, which was harmless while nothing
  consumed them but a live encoder. Feeding an unpaced generator into Opus would
  have handed a phone hours of audio in minutes — and every wall-clock figure
  measured around it (capture gaps, dropouts, throughput) would have described
  the generator instead of a booth feed. `Realtime` paces against a running
  deadline, and resets rather than sprinting to catch up after a long stall.

**Honesty, which is the part that matters.** `encoder_state:"streaming"` is set
only after RTP packets have been READ OUT of the encoder and counted. An encoder
that spawns and emits nothing is reported `unavailable` after a one-second
deadline, its source unpublished and its subscribers dropped — because "the
process started" is not "audio is being encoded", and that distinction is the
whole difference between the five fabrications this codebase has produced and an
honest state. Patron availability then follows that measured state: a perfect
input with a dead encoder is offered to nobody.

The other honesty line is louder now than it was. The stand-in tone used to be a
number on a dashboard; it is now something a person hears. So it is declared on
the PATRON surface too — "You are hearing a test tone, not this auditorium's
audio" — and `ontrack` deliberately does not clear that message when audio
starts flowing, since clearing it is exactly how a generated tone would quietly
become "the auditorium's HI feed".

One claim got demoted along the way: core used to log `webrtc.peer.connected`
when it relayed an answer. It cannot see whether ICE completed. It now logs a
state transition for the relay, and `webrtc.peer.connected` is emitted only by
the process that owns the peer, from the peer's own state change.

**Measured on this host** (`internal/audio/encode`, real gst-launch): RTP
timestamps step exactly 480 ticks per packet; the encoder path holds ~0 frames
of backlog in steady state; the wall delay from writing a 10 ms block to reading
the packet carrying it is ~0.4 ms mean / ~0.5 ms max. That covers ONE row of
§6.7's latency table. The dominant row — the phone-side jitter buffer, 40–300+ ms
— cannot be measured without a phone (B2), so the dashboard still says "not
measured" for glass-to-ear and prints the encode-path estimate under its own name.

**Proven end to end**, because two halves that each pass their own tests is the
failure this codebase has produced repeatedly:
`cmd/captionbridge-core/audio_e2e_test.go` builds and runs all three binaries,
then joins as a patron would — the same protocol `assets/patron.js` speaks, a
receive-only Pion peer with `iceServers: []`, an offer over `/api/audio/ws`, the
answer applied — and asserts Opus RTP arrives, that the answer carries `typ host`
candidates and no `srflx`/`relay`/`stun:`/`turn:`, and that the surface declares
the source a test tone.

## Review-and-repair pass on the §6.7 audio path

The §6.7 build was re-run and re-checked rather than taken on trust: the
end-to-end proof was executed here (25+ Opus RTP packets at a Go subscriber over
the real three-process appliance, plus a hand-written client against a manually
started appliance), and each "done" was mutated to see whether a test actually
fails without it. Most held. Three things did not, and one of them was a real
mid-show fault.

- **A stale peer transition took down the patron's live peer.** `Hub.onPeerState`
  was keyed by peer ID, and pion dispatches state changes on its own goroutine.
  A patron switching HI → VI-N re-offers on the SAME control socket, so under the
  same peer id: the peer they left was closed, and its late "closed" callback
  found the id in the map — now held by their NEW peer — evicted it and closed
  it. The patron's audio stopped for the rest of the show, for changing feeds.
  Reproduced deterministically before fixing. The callback now closes over the
  peer OBJECT and every mutation is identity-guarded
  (`TestAStaleTransitionCannotTouchTheCurrentPeer`).
- **D11's client cap could be walked straight past.** Peers were admitted to the
  hub only after ICE gathering, so the capacity check counted answered peers but
  not the ones mid-negotiation. Under the arrival pattern a real auditorium
  produces — everyone scans the QR at once — 6 of 6 concurrent offers were
  admitted against a cap of 2 (measured). Admission now happens under one lock
  with the check, before negotiating, and every failure path takes the peer back
  out (`TestSubscriberCapHoldsUnderConcurrentOffers`).
- **The log's §6.6 listener count only ever went up.** A clean departure logged
  nothing, so a post-show log read as though every phone that ever connected was
  still listening. `ClosePeer` now records the departure with the resulting count
  (and still no peer id — §9.4).

Three claimed behaviours were correct but unpinned; they now have tests that fail
without them: an encoder that emits nothing must never be reported "streaming"
even inside its one-second proof window (the previous test only checked the end
state, so reporting healthy-on-launch passed); the media process must hand its
stand-in source to a live encode PACED (`Realtime`), which was only a comment;
and §6.7's topology is now checked against real processes — two subscribers to
one stream, and the number of `gst-launch` children must equal the number of
named streams being served, not the number of clients. That last one catches the
shape the plan rules out: with a deliberate second encoder per stream it reports
"4 encoder children for 2 streams being served".

Checked and found honest: availability is gated on a measured RTP packet count;
`latency_measured` is still false and the dashboard still says "not measured"
beside the encode-path estimate under its own name; the connected-listener count
moves 0 → 1 → 0 with a real subscriber (a constant fails the E2E); the synthetic
tone keeps its attribution on both the patron and admin surfaces. No SDP body,
ICE candidate, ICE ufrag or peer id appears in any of the three processes' logs
on a live run; no audio reaches disk; there is no STUN/TURN or internet-reachable
configuration anywhere in the tree.

One claim did not reproduce: `perfect-timestamp=true` on `opusenc` was reported
as necessary to keep the RTP clock frame-spaced, but with the current
`fdsrc blocksize` the timestamps are exactly 480 ticks per packet with or without
it, under paced and bursty writes alike. The flag is correct and harmless, so it
stays — but it is not load-bearing and nothing fails without it.

## §11.4 latency pass — measuring the appliance-side floor, and naming the rest

§15 names HI latency as the plan's largest risk and §6.7 flags the sub-150 ms
target as "aggressive and may be unachievable". Until the §6.7 WebRTC path
existed there was no peer to measure against, so every row of that latency table
was a quoted typical. `internal/audio/latency` (`make latency`, plus a CI-sized
run in the normal suite) turns the rows this host CAN reach into readings and
states the boundary of the claim everywhere a number appears.

**It measures the production path, not a copy of it.** The harness runs the real
`pipeline.Stream` with the paced synthetic source, the real `gst-launch` Opus
encoder, and the real `webrtcpeer.Hub`, and takes its timestamps from a tap
wrapped around the `encode.RTPStream` seam. A harness that re-implemented the
chain would report a delay no patron's audio ever travelled through.

**Correlation, which is the load-bearing part.** Each RTP packet is joined to its
PCM block by ordinal (the encoder is FIFO with DTX off, so the Nth accepted block
is the Nth packet) and to its arrival at the subscriber by RTP timestamp, which
`TrackLocalStaticRTP` preserves. Neither is taken on faith: a write ordinal is
recorded only if `StreamStats.FramesIn` advanced, so a dropped block cannot shift
every later packet onto the wrong write; the RTP sequence number is checked on
every packet and a gap RESYNCHRONISES the ordinal (a silent one-frame drift would
understate the floor by a whole Opus frame); and a join that produces a negative
transit aborts the run rather than reporting a shorter floor than the appliance
has. A spike before writing any of it confirmed the shape: correlating by RTP
timestamp naively is off by one, because `opusenc`'s FIRST timestamp stride is
360 ticks rather than 480. That 120-sample gap is the encoder's pre-skip, and it
became a measurement in its own right (2.5 ms) instead of a bug.

**What came out**, 30 s window, 3000 joined packets, 0 dropped blocks, 0 resyncs
(p50 / p95): channel map 0.8 / 4.4 µs; level analysis 119 / 234 µs; Opus encode
transit 416 / 524 µs; WebRTC fan-out transit 111 / 143 µs; appliance floor 531 /
651 µs. Composed with the structural 10 ms framing term, the appliance-side floor
is ~10.7 ms. Percentiles are nearest-rank, never interpolated, so every printed
duration is one that actually happened; no mean is offered, because a mean hides
exactly the jitter this pass is about.

Two of those readings correct quoted typicals rather than confirming them. §6.7
budgets "~10–20 ms" for Opus encode at a 10 ms frame; measured, the encoder's
wall transit is half a millisecond and the 10 ms is entirely the framing delay of
having to accumulate a frame before one can be emitted — so the row is
structural, not processing cost, and shrinking it means shrinking the frame, not
optimising the encoder.

The second correction is a real (small, out-of-lane) defect, left unfixed and
recorded here. `pipeline.EncoderPathLatencyMS` reports capture period + Opus
frame — 20 ms at the shipped settings — but the two are the SAME accumulation
when the capture period is a whole number of Opus frames, which is the shipped
case: one 10 ms capture frame is handed straight to `WritePCM` and becomes
exactly one Opus frame. Measured, the whole appliance-side path is ~10.7 ms, not
20. The estimate is conservative rather than flattering, which is the safe
direction for a latency budget, so it was NOT changed in this pass — but it is
double-counting, and the harness is the first thing able to say so. Worth a
decision alongside the real ALSA period, where a period LONGER than one frame
does add a genuine second term.

**What it deliberately does not say.** This is a Go-peer-to-Go-peer measurement
on one machine. It is a FLOOR for the appliance's own contribution and says
nothing about glass-to-ear, and the report refuses to be quoted without that: the
headline sentence, the "still a floor" label on the composed total, and the full
list of unmeasured terms — ALSA buffer (B3), Wi-Fi, the phone jitter buffer
(B2, flagged as THE DOMINANT TERM), Opus decode, phone audio output (B2) — are
pinned by tests that fail if any of them is dropped. Nothing is compared to the
100–150 ms HI target as though the target had been met. `latency_measured` on the
dashboard is still false.

The sharpest thing this pass produced is a change in where the risk sits: the
appliance spends under a millisecond of transit, so essentially the whole HI
latency question now lives on the phone side that B2 gates. B2 and B3 were
updated to say so.

Five behaviours were mutation-confirmed (reverted, watched fail, restored): the
ordinal resync after a sequence gap; the negative-transit refusal; the pre-skip
guard that claims nothing from a ragged RTP clock; the Summary headline that
bounds the claim; and the `dur(0)` rendering that makes an unmeasured stage read
as "-" rather than as 0 ms.

## Confining ICE gathering to the patron's network (§6.7, §9.1, D28)

D27 recorded a known limit and left it: pion gathers on every non-loopback
interface, so the SDP answer handed to a patron carried a host candidate for each
one. The reason it survived two passes is that it violates nothing — no STUN, no
TURN, nothing leaves the venue — and on a single-NIC appliance it is invisible.

The first thing done was to stop describing it and measure it. On this build host
the appliance answered a patron with **16 candidates across 5 networks**: the LAN
NIC and its eight IPv6 privacy addresses, a WireGuard address, BOTH Tailscale
addresses, `docker0`, and a docker bridge. Written down that way the cost stops
being hygiene. ICE tries the pairs it is given; a candidate on a network the
phone cannot route to does not fail fast, it fails on a timeout — and it fails
for every phone in the room during the ninety seconds when an auditorium's
patrons all scan the code at once. §6.7 spends its whole budget on milliseconds,
and §11.4 has just finished proving the appliance's own transit is under one. The
second cost is that the answer describes the venue's private topology to anything
on the patron LAN.

**What it is scoped to, and why a name.** `config.SettingClientInterface` — the
interface the setup wizard already records — pushed core → media over a new
`media.network` message. A NAME rather than an address, because a patron NIC
renumbers without becoming a different NIC. It is deliberately not folded into
`media.configure`, which carries the §10.5 audio profile: correcting a network
setting must not restart the room's audio.

**The failure mode this could have introduced, and what was done about it.**
Filtering to an interface that is absent or down does not produce an error — it
produces an answer with no host candidate in it, which a phone cannot tell apart
from a dead network. The patron gets a spinner and the booth gets nothing. So the
interface is re-checked on EVERY answer, not only when the scope is set (it is a
physical thing that can be unplugged between one patron and the next), the offer
is refused by name, and core raises it as a critical dashboard warning. A rejected
scope also does not take hold: the appliance keeps gathering the way it did rather
than silently serving nobody.

**Three facts, kept apart.** Configured, in force, and — the only measurement of
the three — the interfaces that appeared in the last answer this appliance really
emitted, read back out of that SDP. Deriving the third from the first is exactly
how a broken appliance would look identical to a working one, which is the shape
this repo has already caught twice. An appliance nobody has joined yet reports
"nothing offered to measure" rather than a verdict on an offer never made, and an
appliance with no configured interface reports "not confined" with the networks
named, so the mechanism's existence can never read as confinement.

**Proven end to end, not in-package.** `TestAudioPathEndToEndAcrossTheThreeProcesses`
now drives the whole chain with the real three processes: with no setup run, a
patron receives Opus and the dashboard reports **5 networks offered, not
confined**; the test then configures the patron interface through the operator's
own setup API, and the next patron's answer carries candidates on **exactly that
one interface**. Writing that test found a real property worth stating: the
dashboard's copy of the measurement is refreshed by the 15 s posture reconcile,
not by the join, so the row is one reconcile behind the answer it describes —
the same freshness every other posture row has. The confinement itself applies to
the very next answer. That is now in STATUS's remaining-work list in its own
words rather than being smoothed over in the test.

Three behaviours were mutation-confirmed (reverted, watched fail, restored): the
gathering filter itself (removing it put `wg0` back in a scoped answer and took
the post-startup narrowing from 1 interface back to 5); the report reading the
emitted SDP rather than the configured intent (reporting intent made a hub that
had answered nobody claim it had offered `eno1`, and made an unconfined hub that
really offered 5 report 0); and the per-answer usability check (dropping it made
a hub scoped to a missing interface answer successfully — with nothing in it).

`internal/invariants` gained the wiring assertion the house rule requires: the
package half of this work protects nobody unless core actually pushes the
interface, and that push is now pinned by source text with the load-bearing
literals named in the test's own comment.

## The §10.4 QR, and how an unreadable artifact was made checkable (D29)

Two earlier passes declined to render a QR, and left the reason in the code:
"rendering a QR that no offline tool on this build host can decode would be an
artifact nobody has verified scans". That was the right call on the facts
available. This pass changed the facts rather than the standard.

**The build host can now decode.** `zxing-cpp` — the C++ ZXing engine behind a
great many phone scanner apps — installs as a self-contained wheel, and
`python-qrcode` and `segno` provide encoders with no relationship to this repo.
That makes an independent oracle available for both directions: something that
produces reference symbols, and something that reads ours back.

**What is verified, and how far it goes.** Every module of every symbol is
compared against python-qrcode references — all eight data masks for a corpus of
boundary cases, and **all 160 version/correction-level combinations at their
exact byte capacity**, so the ECC block tables are pinned rather than
spot-checked (the capacities themselves are bisected out of the oracle rather
than copied from the table under test). Every reference was decoded by zxing-cpp
before it was committed. And `make qr-verify` decodes the PNGs the appliance
really serves, at the scales it serves them: 42/42.

**Three implementations were needed, because two disagreed.** The first fixtures
came from segno, and this encoder disagreed with it on every case — always in the
padding, always by exactly one codeword. Rather than "fix" our code to match, the
codeword streams were de-interleaved and compared: segno emits an extra `0x00`
before the pad codewords. Its `write_padding_bits` extends by `8 - (length % 8)`
with no outer `% 8`, so a stream that already ends on a codeword boundary — which
in byte mode it always does — gets a whole extra byte. ISO §7.4.10, quoted
verbatim in segno's own docstring immediately above that line, adds padding bits
only when the stream does not end on a boundary. python-qrcode then agreed with
this package module-for-module and disagreed with segno by 146 modules on the
same input. The deviation is harmless — both decode — but the episode is the
reason the fixture generator names three implementations and records which one is
the authority.

**Mask selection is where implementations legitimately differ**, and the fixtures
are shaped around that rather than papering over it. The penalty rules rest on
things the standard leaves open: whether the quiet zone supplies the light margin
for a finder-lookalike at the symbol edge, and whether the format information is
in place while masks are being scored (python-qrcode blanks it during evaluation;
ZXing and Nayuki do not). So the fixtures pin ALL EIGHT masks per case, and the
guarantee became: whichever mask this encoder picks, its output is byte-identical
to a symbol an independent decoder has already read. That is the property that
matters at a door, and it needs no agreement about aesthetics.

**A number, not an adjective, for the printed size.** A longer join URL needs a
larger version, more modules, and therefore a physically BIGGER card to stay
scannable — a relationship that is invisible on a screen, which is exactly where
a booth would get it wrong. `RecommendedPrintWidthMM` derives a minimum width
from the symbol actually being printed and the admin page prints it beside the
code: for a typical join URL, 46 mm.

**On the operator surface**, the QR is served only from the admin listener (it is
a bearer credential for the show — anything that can read it can join), with
`no-store`, because a cached per-show QR is precisely §8.5's stale-code failure
and it fails in the worst way available: the scan succeeds and the patron lands
in the wrong room. With no live show the route is a 404 rather than a blank
image, so an operator holding a card to a phone can tell "no session" from "a QR
that will not scan". The typed join code is rendered at least as prominently as
the image: not every patron can scan, and the code is the accessible path rather
than the fallback.

Verified live: with the three processes running, issuing a show and fetching
`/api/admin/session/qr.png` returns a 456x456 PNG that zxing-cpp reads back as
the exact join URL, with `Cache-Control: no-store`.

Four behaviours were mutation-confirmed (reverted, watched fail, restored): the
ECC-level format bits (numeric order instead of the spec's L=01/M=00/Q=11/H=10
broke 18 of the fixture comparisons); the `no-store` header; and encoding the
short join code instead of the full join URL. The fourth mutation is the useful
one, because it found a defect in the TEST rather than the code: removing the
quiet zone did not fail, because the test measured the margin against the very
constant it was policing. It now asserts against the literal 4 the standard
requires, and fails as it should.

**What this does not clear.** B2. No phone camera, no venue lighting, no printed
card, no lobby. A scanner library reading a PNG on a build host is not a patron
scanning a card in a dark foyer, and nothing on either surface says otherwise.

## §11.5: a pre-show checklist that refuses to tick a box nobody checked (D30)

The readiness measurements have existed for several passes. What §11.5 asks for
is different from a dashboard, and the difference is the second half of its
sentence: "define **what staff do when a check fails** before a show — readiness
checks are only useful if they're tied to a procedure." A page that says
`audio_ready: false` has moved the problem to a person who now has a red line
and no instruction.

So every item carries a remedy, and the remedies are specific enough to act on:
the wrong-room item says *stop*; the caption-assets item distinguishes "the
playlist is not loaded" (ask the projectionist) from "the playlist is loaded and
its assets are not" (a content problem in the DCP, and the show will play with
blank captions); the seat-level Wi-Fi item says that working at the rack and
failing in the seats means AP placement, which is §9.5's named failure.

**Three states, because two would have to lie.** On this host the firewall
ruleset cannot be read without root, so "firewall active" is neither pass nor
fail — it is *not checked*, and it blocks the house exactly as a failure does. The
latency item is a fourth thing again: nothing on this appliance can ever measure
glass-to-ear latency (B2/B3), so it reads CANNOT BE CHECKED HERE and does not
block, because refusing to open a house over a measurement that cannot be taken
here would make the sheet unusable, and an unusable sheet is an ignored one.

**The sharpest line is the audio one.** A synthetic test tone qualifies
*perfectly* — present, streaming, excellent. Passing that item would send a booth
to open a house on a 440 Hz tone. It reports CANNOT BE CHECKED HERE and names the
tone.

**Three items are the staff's, and the appliance records nothing about them.**
Loading the patron page from a seat, checking the admin surface from the patron
SSID, and the loaner kit. It prints the step and the pass criterion, and stores
no attestation — deliberately. A tick the appliance cannot verify, stored and
shown later, is indistinguishable from a measurement and ages far worse: last
week's tick would sit on tonight's page. That exact bug has already shipped here
once (a §10.8 stamp written by pressing "Complete"), which is why the sheet ends
with a line saying in words that the appliance does not know whether those steps
were done.

There is no single "READY" verdict for the same reason. `ApplianceReady` is named
for what it covers; the appliance cannot see the seats.

The printable form is the actual deliverable — wrapped to 78 columns, remedies
hanging-indented under the items that are not passing and only those, readable on
a phone in a dark projection booth and printable for a binder. It works when the
appliance is the thing that is broken.

Four behaviours were mutation-confirmed: an unrun blocking check counting as a
pass; a synthetic tone passing as this room's audio; a staff step reported as
done; and the printed sheet dropping the line that says the staff steps are not
recorded. The third of those is worth noting for a different reason — the first
attempt at that mutation silently did not apply, because the pattern no longer
matched after gofmt, and the test "passed". A mutation that does not apply proves
nothing, so it was re-run with the target asserted before the edit.

## §9.4 support mode: a window with a deadline, and the rule that could never be removed (D31, D31a)

Support mode was a settings key the dashboard read and nothing wrote. That is
worth stating plainly, because a boolean is not a small version of this feature —
it is the failure mode of it. §9.4 makes support mode the only way back into an
appliance whose SSH is disabled, and a persisted flag cannot carry a deadline:
set once, it stays set. The feature whose job is to avoid a permanent way in was
implemented as one.

**What a session is now.** An absolute deadline, clamped to four hours whatever
is asked for, enforced by a timer AND re-checked on every read so no caller can
see a window whose time has passed. It signals the supervisor on open and close
so the firewall changes at that moment rather than at the next posture pass — the
deadline is the promise the feature makes. It does not survive a restart: a
window that came back after a boot would outlive the person who opened it and the
reason they gave, and the trail records those as `ended_by_restart` rather than
losing them.

**The rule is real, because the output chain is `policy drop`.** Opening a
session adds exactly one line: one interface, one address, one port, in the
OUTPUT chain and nowhere else. A partial endpoint is refused rather than
rendered — the only two ways to render a window with no address are a bare `oif
<iface> accept`, which is an unrestricted egress hole opened by a support switch,
and silence, which reports a window that permits nothing. The host must be a
literal IP: resolving a name would need DNS from an appliance whose §9.1 checks
exist to catch DNS leaks, and a name that resolves differently tomorrow is a rule
that quietly changes what it permits.

**The audit is a precondition, not a side effect.** If the trail cannot record
the session, the window does not open. The reason is required — an audit entry
without one is a timestamp — and it is stored locally and never logged, because
it is operator free text that could contain anything. Its LENGTH is logged, so a
trail of empty reasons is visible without the text travelling. The trail is
rendered beside the switch, because a support mode whose history lives only in a
database is one nobody reviews.

**A window is not a connection.** This appliance has no tunnel client and no
support endpoint of its own; it permits an outbound destination and dials
nothing. `TunnelEstablished` is a field on every response, always false in this
build, and the dashboard RENDERS that field rather than asserting the text — so
nothing can drift into reporting a connection the appliance never made. The
existing dashboard test caught the first version of that renderer doing exactly
that: it printed "none established" as a constant, and the guard that every
measured fact must be read from the API failed it.

## The defect found on the way: a ruleset that could never remove a rule (D31a)

Support mode needs its rule to disappear when the window closes, which is what
sent me to look at how the ruleset is applied. `nft -f` on a table definition has
implicit `add` semantics: applied at a table that already exists, it appends
every rule again and leaves whatever was there before. The generated script had
no flush and no delete, and had not needed one until now.

The consequence is not duplicate lines. **A configuration change could not remove
a rule.** The previous cinema server's allow rule would survive the address being
corrected. The admin-on-patron-interface rule would survive a switch from Booth
Diagnostic to Patron-Facing — the stricter posture applied, the looser one still
accepting, and §9.3's introspection finding every expected rule present and
reporting the posture verified. That is the shape of hole this repo keeps looking
for: it reads green.

The fix is the standard idiom — a bare `table` line so the delete cannot fail on
a fresh boot, then `delete table`, then the definition, all inside one `nft -f`
transaction so there is never an instant without a default-deny.

**What is NOT proven here, and it matters.** Applying nftables needs root (B5),
and this host has user namespaces disabled (`unshare -rn` is refused), so there is
no way to demonstrate either the append behaviour or the fix against a live
ruleset on this machine. The defect is established from nft's documented
semantics and the fix is asserted in the generated script text. It goes in STATUS
as something to confirm the first time the helper runs as root on real hardware.

Five behaviours were mutation-confirmed: the duration clamp; sessions surviving a
restart; the reason reaching the log; the egress rule moved into the INPUT chain;
and the table delete removed — the last of which failed the two replacement tests
and the wiring invariant together.

## §9.4 signed offline updates: verifying what cannot yet be installed (D32, B8)

The update path splits cleanly into two halves, and only one of them can exist
on this image. Verifying a signed bundle is pure computation and is now complete.
Installing one needs two system slots and a bootloader that honours a boot-target
contract, and this image has neither — so nothing here stages, installs or rolls
back, and the surfaces say so as a field rather than as prose that could be
dropped.

**The half that is real.** A bundle is a manifest and a payload; the signature
covers the manifest and the manifest binds the payload by digest and size, so one
signature covers both without the signer streaming the image. Verification is
over the exact manifest BYTES that arrived, the key is looked up in the
appliance's own store by the id the manifest names, and the payload is hashed
only after the signature passes — so a bundle nobody trusts cannot make the
appliance do work proportional to its size.

The sharpest test in the package builds a bundle signed by an impostor **under
the same key id as the real vendor**, checks that it verifies perfectly against
the impostor's own key, and then requires the appliance to refuse it. A bundle
that carries its own key and is checked against it is the commonest way a signed
update path turns out not to be one.

**Ordering is an integer.** Is "1.10" newer than "1.9"? The release process owns
a monotonic build number. A downgrade re-introduces every fault the newer build
fixed and a re-install is what a replay looks like on an offline path, so both
are refused unless an operator explicitly asks.

**Rollback is real only because the new image confirms ITSELF.** `ConfirmHealthy`
takes the running slot and refuses if it is not the staged one, so the old image
cannot bless the new one. An updater that marks its own work healthy has an
automatic rollback that fires only when nothing ran at all — and the failure
worth catching is the update that boots, comes up, and cannot reach the cinema
server. Attempts are counted before the boot, so an image that hangs instantly
still exhausts its tries. The limit is three rather than one: a single failed boot
can be a power cut, and rolling back a good update because someone switched the
rack off is its own outage.

**Verified is not installable.** Both facts travel separately to the dashboard,
which renders both, because letting "this bundle is good" read as "this update
can be applied" would leave a booth waiting for something that cannot happen.
What an operator gets today is still worth having: the failure this prevents is
discovering at 19:45 that the stick they were sent is the wrong build, is for
another tier, or was never signed by anyone this appliance trusts.

**A mutation that did NOT fail, and what it changed.** Verifying over a
re-serialised manifest instead of the raw bytes passed every test. Chasing that
down, the comment justifying the raw-bytes choice turned out to be wrong: it
claimed re-serialisation would let a signature pass over content nobody signed,
and it would not — a canonical re-encoding of the parsed struct cannot change
meaning. The real hazard is different and worse in a quieter way: re-encoding
ties the appliance to one JSON encoder's field order, so a signer written in
another language produces bundles this appliance rejects, reported as "bad
signature" — the same words a tampered bundle gets. There is now a test with a
hand-written manifest in a different key order, signed as written, which fails
under exactly that mutation. The claim and the test now match.

Five behaviours were mutation-confirmed in total: byte-exact verification (after
the fix above); trusting a key the appliance does not hold; the downgrade and
replay refusals; the old image confirming the new one's health; and "verified"
being reported as "installable".

## The review pass on those five slices: 17 confirmed, and one lesson repeated three times

Five reviewers, one per slice, each finding then independently refuted by a
skeptic told to default to "not a defect". 19 findings, 17 confirmed, 2 refuted.
The refutations were as useful as the confirmations: a missing audio row IS
honestly a blocking "not checked" (the reviewer wanted it non-blocking), and the
missing `Cache-Control` on `/api/admin/session` really is cosmetic, because the
QR route that serves the same token already forbids caching.

**Three of the confirmed defects were the same defect.** Support mode's endpoint
had three settings with a reader and no writer, so no operator could open a
session on an appliance they had actually set up — I proved that myself during
the live check and did not notice, because I reached for `sqlite3` to write the
settings by hand and treated that as a test-harness detail rather than as the
finding it was. The update path's build number was never stamped by any build, so
`Current.Build` was always 0 and the downgrade refusal was dead code. And
`Current.Tier` was hardcoded empty, so the tier gate could never fire and a
hardened-tier bundle verified anywhere.

All three are this repo's own rule — "a capability with no production caller
protects nobody" — pointed at CONFIGURATION rather than at code, which is where I
had not thought to point it. `internal/invariants` now asserts that the support
endpoint settings have a writer and a route, and that the Makefile and CI stamp
the build number.

Fixing the build number surfaced a second decision. A binary that does not know
its own build cannot tell an upgrade from a replay, and both defaults are wrong
in the way this repo cares about: treating it as 0 makes every bundle look newer
(a downgrade to a version with a known fault installs silently), and treating it
as enormous makes nothing install (and an operator who cannot verify is tempted
to install unverified). It now REFUSES with that reason. The same applies to an
unset hardware tier: a bundle that targets one is refused rather than accepted on
a guess.

**Three findings were tests that did not test their own names.**
`TestTheRequestedCorrectionLevelIsTheOneEncoded` compared a struct field against
the argument that set it — it passed with `formatBits()` stubbed to a constant,
which would have announced the wrong error-correction level to every phone that
scanned. It now reads the format information out of the rendered matrix, where a
scanner reads it, against literals rather than against the function under test.
The mask-selection logic could be deleted entirely and everything stayed green,
because the only test of the choice compared the encoder against its own scoring
function; the penalty rules are now checked against hand-built lines with known
scores, and the selection against a mask it rejected. And the media
scope-refusal log test asserted neither half of its name — it now requires the
interface to appear and every local address to be absent.

**The sharpest single finding.** `PatronScope.usable()` accepted any interface
holding a non-loopback address — but RFC 8445 §5.1.1.1 forbids gathering host
candidates for IPv6 link-local addresses, and pion enforces it. An interface that
is UP with only an `fe80::` address (a patron NIC between `netplan apply` and its
DHCP lease landing) passed the check and then gathered NOTHING, and the patron
was handed an SDP with no host candidate — the exact artifact the whole
`ErrPatronInterfaceUnusable` machinery exists to prevent. Worse, every reporting
layer then painted it green: the warning fired only on "more than one interface",
so zero was as quiet as one; the §11.5 item read the intent flag rather than the
measurement it was labelled `SourceMeasured` from; the dashboard row computed
STATE_OK. The skeptic demonstrated it against a real interface on this host
rather than a fixture.

Three fixes: `usable()` rejects link-local-only interfaces, `Answer` refuses an
SDP that carries no host candidate instead of shipping it, and all three
reporting layers now COMPARE the measurement to the scope in force — which is
what `model.ICEScopeReport`'s own documentation said the pair existed to do, and
what the first implementation did not do.

Two smaller ones worth naming: a support session whose endpoint was reconfigured
under it moved the firewall rule while the trail, the log and the dashboard went
on naming the old address — it now ends the session instead, because moving the
window is a new decision that deserves its own audit row. And `SetPatronScope`
logged a state transition on every one of core's fifteen-second pushes, filling
the log a review reads with events that did not happen.

## Two decisions that had been waiting: the latency double-count and the two-device room

Both were recorded as "worth a decision" rather than as defects, which is why
they survived several passes. Making the decisions turned out to be most of the
work; the code was small.

**The estimate was not conservative, it was wrong.** `EncoderPathLatencyMS`
returned capture period + Opus frame — 20 ms at the shipped settings — and the
previous pass left it alone on the grounds that erring high is the safe direction
for a latency budget. It is not the safe direction when the number is simply
untrue. The captured block is handed straight to the encoder and becomes exactly
one Opus frame: one wait, not two. §11.4 had already measured the whole
appliance-side path at ~10.7 ms and was the first thing able to say so.

The correct model is the smallest whole number of capture periods that holds at
least one Opus frame, which is a CEILING rather than a `max` — with a 7 ms period
and a 10 ms frame the encoder waits for a second block, so the answer is 14 ms,
not 10. That case is in the tests precisely because `max` is the plausible wrong
answer, and it is one of the two mutations confirmed here: reverting to the
double-count fails three cases, and substituting `max` fails exactly the 7/10
one.

The estimate got SMALLER. That is worth stating plainly, because a padded budget
does not just look cautious — it misdirects the effort. §15 names HI latency as
the plan's largest risk, and §11.4 has since shown essentially all of it lives on
the phone side.

**A venue with two USB dongles could not be configured at all.** §6.5 lists
"Multiple USB inputs: Device A = HI, Device B = VI-N" as a supported mapping; the
mapper implemented it; the calibration wizard offered it in the picker. And then
`AuditoriumProfile` had one `input_device_id` to save it into, so the room could
be measured and never recorded.

The wizard's refusal to run the multi-device plan against a single device STAYS,
because that refusal was right: it would have reported both feeds as measured
when only one input was ever opened. What was missing was the record, and the
flow that fills it. A pairing is now built one device at a time — each half
calibrated with a single-stream mapping and saved with `part_of_pairing` — and
the response says which stream is still unassigned, because silence there is
exactly how the second device gets forgotten. The first save now says "This room
is NOT finished: VI-N still has no input device".

Three ways to record it wrongly are refused rather than stored: a stream with no
device, both streams on one device (which is a channel mapping wearing a
pairing's clothes, and hides which channel each stream takes), and per-stream
devices left behind on a single-device mapping. That last one is the stale-state
bug this kind of change usually ships with, and going back to a single-device
room now clears the pairing.

The sharpest detail is in the media process: it checks EVERY device the profile
names against its capture backend, not just the first. A pairing naming one
synthetic device and one real one would otherwise have had half of it served by a
tone under the real device's identity — which is the same fabrication the
single-device refusal was written to prevent, arriving through a door that did
not exist when that refusal was written. Both are mutation-confirmed: checking
only the first device lets the half-real pairing through, and attributing every
stream to `InputDeviceID` puts both feeds under the wrong device.

None of this clears B3/B4. There is still no capture backend for a real device,
and every one of these paths refuses one.

## §9.5's Wi-Fi code, and the question it was really waiting on (D35)

This was recorded as "not built" because it is not a rendering job: it turns on
whether the appliance should hold a network credential at all. Answering that was
the work.

**It should, and the reason is what kind of credential it is.** A patron
network's passphrase is given to every person in the auditorium and printed on a
lobby sign. It is a shared access token for a deliberately untrusted BYOD network
the appliance does not even operate. Refusing to store it would block a stated
requirement in order to protect something that is public by design. What it is,
though, is a credential this appliance did not previously hold — so it is never
logged, never returned in the status document a dashboard polls (that reports
`password_set`, not the value), never on the patron listener, and cleared when
the network is cleared so no passphrase outlives its network.

**One QR cannot be both a network join and a URL**, so this is a second code
printed beside the show's. §9.5's "one scan" is really "one scan each", and
saying so beats shipping a code that does neither.

**The interesting part was the escaping, and it was settled by cross-checking
rather than by reasoning.** The `WIFI:` grammar is de facto — ZXing's — not a
standard. My first implementation escaped the comma, reasoning from the related
MECARD contact grammar where it separates fields. Comparing against an
independent implementation showed it escapes `\\ ; " :` and not the comma, and
thinking it through afterwards the asymmetry is the point: an UNNECESSARY escape
is the more dangerous error, because a parser that does not strip it hands the
phone a passphrase with a literal backslash in it and the join fails, whereas
omitting an escape for a character that never terminates a field costs nothing.

The all-hex rule could not be settled that way. ZXing's documentation says to
quote a value made entirely of hex digits, so a scanner does not read it as raw
bytes; the independent implementation does not quote. Both readings break for
somebody — quoting fails on a parser that does not strip quotes — and neither can
be tested without phones (B2). So the appliance does neither silently: it FLAGS
the ambiguity to the one party who can remove it, because a venue can change one
character of its passphrase and the question disappears.

Four behaviours were mutation-confirmed, and two of the first attempts proved
nothing, which was worth chasing. Adding the passphrase to the status struct did
not fail because a later assignment overwrote the field I set. Logging it under a
field named `password` did not fail either — because `internal/logging` already
redacts field names containing "password", a real mechanism I had not known was
protecting this. Re-run with an innocuous field name (`detail`), the test failed
as it should. A mutation that is silently neutralised proves nothing, and the
second one was worth understanding rather than working around.

## A spec-vs-code audit, and three defects that only existed on a real appliance (D36)

A fresh audit of every buildplan section against the shipped tree returned 39
confirmed gaps — each one found by an auditor and then put to a skeptic told to
refute it, with the survivors carrying reproductions rather than readings.

Three of them broke behaviour that was already shipped, tested, and green. All
three had the same shape, and it is worth naming because it is the second time
this shape has come up: the code was correct and its CONFIGURATION was
unreachable or wrong, so the defect existed only on a real installation and was
invisible on this host.

**The QR I finished this morning pointed every patron's phone at itself.** The
join URL fell back to the admin request's `Host` when the patron listener binds
to a wildcard — and the shipped unit binds patron to `0.0.0.0:80` with admin on
`127.0.0.1:8443`. Every QR a real appliance produced encoded
`http://127.0.0.1:80/`. The auditor verified it by execution rather than by
reading. It is the perfect example of the class this repo hunts: the code scans,
the image decodes, the payload matches the URL exactly — every test I wrote
passed — and the URL is useless.

**Patron audio would not have started on a real appliance.** §9.1 opens UDP
10000-20000 on the patron interface for media; pion bound wherever the kernel
put it. Here that never mattered, because without root no ruleset is loaded
(B5) — so the one host that could have caught it is the one host where it cannot
happen. The peer now binds inside the range, and an invariant holds the two
constants equal across the packages that own them.

**Every screen in a multiplex would have told its patrons the same room.** The
auditorium name came from a flag the shipped systemd unit does not pass, so it
rendered the build-time default "Auditorium 1" everywhere. §8.5 puts that name on
the patron page for exactly one reason — so a patron or passing staff member can
notice they are in the wrong room — and it would have said the same thing in
every room in the building. An unnamed appliance now shows no name at all and
fails a blocking pre-show check, because a wrong name that looks right is worse
than none.

All three are mutation-confirmed by restoring the original behaviour and watching
the new tests fail: the loopback URL comes back verbatim, the room shows
"Auditorium 1", and moving either end of the port range breaks the invariant.

The remaining 36 gaps are recorded in STATUS as the next backlog. The lesson for
working through them is the one D36 ends on: the question that finds this class
is not "is the code right" but "is it reachable, with the values a real
installation has".

## §9.3's SSH row: the one that said the opposite of the truth (D37)

The audit called this the sharpest item in the backlog and it was right. The row
an operator reads as "SSH disabled" came from a config setting nothing in the
tree ever wrote — so it read false, `checkSSH` returned correct, and the
dashboard printed a reassuring answer nobody had checked.

This build host runs sshd on a network. Building the pre-fix code from HEAD and
running it here reports `ssh_disabled: true` with no warnings; the fixed build,
same host, same daemon, reports `ssh_disabled: false`,
`ssh_network_reachable: true` and raises the warning. That contrast is the whole
finding, and it was worth demonstrating rather than arguing — it is the same
before/after a booth would have got.

**A listening socket is the right thing to measure.** Not a unit file, not a
process name: a disabled unit with a running daemon is still a way in, and an
enabled unit that failed to start is not. `/proc/net/tcp` is readable without
privilege, which makes this row unusual — core's fallback pass answers it exactly
as well as the privileged helper does, where for the ruleset it cannot.

Loopback is kept distinct from a network, because §9.2 permits local SSH in Lab
and an appliance reachable only from its own console is not the re-entry path
§9.4 exists to replace.

**And an unmeasured state is neither answer.** Reporting "SSH is enabled" when
nothing looked would be the mirror image of the defect being fixed, so it reads
`ssh_state_not_verified` and does not pass. A mutation showed the guard that ties
`SSHCorrect` to introspection only bites on profiles that PERMIT SSH — everywhere
else the profile rule already fails — which is exactly where an unmeasured state
is most dangerous, and the case my first tests missed. Four behaviours
mutation-confirmed once that gap was closed.

## §8.5's wrong-room guard was running on half its input (D38)

The setup wizard collected the expected screen name. The §10.5 record stored it.
The ACSP machine implemented the comparison, complete with a comment explaining
why it compares case- and whitespace-insensitively but nothing cleverer ("treating
'Screen 3' as 'Auditorium 3' would silence exactly the re-cabling this guard
exists to catch"). The simulator exercised it. And core passed only the
fingerprint.

So an installer filled in the field, and it did nothing.

The mutation makes the failure vivid: with the screen name unwired, a server
announcing "Screen 7" against a pin of "Auditorium 3" reports identity
**"unpinned"** — not a mismatch, not a match, but "nothing was pinned", while the
wizard's field is full. And it is the worse half to have lost: what a server
announces as a fingerprint is a Phase 0A finding (B1), so the screen name is the
only pin an operator can actually set today.

The wizard was also missing the fingerprint field and the physical port label
entirely, so two of §8.5's three pins had no way in at all. Both are now there,
along with the room name §7.2 shows patrons.

The port label gets the §11.5 treatment rather than a fake check: an appliance
cannot see a cable, so the sheet prints the label the installer recorded and asks
a person to confirm the rack matches it. With none recorded it says there is
nothing to check against — an unrecorded label is the finding, not a blank line.

## §9.4's debug capture had no door (D39)

`internal/logging/debugcapture` is a careful piece of work: explicit,
admin-only, time-limited, size-limited, tmpfs-only, self-deleting on a timer and
on reboot, audited without content, with the grantee hashed and the operator's
reason deliberately kept out of the log stream because "free text typed about a
caption fault can quote the caption". Every constraint §9.4 asks for.

It had no production caller. Not one.

That is worse than a missing feature, and the reasoning is worth writing down
because it cuts against the instinct this codebase otherwise runs on. §9.4 does
not forbid capturing caption text or audio; it forbids capturing them CASUALLY,
and offers this as the sanctioned trade. A support engineer who cannot capture
evidence the sanctioned way does not stop needing evidence — they add a log line,
or ask the booth to film the screen with a phone. None of those is bounded,
admin-only, time-limited, visible on the dashboard, or deleted afterwards.
Withholding the safe path does not prevent the act. It removes every constraint
from it.

The §8.3 half was missing in the same way: the manager produced "debug capture
active" warnings and nothing consumed them, so a running capture could not be
visibly indicated however loudly it was configured. Both halves are pinned by an
invariant now — the route, the sink, `RequireTmpfs: true`, and the alert's
consumer — because each was individually absent and each is individually
deletable by a refactor that looks harmless.

## The patron-truth pass: four ways the appliance knew and the patron could not tell (D40)

Every one of these is the same failure seen from a seat. The appliance had the
fact. The page did not show it.

**Two of them were fields already on the wire.** `language_available` has been
sent by the fan-out since it was written, with a comment saying in as many words
that a blank caption area forever is the silent degradation guardrail #7 forbids
— and `patron.js` never read it. So on an uncaptioned reel between captioned
reels, the precise case §5.2 item 5 exists for, a deaf patron got an empty black
box. `caption_grade` was the same: sent, unread, so a subtitle track was
presented as captions to someone relying on speaker names and sound effects.

Both fixes are a few lines in `render()`. What took the thinking was the order:
the uncaptioned-segment check has to come BEFORE the status check, because during
such a segment the link is legitimately "active". Nothing is wrong; there is
simply nothing to show; and those are different sentences.

**A listening patron read "Connecting…" for the whole show.** The §7.2 indicator
was driven only by the caption transport, and `/listen` opens no caption
transport. Sound in the headphones, "Connecting…" on the screen. The fix gives
the audio path the indicator on audio-only routes and ONLY there — and the
mutation that proved the guard matters is the interesting one: removing it made
every test pass, because no test had both transports live at once. With one
added (captions drop, audio keeps playing) it fails properly. Audio reporting
"Connected" while the caption socket reconnects would tell a deaf patron their
captions are fine at the moment they are not.

**A playlist that would not load produced one log line.** No state change, no
warning, and — the part that reaches a patron — the next PlayPlaying advanced to
PLAYING without checking anything had been loaded, so the status said "active"
over a blank area.

**And one reported gap that I decided is not one.** The audit flagged §5.2's
ERROR state as unreachable, which is true: no code assigns it. But the supervisor
already routes transport faults to DISCONNECTED and re-runs the connection
sequence — "one recovery route, not two" — and the model documents ERROR as "a
fault the link cannot carry on from", reporting the server as NOT connected. An
unfetchable playlist is not that. Entering ERROR for it would tell a tech to
check cables for a fault in the DCP. The defect was the silence, not the missing
state; the collapse was a deliberate choice nobody had written down, and now it
is D40.

Six behaviours mutation-confirmed. Two first attempts proved nothing and were
worth chasing: one asserted against the harness's empty initial state rather than
the page's, and one removed a guard no test could exercise.

## §9.4's cue-visibility event, and a mutation that changed the test rather than the code

`caption.cue.visible_no_text` is §9.4's sanctioned trade against the no-content
rule: a cue was shown, in this language, for this long — never the words. The
tracker existed, hashed the cue id, and had no caller. Same shape as the debug
capture: the safe path was unreachable, so investigating a "captions look wrong"
report meant capturing the text.

Wiring it into the fan-out took two corrections, and both are worth recording.

The first was mine and immediate: on a cue-OUT the new state carries no id (it is
a blank), so reporting it left the tracker unable to pair the hide with its show
and it emitted nothing at all. The hide has to name the cue it is ENDING, which
is the previous state's id.

The second is subtler. A mutation that passed the caption TEXT where the id
belongs did not fail anything — because the tracker hashes whatever it is handed,
so no text appeared in the log and every assertion held. That looks like a
non-finding until you consider what the hash is of: a caption line is
low-entropy, and a hash of one is confirmable against a candidate script by
anyone holding the log. "Was this line shown in this screening" is exactly the
question §9.4's opaque identifiers exist to make unanswerable. The test now pins
that the emitted hash is the hash of the cue ID and specifically NOT the hash of
the text, and the mutation fails as it should.

That is the second time in this pass that a mutation proving nothing was the
useful result rather than the wasted one.

## §12.4's requirements were decoration (D41)

`RequireCalibration`, `RequireSignalHealth`, `RequireClearErrorStates`,
`RequiresPhase9Review` — four flags on the Low-Cost Replacement profile, read by
nothing but the profile's own test. That profile is the one for venues running
CaptionBridge **in lieu of** a professional receiver system, which makes it the
one place where an unqualified audio input is not a configuration detail: it is
the thing the venue retired a working system for.

The enforcement mechanism was already there. `internal/web`'s setup gate blocks
on any CRITICAL warning, so routing the unmet requirements through the warning
set stops an installer declaring setup complete, shows on the dashboard, and
blocks the §11.5 sheet — one derivation, three surfaces, no second definition of
"ready" to disagree with the first.

**The calibration evidence had to be a measurement.** An auditorium can be fully
configured — device, mapping, server, port label — with nobody ever having
measured the input, and that is exactly the appliance this requirement is about.
§10.5's `last_qualification` works as evidence only because an earlier honesty
pass made the calibration lane refuse to write it for a synthetic run or a run
that measured nothing. That refusal, made for its own reasons, is what makes this
checkable at all. The mutation for it is the one worth keeping: taking the
evidence from "a profile exists" instead passes every other test.

**And one of the four is a record, not a check.** The appliance cannot verify
that a legal review happened. Storing the operator's reference is a deliberate
departure from D30's refusal to keep staff attestations, and the difference is
staleness: a per-show tick ages badly between shows, while a Phase 9 review is a
one-time event with a reference a later reviewer needs to find. Every surface
says it is a record. What it prevents is the claim going unmade — the profile
that replaces a professional receiver system could otherwise be selected by
anyone, silently, with nothing anywhere saying it had been considered.

`RequireClearErrorStates` got no runtime gate on purpose: it is a property of the
patron copy, not of a running appliance, and D40 is what actually satisfies it.

## The diagnostics that were specified and never produced (D42)

Three fields and an event, all named in the plan, none of them ever written.
Nothing was broken by their absence — which is why they survived so long. They
are only missed at the moment something else breaks and the question cannot be
answered.

**The timeline anchor was declared and never assigned.** §5.5's whole design is
predicting position from a monotonic clock and re-anchoring on the server, and
the field that says when the last fix arrived read 0 everywhere. Zero also means
"never anchored", so it could not distinguish a running show from a dead one.

Getting it right took one correction worth keeping: my first version read the
anchor from the per-language content engines, which only exist once reels have
loaded. That reports "never anchored" for the whole window between the first
Update Timeline and the first loaded language — exactly the window someone
investigating a slow start is staring at. It reads the REFERENCE engine now, for
the same reason §8.2's timeline row does.

**§9.4 lists `caption.timeline.anchor_updated` and nothing emitted it**, so after
a "captions are drifting" report there was no record of whether the server kept
re-anchoring or how far out the prediction was each time.

**Two of §10.8's four compatibility revisions were never written**, so the record
that exists to answer "what changed since this last worked?" answered half of it.
The hardware revision is built from OBSERVED devices rather than stored
configuration, because a stored device id that no longer matches what is plugged
in is the very case being chased. Two details took thought: it is sorted, so a
poll reporting devices in a different order cannot look like a hardware change;
and the record only overwrites with a non-empty value, so the last known hardware
survives the media process going away — which is precisely the outage someone
would be debugging when they read it. Both fields stay EMPTY when unknown, since
a placeholder revision compares equal between two appliances that differ.

**And a latent crash fell out.** `stateLocked()` called `m.ctrl.MaxDriftMS()`
unguarded, while the machine guards for a nil controller everywhere it drives
content — so `State()` on a machine built without one panicked. It had never
fired because every caller supplies a controller. Asking a second question of the
same field is what surfaced it, and it is fixed rather than worked around.

## §5.3/§5.4 — the track a deaf patron actually gets, and who to ask about the one we cannot fetch

Three items were left from the audit's §5 group. Two are now closed and the
third is recorded with the work it actually needs, which turned out to be more
than "wire up the function".

### The coin flip

D8's language order has said "(3) first ClosedCaption track; (4) first subtitle
track" since it was written. Two separate places collapsed those into one step.
`acsp.selectLanguages` fell back to "remaining available languages in RPL order",
and `internal/web`'s `defaultLanguage` returned `available[0]` under a comment
claiming to be D8 (3)/(4) while receiving the machine's raw playlist order.

So on a show carrying both a closed-caption and a subtitle track, with no venue
preference configured, the appliance defaulted to whichever the playlist listed
first. The difference is not cosmetic and it is not a translation preference: a
closed-caption track carries speaker attribution, sound effects and music cues,
and a subtitle track carries dialogue. A deaf patron served the subtitle track is
missing exactly what they came for, on a show that carried the right track all
along — and the picker just shows two languages, so nothing on their phone says
so.

`AccessibilityRank` already existed and already ranked the four kinds correctly.
It was used to pick the best RESOURCE for a language and never to rank the
languages themselves. `rpl.LanguagesByAccessibility` does that now, the machine
uses it for D8 steps (3)/(4), and `CaptionLinkState.AvailableLanguages` reports
the priority order rather than playlist order so the web layer's fallback lands
on the same answer. A test spans the seam deliberately: reordering either side
alone fails it, because either side alone is decoration.

The venue's configured preference still wins. A venue that chose French for its
screens made a decision about its audience, and a track-kind heuristic should not
quietly overturn it.

### A tally nobody could read, keyed by the field nobody can act on

§5.3 asks for "log and classify every unresolved locator form **by server
family**". Half of that was built. `UnresolvedForms()` counted forms and had
exactly one caller — its own test. And `Request.ServerFamily` had been sitting on
the wire since wire.go was written while `handleAnnounce` took only `ServerID`,
so a server that identified itself was ignored.

The half that was missing is the half that turns a count into work. "3 unresolved
`scheme:dcp` locators" says a form exists. "GDC emits `scheme:dcp`" is a handler
to write and a vendor to ask. Coverage is meant to grow from real observations
(B1 says the wire contract is unconfirmed, so field evidence is the plan), and
observations without their source do not compose: two venues reporting the same
form are either one vendor problem or two, and a form count cannot tell them
apart.

The announced family now wins, the auditorium profile's configured value stands
in when the server declares none — which B1 says to expect — and neither known
stays EMPTY. Not "unknown": a placeholder compares equal across two appliances
talking to two different vendors and invents a shared problem out of two separate
ones. Earlier observations keep the family they were recorded under, because
re-attributing them on a family change would fabricate evidence that a vendor
emits a form nobody saw it emit.

The tally reaches `SystemReadiness.UnresolvedLocators` and renders on the §8.2
caption panel, and the row is **absent** when nothing was observed. "Unresolved
locators: none" would be a claim that every locator resolved, which an appliance
that has fetched nothing cannot support. §9.4 holds all the way through: the form
travels, the raw locator never does, because a locator can embed a film title.

### The mutation pass found more than the code did

Six mutations, three of which passed on the first run. Each was more interesting
than the code change that preceded it.

`BestKindFor` returning the LAST resource for a language instead of the most
accessible one broke nothing — every fixture happened to list each language's
best track last, so "take the last one" was indistinguishable from "take the
best". A fixture that lists the best one first now exists.

The other two were the same defect twice. Deleting the supervisor's read of the
tally broke nothing, and deleting `ServerFamily: aud.ServerFamily` from
`captionMachineOptions` broke nothing. The second is the D38 wrong-room defect
exactly: the wizard collects a value, the profile stores it, the machine
implements the behaviour, production never passes it across, and every individual
layer reviews as correct. I had set out to fix a built-but-unwired capability and
built two more on the way.

So the class got a structural fix rather than a third patch. The five caption
dependencies — machine, hub, resolver, controller, warnings — were five fields on
`supervisor` assigned at two call sites, `main()` and the test fixture, which is
precisely how a field missing from one goes on passing the suite. They travel as
one `captionStack` now, built by one constructor both callers use, following the
precedent `newCaptionLoader` had already set for the same reason. A dependency
that is not wired is now not wired for anybody.

### What is left, and why it is not half-done

`RequestLanguage` still has no production caller, and that is recorded rather
than papered over. Production sets no `EagerLanguages` cap, so every language is
fetched up front and the on-demand path is unreachable; adding a hook that is a
no-op in every shipping configuration would be theatre, not wiring.

Turning the cap on needs two things that do not exist. A cap is a memory budget,
and there is no measurement of caption memory against real content to set one
from (B7). And the fan-out has no per-language "loading" state — `kindByLang`
presence currently means "loaded", so a language the show CARRIES but has not
FETCHED would render on a patron's phone as "this show has no captions in your
language". That is fail-wrong on the patron surface, which is the one thing this
codebase will not ship. Both are named in D43 as the work the cap requires.

`make check` green: 753 top-level Go tests, 23 browser cases.
