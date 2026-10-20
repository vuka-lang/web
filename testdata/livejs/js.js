// Tests of live.js's JS commands (data-vk-js-*, and commands a render or
// patch carries) and navigation in node, over dom.js: node js.js live.js
"use strict";
const { page } = require("./dom.js");
const file = process.argv[2];
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const js = (ops) => JSON.stringify(ops).replace(/&/g, "&amp;").replace(/'/g, "&#39;");

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}
const open = (body, o) => page(file, body, o);
// joined opens a live page and joins it with a render of body (v1).
function joined(body, o = {}) {
  const p = open(body, { live: true, ...o });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: body });
  return p;
}

test("classes on this, by selector, closest and inner", () => {
  const p = open(`<div class="card"><button class="b" data-vk-js-click='${js([
    ["add_class", { names: ["on", "x"] }],
    ["toggle_class", { to: { sel: ".t" }, names: ["open"] }],
    ["remove_class", { to: { closest: ".card" }, names: ["card"] }],
    ["add_class", { to: { inner: "i" }, names: ["in"] }],
  ])}'><i></i></button><p class="t"></p><p class="t open"></p></div>`);
  p.fire("button", "click");
  eq(p.$("button").className, "b on x", "this");
  eq(p.doc.querySelectorAll(".t").map((e) => e.className), ["t open", "t"], "toggled");
  eq(p.$("div").className, "", "closest");
  eq(p.$("i").className, "in", "inner");
});

test("a click inside the element runs its commands", () => {
  const p = open(`<button data-vk-js-click='${js([["add_class", { names: ["on"] }]])}'><span>x</span></button>`);
  p.fire("span", "click");
  eq(p.$("button").className, "on");
});

test("refs are the instance's own", () => {
  const p = open(`<div data-vk-id="c1"><ul data-vk-ref="menu"></ul>
    <button data-vk-js-click='${js([["toggle_class", { to: { ref: "menu" }, names: ["open"] }]])}'></button>
    <div data-vk-id="c2"><ul data-vk-ref="menu" id="inner"></ul></div></div>`);
  p.fire("button", "click");
  eq(p.$("[data-vk-ref]").className, "open", "own");
  eq(p.$("#inner").className, "", "nested instance's");
});

test("attributes", () => {
  const p = open(`<button aria-expanded="false" data-vk-js-click='${js([
    ["set_attr", { to: { sel: "#a" }, name: "title", value: "hi" }],
    ["remove_attr", { to: { sel: "#a" }, name: "disabled" }],
    ["toggle_attr", { name: "aria-expanded", values: ["true", "false"] }],
    ["toggle_attr", { to: { sel: "#a" }, name: "open" }],
  ])}'></button><input id="a" disabled>`);
  p.fire("button", "click");
  eq([p.$("#a").getAttribute("title"), p.$("#a").hasAttribute("disabled"), p.$("button").getAttribute("aria-expanded"), p.$("#a").getAttribute("open")],
    ["hi", false, "true", ""]);
  p.fire("button", "click");
  eq([p.$("button").getAttribute("aria-expanded"), p.$("#a").hasAttribute("open")], ["false", false]);
});

test("show, hide, toggle", () => {
  const p = open(`<button id="s" data-vk-js-click='${js([["show", { to: { sel: "#m" }, display: "flex" }]])}'></button>
    <button id="h" data-vk-js-click='${js([["hide", { to: { sel: "#m" } }]])}'></button>
    <button id="t" data-vk-js-click='${js([["toggle", { to: { sel: "#m" } }]])}'></button>
    <div id="m" hidden style="color: red"></div>`);
  const m = p.$("#m");
  p.fire("#s", "click");
  eq([m.hasAttribute("hidden"), m.getAttribute("style")], [false, "color: red; display: flex"], "shown");
  p.fire("#h", "click");
  eq([m.hasAttribute("hidden"), m.getAttribute("style")], [false, "color: red; display: none"], "hidden");
  p.fire("#t", "click");
  eq([m.hasAttribute("hidden"), m.getAttribute("style")], [false, "color: red"], "toggled on");
  p.fire("#t", "click");
  eq(m.getAttribute("style"), "color: red; display: none", "toggled off");
});

test("transitions", () => {
  const tr = ["ease", "o-0", "o-100"];
  const p = open(`<button id="s" data-vk-js-click='${js([["show", { to: { sel: "#m" }, transition: tr, time: 100 }]])}'></button>
    <button id="h" data-vk-js-click='${js([["hide", { to: { sel: "#m" }, transition: ["ease", "o-100", "o-0"], time: 100 }]])}'></button>
    <button id="t" data-vk-js-click='${js([["transition", { to: { sel: "#m" }, transition: ["shake"], time: 50 }]])}'></button>
    <div id="m" class="m" hidden></div>`);
  const m = p.$("#m");
  p.fire("#s", "click");
  eq([m.className, m.hasAttribute("hidden")], ["m ease o-0", false], "start");
  p.tick(40);
  eq(m.className, "m ease o-100", "a frame on");
  p.tick(100);
  eq(m.className, "m", "done");
  p.fire("#h", "click");
  p.tick(40);
  eq([m.className, m.hasAttribute("hidden")], ["m ease o-0", false], "hiding");
  p.fire("#t", "click"); // finishes the hide first
  eq([m.className, m.getAttribute("style")], ["m shake", "display: none"], "finished, then the next");
  p.tick(100);
  eq(m.className, "m", "all off");
});

test("focus, focus_first, the focus stack", () => {
  const p = open(`<button id="open" data-vk-js-click='${js([["push_focus", {}], ["focus_first", { to: { sel: "#dlg" } }]])}'></button>
    <div id="dlg"><button disabled id="d"></button><input type="hidden"><input id="first"><button id="close" data-vk-js-click='${js([["pop_focus", {}]])}'></button></div>
    <a id="f" data-vk-js-click='${js([["focus", { to: { sel: "#first" } }], ["push_focus", { to: { sel: "#close" } }], ["pop_focus", {}]])}'></a>`);
  p.$("#open").focus();
  p.fire("#open", "click");
  eq(p.doc.activeElement.id, "first", "first focusable");
  p.fire("#close", "click");
  eq(p.doc.activeElement.id, "open", "popped");
  p.fire("#f", "click");
  eq(p.doc.activeElement.id, "close", "pushed target");
});

test("dispatch and exec", () => {
  const p = open(`<div id="box"><p id="t" data-cancel='${js([["add_class", { names: ["ran"] }]])}'></p></div>
    <button data-vk-js-click='${js([["dispatch", { to: { sel: "#t" }, event: "my:open", detail: { n: 1 } }], ["exec", { to: { sel: "#t" }, attr: "data-cancel" }]])}'></button>`);
  const got = [];
  p.$("#box").addEventListener("my:open", (e) => got.push([e.target.id, e.detail.n]));
  p.fire("button", "click");
  eq(got, [["t", 1]], "bubbled with its detail");
  eq(p.$("#t").className, "ran", "exec ran the attribute's commands on its element");
});

test("conditions and blocks", () => {
  const ops = [
    ["if", { cond: { to: { sel: "#c" }, checked: true } }],
    ["add_class", { names: ["checked"] }],
    ["if", { cond: { to: { sel: "#v" }, value_eq: "a" } }],
    ["add_class", { names: ["a"] }],
    ["else", {}],
    ["add_class", { names: ["not-a"] }],
    ["end", {}],
    ["else_if", { cond: { to: { sel: "#v" }, value: true } }],
    ["add_class", { names: ["value"] }],
    ["else_if", { cond: { to: { sel: "#x" }, exists: true } }],
    ["add_class", { names: ["x"] }],
    ["else", {}],
    ["add_class", { names: ["none"] }],
    ["end", {}],
    ["if", { cond: { to: { sel: "#v" }, is: ".big" } }],
    ["add_class", { names: ["big"] }],
    ["end", {}],
    ["if", { cond: { to: { sel: "#v" }, attr: "required" } }],
    ["add_class", { names: ["req"] }],
    ["end", {}],
    ["add_class", { names: ["after"] }],
  ];
  const p = open(`<input id="c" type="checkbox"><input id="v" class="big"><button data-vk-js-click='${js(ops)}'></button>`);
  const click = () => { p.$("button").setAttribute("class", ""); p.fire("button", "click"); return p.$("button").className; };
  eq(click(), "none big after", "nothing checked or typed");
  p.$("#v").value = "b";
  eq(click(), "value big after", "a value");
  p.$("#c").checked = true;
  eq(click(), "checked not-a big after", "checked");
  p.$("#v").value = "a";
  p.$("#v").setAttribute("required", "");
  eq(click(), "checked a big req after", "checked and a");
});

test("a read holds when every element passes it; refs on a static page are the document's", () => {
  const p = open(`<input class="c" type="checkbox"><input class="c" type="checkbox" checked><p data-vk-ref="r"></p><div data-vk-id="c1"><p data-vk-ref="r"></p></div>
    <button data-vk-js-click='${js([["if", { cond: { to: { sel: ".c" }, checked: true } }], ["add_class", { names: ["any"] }], ["end", {}],
      ["if", { cond: { to: { sel: ".none" }, value: false } }], ["add_class", { names: ["missing"] }], ["end", {}],
      ["set_value", { to: { sel: ".c" }, value: "true" }], ["add_class", { to: { ref: "r" }, names: ["r"] }]])}'></button>`);
  p.fire("button", "click");
  eq([p.$("button").className, p.doc.querySelectorAll(".c").map((c) => c.checked), p.doc.querySelectorAll(".r").length], ["", [true, true], 2]);
});

test("targets of several parts: every element any part finds, in page order, each once", () => {
  const p = open(`<div data-vk-id="c1"><input data-vk-ref="b" id="b1"><input data-vk-ref="a" id="a1"><input data-vk-ref="c" id="c1">
    <i class="x" id="i1"></i><i class="x y" id="i2"></i><i class="y" id="i3"></i>
    <button data-vk-js-click='${js([
      ["toggle_class", { to: { ref: ["a", "b", "a"] }, names: ["r"] }],
      ["toggle_class", { to: { sel: [".y", ".x", "#i2"] }, names: ["s"] }],
      ["call", { fn: "seen", args: [{ $read: { to: { ref: ["a", "b"] }, attr: "id" } }] }],
      ["focus", { to: { ref: ["c", "a"] } }],
    ])}'></button></div>`);
  let first;
  p.window.vuka.register("seen", (ctx, id) => { first = id; });
  p.fire("button", "click");
  eq(p.doc.querySelectorAll(".r").map((e) => e.id), ["b1", "a1"], "refs, toggled once each");
  eq(p.doc.querySelectorAll(".s").map((e) => e.id), ["i1", "i2", "i3"], "selectors, toggled once each");
  eq([first, p.doc.activeElement.id], ["b1", "a1"], "a read and focus take the first in page order");
});

test("attribute targets: presence, exact values, matchers; the instance's own unless page", () => {
  const p = open(`<div data-vk-id="c1"><input name="username" id="u"><input name="password" id="p"><input name="step-1" data-tags="new old" id="s1">
    <input name="last-step" data-tags="older" id="s2"><input name="pre-draft-x" id="d">
    <button data-vk-js-click='${js([
      ["add_class", { to: { attr: { name: "name", is: ["username", "password"] } }, names: ["exact"] }],
      ["add_class", { to: { attr: { name: "name", is: [{ prefix: "step-" }] } }, names: ["prefix"] }],
      ["add_class", { to: { attr: { name: "name", is: [{ suffix: "-step" }] } }, names: ["suffix"] }],
      ["add_class", { to: { attr: { name: "name", is: [{ contains: "draft" }] } }, names: ["contains"] }],
      ["add_class", { to: { attr: { name: "data-tags", is: [{ word: "old" }] } }, names: ["word"] }],
      ["add_class", { to: { attr: { name: "data-tags" } }, names: ["has"] }],
      ["add_class", { to: { attr: { name: "data-outside" } }, names: ["mine"] }],
      ["add_class", { to: { attr: { name: "data-outside", page: true } }, names: ["page"] }],
      ["add_class", { to: { attr: { name: "name", is: [{ prefix: "" }, { bogus: "u" }] } }, names: ["none"] }],
    ])}'></button><div data-vk-id="c2"><input name="username" id="nested"></div></div><p data-vk-id="c3"><b data-vk-ref="x" data-outside id="out"></b></p>`);
  p.fire("button", "click");
  const by = (c) => p.doc.querySelectorAll("." + c).map((e) => e.id);
  eq(["exact", "prefix", "suffix", "contains", "word", "has", "mine", "page", "none"].map(by),
    [["u", "p"], ["s1"], ["s2"], ["d"], ["s1"], ["s1", "s2"], [], ["out"], []]);
});

test("attribute targets escape: a value can't break out of its selector", () => {
  const evil = `x"], * , [name="`;
  const p = open(`<input name='${evil.replace(/"/g, "&quot;")}' id="evil"><input name="other" id="other"><input name="1a" id="digit"><input name='a\\b' id="slash">
    <button data-vk-js-click='${js([
      ["add_class", { to: { attr: { name: "name", is: [evil] } }, names: ["hit"] }],
      ["add_class", { to: { attr: { name: "name", is: [{ contains: '"]' }] } }, names: ["part"] }],
      ["add_class", { to: { attr: { name: "name", is: ["1a", "a\\b"] } }, names: ["esc"] }],
      ["add_class", { to: { attr: { name: 'x"]' } }, names: ["bad"] }],
    ])}'></button>`);
  p.fire("button", "click");
  eq([p.doc.querySelectorAll(".hit").map((e) => e.id), p.doc.querySelectorAll(".part").map((e) => e.id), p.doc.querySelectorAll(".esc").map((e) => e.id),
    p.doc.querySelectorAll(".bad").length], [["evil"], ["evil"], ["digit", "slash"], 0]);
});

test("conditions join: and, or, not; reads of every element or some; no element fails a read", () => {
  const cond = (c) => [["if", { cond: c }], ["add_class", { names: ["yes"] }], ["else", {}], ["add_class", { names: ["no"] }], ["end", {}]];
  const p = open(`<input class="f" value="a"><input class="f"><input type="checkbox" id="c" checked><button data-vk-js-click="[]"></button>`);
  const holds = (c) => {
    const b = p.$("button");
    b.setAttribute("class", "");
    b.setAttribute("data-vk-js-click", JSON.stringify(cond(c)));
    p.fire(b, "click");
    return b.className;
  };
  const f = { sel: ".f" }, none = { sel: ".none" }, c = { to: { sel: "#c" }, checked: true };
  eq([holds({ to: f, value: true }), holds({ to: f, value: true, some: true }), holds({ to: f, value_eq: "a", some: true }), holds({ to: f, value_eq: "b", some: true })],
    ["no", "yes", "yes", "no"], "every, some");
  eq([holds({ to: none, value: true }), holds({ to: none, value: true, some: true }), holds({ not: { to: none, value: true } }), holds({ to: none, exists: true })],
    ["no", "no", "yes", "no"], "no element");
  eq([holds({ and: [c, { to: f, value: true, some: true }] }), holds({ and: [c, { to: f, value: true }] }), holds({ or: [{ to: f, value: true }, c] }),
    holds({ or: [{ to: none, exists: true }, { not: c }] }), holds({ and: [] }), holds({ or: [] })],
    ["yes", "no", "yes", "no", "yes", "no"], "joined");
  eq(holds({ or: [{ and: [c, { to: f, value: true }] }, { not: { to: none, exists: true } }] }), "yes", "nested: (c and every f) or no .none");
  p.$("#c").checked = false;
  p.$("#c").setAttribute("checked", "");
  let v;
  p.window.vuka.register("r", (ctx, x) => { v = x; });
  window_run(p, [["call", { fn: "r", args: [{ $read: { and: [c, { not: { to: f, value: true } }] } }] }]]);
  eq(v, false, "a joined read as a call's argument");
});

test("when and its else: if blocks, nested", () => {
  // As ui.JS.When(c, ui.JS.When(v, …)).Else(…) encodes.
  const ops = [
    ["if", { cond: { to: { sel: "#c" }, checked: true } }],
    ["if", { cond: { to: { sel: "#v" }, value: true } }], ["add_class", { names: ["both"] }], ["end", {}],
    ["add_class", { names: ["c"] }],
    ["else", {}], ["add_class", { names: ["not-c"] }], ["end", {}],
    ["add_class", { names: ["after"] }],
  ];
  const p = open(`<input id="c" type="checkbox"><input id="v"><button data-vk-js-click='${js(ops)}'></button>`);
  const click = () => { p.$("button").setAttribute("class", ""); p.fire("button", "click"); return p.$("button").className; };
  eq(click(), "not-c after", "else");
  p.$("#c").checked = true;
  eq(click(), "c after", "when");
  p.$("#v").value = "x";
  eq(click(), "both c after", "nested when");
});

// login is a plain POST form, as onSubmit={ui.JS.Busy()} renders it.
const login = (ops = [["busy", {}]], extra = "") => `<form method="post" class="group" data-vk-js-submit='${js(ops)}'>
  <input name="username" value="ann"><input name="password" value="pw"><input name="code" readonly="readonly" value="7">
  <select name="lang"><option>en</option></select><textarea name="note"></textarea>
  <a href="/help" aria-disabled="false" id="help">help</a><button id="go" tabindex="2">Sign in</button>${extra}</form>`;
const busyState = (p) => {
  const a = (sel, n) => p.$(sel).getAttribute(n);
  return [a("form", "aria-busy"), a("[name=username]", "readonly"), a("[name=code]", "readonly"), a("select", "readonly"), a("textarea", "readonly"),
    a("#help", "aria-disabled"), a("#help", "tabindex"), a("#go", "aria-disabled"), a("#go", "tabindex")];
};
const idle = [null, null, "readonly", null, null, "false", null, null, "2"];
const busy = ["true", "", "readonly", "", "", "true", "-1", "true", "-1"];

test("busy: a plain post goes on, its fields still sent; a second submit or click is ignored", () => {
  const p = open(login());
  let posts = 0;
  p.doc.addEventListener("submit", (e) => { if (!e.defaultPrevented) posts++; });
  let clicks = 0;
  p.doc.addEventListener("click", () => { clicks++; });
  const first = p.fire("form", "submit");
  eq([first.defaultPrevented, posts, busyState(p)], [false, 1, busy], "the browser posts it, marked busy");
  const fd = new p.window.FormData(p.$("form"));
  eq([...fd].map(([k]) => k), ["username", "password", "code", "lang", "note"], "readonly fields are submitted");
  eq([p.$("[name=username]").hasAttribute("disabled")], [false], "never disabled");
  p.tick(0);
  const again = p.fire("form", "submit");
  const click = p.fire("#go", "click");
  eq([again.defaultPrevented, click.defaultPrevented, posts, clicks], [true, true, 1, 0], "ignored");
  p.window.dispatchEvent(Object.assign(new p.window.Event("pageshow"), { persisted: false }));
  eq(busyState(p), busy, "a pageshow not from the back/forward cache leaves it");
  p.window.dispatchEvent(Object.assign(new p.window.Event("pageshow"), { persisted: true }));
  eq(busyState(p), idle, "from the back/forward cache: undone, exactly as it was");
  eq(p.fire("form", "submit").defaultPrevented, false, "usable again");
});

test("busy: a click marking its form lets that click's submit through", () => {
  const p = open(login([], "").replace(`<button id="go" tabindex="2">`, `<button id="go" tabindex="2" data-vk-js-click='${js([["busy", { to: { closest: "form" } }]])}'>`));
  p.fire("#go", "click");
  eq([p.$("form").getAttribute("aria-busy"), p.fire("form", "submit").defaultPrevented], ["true", false], "the click's own submit");
  p.tick(0);
  eq(p.fire("form", "submit").defaultPrevented, true, "a later one");
});

test("busy: undone by its timeout", () => {
  const p = open(login([["busy", { timeout: 500 }]]));
  p.fire("form", "submit");
  p.tick(499);
  eq(busyState(p), busy, "before");
  p.tick(1);
  eq(busyState(p), idle, "after");
});

test("busy: the reply to the chain's push undoes it, before on_error runs", () => {
  const form = (ops) => `<div data-vk-id="c1">${login(ops)}<p id="log"></p></div>`;
  const usable = [["if", { cond: { not: { to: { sel: "form" }, attr: "aria-busy" } } }], ["set_attr", { to: { sel: "#log" }, name: "data-usable", value: "yes" }], ["end", {}]];
  for (const [ok, hook] of [[true, "on_ok"], [false, "on_error"]]) {
    const ops = [["busy", {}], ["push", { h: "c1:0", [hook]: usable }]];
    const p = joined(form(ops));
    const s = p.fire("form", "submit");
    eq([s.defaultPrevented, busyState(p)], [true, busy], "a live submit: prevented, busy");
    p.tick(0);
    p.fire("form", "submit");
    eq(p.ws().sent.length, 2, "a second submit sends nothing");
    p.server({ type: "patch", ref: sentRef(p, 1), updates: [{ id: "c1", html: form(ops) }], error: ok ? undefined : "nope" });
    eq([busyState(p), p.$("#log").getAttribute("data-usable")], [idle, "yes"], (ok ? "ok" : "error") + ": undone before its hook");
    p.server({ type: "patch", updates: [{ id: "c1", html: form(ops) }] });
    eq(busyState(p), idle, "a later patch doesn't bring it back");
  }
  // A busy after the push waits for it too; one with no connection fails at once.
  const after = [["push", { h: "c1:0" }], ["busy", {}]];
  const p = joined(form(after));
  p.fire("form", "submit");
  eq(busyState(p), busy, "after the push");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [] });
  eq(busyState(p), idle, "its reply");
  const q = open(form([["busy", {}], ["push", { h: "c1:0" }]]), { live: true });
  q.fire("form", "submit");
  eq(busyState(q), idle, "not joined: the push fails at once, and the mark with it");
});

test("busy: puts back what was there, and the server's later value wins", () => {
  const p = open(`<div data-vk-id="c1"><button id="b" aria-busy="false" aria-disabled="true" tabindex="-1" data-vk-js-click='${js([["busy", { timeout: 100 }]])}'>x</button></div>`);
  p.fire("#b", "click");
  eq([p.$("#b").getAttribute("aria-busy"), p.$("#b").getAttribute("aria-disabled"), p.$("#b").getAttribute("tabindex")], ["true", "true", "-1"]);
  p.tick(100);
  eq([p.$("#b").getAttribute("aria-busy"), p.$("#b").getAttribute("aria-disabled"), p.$("#b").getAttribute("tabindex")], ["false", "true", "-1"], "as it was");
});

test("debounce: the steps after it run once things are quiet, per element and event", () => {
  const p = open(`<input id="q" data-vk-js-input='${js([["add_class", { names: ["typing"] }], ["debounce", { ms: 100 }], ["toggle_class", { names: ["sent"] }]])}'>
    <input id="r" data-vk-js-input='${js([["debounce", { ms: 100 }], ["toggle_class", { names: ["sent"] }]])}'>`);
  p.fire("#q", "input");
  p.tick(60);
  p.fire("#q", "input");
  p.fire("#r", "input");
  eq(p.$("#q").className, "typing", "before the steps after");
  p.tick(60);
  eq(p.$("#q").className, "typing", "still waiting");
  p.tick(50);
  eq([p.$("#q").className, p.$("#r").className], ["typing sent", "sent"], "once each");
});

test("a debounced step in a form runs before its submit", () => {
  const p = open(`<form data-vk-js-submit='${js([["add_class", { to: { sel: "#q" }, names: ["submitted"] }]])}'>
    <input id="q" data-vk-js-input='${js([["debounce", { ms: 300 }], ["add_class", { names: ["typed"] }]])}'></form>`);
  p.fire("#q", "input");
  p.fire("form", "submit");
  eq(p.$("#q").className, "typed submitted");
  p.tick(400);
  eq(p.$("#q").className, "typed submitted", "not again");
});

test("throttle: at most once per interval, a field's last input kept", () => {
  const p = open(`<input id="q" data-vk-js-input='${js([["throttle", { ms: 100 }], ["set_attr", { to: { sel: "#out" }, name: "data-v", value: "x" }], ["exec", { attr: "data-count" }]])}' data-count='[]'><p id="out"></p>`);
  let n = 0;
  p.$("#out").setAttribute = ((set) => function (k, v) { if (k === "data-v") n++; return set.call(this, k, v); })(p.$("#out").setAttribute);
  p.fire("#q", "input");
  p.fire("#q", "input");
  p.fire("#q", "input");
  eq(n, 1, "the first at once");
  p.tick(100);
  eq(n, 2, "the last at the interval's end");
  p.tick(300);
  eq(n, 2, "nothing more");
  p.fire("#q", "input");
  eq(n, 3, "open again");
});

test("key filters the chain", () => {
  const p = open(`<div data-vk-js-keydown='${js([["key", { keys: ["Escape"] }], ["add_class", { names: ["esc"] }]])}'><input></div>`);
  p.fire("input", "keydown", { key: "a" });
  eq(p.$("div").className, "", "another key");
  p.fire("input", "keydown", { key: "Escape" });
  eq(p.$("div").className, "esc", "its key");
});

test("confirm", () => {
  const ops = [["confirm", { msg: "Sure?" }], ["add_class", { names: ["done"] }]];
  const yes = open(`<button data-vk-js-click='${js(ops)}'></button>`);
  yes.fire("button", "click");
  eq([yes.confirms, yes.$("button").className], [["Sure?"], "done"], "yes");
  const no = open(`<button data-vk-js-click='${js(ops)}'></button>`, { confirm: false });
  no.fire("button", "click");
  eq(no.$("button").className, "", "no");
});

test("copy, copy_text, scroll_to, set_value", () => {
  const p = open(`<input id="i" value="secret"><pre id="code"> go run . </pre>
    <button id="c1" data-vk-js-click='${js([["copy", { to: { sel: "#i" } }], ["copy", { to: { sel: "#code" } }], ["copy_text", { text: "hi" }]])}'></button>
    <button id="s" data-vk-js-click='${js([["scroll_to", { to: { sel: "#code" } }], ["set_value", { to: { sel: "#i" }, value: "new" }]])}'></button>`);
  p.fire("#c1", "click");
  eq(p.copied, ["secret", "go run .", "hi"], "copied");
  eq(p.$("#c1").hasAttribute("data-copied"), true, "marked");
  p.tick(1600);
  eq(p.$("#c1").hasAttribute("data-copied"), false, "unmarked");
  const seen = [];
  p.doc.addEventListener("input", (e) => seen.push(e.type + ":" + e.target.value));
  p.doc.addEventListener("change", (e) => seen.push(e.type + ":" + e.target.value));
  p.fire("#s", "click");
  eq([p.doc.scrolled.length, p.doc.scrolled[0][0].id, seen], [1, "code", ["input:new", "change:new"]]);
});

test("a page with only JS commands doesn't connect; push warns once, and without h does nothing", () => {
  const p = open(`<button data-vk-js-click='${js([["push", {}], ["push", { h: "c1:0" }], ["add_class", { names: ["on"] }]])}'></button>`);
  p.fire("button", "click");
  p.fire("button", "click");
  eq([p.sockets.length, p.$("button").className, p.logs.filter((l) => l.startsWith("warn")).length], [0, "on", 1]);
});

test("push sends an event as data-vk-on does", () => {
  const body = `<div data-vk-id="c1"><button value="7" data-vk-js-click='${js([["add_class", { names: ["on"] }], ["push", { h: "c1:0" }]])}'>x</button>
    <a href="/x" data-vk-js-click='${js([["push", { h: "c1:1", value: { id: 3 } }]])}'>x</a>
    <form data-vk-js-submit='${js([["push", { h: "c1:2" }]])}'><input name="text" value="hi"></form>
    <input id="k" data-vk-js-keydown='${js([["push", { h: "c1:3", value: "v" }]])}'></div>`;
  const p = joined(body);
  const sent = () => p.ws().sent.slice(1).map(({ ref, ...m }) => m);
  p.fire("button", "click");
  eq(sent(), [{ type: "event", target: "c1:0", event: "click", value: "7" }], "click");
  eq(p.$("button").getAttribute("aria-busy"), "true", "busy");
  p.server({ type: "patch", ref: p.ws().sent[1].ref, updates: [] });
  eq(p.$("button").hasAttribute("aria-busy"), false, "reply");
  const e = p.fire("a", "click");
  eq([sent()[1], e.defaultPrevented], [{ type: "event", target: "c1:1", event: "click", value: '{"id":3}' }, true], "link, a JSON value");
  const s = p.fire("form", "submit");
  eq([sent()[2], s.defaultPrevented], [{ type: "event", target: "c1:2", event: "submit", form: { text: ["hi"] } }, true], "submit");
  p.fire("#k", "keydown", { key: "Enter" });
  eq(sent()[3], { type: "event", target: "c1:3", event: "keydown", key: "Enter", value: "v" }, "key");
});

test("the nearest element with either attribute takes the event", () => {
  const p = joined(`<div data-vk-on-click="c0:0"><button data-vk-js-click='${js([["add_class", { names: ["on"] }]])}'><i>x</i></button></div>
    <div data-vk-js-click='${js([["add_class", { names: ["outer"] }]])}'><button id="srv" data-vk-on-click="c0:1">y</button></div>
    <form data-vk-js-submit='${js([["add_class", { names: ["sub"] }]])}'></form>`);
  p.fire("i", "click");
  eq([p.$("button").className, p.ws().sent.length], ["on", 1], "commands only");
  p.fire("#srv", "click");
  eq([p.doc.querySelectorAll("div")[1].className, p.ws().sent.length], ["", 2], "the handler only");
  eq([p.fire("form", "submit").defaultPrevented, p.$("form").className], [false, "sub"], "a submit's commands without a push let it go on");
});

test("the chain runs before data-vk-on; a declined confirm keeps the event from the server", () => {
  const body = `<button data-vk-on-click="c0:0" data-vk-js-click='${js([["confirm", { msg: "Delete?" }], ["add_class", { names: ["going"] }]])}'>x</button>`;
  const yes = joined(body);
  let classAtSend;
  const send = yes.ws().send.bind(yes.ws());
  yes.ws().send = (s) => { classAtSend = yes.$("button").className; send(s); };
  yes.fire("button", "click");
  eq([yes.ws().sent.length, classAtSend], [2, "going"], "sent after the chain");
  const no = joined(body, { confirm: false });
  no.fire("button", "click");
  eq(no.ws().sent.length, 1, "declined");
});

test("commands pushed with a render or patch run after it, on the instance's root", () => {
  const body = `<p id="top"></p><div data-vk-id="c1"><span data-vk-ref="msg">old</span></div>`;
  const p = open(body, { live: true });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: body.replace("old", "new"), js: [{ id: "c0", ops: [["add_class", { to: { sel: "#top" }, names: ["joined"] }]] }] });
  eq(p.$("#top").className, "joined", "render");
  p.server({
    type: "patch", updates: [{ id: "c1", html: `<div data-vk-id="c1"><span data-vk-ref="msg">saved</span></div>` }],
    js: [{ id: "c1", ops: [["add_class", { names: ["flash"] }], ["if", { cond: { to: { ref: "msg" }, is: "span" } }], ["add_class", { to: { ref: "msg" }, names: ["ok"] }], ["end", {}]] }],
  });
  eq([p.$("[data-vk-id=c1]").className, p.$("span").className, p.$("span").textContent], ["flash", "ok", "saved"], "patch");
  p.server({ type: "patch", ref: 9, updates: [], error: "nope", js: [{ id: "c1", ops: [["add_class", { names: ["bad"] }]] }] });
  eq(p.$("[data-vk-id=c1]").className, "flash", "not after an error");
});

test("reconciliation: a command's change stays until the server changes the attribute", () => {
  const html = (cls, style, extra = "") => `<div data-vk-id="c1"><ul id="menu" class="${cls}" style="${style}"${extra}></ul><p id="d" hidden></p><b>x</b></div>`;
  const p = open(html("menu", "color: red"), { live: true });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: html("menu", "color: red") });
  window_run(p, [["add_class", { to: { sel: "#menu" }, names: ["open"] }], ["hide", { to: { sel: "#menu" } }], ["show", { to: { sel: "#d" } }]]);
  const m = p.$("#menu"), d = p.$("#d");
  const patch = (h) => p.server({ type: "patch", updates: [{ id: "c1", html: h }] });
  patch(html("menu", "color: red").replace("x", "y"));
  eq([m.className, m.getAttribute("style"), d.hasAttribute("hidden"), p.$("b").textContent], ["menu open", "color: red; display: none", false, "y"], "kept");
  patch(html("menu wide", "color: red"));
  eq([m.className, m.getAttribute("style")], ["menu wide", "color: red; display: none"], "class: the server's");
  patch(html("menu wide", "color: blue"));
  eq(m.getAttribute("style"), "color: blue", "style: the server's");
  patch(html("menu wide", "color: blue").replace(' hidden>', ' hidden="until-found">'));
  eq(d.getAttribute("hidden"), "until-found", "hidden: the server's");
});

test("reconciliation under protocol v2, and a transition across a morph", () => {
  const p = open(`<div id="m" class="m"></div>`, { live: true });
  p.ws().onopen();
  const frame = { t: "a", s: ['<div id="m" class="m"><i>', "</i></div>"], d: ["1"] };
  p.server({ type: "render", ref: 1, trees: [{ id: "c0", full: true, tree: frame }] });
  window_run(p, [["add_class", { to: { sel: "#m" }, names: ["sel"] }], ["transition", { to: { sel: "#m" }, transition: ["fade"], time: 100 }]]);
  p.server({ type: "patch", trees: [{ id: "c0", tree: { u: { 0: "2" } } }], js: [{ id: "c0", ops: [["set_attr", { to: { sel: "#m" }, name: "data-n", value: "2" }]] }] });
  const m = p.$("#m");
  eq([m.className, m.textContent, m.getAttribute("data-n")], ["m sel fade", "2", "2"], "kept through a v2 patch");
  p.tick(200);
  eq(m.className, "m sel", "the transition's classes went");
});

test("a branch that changes element keeps the field after it, focus and all", () => {
  const p = joined(`<div data-vk-id="c1"><p>who?</p><input name="name"></div>`);
  const input = p.$("input");
  input.focus();
  p.server({ type: "patch", updates: [{ id: "c1", html: `<div data-vk-id="c1"><h1>Hello, A!</h1><input name="name"></div>` }] });
  eq([p.$("input") === input, p.doc.activeElement === input, p.$("h1").textContent, p.doc.querySelectorAll("p").length], [true, true, "Hello, A!", 0], "error → heading");
  p.server({ type: "patch", updates: [{ id: "c1", html: `<div data-vk-id="c1"><p>who?</p><input name="name"></div>` }] });
  eq([p.$("input") === input, p.doc.activeElement === input, p.doc.querySelectorAll("h1").length], [true, true, 0], "heading → error");
  p.server({ type: "patch", updates: [{ id: "c1", html: `<div data-vk-id="c1"><span>new</span><p>who?</p><input name="name"></div>` }] });
  eq([p.$("input") === input, p.$("span").textContent, p.$("p").textContent], [true, "new", "who?"], "an inserted node keeps the ones after it");
});

test("a focused field keeps what the user types, and takes a value the server changed since what it sent", () => {
  const html = (v, n) => `<form data-vk-id="c1" data-vk-on-submit="c1:0"><input name="text" value="${v}" data-vk-on-input="c1:1"><p>${n}</p></form>`;
  const p = joined(html("", 0));
  const input = p.$("input");
  input.focus();
  input.value = "hel";
  p.fire("input", "input");
  input.value = "hello";
  p.server({ type: "patch", updates: [{ id: "c1", html: html("hel", 1) }] });
  eq(input.value, "hello", "an echo of what was sent while typing on");
  p.fire("input", "input");
  p.server({ type: "patch", updates: [{ id: "c1", html: html("hello", 2) }] });
  eq(input.value, "hello", "an echo of what it shows");
  p.fire("form", "submit");
  p.server({ type: "patch", updates: [{ id: "c1", html: html("", 3) }] });
  eq([input.value, p.doc.activeElement === input], ["", true], "cleared once sent, focus kept");
  input.value = "x";
  p.fire("input", "input");
  input.value = "xy";
  p.server({ type: "patch", updates: [{ id: "c1", html: html("X", 4) }] });
  eq(input.value, "xy", "typed since: kept");
});

// Optimistic updates.
const sentRef = (p, i) => p.ws().sent[i].ref;
const errors = (p) => p.logs.filter((l) => l.startsWith("error"));

test("optimistic: predictions show at once; a reply keeps them while the server renders the same, the server's change wins", () => {
  const html = (n) => `<div data-vk-id="c1"><button class="like" data-vk-js-click='${js([["add_class", { names: ["liked"] }], ["set_text", { to: { sel: "#n" }, text: "6" }],
    ["push", { h: "c1:0", on_ok: [["add_class", { names: ["ok"] }]] }]])}'>like</button><b id="n">${n}</b></div>`;
  const p = joined(html(5));
  const b = () => p.$("button");
  p.fire("button", "click");
  eq([b().className, p.$("#n").textContent, b().hasAttribute("data-vk-pending"), b().getAttribute("aria-busy")], ["like liked", "6", true, "true"], "predicted");
  p.server({ type: "patch", ref: 99, updates: [{ id: "c1", html: html(5) }] }); // another reply: still pending
  eq([b().className, p.$("#n").textContent, b().hasAttribute("data-vk-pending")], ["like liked", "6", true], "kept through a patch");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [{ id: "c1", html: html(5) }] });
  eq([b().className, p.$("#n").textContent, b().hasAttribute("data-vk-pending"), b().hasAttribute("data-vk-failed")], ["like liked ok", "6", false, false], "the reply: on_ok on this");
  p.server({ type: "patch", updates: [{ id: "c1", html: html(7) }] });
  eq([p.$("#n").textContent, b().className], ["7", "like liked ok"], "the server's text wins");
});

test("optimistic: an error reply undoes attributes, classes, style, hidden, text and values, then runs on_error", () => {
  const body = `<div data-vk-id="c1"><button class="like on" data-vk-js-click='${js([
    ["add_class", { names: ["liked"] }], ["remove_class", { names: ["on"] }], ["set_attr", { name: "aria-pressed", value: "true" }],
    ["hide", { to: { sel: "#x" } }], ["show", { to: { sel: "#y" } }], ["set_text", { to: { sel: "#n" }, text: "6" }],
    ["set_value", { to: { sel: "#v" }, value: "new" }], ["set_value", { to: { sel: "#c" }, value: "true" }],
    ["push", { h: "c1:0", on_error: [["show", { to: { sel: "#err" } }]] }]])}'>like</button>
    <p id="x" style="color: red">x</p><p id="y" hidden>y</p><b id="n">5</b><input id="v" value="old"><input id="c" type="checkbox"><p id="err" hidden>failed</p></div>`;
  const p = joined(body);
  p.fire("button", "click");
  const st = () => [p.$("button").className, p.$("button").getAttribute("aria-pressed"), p.$("#x").getAttribute("style"), p.$("#y").hasAttribute("hidden"),
    p.$("#n").textContent, p.$("#v").value, p.$("#c").checked];
  eq(st(), ["like liked", "true", "color: red; display: none", false, "6", "new", true], "predicted");
  let detail;
  p.doc.addEventListener("vk:error", (e) => { detail = e.detail; });
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [], error: "nope" });
  eq(st(), ["like on", null, "color: red", true, "5", "old", false], "undone");
  eq([p.$("#err").hasAttribute("hidden"), p.$("button").hasAttribute("data-vk-failed"), p.$("button").hasAttribute("data-vk-pending"), detail.message], [false, true, false, "nope"], "on_error, failed, vk:error");
  p.server({ type: "patch", updates: [{ id: "c1", html: body.replace("5", "8") }] });
  eq([p.$("button").className, p.$("#n").textContent, p.$("button").hasAttribute("data-vk-failed")], ["like on", "8", true], "nothing left to reconcile; failed survives a morph");
});

test("optimistic: what the server changed before the error reply is the server's", () => {
  const html = (cls, n, title = "a") => `<div data-vk-id="c1"><button class="${cls}" title="${title}" data-vk-js-click='${js([["add_class", { names: ["liked"] }],
    ["set_attr", { name: "title", value: "b" }], ["set_text", { to: { sel: "#n" }, text: "6" }], ["push", { h: "c1:0" }]])}'>like</button><b id="n">${n}</b></div>`;
  const p = joined(html("like", "5"));
  p.fire("button", "click");
  p.server({ type: "patch", updates: [{ id: "c1", html: html("like big", "9") }] });
  eq([p.$("button").className, p.$("#n").textContent, p.$("button").title || p.$("button").getAttribute("title")], ["like big", "9", "b"], "the server's patch");
  p.server({ type: "error", ref: sentRef(p, 1), error: "nope" });
  eq([p.$("button").className, p.$("#n").textContent, p.$("button").getAttribute("title")], ["like big", "9", "a"], "class and text the server's, the title undone");
  const q = joined(html("like", "5"));
  q.fire("button", "click");
  q.server({ type: "patch", ref: sentRef(q, 1), error: "nope", updates: [{ id: "c1", html: html("like", "7") }] });
  eq([q.$("button").className, q.$("#n").textContent], ["like", "7"], "the error reply's own patch");
});

test("optimistic: hooks run on this, or the instance's root once it is gone", () => {
  const btn = `<button data-vk-js-click='${js([["push", { h: "c1:0", on_ok: [["add_class", { names: ["ok"] }]] }]])}'>x</button>`;
  const p = joined(`<div data-vk-id="c1">${btn}</div>`);
  p.fire("button", "click");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [{ id: "c1", html: `<div data-vk-id="c1"><p>gone</p></div>` }] });
  eq(p.$("[data-vk-id=c1]").className, "ok", "the root");
});

test("optimistic: no reply in time fails the push; a late reply is applied without its hooks", () => {
  const body = `<div data-vk-id="c1"><button data-vk-js-click='${js([["add_class", { names: ["liked"] }],
    ["push", { h: "c1:0", timeout: 500, on_ok: [["add_class", { names: ["ok"] }]], on_error: [["add_class", { names: ["err"] }]] }]])}'>x</button><b>one</b></div>`;
  const p = joined(body);
  p.fire("button", "click");
  p.tick(499);
  eq(p.$("button").className, "liked", "waiting");
  p.tick(1);
  eq([p.$("button").className, p.$("button").hasAttribute("data-vk-failed"), errors(p).length], ["err", true, 1], "timed out");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [{ id: "c1", html: body.replace("one", "two") }] });
  eq([p.$("button").className, p.$("b").textContent], ["err", "two"], "late reply");
});

test("optimistic: keep never undoes; hooks still run", () => {
  const p = joined(`<button data-vk-js-click='${js([["add_class", { names: ["liked"] }], ["push", { h: "c0:0", keep: true, on_error: [["add_class", { names: ["err"] }]] }]])}'>x</button>`);
  p.fire("button", "click");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [], error: "nope" });
  eq(p.$("button").className, "liked err");
});

test("optimistic: each push in a chain settles its own predictions", () => {
  const p = joined(`<button data-vk-js-click='${js([["add_class", { names: ["a"] }], ["push", { h: "c0:0" }], ["add_class", { names: ["b"] }], ["push", { h: "c0:1" }]])}'>x</button>`);
  p.fire("button", "click");
  eq(p.$("button").className, "a b", "both");
  p.server({ type: "patch", ref: sentRef(p, 2), updates: [] });
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [], error: "nope" });
  eq([p.$("button").className, p.$("button").hasAttribute("data-vk-pending")], ["b", false], "the first undone, the second kept");
});

test("optimistic: data-vk-pending while waiting, data-vk-failed until the next push", () => {
  const p = joined(`<div data-vk-id="c1"><button data-vk-js-click='${js([["push", { h: "c1:0" }]])}'>x</button></div>`);
  const b = () => p.$("button"), marks = () => [b().hasAttribute("data-vk-pending"), b().hasAttribute("data-vk-failed")];
  p.fire("button", "click");
  eq(marks(), [true, false], "pending");
  p.server({ type: "patch", ref: sentRef(p, 1), error: "nope", updates: [{ id: "c1", html: `<div data-vk-id="c1"><button>x</button></div>` }] });
  eq(marks(), [false, true], "failed");
  p.window.vuka.run(b(), [["push", { h: "c1:0" }]]);
  eq(marks(), [true, false], "pushed again");
  p.server({ type: "patch", ref: sentRef(p, 2), updates: [] });
  eq(marks(), [false, false], "done");
});

test("optimistic: a pushed chain's push", () => {
  const body = `<div data-vk-id="c1"><p>x</p></div>`;
  const p = joined(body);
  p.server({ type: "patch", updates: [], js: [{ id: "c1", ops: [["add_class", { names: ["saving"] }], ["push", { h: "c1:1", on_error: [["add_class", { names: ["err"] }]] }]] }] });
  eq([p.ws().sent[1].target, p.$("div").className], ["c1:1", "saving"], "sent");
  p.server({ type: "patch", ref: sentRef(p, 1), updates: [], error: "nope" });
  eq(p.$("div").className, "err", "undone");
});

test("optimistic: a push with no connection fails at once, one in flight when the connection drops too", () => {
  const ops = [["add_class", { names: ["liked"] }], ["push", { h: "c0:0", on_error: [["add_class", { names: ["err"] }]] }]];
  const p = open(`<button data-vk-js-click='${js(ops)}'>x</button>`, { live: true });
  p.fire("button", "click");
  eq([p.$("button").className, p.$("button").hasAttribute("data-vk-failed"), p.ws().sent.length], ["err", true, 0], "not joined");
  const q = joined(`<button data-vk-js-click='${js(ops)}'>x</button>`);
  q.fire("button", "click");
  q.ws().onclose({ code: 1006 });
  eq(q.$("button").className, "err", "dropped");
});

test("optimistic: a later command's change stays; a skipped push ends a segment", () => {
  const p = joined(`<button id="a" data-vk-js-click='${js([["set_attr", { to: { sel: "#t" }, name: "title", value: "b" }], ["push", { h: "c0:0" }]])}'>x</button>
    <button id="b" data-vk-js-click='${js([["set_attr", { to: { sel: "#t" }, name: "title", value: "c" }]])}'>x</button>
    <button id="c" data-vk-js-click='${js([["add_class", { names: ["x"] }], ["if", { cond: { exists: false } }], ["push", { h: "c0:1" }], ["end", {}],
      ["add_class", { names: ["y"] }], ["push", { h: "c0:2" }]])}'>x</button><p id="t" title="a"></p>`);
  p.fire("#a", "click");
  p.fire("#b", "click");
  p.server({ type: "patch", ref: sentRef(p, 1), error: "nope" });
  eq(p.$("#t").getAttribute("title"), "c", "the later command's");
  p.fire("#c", "click");
  eq(p.ws().sent[2].target, "c0:2", "the skipped push not sent");
  p.server({ type: "patch", ref: sentRef(p, 2), error: "nope" });
  eq(p.$("#c").className, "x", "only y undone");
});

test("call: reads that test a condition or find nothing; vk:error on this", () => {
  const p = open(`<input id="q" value="a"><button data-vk-js-click='${js([["call", { fn: "f", args: [{ $read: { to: { sel: "#q" }, value_eq: "a" } }, { $read: { to: { sel: "#none" }, value: true } },
    { $read: { to: { sel: "#none" }, exists: true } }, { $read: 1, x: 2 }] }], ["call", { fn: "g" }]])}'>x</button>`);
  let got, detail, at;
  p.window.vuka.register("f", (ctx, ...a) => { got = a; });
  p.window.vuka.register("g", () => { throw new Error("bang"); });
  p.doc.addEventListener("vk:error", (e) => { detail = e.detail; at = e.target.tagName; });
  p.fire("button", "click");
  eq([got, detail, at], [[true, null, false, { $read: 1, x: 2 }], { error: "bang", fn: "g" }, "BUTTON"]);
});

// Calling JavaScript.
const flush = () => new Promise((r) => setImmediate(r));

test("call: a registered function with the element, the root and read args", () => {
  const p = open(`<div data-vk-id="c1"><input id="q" value="hi"><input id="c" type="checkbox" checked>
    <button data-x="7" data-vk-js-click='${js([["call", { fn: "mark", args: [1, { a: "b" }, { $read: { to: { sel: "#q" }, value: true } },
      { $read: { to: { sel: "#c" }, checked: true } }, { $read: { attr: "data-x" } }] }], ["call", { fn: "mark", to: { sel: "#q" } }], ["add_class", { names: ["after"] }]])}'>x</button></div>`);
  const got = [];
  p.window.vuka.register("mark", (ctx, ...args) => { got.push([ctx.el.tagName, ctx.root.getAttribute("data-vk-id"), ctx.event && ctx.event.type, ctx.target({ sel: "#c" }).length, ...args]); });
  p.fire("button", "click");
  eq(got, [["BUTTON", "c1", "click", 1, 1, { a: "b" }, "hi", true, "7"], ["INPUT", "c1", "click", 1]]);
  eq(p.$("button").className, "after");
});

test("call: the chain waits for a promise; false stops it; registered before the runtime loaded", async () => {
  let done;
  const stub = { q: [], register(n, f) { this.q.push([n, f]); } };
  stub.register("wait", () => new Promise((r) => { done = r; }));
  const p = open(`<button id="a" data-vk-js-click='${js([["call", { fn: "wait" }], ["add_class", { names: ["after"] }]])}'>x</button>
    <button id="b" data-vk-js-click='${js([["call", { fn: "no" }], ["add_class", { names: ["after"] }]])}'>x</button>
    <button id="c" data-vk-js-click='${js([["call", { fn: "later" }], ["add_class", { names: ["after"] }]])}'>x</button>`,
    { vuka: stub });
  p.window.vuka.register("no", () => false);
  p.window.vuka.register("later", () => Promise.resolve(false));
  p.fire("#a", "click");
  await flush();
  eq(p.$("#a").className, "", "waiting");
  done();
  await flush();
  eq(p.$("#a").className, "after", "resolved");
  p.fire("#b", "click");
  p.fire("#c", "click");
  await flush();
  eq([p.$("#b").className, p.$("#c").className], ["", ""], "stopped");
});

test("call: a throw stops the chain and fails the push it predicts for, unsent", async () => {
  const p = joined(`<button id="a" data-vk-js-click='${js([["add_class", { names: ["liked"] }], ["call", { fn: "boom" }], ["push", { h: "c0:0", on_error: [["add_class", { names: ["err"] }]] }]])}'>x</button>
    <button id="b" data-vk-js-click='${js([["add_class", { names: ["liked"] }], ["call", { fn: "reject" }], ["push", { h: "c0:0", on_error: [["add_class", { names: ["err"] }]] }]])}'>x</button>`);
  p.window.vuka.register("boom", () => { throw new Error("bang"); });
  p.window.vuka.register("reject", () => Promise.reject(new Error("later bang")));
  let n = 0;
  p.doc.addEventListener("vk:error", () => n++);
  p.fire("#a", "click");
  eq([p.$("#a").className, p.$("#a").hasAttribute("data-vk-failed"), p.ws().sent.length, n], ["err", true, 1, 1], "throw");
  p.fire("#b", "click");
  await flush();
  eq([p.$("#b").className, p.ws().sent.length, n], ["err", 1, 2], "rejection");
});

test("call: a dotted path on window; an unknown function stops the chain", () => {
  const p = open(`<button id="a" data-vk-js-click='${js([["call", { fn: "Picker.refresh", args: [2] }], ["add_class", { names: ["after"] }]])}'>x</button>
    <button id="b" data-vk-js-click='${js([["call", { fn: "Picker.nope" }], ["add_class", { names: ["after"] }]])}'>x</button>
    <button id="c" data-vk-js-click='${js([["call", { fn: "alert('x')" }], ["add_class", { names: ["after"] }]])}'>x</button>`);
  p.window.Picker = { n: 0, refresh(ctx, k) { this.n += k; } };
  p.fire("#a", "click");
  eq([p.window.Picker.n, p.$("#a").className], [2, "after"], "called with this");
  p.fire("#b", "click");
  p.fire("#c", "click");
  eq([p.$("#b").className, p.$("#c").className, p.logs.filter((l) => l.startsWith("warn")).length, errors(p).length], ["", "", 2, 2], "unknown");
});

// Navigation: data-vk-link, the patch and navigate commands, popstate.
const shelf = (n) => `<main><div data-vk-id="c1"><p id="at">page ${n}</p>
  <a id="next" href="/shelf?page=${n + 1}" data-vk-link="patch">next</a>
  <a id="book" href="/books/7#notes" data-vk-link><b>a book</b></a>
  <a id="tab" href="/books/8" target="_blank" data-vk-link>tab</a>
  <a id="away" href="https://elsewhere.test/x" data-vk-link>away</a>
  <a id="jump" href="/shelf?page=${n}#at" data-vk-link>jump</a>
  <button id="inc" data-vk-on-click="c1:0">+</button>
  <button id="js" data-vk-js-click='${js([["navigate", { href: "/books/9" }]])}'>go</button></div></main>`;
const book = (id) => `<main><div data-vk-id="c2"><h1 id="notes">book ${id}</h1><a id="shelf" href="/shelf?page=2" data-vk-link>shelf</a></div></main>`;
const navs = (p) => p.ws().sent.filter((m) => m.type === "navigate").map(({ ref, ...m }) => m);
const lastRef = (p) => p.ws().sent[p.ws().sent.length - 1].ref;

test("navigation: a patch link sends navigate, then the address follows the reply", () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  const seen = [];
  p.window.addEventListener("vk:navigate", (e) => seen.push(e.detail));
  const e = p.fire("#next", "click", { button: 0 });
  eq([e.defaultPrevented, navs(p)], [true, [{ type: "navigate", url: "/shelf?page=2", patch: true }]], "sent");
  eq(p.doc.documentElement.getAttribute("data-vk-navigating"), "/shelf?page=2", "in flight, to where");
  p.fire("#inc", "click");
  eq(p.ws().sent.filter((m) => m.type === "event").length, 0, "no events while a navigation is in flight");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(2).slice(6, -7) }], url: "/shelf?page=2", token: "t2" });
  eq([p.$("#at").textContent, p.location.href, p.entries, p.doc.documentElement.hasAttribute("data-vk-navigating")],
    ["page 2", "http://test/shelf?page=2", ["http://test/shelf?page=2"], false], "applied");
  eq([seen, p.scrolls, p.doc.activeElement === p.doc.body], [[{ url: "http://test/shelf?page=2", patch: true }], [], true], "a patch keeps scroll and focus");
  p.ws().onclose({ code: 1006 });
  p.tick(20000);
  eq(p.ws().url.endsWith("?t=t2"), true, "a reconnect joins the page the navigation reached");
});

test("navigation: a navigate scrolls to its #hash or the top, focuses main, drops the old page's pushes", () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  p.$("[data-vk-id=c1]").setAttribute("data-vk-js-like", js([["add_class", { names: ["liked"] }], ["push", { h: "c1:5", on_error: [["add_class", { to: { sel: "main" }, names: ["failed"] }]] }]]));
  p.fire("[data-vk-id=c1]", "like");
  p.fire("#book b", "click");
  eq(navs(p), [{ type: "navigate", url: "/books/7" }], "the hash stays in the browser");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c0", html: book(7) }], url: "/books/7", token: "t2" });
  eq([p.location.href, p.$("h1").textContent, p.doc.scrolled.length && p.doc.scrolled[0][0].id, p.doc.activeElement.tagName, p.$("main").getAttribute("tabindex")],
    ["http://test/books/7#notes", "book 7", "notes", "MAIN", "-1"], "arrived");
  p.server({ type: "patch", ref: p.ws().sent[1].ref, error: "too late" });
  eq(p.$("main").className, "", "the dropped push's hooks don't run");
  p.fire("#shelf", "click");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c0", html: shelf(2) }], url: "/shelf?page=2" });
  eq(p.scrolls, [[0, 0]], "the top");
});

test("navigation: what the runtime leaves to the browser", () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  for (const [sel, init] of [["#next", { ctrlKey: true }], ["#next", { metaKey: true }], ["#next", { shiftKey: true }], ["#next", { button: 1 }], ["#tab", {}], ["#away", {}], ["#jump", {}]]) {
    eq(p.fire(sel, "click", init).defaultPrevented, false, sel + " " + JSON.stringify(init));
  }
  eq(navs(p), [], "nothing sent");
  const off = open(shelf(1), { url: "http://test/shelf?page=1" });
  eq(off.fire("#next", "click").defaultPrevented, false, "a page that isn't live");
  off.fire("#js", "click");
  eq(off.loads, [["assign", "http://test/books/9"]], "a command on a page that isn't live loads");
});

test("navigation: a failure, a redirect or a lost connection is a page load", () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  p.fire("#js", "click");
  eq(navs(p), [{ type: "navigate", url: "/books/9" }], "the command");
  p.server({ type: "redirect", ref: lastRef(p), url: "/report.pdf" });
  p.fire("#next", "click");
  p.server({ type: "error", ref: lastRef(p), error: "no" });
  p.fire("#book", "click");
  p.ws().onclose({ code: 1006 });
  eq(p.loads, [["assign", "http://test/report.pdf"], ["assign", "http://test/shelf?page=2"], ["assign", "http://test/books/7#notes"]], "loads");
  const q = joined(shelf(1), { url: "http://test/shelf?page=1" });
  q.fire("#next", "click");
  q.tick(10000);
  eq(q.loads, [["assign", "http://test/shelf?page=2"]], "no reply in time");
});

test("navigation: back and forward patch within a page, navigate across pages, restore scroll", () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  const first = p.history.state;
  p.fire("#next", "click");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(2).slice(6, -7) }], url: "/shelf?page=2" });
  const second = p.history.state;
  eq(second.vk, first.vk, "a patch's entry is the same page's");
  p.fire("#book", "click");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c0", html: book(7) }], url: "/books/7" });
  eq(p.history.state.vk !== first.vk, true, "a navigate's is another page's");
  p.pop("/shelf?page=2", { ...second, x: 0, y: 120 });
  eq(navs(p).pop(), { type: "navigate", url: "/shelf?page=2" }, "back to the other page: navigate");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c0", html: shelf(2) }], url: "/shelf?page=2" });
  eq([p.scrolls.pop(), p.history.state.vk, p.entries.length], [[0, 120], first.vk, 2], "scroll back, the entry kept");
  p.pop("/shelf?page=1", first);
  eq(navs(p).pop(), { type: "navigate", url: "/shelf?page=1", patch: true }, "within the page: patch");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(1).slice(6, -7) }], url: "/shelf?page=1" });
  eq([p.$("#at").textContent, p.entries.length], ["page 1", 2], "patched back");
  p.pop("/shelf?page=1#at", null);
  eq(navs(p).length, 4, "a #hash entry of the page shown: nothing");
});

// Only http and https URLs are loaded or navigated to: a redirect from the
// server, a command, a fallback (an open redirect's javascript: URL).
test("navigation: a URL that isn't http or https is refused", () => {
  const bad = ["javascript:void(window.pwned=1)", " JavaScript:alert(1)", "java\tscript:alert(1)", "data:text/html,<script>alert(1)</script>", "vbscript:msgbox(1)"];
  for (const href of bad) {
    const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
    const errors = [];
    p.doc.addEventListener("vk:error", (e) => errors.push(e.detail.message));
    p.fire("#book", "click");
    p.server({ type: "redirect", ref: lastRef(p), url: href });
    p.server({ type: "redirect", url: href });
    window_run(p, [["navigate", { href }], ["patch", { href }]]);
    eq([p.loads, navs(p).length, errors.length], [[], 1, 4], href);
    const off = open(`<button data-vk-js-click='${js([["navigate", { href }]])}'></button>`, { url: "http://test/" });
    off.fire("button", "click");
    eq(off.loads, [], href + " on a page that isn't live");
  }
});

// window.vuka.navigate: what an island's live.navigate / live.patch and
// VukaLink use.
const settled = (pr) => { const s = { v: "pending" }; pr.then((r) => { s.v = r; }, (err) => { s.v = "rejected " + err.message; }); return s; };
const flushed = () => new Promise((r) => setImmediate(r));

test("navigate: over the connection as a link; the promise settles once applied", async () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  const seen = [];
  p.window.addEventListener("vk:navigate", (e) => seen.push(e.detail.patch));
  const pr = settled(p.window.vuka.navigate("/shelf?page=2", { patch: true }));
  eq(navs(p), [{ type: "navigate", url: "/shelf?page=2", patch: true }], "sent as a patch link");
  await flushed();
  eq(pr.v, "pending", "not before the reply");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(2).slice(6, -7) }], url: "/shelf?page=2" });
  await flushed();
  eq([pr.v, p.location.href, p.entries, seen], [{ url: "http://test/shelf?page=2", patch: true }, "http://test/shelf?page=2", ["http://test/shelf?page=2"], [true]], "applied");
  const nv = settled(p.window.vuka.navigate("/books/7"));
  eq(navs(p).pop(), { type: "navigate", url: "/books/7" }, "a navigate");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c0", html: book(7) }], url: "/books/7" });
  await flushed();
  eq([nv.v, p.scrolls], [{ url: "http://test/books/7", patch: false }, [[0, 0]]], "a new page: the top");
});

test("navigate: replace takes the current entry's place; a later move supersedes", async () => {
  const p = joined(shelf(1), { url: "http://test/shelf?page=1" });
  const first = settled(p.window.vuka.navigate("/shelf?page=2", { patch: true, replace: true }));
  const second = settled(p.window.vuka.navigate("/shelf?page=3", { patch: true, replace: true }));
  await flushed();
  eq(first.v, "rejected live: another navigation took its place", "superseded");
  p.server({ type: "patch", ref: lastRef(p) - 1, updates: [{ id: "c1", html: shelf(2).slice(6, -7) }], url: "/shelf?page=2" });
  eq(p.location.href, "http://test/shelf?page=1", "the old reply moves nothing");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(3).slice(6, -7) }], url: "/shelf?page=3" });
  await flushed();
  eq([second.v.url, p.location.href, p.entries, p.history.state.vk !== undefined], ["http://test/shelf?page=3", "http://test/shelf?page=3", [], true], "replaced, no entry added");
  p.$("#next").setAttribute("data-vk-replace", "");
  p.fire("#next", "click");
  p.server({ type: "patch", ref: lastRef(p), updates: [{ id: "c1", html: shelf(4).slice(6, -7) }], url: "/shelf?page=4" });
  eq([p.location.href, p.entries], ["http://test/shelf?page=4", []], "a link with data-vk-replace");
  const lost = settled(p.window.vuka.navigate("/books/7", { replace: true }));
  p.ws().onclose({ code: 1006 });
  await flushed();
  eq([p.loads, lost.v], [[["replace", "http://test/books/7"]], "pending"], "a lost connection: a page load, replacing");
});

test("navigate: no connection, not joined or another origin is a page load", async () => {
  const off = open(shelf(1), { url: "http://test/shelf?page=1" });
  const a = settled(off.window.vuka.navigate("/books/7"));
  off.window.vuka.navigate("/books/8", { replace: true, patch: true });
  off.$("#next").setAttribute("data-vk-replace", "");
  eq(off.fire("#next", "click").defaultPrevented, true, "a replace link on a page that isn't live");
  await flushed();
  eq([off.loads, a.v], [[["assign", "http://test/books/7"], ["replace", "http://test/books/8"], ["replace", "http://test/shelf?page=2"]], "pending"], "loads; the page leaves");
  const p = open(shelf(1), { live: true, url: "http://test/shelf?page=1" });
  p.window.vuka.navigate("/books/7");
  const q = joined(shelf(1), { url: "http://test/shelf?page=1" });
  q.window.vuka.navigate("https://elsewhere.test/x");
  const h = settled(q.window.vuka.navigate("/shelf?page=1#at"));
  await flushed();
  eq([p.loads, q.loads, navs(q), q.location.hash, h.v], [[["assign", "http://test/books/7"]], [["assign", "https://elsewhere.test/x"]], [], "#at", { url: "http://test/shelf?page=1#at", patch: true }], "not joined, elsewhere, a #hash");
});

test("navigate: a URL that isn't http or https is refused and rejects", async () => {
  for (const href of ["javascript:void(window.pwned=1)", " JavaScript:alert(1)", "data:text/html,x", "vbscript:x"]) {
    for (const p of [joined(shelf(1), { url: "http://test/shelf?page=1" }), open(shelf(1))]) {
      const errors = [];
      p.doc.addEventListener("vk:error", (e) => errors.push(e.detail.message));
      const a = settled(p.window.vuka.navigate(href)), b = settled(p.window.vuka.navigate(href, { patch: true, replace: true }));
      await flushed();
      eq([a.v, b.v, p.loads, errors.length, p.window.pwned], ["rejected live: refused to load a URL that isn't http or https", "rejected live: refused to load a URL that isn't http or https", [], 2, undefined], href);
    }
  }
});

test("attributes: no event handlers, srcdoc or script URLs", () => {
  const p = open(`<button data-vk-js-click='${js([
    ["set_attr", { to: { sel: "#a" }, name: "onmouseover", value: "window.pwned=1" }],
    ["set_attr", { to: { sel: "#a" }, name: "ONCLICK", value: "window.pwned=1" }],
    ["toggle_attr", { to: { sel: "#a" }, name: "onfocus", values: ["a()", "b()"] }],
    ["remove_attr", { to: { sel: "#a" }, name: "onblur" }],
    ["set_attr", { to: { sel: "#f" }, name: "srcdoc", value: "<script>alert(1)</script>" }],
    ["set_attr", { to: { sel: "#a" }, name: "href", value: "javascript:alert(1)" }],
    ["set_attr", { to: { sel: "#a" }, name: "HREF", value: " java\nscript:alert(1)" }],
    ["set_attr", { to: { sel: "#f" }, name: "src", value: "data:text/html,<script>alert(1)</script>" }],
    ["set_attr", { to: { sel: "#b" }, name: "formaction", value: "vbscript:x" }],
    ["set_attr", { to: { sel: "#u" }, name: "xlink:href", value: "javascript:alert(1)" }],
    ["toggle_attr", { to: { sel: "#v" }, name: "poster", values: ["/ok.png", "javascript:x"] }],
    ["set_attr", { to: { sel: "#a" }, name: "title", value: "javascript: is fine here" }],
    ["set_attr", { to: { sel: "#b" }, name: "formaction", value: "/save" }],
  ])}'></button><a id="a" onblur="keep()"></a><iframe id="f"></iframe><button id="b"></button><use id="u"></use><video id="v"></video>`);
  p.fire("button", "click");
  const a = p.$("#a");
  eq([a.getAttribute("onmouseover"), a.getAttribute("onclick"), a.getAttribute("onfocus"), a.getAttribute("onblur"), a.getAttribute("href"),
    p.$("#f").getAttribute("srcdoc"), p.$("#f").getAttribute("src"), p.$("#u").getAttribute("xlink:href"), p.$("#v").getAttribute("poster"),
    a.getAttribute("title"), p.$("#b").getAttribute("formaction")],
    [null, null, null, "keep()", null, null, null, null, null, "javascript: is fine here", "/save"]);
});

test("an idle page (4002) connects again once it is in view", () => {
  const p = joined(`<p>x</p>`);
  p.ws().onclose({ code: 4002 });
  eq([p.sockets.length, p.doc.documentElement.getAttribute("data-vk-state")], [2, "connecting"], "in view: at once");
  p.doc.visibilityState = "hidden";
  p.ws().onclose({ code: 4002 });
  eq([p.sockets.length, p.doc.documentElement.getAttribute("data-vk-state")], [2, "idle"], "hidden: waits");
  p.tick(60000);
  eq(p.sockets.length, 2, "no retry while hidden");
  p.doc.visibilityState = "visible";
  p.doc.dispatchEvent(new p.window.Event("visibilitychange"));
  eq(p.sockets.length, 3, "shown");
});

function window_run(p, ops) { p.window.vuka.run(p.doc.body, ops); }

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
