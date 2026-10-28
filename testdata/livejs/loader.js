// Tests of the component loader (loader.js) with live.js in node, over
// dom.js: node loader.js live.js ../../loader.js
"use strict";
const fs = require("fs");
const { page } = require("./dom.js");
const [file, loaderFile] = process.argv.slice(2);
const loaderSrc = fs.readFileSync(loaderFile, "utf8");
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const js = (ops) => JSON.stringify(ops).replace(/&/g, "&amp;").replace(/'/g, "&#39;");
const settle = () => new Promise((r) => setImmediate(r));

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}

// hooks makes a component's hook object recording each call in log.
function hooks(name, log, extra = {}) {
  const at = (el) => el.getAttribute("data-vk-id") || el.id || el.tagName.toLowerCase();
  return {
    mounted(el, ctx) { ctx.at = at(el); log.push(name + " mounted " + ctx.at); ctx.seen = 0; },
    updated(el, ctx) { log.push(name + " updated " + ctx.at + " " + ++ctx.seen); },
    destroyed(el, ctx) { log.push(name + " destroyed " + ctx.at); },
    ...extra,
  };
}

// open opens a page with the loader, its manifest info and modules: each
// module a list of hook objects (a component's scripts, in order).
function open(body, info, modules, o = {}) {
  const mods = {};
  for (const url in modules) mods[url] = { default: modules[url] };
  const src = loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info));
  return page(file, body, { ...o, scripts: [src], modules: mods });
}

function joined(body, info, modules, o = {}) {
  const p = open(body, info, modules, { live: true, ...o });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: body });
  return p;
}

const counter = (id, n) => `<div data-vk-s="k1" data-vk-id="${id}"><output data-vk-s="k1">${n}</output><p data-vk-s="k1" data-vk-ref="note">n</p></div>`;

test("mounted once per instance on load; each script's hooks in order; the join updates nothing", async () => {
  const log = [];
  const body = `<main data-vk-s="l1">${counter("c1", 0)}<section data-vk-s="l1">${counter("c2", 5)}</section></main><b data-vk-s="other"></b>`;
  const p = joined(body, { m: { k1: "/k1.js", l1: "/l1.js" } },
    { "/k1.js": [hooks("a", log), hooks("b", log)], "/l1.js": [hooks("layout", log)] });
  await settle();
  eq(log, ["layout mounted main", "a mounted c1", "b mounted c1", "a mounted c2", "b mounted c2"], "mounted, in the page's order");
  eq(p.window.vukaComponents.instances().map((i) => i.id), ["l1", "k1", "k1"], "the layout's passed-in section isn't an instance of its own");
});

test("a patch updates the instances it touches; one that leaves is destroyed; a new one mounts", async () => {
  const log = [];
  const p = joined(`<main>${counter("c1", 0)}${counter("c2", 0)}</main>`, { m: { k1: "/k1.js" } }, { "/k1.js": [hooks("k", log)] });
  await settle();
  log.length = 0;
  p.server({ type: "patch", ref: 0, updates: [{ id: "c1", html: counter("c1", 1) }] });
  eq(log, ["k updated c1 1"], "patched c1");
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${counter("c1", 2)}${counter("c3", 0)}</main>` }] });
  await settle();
  eq(log, ["k updated c1 1", "k destroyed c2", "k updated c1 2", "k mounted c3"], "a page's patch");
});

test("live navigation: the old page's instances leave, the new page's module loads and mounts", async () => {
  const log = [], loaded = [];
  const p = joined(`<main>${counter("c1", 0)}</main>`, { m: { k1: "/k1.js", s2: "/s2.js" } },
    { "/k1.js": [hooks("k", log)], "/s2.js": [hooks("shelf", log)] });
  await settle();
  p.fire(p.doc.body, "click"); // nothing
  p.window.vuka.run(p.doc.body, [["navigate", { href: "/shelf" }]]);
  const nav = p.ws().sent.find((m) => m.type === "navigate");
  p.server({ type: "patch", ref: nav.ref, url: "/shelf", updates: [{ id: "c0", html: `<main><ul data-vk-s="s2" data-vk-id="c4"></ul></main>` }] });
  await settle();
  eq(log, ["k mounted c1", "k destroyed c1", "shelf mounted c4"]);
  eq(p.location.pathname, "/shelf", "the address");
  loaded.push(...p.logs.filter((l) => l.startsWith("error")));
  eq(loaded, [], "no errors");
});

test("ctx: el, ref (its own), on (PushEvent, kept until the module loads), push", async () => {
  const got = [];
  const p = joined(`<main>${counter("c1", 0)}</main>`, { m: { k1: "/k1.js" } }, {
    "/k1.js": [{
      mounted(el, ctx) {
        got.push(["el", ctx.el === el, ctx.ref("note").tagName, ctx.ref("missing")]);
        ctx.on("bumped", (v) => got.push(["bumped", v]));
      },
    }],
  });
  // pushed with the join, before the module loaded
  p.server({ type: "patch", ref: 0, js: [{ id: "c1", ops: [["dispatch", { event: "vk:event", bubbles: false, detail: { name: "bumped", payload: { n: 1 } } }]] }] });
  await settle();
  p.server({ type: "patch", ref: 0, js: [{ id: "c1", ops: [["dispatch", { event: "vk:event", bubbles: false, detail: { name: "bumped", payload: 2 } }]] }] });
  eq(got, [["el", true, "P", null], ["bumped", { n: 1 }], ["bumped", 2]]);
  const inst = p.window.vukaComponents.instances()[0];
  const done = inst.ctx.push("Add", 5);
  const form = inst.ctx.push("Save", { title: "x", tags: ["a", "b"] });
  const [add, save] = p.ws().sent.filter((m) => m.event === "push");
  eq([add, save], [{ type: "event", target: "c1:Add", event: "push", value: "5", ref: add.ref },
    { type: "event", target: "c1:Save", event: "push", form: { title: ["x"], tags: ["a", "b"] }, ref: save.ref }], "sent");
  p.server({ type: "patch", ref: add.ref });
  p.server({ type: "error", ref: save.ref, error: "nope" });
  await done;
  await form.then(() => { throw new Error("resolved"); }, (err) => eq(err.message, "nope", "rejected"));
});

test("push waits for the join", async () => {
  const p = open(`<main>${counter("c1", 0)}</main>`, { m: { k1: "/k1.js" } }, { "/k1.js": [{ mounted(el, ctx) { ctx.push("Hi"); } }] }, { live: true });
  await settle();
  p.ws().onopen();
  eq(p.ws().sent.map((m) => m.type), ["join"], "not yet");
  p.server({ type: "render", ref: 1, html: `<main>${counter("c1", 0)}</main>` });
  eq(p.ws().sent.map((m) => m.target || m.type), ["join", "c1:Hi"], "after the join");
});

test("ui.JS.Call: the component's own method first, then the app's, then vuka.register's", async () => {
  const got = [];
  const click = (ops) => js(ops);
  const body = `<main>
    <div data-vk-s="k1" data-vk-id="c1"><button id="own" data-vk-s="k1" data-vk-js-click='${click([["call", { fn: "bump", args: [2] }], ["add_class", { names: ["after"] }]])}'></button>
      <div data-vk-s="k2" data-vk-id="c2"><button id="child" data-vk-s="k2" data-vk-js-click='${click([["call", { fn: "bump" }]])}'></button></div>
      <button id="app" data-vk-s="k1" data-vk-js-click='${click([["call", { fn: "shout" }]])}'></button>
    </div></main>`;
  const p = open(body, { m: { k1: "/k1.js", k2: "/k2.js" }, f: { k1: ["bump"], "": ["shout"] }, g: "/app.js" }, {
    "/k1.js": [{ bump(el, ctx, by) { got.push(["own", el.getAttribute("data-vk-id"), ctx.target.id, ctx.event.type, by]); } }],
    "/k2.js": [{}],
    "/app.js": [{ shout(el) { got.push(["app", el.tagName]); } }],
  }, { vuka: { q: [["bump", () => got.push(["registered"])]] } });
  await settle();
  p.fire("#own", "click");
  await settle();
  p.fire("#child", "click");
  p.fire("#app", "click");
  await settle();
  eq(got, [["own", "c1", "own", "click", 2], ["registered"], ["app", "BODY"]]);
  eq(p.$("#own").className, "after", "the chain went on once the method returned");
});

test("ref.all reads the current instance and watches changes after patches", async () => {
  let read;
  const log = [];
  const body = (n) => `<main data-vk-s="k1" data-vk-id="c1">${Array.from({ length: n }, (_, i) => `<input data-vk-s="k1" data-vk-ref="item" id="item${i}">`).join("")}</main>`;
  const p = joined(body(1), { m: { k1: "/k1.js" } }, { "/k1.js": [{
    mounted(el, ctx) { read = ctx.ref.all("item"); read.watch((xs) => log.push(xs.length)); const off = read.watch(() => log.push("stopped")); off(); },
  }] });
  await settle();
  eq(read().length, 1);
  p.server({ type: "patch", updates: [{ id: "c1", html: body(2) }] });
  eq(read().length, 2);
  eq(log, [2]);
  p.server({ type: "patch", updates: [{ id: "c1", html: body(2) }] });
  eq(log, [2], "unchanged elements don't notify");
  p.server({ type: "patch", updates: [{ id: "c1", html: body(0) }] });
  eq(read().length, 0);
  eq(log, [2, 0]);
});

test("listen removes early and on navigation; onDestroy runs once", async () => {
  const log = [];
  let target;
  const p = joined(counter("c1", 0), { m: { k1: "/k1.js" } }, { "/k1.js": [{
    mounted(el, ctx) {
      target = ctx.ref("note");
      ctx.listen(target, "click", () => log.push("click"));
      const off = ctx.listen(target, "click", () => log.push("early"));
      off(); off();
      ctx.onDestroy(() => log.push("destroy"));
    },
  }] });
  await settle();
  p.fire(target, "click");
  eq(log, ["click"]);
  p.window.vuka.run(p.doc.body, [["navigate", { href: "/users" }]]);
  const nav = p.ws().sent.find((m) => m.type === "navigate");
  p.server({ type: "patch", ref: nav.ref, url: "/users", updates: [{ id: "c0", html: "<main>users</main>" }] });
  await settle();
  p.fire(target, "click");
  eq(log, ["click", "destroy"]);
});

test("loop refs belong to their own instance even when nested scopes match", async () => {
  const seen = {};
  const body = `<main data-vk-s="k1" data-vk-id="c1"><input id="own" data-vk-s="k1" data-vk-ref="item"><section data-vk-s="k1" data-vk-id="c2"><input id="nested" data-vk-s="k1" data-vk-ref="item"></section></main>`;
  joined(body, { m: { k1: "/k1.js" } }, { "/k1.js": [{
    mounted(el, ctx) { seen[el.getAttribute("data-vk-id")] = ctx.ref.all("item")().map(x => x.id); },
  }] });
  await settle();
  eq(seen, { c1: ["own"], c2: ["nested"] });
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
    } catch (err) {
      failed++;
      console.log("FAIL " + t.name + ": " + err.message);
    }
  }
  console.log(`${tests.length} tests, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
