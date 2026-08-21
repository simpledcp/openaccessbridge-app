"use strict";
/* Behaviour tests for internal/web/assets/patron.js, run by TestPatronClient in
 * internal/web/patron_client_test.go. Each case names the buildplan requirement it
 * defends. Nothing here asserts anything about a real phone, a real Wi-Fi network
 * or real audio — those remain BLOCKER B2. The real appliance path is proven
 * against real processes in cmd/captionbridge-core/audio_e2e_test.go.
 */

const assert = require("assert");
const { load } = require("./patron_harness.js");

const cases = [];
function test(name, fn) { cases.push({ name, fn }); }

/* §7.2 "Connection state, always visible" + §5.6's WS-primary / SSE-fallback pair.
 * The fallback used to be unreachable: connectSSE ran only if the WebSocket
 * CONSTRUCTOR threw, so a refused handshake retried WebSocket forever. */
test("a websocket that never opens falls back to SSE", () => {
  const env = load({ mode: "caption" });
  assert.strictEqual(env.captionSockets().length, 1, "no websocket attempt was made");
  assert.strictEqual(env.sources.length, 0, "SSE started before WebSocket failed");

  // First failure: closes before ever opening.
  env.captionSockets()[0].fail();
  env.runTimers();
  assert.strictEqual(env.sources.length, 0, "fell back after a single failure");
  assert.strictEqual(env.captionSockets().length, 2, "did not retry WebSocket once");

  // Second failure: the transport is judged unusable and SSE takes over.
  env.captionSockets()[1].fail();
  env.runTimers();
  assert.strictEqual(env.sources.length, 1, "SSE fallback never engaged");
  assert.strictEqual(env.client.state().transport, "sse");
});

test("a websocket that opens is never abandoned for SSE", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.close(); // a normal mid-show drop
  env.runTimers();
  assert.strictEqual(env.sources.length, 0, "a working transport was abandoned");
  assert.strictEqual(env.client.state().transport, "ws");
  assert.strictEqual(env.client.state().wsFailures, 0);
});

/* §7.2 show-end behavior: "the page shows a polite 'this show has ended' state
 * rather than dangling." Show end used to render as "waiting for the show to
 * start" with the connection open indefinitely. */
test("show end is polite, final, and stops reconnecting", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ type: "state", status: "waiting_for_show", visible: false });
  assert.match(env.els.message.textContent, /waiting for the show to start/i);

  ws.deliver({ type: "show_ended" });
  assert.match(env.els.message.textContent, /this show has ended/i);
  assert.strictEqual(env.client.state().showOver, true);
  assert.strictEqual(env.els.connstate.className, "state state-ended");

  const before = env.captionSockets().length;
  env.runTimers();
  assert.strictEqual(env.captionSockets().length, before, "kept reconnecting after the show ended");
});

/* Reconnect must be idempotent. A language switch on a closed transport plus a
 * pending backoff timer would otherwise leave two live subscriptions, one of them
 * orphaned and still holding a fan-out slot (D11 caps those). */
test("a language switch while disconnected does not double-connect", () => {
  const env = load({ mode: "caption" });
  env.captionSockets()[0].fail(); // closed, one reconnect timer pending
  env.els.lang.value = "es-419";
  env.els.lang.fire("change");
  const afterSwitch = env.captionSockets().length;
  assert.strictEqual(afterSwitch, 2, "the language switch did not reconnect");
  env.runTimers();
  assert.strictEqual(env.captionSockets().length, afterSwitch, "the pending timer opened a second socket");
});

/* §7.1 language selection + DECISIONS D8. A substitution must be visible: a phone
 * asking for a language the show does not carry is told, not quietly switched. */
test("a substituted language is announced to the patron", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ type: "language", serving: "en", requested: "de", substituted: true, available: ["en"] });
  assert.match(env.els.message.textContent, /not available in de/i);
  assert.strictEqual(env.client.state().lang, "en");
});

test("a clean language negotiation says nothing at all", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ type: "language", serving: "en", available: ["en", "es-419"] });
  assert.strictEqual(env.els.message.textContent, "");
  assert.strictEqual(env.client.state().lang, "en");
});

/* §5.2 item 5 / §5.6: "reels or compositions with empty ReelResources must
 * render 'no captions for this segment' to the patron, not an error and not
 * silence."
 *
 * The server has always sent language_available:false and the page never read
 * it, so on an uncaptioned reel between captioned reels a patron saw an empty
 * black box and nothing else. From a seat, mid-show, that is indistinguishable
 * from a phone that has stopped working. */
test("an uncaptioned segment says so instead of going blank", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ status: "active", visible: true, text: "a line from the captioned reel",
    language: "en", caption_grade: true, language_available: true });
  assert.strictEqual(env.els.caption.textContent, "a line from the captioned reel");

  // The next reel carries no track for this language.
  ws.deliver({ status: "active", visible: false, language: "en", language_available: false });
  assert.strictEqual(env.els.caption.textContent, "");
  assert.match(env.els.message.textContent, /no captions for this part/i);
});

test("captions coming back clears the uncaptioned-segment message", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ status: "active", visible: false, language: "en", language_available: false });
  assert.match(env.els.message.textContent, /no captions for this part/i);

  ws.deliver({ status: "active", visible: true, text: "captions resume",
    language: "en", caption_grade: true, language_available: true });
  assert.strictEqual(env.els.caption.textContent, "captions resume");
  assert.strictEqual(env.els.message.textContent, "");
});

/* §5.4: "If only subtitle tracks exist, the patron-facing copy should say
 * 'Subtitles available' rather than 'Closed captions', so a patron relying on
 * caption-grade information is not misled." A subtitle track carries dialogue; a
 * caption-grade one carries speaker names, sound effects and music cues — which
 * is what a deaf patron is relying on. */
test("a subtitle-grade track says what is missing from it", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ status: "active", visible: true, text: "dialogue only",
    language: "fr", kind: "subtitle", caption_grade: false, language_available: true });
  assert.match(env.els.captiongrade.textContent, /subtitles/i);
  assert.match(env.els.captiongrade.textContent, /speaker names|sound effects/i);
});

test("a caption-grade track says nothing at all", () => {
  const env = load({ mode: "caption" });
  const ws = env.captionSockets()[0];
  ws.open();
  ws.deliver({ status: "active", visible: true, text: "[door slams] Who's there?",
    language: "en", kind: "closed-caption", caption_grade: true, language_available: true });
  assert.strictEqual(env.els.captiongrade.textContent, "",
    "a note on every show would be read by nobody");
});

/* §7.2: "Connection state, always visible: connected, reconnecting…, waiting for
 * show to start, unavailable."
 *
 * The indicator was driven only by the CAPTION transport, and /listen opens no
 * caption transport — so a patron listening to audio read "Connecting…" for the
 * whole show while sound was coming out of their headphones. Wrong in the
 * reassuring direction, on the one element a patron checks when they think
 * something is broken. */
test("a listening patron's connection state follows the audio, not the captions", () => {
  const env = load({ mode: "listen" });
  // The harness starts elements empty; the real page ships "Connecting…" in the
  // HTML, which is exactly what a listening patron used to be left staring at.
  assert.ok(!/connected/i.test(env.els.connstate.textContent), "nothing is connected yet");

  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  });
  env.client.chooseStream("HI");
  env.peers()[0].fireTrack();

  assert.match(env.els.connstate.textContent, /connected/i,
    "audio is playing and the indicator still says connecting");
});

test("an unavailable feed says unavailable rather than connecting forever", () => {
  const env = load({ mode: "listen" });
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: false,
      reason: "Hearing assistance is not currently available." }],
  });
  env.client.chooseStream("HI");
  assert.match(env.els.connstate.textContent, /unavailable/i);
});

/* On a route that has BOTH, the caption transport stays the authority. Two
 * writers would fight, and the way they would fight matters: audio reporting
 * "Connected" while the caption socket is reconnecting tells a deaf patron their
 * captions are fine at the exact moment they are not. */
test("on a captions route the caption transport keeps the indicator", () => {
  const env = load({ mode: "accessibility" });
  const ws = env.captionSockets()[0];
  ws.open();
  assert.match(env.els.connstate.textContent, /connected/i);

  // Captions drop. Audio keeps playing — and must not overwrite the warning.
  ws.close();
  assert.match(env.els.connstate.textContent, /reconnecting/i);

  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  });
  env.client.chooseStream("HI");
  if (env.peers().length) env.peers()[0].fireTrack();

  assert.match(env.els.connstate.textContent, /reconnecting/i,
    "audio overwrote the caption transport's state: a patron whose captions just " +
    "dropped would be told everything is connected");
});

/* §6.7 receive-side tuning: "a small per-patron 'audio is choppy → add a little
 * buffer' control that trades latency for stability". */
test("the buffer control trades latency for stability monotonically", () => {
  const env = load({ mode: "listen" });
  const b = env.client.bufferSeconds;
  assert.strictEqual(b(0), 0, "the fastest setting must add no buffer");
  assert.ok(b(1) > b(0) && b(2) > b(1) && b(3) > b(2), "buffer levels are not increasing");
  assert.strictEqual(b(99), b(3), "out-of-range levels must clamp, not wrap");
  assert.strictEqual(b("nonsense"), 0);

  env.els.buffer.value = "2";
  env.els.buffer.fire("change");
  assert.strictEqual(env.client.state().bufferLevel, 2);
});

test("volume is applied and clamped", () => {
  const env = load({ mode: "listen" });
  env.els.volume.value = "40";
  env.els.volume.fire("input");
  assert.strictEqual(env.client.state().volume, 0.4);
  env.els.volume.value = "500";
  env.els.volume.fire("input");
  assert.strictEqual(env.client.state().volume, 1);
});

/* §7.1 audio-feed selection + §7.2 error copy. An unavailable feed is shown as
 * unavailable with its plain-language reason, and cannot be started. */
test("the picker refuses an unavailable feed and shows its reason", () => {
  const env = load({ mode: "listen" });
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [
      { stream: "HI", label: "Hearing assistance", available: true },
      { stream: "VI-N", label: "Audio description", available: false, reason: "Audio description is not currently available. Please contact theater staff." },
    ],
  });
  assert.strictEqual(env.els.streampicker.children.length, 2);
  assert.strictEqual(env.els.streampicker.children[1].disabled, true, "an unavailable feed was left tappable");

  env.client.chooseStream("VI-N");
  assert.match(env.els.audiostatus.textContent, /not currently available/i);
  assert.strictEqual(env.client.state().selected, null);
});

/* The WebRTC peer is a SEAM (B4/B2). With no peer the page must say so, not draw
 * a player that cannot play. */
test("with no listening peer the page is honest and starts nothing", () => {
  const env = load({ mode: "listen" });
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: false,
    playback_reason: "Listening on your phone is not available in this auditorium right now. Please contact theater staff.",
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  });
  assert.match(env.els.audiostatus.textContent, /not available in this auditorium/i);

  env.client.chooseStream("HI");
  assert.strictEqual(env.client.state().selected, null, "a stream was selected with no peer to serve it");
  const audioWS = env.audioSockets()[0];
  assert.ok(!audioWS || audioWS.sent.length === 0, "an offer was sent with no peer available");
});

/* §7.2: a feed that dies mid-show must surface, not silently keep a dead
 * selection highlighted. */
test("a feed that goes away mid-show clears the selection loudly", () => {
  const env = load({ mode: "listen" });
  const good = {
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  };
  env.client.handleAudioFrame(good);
  env.client.chooseStream("HI");
  assert.strictEqual(env.client.state().selected, "HI");

  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: false, reason: "Hearing assistance audio signal is too low. Please contact theater staff." }],
  });
  assert.strictEqual(env.client.state().selected, null);
  assert.match(env.els.audiostatus.textContent, /too low/i);
});

/* §6.7: "SDP offer/answer + ICE are exchanged over the existing local web
 * server's WebSocket — no third-party signaling", and the local path is host
 * candidates only: "no internet STUN/TURN is expected or permitted". */
test("the offer goes over the local audio socket with no STUN or TURN", async () => {
  const env = load({ mode: "listen" });
  env.audioSockets()[0].open();
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  });
  env.client.chooseStream("HI");

  const pc = env.peers()[0];
  assert.ok(pc, "no peer connection was created for an available feed");
  // (Compared by length: the array is built inside the vm context, so it is an
  // Array from another realm and deepStrictEqual would reject it on prototype.)
  assert.ok(pc.config.iceServers, "the patron peer was given no ICE configuration at all");
  assert.strictEqual(pc.config.iceServers.length, 0, "the patron peer was given STUN/TURN servers");

  await Promise.resolve();
  await Promise.resolve();
  const sent = env.audioSockets()[0].sent;
  const offer = sent.filter(function (m) { return m.type === "offer"; })[0];
  assert.ok(offer, "no offer was sent over the appliance's own socket: " + JSON.stringify(sent));
  assert.strictEqual(offer.stream, "HI");
  assert.ok(offer.sdp, "the offer carried no SDP");

  // The appliance's answer, relayed back over the same socket, is applied.
  env.client.handleAudioFrame({ type: "answer", stream: "HI", sdp: "v=0\r\nfake-answer\r\n" });
  await Promise.resolve();
  assert.match(pc.remoteDescription.sdp, /fake-answer/);
});

/* THE HONESTY LINE (BLOCKER B3). A patron can really receive audio now, but on
 * an appliance with no capture hardware what they receive is a generated tone.
 * The page must keep saying so while it plays — clearing the status on ontrack
 * would quietly turn a test tone into "the auditorium's HI feed". */
test("a feed that is a test tone says so while it is playing", () => {
  const env = load({ mode: "listen" });
  env.audioSockets()[0].open();
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true, test_tone: true }],
  });
  env.client.chooseStream("HI");
  const pc = env.peers()[0];
  pc.fireTrack({ streams: [{}] });
  assert.match(env.els.audiostatus.textContent, /test tone/i);
  assert.doesNotMatch(env.els.audiostatus.textContent, /synthetic-|device|dbfs/i);
});

/* A real input carries no such note — the attribution must follow the source,
 * not decorate every stream. */
test("a real feed carries no test-tone note", () => {
  const env = load({ mode: "listen" });
  env.audioSockets()[0].open();
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{ stream: "HI", label: "Hearing assistance", available: true }],
  });
  env.client.chooseStream("HI");
  env.peers()[0].fireTrack({ streams: [{}] });
  assert.strictEqual(env.els.audiostatus.textContent, "");
});

/* §6.3: a Marginal input is offered WITH its warning, never silently. */
test("a cautioned feed keeps its warning visible while playing", () => {
  const env = load({ mode: "listen" });
  env.audioSockets()[0].open();
  env.client.handleAudioFrame({
    type: "audio_state",
    playback_available: true,
    streams: [{
      stream: "HI", label: "Hearing assistance", available: true,
      caution: true, reason: "This feed may be uneven right now.",
    }],
  });
  env.client.chooseStream("HI");
  env.peers()[0].fireTrack({ streams: [{}] });
  assert.match(env.els.audiostatus.textContent, /may be uneven/i);
});

/* §7.1: /caption is captions only and /listen is audio only — the client must not
 * open the other surface's socket. */
test("each route opens only its own transports", () => {
  const capt = load({ mode: "caption" });
  assert.strictEqual(capt.captionSockets().length, 1);
  assert.strictEqual(capt.audioSockets().length, 0, "/caption opened an audio socket");

  const listen = load({ mode: "listen" });
  assert.strictEqual(listen.captionSockets().length, 0, "/listen opened a caption socket");
  assert.strictEqual(listen.audioSockets().length, 1);

  const both = load({ mode: "accessibility" });
  assert.strictEqual(both.captionSockets().length, 1);
  assert.strictEqual(both.audioSockets().length, 1);
});

(async function run() {
  let failed = 0;
  for (const c of cases) {
    try {
      await c.fn();
      console.log("ok   - " + c.name);
    } catch (e) {
      failed++;
      console.log("FAIL - " + c.name + ": " + (e && e.message ? e.message : e));
    }
  }
  if (failed > 0) {
    console.log(failed + " patron client case(s) failed");
    process.exit(1);
  }
})();
