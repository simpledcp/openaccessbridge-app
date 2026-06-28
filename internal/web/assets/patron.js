/* CaptionBridge patron caption client (§7).
 * Connects to the caption fan-out over WebSocket (primary) with an SSE fallback,
 * renders the current cue + a calm patron-facing status, and never shows
 * diagnostics. No internet, no app install. */
(function () {
  "use strict";

  var captionEl = document.getElementById("caption");
  var messageEl = document.getElementById("message");
  var stateEl = document.getElementById("connstate");
  var audEl = document.getElementById("auditorium");
  var langSel = document.getElementById("lang");

  // Plain-language patron status copy (§7.2). Never technical.
  var STATUS_TEXT = {
    waiting_for_show: { msg: "Captions are waiting for the show to start.", cls: "waiting" },
    temporarily_reconnecting: { msg: "Connection lost. Reconnecting…", cls: "waiting" },
    no_captions_for_content: { msg: "No captions for this part of the program.", cls: "" },
    unavailable: { msg: "Captions are not currently available. Please contact theater staff.", cls: "error" },
    active: { msg: "", cls: "" }
  };

  var params = new URLSearchParams(location.search);
  var lang = params.get("lang") || (navigator.language || "en");

  function setConn(label, cls) {
    stateEl.textContent = label;
    stateEl.className = "state state-" + cls;
  }

  function render(state) {
    if (state.auditorium) audEl.textContent = "Auditorium: " + state.auditorium;
    var st = STATUS_TEXT[state.status] || STATUS_TEXT.unavailable;
    if (state.visible && state.text) {
      captionEl.textContent = state.text;
      messageEl.textContent = "";
      messageEl.className = "message";
    } else {
      captionEl.textContent = "";
      messageEl.textContent = st.msg;
      messageEl.className = "message " + st.cls;
    }
  }

  // --- Wake lock (best-effort; requires a secure context, §6.7) ---
  var wakeLock = null;
  async function requestWakeLock() {
    try {
      if ("wakeLock" in navigator) wakeLock = await navigator.wakeLock.request("screen");
    } catch (e) { /* not available over plain HTTP; ignore */ }
  }
  document.addEventListener("visibilitychange", function () {
    if (wakeLock !== null && document.visibilityState === "visible") requestWakeLock();
  });
  requestWakeLock();

  // --- Transport: WebSocket primary, SSE fallback ---
  var backoff = 500;
  function wsURL() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + "/api/caption/ws?lang=" + encodeURIComponent(lang);
  }

  var ws = null;
  function connectWS() {
    setConn("Connecting…", "connecting");
    try { ws = new WebSocket(wsURL()); }
    catch (e) { return connectSSE(); }
    ws.onopen = function () { setConn("Connected", "connected"); backoff = 500; };
    ws.onmessage = function (ev) {
      try { render(JSON.parse(ev.data)); } catch (e) {}
    };
    ws.onclose = function () {
      setConn("Reconnecting…", "reconnecting");
      scheduleReconnect(connectWS);
    };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
  }

  var es = null;
  function connectSSE() {
    setConn("Connecting…", "connecting");
    try { es = new EventSource("/api/caption/stream?lang=" + encodeURIComponent(lang)); }
    catch (e) { return scheduleReconnect(connectWS); }
    es.onopen = function () { setConn("Connected", "connected"); backoff = 500; };
    es.onmessage = function (ev) { try { render(JSON.parse(ev.data)); } catch (e) {} };
    es.onerror = function () {
      setConn("Reconnecting…", "reconnecting");
      try { es.close(); } catch (e) {}
      scheduleReconnect(connectWS);
    };
  }

  function scheduleReconnect(fn) {
    backoff = Math.min(backoff * 2, 8000);
    setTimeout(fn, backoff);
  }

  function switchLanguage(newLang) {
    lang = newLang;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "setLanguage", language: lang }));
    } else if (es) {
      try { es.close(); } catch (e) {}
      connectSSE();
    }
  }

  // --- Language list ---
  fetch("/api/caption/languages").then(function (r) { return r.json(); }).then(function (langs) {
    if (!Array.isArray(langs) || !langs.length) { langs = [lang]; }
    langSel.innerHTML = "";
    langs.forEach(function (l) {
      var o = document.createElement("option");
      o.value = l.code || l; o.textContent = l.label || l.code || l;
      if ((l.code || l) === lang) o.selected = true;
      langSel.appendChild(o);
    });
  }).catch(function () {
    var o = document.createElement("option"); o.value = lang; o.textContent = lang;
    langSel.appendChild(o);
  });
  langSel.addEventListener("change", function () { switchLanguage(langSel.value); });

  // --- Text size + theme controls ---
  var fs = 6;
  function applyFS() { document.documentElement.style.setProperty("--fs", fs + "vmin"); }
  document.getElementById("bigger").addEventListener("click", function () { fs = Math.min(fs + 1, 14); applyFS(); });
  document.getElementById("smaller").addEventListener("click", function () { fs = Math.max(fs - 1, 3); applyFS(); });
  document.getElementById("theme").addEventListener("click", function () {
    document.body.classList.toggle("theme-light");
    document.body.classList.toggle("theme-dark");
  });

  connectWS();
})();
