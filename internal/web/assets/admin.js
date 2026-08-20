/* CaptionBridge admin dashboard (§8.2 rows, §8.3 loud warnings, §8.4 banner,
 * §10.6 two independent verdicts). Vanilla JS, embedded, no framework, no build
 * step, no network fetch beyond this appliance's own /api/admin/status (D4).
 *
 * THE RULE THIS FILE EXISTS TO KEEP: a fact the appliance did not measure is
 * rendered as "not measured". Never a number, never a tick, never a soothing
 * default. /api/admin/status distinguishes measured-false from never-measured
 * (firewall_introspected, latency_measured, encoder_state, the Tri fields, the
 * identity-check outcome); this page has to preserve that distinction or the
 * whole point of measuring is lost.
 *
 * It also never shows caption text. "Current cue" is a VISIBILITY row and the
 * API carries no cue text to it (§9.4 caption.cue.visible_no_text). */
(function () {
  "use strict";

  var STATE_OK = "ok", STATE_BAD = "bad", STATE_WARN = "warn", STATE_UNKNOWN = "unknown";

  function el(id) { return document.getElementById(id); }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  /* row(label, value, state, note) — state is one of STATE_*; null leaves it
   * neutral. note is a second line explaining a caveat (e.g. why a figure is an
   * estimate rather than a measurement). */
  function row(label, value, state, note) {
    return { label: label, value: value, state: state || null, note: note || "" };
  }

  function dl(target, rows) {
    target.innerHTML = "";
    rows.forEach(function (r) {
      var dt = document.createElement("dt");
      dt.textContent = r.label;
      var dd = document.createElement("dd");
      dd.textContent = r.value;
      if (r.state) dd.className = r.state;
      if (r.note) {
        var n = document.createElement("span");
        n.className = "note";
        n.textContent = r.note;
        dd.appendChild(n);
      }
      target.appendChild(dt);
      target.appendChild(dd);
    });
  }

  /* tri renders a model.Tri ("yes" | "no" | absent). An absent value is NOT
   * "no": it means nothing measured it. goodIsYes says which answer is the
   * healthy one, so a row can be coloured without pretending unknown is either. */
  function tri(v, goodIsYes) {
    if (v !== "yes" && v !== "no") return { value: "not measured", state: STATE_UNKNOWN };
    var good = (v === "yes") === (goodIsYes !== false);
    return { value: v, state: good ? STATE_OK : STATE_BAD };
  }

  function triRow(label, v, goodIsYes, note) {
    var t = tri(v, goodIsYes);
    return row(label, t.value, t.state, note);
  }

  function yesNo(b, goodIsYes) {
    var good = !!b === (goodIsYes !== false);
    return { value: b ? "yes" : "no", state: good ? STATE_OK : STATE_BAD };
  }

  function ynRow(label, b, goodIsYes, note) {
    var v = yesNo(b, goodIsYes);
    return row(label, v.value, v.state, note);
  }

  function ago(iso) {
    if (!iso) return "";
    var t = Date.parse(iso);
    if (isNaN(t)) return "";
    var s = Math.max(0, Math.round((Date.now() - t) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.round(s / 60) + "m ago";
    if (s < 86400) return Math.round(s / 3600) + "h ago";
    return Math.round(s / 86400) + "d ago";
  }

  /* ---- §8.4 deployment banner ---------------------------------------------
   * The profile descriptor carries WarningBanner precisely because §8.4 permits
   * admin on the patron network only in Lab and Booth Diagnostic and only behind
   * a prominent banner. The backend composes the wording (it is the side that
   * knows whether admin really is reachable from the patron side); this only has
   * to make it impossible to miss. */
  function renderDeployBanner(s) {
    var b = el("deploy-banner");
    if (!s.deployment_banner) { b.hidden = true; b.textContent = ""; return; }
    b.hidden = false;
    b.textContent = "⚠ " + s.deployment_banner;
  }

  /* ---- §8.3 loud warnings -------------------------------------------------- */
  function renderWarnings(s) {
    var banner = el("warning-banner");
    var warns = (s.warnings || []).slice();
    if (!warns.length) { banner.hidden = true; banner.innerHTML = ""; return; }
    // Critical first: the thing that stops the show goes at the top.
    warns.sort(function (a, b) {
      var ra = a.severity === "critical" ? 0 : 1, rb = b.severity === "critical" ? 0 : 1;
      return ra - rb;
    });
    banner.hidden = false;
    banner.innerHTML = warns.map(function (w) {
      var cls = w.severity === "critical" ? "warn-critical" : "warn-warning";
      return '<div class="' + cls + '">⚠ ' + escapeHtml(w.message) + "</div>";
    }).join("");
  }

  /* ---- §10.6 the two independent verdicts ---------------------------------
   * Never merged into one health light. Each carries its own "why" so "NOT
   * READY" is actionable rather than merely alarming. */
  function renderVerdicts(s) {
    setVerdict("caption", s.caption_ready, captionWhy(s));
    setVerdict("audio", s.audio_ready, audioWhy(s));
  }

  function setVerdict(which, ok, why) {
    var box = el(which + "-big");
    box.className = "bigverdict " + (ok ? "ok" : "bad");
    el(which + "-verdict").textContent = ok ? "READY" : "NOT READY";
    el(which + "-why").textContent = why;
  }

  function captionWhy(s) {
    var cl = s.caption_link || {};
    if (s.caption_ready) return "Link " + (cl.state || "?").toLowerCase() + ", lease held, assets loaded.";
    if (!cl.state || cl.state === "DISCONNECTED") return "No ACSP link to the cinema server.";
    if (!cl.lease_active) return "Connected, but no lease is held.";
    if (!cl.rpl_loaded) return "Lease held, but no RPL has arrived for this show.";
    return "Link is " + cl.state + "; not yet serving captions.";
  }

  function audioWhy(s) {
    var inputs = s.audio_inputs || [];
    if (!inputs.length) return "The media process reported no audio inputs.";
    var failed = inputs.filter(function (a) { return a.status === "failed"; })
      .map(function (a) { return a.stream_type; });
    if (failed.length) return failed.join(" and ") + " failed qualification.";
    if (s.audio_ready) return inputs.length + " input(s) qualified usable or better.";
    return "No input reached a usable verdict.";
  }

  /* ---- §8.2 caption-path rows --------------------------------------------- */
  function renderCaption(s) {
    var cl = s.caption_link || {};
    var langs = (cl.available_languages || []).join(", ");
    var timeline = s.timeline_state || "no_timeline";
    dl(el("caption-detail"), [
      row("Link state", cl.state || "—", null),
      ynRow("Lease active", cl.lease_active, true,
        cl.lease_active && cl.lease_expires_in_ms ? "expires in " + Math.round(cl.lease_expires_in_ms / 1000) + "s" : ""),
      timelineRow(timeline),
      ynRow("RPL loaded", cl.rpl_loaded, true),
      ynRow("Caption assets loaded", cl.assets_loaded, true),
      row("Current language", cl.current_language || "—", null),
      row("Languages available", langs || "—", null),
      // §8.2 reads "Current cue: visible / none". §9.4: VISIBILITY ONLY — the
      // cue's words are never carried to this page, logged, or persisted.
      cueRow(s.current_cue_visible),
      row("Connected caption clients",
        (s.connected_caption_clients || 0) + " of " + (s.max_concurrent_clients ? s.max_concurrent_clients : "unlimited"),
        capState(s), s.max_concurrent_clients ? "" : "no per-profile cap is configured (D11)"),
      driftRow(cl, s.caption_drift_budget_ms),
    ]);
  }

  /* timelineRow is §8.2's "Timeline: playing / paused / stopped". "no timeline"
   * is its own fourth answer — the server has not said — and it is rendered as
   * an unmeasured fact, not as "stopped". Paused and stopped are MEASURED and
   * unremarkable states, so they are neutral rather than greyed out. */
  function timelineRow(timeline) {
    if (timeline === "playing") return row("Timeline", "playing", STATE_OK);
    if (timeline === "paused" || timeline === "stopped") return row("Timeline", timeline, null);
    return row("Timeline", "no timeline", STATE_UNKNOWN, "the server has not reported a transport state");
  }

  /* cueRow is §8.2's "Current cue: visible / none". "not measured" is a real
   * third answer: with no caption language loaded there is nothing that could be
   * on screen, and printing "none" would claim an observation nobody made. */
  function cueRow(v) {
    if (v === "yes") {
      return row("Current cue", "visible", STATE_OK, "visibility only — cue text never leaves the patron path");
    }
    if (v === "no") {
      return row("Current cue", "none", null, "visibility only — cue text never leaves the patron path");
    }
    return row("Current cue", "not measured", STATE_UNKNOWN, "no caption language is loaded, so no cue can be on screen");
  }

  /* driftRow shows §5.5's drift measurement against the budget it is judged by,
   * rather than as a bare number. It is deliberately NOT coloured as a verdict:
   * drift is measured at each re-anchor, so a legitimate server seek or a show
   * loop yields a large figure that is not a fault. The budget is shown so a
   * tech can judge it; escalating it to a red row would cry wolf on every seek. */
  function driftRow(cl, budget) {
    if (cl.drift_ms_estimate == null) {
      return row("Drift at last re-anchor", "not measured", STATE_UNKNOWN);
    }
    var note = budget ? "warn budget " + budget + " ms — a server seek or show loop also shows here" : "";
    return row("Drift at last re-anchor", cl.drift_ms_estimate + " ms", null, note);
  }

  function capState(s) {
    if (!s.max_concurrent_clients) return STATE_WARN;
    if ((s.connected_caption_clients || 0) >= s.max_concurrent_clients) return STATE_WARN;
    return null;
  }

  /* ---- §8.5 wrong-room: pinned identity vs who actually answered ---------- */
  function renderServer(s) {
    var cl = s.caption_link || {};
    var check = s.server_identity_check || "unknown";
    var tag = el("identity-tag");
    tag.textContent = identityTagText(check);
    tag.className = "tag " + identityTagState(check);

    var rows = [
      // The PINNED identity and who actually answered are separate rows on
      // purpose: a dashboard that shows only the connected server's own claim
      // can never display a mismatch, because it cannot contradict itself.
      row("Allowed cinema server (pinned)", s.allowed_cinema_server || "not pinned",
        s.allowed_cinema_server ? null : STATE_WARN),
      row("Connected server (announced)", s.connected_cinema_server || "—",
        mismatchState(check)),
      // Derived server-side (model.CaptionLinkState.ServerConnected) so "which
      // ACSP states count as connected" lives in one tested place, not here.
      triRow("Server connected", s.server_connected, true),
      row("Wrong-room check", identityCheckLabel(check), identityRowState(check)),
      // §10.5: a composite identity MUST be labelled as such so it is not
      // mistaken for a security property the protocol does not provide.
      row("Identity strength", identityStrengthLabel(s.identity_strength),
        s.identity_strength === "composite" ? STATE_WARN : null,
        s.identity_strength === "composite"
          ? "a wrong-room and misconfiguration guard — NOT authentication of the server"
          : ""),
      row("Output mode", cl.output_mode || "unknown", cl.output_mode ? null : STATE_UNKNOWN),
      row("Playout ID", cl.current_playout_id || "—", null),
    ];
    dl(el("server-detail"), rows);
  }

  function identityCheckLabel(check) {
    switch (check) {
      case "match": return "matched the pinned identity";
      case "mismatch": return "DOES NOT MATCH — wrong room";
      case "unverified": return "not verified — the server announced nothing to check the pin against";
      case "unpinned": return "not checked — this auditorium pins no server identity";
      default: return "not checked — no server has announced yet";
    }
  }
  function identityTagText(check) {
    if (check === "match") return "verified";
    if (check === "mismatch") return "WRONG ROOM";
    return "unverified";
  }
  function identityTagState(check) {
    if (check === "match") return STATE_OK;
    if (check === "mismatch") return STATE_BAD;
    return STATE_UNKNOWN;
  }
  function identityRowState(check) {
    if (check === "match") return STATE_OK;
    if (check === "mismatch") return STATE_BAD;
    return STATE_UNKNOWN;
  }
  function mismatchState(check) { return check === "mismatch" ? STATE_BAD : null; }

  function identityStrengthLabel(v) {
    switch (v) {
      case "tls_fingerprint": return "TLS certificate fingerprint";
      case "server_id": return "server-reported identifier";
      case "composite": return "composite identity (IP + family + screen name)";
      case undefined: case null: case "": return "not pinned";
      default: return v;
    }
  }

  /* ---- §8.2 audio rows ----------------------------------------------------- */
  function renderAudio(s) {
    var host = el("audio-detail");
    host.innerHTML = "";
    var inputs = s.audio_inputs || [];
    var summaries = s.audio_streams || [];
    if (!summaries.length) {
      var none = document.createElement("p");
      none.className = "muted";
      none.textContent = "The media process reported no audio inputs, so nothing about HI or VI-N " +
        "has been measured — this is not the same as “no audio problem”.";
      host.appendChild(none);
      return;
    }
    var byType = {};
    inputs.forEach(function (a) { byType[a.stream_type] = a; });

    summaries.forEach(function (sum) {
      var raw = byType[sum.stream_type] || {};
      var d = document.createElement("div");
      d.className = "audio-row";

      var head = document.createElement("div");
      var name = document.createElement("span");
      name.className = "name";
      name.textContent = sum.stream_type;
      var q = document.createElement("span");
      q.className = "tag " + qualityState(sum.quality);
      q.textContent = sum.quality || "not judged";
      head.appendChild(name);
      head.appendChild(q);
      d.appendChild(head);

      var facts = document.createElement("dl");
      dl(facts, [
        triRow("Input present", sum.input_present, true),
        // §8.2 "HI stream active / VI stream active". With no WebRTC subscriber
        // transport (BLOCKERS B2/B4) a proven encoder still reaches nobody, so
        // the honest answer is "no" with the reason attached.
        triRow("Stream active", sum.stream_active, true, encoderNote(sum.encoder_state)),
        row("Connected audio clients", String(sum.connected_clients || 0), null),
        row("Level", raw.level_dbfs != null ? raw.level_dbfs.toFixed(1) + " dBFS" : "not measured",
          raw.level_dbfs != null ? null : STATE_UNKNOWN),
        latencyRow(sum),
      ]);
      d.appendChild(facts);

      if (raw.reasons && raw.reasons.length) {
        var why = document.createElement("p");
        why.className = "reasons";
        why.textContent = raw.reasons.join("; ");
        d.appendChild(why);
      }
      host.appendChild(d);
    });
  }

  /* latencyRow is the sharpest honesty case in the audio block. §8.2 asks for
   * "Estimated audio latency"; §6.7 / D10 budgets are GLASS-TO-EAR, and the only
   * number this appliance has is an encode-path estimate that excludes the Wi-Fi
   * hop, the browser jitter buffer (the dominant term) and the phone's output.
   * Until latency_measured is true (BLOCKER B3) the row says "not measured", and
   * the estimate appears beneath it under its own name. */
  function latencyRow(sum) {
    if (sum.latency_measured) {
      return row("Audio latency (glass-to-ear)", sum.latency_ms + " ms", STATE_OK, "end-to-end measurement");
    }
    var note = sum.encode_path_estimate_ms
      ? "encode-path estimate only: " + sum.encode_path_estimate_ms +
        " ms (excludes Wi-Fi, browser jitter buffer and phone output)"
      : "no encoder ran, so there is not even an estimate";
    return row("Audio latency (glass-to-ear)", "not measured", STATE_UNKNOWN, note);
  }

  function encoderNote(state) {
    switch (state) {
      case "ready_no_transport":
        return "a real encode runs locally, but there is no subscriber transport to carry it";
      case "unavailable":
        return "no encoder ran — nothing can reach a patron";
      case "unknown": case undefined: case null: case "":
        return "the encode path has not been exercised";
      default:
        return "encoder state “" + state + "” is not one this build recognises";
    }
  }

  function qualityState(q) {
    if (q === "excellent" || q === "usable") return STATE_OK;
    if (q === "marginal") return STATE_WARN;
    if (q === "failed") return STATE_BAD;
    return STATE_UNKNOWN;
  }

  /* ---- §9.x network and security ------------------------------------------ */
  function renderNet(s) {
    dl(el("net-detail"), [
      // THE sharpest row on this page. "Active" and "active but nobody read the
      // ruleset" must not look alike: the second is an assertion, and §9.3 has
      // the appliance prove its posture by introspection. Rendering them the
      // same is what made the measurement pointless.
      firewallRow(s),
      row("IP forwarding", s.forwarding_disabled ? "disabled" : "ENABLED",
        s.forwarding_disabled ? STATE_OK : STATE_BAD,
        s.forwarding_disabled ? "" : "the appliance must never route between zones"),
      ynRow("Bridges present", s.bridges_present, false),
      // Two DIFFERENT questions, and conflating them is how §8.4's condition
      // hides. The first is the raw fact — can a phone on the patron SSID reach
      // /admin. The second is whether that meets the ACTIVE profile's rule, and
      // Lab and Booth Diagnostic have no rule, so it reads "yes" in exactly the
      // case the banner above exists for.
      triRow("Admin on patron network", s.admin_reachable_from_patron, false,
        s.admin_reachable_from_patron === "yes"
          ? "the patron network is untrusted BYOD — authentication alone is not the boundary (§8.4)"
          : ""),
      ynRow("Admin binding meets profile", s.admin_isolated, true),
      ynRow("Default credentials", s.default_credentials_present, false),
      ynRow("SSH disabled", s.ssh_disabled, true),
      row("Theater interface", s.theater_interface || "not configured", s.theater_interface ? null : STATE_WARN),
      row("Client interface", s.client_interface || "not configured", s.client_interface ? null : STATE_WARN),
      // §8.2's last two rows read "Off" when healthy, so they are rendered as
      // on/off rather than run through the yes/no helper — flipping the WORD to
      // suit the colour would print "yes" for a service that is switched off.
      offOnRow("Discovery mode", s.discovery_mode,
        "no discovery/advertisement responder runs in any CaptionBridge process"),
      offOnRow("Support session", s.support_session,
        s.support_session === "yes"
          ? "outbound-only, time-limited and audited (§9.4) — close it before the show"
          : ""),
    ]);
  }

  /* offOnRow renders a Tri whose healthy answer is "no" as Off/ON, colouring ON
   * as a warning. Unknown stays "not measured": a switch nothing could read is
   * not the same as a switch that is off. */
  function offOnRow(label, v, note) {
    if (v !== "yes" && v !== "no") return row(label, "not measured", STATE_UNKNOWN, note);
    return row(label, v === "yes" ? "ON" : "off", v === "yes" ? STATE_WARN : STATE_OK, note);
  }

  /* Four distinct states, three distinct renderings. The pair
   * (firewall_active, firewall_introspected) is the whole point of the
   * introspection work: "active" and "active but nobody read the ruleset" are
   * different claims, and so are "measured not default-deny" and "could not be
   * read at all". None of them may be printed as plain "Active". */
  function firewallRow(s) {
    if (s.firewall_active && s.firewall_introspected) {
      return row("Firewall", "active (verified)", STATE_OK, "the loaded ruleset was read and is default-deny");
    }
    if (s.firewall_active) {
      return row("Firewall", "active (UNVERIFIED)", STATE_WARN,
        "asserted by a caller — nothing read the loaded ruleset, so this posture is not proven");
    }
    if (s.firewall_introspected) {
      return row("Firewall", "NOT default-deny", STATE_BAD, "the loaded ruleset was read and it does not deny by default");
    }
    // Neither active nor introspected: the ruleset could not be read, so the
    // posture is unproven in BOTH directions. Saying "no ruleset is loaded"
    // here would be the same fabrication in the opposite direction.
    return row("Firewall", "not proven active", STATE_BAD,
      "the loaded ruleset could not be read — see the warning above for why");
  }

  /* ---- §3.5 per-process liveness ------------------------------------------ */
  function renderProcesses(s) {
    var procs = s.processes || [];
    if (!procs.length) {
      dl(el("process-detail"), [row("Liveness", "not reported", STATE_UNKNOWN)]);
      return;
    }
    dl(el("process-detail"), procs.map(function (p) {
      if (p.alive) {
        return row(p.name, "alive", STATE_OK, p.last_seen ? "last answered " + ago(p.last_seen) : "");
      }
      // A helper that has NEVER answered reads differently from one that
      // answered a while ago and has since gone. Collapsing the two hides
      // "it never came up" behind "it died", which are different repairs.
      var note = p.last_seen
        ? "last answered " + ago(p.last_seen) + " — it was up and has stopped"
        : "has never answered since core started";
      if (p.detail) note += " (" + p.detail + ")";
      return row(p.name, "NOT ANSWERING", STATE_BAD, note);
    }));
  }

  /* ---- §10.8 what changed since this last worked -------------------------- */
  function renderVersions(s) {
    var v = s.system_config || {};
    var fail = v.last_readiness_check_failure;
    dl(el("version-detail"), [
      row("Config schema", v.config_schema_version != null ? String(v.config_schema_version) : "not recorded",
        v.config_schema_version != null ? null : STATE_UNKNOWN),
      row("Hardware profile", v.hardware_profile_version || "not recorded",
        v.hardware_profile_version ? null : STATE_UNKNOWN),
      row("Firewall profile rev", v.firewall_profile_version || "not recorded",
        v.firewall_profile_version ? null : STATE_UNKNOWN),
      row("Server family profile", v.server_family_compat_profile || "not recorded",
        v.server_family_compat_profile ? null : STATE_UNKNOWN),
      row("Audio device profile", v.audio_device_compat_profile || "not recorded",
        v.audio_device_compat_profile ? null : STATE_UNKNOWN),
      // §10.8 exists to answer "what changed since this last worked". A stamp
      // nothing wrote must say so — this field was fabricated once already, by
      // being stamped when an operator clicked Finish.
      row("Last readiness check passed",
        v.last_readiness_check_ok ? v.last_readiness_check_ok + " (" + ago(v.last_readiness_check_ok) + ")" : "never",
        v.last_readiness_check_ok ? STATE_OK : STATE_UNKNOWN),
      row("Last readiness failure",
        fail ? fail.at + " (" + ago(fail.at) + ")" : "none recorded",
        fail ? STATE_WARN : null,
        fail && fail.reason ? "reason: " + fail.reason : ""),
      row("Update channel", v.update_channel || "not recorded", null),
    ]);
  }

  function render(s) {
    // §8.5: name the room this page describes. A tech with two booth laptops
    // open should never have to guess which auditorium they are looking at.
    el("room").textContent = s.auditorium ? "· " + s.auditorium : "";
    el("profile-line").textContent = s.deployment_profile_title
      ? "Deployment profile: " + s.deployment_profile_title
      : "Deployment profile: not reported";
    renderDeployBanner(s);
    renderWarnings(s);
    renderVerdicts(s);
    renderCaption(s);
    renderServer(s);
    renderAudio(s);
    renderNet(s);
    renderProcesses(s);
    renderVersions(s);
    el("refreshed").textContent = "updated " + new Date().toLocaleTimeString();
  }

  /* A dashboard that silently keeps showing the last good snapshot when the API
   * stops answering is the same failure this page exists to prevent, so a failed
   * poll says so loudly rather than leaving stale facts on screen. */
  function pollFailed() {
    var banner = el("warning-banner");
    banner.hidden = false;
    banner.innerHTML = '<div class="warn-critical">⚠ The admin API is not answering — ' +
      "every figure below is stale. Check captionbridge-core.</div>";
  }

  function refresh() {
    fetch("/api/admin/status", { credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.json();
      })
      .then(render)
      .catch(pollFailed);
  }
  refresh();
  setInterval(refresh, 3000);
})();
