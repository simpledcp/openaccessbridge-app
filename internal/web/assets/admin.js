/* CaptionBridge admin — minimal status poller. The full dashboard, setup wizard,
 * and loud-warning rendering are built in the admin milestone (§8). */
(function () {
  "use strict";
  function refresh() {
    fetch("/api/admin/status").then(function (r) { return r.json(); }).then(function (s) {
      document.getElementById("status").textContent = JSON.stringify(s, null, 2);
      var crit = (s.warnings || []).filter(function (w) { return w.severity === "critical"; });
      var banner = document.getElementById("warning-banner");
      if (crit.length) {
        banner.hidden = false;
        banner.textContent = "⚠ " + crit.map(function (w) { return w.message; }).join(" · ");
      } else {
        banner.hidden = true;
      }
    }).catch(function () {
      document.getElementById("status").textContent = "status unavailable";
    });
  }
  refresh();
  setInterval(refresh, 3000);
})();
