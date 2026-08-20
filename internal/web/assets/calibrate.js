/* CaptionBridge audio calibration wizard (§6.4). Vanilla, offline, no build step
 * (DECISIONS D4). Talks to /api/admin/calibration/*.
 *
 * The one rule this file exists to keep: nothing it renders may look like a
 * measurement of the selected input unless the server said it was one. Every
 * figure is drawn next to its provenance, and a run that measured nothing renders
 * "not measured" rather than a level or a verdict.
 */
(function () {
  "use strict";

  var el = function (id) { return document.getElementById(id); };
  var devices = [];
  var stream = null;   // the live EventSource (§6.4 step 5)
  var latest = null;   // the last frame the meter delivered

  /* ---- §6.4 steps 1-3: the pickers -------------------------------------- */

  function selectedDevice() {
    for (var i = 0; i < devices.length; i++) {
      if (devices[i].id === el("device").value) return devices[i];
    }
    return null;
  }

  function option(value, label) {
    var o = document.createElement("option");
    o.value = value;
    o.textContent = label;
    return o;
  }

  function fillDevices(data) {
    devices = data.devices || [];
    var sel = el("device");
    sel.innerHTML = "";
    if (!devices.length) {
      sel.appendChild(option("", "no capture input detected"));
    }
    devices.forEach(function (d) {
      sel.appendChild(option(d.id, d.friendly_name || d.id));
    });

    var uses = el("use");
    uses.innerHTML = "";
    (data.uses || []).forEach(function (u) { uses.appendChild(option(u.value, u.label)); });

    var tiers = el("tier");
    tiers.innerHTML = "";
    (data.tiers || []).forEach(function (t) { tiers.appendChild(option(t.value, t.label)); });

    // "No sound subsystem" and "no inputs attached" are different faults with the
    // same symptom; the wizard must not collapse them into an empty dropdown.
    var err = el("device-error");
    var msg = data.helper_error || data.enumeration_error || "";
    err.hidden = !msg;
    err.textContent = msg;

    el("auditorium").value = data.default_auditorium_id || "";
    var saved = data.saved_profile;
    if (saved) {
      if (saved.auditorium_id) el("auditorium").value = saved.auditorium_id;
      el("gain-notes").value = saved.gain_notes || "";
      if (saved.input_device_id) sel.value = saved.input_device_id;
    }
    onDeviceChange();
    if (saved && saved.channel_mapping) el("mapping").value = saved.channel_mapping;
  }

  function onDeviceChange() {
    var d = selectedDevice();
    var note = el("device-note");
    if (!d) {
      note.textContent = "No input selected.";
      fillMappings();
      return;
    }
    var bits = [];
    bits.push("identifier: " + d.id + (d.stable ? " (stable across reboots)" : " (NOT reboot-stable)"));
    bits.push(d.channels_known
      ? "channels: " + d.channels
      : "channels: unknown until the device is opened");
    if (!d.measurable) {
      bits.push("CANNOT BE MEASURED HERE — " + (d.detail || "no capture backend for this device"));
    }
    note.textContent = bits.join(" · ");
    fillMappings();
  }

  /* §6.5: the picker offers only the mappings the detected hardware can satisfy.
   * The server computes that list, so the rule has one home. */
  function fillMappings() {
    var d = selectedDevice();
    var use = el("use").value;
    var sel = el("mapping");
    sel.innerHTML = "";
    var modes = (d && d.modes && d.modes[use]) || [];
    if (!modes.length) {
      sel.appendChild(option("", "no mapping this input can satisfy for that use"));
      el("mapping-note").textContent =
        "This input cannot serve the selected use. Choose another use, or a device with more channels.";
      return;
    }
    modes.forEach(function (m) { sel.appendChild(option(m.value, m.label)); });
    el("mapping-note").textContent =
      d && !d.channels_known
        ? "This device does not report a channel count until it is opened, so the mapping is confirmed by the meter below."
        : "";
  }

  /* ---- §6.4 step 5: the live meter -------------------------------------- */

  function params() {
    return "device_id=" + encodeURIComponent(el("device").value) +
      "&channel_mapping=" + encodeURIComponent(el("mapping").value) +
      "&use=" + encodeURIComponent(el("use").value) +
      "&input_tier=" + encodeURIComponent(el("tier").value);
  }

  function start() {
    stopStream();
    if (!el("mapping").value) {
      el("meter-state").textContent = "select a mapping first";
      return;
    }
    el("meter-state").textContent = "measuring…";
    stream = new EventSource("/api/admin/calibration/meter?" + params());
    stream.onmessage = function (ev) {
      try { render(JSON.parse(ev.data)); } catch (e) { /* a malformed frame renders nothing */ }
    };
    stream.onerror = function () {
      el("meter-state").textContent = "meter connection lost — retrying";
    };
  }

  function stopStream() {
    if (stream) { stream.close(); stream = null; }
  }

  function stop() {
    stopStream();
    el("meter-state").textContent = "stopped";
    fetch("/api/admin/calibration/stop", { method: "POST" }).catch(function () {});
  }

  /* ---- rendering -------------------------------------------------------- */

  function provenanceClass(kind) {
    return kind === "hardware" ? "hardware" : (kind === "synthetic" ? "synthetic" : "none");
  }

  function renderProvenance(r) {
    var box = el("provenance");
    box.hidden = false;
    box.className = "provenance " + provenanceClass(r.source_kind);
    box.textContent = "";
    var head = document.createElement("span");
    if (r.source_kind === "hardware") {
      head.textContent = "Measured from " + (r.measured_device_id || "the selected input") + ".";
    } else if (r.source_kind === "synthetic") {
      head.textContent = "SYNTHETIC TEST SOURCE — every figure below measures a generated tone, not an audio input.";
    } else {
      head.textContent = "Nothing is being measured.";
    }
    box.appendChild(head);
    var sub = document.createElement("span");
    sub.className = "sub";
    sub.textContent = r.source_detail ||
      (r.source_kind === "hardware" ? "" : (r.error || "Start the meter to take a measurement."));
    if (r.source_kind === "synthetic" && r.requested_device_id && r.requested_device_id !== r.measured_device_id) {
      sub.textContent = "The selected input (" + r.requested_device_id + ") was NOT measured. " + sub.textContent;
    }
    box.appendChild(sub);
  }

  function chip(kind) {
    var c = document.createElement("span");
    c.className = "chip " + provenanceClass(kind);
    c.textContent = kind === "hardware" ? "hardware" : (kind === "synthetic" ? "synthetic" : "not measured");
    return c;
  }

  /* Map a dBFS reading onto the bar. -60 dBFS is the bottom of the scale; the
   * qualifier's own floor sentinel sits far below that and pins to zero width. */
  function barPercent(dbfs) {
    if (dbfs == null || !isFinite(dbfs)) return 0;
    var pct = (dbfs + 60) / 60 * 100;
    return Math.max(0, Math.min(100, pct));
  }

  function renderMeters(r) {
    var box = el("meters");
    box.textContent = "";
    if (!r.measured) {
      var p = document.createElement("p");
      p.className = "muted";
      // Running but nothing qualified yet is "starting", not a fault — and a run
      // that is not running at all says why, in the server's own words.
      p.textContent = r.error || (r.running ? "Starting the capture — no reading yet." : "Not measured.");
      box.appendChild(p);
      return;
    }
    (r.streams || []).forEach(function (st) {
      var row = document.createElement("div");
      row.className = "meter";

      var name = document.createElement("div");
      name.className = "name";
      name.textContent = st.stream_type + "  ← " + (st.source_channel || "?");
      name.appendChild(document.createTextNode(" "));
      name.appendChild(chip(r.source_kind));
      row.appendChild(name);

      var bar = document.createElement("div");
      bar.className = "bar";
      var level = document.createElement("div");
      level.className = "level" + (st.clipping ? " hot" : (st.silence ? " low" : ""));
      level.style.width = barPercent(st.level_dbfs) + "%";
      bar.appendChild(level);
      var floor = document.createElement("div");
      floor.className = "floor";
      floor.style.left = barPercent(st.noise_floor_dbfs) + "%";
      bar.appendChild(floor);
      row.appendChild(bar);

      var fig = document.createElement("div");
      fig.className = "figures";
      fig.textContent =
        "level " + fmtDb(st.level_dbfs) + " · noise floor " + fmtDb(st.noise_floor_dbfs) +
        " · DC offset " + (st.dc_offset != null ? st.dc_offset.toFixed(3) : "—") +
        " · " + fmtCount(st.sample_rate, " Hz") + " / " + fmtCount(st.channels, " ch") +
        " · dropouts " + (st.dropouts || 0);
      row.appendChild(fig);

      var flags = [];
      if (st.clipping) flags.push("CLIPPING");
      if (st.silence) flags.push("SILENT");
      if (!st.present) flags.push("INPUT NOT PRESENT");
      if (!st.device_connected) flags.push("DEVICE DISCONNECTED");
      if (flags.length) {
        var f = document.createElement("div");
        f.className = "flags";
        f.textContent = flags.join(" · ");
        row.appendChild(f);
      }
      box.appendChild(row);
    });
  }

  function fmtDb(v) {
    if (v == null || !isFinite(v) || v <= -120) return "not measurable";
    return v.toFixed(1) + " dBFS";
  }

  /* Sample rate and channel width are 0 when no analysis window has closed yet.
   * "0 Hz / 0 ch" is a reading nobody took printed as a figure, and 0 channels
   * is exactly the value §6.1 forbids rendering as if it were a detected shape.
   * Say "not measured" and let the installer keep the signal playing. */
  function fmtCount(v, unit) {
    if (!v) return "not measured";
    return v + unit;
  }

  /* §6.4 step 6: what was detected, per stream, in the qualifier's own words. */
  function renderDefects(r) {
    var box = el("defects");
    box.textContent = "";
    // §6.5's caveat about the mapping belongs on the screen whether or not the
    // mapping was judged impossible: "this device feeds one stream; pair a
    // second device for the other" is carried on a FEASIBLE mapping, and hiding
    // it behind !ok meant the one sentence telling an installer they have
    // calibrated half a pair never rendered at all.
    if (r.feasibility && r.feasibility.reason) {
      box.appendChild(muted(r.feasibility.reason));
    }
    if (!r.measured) {
      box.appendChild(muted(r.error || "Nothing measured yet."));
      return;
    }
    var any = false;
    (r.streams || []).forEach(function (st) {
      (st.reasons || []).forEach(function (why) {
        any = true;
        var p = document.createElement("p");
        p.className = "reasons";
        p.textContent = st.stream_type + ": " + why;
        box.appendChild(p);
      });
    });
    if (!any) box.appendChild(muted("No defects detected on the measured signal."));
  }

  /* §6.4 step 7: a CONCRETE fix, ordered blocking → important → advisory. */
  function renderRecs(r) {
    var box = el("recs");
    box.textContent = "";
    var recs = r.recommendations || [];
    if (!recs.length) {
      box.appendChild(muted(r.measured ? "No changes needed." : "Nothing measured yet."));
      return;
    }
    recs.forEach(function (rec) {
      var d = document.createElement("div");
      d.className = "rec " + (rec.severity || "advisory");
      var a = document.createElement("div");
      a.className = "action";
      a.textContent = rec.action;
      var why = document.createElement("div");
      why.className = "why";
      why.textContent = rec.why;
      d.appendChild(a);
      d.appendChild(why);
      box.appendChild(d);
    });
  }

  /* §6.4 step 8: the explicit verdict and the plain-language summary block. */
  function renderVerdict(r) {
    var v = el("verdict");
    if (!r.measured) {
      v.className = "verdict none";
      v.textContent = r.running ? "measuring…" : "not measured";
      el("summary").textContent = r.error ||
        (r.running ? "Starting the capture — no reading yet." : "Not measured.");
    } else {
      // The GREEN pill is the one thing on this page a hurried booth tech reads,
      // so it is reserved for a verdict measured on the selected hardware. A
      // synthetic run really did measure something and keeps its verdict text,
      // but it must not paint the same green tick a qualified input does —
      // an unverified state that looks identical to a verified one is the whole
      // defect class this screen exists to close.
      var ok = (r.verdict === "excellent" || r.verdict === "usable") &&
        r.source_kind === "hardware";
      var bad = r.verdict !== "excellent" && r.verdict !== "usable" && r.verdict !== "marginal";
      v.className = "verdict " + (ok ? "ok" : (bad ? "bad" : "warn"));
      v.textContent = r.source_kind === "hardware"
        ? r.verdict
        : r.verdict + " (of the test source — this input is unqualified)";
      el("summary").textContent = (r.summary_lines || []).join("\n");
    }
    var vp = el("verdict-provenance");
    vp.className = "chip " + provenanceClass(r.source_kind);
    vp.textContent = r.source_kind === "hardware"
      ? "measured on hardware"
      : (r.source_kind === "synthetic" ? "synthetic source — not a qualification of any input" : "nothing measured");

    el("qualify-note").textContent = r.qualified
      ? "This verdict will be recorded as this input's qualification."
      : (r.qualified_detail || "No verdict will be recorded.");
  }

  function muted(text) {
    var p = document.createElement("p");
    p.className = "muted";
    p.textContent = text;
    return p;
  }

  function render(r) {
    latest = r;
    el("meter-state").textContent = r.running
      ? "measuring — " + r.measured_ms + " ms" + (r.settled ? "" : " (settling)")
      : "not running";
    var merr = el("meter-error");
    merr.hidden = !r.error;
    merr.textContent = r.error || "";
    renderProvenance(r);
    renderMeters(r);
    renderDefects(r);
    renderRecs(r);
    renderVerdict(r);
  }

  /* ---- §6.4 step 9: save ------------------------------------------------ */

  function save() {
    var body = {
      auditorium_id: el("auditorium").value,
      device_id: el("device").value,
      channel_mapping: el("mapping").value,
      use: el("use").value,
      input_tier: el("tier").value,
      gain_notes: el("gain-notes").value
    };
    fetch("/api/admin/calibration/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (resp) {
      return resp.json().then(function (j) { return { ok: resp.ok, body: j }; });
    }).then(function (out) {
      var note = el("save-note");
      if (!out.ok || out.body.error) {
        note.className = "loud";
        note.textContent = out.body.error || "The profile could not be saved.";
        return;
      }
      note.className = out.body.qualified ? "" : "chip synthetic";
      note.textContent = out.body.note || "Saved.";
    }).catch(function () {
      el("save-note").className = "loud";
      el("save-note").textContent = "The profile could not be saved.";
    });
  }

  /* ---- wiring ----------------------------------------------------------- */

  el("device").addEventListener("change", onDeviceChange);
  el("use").addEventListener("change", fillMappings);
  el("start").addEventListener("click", start);
  el("stop").addEventListener("click", stop);
  el("save").addEventListener("click", save);
  window.addEventListener("beforeunload", stopStream);

  fetch("/api/admin/calibration/devices")
    .then(function (r) { return r.json(); })
    .then(fillDevices)
    .catch(function () {
      var err = el("device-error");
      err.hidden = false;
      err.textContent = "The appliance did not answer the device list request.";
    });

  // Exposed for the page test, which asserts the elements this script needs exist.
  window.__calibrate = { render: render, latest: function () { return latest; } };
})();
