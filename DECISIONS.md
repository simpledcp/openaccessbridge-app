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
