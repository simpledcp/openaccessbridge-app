/* OpenAccessBridge patron client (§7).
 *
 * One script serves all three §7.1 routes; the server decides which controls
 * exist in the document, and this file drives whichever are present:
 *   /caption        captions only
 *   /listen         HI / VI-N audio only
 *   /accessibility  both, switchable without a reload
 *
 * It renders the current cue, the always-visible connection state and room
 * identity, the audio picker/volume/buffer controls, and calm non-technical error
 * states. It never shows a diagnostic: no address, interface, device, log line or
 * asset path ever reaches this page (§7.2).
 *
 * The WebRTC peer on the appliance is REAL (pion — DECISIONS D20): this page
 * offers, the appliance answers, and Opus arrives. Two things are still not
 * real, and this page must keep saying so rather than letting the working
 * player imply otherwise:
 *
 *  - with no capture hardware attached (BLOCKER B3) the audio is a generated
 *    test tone, which the server marks `test_tone` and sourceNote() states
 *    plainly while it plays;
 *  - no phone has ever run this page (BLOCKER B2), so autoplay policy, mDNS
 *    candidates, Wake Lock and the jitter buffer that dominates §6.7's latency
 *    budget are all unexercised. Nothing here claims otherwise.
 *
 * When the appliance cannot serve audio at all, the server says so and this page
 * shows that sentence rather than drawing a player that cannot play.
 */
(function () {
  "use strict";

  var body = document.body;
  var MODE = (body && body.dataset && body.dataset.mode) || "accessibility";
  var WANT_CAPTIONS = !!(body && body.dataset && body.dataset.captions);
  var WANT_AUDIO = !!(body && body.dataset && body.dataset.audio);

  var el = function (id) { return document.getElementById(id); };
  var captionEl = el("caption");
  var messageEl = el("message");
  var stateEl = el("connstate");
  var gradeEl = el("captiongrade");
  var langSel = el("lang");

  // Plain-language patron status copy (§7.2). Never technical.
  var STATUS_TEXT = {
    waiting_for_show: { msg: "Captions are waiting for the show to start.", cls: "waiting" },
    temporarily_reconnecting: { msg: "Connection lost. Reconnecting…", cls: "waiting" },
    no_captions_for_content: { msg: "No captions for this part of the program.", cls: "" },
    unavailable: { msg: "Captions are not currently available. Please contact theater staff.", cls: "error" },
    // §7.2 show-end behavior: a polite ending, not a dangling "waiting" state.
    show_ended: { msg: "This show has ended. Thank you for joining us.", cls: "" },
    active: { msg: "", cls: "" }
  };

  var params = new URLSearchParams(location.search);
  var lang = params.get("lang") || "";
  var showOver = false;

  function setConn(label, cls) {
    if (!stateEl) return;
    stateEl.textContent = label;
    stateEl.className = "state state-" + cls;
  }

  function setMessage(text, cls) {
    if (!messageEl) return;
    messageEl.textContent = text || "";
    messageEl.className = "message " + (cls || "");
  }

  function render(state) {
    var st = STATUS_TEXT[state.status] || STATUS_TEXT.unavailable;

    /* §5.2 item 5 / §5.6: a reel or composition with no track for this language.
     * The server has always sent this and the page never read it, so on an
     * uncaptioned reel between captioned reels a patron saw an empty black box
     * and nothing else — indistinguishable, from their seat, from a phone that
     * had stopped working. Mid-show, in the dark, that is the judgement they are
     * left to make.
     *
     * It is checked BEFORE the status, because the link is legitimately "active"
     * during an uncaptioned segment: nothing is wrong, there is simply nothing
     * to show, and that is a different thing to say. */
    if (state.language_available === false) {
      if (captionEl) captionEl.textContent = "";
      setMessage(STATUS_TEXT.no_captions_for_content.msg, STATUS_TEXT.no_captions_for_content.cls);
      renderGrade(state);
      return;
    }

    if (captionEl && state.visible && state.text) {
      captionEl.textContent = state.text;
      setMessage("", "");
    } else {
      if (captionEl) captionEl.textContent = "";
      setMessage(st.msg, st.cls);
    }
    renderGrade(state);
  }

  /* §5.4: "If only subtitle tracks exist, the patron-facing copy should say
   * 'Subtitles available' rather than 'Closed captions'."
   *
   * The distinction is not cosmetic. A subtitle track carries dialogue; a
   * caption-grade track carries speaker names, sound effects and music cues —
   * the information a deaf or hard-of-hearing patron is relying on to follow a
   * film. Presenting one as the other misleads exactly the patron this product
   * exists for, so when the track is not caption-grade the page says so and
   * says what is missing. Silence is the correct rendering for a caption-grade
   * track: a note on every show would be read by nobody. */
  function renderGrade(state) {
    if (!gradeEl) return;
    if (state.language_available === false || !state.status || state.status === "unavailable") {
      gradeEl.textContent = "";
      return;
    }
    if (state.caption_grade === false) {
      gradeEl.textContent = "Subtitles — this show has no closed-caption track, " +
        "so speaker names and sound effects may not appear.";
      return;
    }
    gradeEl.textContent = "";
  }

  // The show is over: stop reconnecting, say so politely, and stay put (§7.2).
  function showEnded() {
    showOver = true;
    if (captionEl) captionEl.textContent = "";
    setMessage(STATUS_TEXT.show_ended.msg, "");
    setConn("Show ended", "ended");
    closeTransports();
    if (audioWS) { try { audioWS.close(); } catch (e) {} audioWS = null; }
  }

  // --- Wake lock (best-effort; requires a secure context, §6.7/D12) ---
  var wakeLock = null;
  function requestWakeLock() {
    try {
      if (navigator && "wakeLock" in navigator) {
        navigator.wakeLock.request("screen").then(function (l) { wakeLock = l; }, function () {});
      }
    } catch (e) { /* not available over plain HTTP; ignore */ }
  }
  document.addEventListener("visibilitychange", function () {
    if (wakeLock !== null && document.visibilityState === "visible") requestWakeLock();
  });
  requestWakeLock();

  // --- Join by short code (§9.4 "per-show access code or signed QR token") ---
  var joinForm = el("joinform");
  if (joinForm) {
    joinForm.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var errEl = el("joinerror");
      var code = (el("joincode") || {}).value || "";
      fetch("/api/patron/join", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ code: code })
      }).then(function (r) {
        return r.json().then(function (b) { return { ok: r.ok, body: b }; });
      }).then(function (res) {
        if (res.ok) { location.reload(); return; }
        if (errEl) { errEl.textContent = (res.body && res.body.error) || "That code was not recognized."; errEl.className = "message error"; }
      }).catch(function () {
        if (errEl) { errEl.textContent = "Could not reach this room's system. Please contact theater staff."; errEl.className = "message error"; }
      });
    });
  }

  /* ------------------------------------------------------------------
   * Caption transport: WebSocket primary, SSE fallback (§5.6).
   *
   * The fallback used to be unreachable: it ran only if the WebSocket
   * CONSTRUCTOR threw, which essentially never happens. Every realistic failure
   * (handshake refused, a proxy stripping Upgrade, a middlebox dropping ws:)
   * arrives as onerror→onclose, so the page retried WebSocket forever and the
   * patron sat on "Reconnecting…". A connection that closes BEFORE it ever
   * opened now counts as a failed transport, and after WS_FAIL_LIMIT of those we
   * move to SSE and stay there, re-probing WebSocket occasionally in case the
   * network changed.
   * ------------------------------------------------------------------ */
  var WS_FAIL_LIMIT = 2;
  var WS_REPROBE_MS = 60000;
  var backoff = 500;
  var transport = "ws";
  var wsFailures = 0;
  var sseSince = 0;
  var ws = null;
  var es = null;

  function captionQuery() {
    return lang ? "?lang=" + encodeURIComponent(lang) : "";
  }

  function wsURL() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + "/api/caption/ws" + captionQuery();
  }

  function closeTransports() {
    if (ws) { try { ws.close(); } catch (e) {} ws = null; }
    if (es) { try { es.close(); } catch (e) {} es = null; }
  }

  function handleFrame(msg) {
    if (!msg || typeof msg !== "object") return;
    if (msg.type === "show_ended") { showEnded(); return; }
    if (msg.type === "language") { applyLanguageFrame(msg); return; }
    render(msg);
  }

  function connect() {
    if (showOver) return;
    // A transport already exists (or is mid-handshake). Reconnect timers must be
    // idempotent, or a language switch plus a pending backoff timer would leave
    // two live subscriptions and one orphaned socket holding a fan-out slot.
    if (ws || es) return;
    if (transport === "sse" && Date.now() - sseSince > WS_REPROBE_MS) {
      transport = "ws";
      wsFailures = 0;
    }
    if (transport === "ws") connectWS(); else connectSSE();
  }

  function connectWS() {
    setConn("Connecting…", "connecting");
    var opened = false;
    var sock;
    try { sock = new WebSocket(wsURL()); }
    catch (e) { switchToSSE(); return; }
    ws = sock;
    sock.onopen = function () { opened = true; wsFailures = 0; setConn("Connected", "connected"); backoff = 500; };
    sock.onmessage = function (ev) {
      try { handleFrame(JSON.parse(ev.data)); } catch (e) {}
    };
    sock.onclose = function () {
      // A socket we already replaced (browsers fire onclose asynchronously, so a
      // superseded socket can close after its successor is live). It must not
      // clear or reschedule the current transport.
      if (ws !== sock) return;
      ws = null;
      if (showOver) return;
      if (!opened) {
        wsFailures++;
        if (wsFailures >= WS_FAIL_LIMIT) { switchToSSE(); return; }
      }
      setConn("Reconnecting…", "reconnecting");
      scheduleReconnect();
    };
    sock.onerror = function () { try { sock.close(); } catch (e) {} };
  }

  function switchToSSE() {
    transport = "sse";
    sseSince = Date.now();
    connectSSE();
  }

  function connectSSE() {
    setConn("Connecting…", "connecting");
    var src;
    try { src = new EventSource("/api/caption/stream" + captionQuery()); }
    catch (e) { scheduleReconnect(); return; }
    es = src;
    src.onopen = function () { setConn("Connected", "connected"); backoff = 500; };
    src.onmessage = function (ev) { try { handleFrame(JSON.parse(ev.data)); } catch (e) {} };
    src.onerror = function () {
      if (es !== src) return; // superseded stream; see connectWS
      try { src.close(); } catch (e) {}
      es = null;
      if (showOver) return;
      setConn("Reconnecting…", "reconnecting");
      scheduleReconnect();
    };
  }

  function scheduleReconnect() {
    backoff = Math.min(backoff * 2, 8000);
    setTimeout(connect, backoff);
  }

  /* --- Language (§7.1, DECISIONS D8) ---------------------------------
   * The server negotiates the language (en-US matches an en track) and reports
   * back what it is actually serving. If a patron's explicit choice is not
   * carried by this show, they are told — a substitution is never silent.
   */
  function applyLanguageFrame(msg) {
    if (msg.serving) {
      lang = msg.serving;
      if (langSel) selectOption(langSel, lang);
    }
    if (msg.substituted) {
      setMessage("These captions are not available in " + msg.requested +
        ". Showing " + msg.serving + " instead.", "waiting");
    }
    if (msg.available && langSel && langSel.options && langSel.options.length === 0) {
      fillLanguages(msg.available.map(function (c) { return { code: c, label: c }; }));
    }
  }

  function selectOption(sel, value) {
    if (!sel.options) return;
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value === value) { sel.selectedIndex = i; return; }
    }
  }

  function fillLanguages(langs) {
    if (!langSel) return;
    langSel.innerHTML = "";
    langs.forEach(function (l) {
      var o = document.createElement("option");
      o.value = l.code || l;
      o.textContent = l.label || l.code || l;
      langSel.appendChild(o);
    });
    if (lang) selectOption(langSel, lang);
  }

  function switchLanguage(newLang) {
    lang = newLang;
    if (ws && ws.readyState === 1) {
      ws.send(JSON.stringify({ type: "setLanguage", language: lang }));
    } else {
      closeTransports();
      connect();
    }
  }

  if (WANT_CAPTIONS && langSel) {
    fetch("/api/caption/languages").then(function (r) { return r.json(); }).then(function (langs) {
      if (Array.isArray(langs) && langs.length) fillLanguages(langs);
    }).catch(function () {});
    langSel.addEventListener("change", function () { switchLanguage(langSel.value); });
  }

  /* --- Text size + theme (§7.2 accessibility-first display) --- */
  var fs = 6;
  function applyFS() { document.documentElement.style.setProperty("--fs", fs + "vmin"); }
  var bigger = el("bigger"), smaller = el("smaller"), themeBtn = el("theme");
  if (bigger) bigger.addEventListener("click", function () { fs = Math.min(fs + 1, 14); applyFS(); });
  if (smaller) smaller.addEventListener("click", function () { fs = Math.max(fs - 1, 3); applyFS(); });
  if (themeBtn) themeBtn.addEventListener("click", function () {
    body.classList.toggle("theme-light");
    body.classList.toggle("theme-dark");
  });

  /* ------------------------------------------------------------------
   * Assistive audio (§6.7, §7.2): picker, volume, and the "add a little buffer
   * if choppy" control.
   * ------------------------------------------------------------------ */
  var audioWS = null;
  var audioDoc = { streams: [], playback_available: false, playback_reason: "" };
  var selected = null;
  var pc = null;          // RTCPeerConnection, once a peer exists
  var audioEl = null;     // the <audio> sink, created on the tap-to-start gesture
  var volume = 0.8;

  // §6.7 receive-side tuning: the jitter buffer is the dominant, least
  // controllable latency term, so the patron gets a direct trade — latency for
  // stability. These are the target playout delays in seconds, not measurements.
  var BUFFER_SECONDS = [0, 0.15, 0.4, 0.8];
  function bufferSeconds(level) {
    var i = parseInt(level, 10);
    if (isNaN(i) || i < 0) i = 0;
    if (i >= BUFFER_SECONDS.length) i = BUFFER_SECONDS.length - 1;
    return BUFFER_SECONDS[i];
  }
  var bufferLevel = 1;

  function applyBuffer() {
    if (!pc || !pc.getReceivers) return;
    var target = bufferSeconds(bufferLevel);
    pc.getReceivers().forEach(function (r) {
      // Two spellings exist across browsers; set whichever is present.
      try { if ("jitterBufferTarget" in r) r.jitterBufferTarget = target * 1000; } catch (e) {}
      try { if ("playoutDelayHint" in r) r.playoutDelayHint = target; } catch (e) {}
    });
  }

  function applyVolume() { if (audioEl) audioEl.volume = volume; }

  /* §7.2 "Connection state, always visible". The indicator is driven by the
   * CAPTION transport, and /listen opens no caption transport at all — so a
   * patron listening to audio read "Connecting…" for the whole show while sound
   * came out of their headphones. A status light that is wrong in the reassuring
   * direction is worse than none: it is the one thing a patron checks when they
   * think something is broken.
   *
   * On a route with captions the caption transport stays the authority (it is
   * the one that reconnects, and two writers would fight). On an audio-only
   * route the audio path owns it. */
  function audioConnState(label, cls) {
    if (MODE !== "listen") return;
    setConn(label, cls);
  }

  function setAudioStatus(text, cls) {
    var n = el("audiostatus");
    if (!n) return;
    n.textContent = text || "";
    n.className = "message " + (cls || "");
  }

  function renderPicker() {
    var host = el("streampicker");
    if (!host) return;
    host.innerHTML = "";
    audioDoc.streams.forEach(function (s) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "streambtn" + (selected === s.stream ? " selected" : "") + (s.available ? "" : " off");
      b.textContent = s.label;
      b.setAttribute("aria-pressed", selected === s.stream ? "true" : "false");
      if (!s.available) {
        b.disabled = true;
        b.setAttribute("aria-disabled", "true");
      }
      b.addEventListener("click", function () { chooseStream(s.stream); });
      host.appendChild(b);
    });
    if (!audioDoc.streams.length) {
      setAudioStatus("Assistive audio is not offered in this auditorium right now.", "waiting");
    } else if (!audioDoc.playback_available) {
      setAudioStatus(audioDoc.playback_reason || "Listening is not available right now.", "waiting");
    } else if (!selected) {
      setAudioStatus("Choose a feed, then tap to start.", "");
    } else if (sourceNote()) {
      // A selected feed that is playing still says what it is playing.
      setAudioStatus(sourceNote(), "waiting");
    }
  }

  function streamByName(name) {
    for (var i = 0; i < audioDoc.streams.length; i++) {
      if (audioDoc.streams[i].stream === name) return audioDoc.streams[i];
    }
    return null;
  }

  /* What the selected feed IS, in a patron's words. Two things can be true of a
   * feed that is playing perfectly well, and a listener has to be told both:
   *
   *  - test_tone: the appliance has no capture hardware attached, so this
   *    stream carries a generated tone rather than the auditorium's audio
   *    (BLOCKER B3). Someone handed a sine wave with no explanation would
   *    reasonably decide the accessibility service is broken and give up;
   *  - caution: the input qualified only Marginal, which §6.3 says to offer
   *    WITH the warning rather than silently.
   *
   * Neither sentence names a device, a level or a reason string (§7.2). */
  function sourceNote() {
    var s = streamByName(selected);
    if (!s) return "";
    var parts = [];
    if (s.test_tone) {
      parts.push("You are hearing a test tone, not this auditorium's audio.");
    }
    if (s.caution && s.reason) parts.push(s.reason);
    return parts.join(" ");
  }

  function chooseStream(name) {
    var s = streamByName(name);
    if (!s) return;
    if (!s.available) {
      setAudioStatus(s.reason || "That feed is not available right now.", "error");
      audioConnState("Unavailable", "error");
      return;
    }
    if (!audioDoc.playback_available) {
      setAudioStatus(audioDoc.playback_reason || "Listening is not available right now.", "waiting");
      return;
    }
    selected = name;
    renderPicker();
    // D12: plain HTTP means autoplay needs a user gesture. This click IS the
    // gesture, so the sink is created here and never before.
    ensureSink();
    sendAudio({ type: "select", stream: name });
    startPeer(name);
  }

  function ensureSink() {
    if (audioEl) return audioEl;
    audioEl = document.createElement("audio");
    audioEl.autoplay = true;
    audioEl.setAttribute("playsinline", "");
    applyVolume();
    var host = el("audiopanel");
    if (host) host.appendChild(audioEl);
    return audioEl;
  }

  function sendAudio(obj) {
    if (audioWS && audioWS.readyState === 1) {
      try { audioWS.send(JSON.stringify(obj)); } catch (e) {}
    }
  }

  /* startPeer builds the browser side of the §6.7 peer. It is receive-only, uses
   * no STUN/TURN (the appliance and the phone are one hop apart on the patron
   * LAN, host candidates only), and sends its offer over the same local socket.
   * If the browser has no WebRTC at all we say so calmly instead of failing mute.
   */
  function startPeer(stream) {
    if (typeof RTCPeerConnection === "undefined") {
      setAudioStatus("This browser cannot play the assistive audio feed. Please contact theater staff.", "error");
      return;
    }
    try {
      if (pc) { try { pc.close(); } catch (e) {} }
      pc = new RTCPeerConnection({ iceServers: [] });
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.ontrack = function (ev) {
        ensureSink().srcObject = ev.streams && ev.streams[0] ? ev.streams[0] : new MediaStream([ev.track]);
        applyVolume();
        applyBuffer();
        // Audio is flowing: clear the connecting state, but never clear the
        // statement of what this feed actually is.
        setAudioStatus(sourceNote(), sourceNote() ? "waiting" : "");
        audioConnState("Connected", "connected");
      };
      pc.onicecandidate = function (ev) {
        if (!ev.candidate) return;
        sendAudio({
          type: "ice", stream: stream,
          candidate: ev.candidate.candidate,
          sdp_mid: ev.candidate.sdpMid,
          sdp_m_line_index: ev.candidate.sdpMLineIndex || 0
        });
      };
      pc.createOffer().then(function (offer) {
        return pc.setLocalDescription(offer).then(function () {
          sendAudio({ type: "offer", stream: stream, sdp: offer.sdp });
        });
      }).catch(function () {
        setAudioStatus("Listening could not be started. Please contact theater staff.", "error");
      });
    } catch (e) {
      setAudioStatus("Listening could not be started. Please contact theater staff.", "error");
      audioConnState("Unavailable", "error");
    }
  }

  function handleAudioFrame(msg) {
    if (!msg || typeof msg !== "object") return;
    switch (msg.type) {
      case "audio_state":
        audioDoc = {
          streams: msg.streams || [],
          playback_available: !!msg.playback_available,
          playback_reason: msg.playback_reason || ""
        };
        var lostReason = "";
        if (selected && !((streamByName(selected) || {}).available)) {
          var lost = streamByName(selected);
          selected = null;
          lostReason = (lost && lost.reason) || "That feed stopped. Please contact theater staff.";
        }
        // renderPicker() writes the idle hint when nothing is selected, so a loud
        // reason is applied after it, never before (guardrail #7).
        renderPicker();
        if (lostReason) setAudioStatus(lostReason, "error");
        break;
      case "unavailable":
        selected = null;
        renderPicker();
        setAudioStatus(msg.reason || "That feed is not available right now.", "error");
        break;
      case "answer":
        if (pc && msg.sdp) {
          pc.setRemoteDescription({ type: "answer", sdp: msg.sdp }).catch(function () {
            setAudioStatus("Listening could not be started. Please contact theater staff.", "error");
          });
        }
        break;
      case "ice":
        if (pc && msg.candidate) {
          pc.addIceCandidate({
            candidate: msg.candidate,
            sdpMid: msg.sdp_mid,
            sdpMLineIndex: msg.sdp_m_line_index
          }).catch(function () {});
        }
        break;
      case "show_ended":
        showEnded();
        break;
    }
  }

  var audioBackoff = 500;
  function connectAudio() {
    if (!WANT_AUDIO || showOver) return;
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    try { audioWS = new WebSocket(proto + "//" + location.host + "/api/audio/ws"); }
    catch (e) { pollAudioOnce(); return; }
    audioWS.onmessage = function (ev) {
      try { handleAudioFrame(JSON.parse(ev.data)); } catch (e) {}
    };
    audioWS.onclose = function () {
      audioWS = null;
      if (showOver) return;
      audioBackoff = Math.min(audioBackoff * 2, 8000);
      setTimeout(connectAudio, audioBackoff);
    };
    audioWS.onerror = function () { try { audioWS.close(); } catch (e) {} };
    audioWS.onopen = function () { audioBackoff = 500; };
  }

  // If the control socket cannot even be constructed, fall back to a one-shot
  // read of the availability document so the panel is honest rather than blank.
  function pollAudioOnce() {
    fetch("/api/audio/streams").then(function (r) { return r.json(); }).then(function (doc) {
      handleAudioFrame({
        type: "audio_state",
        streams: doc.streams,
        playback_available: doc.playback_available,
        playback_reason: doc.playback_reason
      });
    }).catch(function () {
      setAudioStatus("Assistive audio is not available right now. Please contact theater staff.", "error");
    });
  }

  var volEl = el("volume"), bufEl = el("buffer");
  if (volEl) volEl.addEventListener("input", function () {
    volume = Math.max(0, Math.min(100, parseInt(volEl.value, 10) || 0)) / 100;
    applyVolume();
  });
  if (bufEl) bufEl.addEventListener("change", function () {
    bufferLevel = parseInt(bufEl.value, 10) || 0;
    applyBuffer();
  });

  if (WANT_CAPTIONS) connect();
  if (WANT_AUDIO) connectAudio();

  // Test surface: present only under a CommonJS loader (the Node-driven tests in
  // internal/web). Browsers have no `module`, so nothing is exported there.
  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      bufferSeconds: bufferSeconds,
      handleFrame: handleFrame,
      handleAudioFrame: handleAudioFrame,
      chooseStream: chooseStream,
      state: function () {
        return {
          mode: MODE,
          transport: transport,
          wsFailures: wsFailures,
          showOver: showOver,
          lang: lang,
          selected: selected,
          bufferLevel: bufferLevel,
          volume: volume,
          audioDoc: audioDoc
        };
      }
    };
  }
})();
