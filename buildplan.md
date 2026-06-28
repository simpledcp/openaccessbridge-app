# CaptionBridge — Phased Build Plan

**A local cinema accessibility appliance: ST 430-10/ST 430-11 captions to smartphones, plus HI/VI audio from low-cost analog inputs.**

Version: Build-plan draft 1.2 (incorporates a second external technical-review pass)
Source: `CaptionBridge_Project_Concept.md`
Status: Engineering plan for review. Not a compliance certification, not legal advice.

---

## 0. How to read this document

This is an implementation plan for an engineering team, written so that it can be executed in phases with hard go/no-go gates between them. It is deliberately opinionated: where the concept brief left a decision open, this plan makes a recommendation and states the rationale and the risk. Anything marked **OPEN** is a blocker that must be resolved with real hardware, a real cinema server, or a venue/legal stakeholder before the relevant phase ships.

The document is organized as: scope and guardrails → reference baseline → architecture → hardware → the two functional subsystems (captions, audio) → the two interfaces (patron, admin) → security → data models → test strategy → deployment profiles → the phased plan with MVP boundaries → unresolved standards questions → consolidated risks.

The phased plan in Section 13 is the spine. Everything before it is the design those phases build toward.

---

## 1. Scope, guardrails, and what CaptionBridge is (and is not)

### 1.1 One-sentence definition

CaptionBridge is a locked-down, dual-interface appliance that consumes a cinema server's standardized off-screen timed-text feed (SMPTE ST 430-10 synchronization + ST 430-11 resource lists + referenced timed-text assets), captures hearing-impaired (HI) and visually-impaired narrative (VI-N) audio from whatever analog output a venue can provide, and serves both to patron smartphones over a local web page using a low-latency browser audio path — without ever routing packets between the theater network and the patron network.

### 1.2 Hard guardrails (these are non-negotiable and shape every later decision)

Guardrails 1–6 are lifted directly from the brief and treated as invariants throughout; guardrail 7 is added by this plan as a derived invariant:

1. **Captions come from the server-side ST 430-10/ST 430-11 path, never from the auditorium IR/RF emitter.** Reverse-engineering Dolby/USL/QSC/Sony receiver protocols is explicitly out of scope as the initial path and is not assumed anywhere in this plan.
2. **The appliance is an application-layer bridge, not an IP-layer device.** It is never a router, NAT, bridge, proxy, or general-purpose gateway. IPv4 and IPv6 forwarding are disabled at the kernel and re-checked continuously.
3. **Consumer analog audio input is a first-class supported path,** not a development fallback. The mini-PC mic/line jack and a cheap USB audio dongle are treated as real field deployment targets.
4. **WebRTC/Opus is the primary patron audio transport.** HLS and similar are debug/fallback only.
5. **No persistence of caption text or HI/VI audio by default.** All caption and audio data lives in transient memory and is purged at show end. Any debug capture is explicit, admin-only, time-limited, size-limited, and visibly indicated.
6. **Default-deny firewalling with verified interface isolation** is part of the product, not an afterthought, and is re-verified at install time and at runtime.
7. **No silent degradation.** If captions, HI, VI-N, network isolation, admin separation, or latency fall below the active profile's thresholds, the system must surface a clear staff warning *and* an appropriate patron-facing state. It must never present an unavailable or materially degraded service as if it were working — this is core to accessibility trust and underpins the loud-warning, four-state-qualifier, and patron-error-state behavior throughout.

### 1.3 What CaptionBridge is *not*, and a necessary caution

A phone-based caption/audio page is **not** automatically equivalent to a professional cinema accessibility system, and deploying it "in lieu of" one has operational and regulatory implications that this plan cannot resolve for you. In several jurisdictions (for example, US ADA obligations for digital cinema) exhibitors are required to provide and maintain working closed-captioning and audio-description equipment; whether a bring-your-own-device web approach satisfies that obligation depends on jurisdiction, enforcement posture, device availability, staff procedure, and how the venue operationalizes it.

This is not legal advice, and the author is not a lawyer. **Phase 9 of this plan exists specifically to force an operational and legal review before any venue relies on CaptionBridge as a replacement.** Treat the "potential replacement" framing as a product goal that must be *earned* through reliability work and venue review, not assumed.

**Access-equivalence is not only latency and reliability — it is device availability.** A bring-your-own-device model provides access only if the patron actually has a usable device: a compatible phone, a charged battery, headphones, workable network settings, and the comfort to scan a QR code and use a web UI in a dark auditorium. A venue cannot claim equivalent access on CaptionBridge alone unless it *also* has a procedure for patrons who have none of these — for example venue-owned loaner phones/tablets, loaner wired earbuds, battery packs, staff setup assistance, and a documented fallback to professional receivers where they exist. Without that, the residual risk is not performance; it is access-equivalence and operational availability, and Phase 9 must verify the procedure exists rather than only that the technology works.

### 1.4 Standards and references you must license and read before building

Public summaries (used to write this plan) are enough to design the architecture, but the actual byte-level protocol and XML schemas must come from the licensed standards. At minimum, obtain and read:

- **SMPTE ST 430-10** — *D-Cinema Operations — Auxiliary Content Synchronization Protocol (ACSP).* The caption sync/lease/timeline protocol. This plan's state machine (Section 5) is built from public ISDCF implementation notes and must be reconciled against the real PDU definitions.
- **SMPTE ST 430-11** — *D-Cinema Operations — Auxiliary Resource Presentation List (RPL).* The XML list that points at timed-text assets.
- **SMPTE ST 429-12** — *D-Cinema Packaging — Caption and Closed Subtitle.* Defines, at the Composition Playlist level, the on-screen `MainSubtitle`/`MainCaption` and the **off-screen (closed) `ClosedSubtitle`/`ClosedCaption` text tracks** (up to six per reel). The closed text tracks are exactly what CaptionBridge consumes via the ST 430-11 RPL, so this standard — not just ST 428-7 — governs how an accessibility device distinguishes captions from subtitles.
- **SMPTE ST 429-5** — *D-Cinema Packaging — Timed Text Track File.* The MXF track-file wrapper that all D-Cinema timed-text assets are packaged in; relevant for understanding how referenced assets are carried and resolved.
- **SMPTE ST 428-7** — *D-Cinema Distribution Master — Subtitle.* The base subtitle timed-text XML grammar (used directly for on-screen subtitles).
- **SMPTE ST 428-10** — *D-Cinema Distribution Master — Closed Caption and Closed Subtitle.* The XML profile that **closed** (off-screen) caption/subtitle text conforms to — i.e., the format CaptionBridge's parser actually receives for accessibility tracks. It builds on and constrains ST 428-7 rather than replacing it. (Plus **legacy Interop subtitle XML** in older content.)
- **ISDCF** — *SMPTE ST 430-10 Implementation Notes* (Hallikainen, 2018) and the ISDCF channel-assignment / accessibility guidance for HI and VI-N channel definitions.
- Vendor context (to understand the professional baseline you are standing in for): Dolby Accessibility Solution / AccessLink / Fidelio product documentation.

> **Note on inferred protocol detail.** Throughout Section 5, request names and sequences (Announce, Get New Lease, Update Timeline, Set RPL Location, Set Output Mode, Terminate Lease) and behaviors such as "request interval = half the lease" and "continue output on comms loss until lease expiry" are taken from public ISDCF implementation notes. They are accurate enough to design against, but the team **must** confirm exact framing, transport, and field semantics against the licensed ST 430-10 text and against the first real target server.

---

## 2. Reference baseline: the professional system you are standing in for

Understanding the professional baseline clarifies one architectural decision that dominates this entire plan.

A professional system (e.g., Dolby's accessibility receiver line) does two things at once: it takes the **closed-caption text** *and* the **HI and VI-N audio** directly out of the decrypted DCP, in perfect sync, and streams all three wirelessly to a dedicated handheld/seat receiver. Because it lives inside the playback security boundary, it gets the audio essence digitally and time-aligned for free.

**CaptionBridge cannot do that, and must not try.** It is an outboard device with no access to decrypted audio essence. This produces the defining asymmetry of the whole product:

| Path | Professional receiver | CaptionBridge |
|---|---|---|
| **Captions** | From decrypted DCP timed text, in sync | From ST 430-10/ST 430-11 — *the same standards-based auxiliary-content path professional accessibility systems are designed to consume*. **Protocol/source parity is achievable**; full *rendering* parity (styling, positioning, image subtitles) is a later milestone (Section 5.4, Phase 8). |
| **HI / VI-N audio** | From decrypted DCP audio essence, digitally, sample-accurate | From an **analog tap** of the cinema processor's accessibility outputs. Quality and latency are bounded by booth wiring and a consumer ADC. Parity is *not* achievable; "good enough for assistive listening" is the goal. |

Two consequences fall out of this and recur throughout the plan:

1. **The caption path can be excellent; the audio path is constrained by physics and wiring.** That is why captions and audio have fully independent readiness/health state everywhere in this design — you can have perfect captions and a dead VI feed, or clean HI audio and no captions.
2. **HI/VI audio is sourced from the cinema audio processor's assistive-listening analog outputs** — typically the very same HI and VI-N outputs that would normally feed the venue's IR/RF assistive-listening transmitter. In SMPTE/ISDCF channel assignments, HI and VI-N are discrete channels the processor decodes and can route to analog outputs. Confirm the exact output jacks/levels per processor and per venue during install (this is an **OPEN** field question, Section 14).

---

## 3. Overall architecture

### 3.1 System context

```text
┌──────────────────────── THEATER SIDE (eth0) ────────────────────────┐
│                                                                      │
│  Cinema server / IMB  ⇄  ST 430-10 ACSP session (TCP)                 │
│        │              ⇄  RPL locator/data + timed-text retrieval       │
│        │                 (direction + asset form per server; Phase 0A) │
│        │                                                             │
│  Cinema audio processor ── HI analog out ──┐                         │
│        (CP / renderer)  ── VI-N analog out ─┤ (the same feeds that   │
│                                            │  drive the IR/RF Tx)    │
└────────────────────────────────────────────┼───────────────────────┘
                                              │ analog wiring
                  ┌───────────────────────────▼───────────────────────┐
                  │              CaptionBridge appliance               │
                  │                                                    │
                  │  Caption plane:  ACSP client → RPL parser →        │
                  │                  timed-text parser → timing engine │
                  │                  → caption fan-out (WS/SSE)        │
                  │                                                    │
                  │  Audio plane:    ALSA/USB capture → qualifier →    │
                  │                  channel mapper → Opus encode →    │
                  │                  WebRTC (webrtcbin) peer          │
                  │                                                    │
                  │  Control plane:  admin/setup, dashboards,         │
                  │                  firewall mgr, health, updates    │
                  │                                                    │
                  │  *** NO IP FORWARDING BETWEEN INTERFACES ***       │
                  └───────────────────────────┬───────────────────────┘
                                              │ web UI + WS/SSE + WebRTC
┌──────────────────────── PATRON SIDE (wlan0 / eth1) ─────────────────┐
│   Patron phones (browser + headphones)   Admin laptop/tablet         │
│   /accessibility  /caption  /listen      /admin                      │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.2 Three planes, two physical interfaces

The software is decomposed into three logical planes that map onto a strict two-interface physical model:

- **Caption plane** (theater-facing input → patron-facing output): the ACSP client/state machine, RPL parser, timed-text parser, timing engine, and the caption fan-out that pushes the active cue to browsers.
- **Audio plane** (theater-facing analog input → patron-facing output): capture, qualification, channel mapping, Opus encode, WebRTC delivery, and continuous health monitoring.
- **Control plane** (admin-facing only): setup wizard, dashboards, firewall manager, calibration wizard, diagnostics, logging, support mode, update manager.

The theater-facing **eth0** sees only the allowlisted ACSP/TCP relationship with the configured cinema server — in the confirmed initiation direction for that server family (Phase 0A) — plus any permitted timed-text asset retrieval, and (passively) the analog audio jacks. The patron-facing **wlan0/eth1** sees only the web UI, caption stream, and audio stream. No plane forwards packets between the two; data crosses the appliance only as fully terminated, re-originated application traffic.

### 3.3 The "application-layer bridge" principle, made concrete

"Application-layer bridge, not IP-layer bridge" is enforced by construction, not policy:

- The caption plane **opens an outbound TCP connection** from CaptionBridge to the cinema server — CaptionBridge initiates the socket, and the server then drives the *application-layer* request/response. (These are two different layers, not a contradiction.) The exact TCP-initiation direction is confirmed against a real server in **Phase 0A** before the firewall and simulator are built around it. The patron plane never connects to the server; it connects to CaptionBridge. There is no socket, route, or rule that joins them.
- Captions are decoded into an internal cue model and re-serialized into a separate WebSocket/SSE protocol for browsers. Patrons never receive raw ST 430-10/-11 traffic.
- Audio is decoded from analog, re-encoded to Opus, and delivered via a fresh WebRTC peer connection. No theater-side network audio (there isn't any in the supported path) is ever proxied.
- Kernel `net.ipv4.ip_forward=0` and `net.ipv6.conf.all.forwarding=0`; no bridge devices; nftables default-deny with no `forward`-chain accepts (Section 9). A runtime watchdog re-asserts these and the dashboard screams if any flip.

### 3.4 Recommended technology stack

Build plans should make decisions. The recommendation, with rationale:

| Concern | Recommendation | Why |
|---|---|---|
| **Caption/control services** (ACSP state machine, RPL/timed-text parsing, timing engine, web/admin server, firewall mgr) | **Go** (single static binary, systemd-managed) | Strong concurrency for the state machine + many WebSocket clients; trivial deployment as one binary on a read-only-rootfs appliance; easy cross-compile for arm64/amd64; good XML and net libraries. |
| **Media pipeline** (analog capture → level/qualify → Opus → WebRTC) | **GStreamer with `webrtcbin`**, run as a **separate supervised process** (`captionbridge-media`), not embedded in the Go binary (see 3.5) | Battle-tested ALSA capture, level analysis (`level` element), Opus encode, and standards-compliant WebRTC in one well-supported pipeline; far less risky than hand-rolling RTP/SRTP. Isolating it in its own process keeps a media crash from taking down the caption lease. |
| **Patron + admin UI** | Static SPA (lightweight; e.g., Preact/Svelte) served by the Go binary; no SSR framework | Keeps the appliance dependency-light and offline; UI is simple enough not to need a heavy framework. |
| **Config/state store** | SQLite for *configuration and profiles only* (never caption/audio content) | Single-file, crash-safe, easy backup/restore for auditorium profiles. Content stays in memory. |
| **OS** | Minimal Debian/Ubuntu LTS or a Yocto-built image for the hardened tier | LTS for speed early; Yocto for a reproducible, read-only, A/B-updatable hardened image later. |

**Acceptable alternative:** Python + `aiortc` + GStreamer for faster prototyping in Phases 0–5, with a planned port of the control plane to Go before the hardened tier. The one piece this plan strongly recommends *not* reinventing is the WebRTC/Opus media path — use GStreamer/`webrtcbin` (or `aiortc`) rather than a bespoke RTP stack. The process boundary in 3.5 holds regardless of which media backend is used.

### 3.5 Process and privilege separation

The appliance runs as **three supervised processes** from day one rather than one monolith, communicating only over local loopback (Unix domain socket or loopback gRPC). This makes the plan's "captions and audio are independent" principle true at the process boundary, not just in state:

```text
captionbridge-core      Go: ACSP state machine, RPL/timed-text, timing engine,
                        caption fan-out, web/admin server, sessions
captionbridge-media     GStreamer: ALSA capture → qualify → Opus → WebRTC
captionbridge-firewall  privileged helper: nftables apply + sysctl/forwarding
                        verification (the only component that needs root for
                        network state)
```

Benefits: a GStreamer/media crash cannot drop the caption lease or the admin surface; the privileged network-verification helper is a small, auditable component kept separate from the large web/parsing surface; each process restarts and health-checks independently; and the media backend can be swapped later (e.g., `aiortc`) without touching the caption core. `captionbridge-core` supervises the others (systemd units with restart policies) and reports each one's liveness to the dashboard.

---

## 4. Hardware targets

Three tiers, matching the brief's prototype / low-cost / hardened deployment intents. All tiers require **two network interfaces** (one theater-facing, one patron-facing) and a clean way to present a patron Wi-Fi.

### 4.1 Prototype / bench tier

Purpose: Phases 0–5 development and lab testing.

- **Compute:** Raspberry Pi 5 (8 GB) *and* a fanless x86 mini-PC (e.g., N100-class). Develop against **both** from the start so you don't bake in Pi-specific or x86-specific audio quirks — the audio chipset differences *are* the test surface in Phase 0C.
- **Networking:** onboard Ethernet (theater side) + a known-good USB-Ethernet or the second NIC for patron side; onboard Wi-Fi acceptable for the bench only.
- **Audio:** built-in jack (where present) + a small pile of cheap USB dongles for the compatibility matrix.
- **Storage:** ordinary SSD/SD; no hardening yet.

### 4.2 Low-cost deployment tier (the "replacement" workhorse)

Purpose: real venues that have only basic analog outputs and a non-specialist installer.

- **Compute:** fanless x86 mini-PC (N100/N305-class, 8 GB, dual NIC) preferred over a Pi for thermal stability, USB-audio reliability, and BIOS-level watchdog support. A Pi 5 is acceptable where cost dominates and a CM4/CM5 carrier with dual Ethernet is used.
- **Networking:** **two wired NICs** strongly preferred. For patron Wi-Fi, a **dedicated external access point** on the patron NIC rather than onboard Wi-Fi (capacity, range, and reliability for a full auditorium — the professional baseline uses a dedicated router for exactly this reason).
- **Audio:** the shipped **low-cost audio kit** (Section 6.2). Recommend a USB audio *interface with a real line input* (Tier 2) over a bare dongle wherever budget allows, because gain control and noise floor are dramatically better and produce far fewer field surprises.
- **Storage:** SSD with room for an A/B update layout.

### 4.3 Hardened appliance tier

Purpose: production "in lieu of professional system" deployments and chain rollouts.

- **Compute:** industrial fanless mini-PC, dual wired NIC, hardware watchdog, TPM (for signed-update key storage and secure boot where available), wide-temp rating for booth environments.
- **Networking:** two wired NICs + certified external AP(s); optional multiple APs for large rooms.
- **Audio:** Tier 2 USB interface as the default shipped input; Tier 3 (balanced/AES3/Dante/AES67) as a documented upgrade path (Section 6) — **not** required for the appliance to function.
- **Storage / OS:** read-only root filesystem, A/B partitions for atomic signed updates, encrypted config partition, secure boot where supported.
- **Physical:** lockable/rack-mountable, tamper-evident, clear labeling of theater vs patron ports to prevent miswiring in the booth.

---

## 5. Caption subsystem (ST 430-10 / ST 430-11 / timed text)

The caption subsystem is where **protocol/source parity** with a professional device is achievable, because it consumes the *same* standards-based auxiliary-content feed — though full *rendering* parity (styling, positioning, image subtitles) is a later milestone (5.4, Phase 8). It has five stages: the ACSP transport/state machine, the RPL parser, the timed-text parser, the timing engine, and the caption fan-out.

### 5.1 ACSP transport and role model

Per the public reference material, ST 430-10 (the Auxiliary Content Synchronization Protocol, ACSP) is an Ethernet/TCP request–response protocol. The role model — **confirm against the licensed standard and your first target server** — is:

- The **cinema server / media block (IMB)** is the synchronization source.
- **CaptionBridge is the Auxiliary Content Server (ACS)** — the device that ultimately serves auxiliary content (captions) to patrons.
- **CaptionBridge (the ACS) opens the TCP connection** to the media block; once connected, the media block drives the *application-layer* exchange by issuing ACSP requests, to which the ACS responds. (Client-initiates-TCP / server-drives-protocol is one pattern, not two contradictory ones — but the exact TCP-initiation direction is itself confirmed in **Phase 0A** against a real server before anything is built around it.) CaptionBridge then **retrieves the referenced timed-text resources** from the media block based on the timing information it receives (the resource form — direct XML, a track-file reference, or an MXF-wrapped track — is a Phase 0A finding; see 5.3).

Two registration facts are **OPEN** and gate any real deployment (Section 14); both are closed by the **Phase 0A ACSP Wire Contract** (Section 13):

1. **Endpoint registration:** does the target server auto-discover an ACSP device, or must an operator configure CaptionBridge's IP into the server's accessibility settings? This varies by server family and directly affects installability.
2. **Connection initiation direction and ports:** confirm which side initiates TCP, on which port(s), and whether any TLS/auth is involved, per server family.

> DCI security rules permit decrypted timed-text track files to be stored in the clear locally. CaptionBridge is therefore *permitted* to fetch and cache timed-text XML — but per guardrail #5 it still keeps that cache **in transient memory and purges at show end** rather than persisting it.

### 5.2 ST 430-10 state machine

Do not model ACSP as a parser; model it as a state machine with an explicit lease timer and timeline mapping. States (extending the brief's model):

```text
DISCONNECTED
   │  (TCP connect to allowlisted server OK)
   ▼
CONNECTED ──► ANNOUNCED ──► LEASED ──► TIMELINE_ACTIVE ──► RPL_RECEIVED ──► READY
                                                                              │
                                                  Set Output Mode = enabled   │
                                                            ▼                 │
                                                         PLAYING ◄────────────┘
                                                            │
            ┌──────────────┬───────────────┬───────────────┼───────────────┐
            ▼              ▼               ▼               ▼               ▼
         PAUSED         STOPPED        SHOW_CHANGE     LEASE_EXPIRED       ERROR
            │              │               │               │               │
            └──────────────┴───────────────┴───► (re-run connection/new-show sequence) ◄┘
```

**ACSP request types to handle** (names per ISDCF notes): `Announce`, `Get New Lease`, `Terminate Lease`, `Update Timeline`, `Set RPL Location` (one or more), `Set Output Mode`, and the periodic status request/response that carries lease and timeline state.

**Canonical sequences to implement and test:**

- **New connection / reconnection (server → ACS):** `Announce` → `Get New Lease` → `Update Timeline` → `Set RPL Location(s)` → `Set Output Mode` (reflecting current enabled/disabled status). This must work whether the connection arrives before or during playback.
- **New show load:** `Terminate Lease` → `Get New Lease` → `Update Timeline` → `Set RPL Location(s)`. If `Terminate Lease`/`Get New Lease` is *not* re-sent on a new show, the ACS would otherwise keep holding the abandoned previous show's content — handle this explicitly.

**Behaviors that are easy to get wrong (bake these into tests from day one):**

1. **Request interval = half the lease interval.** Some servers push status requests close to lease expiry; with the half-lease cadence the lease should never lapse from timing jitter. Treat anything approaching the lease as a warning.
2. **Continue output on comms loss until the lease expires.** ST 430-10 (per the implementation notes, §7.2.6.6) requires the ACS to keep outputting resources through a momentary communications interruption until the lease times out. CaptionBridge must therefore *keep showing valid captions across a brief Ethernet blip* and only blank or warn once the lease genuinely expires. This is a reliability feature, not a bug.
3. **`Set Output Mode` = the server's play/stop status.** Use it to drive a "welcome / no captions for this content" state versus an active-caption state. Some servers only send it for captioned content; handle its absence gracefully (assume unknown rather than crash).
4. **Playout ID uniqueness quirks.** Some servers send `Playout ID = 0` for every uncaptioned composition (trailers, etc.), producing several RPLs and timeline segments with non-unique Playout IDs that can look like a "loop back" to naïve code. Do **not** treat repeated/zero Playout IDs as a seek-to-start. Disambiguate using timeline position, not Playout ID alone.
5. **Empty `ReelResources`.** Reels/compositions without captions legitimately carry empty resource lists. Render "no captions for this segment," not an error.
6. **Lease-expiry recovery.** On a reported lease expiry, attempt the full new-connection sequence to self-heal rather than sitting in a dead state.

The state machine emits a single observable `caption_link_state` to the dashboard and a derived patron-facing status (e.g., *active*, *waiting for show*, *temporarily reconnecting*, *unavailable*).

### 5.3 ST 430-11 RPL parser

The RPL (Auxiliary Resource Presentation List) is the XML the server provides (or points to) that enumerates, per reel, the auxiliary resources — including the timed-text assets for captions/subtitles. The parser must:

- Parse the RPL identified by each `Set RPL Location`, keyed by its (ideally unique) **Playout ID**, and maintain a map from timeline segments → reel resources.
- Identify **caption/subtitle timed-text resources** among the reel resources, distinguishing closed-caption text tracks from other auxiliary resource types, and capturing **language** per track for multi-language selection.
- Resolve and **fetch the referenced timed-text resources** from the media block on demand, honoring the timeline so it can **skip fetching reels already past** (use the `Update Timeline` value to avoid pulling content that will never play — important on mid-show reconnects).
- Tolerate the quirks in 5.2 (zero/duplicate Playout IDs, empty `ReelResources`, abandoned/rewound RPLs) and discard stale fetched content safely (an extra cached copy after a reconnect is fine; it is dropped once recognized as stale).

**Resource locators are a compatibility surface, not a solved abstraction.** The RPL provides *locations* for resources, but the locator scheme, path form, and fetch mechanism vary by server family and may not be fully pinned even by the licensed standard. Build the fetcher as a **pluggable resource resolver** with handlers for the forms seen in the field, and **log and classify every unresolved locator form by server family** so coverage grows from real observations (seeded by the Phase 0A wire capture):

```text
resolver handlers (extend as server families are characterized):
  server-relative reference exposed over the ACSP-compatible mechanism
  http://server/path
  https://server/path
  file-like path served by the media block
  vendor-specific form  →  logged + classified, never silently dropped
```

**Bounded transient cache (no eager whole-show fetch).** Because nothing is persisted and a long multi-language feature could otherwise consume meaningful RAM, the fetcher is governed by an explicit cache policy:

- a **fetch-ahead window by reel** (fetch the current and next reel(s), not the whole composition);
- a **maximum transient cache size**, with **eviction at reel and show boundaries**;
- **language-prioritized fetch** — fetch the patron-selected and venue-default languages first;
- a defined behavior when a patron **switches to a language not yet fetched** (fetch on demand behind a brief "loading captions…" state rather than blocking the other languages).

**The fetched resource may not be plain XML.** Depending on the confirmed server behavior (Phase 0A), the resolver may receive direct XML essence, a track-file reference, an **MXF-wrapped timed-text track file (ST 429-5)**, or server-extracted XML. The resolver pipeline is therefore explicit:

```text
resource resolver → optional track-file / MXF (ST 429-5) unwrap
                  → timed-text XML → cue normalization (5.4)
```

Do not build only an XML fetcher: the unwrap step must exist even if the first target server happens to return plain XML, so that a track-file-returning server is a configuration case, not a rewrite.

Output of this stage: a set of language-tagged timed-text **XML** documents (post-unwrap) with their reel/timeline anchoring, handed to the timed-text parser.

### 5.4 Timed-text parser and cue normalization

By the time assets reach this stage they are timed-text XML (the resolver in 5.3 has handled any track-file/MXF unwrap). Get the format expectations right, because the **closed** (accessibility) tracks are not simply on-screen subtitles:

- **On-screen subtitles** conform to **SMPTE ST 428-7** (the base subtitle XML grammar).
- **Closed captions / closed subtitles** — the off-screen tracks an accessibility device actually consumes via the ST 430-11 RPL — conform to **SMPTE ST 428-10**, which builds on and constrains ST 428-7 rather than replacing it, and are declared as `ClosedCaption`/`ClosedSubtitle` tracks per **ST 429-12**.
- **Legacy Interop subtitle XML** appears in older content.

So the parser must handle the ST 428-7 grammar *plus the ST 428-10 closed-caption profile* plus Interop, and must read the ST 429-12 track type to know whether a track is a caption or a subtitle. It normalizes all of these into the internal cue model (Section 10):

- **Phase-1 priority:** correct **text**, correct **language**, correct **start/end timing** relative to the reel/timeline, and robust handling of edit-rate/timecode → milliseconds conversion. Resolve cue times into a single timeline-relative `start_ms`/`end_ms`.
- **Defer:** styling, positioning, fonts, ruby, and PNG-image subtitle rendering. Carry `style`/`position` as opaque pass-through fields but do not render them until the feed and timing are proven stable (the brief is explicit about this ordering).
- Validate against malformed XML, missing assets, and unexpected namespaces without taking down the caption link; a bad asset for one reel should degrade to "no caption" for that reel, not a crash.

**Closed captions are not the same as subtitles, and the distinction is an accessibility decision, not cosmetics.** Closed captions carry speaker identification, sound effects, and music cues for deaf and hard-of-hearing patrons; foreign-language subtitles typically carry only translated dialogue. The track-selection model:

- In accessibility mode, **prefer `ClosedCaption` tracks over `ClosedSubtitle`/subtitle tracks** of the same language.
- If only subtitle tracks exist, the patron-facing copy should say "**Subtitles available**" rather than "Closed captions," unless the content type is known — so a patron relying on caption-grade information is not misled.
- **Multi-language default priority** — **provisionally set here and validated in Phase 7 UX testing (no longer an OPEN standards blocker; see 5.6 and Section 14)**: (1) venue/show-configured default; (2) browser-language match; (3) first closed-caption track; (4) first subtitle track.

### 5.5 Timing engine

The single most important caption-quality component. It must **continuously map the server playback timeline to a local monotonic clock and resolve the currently-active cue** — never a `sleep until next cue` loop.

```text
server timeline position (from Update Timeline + elapsed)  ──┐
                                                             ├─► active-cue resolver ─► fan-out
local CLOCK_MONOTONIC (drift-free, immune to NTP steps) ─────┘
```

Requirements:

- On each `Update Timeline`, capture `(server_timeline_position, local_monotonic_timestamp)` as an anchor; between updates, advance the predicted server position from the monotonic delta, re-anchoring whenever a fresh timeline message arrives.
- Handle **play, pause, resume, stop, seek/jump, SPL/CPL changes, communications loss, and lease expiry** as explicit timeline events. Pause freezes prediction; resume re-anchors; seek discards the prediction and re-resolves.
- Resolve the active cue by interval lookup against the normalized cue set for the current reel; push **only on change** (cue-in / cue-out / text change), not on a fixed tick, to keep the fan-out quiet and the phone UI stable.
- Respect a configurable **drift/latency budget** (target captions on-screen within ~250–500 ms of the reference, per the brief) and expose measured drift to diagnostics.

### 5.6 Caption fan-out to browsers

- Push active-cue changes to phones over **WebSocket** (primary) with **SSE** as a simpler fallback. Payload is the minimal cue (text, language, cue id, visible/cleared), never raw server traffic.
- Support **multiple simultaneous caption languages**: the server feed may carry several tracks; the fan-out serves the patron's selected language and switches without reload (language-selection UX in Section 7). The default-language selection policy is **provisionally defined in 5.4** and is validated in Phase 7 UX testing rather than left as a standards blocker.
- Stateless reconnect: a phone that drops and reconnects immediately receives the current active cue (or "cleared") so it never waits for the next cue boundary.
- Backpressure/limits: cap clients per the deployment profile; never block the timing engine on a slow client.


## 6. Audio subsystem (HI / VI-N over low-cost analog input)

The audio subsystem is the part most exposed to imperfect, inconsistent hardware. The governing principle from the brief is that the software must **actively qualify** the input and tell the installer whether it is *excellent / usable / marginal / failed* — it must never merely accept whatever it is handed and assume success.

### 6.1 Capture architecture

- Capture via **ALSA** (optionally through PipeWire) using stable device identifiers (by-id / by-path), not card indices that renumber on reboot. Persist the chosen device as part of the auditorium profile.
- Fixed internal capture format: **48 kHz**, with per-input channel count detected (mono vs stereo), feeding a per-input analysis tap and the channel mapper before encode.
- Each configured input runs an independent capture → analyze → map → encode → stream chain so HI and VI-N are fully independent (independent status is a hard requirement).
- Hot-plug aware: detect device connect/disconnect and surface it immediately (USB dongles get bumped in booths).

### 6.2 Input tier model and the shipped low-cost kit

The capture layer is written once but is validated against, and documents support for, four tiers:

```text
Tier 0 — Built-in mini-PC mic/line jack
         Required fallback. Highest variability (mono, TRRS, electret bias,
         AGC, easily overloaded by line level). First-class supported, but
         the qualifier must aggressively warn.
Tier 1 — Cheap USB audio dongle
         Recommended minimum practical deployment. Isolates from mainboard
         audio quirks. Still consumer-grade.
Tier 2 — USB audio interface with real line input  ◄ default shipped input
         Preferred low-cost deployment. Real gain control, lower noise,
         far fewer field surprises.
Tier 3 — Balanced / AES3 / Dante / AES67
         Premium / future. Same capture abstraction; do not require it.
```

**Shipped low-cost deployment kit** (so a non-specialist installer can succeed): a known-compatible USB audio interface/dongle, 3.5 mm TRS/TRRS adapters, RCA→3.5 mm and dual-RCA→stereo-3.5 mm cables, an **inline attenuator/pad** for line-level → mic-level reduction, a **ground-loop isolator**, short shielded cables, and an optional splitter/test cable. The bar is "predictable field deployment," not audiophile quality. The known-good kit contents are an output of **Phase 0C**.

### 6.3 Input qualification engine

Runs during calibration *and* continuously during playback. For each input it measures and classifies:

- **Presence / device health:** input present, device connected, not failed.
- **Level:** current `dBFS`, with thresholds for too-low (mic into line) and clipping/overload.
- **Clipping / overload detection** (sample saturation + sustained near-0 dBFS).
- **Sustained silence detection** (distinguish "no signal" from "quiet passage" via duration + noise-floor context).
- **Noise floor estimate** (quiet/moderate/high) — critical for consumer mic inputs.
- **DC offset** (common on electret/biased mic inputs).
- **Mono vs stereo capability** (so an impossible stereo mapping is caught before showtime).
- **Stability** (device renumbering, dropouts, sample-rate glitches).

It emits one of four states per input — **Excellent / Usable / Marginal / Failed** — plus plain-language reasons, e.g.:

```text
Marginal: signal present, but noise floor is high.
Marginal: audio is clipping — reduce source level or add the inline attenuator.
Failed:  no input detected.
Failed:  input appears mono, but this profile requires two channels.
Failed:  configured VI input is silent during playback.
Warning: this input appears mic-level and may distort if fed a line-level
         cinema output — use the supplied attenuator pad.
```

### 6.4 Audio calibration wizard

A guided, non-specialist workflow in the admin UI (this is what makes the low-cost replacement profile viable in the field):

1. **Select input device** (from detected devices, with friendly names).
2. **Select intended use:** HI, VI-N, or dual-channel HI/VI.
3. **Select channel mapping** (Section 6.5).
4. **Prompt the installer to play a known signal** — a test tone, trailer audio, or the venue's accessibility feed — from the cinema system.
5. **Show a live input meter** (the qualifier's real-time output).
6. **Detect** silence, clipping, DC offset, heavy noise, mono/stereo availability, and unstable device behavior.
7. **Recommend** a concrete fix: lower source level, add the attenuator pad, insert the ground-loop isolator, switch to a USB interface, etc.
8. **Confirm usability** with an explicit Excellent/Usable/Marginal/Failed verdict and a plain-language summary like:

```text
Input detected:    Yes
Signal level:      Low but usable
Clipping:          No
Noise floor:       Moderate
Stereo channels:   No — mono only
Recommended mode:  VI-N only or HI only
Warning:           Mic-level input; may distort on a line-level output.
```

9. **Save an auditorium input profile** (device id + mapping + gain notes + verdict) so a re-open of the same room reuses known-good settings.

### 6.5 HI / VI-N channel mapping model

Because low-end inputs may offer only one mono signal or a single stereo pair, mapping must be flexible. Supported modes:

```text
Mono input:
   one stream only — HI or VI-N

Stereo input:
   Left = HI,  Right = VI-N
   Left = VI-N, Right = HI
   summed mono = HI
   summed mono = VI-N
   stereo pair = HI
   stereo pair = VI-N

Multiple USB inputs:
   Device A = HI,  Device B = VI-N
```

Admin selection mirrors this directly (mono HI / mono VI-N / stereo split either direction / stereo-as-one / multi-device). The mapper sits between capture and encode and is the single place that decides which captured channel(s) feed which named stream. A mapping that the detected hardware cannot satisfy (e.g., stereo split on a mono device) is a **Failed** qualifier state surfaced *before* a show, not a silent fault during one.

### 6.6 Continuous health monitoring

During playback, per input/stream, continuously report: input present/missing, device connected/disconnected, current level, clipping, sustained silence, noise floor, dropouts, **connected listener count**, encoder state, **estimated stream latency**, and browser playback/reconnect state where the browser exposes it. These drive both the admin dashboard tiles (Section 8) and the patron-facing error states (Section 7). A configured stream that goes silent or unavailable mid-show raises a loud admin warning *and* a calm patron message.

### 6.7 WebRTC/Opus streaming design

**Topology.** One **capture/analysis/Opus source per named stream** (HI, VI-N), distributed to **one WebRTC peer connection per browser subscriber** — not literal multicast. The appliance is the WebRTC peer (via GStreamer `webrtcbin`); each phone is a separate remote peer with its own SRTP/session state. The design must avoid re-running capture/analysis per client; whether the *encoded* Opus packets can be reused across peers (vs. re-packetized per peer) depends on the media backend, so treat that as an implementation detail of `captionbridge-media`, not a guarantee. Because every phone is on the appliance's own patron LAN, this is a single-hop, same-subnet path with **no NAT and no internet dependency**, so the path is designed for **host ICE candidates only**: **no internet STUN/TURN is expected or permitted in the production local-only path.** That host-candidate-only delivery is a Phase 0B **proof obligation**, not an assumption — mDNS `*.local` candidates, AP client-isolation, and mobile-browser quirks must be shown not to break it on the supported patron-network profile.

**Signaling.** SDP offer/answer + ICE are exchanged over the existing local web server's WebSocket — no third-party signaling. The patron page requests a stream (`HI` or `VI-N`), the appliance answers with a `webrtcbin` peer for that stream.

**Codec.** **Opus**, tuned for low latency: small frame size (10 ms; 20 ms as a stability fallback), `application=lowdelay`/`restricted-lowdelay` voice settings, FEC enabled for resilience over Wi-Fi, DTX **off** for VI-N/HI (assistive audio should not gate on silence detection in a way that clips speech onsets). Mono per stream (HI and VI-N are each single-listener-channel experiences; stereo-pair modes still deliver as a single Opus stream).

**Why not HLS for the primary path.** Segment-based transports add seconds of latency — unacceptable for HI where the patron also hears the auditorium. HLS/long-buffer transport is retained only as an explicit debug/fallback, never the synchronized patron path.

**Latency budget (HI is the hard target).** End-to-end "glass-to-ear" target: **under ~100–150 ms for HI**, under ~250 ms for VI-N. Realistic contributors:

| Stage | Typical | Lever |
|---|---|---|
| Analog capture + ALSA buffer | 5–20 ms | small period/buffer sizes |
| Level analysis + channel map | <5 ms | lightweight DSP |
| Opus encode (10 ms frame) | ~10–20 ms | small frame size |
| Local Wi-Fi transport | 5–30 ms | dedicated AP, 5 GHz, low contention |
| **Browser jitter buffer (dominant)** | **40–300+ ms** | `playoutDelayHint` / `jitterBufferTarget` minimized; good RF |
| Phone audio output | 10–40 ms | device-dependent |

> **Honesty flag (validate in Phase 0C/5, do not assume).** Browser WebRTC audio with default settings is frequently 150–400 ms end-to-end; the dominant, least-controllable term is the **phone-side jitter buffer**. The sub-150 ms HI target is *aggressive* and may be unachievable on weak Wi-Fi or some phones even with `playoutDelayHint` minimized. Treat the HI latency target as a **risk to be measured early** (Section 11.4), and be prepared to (a) tune the jitter buffer aggressively, (b) require a strong dedicated AP, and (c) set venue/staff expectations honestly if HI cannot be brought low enough on a given device fleet. VI-N's looser budget is comfortably achievable.

**Wired headphones are the recommended patron path, especially for HI.** The 10–40 ms phone-output figure above assumes a wired connection. **Bluetooth earbuds add large, device-dependent output latency** — commonly 100–300 ms, and even "low-latency" codecs add tens of milliseconds and are not guaranteed on a given phone — which lands directly on top of the jitter-buffer term and can push HI well past usability while the patron is also hearing the auditorium acoustically. Patron guidance and the QR/onboarding copy should recommend **wired headphones for HI**; Bluetooth may be acceptable for VI-N given its looser budget but should be treated as best-effort.

**Receive-side tuning.** Where the browser exposes it, minimize the jitter buffer via `RTCRtpReceiver` `playoutDelayHint`/`jitterBufferTarget`. Provide a small per-patron "audio is choppy → add a little buffer" control that trades latency for stability, so a patron on poor RF can self-rescue without staff.

**A real onboarding gotcha — secure context / mDNS.** Two browser realities must be designed for, not discovered late:
- **mDNS ICE candidates:** modern browsers emit `*.local` host candidates for privacy; the appliance must resolve/handle these for the same-LAN connection to complete. Plan for mDNS candidate handling (or appropriate configuration) in the signaling path.
- **HTTPS / secure context:** WebRTC *receive-only* playback can work over plain HTTP in current browsers, but autoplay policy requires a user gesture to start audio, and Wake Lock (Section 7) requires a secure context. A self-signed cert triggers scary warnings on patron phones. This is a known hard problem for local appliances; the candidate strategies are **evaluated up front in the Phase 0B browser local-appliance spike (Section 13) and locked by Phase 7**, tracked **OPEN** in Section 14: accept a documented cert warning with clear staff/QR guidance; provision a real cert via a domain that resolves to the private IP (adds DNS + renewal complexity and a provisioning-time internet dependency); or run HTTP and accept the gesture-to-play + no-wake-lock limitations. Whichever is chosen must be consistent across the patron fleet and rehearsed.


## 7. Unified patron web UI

One local browser app serves all patron-facing modes. It must be usable by people with hearing or vision needs, on their own phone, in a dark auditorium, possibly one-handed, with no app install.

### 7.1 Routes and modes

```text
/accessibility   Combined: captions + selectable HI/VI-N audio   (primary entry)
/caption         Captions only
/listen          HI/VI-N audio selection only
```

Supported experiences (selectable without reload): captions only; HI only; VI-N only; captions + HI; captions + VI-N; language selection where multiple caption tracks exist; audio-feed selection.

### 7.2 Patron UX requirements

- **Onboarding by QR:** a per-show QR encodes the local URL + session token (Section 10.4) so patrons join in one scan. No internet, no app.
- **Accessibility-first display:** large text, high-contrast themes, adjustable font size, comfortable line length, and a calm dark theme for auditorium use. Caption rendering ignores source styling/position in early phases (text correctness first).
- **Audio controls:** stream picker (HI / VI-N), volume, and the "add a little buffer if choppy" control (Section 6.7). Headphone-only assumption; warn if the phone is on speaker where detectable.
- **Connection state, always visible:** *connected*, *reconnecting…*, *waiting for show to start*, *unavailable*.
- **Auditorium identity, always visible:** the page shows which room it is serving (e.g., "You are connected to **Auditorium 3**") so a patron — and passing staff — can immediately catch a stale QR code or a phone that joined the wrong room's network. Show room identity only, never show plot/title details that would spoil the film. See the wrong-room safeguards in Section 8.5.
- **Screen wake-lock** where the platform allows (requires secure context — see Section 6.7), so the page doesn't sleep mid-film.
- **Clear, non-technical error states**, e.g.:

```text
VI audio is not currently available. Please contact theater staff.
HI audio signal is too low. Please contact theater staff.
Captions are waiting for the show to start.
Connection lost. Reconnecting…
```

- **No admin/diagnostic leakage.** The patron UI must never expose cinema server IPs, firewall details, logs, caption-asset paths, audio device names, or any diagnostics (enforced by route separation and bind-address separation, Section 9/10).
- **Show-end behavior:** streams terminate and the session expires at show end; the page shows a polite "this show has ended" state rather than dangling.

### 7.3 Native app?

Not for MVP. Browser delivery is the target. Only revisit a native app if browser limitations (autoplay friction, background audio, wake-lock, jitter control) prove insufficient in field testing — and record that decision against real evidence, not speculation.

---

## 8. Admin UI and first-boot setup wizard

The admin interface is the difference between "a clever prototype" and "a thing a non-specialist can install and a booth tech can trust before a show."

### 8.1 First-boot / installer workflow

1. Bring up a temporary **"CaptionBridge Setup"** network and serve a local setup URL.
2. **Force admin credential creation** — no default credentials may remain active past setup.
3. Identify **theater-side** vs **client-side** interfaces (with a physical-port confirmation step to prevent miswiring).
4. Configure the theater-side interface (static IP or DHCP).
5. Collect **cinema server / media-block IP** and server family (if known) → this becomes the firewall allowlist.
6. Choose a **firewall profile:** Lab / Booth Diagnostic / Patron-Facing / Low-Cost Replacement.
7. Optionally enable hosted patron Wi-Fi (dedicated AP recommended) or attach to an existing client/admin VLAN.
8. Configure ST 430-10/-11 target behavior (endpoint registration per server family — **OPEN**).
9. Configure audio device(s), channel mapping, and HI/VI-N stream labels.
10. Run the **audio calibration wizard** and save the auditorium input profile.
11. **Apply the firewall profile and run network/security verification** (Section 9.3).
12. Run the **caption readiness check**: ACSP handshake, lease, timeline, RPL receipt, timed-text fetch, parser sanity, and patron caption-page reachability.
13. Run the **audio readiness check**: input health, encode, browser playback, and a measured latency estimate.
14. **Exit setup mode automatically** on completion or timeout (no lingering open setup network).

### 8.2 Admin dashboard

A single status surface that lets staff answer "is this room ready for the next show?" at a glance — with captions and audio reported **separately**:

```text
Firewall:                 Active
Forwarding:               Disabled
Theater interface:        eth0
Client interface:         wlan0
Allowed cinema server:    10.10.20.15
Server connected:         yes/no
Lease active:             yes/no
Timeline:                 playing / paused / stopped
RPL loaded:               yes/no
Caption assets loaded:    yes/no
Current language:         en-US
Current cue:              visible / none
HI input present:         yes/no
VI input present:         yes/no
Audio input quality:      usable / marginal / failed
HI stream active:         yes/no
VI stream active:         yes/no
Connected caption clients: 4
Connected audio clients:   2
Estimated audio latency:  120 ms
SSH:                      Disabled
Discovery mode:           Off
Support session:          Off
```

### 8.3 Loud warnings (the dashboard must shout, not whisper)

Surface a prominent alert if **any** of these are true: IP forwarding enabled; interfaces bridged; firewall inactive; admin UI reachable on the public patron network; default password still set; SSH enabled outside setup/support; patron Wi-Fi and theater network sharing one unsegmented interface; DNS/internet routing leaking across interfaces; HI or VI input silent during playback; audio clipping; a configured stereo mapping impossible on a detected mono input; audio latency over threshold; the connected cinema server does not match the auditorium's pinned expected identity (Section 8.5); or debug capture / support mode active.

### 8.4 Public vs admin separation

Patron and admin surfaces are separated in **routes, permissions, and ideally bind address/interface**:

```text
/caption            public or session-coded; captions only; no diagnostics
/listen             public or session-coded; HI/VI-N selection; no diagnostics
/accessibility      public or session-coded; combined
/admin              password/passkey protected; setup, status, logs, firewall,
                    calibration, diagnostics, updates
/api/caption-stream read-only caption stream
/api/audio/*        authorized local audio streams only
/api/admin/*        authenticated only
```

**Binding `/admin` and `/api/admin/*` away from the patron network is mandatory in the Patron-Facing and Low-Cost Replacement profiles**, not merely "where the hardware allows":

- `/admin` and `/api/admin/*` **must not bind on the patron SSID/interface** in production profiles; they live on a separate wired admin port, a dedicated setup AP, or a time-limited setup mode.
- If the hardware has **only two interfaces**, then **setup/admin mode and patron-serving mode are mutually exclusive** — the appliance is either being administered or serving patrons, never exposing admin on the live patron network.
- Admin reachable over the patron network exists **only** in the Lab and Booth Diagnostic profiles, and only behind a prominent warning banner.
- This is enforced as a hard check in install/runtime verification (Section 9.3): in production profiles, an admin surface reachable from the patron side **refuses to leave setup**. The patron network is untrusted BYOD; authentication alone is not the boundary.


### 8.5 Auditorium and show-identity safeguards (wrong-room protection)

A real multiplex failure is the appliance wired or configured to the **wrong server/auditorium**, or a **stale QR code** that points patrons at the wrong room. Cheap safeguards:

- **Pin the expected server identity** per auditorium profile (expected IP, ACSP server fingerprint, and screen name — Section 10.5); warn loudly if the connected server does not match.
- **Tie the auditorium profile to the physical port label** so a re-cabling that swaps rooms is caught at setup, not mid-show.
- **Per-show QR codes expire and include the auditorium identifier**, so yesterday's code cannot silently admit patrons to today's wrong room.
- The **patron page displays the auditorium it is serving** (Section 7.2) so patrons and staff can spot a wrong-room join immediately.
- When later integrated with a **TMS**, raise an admin warning if the received RPL/CPL/show metadata does not match the auditorium's expected schedule. (Surfaced in the loud-warning set, Section 8.3.)

Even with no TMS integration, "you are connected to Auditorium X" plus a pinned expected server identity catches the common mistakes.


## 9. Network and security model

CaptionBridge may sit on a booth network alongside cinema servers, projectors/media blocks, TMS, automation, NAS, and business systems. It is designed as a **locked-down booth device** whose security posture is configured *and continuously verified*, not set once and forgotten.

### 9.1 Interface isolation and firewall

- **Two zones, never joined:** `eth0` = theater side (cinema server / IMB / accessibility analog source); `wlan0`/`eth1` = client side (patron display devices, admin/setup). The application reads the theater feed and serves the patron streams; **it does not forward packets between zones.**
- **Default-deny firewall, nftables.** No `forward`-chain accepts at all. Kernel forwarding disabled for IPv4 **and** IPv6. No bridge devices.
- **Theater-side ingress/egress** limited to the configured cinema server/media-block IP(s) and only the required ACSP port(s)/protocol(s) (confirm per server family — **OPEN**). Nothing else on the theater side is reachable or initiated.
- **Client-side** limited to the caption UI, audio stream, WebSocket/SSE caption stream, and — depending on profile — the setup/admin UI.
- **SSH disabled by default** after first setup. Support mode (Section 9.4) is the only way back in, and it is explicit, outbound-only, time-limited, and audited.

The real ruleset is profile-driven and generated by the firewall manager. **The theater-side rule depends on which side initiates the ACSP TCP connection — and that must be the confirmed direction from Phase 0A, because the wrong rule is either useless or a hole.** Two variants; ship only the one matching the confirmed transport profile.

Common to both (the parts that never change):

```text
table inet captionbridge {
  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    iif "lo" accept
    iif "wlan0" tcp dport { 80, 443 } accept     # patron web UI
    iif "wlan0" udp dport $WEBRTC_RANGE accept    # WebRTC media
    # theater-side ACSP rule: see variant below
    # everything else dropped
  }
  chain forward {
    type filter hook forward priority 0; policy drop;  # NEVER accept
  }
  chain output {
    type filter hook output priority 0; policy drop;
    ct state established,related accept
    oif "lo" accept
    # theater-side ACSP rule: see variant below
    # optional, support mode only: time-limited outbound to update/support host
  }
}
```

**Variant A — ACS-initiated** (CaptionBridge opens the TCP connection; current working assumption):

```text
# output: allow the outbound connection to the server's ACSP port
oif "eth0" ip daddr $CINEMA_SERVER tcp dport $ACSP_PORT accept
# input: nothing explicit needed — return traffic is admitted by the
#        `ct state established,related accept` rule above.
```

**Variant B — server-initiated** (the media block opens the connection to CaptionBridge):

```text
# input: allow the server to reach CaptionBridge's local ACSP listener port
iif "eth0" ip saddr $CINEMA_SERVER tcp dport $LOCAL_ACSP_PORT accept
# output: nothing explicit needed — replies are established,related.
```

Why this matters: in Variant A an explicit inbound `tcp sport $ACSP_PORT accept` rule is **redundant** (connection tracking already admits the replies) and only invites confusion; in Variant B such a rule is **required**, but it must match the *destination* port (CaptionBridge's listener), not the source port. Pinning the direction in Phase 0A removes the guesswork.

### 9.2 Firewall / install profiles

| Profile | Theater side | Patron side | Admin UI | SSH | Notes |
|---|---|---|---|---|---|
| **Lab** | broader discovery allowed | local | open on setup net | local SSH ok | Warning banner; **never** for public installs; logs/pcaps allowed. |
| **Booth Diagnostic** | allowlisted server IPs only | caption/audio on local Wi-Fi | configured admin subnet/setup net | disabled by default | For booth techs/festival staff. |
| **Patron-Facing** | allowlisted server/IMB IPs only | caption/audio display only | **hidden from public SSID** | disabled | Session code / signed show token required; caches + sessions purged at show end. Most locked down. |
| **Low-Cost Replacement** | ST 430-10/-11 over Ethernet, allowlisted | local Wi-Fi web app | admin separated | disabled | Non-technical installer; calibration wizard + signal health required; per-show token; no internet rebroadcast; clear staff/patron error states. |

### 9.3 Install-time and runtime verification

Security is only real if it is checked. The firewall manager runs a **verification pass** at install and periodically at runtime, asserting and surfacing:

- forwarding disabled (v4 + v6); no bridge devices present;
- default-deny active with no forward-accepts;
- theater-side reachable scope == allowlist only — verified primarily by **introspecting CaptionBridge's own routes and firewall permissions** (the appliance has no configured route or rule beyond the allowlist), *not* by scanning the theater network. Any active reachability check is **limited to the configured server, rate-limited, profile-controlled, and never a broad subnet scan** in production profiles — the appliance must never behave like a network scanner on a cinema LAN;
- no DNS/route leak across interfaces (the patron side cannot reach the theater side or the internet through the appliance);
- admin surface not bound/exposed on the patron network;
- no default credentials; SSH state correct for the profile.

Any failure flips a loud dashboard warning (Section 8.3) and, for the locked profiles, can refuse to leave setup.

### 9.4 Content handling, logging, support mode, updates

**No-persistence by default (guardrail #5), with crash semantics.** "No persistence" is only real if the failure and crash paths honor it too:
- No persistent caption-text storage; cues live in memory and purge at show end. Transient assets (fetched timed-text, working buffers) live in **memory or `tmpfs`**, never on the persistent filesystem.
- No audio recording; audio is transient.
- **Core dumps are disabled (or scrubbed) in production profiles** — a crash dump is the classic way caption text or audio leaks to disk unnoticed.
- **Logs never contain caption text or audio samples by default**, and must not record WebRTC SDP bodies or client identifiers into detailed persistent logs.
- The SQLite config store must not accidentally hold **current cue text, asset paths that embed title names, or show identifiers beyond what configuration genuinely needs**.
- Debug caption logging and debug audio capture are each **explicit, admin-only, time-limited, size-limited (audio), and visibly indicated** on the dashboard while active, write only to a **bounded `tmpfs` or an encrypted support bundle**, and **auto-delete on a timer and on reboot**.

**Structured observability (states and counts, never content).** Operators need rich diagnostics without content logging. Emit structured events keyed on state transitions, counts, durations, error classes, and opaque/hashed IDs — never caption text or audio samples:

```text
caption.acsp.connected            audio.input.connected
caption.lease.granted             audio.level.clipping
caption.lease.expiring            audio.silence.sustained
caption.timeline.anchor_updated   webrtc.peer.connected
caption.rpl.received              webrtc.peer.reconnect
caption.asset.fetch_failed        security.forwarding_enabled_detected
caption.cue.visible_no_text       firewall.profile.applied
```

(`caption.cue.visible_no_text` records that a cue was shown and for how long, not its words — enough to debug a "captions look wrong" report without storing content.)

**Streams are local-only.** No internet rebroadcast, ever. Per-show **access code or signed QR token** for patron access; sessions auto-expire at show end. (CaptionBridge may be retransmitting copyrighted accessibility assets locally; treat both captions and audio as sensitive content that never leaves the venue LAN.)

**Support mode.** Off by default. When enabled by an admin it is **outbound-only, time-limited, and audited**, and visibly indicated. It is the only re-entry path once SSH is disabled.

**Signed / offline updates.** The primary update path is **signed and offline-capable** (e.g., a signed bundle applied from local media), verified against a key held in the TPM/secure storage on the hardened tier. Cloud update is **optional and outbound-only**. The hardened tier uses **A/B partitions for atomic updates with automatic rollback** on failed boot/health-check, so a bad update never bricks a booth appliance.

### 9.5 Patron Wi-Fi minimum requirements

"Dedicated external AP" is the right recommendation, but for a full auditorium it has to become a **spec**, not a preference. For Patron-Facing and Low-Cost Replacement deployments:

- **Band/standard:** AP supports **5 GHz**, preferably **Wi-Fi 6 or better**; 2.4 GHz alone is not acceptable for a full room.
- **Band steering** disabled or explicitly controlled (uncontrolled steering breaks sticky low-latency audio sessions).
- **Client isolation** must be evaluated carefully: patron phones **must be able to reach the appliance**, so naive "wireless client isolation" will break the product. If isolation is required by venue policy, the appliance must remain reachable and **mDNS/multicast must still resolve** for WebRTC candidate handling.
- **DHCP scope** sized to expected auditorium capacity (plus headroom); **captive portal disabled** unless intentionally implemented.
- **QR onboarding** carries SSID + password + URL where the platform allows, so a patron joins the network and the page in one scan.
- **Channel planning** for multiplexes with many adjacent rooms (co-channel contention across screens is a real failure mode).
- **Target concurrent clients by profile**, validated by the Wi-Fi soak test (Section 11.6): e.g., **10 / 25 / 50 / 100** for Lab / Booth Diagnostic / Patron-Facing / Low-Cost Replacement respectively (tune to real venue sizes).

**RF-reality failure mode to test for:** some booths/auditoriums are RF-hostile, and an AP placed in the booth may pass bench testing yet fail in the seats. AP **placement for seat-level coverage** is part of the install, and the latency/soak tests (Sections 11.4, 11.6) must be run **from representative seats**, not next to the rack.


## 10. Data models and internal state models

Only **configuration and profiles** are persisted (SQLite); **caption and audio content are never persisted**. The models below are the in-memory and on-disk shapes the subsystems share.

### 10.1 Caption cue model (in-memory, transient)

```json
{
  "resource_id": "uuid",
  "language": "en-US",
  "start_ms": 123456,
  "end_ms": 127890,
  "text": "Caption text here",
  "style": {},        // pass-through, not rendered in early phases
  "position": {}      // pass-through, not rendered in early phases
}
```

### 10.2 Audio input/stream model (in-memory, transient)

```json
{
  "input_device_id": "alsa_or_pipewire_device_id",
  "stream_type": "HI",            // HI | VI-N
  "source_channel": "left",       // left | right | summed | stereo
  "sample_rate": 48000,
  "channels": 1,
  "level_dbfs": -18.2,
  "noise_floor_dbfs": -62.0,
  "clipping": false,
  "silence": false,
  "status": "usable",             // excellent | usable | marginal | failed
  "latency_ms_estimate": 120
}
```

Audio stream status is **independent** of caption status — the system can have valid captions and a failed VI feed, or clean HI audio and no captions.

### 10.3 Caption-link state (ACSP)

```json
{
  "state": "PLAYING",             // DISCONNECTED..READY..PLAYING..ERROR (Section 5.2)
  "server": "10.10.20.15",
  "lease_active": true,
  "lease_expires_in_ms": 14000,
  "request_interval_ms": 15000,   // == half lease
  "timeline_position_ms": 542300,
  "output_mode": "enabled",       // enabled | disabled | unknown
  "current_playout_id": "…",
  "rpl_loaded": true,
  "assets_loaded": true,
  "current_language": "en-US",
  "available_languages": ["en-US","es-419"],
  "last_timeline_anchor_monotonic_ns": 123456789
}
```

### 10.4 Patron session / access token

```json
{
  "session_id": "uuid",
  "show_id": "opaque-per-show",
  "token": "signed-or-coded",
  "granted_modes": ["caption","HI","VI-N"],
  "issued_at": "…",
  "expires_at": "show-end",       // auto-expire; no carry-over between shows
  "client_count_caption": 4,
  "client_count_audio": 2
}
```

### 10.5 Auditorium input profile (persisted config)

```json
{
  "auditorium_id": "screen-3",
  "input_device_id": "usb-XYZ-interface-00",
  "channel_mapping": "stereo:L=HI,R=VI-N",
  "gain_notes": "attenuator pad inline; isolator fitted",
  "last_qualification": "usable",
  "cinema_server_ip": "10.10.20.15",
  "server_family": "vendor/model",
  "acsp_port": "…",               // confirmed per server family
  "expected_screen_name": "Auditorium 3",       // for wrong-room detection
  "expected_server_fingerprint": "opaque-id",    // see note on what this is
  "firewall_profile": "low_cost_replacement"
}
```

> **What `expected_server_fingerprint` is** depends on what the confirmed server-family profile actually exposes (a Phase 0A finding), in rough order of strength: a TLS certificate fingerprint if the ACSP transport uses TLS; an ACSP-exposed server identifier or media-block serial if available; otherwise a **weaker composite identity** (configured IP + server family + screen name). It is a wrong-room and misconfiguration guard, **not** necessarily a cryptographic authentication of the server — when only a composite identity is available, label it as such in the UI so it is not mistaken for a security property the protocol does not provide.

### 10.6 System / readiness state (what the dashboard renders)

A composite of caption-link state (10.3), per-input audio state (10.2), firewall/verification state (Section 9.3), and network/interface assignment — reduced to two top-line verdicts the dashboard exposes: **caption_ready** and **audio_ready**, plus the loud-warning set (Section 8.3).

### 10.7 Threat model summary

- **Primary asset to protect:** the theater network. The patron network must never reach cinema servers, projectors, TMS, automation, NAS, or business systems through the appliance. Enforced by zero forwarding + allowlist + runtime verification.
- **Content asset:** captions and HI/VI-N audio are potentially copyrighted; keep them on the venue LAN, transient, token-gated, and never rebroadcast.
- **Appliance integrity:** no default creds, SSH off by default, signed updates, secure boot/TPM on the hardened tier, support mode outbound-only/audited.
- **Patron privacy:** no personal data collected; tokens are per-show and expire; no tracking; no internet calls on the patron path.

### 10.8 System config and versioning (persisted)

A production appliance has to answer "what changed since this last worked?" Persist a small versioned config/state record (SQLite, configuration only):

```json
{
  "config_schema_version": 3,
  "hardware_profile_version": "lowcost-v2",
  "firewall_profile_version": "patron-facing-v4",
  "server_family_compat_profile": "vendorX/modelY@rev",
  "audio_device_compat_profile": "usb-interface-C@rev",
  "last_readiness_check_ok": "2026-06-27T18:40:00Z",
  "last_readiness_check_failure": {
    "at": "2026-06-20T01:05:00Z",
    "reason": "HI input silent during playback"
  },
  "update_channel": "stable"      // stable | beta ring, for staged rollout
}
```

These fields make field support tractable: a tech can see the schema, hardware, firewall, and per-family/per-device compatibility revisions in play, plus the last good and last failed readiness checks, before changing anything.


## 11. Test strategy

Because this may be used in lieu of a professional system, testing is weighted toward reliability, real-hardware variability, and field readiness — not just unit correctness.

### 11.1 ST 430-10 simulator (build this early — Phase 3)

A fake cinema-server process that emits **realistic** ACSP behavior so the caption path can be developed and regression-tested without a real server (which is scarce and expensive lab time). It must be able to reproduce:

- the new-connection sequence and the new-show-load sequence (Section 5.2);
- timeline updates at a configurable cadence; play / pause / resume / stop / seek;
- lease grant/renewal at configurable durations, plus **near-expiry and actual-expiry** cases;
- **communications loss** (drop the link mid-show, then reconnect before/after lease expiry) to prove "continue output until lease expires";
- **show change / rewind / abandoned RPL**, including the **Playout ID = 0 / duplicate Playout ID** quirk;
- `Set Output Mode` enabled/disabled toggling, including servers that omit it;
- empty `ReelResources`; multiple languages; malformed/unfetchable assets.

The simulator is also the backbone of CI: every caption-path change runs the full scenario suite headless.

### 11.2 Lab server testing (Phase 4 — caption MVP gate)

Against at least one **real** cinema server/IMB, verify the full chain end to end: endpoint registration, handshake, lease, timeline updates, RPL delivery, timed-text fetch, parsing, and correct active-cue behavior across every playback state change. Expand server coverage over time toward the likely field set — **Dolby/Doremi, GDC, Qube, Barco/Alchemy, Christie, Sony** — capturing per-family quirks (discovery/registration, lease behavior, timeline cadence, output-mode habits) into a compatibility log. **Server interoperability is the single biggest external unknown;** budget real lab time generously.

### 11.3 Audio hardware compatibility matrix (starts Phase 0C, lives forever)

Maintain a living matrix of input device × adapter/cabling × outcome, e.g.:

| Input device | Tier | Cabling | Mono/stereo | Result | Notes |
|---|---|---|---|---|---|
| Mini-PC mic jack (model A) | 0 | RCA→3.5 direct | mono | Marginal | mic-level; needs pad; AGC suspected |
| Mini-PC mic jack (model A) | 0 | RCA→3.5 + pad + isolator | mono | Usable | hum gone with isolator |
| USB dongle (model B) | 1 | RCA→3.5 + pad | mono | Usable | stable; low headroom |
| USB interface (model C) | 2 | line in | stereo | Excellent | clean; real gain control |

Phase 0C deliverable is the **known-good minimum hardware list + documented failure modes** that feeds the shipped kit (Section 6.2). Re-run when shipped hardware changes.

### 11.4 Latency testing (the HI risk)

A repeatable method to measure **glass-to-ear** latency, not just encoder latency:

- Inject a known marker (click/tone) into the analog input and capture the phone's headphone output (loopback mic or splitter) to measure true end-to-end delay across representative phones and Wi-Fi conditions.
- Record the distribution (not just best case) under good and degraded RF, with N phones contending.
- Validate against the budget (Section 6.7): HI < ~100–150 ms target, VI-N < ~250 ms. **Surface the realistic achievable HI latency honestly**; if a device fleet can't hit it, that is a venue decision, documented — not a number quietly fudged.
- Add a continuous in-product latency estimate for the dashboard, calibrated against the loopback ground truth.

### 11.5 Field readiness checks / pre-show QA checklist

The install/admin readiness checks (Sections 8.1, 9.3) double as the field QA gate. Codify a **staff pre-show checklist** with explicit pass/fail, e.g.: firewall active + forwarding disabled; server connected + lease active; RPL + caption assets loaded; correct language available; HI present + qualified; VI-N present + qualified; latency within threshold; patron page reachable on patron Wi-Fi; admin hidden from patron SSID. Crucially, define **what staff do when a check fails** before a show (Section 14 / Phase 9) — readiness checks are only useful if they're tied to a procedure.

### 11.6 Other test layers

- **Unit/integration:** parsers (**ST 428-7 + the ST 428-10 closed-caption profile + Interop** timed text; ST 429-12 track-type handling), timeline math (edit-rate/timecode → ms, drift under re-anchoring), channel mapper, qualifier classification thresholds, **resource-resolver locator forms**.
- **Soak/reliability:** multi-hour runs with show changes, reconnects, and device hot-plug; memory/leak watch (content must purge at show end).
- **Security tests:** automated assertions that no forwarding/bridge exists, patron→theater is unreachable, admin not exposed on patron net, no default creds, SSH state correct — run in CI against a network test harness and at runtime on the appliance.
- **Soak the Wi-Fi:** many simultaneous patron clients on the dedicated AP; verify caption fan-out and N WebRTC subscribers hold up.


## 12. Deployment profiles

Four shipping configurations plus an optional **Loaner Device** overlay (12.5). They share one codebase; they differ in firewall posture, which surfaces are exposed, access control, and operational assumptions. (Firewall specifics in Section 9.2.) **The first externally useful product is likely Booth Diagnostic (12.2), not a patron-facing or replacement profile** — it creates real value and collects server-family, audio-output, and venue-network compatibility data without making any access-equivalence claim.

### 12.1 Lab

Development and bench only. Broader discovery, local SSH, full logs and packet captures permitted. Prominent warning banner. **Never** used for public installs. This is the profile Phases 0–6 run in.

### 12.2 Booth Diagnostic

For booth techs, festival staff, and Simple-DCP-style verification. Theater side restricted to allowlisted server IPs; admin UI available to the configured admin subnet/setup network; caption/audio display on local Wi-Fi; no forwarding; SSH disabled by default. Purpose: confirm whether ST 430-10/-11 is active, whether RPL/timed-text is available, show live captions, show HI/VI input levels and stream health, and help localize a fault to server / wiring / transmitter / receiver. The primary value here is **diagnosis** — telling a tech *where* a failure is. Because it makes no access-equivalence claim, this is the **lowest-risk first shippable product**: it earns its keep in the field — proving ST 430-10/-11 interop across server families, surfacing audio-output realities, and exercising the network posture — while the patron/replacement claims are still being earned through the later gates.

### 12.3 Patron-Facing

For temporary/festival/pop-up accessibility use. Most locked down: theater side restricted to server/IMB IPs; patrons reach caption/audio display only; admin hidden from the public SSID; per-show session code or signed token required; caption cache and audio sessions purged at show end. Patrons pick captions / HI / VI-N / captions+audio; streams terminate at show end.

### 12.4 Low-Cost Accessibility Replacement

For venues running CaptionBridge **in lieu of** a professional receiver system, often with only basic analog outputs and a non-specialist installer. Adds operational requirements on top of Patron-Facing: ST 430-10/-11 captions over Ethernet; 3.5 mm/USB audio for HI/VI-N; local Wi-Fi web app; **mandatory audio calibration wizard**; **mandatory signal-health monitoring**; per-show access code/token; no internet rebroadcast; no recording by default; and **clear staff/patron error states**. This profile may only ship to a venue **after Phase 9** operational/legal review (Section 1.3).

### 12.5 Loaner Device Mode (overlay on Patron-Facing or Low-Cost Replacement)

Not a separate firewall profile but a **device-provisioning posture** the venue can layer on top of Patron-Facing or Low-Cost Replacement: the venue supplies the endpoints rather than relying on patron BYOD. Because the venue controls the device, this overlay dissolves several of the hardest BYOD problems at once and supports a **stronger access claim**:

- a **pre-trusted HTTPS/cert profile** (install a local CA on the fleet), which sidesteps the self-signed-cert warning dilemma (6.7) entirely for these devices;
- a **controlled browser/device fleet** with known WebRTC/autoplay/Wake-Lock behavior;
- **supplied wired headphones**, removing Bluetooth latency and the dead-battery / no-headphones failure modes (1.3, 6.7);
- **kiosk / guided-access / MDM lock** and preloaded per-room shortcuts, so a patron is handed a device already on the correct auditorium.

This is the natural bridge between BYOD diagnostic/festival use and a defensible "replacement" posture, and it directly serves the access-equivalence requirement (1.3): venue-owned loaner devices are one of the fallback mechanisms a venue needs for patrons without a usable phone. It does not remove the Phase 9 review, but it materially de-risks it.

---

## 13. Phased implementation plan (with MVP boundaries)

The plan extends the brief's phases and adds explicit **gates**. Each phase lists its goal, scope, key deliverables, and the **exit criteria** that must pass before the next phase starts. The load-bearing MVP boundaries are **G1 Caption MVP**, then the audio MVP split into **G2a VI-N audio** and **G2b HI assistive-listening** (separated because HI is far harder — see Phase 5), **G3 Combined Patron Pilot**, **G4 Production/Replacement** after hardening, and **G5 Field/Compliance** before any "in lieu of professional" claim. The early de-risking work is split into **Phase 0A (ACSP wire), 0B (browser), and 0C (audio/latency)** so the simulator, firewall, and patron stack are built around *confirmed* behavior rather than assumptions.

> Sequencing rationale: prove the **caption path and network isolation** first (highest-value, most-deterministic, and the security spine), then add audio (highest hardware risk), then integrate, then harden, then validate in the field. Captions and isolation carry the early value; audio carries the early *risk*. Three things are pulled to the very front as Phase 0 spikes because getting them wrong invalidates later work: the **ACSP wire contract** (so the simulator models the right side of the wire), the **browser local-appliance reality** (HTTPS/secure-context/mDNS/autoplay/Wake Lock — these gate the *primary audio path*, not just UX), and the **audio-hardware + HI-latency** reality.

### Phase 0A — ACSP wire discovery

- **Goal:** pin the one assumption everything else is built on — how the caption protocol actually behaves on a real server — *before* writing the simulator, firewall rules, or fetcher.
- **Scope:** one session against a **real cinema server/IMB** of the chosen first target family, capturing a **pcap** and answering: does CaptionBridge connect to the server or vice versa; the ACSP port; whether the appliance IP must be registered in the server UI; request framing; the RPL location/locator form; how timed-text assets are actually fetched (server-relative / http(s) / file-like / vendor-specific); and whether the **theater-side firewall needs inbound rules**.
- **Deliverables:** **ACSP Wire Contract v0** — first target server family; connection direction; port; registration model; sample packet capture; sample RPL; sample timed-text asset plus its fetch mechanism; whether inbound theater-side firewall rules are needed. (This does not require a finished parser — one real server session and a capture.)
- **Exit:** the Wire Contract is documented, and the **firewall variant (Section 9.1) and simulator (Phase 3) are committed to the confirmed direction**, not a guess.

### Phase 0B — Browser local-appliance spike

- **Goal:** find out early whether the **primary audio path connects reliably** across real phones — because HTTPS/secure-context, mDNS, and autoplay are existential for WebRTC, not Phase-7 polish.
- **Scope:** test **iOS Safari, Android Chrome, and desktop Chrome** against the appliance for: HTTP vs HTTPS behavior; self-signed-cert warnings; feasibility of a **locally-trusted cert / local-DNS** approach; captive-portal behavior; **QR onboarding**; **autoplay** gesture requirements; **Wake Lock** availability; and **WebRTC host ICE / mDNS `*.local` candidate** handling for a same-LAN connection.
- **Deliverables:** a decision matrix of viable secure-context strategies (HTTP-only with tap-to-start; real cert + local DNS; installed local CA for chain-owned devices) with their trade-offs, and a recommended default to **lock by Phase 7**.
- **Exit:** WebRTC audio demonstrably plays in iOS Safari and Android Chrome against the appliance under at least one documented configuration; the secure-context/onboarding options are characterized rather than discovered late.

### Phase 0C — Audio hardware & latency spike

- **Goal:** de-risk the worst hardware unknown (consumer analog input) before writing much media software.
- **Scope:** test mini-PC built-in mic jacks, cheap USB dongles, USB interfaces, TRS/TRRS adapters, RCA→3.5 wiring, inline attenuation pads, ground-loop isolators; mono-vs-stereo detection; common booth output levels; Linux audio-capture stability on both Pi 5 and x86. A **first crude glass-to-ear latency probe** to sanity-check the WebRTC/Opus HI target — **including a wired-vs-Bluetooth comparison** — before committing to it.
- **Deliverables:** known-good minimum hardware list + documented failure modes (→ shipped kit); the seed of the audio compatibility matrix (11.3); an early read on achievable HI latency, wired and Bluetooth.
- **Exit:** at least one Tier-1 and one Tier-2 input path reliably qualified "Usable/Excellent" on both compute targets; failure modes catalogued; a defensible (even if uncomfortable) latency expectation recorded.

### Phase 1 — Offline web caption player

- **Goal:** prove cue rendering and phone UX with no server in the loop.
- **Scope:** manually load timed-text XML, parse cues, serve a phone-friendly caption page, play cues against a **fake show clock** using the monotonic timing engine (not sleep loops).
- **Deliverables:** timed-text parser v0 (**ST 428-7 + ST 428-10 closed-caption profile + Interop**, text/timing only), timing engine v0, caption fan-out (WS/SSE), patron caption page v0 (large text, contrast, font size, connection state).
- **Exit:** cues appear/clear correctly against the fake clock through play/pause/seek; phone page is legible and stable in a dark room.

### Phase 2 — RPL + timed-text ingest

- **Goal:** drive captions from real ST 430-11 structures.
- **Scope:** load ST 430-11 RPL files, resolve referenced timed-text assets, normalize cues, select language, display the current caption; handle empty `ReelResources` and multi-language.
- **Deliverables:** RPL parser v1; asset resolver/fetcher; language selection plumbed through fan-out and UI.
- **Exit:** given representative RPL + assets, the correct cue in the selected language renders against the timeline model; quirks (empty resources, multiple languages) handled.

### Phase 3 — ST 430-10 simulator (built from the observed wire contract)

- **Goal:** make the caption path testable and CI-able without scarce real-server time — modeling the **confirmed** wire behavior from Phase 0A, not assumptions, so the simulator cannot simulate the wrong side of the wire.
- **Scope:** build the simulator (11.1) — seeded by the Phase 0A pcap and Wire Contract — covering connection/new-show sequences (in the confirmed initiation direction), timeline, lease (incl. near-/actual-expiry), comms loss + reconnect, show change/rewind/abandoned RPL, Playout-ID quirks, output-mode toggling, the observed resource-locator forms, and malformed assets.
- **Deliverables:** simulator + a headless scenario suite wired into CI; ACSP state machine v1 driven entirely by the simulator.
- **Exit:** the full scenario suite passes headless; "continue output until lease expiry on comms loss" and "no false loop-back on duplicate/zero Playout IDs" are explicitly demonstrated; the simulator's behavior matches the Phase 0A capture.

### Phase 4 — Real cinema-server lab test  ⟶ **GATE G1: Caption MVP**

- **Goal:** prove the caption path against real hardware.
- **Scope:** connect to a lab cinema server/IMB; verify endpoint registration, handshake, lease, timeline, RPL delivery, timed-text fetch, parsing, and behavior across all playback state changes; begin the server-family compatibility log.
- **Deliverables:** validated end-to-end caption path on ≥1 real server; documented registration/quirk notes for that family; **the minimum caption + security MVP** (local caption web page over Wi-Fi; admin dashboard showing connection/lease/RPL/caption-asset/firewall status; theater-vs-client interface assignment; **default-deny firewall with no forwarding**; setup mode with auto-expiry; configured server allowlist; basic diagnostics/readiness checks; signed/offline update path).
- **G1 exit criteria:** captions are correct and stable on a real server across play/pause/resume/stop/seek/reconnect/show-change; firewall verification (Section 9.3) passes; setup mode auto-expires; signed update applies. **No audio required to pass G1.**

### Phase 5 — Basic audio MVP  ⟶ **GATES G2a (VI-N) and G2b (HI)**

The audio MVP is split into two gates because the two streams have **fundamentally different difficulty**: a VI-N (narration) listener has no competing acoustic source, so the looser latency budget is comfortably achievable; an HI listener may still be hearing the auditorium directly, so a late phone stream produces a disturbing slap-back echo. VI-N is likely achievable; **HI must be experimentally earned, not assumed.**

- **Goal:** prove one analog input → one browser audio stream, and characterize HI latency honestly against a real phone fleet.
- **Scope:** capture a single analog input; run the calibration wizard; encode Opus; deliver via WebRTC to a phone browser; level meter, clipping detection, silence detection; simple staff/patron error messages. **Run the real glass-to-ear latency test (11.4) here**, across **iOS Safari and Android Chrome and both wired and Bluetooth** headphones, and confront the HI target.
- **Deliverables:** capture + qualifier v1; calibration wizard v1; GStreamer `webrtcbin` audio path (as the `captionbridge-media` process, Section 3.5); **minimum audio MVP** (one consumer analog input path; calibration wizard; level meter; clipping + silence + mono/stereo detection; one WebRTC/Opus stream; patron error messages; no recording; local-only access).
- **G2a (VI-N) exit criteria:** a non-specialist can calibrate a Tier-1/Tier-2 input to "Usable" via the wizard; one **VI-N** stream plays stably in iOS Safari and Android Chrome; **measured VI-N latency is within budget**; nothing is recorded.
- **G2b (HI) exit criteria:** **measured glass-to-ear HI latency is acceptable while auditorium sound is present**, validated (not assumed) across the test phone fleet with **wired** headphones (Bluetooth characterized separately); and a **documented failure threshold** is defined above which a venue should **not** offer HI through CaptionBridge. G2b may be met as "achieved on this fleet" or explicitly recorded as "device/Wi-Fi-limited — HI offered only under stated conditions"; either way the claim is evidence-based.

### Phase 6 — HI/VI channel mapping

- **Goal:** support real HI+VI-N configurations and multi-device inputs.
- **Scope:** mono/stereo mapping, left/right split (both directions), summed-mono, stereo-as-one, multiple USB input devices; named HI/VI-N streams; per-stream health checks; admin diagnostics for mapping impossibility (stereo-on-mono → Failed before show).
- **Deliverables:** channel mapper v1; dual-stream encode/deliver; admin mapping UI + health tiles.
- **Exit:** HI and VI-N deliver simultaneously and independently under each supported mapping; impossible mappings are caught and surfaced pre-show, not mid-show.

### Phase 7 — Integrated patron accessibility UI  ⟶ **GATE G3: Combined Patron Pilot**

- **Goal:** one unified patron experience for captions + selectable HI/VI-N.
- **Scope:** combine captions and selectable audio in `/accessibility`; per-show session access (code/signed token); QR onboarding; reconnect behavior; show-end expiration; **lock the HTTPS/secure-context + mDNS onboarding decision** (Section 6.7) for the patron fleet, choosing from the strategies characterized in the Phase 0B spike; and handle the **accessibility-critical caption rendering subset** needed for real patrons — readable multi-line cues, speaker labels, and sound-effect/music notation (the full fidelity pass is Phase 8).
- **Deliverables:** unified patron app; session/token model (10.4); QR provisioning; the **combined MVP target** (captions + one selected audio feed served locally to a smartphone; admin shows caption readiness and audio readiness **separately**; staff can tell whether the system is ready for a show). Also a **staff runbook v0** (pre-show check; how to issue a QR/access code; how to provide a loaner phone/headphones; what to do if captions, HI, or VI-N fail; how to fall back to a professional receiver; what to tell patrons without over-explaining; incident logging) — drafted here so Phase 9 *validates* a procedure rather than inventing one.
- **G3 exit criteria:** patrons join by QR with no app, select captions/HI/VI-N/combined, survive reconnects, and are cleanly expired at show end; admin shows independent caption/audio readiness; the secure-context approach is decided and consistent.

### Phase 8 — Hardened replacement appliance  ⟶ **GATE G4: Production / Replacement-ready (technical)**

- **Goal:** make it a deployable, trustworthy appliance.
- **Scope:** watchdog; signed offline updates with A/B + rollback; installer workflow; read-only-capable root filesystem; encrypted config; secure boot/TPM where available; production firewall profiles with runtime verification; auditorium profiles; full pre-show QA checklist; field diagnostics; support mode (outbound-only, time-limited, audited); robust failure messaging. **Caption rendering fidelity pass:** validate the deferred source styling/positioning decisions, multi-line behavior, italics/color, non-Latin scripts, RTL text and CJK line-breaking, speaker/sound-effect notation, font-size scaling without losing meaning, and image-based (PNG) subtitle fallback — including the deliberate product decision of whether theatrical positioning is preserved or intentionally normalized for phone display.
- **Deliverables:** hardened image; production Booth-Diagnostic / Patron-Facing / Low-Cost-Replacement profiles; installer + QA checklist; support tooling; caption rendering-fidelity decisions documented (preserve-vs-normalize positioning, multi-line, scripts/RTL/CJK, image-subtitle fallback).
- **G4 exit criteria:** survives power-loss/soak/hot-plug without corruption or content persistence; a bad update auto-rolls-back; runtime security verification continuously passes; a non-specialist can complete first-boot → calibrated → ready using only the wizard and kit.

### Phase 9 — Field pilot & operational/compliance validation  ⟶ **GATE G5: "In-lieu-of-professional" sign-off**

- **Goal:** earn the "replacement" claim responsibly (this gate is *not* in the original brief and is added deliberately — see Section 1.3).
- **Scope:** supervised pilots in real venues; validate staff procedures (including **what staff do when a readiness check fails**), device availability, glare/battery/headphone realities, multilingual needs, and failure fallback; gather latency/reliability data across a real phone fleet; and complete the **operational and legal/accessibility-obligation review** for each target jurisdiction/venue.
- **Deliverables:** pilot report (reliability, latency distribution, incident log); the **validated staff runbook** (the Phase 7 v0 proven and corrected in real venues); a documented compliance posture per venue/jurisdiction (produced with the venue's own legal/accessibility counsel — **not** by the engineering team alone).
- **G5 exit criteria:** pilots meet agreed reliability/latency thresholds; staff procedures are proven; and a venue-specific operational/legal review has explicitly cleared CaptionBridge for use in lieu of a professional system **before** it is sold/deployed as such.


## 14. Open standards & interoperability questions to resolve before production

These are blockers or near-blockers. Each should be assigned an owner and closed with a real server, real venue, or real stakeholder — not assumed. (Marked **OPEN** where referenced above.) The ACSP/caption-path items 1–4 are the explicit job of the **Phase 0A ACSP Wire Contract** (Section 13).

**ACSP / caption path**
1. **Endpoint registration:** does the first target server **auto-discover** an ACSP device, or must an operator **configure CaptionBridge's IP** into the server's accessibility settings? Per server family. *(Biggest installability unknown.)*
2. **Transport details:** exact ACSP framing/PDU structure, port(s), connection-initiation direction, and any TLS/authentication — confirmed against the **licensed ST 430-10** and the first target server.
3. **Which server family to target first** for lab validation (drives Phase 4 scheduling and lab-time cost).
4. **Message-flow and resource-locator specifics** the chosen server actually exposes: lease durations, timeline cadence, output-mode habits, Playout-ID behavior, **and the form of the RPL's resource locators** (URI schemes, paths, and the timed-text fetch mechanism) — which determines the resource-resolver handlers (Section 5.3).
5. **Multi-language default policy** — *provisionally resolved*: the priority in Section 5.4 (venue/show default → browser language → first ClosedCaption track → first subtitle track) is set; it remains a **Phase 7 UX validation** item with venue stakeholders, not an open standards blocker.
6. **Caption latency/drift budget:** confirm the acceptable on-screen target (the ~250–500 ms working figure) with stakeholders.

**Audio path**
7. **Where HI and VI-N are sourced** in the first target venues (which processor outputs/jacks, which channels, what level).
8. **Output topology in the field:** one mono output? a stereo pair? separate HI/VI-N outputs? headphone-level only? (Drives default channel mapping + shipped cabling.)
9. **Minimum consumer audio hardware** to ship/recommend (output of Phase 0C, but must be ratified).
10. **Acceptable HI vs VI-N latency** in a phone-browser workflow — confirmed against the **measured** achievable numbers from Phases 0C/5, not just the aspirational targets.

**Network / security / operations**
11. **Patron Wi-Fi model:** does CaptionBridge **host its own** caption/audio Wi-Fi (recommended), or integrate into an existing theater guest/admin VLAN? Per venue/chain.
12. **Minimum security profile a chain will accept** on a booth network (drives the production firewall profile and audit features).
13. **Secure-context / cert strategy** for patron WebRTC + wake-lock (Section 6.7) — pick one approach and standardize it (candidates **characterized in Phase 0B, locked in Phase 7**; note that Loaner Device Mode, 12.5, can sidestep this for venue-owned fleets).
14. **First-deployment intent:** internal diagnostic-only, festival operations, low-cost replacement, or patron-facing pilot? (Sets which profile ships first.)

**Operational / legal**
15. **Staff procedure on readiness-check failure** before a show (no-go criteria + fallback).
16. **Legal/compliance and access-equivalence posture** if used in lieu of a professional accessibility receiver system — including the venue's procedure for patrons without a compatible phone, headphones, or charge (Section 1.3) — resolved per jurisdiction/venue with proper counsel (Phase 9 / Section 1.3).

---

## 15. Consolidated risks & mitigations

| # | Risk | Likelihood / impact | Mitigation |
|---|---|---|---|
| R1 | **Server interoperability** — discovery/registration/lease/timeline/output-mode/Playout-ID **and resource-locator forms** differ across families | High / High | **Phase 0A wire contract (pcap + confirmed direction) before building**; pluggable resource resolver (Section 5.3); simulator covering known quirks (Phase 3); per-family compatibility log; budget generous real-server lab time (Phase 4); start with one family, expand. |
| R2 | **HI audio latency** — browser jitter buffer (and Bluetooth output latency) make sub-150 ms hard, especially while the patron also hears the auditorium | High / High | Measure glass-to-ear early (Phase 0C crude, Phase 5 rigorous); **separate HI gate (G2b) that must be earned**; **recommend wired headphones for HI**; minimize `playoutDelayHint`/`jitterBufferTarget`; mandate dedicated AP; **set venue expectations honestly**; patron buffer control for bad RF; documented threshold below which HI is not offered. |
| R3 | **Low-capability analog input** — mono, noisy, biased, AGC, easily overloaded | High / Med-High | Active qualification (not acceptance); calibration wizard; shipped kit with pad + isolator; prefer Tier-2 interface; four-state verdicts surfaced pre-show. |
| R4 | **Network-isolation failure** — any path patron→theater would be serious | Low (by design) / Critical | Zero forwarding (v4+v6); no bridges; allowlist; install-time **and** runtime verification; loud dashboard warnings; security tests in CI and on-device. |
| R5 | **Timing drift / desync** under pause/seek/reconnect | Med / High | Monotonic-clock mapping with re-anchoring (never sleep loops); explicit timeline-event handling; drift surfaced to diagnostics; soak tests. |
| R6 | **Content leakage / unwanted persistence** of captions or audio | Low / High | Transient-only by default; purge at show end; local-only token-gated streams; debug capture explicit/limited/visible; no internet rebroadcast. |
| R7 | **Patron onboarding friction** — cert warnings, autoplay, no app | Med / Med | Characterize secure-context strategies in the Phase 0B spike and **lock the choice by Phase 7**; QR + token onboarding; clear staff guidance; gesture-to-play handled gracefully. |
| R8 | **Wi-Fi reliability** for a full auditorium of clients | Med / Med-High | Dedicated external AP over onboard radios; capacity soak tests; reconnect-resilient fan-out and WebRTC. |
| R9 | **Bricking on update** in a remote booth | Low / High | Signed offline updates; A/B partitions with automatic rollback on failed health-check; TPM-held keys on hardened tier. |
| R10 | **Compliance over-claim** — treated as a pro replacement before it's earned, including **access-equivalence** for patrons without a usable phone/headphones/charge | Med / High | Phase 9 gate (G5); per-venue operational + legal review before any "in-lieu-of" deployment; **require a venue fallback procedure (loaner devices, wired earbuds, staff help, fallback to pro receivers)**; reliability/latency thresholds agreed up front. |
| R11 | **Field miswiring** — theater vs patron ports swapped in the booth | Med / High | Clear physical port labeling; first-boot interface-confirmation step; runtime detection of unexpected interface topology. |
| R12 | **Wrong auditorium / stale QR** — appliance miswired to the wrong server, or yesterday's QR admits patrons to today's wrong room | Med / Med-High | Pin expected server identity per auditorium (Sections 8.5, 10.5); auditorium profile tied to physical port label; per-show QR with auditorium ID + expiry; patron page shows "Auditorium X"; TMS metadata-mismatch warning when integrated. |
| R13 | **Patron device path** — Bluetooth headphone latency, speaker-mode playback, dead battery, or no compatible phone make the BYOD path unusable for some patrons | Med / Med-High | Recommend wired headphones (esp. HI); warn on speaker-mode where detectable; characterize Bluetooth separately (Phase 5); venue loaner-device/earbud fallback (R10) for access-equivalence. |

---

## 16. Summary

CaptionBridge is buildable as a phased product whose **caption path can reach protocol/source parity** with professional systems (it consumes the same standards-based ST 430-10/ST 430-11 auxiliary-content feed) — with full *rendering* parity a later milestone — and whose **audio path is honestly constrained** by analog wiring and consumer ADCs, which is why captions and audio carry fully independent state, health, and readiness throughout. The likely first shippable product is the **Booth Diagnostic** profile, which earns real-world compatibility data without making an access-equivalence claim.

The plan front-loads the things most likely to sink the project into Phase 0 spikes — the **ACSP wire contract (0A)**, **browser local-appliance reality (0B)**, and **audio-hardware + HI-latency reality (0C)** — and treats **network isolation correctness** as a first-class gate (woven into Phase 4's G1). It treats the **ST 430-10 state machine** as a lease-and-timeline state machine (not a parser), grounds it in the concrete ACSP request sequences and robustness behaviors from public implementation notes, and demands those behaviors be reconciled against the **licensed standards and a real server** before shipping. It makes the **WebRTC/Opus** decision concrete (host-only ICE, a supervised GStreamer `webrtcbin` media process, tuned Opus) while flagging the **HI latency target as an aggressive risk to measure, not assume** — which is why audio is split into a likely-achievable **VI-N gate (G2a)** and an earn-it **HI gate (G2b)**. And it adds a deliberate **Phase 9 / G5 gate** so the "in lieu of a professional system" claim is *earned* through field reliability and venue/legal review rather than asserted.

Build captions and isolation first. Add audio second, with eyes open about latency and cheap hardware. Integrate, harden, then validate in the field before anyone's accessibility depends on it.
