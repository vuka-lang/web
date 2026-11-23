// The keyed morph of live.js in node, over dom.js: node keyed.js live.js
// [-write]. Each step's markup, which elements it kept (by the mark each
// element got when it came) and the focus must be keyed.golden.json's,
// written by the runtime before keyed lookups were indexed: the index
// changes what the morph costs, never what it does.
"use strict";
const fs = require("fs");
const path = require("path");
const { page } = require("./dom.js");
const file = process.argv[2];
const li = (k, t, extra = "") => `<li data-vk-key="${k}"${extra}>${t}</li>`;
const root = (inner) => `<div data-vk-id="c1">${inner}</div>`;
const out = [];
let p;
let mi = 0;
function mark() { for (const e of p.doc.querySelectorAll("*")) if (!e.__m) e.__m = "m" + ++mi + ":" + e.tagName; }
function state(what) {
  out.push({ what, html: p.$("[data-vk-id=c1]").outerHTML, marks: p.doc.querySelectorAll("[data-vk-id=c1] *").map((e) => e.__m || "new"), focus: p.doc.activeElement && p.doc.activeElement.__m || null });
}
function patch(inner) { p.server({ type: "patch", updates: [{ id: "c1", html: root(inner) }] }); mark(); }
function open(inner) { p = page(file, root(inner), { live: true }); p.ws().onopen(); p.server({ type: "render", ref: 1, html: root(inner) }); mark(); }
const ul = (rows) => `<ul>${rows.join("")}</ul>`;

// loop keys: inserts at the front, the middle and the end, a move, a removal, in one message
open(ul([1, 2, 3, 4, 5, 6].map((k) => li(k, "r" + k))));
patch(ul([li("a", "ra"), li(1, "r1"), li(5, "r5 moved"), li(2, "r2"), li("b", "rb"), li("c", "rc"), li(4, "r4"), li(6, "r6"), li("d", "rd"), li("e", "re")]));
state("loop: insert/move/remove");
patch(ul([li("e", "re"), li("d", "rd"), li(6, "r6"), li(4, "r4"), li("c", "rc"), li("b", "rb"), li(2, "r2"), li(5, "r5"), li(1, "r1"), li("a", "ra")]));
state("loop: reversed");
patch(ul([li("x", "rx"), li("y", "ry")]));
state("loop: all replaced");
patch(ul([]));
state("loop: cleared");
patch(ul(Array.from({ length: 30 }, (_, i) => li("n" + i, "n" + i))));
state("loop: created from empty");

// ids: duplicates on the page, a duplicate in the message, an id the server changed
open(`<p id="x">x1</p><p id="x">x2</p><p id="y">y</p><p id="z">z</p><span>u</span>`);
patch(`<p id="y">y</p><p id="x">x</p><p id="w">w (was z)</p><span>u</span>`);
state("ids: duplicate on the page, id changed");
patch(`<p id="n">n1</p><p id="n">n2</p><p id="w">w</p><p id="y">y</p><span>u</span>`);
state("ids: the same new id twice in one message");
patch(`<p id="w">w</p><p id="n">n</p><span>u</span><p id="y">y</p>`);
state("ids: after the duplicate");

// data-vk-ignore and focus in moving rows
open(ul([li(1, "r1"), `<li id="keep" data-vk-ignore>kept</li>`, li(2, `<input id="f">`), li(3, "r3"), li(4, "r4")]));
p.$("#keep").textContent = "the browser's";
p.$("#f").focus();
patch(ul([li(4, "r4"), li(3, "r3"), `<li id="keep" data-vk-ignore>server</li>`, li("new", "rnew"), li(2, `<input id="f">`), li(1, "r1")]));
state("ignore + focus: reordered");
patch(ul([li(2, `<input id="f">`), li("new2", "n2"), li(1, "r1"), `<li id="keep" data-vk-ignore>server</li>`]));
state("ignore + focus: focused row first");

// instance roots keyed by data-vk-id beside unkeyed nodes
open(`<section><div data-vk-id="c2">two</div>text<div data-vk-id="c3">three</div><b>b</b></section>`);
patch(`<section><div data-vk-id="c4">four</div><div data-vk-id="c3">three</div>text<b>b</b><div data-vk-id="c2">two</div></section>`);
state("instances: inserted, moved");
const golden = path.join(__dirname, "keyed.golden.json"), got = JSON.stringify(out, null, 1) + "\n";
if (process.argv[3] === "-write") fs.writeFileSync(golden, got);
else if (got !== fs.readFileSync(golden, "utf8")) {
  const want = JSON.parse(fs.readFileSync(golden, "utf8"));
  for (let i = 0; i < Math.max(out.length, want.length); i++) {
    if (JSON.stringify(out[i]) !== JSON.stringify(want[i])) console.log("FAIL " + (want[i] || out[i]).what + "\n   got  " + JSON.stringify(out[i]) + "\n   want " + JSON.stringify(want[i]));
  }
  process.exit(1);
}
console.log(out.length + " keyed steps as before");
