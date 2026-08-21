/* CaptionBridge first-boot setup wizard (§8.1). Talks to /api/admin/setup/*. */
(function () {
  "use strict";
  function post(path, body) {
    return fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }
  function setStatus(step, ok, msg) {
    var el = document.querySelector('.status[data-step="' + step + '"]');
    if (!el) return;
    el.textContent = (ok ? "✓ " : "✗ ") + (msg || "");
    el.style.color = ok ? "#0a0" : "#b00020";
  }
  function refresh() {
    fetch("/api/admin/setup/state").then(function (r) { return r.json(); }).then(function (s) {
      setStatus("credentials", s.has_credentials && !s.has_default_credential, s.has_credentials ? "set" : "not set");
      setStatus("network", s.network_configured, s.network_configured ? "assigned" : "not assigned");
      setStatus("server", s.server_configured, s.server_configured ? "configured" : "not configured");
      setStatus("profile", s.profile_selected, s.profile_selected ? "selected" : "not selected");
      setStatus("complete", s.complete, s.complete ? "setup complete" : "");
      var exp = document.getElementById("expiry");
      if (s.complete) {
        exp.textContent = "Setup is complete. Setup mode is closed.";
      } else if (s.setup_mode_active) {
        exp.textContent = "Setup mode auto-closes in " + Math.floor(s.expires_in_sec / 60) + "m " + (s.expires_in_sec % 60) + "s.";
      } else {
        exp.textContent = "Setup mode window has elapsed.";
      }
    });
  }
  function handle(promise, step) {
    promise.then(function (r) {
      if (r.ok) { setStatus(step, true, "saved"); refresh(); }
      else { r.text().then(function (t) { setStatus(step, false, t); }); }
    }).catch(function () { setStatus(step, false, "error"); });
  }

  document.getElementById("cred-save").addEventListener("click", function () {
    handle(post("/api/admin/setup/credentials", {
      username: document.getElementById("cred-user").value,
      password: document.getElementById("cred-pass").value,
    }), "credentials");
  });
  document.getElementById("net-save").addEventListener("click", function () {
    handle(post("/api/admin/setup/network", {
      theaterIface: document.getElementById("net-theater").value,
      clientIface: document.getElementById("net-client").value,
    }), "network");
  });
  document.getElementById("srv-save").addEventListener("click", function () {
    handle(post("/api/admin/setup/server", {
      auditorium_id: document.getElementById("srv-aud").value,
      cinema_server_ip: document.getElementById("srv-ip").value,
      expected_screen_name: document.getElementById("srv-name").value,
      // §8.5's remaining pins. Sent even when blank so clearing one is possible:
      // a stale pin from a previous room is exactly what this guard is for.
      expected_server_fingerprint: document.getElementById("srv-fp").value,
      physical_port_label: document.getElementById("srv-port").value,
      // §7.2: the room name patrons see. Without it every appliance shows the
      // same build-time default, which is the confusion §8.5 exists to catch.
      auditorium_name: document.getElementById("srv-room").value,
    }), "server");
  });
  document.getElementById("prof-save").addEventListener("click", function () {
    handle(post("/api/admin/setup/profile", { profile: document.getElementById("prof-select").value }), "profile");
  });
  document.getElementById("finish").addEventListener("click", function () {
    post("/api/admin/setup/complete", {}).then(function (r) {
      if (r.ok) { setStatus("complete", true, "setup complete"); document.getElementById("result").textContent = "Setup complete. You may now serve patrons."; }
      else { r.text().then(function (t) { setStatus("complete", false, t); }); }
      refresh();
    });
  });

  refresh();
  setInterval(refresh, 5000);
})();
