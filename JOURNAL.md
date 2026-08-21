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
