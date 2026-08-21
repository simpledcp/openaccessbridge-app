# DECISIONS

Every assumption and open-item default, with rationale and a buildplan section
pointer. Items marked **OPEN** in the plan get a safe, configurable default here —
never silently hardcoded as settled.

## Environment / build-host decisions

### D0. Project lives in `/home/libserv-2/captionbridge-app`, not `/home/libserv-2/captionbridge`
The intended project dir `/home/libserv-2/captionbridge` (where `buildplan.md`
and `kickoff.md` were placed) is **root-owned and not writable** by the build
user `libserv-2`, and there is **no passwordless sudo** to chown it (the
`dcpvault-chown` NOPASSWD helper is locked to mount paths). To work autonomously
without blocking, the buildable repo is a self-contained git repo at
`/home/libserv-2/captionbridge-app`, with `buildplan.md`/`kickoff.md` copied in.
**To relocate to the intended path**, an operator can run once:
`sudo chown -R libserv-2:libserv-2 /home/libserv-2/captionbridge && \
 cp -a /home/libserv-2/captionbridge-app/. /home/libserv-2/captionbridge/`.

### D1. Local Go toolchain at `/home/libserv-2/go-sdk` (Go 1.26.4)
Go was not installed and there's no passwordless apt. Installed Go 1.26.4 to a
home-dir path and added it to `~/.bashrc`/`~/.profile`. `GOTOOLCHAIN=local` is set
everywhere to avoid surprise network toolchain fetches on an offline appliance
build host.

### D2. Pure-Go SQLite (`modernc.org/sqlite`), no cgo
§3.4 mandates SQLite for config/profiles. Chose the pure-Go driver so the
appliance ships as a single static binary with no libsqlite3 dependency and
cross-compiles cleanly for arm64/amd64. Trade-off: a larger dependency tree
(modernc.*), acceptable for a config-only store.

### D3. Password hashing via stdlib `crypto/pbkdf2` (Go 1.24+)
Avoids an external crypto dependency (`golang.org/x/crypto`). PBKDF2-HMAC-SHA256,
210k iterations (OWASP floor), random per-credential salt, constant-time compare.
Encoded as `pbkdf2-sha256$iter$salt$hash`. (§8.1 "no default creds.")

### D4. Patron + admin UI as static assets via Go `embed`, vanilla JS — no Node build
§3.4 suggests "lightweight; e.g., Preact/Svelte … no SSR." To keep the appliance
dependency-light, fully offline, and CI robust (no `npm install` that needs
network), the SPA is plain HTML/CSS/vanilla-JS embedded into the Go binary with
`embed.FS`. Revisit only if UI complexity demands a framework. (§7, §8.)

### D5. GStreamer media path via `gst-launch-1.0` subprocess behind a seam
The GStreamer **runtime** (1.24.2) + `opusenc`/`audiotestsrc`/`alsasrc`/`level`
plugins are present, but **dev headers (`gstreamer-1.0.pc`) and `webrtcbin` are
not**, and there's no passwordless apt to install them. So `captionbridge-media`'s
encoder/transport is an interface (`internal/audio/encode`) with: (a) a pure-Go
synthetic backend for tests, and (b) a GStreamer backend that drives a pipeline
via `gst-launch-1.0` for the synthetic-tone smoke test. The real `webrtcbin` peer
is hardware/lib-gated (see BLOCKERS). The qualifier and channel mapper are pure Go
and fully unit-tested independent of GStreamer. (§3.5, §6.7.)

## OPEN-item defaults (configurable, from buildplan §14)

### D6. ACSP connection-initiation direction → default **Variant A (ACS-initiated)** [OPEN #2, §5.1/§9.1]
CaptionBridge opens the outbound TCP connection to the server's ACSP port; return
traffic is admitted by conntrack. This is the plan's "current working assumption."
Encoded as `model.ACSInitiated`, stored per-auditorium (`ACSPInitiation`), and the
firewall manager generates the matching variant. Phase 0A confirms the real
direction per server family; swapping to Variant B is a config change, not a rewrite.

### D7. ACSP transport is PROVISIONAL, modeled from ISDCF notes [OPEN #2, §5.1]
Request names/sequences (Announce, Get New Lease, Update Timeline, Set RPL
Location, Set Output Mode, Terminate Lease), "request interval = half lease," and
"continue output on comms loss until lease expiry" come from public ISDCF notes,
not the licensed standard. The simulator + state machine implement this behind a
clearly-marked seam (`internal/caption/acsp` wire codec) for swap-in of the real
Phase 0A contract. The default ACSP port is **4170** (placeholder), configurable.

### D8. Multi-language default priority [§5.4, OPEN #5 — provisionally resolved]
Order: (1) venue/show-configured default (`AuditoriumProfile.PreferredLanguages`);
(2) browser-language match; (3) first `ClosedCaption` track; (4) first subtitle
track. Validated in Phase 7 UX testing, not an OPEN standards blocker.

### D9. Caption drift/latency budget → **target ≤ 400 ms**, warn at 500 ms [§5.5, OPEN #6]
Within the plan's 250–500 ms working figure. Configurable; measured drift exposed
to diagnostics.

### D10. HI/VI-N latency targets → HI ≤ 150 ms, VI-N ≤ 250 ms (aspirational; measured later) [§6.7, OPEN #10]
Targets used by the latency-estimate plumbing and the dashboard
over-threshold warning. Real glass-to-ear numbers are hardware-gated (Phase 0C/5).

### D11. Concurrent-client caps by profile → 10 / 25 / 50 / 100 [§9.5, OPEN]
Lab / Booth Diagnostic / Patron-Facing / Low-Cost Replacement. Configurable;
enforced by the fan-out backpressure and (later) WebRTC peer admission.

### D12. Secure-context / cert strategy → default **HTTP with tap-to-start**, cert profile configurable [§6.7, OPEN #13]
Default to plain HTTP for receive-only WebRTC playback with a user-gesture
tap-to-start (works in current browsers; loses Wake Lock). The two alternatives
(real cert + local DNS; installed local CA for chain-owned/Loaner devices) are
characterized in Phase 0B and selectable. Hardware-gated to validate.

### D13. First-shippable target profile → **Booth Diagnostic** [§12, OPEN #14]
Per §12.2/§16, Booth Diagnostic is the lowest-risk first product (no
access-equivalence claim). The default profile for a fresh setup is
`booth_diagnostic`; production profiles require completing the setup wizard.

### D14. Module path `github.com/simpledcp/captionbridge`
Matches the `simpledcp` git identity on this machine. No repo is pushed yet.

### D15. MXF/track-file unwrap is a PROVISIONAL heuristic; asdcplib is the real path [§5.3]
`internal/caption/resolver/unwrap.go` detects plain XML vs an MXF-wrapped
timed-text track (ST 429-5) and, for MXF, heuristically extracts the contiguous
timed-text XML essence (locate XML root open → last root close). This is enough
for the simulator's synthetic track files and keeps "a track-file-returning
server is a configuration case, not a rewrite." Real ST 429-5 MXF demuxing should
use **asdcplib**, whose source is present on this dev host at
`/home/libserv-2/asdcplib`; wiring it in needs a cgo build (see BLOCKERS B7). The
unwrap stage always exists in the pipeline even when the server returns plain XML.

### D17. verify treats a missing IPv6 forwarding sysctl as SECURE (disabled) [§9.3]
A missing `/proc/sys/net/ipv6/conf/all/forwarding` means IPv6 is disabled at the
kernel (`ipv6.disable=1`) — forwarding is impossible, which is the secure state,
not "forwarding enabled". So a missing IPv6 file → disabled; a missing IPv4
`ip_forward` (which essentially always exists) → fail loud. This avoids a spurious
critical warning on hardened IPv6-off appliances. (Surfaced by the adversarial
verification pass; an unknown/empty firewall profile also conservatively requires
admin isolation rather than failing open.)

### D18. AudioReady requires ≥1 usable stream and NO failed stream [§10.6]
Independent of caption readiness. A configured stream that Failed makes
`audio_ready` false AND raises a loud warning — a failure is surfaced, never
silently tolerated (guardrail #7). No configured audio → `audio_ready` false
(honest: there is none), while captions can still be ready.

### D19. RPL/resolver grammar is PROVISIONAL [§5.3, OPEN #4]
The ST 430-11 RPL element grammar and the resource-locator forms are implemented
from public summaries. The simulator emits the same grammar so parser↔simulator
agree. The pluggable resolver classifies and counts unresolved (vendor-specific)
locator forms by FORM CLASS (never the raw path, which can embed a title) so
coverage grows from real Phase 0A observations.

### D20. Core reconciles the stored configuration into the running system [§3.5, §9.1, §10.5]
Configuration that nothing consumes is not configuration. `captionbridge-core`
runs a reconcile pass at startup and every 15 s: it derives the firewall ruleset
from the stored profile + interfaces + allowlist and has the privileged helper
install it, runs the §9.3 verification pass **through that helper** (which can
read the loaded nftables table), pushes the §10.5 auditorium input profile to
the media process, and records the §10.8 result. Audio state is polled every 2 s.
Incomplete configuration (no interfaces or no allowlist) is skipped with a logged
reason rather than sent as a half-formed ruleset.

### D21. Firewall posture is MEASURED, never asserted [§9.3]
Both `verify.Run` call sites supply `verify.NFTRulesetReader()`. The deprecated
caller-asserted `FirewallActive` flag is supplied by neither: "a helper answered a
ping" and "--allow-apply was passed" are not evidence that a default-deny ruleset
is loaded. When core cannot reach the helper it runs the pass locally, where the
`nft` read fails without root — reported as "could not be confirmed", a loud
failure, never a pass. `SystemReadiness.firewall_introspected` tells the dashboard
whether the claim was measured; an active-but-unmeasured claim raises
`firewall_active_not_introspected` rather than the misleading `firewall_inactive`.

### D22. Caption-path warnings are a live SET, cleared by measurement [§8.3, §5.2]
The ACSP machine raises warnings as one-shot events; the dashboard needs the
conditions that are true now. `readiness.CaptionWarnings` holds each raised
warning and drops it only when something MEASURED shows the condition passed: a
status request that pushes the lease deadline back out, a window load that finds
nothing missing and nothing degraded (`resolver.Loader.OnLoadComplete`), a
torn-down link. Nothing expires on a timer, and nothing is cleared by the link
state merely changing shape — the content warnings are raised part-way through
loading a show, before the server has necessarily said which playout is on
screen, so anything inferred from the link state at that moment files them under
the wrong show and drops them unread. For the same reason a warning's link
context is bound from the first state observed AFTER it was raised, not the last
one seen before. Defaults
(`caption_lease_warn_fraction` 0.25, `caption_asset_fetch_timeout_ms` 5000) are
settings, not constants reached by omission; an out-of-range value falls back
loudly.

### D23. §10.5 `last_qualification` is recorded only from a settled verdict [§6.4, §10.8]
Core writes the auditorium's last qualification from the WORST live stream
verdict — a healthy HI feed does not excuse a failed VI-N one — and skips the
pass in which the media plan was just rebuilt, because a verdict read a moment
after the streams restarted describes the restart, not the booth's input. §10.8's
`firewall_profile_version` is recorded only when rules were REALLY installed
(dry-run records nothing), and `audio_device_compat_profile` records the stable
device identity in play; there is no per-device compatibility revision to record
until real qualification data exists (BLOCKER B3).

### D24. A config database from a newer schema is refused, not opened [§10.8]
`config_schema_version` is read and reconciled on open: a fresh database is
stamped, an older one runs the (currently empty) migration path and is
re-stamped, and one written by a NEWER build is refused with a named error rather
than read as if it were ours. An unparsable version is refused too. Silently
misreading configuration is how an appliance comes up in a posture nobody chose.

### D20. WebRTC peer is **Pion** (pure Go), not GStreamer `webrtcbin` [§6.7, supersedes part of D5]
§6.7 names `webrtcbin` as the appliance-side peer, and BLOCKER B4 recorded it as
unreachable: `gst-plugins-bad` and the `gstreamer-1.0.pc` dev headers cannot be
installed without passwordless sudo. That blocker was scoped to ONE
implementation. `github.com/pion/webrtc/v4` is a pure-Go WebRTC stack needing no
dev headers and no cgo, so it clears B4 in software.

**Verified by spike before adopting, both with `CGO_ENABLED=0`:**
- two Pion peers negotiate with `ICEServers: []` — host candidates only, no
  STUN/TURN, as §6.7 requires — with the BROWSER offering and the appliance
  answering, which is the direction `assets/patron.js` already implements, and
  Opus RTP flows over it;
- `gst-launch-1.0 … ! opusenc frame-size=10 audio-type=voice ! rtpopuspay !
  udpsink` emits RTP that `pion/rtp` parses, with the runtime plugins already
  present on this host.

**D5 stands:** GStreamer remains the ENCODER behind the same
`internal/audio/encode` seam. Pion replaces only the transport. The split keeps
the appliance a single static binary with no libgstreamer link-time dependency
(consistent with D2's reasoning for pure-Go SQLite).

**Note on the codec line:** WebRTC signals Opus as `48000/2` by convention even
when the payload is mono. §6.7's "mono per stream" is about the audio content,
not the SDP channel count; declaring 1 channel makes the offer's codec unmatched
and the track never starts. This cost a spike iteration and is worth remembering.

**What this does NOT clear:** B2. Real phones on a real venue AP remain
untested, and the phone-side jitter buffer — §6.7's dominant, least-controllable
latency term — cannot be measured here at all.

### D25. Mapped PCM reaches the encoder on stdin; RTP comes back on loopback UDP [§6.7, §6.5]
The §6.7 spike drove `audiotestsrc` inside GStreamer, but the real source is the
MAPPED output of `internal/audio/pipeline`. The shipped encoder is therefore
`gst-launch-1.0 fdsrc fd=0 ! rawaudioparse … ! opusenc … ! rtpopuspay ! udpsink
host=127.0.0.1`, one per NAMED STREAM, with the appliance writing the mapped
lane in as S16LE and reading RTP back off a loopback socket it bound first.

Why not let GStreamer open the device: §6.5 makes the channel mapper the single
place that decides which captured channel feeds which named stream. A second
mapping inside the pipeline would mean the qualifier's verdict describes a
different lane from the one a patron hears. It also keeps the capture device
behind the pure-Go `capture.Source` seam, where the real ALSA source lands (B3).

Why `rawaudioparse` and `perfect-timestamp=true`: `fdsrc` is a byte stream with
no format, so the caps are stated rather than guessed, and the Opus frames are
timestamped from the SAMPLE COUNT. Without the latter, a short read on the pipe
makes the RTP clock advance unevenly — measured at 360/480/360 ticks before the
fix, exactly 480 after — and an uneven RTP clock is jitter the phone-side buffer
(§6.7's dominant term) has to absorb.

Why loopback UDP rather than a pipe: RTP is a datagram format, the §6.7 spike
proved `pion/rtp` parses what `rtpopuspay` emits over it, and the socket is
bound on 127.0.0.1 before the port is handed to `udpsink` — so the port cannot be
raced, and nothing off this appliance can reach the pre-SRTP media (§9.4). The
reader latches the encoder's source address from the first packet and drops
anything else.

### D26. `encoder_state:"streaming"` is set from counted RTP packets, never from a launched process [§6.6, §6.7]
The state that flips patron availability to available is the one most worth
faking, so it is measured: `pipeline.Stream` reports `streaming` only once
`encode.StreamStats.PacketsOut > 0`. An encoder that starts and emits nothing is
reported `unavailable` after one second (two orders of magnitude of slack on a
10 ms frame), its source unpublished and its subscribers dropped. PCM the encoder
cannot take is counted as dropped frames and logged as a count, never silently
discarded. §8.2's "stream active" reads `yes` only from this measured state.

### D27. The appliance answers with ICE gathering COMPLETE; only the browser trickles [§6.7]
With no STUN and no TURN, the appliance's gathering is an enumeration of its own
interfaces and finishes in milliseconds, so the answer carries every host
candidate and the core→media signaling seam stays a request/response socket with
no push channel. The browser still trickles its candidates in and `AddCandidate`
takes them; pion's default mDNS mode (query-only) is set EXPLICITLY so §6.7's
`*.local` gotcha is a decision rather than an inherited default — the appliance
resolves the browser's mDNS candidates and advertises none of its own.

**Known limit — CLOSED by D28.** pion gathered on every non-loopback interface,
so on a multi-homed box the answer carried docker/VPN addresses. Gathering is now
confined to the configured patron interface; see D28 for what that measured.

### D28. ICE gathering is confined to the configured patron interface, and what a patron was offered is MEASURED [§6.7, §9.1, §8.2]
D27 left pion enumerating every interface. Measured on the build host, the
appliance answered a patron with **16 candidates across 5 networks**: the patron
LAN NIC and its 8 IPv6 privacy addresses, a WireGuard address, BOTH Tailscale
addresses, `docker0`, and a docker bridge.

Two costs, and the first is the one that matters at showtime. ICE tries the pairs
it is given, and a candidate on a network the phone cannot route to does not fail
fast — it fails on a timeout, for every phone in the room, during the ninety
seconds in which an auditorium's patrons all scan the code at once. §6.7 spends
its entire budget on milliseconds. The second is that an SDP answer naming the
venue's overlay subnets is a description of its private topology handed to
anything on the patron LAN. Neither is a local-only violation — there is still no
STUN, no TURN, and nothing leaves the venue — which is exactly why this sat
unfixed: it was invisible.

**The scope is an interface NAME, not an address.** A patron NIC renumbers (DHCP
lease, IPv6 privacy rotation) without becoming a different NIC, and the question
ICE has to answer is which wire, not which address today. It comes from
`config.SettingClientInterface` — the interface the setup wizard already records
— pushed core → media over a new `media.network` message. Deliberately NOT folded
into `media.configure`: that carries the §10.5 AUDIO profile, and correcting a
network setting must not restart the room's audio.

**A missing patron interface is refused, not absorbed.** Gathering with the filter
on an interface that is absent or down yields an answer with no host candidate at
all, which a phone cannot tell apart from a dead network: the patron gets a
spinner and the booth gets nothing. So the interface is re-checked on every
answer — it is a physical thing that can be unplugged between one patron and the
next — and a fault is returned by name and raised as a critical dashboard warning.

**Three separate facts, and the dashboard shows all three.** Configured (an
operator chose one), in force (the filter is installed and the interface is
usable), and — the only measurement of the three — the interfaces that appeared
in the last answer this appliance actually emitted. They are reported separately
because a disagreement between them is precisely the bug worth seeing, and
because deriving the third from the first is how a broken appliance would look
identical to a working one. An appliance nobody has joined yet reports "nothing
offered to measure" rather than a verdict on an offer that was never made.

**Not confined is a legitimate state.** An appliance whose setup wizard has not
run gathers on everything, as before, and says so on the dashboard with the
networks named. Silence there would let the mechanism's existence read as
confinement.


### D29. The QR encoder is in-tree and pure Go, and it is verified against implementations that are not ours [§10.4, §7.2, §9.5]
§7.2 wants a per-show QR so a patron joins in one scan. Two earlier passes
declined to render one, and the recorded reason was right: a QR is an artifact
nobody on the build team can read, so a bitmap with a bug in it looks exactly
like a bitmap without one — square, three corners, and a silent failure at the
auditorium door in a patron's hands. Shipping one unverified would have been a
fabricated result.

**Why in-tree rather than a module.** The appliance ships as one static binary
with no cgo (D2, D4) and is deliberately offline. QR Model 2 is a closed, frozen,
fully specified format that will never need updating, so the choice was a few
hundred lines of arithmetic against a permanent supply-chain surface on a box
whose whole security posture is that nothing reaches it.

**How the objection was answered rather than waived.** Nothing in
`internal/qr` is checked against our own reading of ISO/IEC 18004:

- Every module of every symbol is compared against references from
  **python-qrcode**, an unrelated implementation — across all 8 data masks for a
  corpus of boundary cases, and across **all 160 version/correction-level
  combinations at their exact byte capacity**, so the spec tables are pinned
  rather than spot-checked. The capacities themselves come from bisecting the
  oracle, not from copying the table under test.
- Every reference was decoded by **zxing-cpp**, the C++ ZXing engine behind a
  great many phone scanner apps, which had to return the exact payload before the
  fixture was committed.
- `make qr-verify` decodes the **PNGs this appliance actually serves**, at the
  scales it serves them, with the same decoder. 42/42 at the time of writing.
  It lives outside CI because CI has no Python and must not need any; the
  committed fixtures carry the load there.

**Fixtures are indexed by MASK, and that is a finding, not a detail.**
Implementations genuinely disagree about which of the eight data masks to pick,
because the penalty rules rest on things the standard leaves open — whether the
quiet zone supplies the light margin for a finder-lookalike at the symbol edge,
and whether the format information is in place while masks are scored
(python-qrcode blanks it; ZXing and Nayuki do not). None of it affects
readability: the chosen mask is recorded in the format information, so a scanner
reads our answer rather than reproducing our reasoning. So the fixtures pin all
eight masks, and the guarantee is that WHATEVER mask is chosen, the output is
byte-identical to a symbol an independent decoder has already read.

**A third implementation was needed to settle the first disagreement.** The first
fixtures came from segno, and our encoder disagreed with it on every case, always
in the padding, always by one codeword. segno's `write_padding_bits` extends by
`8 - (length % 8)` with no outer `% 8`, so a bit stream that already ends on a
codeword boundary — which in byte mode it always does — gets an extra `0x00`.
ISO §7.4.10, quoted verbatim in segno's own docstring directly above that line,
adds padding bits only when the stream does NOT end on a boundary. python-qrcode
agrees with this package module-for-module and disagrees with segno by 146
modules on the same input. Two implementations and the spec text against one.
The deviation is harmless — both symbols decode — but the episode is why the
generator names three implementations instead of trusting one.

**ECC level M by default.** Not because more correction is worse, but because a
higher level needs a higher version, which means more and smaller modules at a
given printed size — and what defeats scans at a cinema door is small modules in
bad light, not codeword corruption. A typical §10.4 join URL is 53x53 modules at
M and 61x61 at Q.

**The physical size is computed, not guessed.** `RecommendedPrintWidthMM` derives
a minimum card width from the symbol actually being printed (0.8 mm per module),
because a longer join URL means a larger version, more modules, and a card that
must be physically BIGGER — a relationship that is invisible on a screen and is
exactly where a booth would get it wrong. The admin page prints that number
beside the code.

**What this does NOT clear: B2.** No phone camera, no venue lighting, no lobby.
Local decoding by a scanner library is not a patron scanning a printed card in a
dark foyer, and nothing on the operator surface claims otherwise.

### D30. A pre-show check has three states, and the ones only a person can do are never recorded [§11.5, §8.1, §9.3]
§11.5 asks for two things, and the second is the one usually skipped: "Codify a
staff pre-show checklist with explicit pass/fail … Crucially, define **what staff
do when a check fails** before a show — readiness checks are only useful if
they're tied to a procedure." So every item on the sheet carries a REMEDY. An
item without one is a fact about the appliance, not a checklist item, and it does
not belong on a page consulted ninety seconds before a house opens.

**Three states, not two.** `pass`, `fail`, `not checked`, and `cannot be checked
here` — the last two are different and lead a booth to do different things.
"Nothing read the firewall ruleset" (D17) is not a failure and is certainly not a
pass; "no glass-to-ear latency has ever been measured on this appliance" is
structural and will not change by trying again. A blocking item that was never
run counts against the appliance's verdict exactly as a failure does, because the
entire purpose of the third state is that "we did not look" must not buy the same
silence as "we looked and it was fine".

**Staff steps are printed and never recorded.** Three items on the sheet can only
be answered by a person: loading the patron page FROM A SEAT rather than at the
rack (§9.5's RF-reality failure), confirming the admin surface is unreachable
from the patron SSID (the appliance can answer for its BINDING, not for what the
network permits), and the loaner kit. The appliance prints the step and the pass
criterion and stores nothing about whether it was done.

That is a deliberate refusal, not a missing feature. A tick box the appliance
cannot verify, stored and shown back later, is indistinguishable from a
measurement and ages far worse — last week's tick would sit on tonight's page.
This repo has already shipped exactly that bug once, in the §10.8 readiness stamp
written by pressing "Complete" rather than by a check that ran. So
`StaffStepsOutstanding` is always the full count of staff items, and the sheet
says in words that the appliance does not know whether they were done.

**There is no single "ready" verdict.** `ApplianceReady` is named for what it
covers. The appliance can answer for its own state; it cannot see the seats, the
SSID list or the loaner drawer, and one composite boolean would let the half it
knows vouch for the half it does not.

**Two representations, and the plain-text one is the deliverable.** A checklist
that exists only in a browser is not a procedure. `/api/admin/preshow?format=text`
prints a wrapped, fixed-width sheet for a booth binder, with the remedy set under
each item that is not passing — and only those, because a sheet that repeats an
instruction under all sixteen lines is one nobody reads to the bottom. It is
readable when the appliance is the thing that is broken.

**Derived from the dashboard verdict, not beside it.** The checklist reads
`model.SystemReadiness` — the same account of itself the operator is looking at —
rather than re-deriving from `readiness.Inputs`. A parallel derivation could
disagree with the page, and two truths on one screen is worse than either.

### D31. Support mode is a WINDOW with a deadline, not a flag — and a window is not a connection [§9.4, §9.1]
§9.4: "Off by default. When enabled by an admin it is outbound-only,
time-limited, and audited, and visibly indicated. It is the only re-entry path
once SSH is disabled." That last sentence is what makes this the one feature in
the product whose failure mode is a permanent way in, so the design is shaped
around the failure rather than the feature.

**What existed before was a boolean.** `support_mode` was a settings key the
dashboard read and nothing wrote. A persisted boolean cannot carry a deadline: set
once, it stays set, which is precisely the permanent re-entry path §9.4's time
limit exists to prevent. Startup now CLEARS a stored `support_mode = true` — it
grants no window and no rule, and leaving it set would tell a future reader that
support mode is on.

**Opening a session opens exactly one outbound rule.** The output chain is
`policy drop`, so this is real: `oif <iface> ip daddr <host> tcp dport <port>
accept`, in the OUTPUT chain and nowhere else. Return traffic is admitted by the
established/related rule, exactly as Variant A's ACSP replies are. An inbound
accept would turn "outbound-only" into a listening port on an appliance whose SSH
is deliberately off, which is the thing support mode replaces.

**A partially configured endpoint is refused, never rendered.** The only two ways
to render a window with no address are a bare `oif <iface> accept` — an
unrestricted egress hole opened by a support switch — and silence, which reports
a window that permits nothing. The host must be a literal IP, not a name: resolving
one would need DNS from an appliance whose §9.1 checks exist to detect DNS leaks,
and a name that resolves differently tomorrow is a rule that quietly changes what
it permits.

**Time-limited means the appliance closes it.** An absolute deadline, clamped to
four hours whatever is requested, enforced by a timer AND re-checked on every
read so no caller can see a window the deadline has already closed. Opening and
closing signal the supervisor to reconcile immediately rather than at the next
posture pass: the deadline is the promise, and a rule that lingers past it breaks
it. It does NOT survive a restart — a window that came back after a boot would
outlive the person who opened it and the reason they gave.

**Audited, and the audit is a precondition.** If the trail cannot record the
session, the window does not open: an audited feature whose audit failed is an
unaudited feature. The operator's REASON is required — an audit entry without one
is a timestamp — and it is stored locally and NEVER logged, because it is free
text that could contain anything (§9.4). Its length is logged, so a trail of
empty reasons is visible without the text travelling. The grantee is hashed.

**A window is not a connection, and that is a field rather than a sentence.**
This appliance has no tunnel client and no support endpoint of its own. It
permits an outbound destination; it does not dial one. `TunnelEstablished` is
present on every response and is always false in this build, so nothing can
quietly start reading "a support session is open" as "a support engineer is on
the box" — and the dashboard renders that field rather than asserting it.

### D31a. The nftables ruleset REPLACES its table rather than adding to it [§9.1, §9.3]
Found while building D31, and it predates it. `nft -f` on a table definition has
implicit `add` semantics: applied at a table that already exists it appends every
rule again and leaves whatever was there before. The generated script had no
flush and no delete.

The consequence is not duplicate lines. It is that **a configuration change could
not remove a rule**: the previous cinema server's allow rule would survive the
address being corrected, and the admin-on-patron-interface rule would survive a
switch from Booth Diagnostic to a production profile. The stricter posture would
be applied, the looser one would still be accepting packets, and introspection
would find the expected rules present and report the posture verified.

Generate now emits `table inet captionbridge` (creating it if absent, so the
delete cannot fail on a fresh boot) followed by `delete table inet captionbridge`
before the definition. Both land inside the same `nft -f` transaction, so there
is no instant in which the appliance has no table and therefore no default-deny.

**Not demonstrated on a live ruleset here.** Applying nftables needs root (B5)
and this host has user namespaces disabled, so the behaviour is established from
nft's documented semantics and the fix is asserted in the generated text. It
should be confirmed against a real ruleset the first time the helper runs as root
on an appliance.

### D32. The appliance verifies updates and can never sign them; "verified" and "installable" are separate answers [§9.4, §4.3]
§9.4: "The primary update path is signed and offline-capable … verified against a
key held in the TPM/secure storage on the hardened tier … A/B partitions for
atomic updates with automatic rollback on failed boot/health-check, so a bad
update never bricks a booth appliance."

**The appliance holds public keys only.** There is no field, no setting and no
code path for a private key anywhere in the tree, and an invariant test asserts
that across the whole update path — `ed25519.Sign`, `ed25519.PrivateKey` and
`ed25519.GenerateKey` may not appear in any of it. An appliance that could sign
an update it would then accept is one whose compromise is permanent: one write to
its trust store and every future update is whatever the attacker wants.

**The trusted key never comes from the bundle.** The manifest NAMES a key id; the
key itself must already be in the appliance's store. A bundle checked against a
key it carries verifies perfectly and proves nothing, which is the commonest way
a signed update path turns out not to be one. A bundle naming a key this
appliance does not hold is refused by name, so an operator is told which key was
wanted rather than "bad signature".

**The signature covers the bytes that arrived.** Verification is over the exact
manifest bytes, not a re-serialisation of the parsed struct. Re-encoding would
tie the appliance to one JSON encoder's field order and spacing: a signer written
in another language would produce bundles this appliance rejected, reported as
"bad signature" — the same words a tampered bundle gets. Being unable to tell a
mis-encoded bundle from a hostile one is the fault worth avoiding. (This is a
correction: the first version of that comment claimed re-serialisation would let
a signature pass over unsigned content. It would not — a canonical re-encoding of
the parsed struct cannot change meaning. The mutation pass caught the claim being
untestable, and both the comment and the test were fixed.)

**Ordering is an integer, not a version string.** Is "1.10" newer than "1.9"?
The release process owns a monotonic build number and the appliance compares
that. A downgrade or a re-install is REFUSED unless an operator explicitly asks:
a downgrade re-introduces every fault the newer build fixed, and re-installing
the current build is the shape a replay takes on an offline path.

**Rollback is real only because the NEW image confirms itself.** `ConfirmHealthy`
takes the running slot as a parameter and refuses if it is not the staged one, so
the old image cannot bless the new one. An updater that marks its own work
healthy has an automatic rollback that can only fire when nothing ran at all —
and the failure worth catching is the update that boots, comes up, and cannot
reach the cinema server. Attempts are counted BEFORE the boot, so an image that
hangs instantly still exhausts its tries; the limit is three rather than one,
because a single failed boot can be a power cut and rolling back a good update
because someone switched the rack off is its own outage.

**Verified is not installable, and they are separate fields.** This image has no
A/B slots (BLOCKERS B8), so a bundle can be perfectly valid and still not
installable here. `VerifyResult.Installable` and `Report.ABSupported` carry that
separately all the way to the dashboard, which renders both. Collapsing them
would let "this bundle is good" read as "this update can be applied", and a booth
would wait for something that is never going to happen.

**Cloud update is not built at all.** §9.4 makes it "optional and outbound-only",
and nothing here reaches the network for an update. If it is ever built it should
reuse D31's support-window shape rather than adding a second egress path.

### D33. The encode-path latency estimate is ONE accumulation, and it got smaller [§6.7, §11.4]
`pipeline.EncoderPathLatencyMS` returned capture period + Opus frame, which at
the shipped settings (a 10 ms capture period feeding 10 ms Opus frames) claimed
20 ms. Those are the same wait. The captured block is handed straight to the
encoder and becomes exactly one Opus frame; nothing accumulates twice. §11.4
measured the whole appliance-side path at ~10.7 ms — 10 ms of framing plus 651 µs
of measured transit — and was the first thing able to say so.

The estimate is now the smallest whole number of capture periods that holds at
least one Opus frame: 10/10 → 10, 5/10 → 10, 20/10 → 20, 7/10 → **14** (a period
that does not divide the frame costs a whole extra block, which is why this is a
ceiling and not a `max`).

**It got smaller, and that is the point.** The previous pass left this alone
because it "errs conservative". It does not: it was wrong, and a latency budget
padded by a term that does not exist misdirects effort away from the terms that
do. §15 names HI latency as the plan's largest risk, and §11.4 has since shown
that essentially all of it lives on the phone side.

Deliberately still excluded, and stated in the code: the measured transit (a
measurement does not belong in a structural estimate), the analysis tap (it runs
beside the delivery path, §6.1), and everything off the appliance — Wi-Fi, the
phone jitter buffer, phone output. `LatencyMeasured` stays false.

### D34. A two-device room is representable, and a pairing is built one device at a time [§6.5, §10.5]
§6.5 lists "Multiple USB inputs: Device A = HI, Device B = VI-N" as a supported
mapping. The mapper implemented it and the wizard offered it — and then
`AuditoriumProfile` had one `input_device_id` to save it into, so a venue wired
that way could not be configured at all.

`StreamDevices map[StreamType]string` holds the per-stream assignment, and
`ValidateInputs` refuses the three ways it can be recorded wrongly: a stream with
no device (nothing feeds it, and a patron selecting that feed hears silence);
both streams on one device (that is a channel mapping, and recording it as a
pairing hides which channel each stream takes); and per-stream devices on a
single-device mapping (they would be ignored and the room would run on a mapping
nobody chose).

**The wizard's refusal to run the multi-device plan against one device STAYS**,
because it was right: it would report both feeds as measured when only one was
opened. A pairing is instead built one device at a time, each with a
single-stream mapping and `part_of_pairing`, and the response says which stream
is still unassigned — silence there is how the second device gets forgotten. A
subsequent single-device save clears the pairing, so no stale per-stream device
can survive into a room that no longer has one.

The media process checks EVERY device the profile names against its capture
backend, not just the first: a pairing naming one synthetic and one real device
would otherwise have had half of it served by a tone under the real device's
identity — the same fabrication the single-device refusal exists to prevent. The
hardware refusal itself is unchanged (B3/B4).

### D35. The appliance stores the patron Wi-Fi passphrase, and the Wi-Fi code is a SECOND code [§9.5, §9.4]
§9.5: "QR onboarding carries SSID + password + URL where the platform allows, so
a patron joins the network and the page in one scan." That requirement was
recorded as not built because it turns on a question this project takes
seriously: should the appliance hold a network credential at all?

**It should, and the reason is what kind of credential it is.** A patron
network's passphrase is handed to every person in the auditorium and printed on a
sign in the lobby. It is not a secret in the security sense; it is a shared
access token for a deliberately untrusted BYOD network the appliance does not
even operate (§9.5's "dedicated external AP"). Refusing to store it would block a
stated requirement in order to protect something that is, by design, public.

What it IS is a credential this appliance did not previously hold, so it gets the
handling one deserves: never logged (the logger already redacts fields named
`password`, and a test proves the value does not reach a log under an innocuous
field name either), never returned in the status document a dashboard polls —
which reports `password_set` rather than the value — never served on the patron
listener, and cleared when the network is cleared, so no passphrase outlives the
network it belonged to.

**One QR cannot be both a network join and a URL.** `WIFI:` is a distinct grammar
that phones hand to their Wi-Fi settings. So this is a SECOND code, printed
beside the show's: network first, page second. §9.5's "in one scan" is really "in
one scan each", and saying so is better than shipping a code that does neither.

**Two escaping decisions, and both were settled by cross-checking rather than by
reasoning.** The format is de facto (ZXing's), not a standard.

  - The COMMA is not escaped. The first implementation escaped it, reasoning from
    the related MECARD contact grammar where it is a separator. It is not a field
    terminator here, and an unnecessary escape is the more dangerous error: a
    parser that does not strip it hands the phone a passphrase containing a
    literal backslash and the join fails. An independent implementation agreed
    with ZXing and not with us.
  - An all-hexadecimal value is NOT quoted, and is FLAGGED instead. ZXing's
    documentation says to quote it so a scanner does not read the hex as raw
    bytes — but quoting is the same gamble pointed the other way, since a parser
    that does not strip the quotes puts literal quote marks in the passphrase.
    Both readings break for somebody and neither can be tested here (no phones,
    B2). So the appliance reports the ambiguity to the one party who can remove
    it: the venue, by changing one character. Guessing silently on a patron's
    behalf is the only option that helps nobody.

### D36. Three defects a spec-vs-code audit found in shipped behaviour, and what they had in common [§7.2, §8.5, §9.1, §6.7]
A fresh audit of every buildplan section against the tree produced 39 confirmed
gaps. Three of them broke behaviour that was already shipped and looked correct,
and all three shared a shape: the code was right and its CONFIGURATION was
unreachable, so the fault only existed on a real installation.

**The QR pointed every patron's phone at itself.** `joinURL` derived the patron
address from the ADMIN request's `Host` when the patron bind was a wildcard. The
shipped unit binds `--patron-bind=0.0.0.0:80 --admin-bind=127.0.0.1:8443`, so on
any real appliance that Host is `127.0.0.1` — and the QR encoded
`http://127.0.0.1:80/accessibility?t=…`. It scanned perfectly. Nobody would have
looked for a fault, because the scan worked.

The address now comes from the §9.1 CLIENT INTERFACE — the patron LAN by
definition, and the same interface D28 confines ICE gathering to — falling back
to the admin Host only when it is not loopback. When no address can be
determined there is NO join URL and NO QR, with the reason on the operator's
screen: a code that scans and goes nowhere is worse than no code.

**Patron media landed outside the ports the firewall opens.** §9.1's ruleset
opens `iif <client> udp dport 10000-20000 accept`, and nothing constrained pion,
which bound to whatever ephemeral ports the kernel gave it (typically
32768-60999). On a real appliance with the ruleset loaded as root, the phone's
inbound connectivity checks arrive on ports the input chain drops; audio worked
here only because no ruleset is ever loaded without root (B5). The peer now sets
`SetEphemeralUDPPortRange` from constants an invariant test holds equal to
`firewall.DefaultWebRTCPortRange` — the two halves live in different packages and
nothing but that assertion connects them.

**Every screen in a multiplex told its patrons the same room name.** The
auditorium name came only from `--auditorium`, whose default is "Auditorium 1",
and the shipped systemd unit passes no such flag. §8.5 puts the room name on the
patron page precisely so a patron or passing staff member can catch a wrong-room
join — and it would have read "Auditorium 1" in every room. It is now a stored
setting the wizard writes and the page resolves PER REQUEST (so naming a room
needs no restart), the flag is a development fallback used only when explicitly
passed, and an unnamed appliance shows NO name and fails a blocking pre-show
check. A wrong name that looks right is worse than none.

**What they have in common is the lesson.** Each was reachable only through the
configuration a real install produces, and each was invisible on this host —
because the developer path passes the flag, reaches admin over a real address,
and never loads a ruleset. That is the same class as the three
readers-without-writers the previous review found, and it is now the thing to
look for first: not "is the code right" but "is the code reachable, with the
values a real installation has".

### D37. The SSH row is MEASURED from a listening socket, and an unmeasured one is neither answer [§9.1, §9.3]
The §9.3 row an operator reads as "SSH disabled" was derived from the config
setting `ssh_enabled` — which nothing in the tree ever wrote. Absent, it read
false, `checkSSH` returned correct, and the dashboard printed "SSH disabled:
yes". On an appliance where sshd was genuinely running it said the opposite of
the truth, with no warning. That is fail-WRONG on a security row, and it is
exactly the defect the firewall row had already fixed (D17).

**Demonstrated, not argued.** This build host runs sshd. The pre-fix binary,
built from the same tree and run against the same machine, reports
`ssh_disabled: true` and no warnings. The fixed one reports
`ssh_disabled: false`, `ssh_introspected: true`, `ssh_network_reachable: true`
and raises `ssh_enabled_outside_support`.

**What is measured: a listening socket.** Not a unit file and not a process
name. The question §9.1 is asking is "is there a way in", and a disabled unit
with a running daemon is still a way in while an enabled unit that failed to
start is not. `/proc/net/tcp` and `/proc/net/tcp6` are readable without
privilege, so — unlike the ruleset — this row is answerable by core's fallback
pass exactly as well as by the privileged helper.

**Loopback is distinguished from a network.** §9.2 permits local SSH in the Lab
profile, and an appliance reachable only from its own console is not the
re-entry path §9.4 replaces. The measurement carries both facts.

**An unmeasured state is neither answer.** If the tables cannot be read, the row
is `ssh_introspected: false`, `SSHCorrect` is false regardless of profile, and a
distinct `ssh_state_not_verified` warning is raised — never
`ssh_enabled_outside_support`, because asserting SSH is OPEN on no evidence is
the mirror image of the defect this replaced. A mutation found that the guard
making `SSHCorrect` depend on introspection is load-bearing ONLY on profiles
that permit SSH (elsewhere the profile rule already fails), which is precisely
where an unmeasured state is most dangerous; there is now a test for that case.

### D38. §8.5's wrong-room guard ran on half its input, and the half it had is the one a server may not announce [§8.5, §10.5, §7.2]
§8.5 pins two facts per auditorium — the expected server fingerprint and the
expected screen name — and warns loudly when the connected server matches
neither. The setup wizard collected the screen name. The §10.5 record stored it.
`internal/caption/acsp` implemented the comparison. The simulator exercised it.
And `cmd/captionbridge-core` passed only the fingerprint, so on a real install
the guard an installer had just configured did nothing with the field they
filled in.

**It is the worse half to lose.** What a cinema server announces as a fingerprint
is a Phase 0A finding (B1) — unknown until someone captures a real session — so
the screen name is the only pin an operator can actually set today. Unwired, a
server announcing "Screen 7" against a pin of "Auditorium 3" reported identity
`unpinned`: not a mismatch, not a match, but "nothing was pinned" — while the
wizard's field was full.

Both halves are now passed, both are settable in the wizard (the fingerprint
field was missing from it entirely), and an invariant asserts each literal
reaches the machine.

**The port label is a STAFF step, because an appliance cannot see a cable.**
§8.5 also asks to "tie the auditorium profile to the physical port label so a
re-cabling that swaps rooms is caught at setup, not mid-show". The honest form is
the §11.5 pattern: the sheet prints the label the installer recorded and asks a
person to confirm the rack matches. With no label recorded it says there is
nothing to check against, rather than printing an empty instruction — an
unrecorded label is itself the finding.

### D39. §9.4's sanctioned debug capture needed a door, because the alternative to a bounded path is an unbounded one [§9.4, §8.3]
`internal/logging/debugcapture` implemented every constraint §9.4 asks for —
explicit, admin-only, time-limited, size-limited, tmpfs-only, self-deleting on a
timer and on reboot, audited without content — and had no production caller. So
the sanctioned way to capture caption text or audio for a support case did not
exist on any surface.

**That is worse than a missing feature.** §9.4 does not forbid capturing content;
it forbids capturing it casually, and offers this as the trade. A support
engineer who cannot capture evidence the sanctioned way does not stop needing
evidence — they add a log line, or ask a booth to film the screen with a phone.
None of those is bounded, admin-only, time-limited, visible on the dashboard, or
deleted afterwards. Withholding the safe path does not prevent the act; it just
removes every constraint from it.

It now has an admin route, a sink on the unit's tmpfs (with `RequireTmpfs: true`,
so a capture cannot quietly write content to the persistent filesystem and breach
guardrail #5), a sweeper, and — the half that was equally missing — §8.3's
"debug capture active" alert is finally CONSUMED: the manager produced those
warnings and nothing read them, so a running capture could not be visibly
indicated however loudly it was configured.

### D40. The caption path tells a patron what is happening, and ERROR stays collapsed into DISCONNECTED [§5.2, §5.4, §5.6, §7.2]
Four faults, all the same failure from a seat: the appliance knew something and
the patron could not tell.

**An uncaptioned segment went blank.** The fan-out has always sent
`language_available:false` — with a comment saying "a blank caption area forever
is exactly the silent degradation guardrail #7 forbids" — and `patron.js` never
read the field. On an uncaptioned reel between captioned reels, the exact case
§5.2 item 5 was written for, a deaf patron saw an empty black box and nothing
else. From a seat, mid-show, that is indistinguishable from a phone that has
stopped working, and it is the judgement they are left to make. The page now
shows "No captions for this part of the program" — copy that already existed for
a different case — and checks it BEFORE the link status, because during an
uncaptioned segment the link is legitimately "active": nothing is wrong, there is
simply nothing to show, and that is a different thing to say.

**A subtitle track was presented as captions.** `caption_grade` was on the wire
and unread. §5.4 asks for this because a subtitle track carries dialogue while a
caption-grade track carries speaker names, sound effects and music cues — the
information a deaf patron is relying on. The page now names it and says what is
missing. A caption-grade track shows NOTHING: a note on every show would be read
by nobody.

**A listening patron read "Connecting…" for the whole show.** §7.2's connection
indicator was driven only by the caption transport, and `/listen` opens no
caption transport at all — so sound came out of the headphones while the status
light said connecting. Wrong in the reassuring direction, on the one element a
patron checks when they think something is broken. The audio path now owns the
indicator on audio-only routes and ONLY there: on a combined route the caption
transport stays the authority, because audio reporting "Connected" while the
caption socket is reconnecting would tell a deaf patron their captions are fine
at the moment they are not.

**A playlist that would not load produced one log line.** No state change, no
warning, nothing on the dashboard — and the next PlayPlaying advanced to PLAYING
without checking that anything had been loaded, so the patron was shown "active"
over a blank area. There is now a distinct `caption_rpl_unavailable` warning
(separate from a missing track file inside a playlist that DID load: different
fault, different remedy), and PLAYING with no playlist is no longer reported to a
patron as working captions.

**ERROR stays unreachable, deliberately, and that is now written down.** §5.2
lists ERROR as a state and no code assigns it, which the audit reported as a gap.
It is not one. The supervisor already routes every transport-class fault to
DISCONNECTED and re-runs the connection sequence — "one recovery route, not two"
— and `model.CaptionLinkState` documents ERROR as "a fault the link cannot carry
on from", reporting the server as NOT CONNECTED. An unfetchable playlist is not
that: the link is fine and the next Set RPL may well succeed. Entering ERROR for
it would send a tech to check cables for a fault in the DCP. The real defect was
the silence, not the missing state, and the collapse is a deliberate choice that
was simply never recorded.
