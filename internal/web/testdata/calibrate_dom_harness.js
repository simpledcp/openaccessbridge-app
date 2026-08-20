/* A minimal DOM shim for internal/web/assets/calibrate.js (§6.4).
 *
 * Go tests can prove the API emits a field. They cannot prove the SCREEN renders
 * it honestly — a page whose script paints a green tick for an unmeasured input
 * passes every server-side test there is. So the real calibrate.js is executed
 * here, handed hand-built frames, and the resulting text is printed for the Go
 * test to assert on.
 *
 * argv[2] = path to calibrate.js
 * stdin   = JSON array of CalibrationReport frames
 * stdout  = JSON array of {id: {text, class}} snapshots, one per frame
 */
"use strict";

var fs = require("fs");

function Node(tag) {
  this.tag = tag || "div";
  this.children = [];
  this.className = "";
  this.hidden = false;
  this.value = "";
  this.style = {};
  this._text = "";
}
Object.defineProperty(Node.prototype, "textContent", {
  // Real DOM keeps the text node a textContent assignment created when children
  // are appended afterwards, and calibrate.js relies on that (a meter row sets
  // its label, then appends the provenance chip). Concatenate, do not replace.
  get: function () {
    return this._text + this.children.map(function (c) { return c.textContent; }).join("");
  },
  set: function (v) { this._text = String(v); this.children = []; }
});
Object.defineProperty(Node.prototype, "innerHTML", {
  get: function () { return ""; },
  set: function () { this.children = []; this._text = ""; }
});
Node.prototype.appendChild = function (c) { this.children.push(c); return c; };
Node.prototype.addEventListener = function () {};

function TextNode(t) { Node.call(this, "#text"); this._text = String(t); }
TextNode.prototype = Object.create(Node.prototype);

var nodes = {};
global.document = {
  getElementById: function (id) {
    if (!nodes[id]) nodes[id] = new Node("div");
    return nodes[id];
  },
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (t) { return new TextNode(t); }
};
global.window = { addEventListener: function () {} };
// The wizard must not reach the network from the harness: any fetch/EventSource
// it starts is a promise that never settles, which is exactly what we want.
global.fetch = function () { return { then: function () { return this; }, catch: function () { return this; } }; };
global.EventSource = function () { this.close = function () {}; };

var src = fs.readFileSync(process.argv[2], "utf8");
// eslint-disable-next-line no-eval
(0, eval)(src);

var frames = JSON.parse(fs.readFileSync(0, "utf8"));
var out = frames.map(function (f) {
  window.__calibrate.render(f);
  var snap = {};
  Object.keys(nodes).forEach(function (id) {
    snap[id] = { text: nodes[id].textContent, class: nodes[id].className };
  });
  return snap;
});
process.stdout.write(JSON.stringify(out));
