# OpenAccessBridge — autonomous build kickoff

You are building **OpenAccessBridge** from the specification in `./buildplan.md`. Read that
file in full before doing anything; it is the authoritative source for the architecture,
the three-process model, the protocol/state-machine design, the data models, the security
invariants, the phased plan, and the deployment profiles. Re-read individual sections as
you implement them rather than relying on memory.

## Goal for this session
Take the project as far as it can go **in software, on this dev machine, without physical
hardware or a real cinema server.** Build a working, tested, simulator-driven system and
leave a clean handoff for everything hardware-gated. Work continuously; do NOT stop to ask
me questions — make reasonable decisions, record them, and keep going.

## In scope (build and test these now), following the plan's ordering
1. **Foundation:** repo scaffold; the three supervised processes (`openaccessbridge-core`,
   `openaccessbridge-media`, `openaccessbridge-firewall`) with loopback IPC per §3.5; build
   system; CI; the SQLite config store and versioning record (§10, §10.8); structured
   logging with the observability event taxonomy and the *no caption text / audio / SDP /
   client-IDs in logs* rule (§9.4); the data models (§10).
2. **Caption path (Phases 1–3) — the heart of this session:**
   - timed-text parser — ST 428-7 + the ST 428-10 closed-caption profile + legacy Interop,
     text/timing only (§5.4);
   - the monotonic-clock timing engine (§5.5) — never sleep-loops;
   - the RPL parser, the **pluggable resource resolver with the optional MXF/track-file
     unwrap stage**, and the bounded transient cache (§5.3);
   - caption fan-out over WS/SSE (§5.6) and the patron caption page (§7);
   - the **ST 430-10 state machine** (§5.2) and the **ST 430-10 simulator** (§11.1) with the
     full quirk scenario suite (lease near-/actual-expiry; comms-loss-continue-until-lease;
     Playout-ID=0 / duplicates; empty ReelResources; output-mode toggling/absence; show
     change / rewind / abandoned RPL) **wired into CI as the regression backbone.**
   Build the simulator and state machine against the protocol model documented in §5.1–5.2
   (public ISDCF notes), clearly marked **PROVISIONAL** with a seam to swap in the real
   Phase 0A wire contract later.
3. **Audio pipeline code (Phases 5–6):** capture → four-state qualifier → channel mapper →
   Opus encode → WebRTC, as `openaccessbridge-media`. Unit-test the qualifier classification
   and the mapper against synthetic/recorded fixtures; add a synthetic-tone source so the
   pipeline smoke-tests end-to-end locally. (Real-device latency, real cinema outputs, and
   real phones are out of scope — see below.)
4. **Admin UI + setup wizard (§8):** first-boot workflow, dashboard, loud-warnings,
   public/admin route+bind separation, the auditorium/show-identity safeguards (§8.5).
5. **Network/security (§9):** firewall manager generating the nftables Variant A/B rules;
   install/runtime verification by **self-introspection** of routes/rules (not subnet
   scanning, per §9.3); forwarding-disabled assertions; the security tests (§11.6) in CI.
6. **Deployment profiles (§12)** including the Loaner Device overlay, as configuration.

## Out of scope this run — do NOT fake; record in BLOCKERS.md and move on
Anything needing a real cinema server, real iOS/Android phones, or physical audio hardware:
Phase 0A wire capture, Phase 0B browser/secure-context/mDNS validation, Phase 0C
audio-hardware + glass-to-ear latency, and gates G1/G2a/G2b/G3/G4/G5. When you reach one,
stub or simulate behind the seam, write exactly what hardware/artifact is needed to clear it
in `BLOCKERS.md`, and continue to the next buildable item. Do not stop and wait for me.

## Operating rules
- **buildplan.md is the spec.** Follow its decisions and invariants. Where it marks
  something **OPEN** (HTTP-vs-cert, concurrent-client counts, which server family is
  Target A), pick a safe default to make the system run, record it in `DECISIONS.md` with
  rationale + a section pointer, and keep it **configurable** — never silently hardcode an
  open decision as if settled.
- **Honor the invariants from day one and write tests asserting them:** no persistence of
  caption/audio content (memory/tmpfs only); no IP forwarding (v4+v6); default-deny
  firewall; three-process separation; independent caption/audio readiness state;
  no-silent-degradation surfacing; nothing sensitive in logs.
- **Never fabricate results.** If you can't validate something without hardware, say so in
  BLOCKERS.md — never write a test or log line that pretends a real server/phone/jack was
  exercised.
- **Test-driven and incremental.** Keep `build` and the full test suite green at all times;
  the simulator scenario suite grows with the caption path. **Commit frequently** with clear
  messages.
- **Durable external memory (you will be compacted — survive it).** Maintain four living
  files at the repo root, updated on every milestone and commit:
  - `STATUS.md` — done / in-progress / next, plus how to build and test.
  - `DECISIONS.md` — every assumption and open-item default, with rationale + buildplan §.
  - `BLOCKERS.md` — each hardware/external dependency and what's needed to clear it.
  - `JOURNAL.md` — a short running log of what you did and why.
  Re-read buildplan.md sections on demand instead of holding the whole file in context.
- **Stay bounded and safe.** Keep all work inside the project directory. You may install
  standard toolchain/dev dependencies (Go, GStreamer dev libs, Node for the SPA, etc.). Make
  no destructive changes to the wider system; exfiltrate nothing; no network calls beyond
  package installs and fetching public standards/reference material.
- **Breadth before depth.** Prefer a coherent end-to-end skeleton early, then deepen — so if
  you run out of context I have a working partial system, not one over-polished corner.
- **Maintain a todo list and work it continuously.** When a unit is done, start the next.
  Only stop when the in-scope backlog above is genuinely exhausted — then write a final
  STATUS handoff rather than asking me what to do next.

## Start by
1. Reading `buildplan.md` in full.
2. Verifying/installing the toolchain and scaffolding the repo and CI.
3. Writing the initial `STATUS.md`, `DECISIONS.md`, `BLOCKERS.md`, `JOURNAL.md`, and a todo
   list derived from the plan.
4. Proceeding through the in-scope backlog, simulator-and-test-first, committing as you go.

Begin.
