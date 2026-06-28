/* CaptionBridge admin dashboard (§8.2, §8.3). Renders caption + audio readiness
 * SEPARATELY and shouts the loud-warning set. Polls /api/admin/status. */
(function () {
  "use strict";

  function verdict(el, ok) {
    el.textContent = ok ? "READY" : "NOT READY";
    el.className = "verdict " + (ok ? "ok" : "bad");
  }
  function dl(el, rows) {
    el.innerHTML = "";
    rows.forEach(function (r) {
      var dt = document.createElement("dt"); dt.textContent = r[0];
      var dd = document.createElement("dd"); dd.textContent = r[1];
      if (r[2] === false) dd.className = "bad";
      if (r[2] === true) dd.className = "ok";
      el.appendChild(dt); el.appendChild(dd);
    });
  }
  function yn(b) { return b ? "yes" : "no"; }

  function render(s) {
    // Loud warnings (§8.3) — the dashboard must shout, not whisper.
    var banner = document.getElementById("warning-banner");
    var warns = s.warnings || [];
    if (warns.length) {
      banner.hidden = false;
      banner.innerHTML = warns.map(function (w) {
        return '<div class="warn-' + w.severity + '">⚠ ' + escapeHtml(w.message) + "</div>";
      }).join("");
    } else {
      banner.hidden = true;
    }

    verdict(document.getElementById("caption-verdict"), s.caption_ready);
    var cl = s.caption_link || {};
    dl(document.getElementById("caption-detail"), [
      ["Link state", cl.state || "—"],
      ["Server connected", yn(cl.state && cl.state !== "DISCONNECTED")],
      ["Lease active", yn(cl.lease_active), cl.lease_active],
      ["RPL loaded", yn(cl.rpl_loaded), cl.rpl_loaded],
      ["Assets loaded", yn(cl.assets_loaded), cl.assets_loaded],
      ["Output mode", cl.output_mode || "unknown"],
      ["Current language", cl.current_language || "—"],
      ["Languages", (cl.available_languages || []).join(", ") || "—"],
      ["Drift (ms)", cl.drift_ms_estimate != null ? cl.drift_ms_estimate : "—"],
    ]);

    verdict(document.getElementById("audio-verdict"), s.audio_ready);
    var ad = document.getElementById("audio-detail");
    ad.innerHTML = "";
    var inputs = s.audio_inputs || [];
    if (!inputs.length) {
      ad.textContent = "No audio inputs configured.";
    } else {
      inputs.forEach(function (a) {
        var d = document.createElement("div");
        d.className = "audio-row";
        var ok = a.status === "excellent" || a.status === "usable";
        d.innerHTML = "<strong>" + escapeHtml(a.stream_type) + "</strong> " +
          '<span class="verdict ' + (ok ? "ok" : "bad") + '">' + escapeHtml(a.status || "?") + "</span> " +
          "level " + (a.level_dbfs != null ? a.level_dbfs.toFixed(1) : "?") + " dBFS" +
          (a.clipping ? " · clipping" : "") + (a.silence ? " · silent" : "") +
          ((a.reasons && a.reasons.length) ? "<br><small>" + escapeHtml(a.reasons.join("; ")) + "</small>" : "");
        ad.appendChild(d);
      });
    }

    dl(document.getElementById("net-detail"), [
      ["IP forwarding", s.forwarding_disabled ? "disabled" : "ENABLED", s.forwarding_disabled],
      ["Firewall", s.firewall_active ? "active" : "INACTIVE", s.firewall_active],
      ["Bridges present", yn(s.bridges_present), !s.bridges_present],
      ["Admin isolated from patron", yn(s.admin_isolated), s.admin_isolated],
      ["Default credentials", yn(s.default_credentials_present), !s.default_credentials_present],
      ["SSH disabled", yn(s.ssh_disabled), s.ssh_disabled],
      ["Theater interface", s.theater_interface || "—"],
      ["Client interface", s.client_interface || "—"],
      ["Allowed cinema server", s.allowed_cinema_server || "—"],
    ]);
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  function refresh() {
    fetch("/api/admin/status").then(function (r) { return r.json(); }).then(render).catch(function () {});
  }
  refresh();
  setInterval(refresh, 3000);
})();
