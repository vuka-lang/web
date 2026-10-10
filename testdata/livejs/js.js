// Tests of live.js's JS commands (data-vk-js-*, and commands a render or
// patch carries) in node, over dom.js: node js.js live.js
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

test("conditions read the first element; refs on a static page are the document's", () => {
  const p = open(`<input class="c" type="checkbox"><input class="c" type="checkbox" checked><p data-vk-ref="r"></p><div data-vk-id="c1"><p data-vk-ref="r"></p></div>
    <button data-vk-js-click='${js([["if", { cond: { to: { sel: ".c" }, checked: true } }], ["add_class", { names: ["any"] }], ["end", {}],
      ["if", { cond: { to: { sel: ".none" }, value: false } }], ["add_class", { names: ["missing"] }], ["end", {}],
      ["set_value", { to: { sel: ".c" }, value: "true" }], ["add_class", { to: { ref: "r" }, names: ["r"] }]])}'></button>`);
  p.fire("button", "click");
  eq([p.$("button").className, p.doc.querySelectorAll(".c").map((c) => c.checked), p.doc.querySelectorAll(".r").length], ["", [true, true], 2]);
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
  eq(p.fire("form", "submit").defaultPrevented, true, "a submit's commands prevent it");
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
