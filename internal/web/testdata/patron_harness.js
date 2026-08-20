"use strict";
/* A minimal browser stand-in for internal/web/assets/patron.js.
 *
 * It is deliberately small: just enough DOM, transport and timer surface for the
 * patron client's own logic (transport fallback, show-end, language substitution,
 * the audio picker state machine and the buffer control). It simulates NOTHING
 * about a real phone, a real network or real media — those stay under BLOCKERS
 * B2 — and no assertion built on it claims otherwise. The RTCPeerConnection
 * stand-in below simulates the API shape the page drives, never real media: the
 * real peer is proven against a real appliance in
 * cmd/captionbridge-core/audio_e2e_test.go.
 */

const path = require("path");

function makeEl(id, tag) {
  const el = {
    id: id || "",
    tagName: tag || "div",
    textContent: "",
    className: "",
    value: "",
    autoplay: false,
    volume: 1,
    disabled: false,
    srcObject: null,
    options: [],
    children: [],
    selectedIndex: -1,
    dataset: {},
    attrs: {},
    listeners: {},
    style: { setProperty() {} },
    classList: { toggle() {}, add() {}, remove() {} },
    addEventListener(ev, fn) {
      (this.listeners[ev] = this.listeners[ev] || []).push(fn);
    },
    setAttribute(k, v) { this.attrs[k] = v; },
    getAttribute(k) { return this.attrs[k]; },
    appendChild(c) {
      this.children.push(c);
      if (c.tagName === "option") this.options.push(c);
      return c;
    },
    fire(ev, arg) {
      (this.listeners[ev] || []).forEach(function (f) {
        f(arg || { preventDefault() {} });
      });
    },
  };
  Object.defineProperty(el, "innerHTML", {
    get() { return ""; },
    set(v) { if (v === "") { this.children = []; this.options = []; } },
  });
  return el;
}

function makeSocketClass(registry) {
  return class FakeSocket {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.sent = [];
      registry.push(this);
    }
    send(data) { this.sent.push(JSON.parse(data)); }
    close() {
      if (this.readyState === 3) return;
      this.readyState = 3;
      if (this.onclose) this.onclose();
    }
    // --- test drivers ---
    open() { this.readyState = 1; if (this.onopen) this.onopen(); }
    deliver(obj) { if (this.onmessage) this.onmessage({ data: JSON.stringify(obj) }); }
    fail() { if (this.onerror) this.onerror(); this.close(); }
  };
}

/* A stand-in for the browser's RTCPeerConnection. It simulates the API SHAPE the
 * patron client drives — offer/answer, trickled candidates, receivers for the
 * §6.7 buffer control, and an ontrack the test fires — and nothing about real
 * media, real ICE or a real phone (BLOCKER B2). What it makes testable is the
 * page's own behaviour once audio arrives. */
function makePeerClass(registry) {
  return class FakePeerConnection {
    constructor(config) {
      this.config = config;
      this.transceivers = [];
      this.receivers = [];
      this.candidates = [];
      this.localDescription = null;
      this.remoteDescription = null;
      this.closed = false;
      registry.push(this);
    }
    addTransceiver(kind, init) {
      this.transceivers.push({ kind: kind, init: init });
      this.receivers.push({});
    }
    getReceivers() { return this.receivers; }
    createOffer() { return Promise.resolve({ type: "offer", sdp: "v=0\r\nfake-offer\r\n" }); }
    setLocalDescription(d) { this.localDescription = d; return Promise.resolve(); }
    setRemoteDescription(d) { this.remoteDescription = d; return Promise.resolve(); }
    addIceCandidate(c) { this.candidates.push(c); return Promise.resolve(); }
    close() { this.closed = true; }
    // --- test drivers ---
    fireTrack(ev) { if (this.ontrack) this.ontrack(ev || { streams: [{}] }); }
    fireCandidate(c) { if (this.onicecandidate) this.onicecandidate({ candidate: c }); }
  };
}

/* load() builds the environment, requires a fresh copy of patron.js, and returns
 * the client's test surface together with the stubs that drive it. */
function load(opts) {
  opts = opts || {};
  const mode = opts.mode || "accessibility";
  const wantCaptions = mode !== "listen";
  const wantAudio = mode !== "caption";

  const ids = ["connstate", "auditorium", "message"];
  if (wantCaptions) ids.push("caption", "lang", "bigger", "smaller");
  if (wantAudio) ids.push("audiopanel", "audiostatus", "streampicker", "volume", "buffer");
  ids.push("theme");

  const els = {};
  ids.forEach(function (id) { els[id] = makeEl(id); });

  const body = makeEl("body", "body");
  body.dataset.mode = mode;
  if (wantCaptions) body.dataset.captions = "1";
  if (wantAudio) body.dataset.audio = "1";

  const timers = [];
  const sockets = [];
  const sources = [];
  const fetches = [];
  const peers = [];

  const env = {
    body,
    els,
    sockets,
    sources,
    fetches,
    timers,
    reloaded: 0,
    runTimers() {
      const due = timers.splice(0, timers.length);
      due.forEach(function (t) { t.fn(); });
      return due.length;
    },
    // captionSockets/audioSockets split the two control channels by URL.
    captionSockets() { return sockets.filter(function (s) { return s.url.indexOf("/api/caption/ws") >= 0; }); },
    audioSockets() { return sockets.filter(function (s) { return s.url.indexOf("/api/audio/ws") >= 0; }); },
    peers() { return peers; },
  };

  const sandbox = {
    document: {
      body: body,
      documentElement: makeEl("html", "html"),
      getElementById(id) { return els[id] || null; },
      createElement(tag) { return makeEl("", tag); },
      addEventListener() {},
      visibilityState: "visible",
    },
    location: { protocol: "http:", host: "10.10.0.1:8080", search: opts.search || "", reload() { env.reloaded++; } },
    navigator: {},
    WebSocket: makeSocketClass(sockets),
    EventSource: makeSocketClass(sources),
    RTCPeerConnection: makePeerClass(peers),
    MediaStream: function MediaStream(tracks) { this.tracks = tracks || []; },
    fetch(url) {
      fetches.push(url);
      const body = (opts.fetchResponses || {})[url];
      if (body === undefined) return Promise.reject(new Error("no stub for " + url));
      return Promise.resolve({ ok: true, json() { return Promise.resolve(body); } });
    },
    setTimeout(fn, ms) { timers.push({ fn: fn, ms: ms }); return timers.length; },
    clearTimeout() {},
    URLSearchParams: URLSearchParams,
    Date: Date,
    Math: Math,
    JSON: JSON,
    isNaN: isNaN,
    parseInt: parseInt,
    console: console,
  };

  // Run patron.js with the sandbox as its globals. `module` is provided, which is
  // what makes the client export its test surface.
  const fs = require("fs");
  const vm = require("vm");
  const src = fs.readFileSync(path.join(__dirname, "..", "assets", "patron.js"), "utf8");
  const context = vm.createContext(sandbox);
  const module_ = { exports: {} };
  sandbox.module = module_;
  sandbox.exports = module_.exports;
  sandbox.globalThis = sandbox;
  vm.runInContext(src, context, { filename: "patron.js" });

  env.client = module_.exports;
  env.sandbox = sandbox;
  return env;
}

module.exports = { load, makeEl };
