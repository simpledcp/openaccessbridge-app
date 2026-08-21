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

