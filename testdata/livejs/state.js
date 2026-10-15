// Tests of live.js's client state (ui.State): bindings, batching, keys,
// struct fields, morphs, pushed commands and ui.Show's phases, in node over
// dom.js: node state.js live.js
"use strict";
const { page } = require("./dom.js");
const file = process.argv[2];
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const j = (v) => JSON.stringify(v).replace(/&/g, "&amp;").replace(/'/g, "&#39;");

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}
const open = (body, o) => page(file, body, o);
function joined(body, o = {}) {
  const p = open(body, { live: true, ...o });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: body });
  return p;
}
// writes counts each element's attribute and text writes from now on.
function writes(p) {
  const n = new Map(), E = p.window.Element.prototype;
  const count = (el, what) => { const k = (el.id || el.tagName) + " " + what; n.set(k, (n.get(k) || 0) + 1); };
  const set = E.setAttribute, rm = E.removeAttribute;
  E.setAttribute = function (a, v) { count(this, a); return set.call(this, a, v); };
  E.removeAttribute = function (a) { if (this.hasAttribute(a)) count(this, a); return rm.call(this, a); };
  const text = Object.getOwnPropertyDescriptor(p.window.Node.prototype, "textContent");
  Object.defineProperty(E, "textContent", { get: text.get, set(v) { count(this, "#text"); text.set.call(this, v); }, configurable: true });
  return (k) => n.get(k) || 0;
}

const s = (key, i, p) => ({ s: key, i, ...(p ? { p } : {}) });
const open1 = s("c1:open:1", false), tab = s("c1:tab:1", "logs");

test("a menu: toggle, attributes, text and classes follow", () => {
  const p = open(`<div>
    <button id="b" aria-expanded="false" data-vk-b-aria-expanded='${j(open1)}' data-vk-js-click='${j([["state_toggle", open1]])}'>Menu</button>
    <ul id="m" hidden data-vk-b-hidden='${j(["!", open1])}' class="menu" data-vk-b-class='${j(["class", { "is-open shadow": open1, menu: true }])}'></ul>
    <span id="t"><vk-t data-vk-b='${j(open1)}'>false</vk-t></span></div>`);
  p.fire("#b", "click");
  eq([p.$("#b").getAttribute("aria-expanded"), p.$("#m").hasAttribute("hidden"), p.$("#m").className, p.$("#t").textContent],
    ["true", false, "menu is-open shadow", "true"], "open");
  p.fire("#b", "click");
  eq([p.$("#b").getAttribute("aria-expanded"), p.$("#m").hasAttribute("hidden"), p.$("#m").className, p.$("#t").textContent],
    ["false", true, "menu", "false"], "closed");
});

test("operators", () => {
  const n = s("c1:n:1", 2), q = s("c1:q:1", "héllo");
  const p = open(`<div>
    <i id="a" data-vk-b-title='${j(["+", ["-", n, 1], ["neg", -10]])}'></i>
    <i id="b" data-vk-b-title='${j(["+", q, "!"])}'></i>
    <i id="c" data-vk-b-data-x='${j(["&&", [">=", n, 2], ["||", ["<", n, 0], ["==", ["len", q], 6]]])}'></i>
    <i id="d" data-vk-b-data-y='${j(["!=", { v: { a: 1 } }, { v: { a: 1 } }])}'></i>
    <button id="up" data-vk-js-click='${j([["state_inc", { ...n, by: 1 }]])}'></button>
    <button id="down" data-vk-js-click='${j([["state_inc", { ...n, by: -1 }], ["state_inc", { ...n, by: -1 }], ["state_inc", { ...n, by: -1 }]])}'></button></div>`);
  eq([p.$("#a").getAttribute("title"), p.$("#b").getAttribute("title"), p.$("#c").getAttribute("data-x"), p.$("#d").hasAttribute("data-y")],
    ["11", "héllo!", "", false], "at first (len counts UTF-8 bytes, as Go does)");
  p.fire("#up", "click");
  eq(p.$("#a").getAttribute("title"), "12", "inc");
  p.fire("#down", "click");
  eq([p.$("#a").getAttribute("title"), p.$("#c").hasAttribute("data-x")], ["9", false], "dec");
});

test("a chain's writes land together: one write per binding", () => {
  const p = open(`<div>
    <button id="b" data-vk-js-click='${j([["state_set", { ...tab, v: "env" }], ["state_set", { ...open1, v: true }], ["state_set", { ...tab, v: "logs" }], ["state_set", { ...tab, v: "env" }]])}'></button>
    <p id="x" data-vk-b-title='${j(["&&", open1, ["==", tab, "env"]])}'></p>
    <p id="y" data-vk-b-title='${j(tab)}' title="logs"></p></div>`);
  const w = writes(p);
  p.fire("#b", "click");
  eq([p.$("#x").getAttribute("title"), p.$("#y").getAttribute("title")], ["", "env"], "values");
  eq([w("x title"), w("y title")], [1, 1], "writes");
});

test("writes come before the chain's other steps and its push", () => {
  const p = joined(`<div data-vk-id="c1">
    <button id="b" data-vk-js-click='${j([["state_set", { ...tab, v: "env" }], ["add_class", { to: { sel: "#y" }, names: ["seen"] }], ["push", { h: "c1:0" }]])}'></button>
    <p id="y" data-vk-b-data-tab='${j(tab)}' data-tab="logs"></p></div>`);
  let at = null;
  const ws = p.ws(), send = ws.send;
  ws.send = function (m) { at = p.$("#y").getAttribute("data-tab"); return send.call(this, m); };
  p.fire("#b", "click");
  eq([at, p.$("#y").className, ws.sent[ws.sent.length - 1].target], ["env", "seen", "c1:0"]);
});

test("each instance its own state; the page's shared", () => {
  const a = s("c1:open:1", false), b = s("c2:open:1", false), g = s("g:theme", "light");
  const p = open(`<div>
    <div data-vk-id="c1"><button id="a" data-vk-js-click='${j([["state_toggle", a]])}'></button><ul id="am" hidden data-vk-b-hidden='${j(["!", a])}'></ul>
      <i id="ag" data-vk-b-title='${j(g)}' title="light"></i></div>
    <div data-vk-id="c2"><button id="b" data-vk-js-click='${j([["state_toggle", b]])}'></button><ul id="bm" hidden data-vk-b-hidden='${j(["!", b])}'></ul>
      <i id="bg" data-vk-b-title='${j(g)}' title="light"></i></div>
    <button id="dark" data-vk-js-click='${j([["state_set", { ...g, v: "dark" }]])}'></button></div>`);
  p.fire("#a", "click");
  eq([p.$("#am").hasAttribute("hidden"), p.$("#bm").hasAttribute("hidden")], [false, true], "one instance's");
  p.fire("#dark", "click");
  eq([p.$("#ag").getAttribute("title"), p.$("#bg").getAttribute("title")], ["dark", "dark"], "the page's");
});

test("a child reading its parent's state reads the same state", () => {
  const p = open(`<div data-vk-id="c1"><button id="b" data-vk-js-click='${j([["state_toggle", open1]])}'></button>
    <ul data-vk-id="c2" id="kid" hidden data-vk-b-hidden='${j(["!", open1])}'></ul></div>`);
  p.fire("#b", "click");
  eq(p.$("#kid").hasAttribute("hidden"), false);
});

test("a struct's fields: a field's write reaches its readers only", () => {
  const f = { s: "c1:f:1", i: { q: "", Sort: "name" } };
  const p = open(`<div>
    <input id="q" data-vk-b-value='${j({ ...f, p: ["q"] })}' value="">
    <p id="sort" data-vk-b-title='${j({ ...f, p: ["Sort"] })}' title="name"></p>
    <p id="all" data-vk-b-data-n='${j(["len", f])}' data-n="2"></p>
    <button id="setq" data-vk-js-click='${j([["state_set", { ...f, p: ["q"], v: "dog" }]])}'></button>
    <button id="setall" data-vk-js-click='${j([["state_set", { ...f, v: { q: "", Sort: "age", Page: 2 } }]])}'></button>
    <button id="reset" data-vk-js-click='${j([["state_set", { ...f, v: f.i }]])}'></button></div>`);
  const w = writes(p);
  p.fire("#setq", "click");
  eq([p.$("#q").value, p.$("#q").getAttribute("value"), p.$("#sort").getAttribute("title")], ["dog", "dog", "name"], "q");
  eq([w("q value"), w("sort title"), w("all data-n")], [1, 0, 0], "only q's reader, and the struct's (unchanged)");
  p.fire("#setall", "click");
  eq([p.$("#q").value, p.$("#sort").getAttribute("title"), p.$("#all").getAttribute("data-n")], ["", "age", "3"], "a whole set");
  p.fire("#reset", "click");
  eq([p.$("#sort").getAttribute("title"), p.$("#all").getAttribute("data-n")], ["name", "2"], "reset");
});

test("a morph keeps the browser's values", () => {
  const html = (n) => `<div data-vk-id="c1"><button id="b" data-vk-js-click='${j([["state_toggle", open1]])}'>${n}</button>
    <ul id="m" hidden data-vk-b-hidden='${j(["!", open1])}' class="menu" data-vk-b-class='${j(["class", { on: open1 }])}'></ul>
    <span id="t"><vk-t data-vk-b='${j(open1)}'>false</vk-t></span></div>`;
  const p = joined(html(0));
  p.fire("#b", "click");
  p.server({ type: "patch", ref: 2, updates: [{ id: "c1", html: html(1) }] });
  eq([p.$("#b").textContent, p.$("#m").hasAttribute("hidden"), p.$("#m").className, p.$("#t").textContent], ["1", false, "menu on", "true"]);
  p.fire("#b", "click");
  eq([p.$("#m").hasAttribute("hidden"), p.$("#t").textContent], [true, "false"], "and still follow");
});

test("the server sets a state with a pushed command", () => {
  const p = joined(`<div data-vk-id="c1"><p id="y" data-vk-b-data-tab='${j(tab)}' data-tab="logs"></p></div>`);
  p.server({ type: "patch", ref: 2, updates: [], js: [{ id: "c1", ops: [["state_set", { ...tab, v: "env" }]] }] });
  eq(p.$("#y").getAttribute("data-tab"), "env");
});

test("bindings refuse what commands refuse", () => {
  const u = s("c1:u:1", "/a");
  const p = open(`<a id="a" href="/a" data-vk-b-href='${j(u)}'></a><button id="b" data-vk-js-click='${j([["state_set", { ...u, v: "javascript:alert(1)" }]])}'></button>`);
  p.fire("#b", "click");
  eq(p.$("#a").getAttribute("href"), "/a");
});

// Show: a <vk-show> around its children, the phases on them.
const shown = s("c1:shown:1", false);
const showPage = (o, kid = `<div id="k" class="fade"></div>`) => open(`<div>
  <button id="t" data-vk-js-click='${j([["state_toggle", shown]])}'></button>
  <vk-show id="w" data-vk-show='${j(shown)}' hidden>${kid}</vk-show></div>`, o);
const styled = (dur) => ({ getComputedStyle: () => ({ transitionDuration: dur, transitionDelay: "0s", animationDuration: "0s", animationDelay: "0s" }) });
const state = (p) => [p.$("#w").hasAttribute("hidden"), p.$("#w").getAttribute("style"),
  ["data-closed", "data-enter", "data-leave"].filter((a) => p.$("#k").hasAttribute(a)).join(" ")];

test("show: data-closed at first, enter and leave phases on transitionend", () => {
  const p = showPage({ globals: styled("0.15s") });
  eq(state(p), [true, null, "data-closed"], "at first");
  p.fire("#t", "click");
  eq(state(p), [false, "display: contents", "data-closed data-enter"], "entering");
  p.tick(32);
  eq(state(p), [false, "display: contents", "data-enter"], "two frames later");
  p.fire("#k", "transitionend", { elapsedTime: 0.15 });
  eq(state(p), [false, "display: contents", ""], "entered");
  p.fire("#t", "click");
  eq(state(p), [false, "display: contents", "data-leave"], "leaving");
  p.tick(32);
  eq(state(p), [false, "display: contents", "data-closed data-leave"], "closing");
  p.fire("#k", "transitionend", { elapsedTime: 0.05 }); // another property's, shorter
  eq(state(p)[0], false, "not before the longest");
  p.fire("#k", "transitionend", { elapsedTime: 0.15 });
  eq(state(p), [true, null, "data-closed"], "hidden");
});

test("show: the timeout ends a phase no event ends", () => {
  const p = showPage({ globals: styled("150ms") });
  p.fire("#t", "click");
  p.tick(32);
  p.tick(150);
  eq(state(p)[2], "data-enter", "waiting");
  p.tick(60);
  eq(state(p), [false, "display: contents", ""], "entered");
});

test("show: no transition, reduced motion, and a toggle mid-phase", () => {
  let p = showPage();
  p.fire("#t", "click");
  p.tick(32);
  eq(state(p), [false, "display: contents", ""], "no transition: shown two frames later");
  p = showPage({ globals: { ...styled("1s"), matchMedia: (q) => ({ matches: /reduce/.test(q) }) } });
  p.fire("#t", "click");
  eq(state(p), [false, "display: contents", ""], "reduced motion: at once");
  p = showPage({ globals: styled("0.3s") });
  p.fire("#t", "click");
  p.tick(32);
  p.fire("#t", "click");
  p.tick(32);
  p.tick(400);
  eq(state(p), [true, null, "data-closed"], "the last toggle wins");
});

test("show: a popover is shown in the top layer", () => {
  const calls = [];
  const p = showPage({ globals: styled("0s") }, `<div id="k" popover></div>`);
  const k = p.$("#k");
  k.showPopover = () => calls.push("show");
  k.hidePopover = () => calls.push("hide");
  p.fire("#t", "click");
  p.tick(32);
  p.fire("#t", "click");
  p.tick(32);
  eq([calls, p.$("#w").hasAttribute("hidden")], [["show", "hide"], false]);
});

test("show: a morph keeps it open, with no transition", () => {
  const html = `<div data-vk-id="c1"><button id="t" data-vk-js-click='${j([["state_toggle", shown]])}'></button>
    <vk-show id="w" data-vk-show='${j(shown)}' hidden><div id="k"></div></vk-show></div>`;
  const p = joined(html, { globals: styled("0.2s") });
  p.fire("#t", "click");
  p.tick(32);
  p.tick(300);
  p.server({ type: "patch", ref: 2, updates: [{ id: "c1", html }] });
  eq(state(p), [false, "display: contents", ""]);
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
    } catch (err) {
      failed++;
      console.log("FAIL " + t.name + ": " + err.message + (err.stack && !(err.message || "").includes("got") ? "\n" + err.stack : ""));
    }
  }
  console.log(`${tests.length} tests, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
