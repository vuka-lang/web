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

test("a Derived: computed once per change, in a chain, its readers following only a new value", () => {
  const count = s("c1:count:1", 2), price = s("c1:price:1", 30);
  const total = { s: "c1:total:1", i: 60, x: ["*", count, price] };
  const over = { s: "c1:over:1", i: false, x: [">", total, 100] };
  const p = open(`<div>
    <i id="t" data-vk-b-title='${j(total)}' title="60"></i>
    <i id="u" data-vk-b-title='${j(["+", total, 1])}' title="61"></i>
    <i id="o" data-vk-b-data-over='${j(over)}'></i>
    <button id="up" data-vk-js-click='${j([["state_inc", { ...count, by: 2 }]])}'></button>
    <button id="same" data-vk-js-click='${j([["state_set", { ...count, v: 1 }], ["state_set", { ...price, v: 120 }]])}'></button>
    <button id="bad" data-vk-js-click='${j([["state_set", { ...total, v: 5 }]])}'></button></div>`);
  const w = writes(p);
  p.fire("#up", "click");
  eq([p.$("#t").getAttribute("title"), p.$("#u").getAttribute("title"), p.$("#o").hasAttribute("data-over")], ["120", "121", true], "4 * 30");
  eq([w("t title"), w("u title"), w("o data-over")], [1, 1, 1], "one write each");
  p.fire("#same", "click");
  eq([p.$("#t").getAttribute("title"), w("t title"), w("o data-over")], ["120", 1, 1], "1 * 120: the same total, nothing written");
  p.fire("#bad", "click");
  eq(p.$("#t").getAttribute("title"), "120", "a Derived isn't written");
  eq(p.window.vuka.states.read(total), 120, "read by islands and scripts");
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

test("watches: after the page shows the write, with the value before; not on a render", () => {
  const html = (n) => `<div data-vk-id="c1"><button id="b" data-vk-js-click='${j([["state_set", { ...tab, v: "env" }], ["state_set", { ...open1, v: true }]])}'>${n}</button>
    <p id="y" data-vk-b-data-tab='${j(tab)}' data-tab="logs"></p></div>`;
  const p = joined(html(0));
  const seen = [];
  p.window.vuka.states.watch(tab, (now, was) => seen.push([now, was, p.$("#y").getAttribute("data-tab")]), null, "tab");
  p.server({ type: "patch", ref: 2, updates: [{ id: "c1", html: html(1) }] });
  eq(seen, [], "a render isn't a write");
  p.fire("#b", "click");
  eq(seen, [["env", "logs", "env"]], "one call, the page already showing env");
  p.fire("#b", "click");
  eq(seen.length, 1, "an unchanged value isn't watched again");
});

test("a watch writing states: applied in the next round; a loop is cut with the states' names", () => {
  const n = s("c1:n:1", 0), m = s("c1:m:1", 0);
  const p = open(`<div data-vk-id="c1"><i id="n" data-vk-b-title='${j(n)}'></i><i id="m" data-vk-b-title='${j(m)}'></i>
    <button id="b" data-vk-js-click='${j([["state_inc", { ...n, by: 1 }]])}'></button></div>`);
  const st = p.window.vuka.states;
  st.watch(n, (v) => st.write(m, v * 10), null, "n");
  p.fire("#b", "click");
  eq([p.$("#n").getAttribute("title"), p.$("#m").getAttribute("title")], ["1", "10"], "a watch's write");
  st.watch(m, (v) => st.write(n, v + 1), null, "m");
  p.fire("#b", "click");
  if (!p.logs.some((l) => /warn .*watches keep writing (n, m|m, n): cut a loop/.test(l))) throw new Error("no warning: " + p.logs.join("\n"));
});

test("@ui.Notify: a browser write calls the handler with the value, once per write; a server's doesn't", () => {
  const q = { ...s("c1:q:1", ""), w: [["push", { h: "c1:5", args: [""] }]] };
  const p = joined(`<div data-vk-id="c1"><button id="b" data-vk-js-click='${j([["state_set", { ...q, v: "dog" }]])}'></button>
    <button id="c" data-vk-js-click='${j([["state_set", { ...q, v: "dog" }]])}'></button></div>`);
  const ws = p.ws(), before = ws.sent.length;
  p.fire("#b", "click");
  const sent = ws.sent.slice(before);
  eq(sent.map((m) => [m.target, m.args]), [["c1:5", ["dog"]]], "the handler, with the value");
  p.fire("#c", "click");
  eq(ws.sent.length, before + 1, "the same value isn't a write");
  p.server({ type: "patch", ref: 2, updates: [], js: [{ id: "c1", ops: [["state_set", { ...q, v: "cat" }]] }] });
  eq(ws.sent.length, before + 1, "the server's write isn't sent back");
});

// tabs is a localStorage and a BroadcastChannel two pages share, as two
// tabs of one origin do.
function tabs() {
  const data = new Map(), chans = [];
  const localStorage = { getItem: (k) => data.has(k) ? data.get(k) : null, setItem: (k, v) => data.set(k, String(v)) };
  class BroadcastChannel {
    constructor(name) { this.name = name; chans.push(this); }
    postMessage(d) { const m = JSON.parse(JSON.stringify(d)); chans.filter((c) => c !== this && c.name === this.name).forEach((c) => c.onmessage && c.onmessage({ data: m })); }
  }
  return { data, globals: { localStorage, BroadcastChannel } };
}

test("@ui.Keep: the stored value wins at load, each write is stored; no storage is a plain state", () => {
  const t = tabs();
  t.data.set("vuka:theme", JSON.stringify("dark"));
  const theme = { ...s("c1:theme:1", "light"), k: "theme" };
  const body = `<div data-vk-id="c1"><p id="t" data-vk-b-title='${j(theme)}' title="light"></p>
    <button id="b" data-vk-js-click='${j([["state_set", { ...theme, v: "blue" }]])}'></button></div>`;
  const p = open(body, { globals: t.globals });
  eq(p.$("#t").getAttribute("title"), "dark", "the stored value");
  p.fire("#b", "click");
  eq([p.$("#t").getAttribute("title"), t.data.get("vuka:theme")], ["blue", '"blue"'], "stored");
  const bad = { getItem() { throw new Error("denied"); }, setItem() { throw new Error("denied"); } };
  const q = open(body, { globals: { localStorage: bad } });
  q.fire("#b", "click");
  eq(q.$("#t").getAttribute("title"), "blue", "a refused storage");
});

test("@ui.Shared: a write shows in the other tab, which watches it and doesn't send it back", () => {
  const t = tabs();
  const side = (k) => ({ ...s(k, false), k: "sidebar", sh: "k:sidebar" });
  const body = (k) => `<div data-vk-id="c1"><p id="s" data-vk-b-hidden='${j(["!", side(k)])}' hidden></p>
    <button id="b" data-vk-js-click='${j([["state_toggle", side(k)]])}'></button></div>`;
  const a = open(body("c1:side:1"), { globals: t.globals }), b = open(body("c1:side:9"), { globals: t.globals });
  const seen = [], posts = [];
  b.window.vuka.states.watch(side("c1:side:9"), (now, was) => seen.push([now, was]));
  const send = t.globals.BroadcastChannel.prototype.postMessage;
  t.globals.BroadcastChannel.prototype.postMessage = function (d) { posts.push(d); return send.call(this, d); };
  a.fire("#b", "click");
  eq([a.$("#s").hasAttribute("hidden"), b.$("#s").hasAttribute("hidden")], [false, false], "shown in both");
  eq([seen, posts.length], [[[true, false]], 1], "watched there, posted once: by the tab that wrote it");
  b.fire("#b", "click");
  eq(a.$("#s").hasAttribute("hidden"), true, "and back");
});

test("optimistic: a failed push puts back the states its chain wrote; plain pushes keep them", () => {
  const liked = s("c1:liked:1", false), n = s("c1:n:1", 0);
  const p = joined(`<div data-vk-id="c1"><vk-t id="l" data-vk-b='${j(liked)}'>false</vk-t><vk-t id="n" data-vk-b='${j(n)}'>0</vk-t>
    <button id="o" data-vk-js-click='${j([["optimistic"], ["state_toggle", liked], ["state_inc", { ...n, by: 1 }], ["push", { h: "c1:1" }]])}'></button>
    <button id="plain" data-vk-js-click='${j([["state_toggle", liked], ["push", { h: "c1:1" }]])}'></button></div>`);
  const ws = p.ws(), last = () => ws.sent[ws.sent.length - 1].ref;
  const shown = () => [p.$("#l").textContent, p.$("#n").textContent];
  const errs = [];
  p.doc.addEventListener("vk:error", () => errs.push(shown()));
  p.fire("#o", "click");
  eq(shown(), ["true", "1"], "shown at once");
  p.server({ type: "patch", ref: last(), updates: [], error: "nope" });
  eq([shown(), errs.length > 0 && errs.every((e) => e.join() === "false,0")], [["false", "0"], true], "put back before the error shows");
  p.fire("#o", "click");
  p.server({ type: "patch", ref: last(), updates: [] });
  eq(shown(), ["true", "1"], "kept on success");
  p.fire("#plain", "click");
  p.server({ type: "patch", ref: last(), updates: [], error: "nope" });
  eq(shown(), ["false", "1"], "a plain push keeps its writes");
});

test("optimistic: two in flight fail in either order and leave what was there before both", () => {
  for (const order of [[0, 1], [1, 0]]) {
    const liked = s("c1:liked:1", false);
    const p = joined(`<div data-vk-id="c1"><vk-t id="l" data-vk-b='${j(liked)}'>false</vk-t>
      <button id="o" data-vk-js-click='${j([["optimistic"], ["state_toggle", liked], ["push", { h: "c1:1" }]])}'></button></div>`);
    const ws = p.ws(), refs = [];
    p.fire("#o", "click"); refs.push(ws.sent[ws.sent.length - 1].ref);
    p.fire("#o", "click"); refs.push(ws.sent[ws.sent.length - 1].ref);
    eq(p.$("#l").textContent, "false", "toggled twice");
    for (const i of order) p.server({ type: "patch", ref: refs[i], updates: [], error: "nope" });
    eq(p.$("#l").textContent, "false", "order " + order);
  }
  const liked = s("c1:liked:1", false);
  const p = joined(`<div data-vk-id="c1"><vk-t id="l" data-vk-b='${j(liked)}'>false</vk-t>
    <button id="t" data-vk-js-click='${j([["optimistic"], ["state_set", { ...liked, v: true }], ["push", { h: "c1:1" }]])}'></button>
    <button id="f" data-vk-js-click='${j([["optimistic"], ["state_set", { ...liked, v: false }], ["push", { h: "c1:1" }]])}'></button></div>`);
  const ws = p.ws(), refs = [];
  p.fire("#t", "click"); refs.push(ws.sent[ws.sent.length - 1].ref);
  p.fire("#f", "click"); refs.push(ws.sent[ws.sent.length - 1].ref);
  p.server({ type: "patch", ref: refs[1], updates: [], error: "nope" });
  eq(p.$("#l").textContent, "true", "the later one fails: back to the earlier's prediction");
  p.server({ type: "patch", ref: refs[0], updates: [], error: "nope" });
  eq(p.$("#l").textContent, "false", "then the earlier: back to the start");
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
const document_ = (p) => p.doc;
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

// popPage is a Show around a popover whose showPopover and hidePopover act
// as the browser's do; dismiss closes it as a light dismiss does.
function popPage(when, o = {}) {
  const p = open(`<div>
    <button id="t" data-vk-js-click='${j([["state_toggle", shown]])}'></button>
    <i id="v" data-vk-b-aria-label='${j(shown)}' aria-label="false"></i>
    <vk-show id="w" data-vk-show='${j(when)}' hidden><div id="k" popover></div></vk-show></div>`, { globals: styled("0s"), ...o });
  const k = p.$("#k");
  let on = false;
  k.showPopover = () => { on = true; };
  k.hidePopover = () => { on = false; };
  const m = k.matches;
  k.matches = (sel) => sel === ":popover-open" ? on : m.call(k, sel);
  const dismiss = () => { on = false; p.fire(k, "toggle", { bubbles: false, newState: "closed", oldState: "open" }); };
  return { p, k, isOpen: () => on, dismiss };
}

test("show: a popover the browser closes writes its state back", () => {
  const { p, isOpen, dismiss } = popPage(shown);
  p.fire("#t", "click");
  p.tick(32);
  eq([isOpen(), p.$("#v").getAttribute("aria-label")], [true, "true"], "opened");
  p.fire(document_(p), "keydown", { key: "Escape" });
  dismiss();
  eq([isOpen(), p.$("#v").getAttribute("aria-label"), state(p)[2]], [false, "false", "data-closed"], "closed by Escape: the state is false");
  p.fire("#t", "click");
  p.tick(32);
  eq(isOpen(), true, "and opens again");
});

test("show: a negated state is written true; a derived one stays", () => {
  let { p, isOpen, dismiss } = popPage(["!", shown]);
  p.fire("#t", "click");
  p.fire("#t", "click");
  eq([isOpen(), p.$("#v").getAttribute("aria-label")], [true, "false"], "shown while false");
  dismiss();
  eq(p.$("#v").getAttribute("aria-label"), "true", "written true");
  ({ p, isOpen, dismiss } = popPage(["&&", ["!", shown], true]));
  dismiss();
  eq(p.$("#v").getAttribute("aria-label"), "false", "a derived expression isn't written");
});

test("show: a click on the trigger while open ends closed, whichever comes first", () => {
  // the close first, then the click
  let { p, isOpen, dismiss } = popPage(shown);
  p.fire("#t", "click");
  p.tick(32);
  p.fire("#t", "pointerdown");
  dismiss();
  p.fire("#t", "click");
  p.tick(32);
  eq([isOpen(), p.$("#v").getAttribute("aria-label")], [false, "false"], "close, then click");
  p.fire("#t", "pointerdown");
  p.fire("#t", "click");
  p.tick(32);
  eq(isOpen(), true, "the next click opens it");
  // the click first, then the close
  ({ p, isOpen, dismiss } = popPage(shown));
  p.fire("#t", "click");
  p.tick(32);
  p.fire("#t", "pointerdown");
  p.fire("#t", "click");
  dismiss();
  p.tick(32);
  eq([isOpen(), p.$("#v").getAttribute("aria-label")], [false, "false"], "click, then close");
});

test("show: with no transition the old content is hidden at once; two swapped never both show", () => {
  const a = s("c1:a:1", true);
  const p = open(`<div>
    <button id="sw" data-vk-js-click='${j([["state_toggle", a]])}'></button>
    <vk-show id="wa" data-vk-show='${j(a)}' style="display: contents"><div id="ka"></div></vk-show>
    <vk-show id="wb" data-vk-show='${j(["!", a])}' hidden><div id="kb" data-closed></div></vk-show></div>`, { globals: styled("0s") });
  const both = () => !p.$("#wa").hasAttribute("hidden") && !p.$("#wb").hasAttribute("hidden");
  p.fire("#sw", "click");
  eq([both(), p.$("#wa").hasAttribute("hidden"), p.$("#wb").hasAttribute("hidden"), p.$("#ka").hasAttribute("data-closed")], [false, true, false, true], "swapped in the same batch");
  p.fire("#sw", "click");
  eq([both(), p.$("#wa").hasAttribute("hidden"), p.$("#wb").hasAttribute("hidden")], [false, false, true], "and back");
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

test("select-all mixed state follows initial render and live patches", () => {
 const html = (attrs) => `<div data-vk-id="c1"><input id="all" type="checkbox" ${attrs}></div>`;
 const p = joined(html('data-vk-mixed="true" aria-checked="mixed"'));
 eq(p.$("#all").indeterminate, true, "initial partial selection");
 p.server({type:"patch", ref:2, updates:[{id:"c1", html:html("checked")}]});
 eq([p.$("#all").checked, p.$("#all").indeterminate], [true,false], "all selected");
 p.server({type:"patch", ref:3, updates:[{id:"c1", html:html('data-vk-mixed="true" aria-checked="mixed"')}]});
 eq([p.$("#all").checked, p.$("#all").indeterminate], [false,true], "one deselected");
 p.server({type:"patch", ref:4, updates:[{id:"c1", html:html("")}]});
 eq([p.$("#all").checked, p.$("#all").indeterminate], [false,false], "cleared");
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
