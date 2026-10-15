// Tests of the island runtime (loader.js with live.js) in node, over dom.js:
// node islands.js live.js ../../loader.js — function props, state props
// (two-way), live props and once.
"use strict";
const fs = require("fs");
const { page } = require("./dom.js");
const [file, loaderFile] = process.argv.slice(2);
const loaderSrc = fs.readFileSync(loaderFile, "utf8");
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };
const attr = (v) => JSON.stringify(v).replace(/&/g, "&amp;").replace(/'/g, "&#39;");

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}

// island is a vanilla island module recording what it is given.
function island(log) {
  const it = {};
  return {
    it,
    mod: {
      mount(el, props, ctx) {
        it.el = el; it.props = props; it.ctx = ctx;
        log.push(["mount", Object.keys(props).sort()]);
        return { update(p) { it.props = p; log.push(["update", p]); }, unmount() { log.push(["unmount"]); } };
      },
    },
  };
}

const host = (props, extra = "") => `<vk-island data-vk-s="p#1" data-vk-island="Pick" data-vk-client="load" data-vk-props='${attr(props)}'${extra}></vk-island>`;
const info = { m: {}, i: { Pick: ["/pick.js", ""] } };

function joined(body, mod) {
  const p = page(file, body, { live: true, scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod } });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: body });
  return p;
}

const n = { s: "c0:n:1", i: 1 };

test("a function prop pushes the host's handler with its first argument", async () => {
  const log = [], { it, mod } = island(log);
  const p = joined(`<main>${host({ day: "mon", onSelect: { $vk: "fn", e: "onselect" } }, ` data-vk-on-onselect="c1:3"`)}</main>`, mod);
  await settle();
  eq(typeof it.props.onSelect, "function", "a function");
  it.props.onSelect("tue", "ignored");
  const sent = p.ws().sent.filter((m) => m.type === "event");
  eq(sent.map((m) => [m.target, m.event, m.data]), [["c1:3", "emit", "tue"]], "the push");
});

test("a function prop of commands runs them on the host", async () => {
  const log = [], { it, mod } = island(log);
  const p = joined(`<main>${host({ onClose: { $vk: "fn", e: "onclose" } }, ` data-vk-js-onclose='${attr([["add_class", { names: ["closed"] }]])}'`)}</main>`, mod);
  await settle();
  await it.props.onClose();
  eq(p.$("vk-island").className, "closed", "ran");
});

test("a state prop: the island gets the value, follows the page's writes, writes back", async () => {
  const log = [], { it, mod } = island(log);
  const body = `<main><vk-t data-vk-b='${attr(n)}'>1</vk-t><button data-vk-js-click='${attr([["state_set", { ...n, v: 7 }]])}'>7</button>${host({ count: { $vk: "state", ...n } })}</main>`;
  const p = joined(body, mod);
  await settle();
  eq(it.props.count, 1, "initial");
  eq(it.ctx.bound, ["count"], "bound");
  p.fire("button", "click");
  await settle();
  eq(log.filter((l) => l[0] === "update"), [["update", { count: 7 }]], "page write → one update");
  it.ctx.set("count", 9);
  await settle();
  eq(p.$("vk-t").textContent, "9", "island write → the page's binding");
  eq(log.filter((l) => l[0] === "update").length, 2, "and the island follows");
});

test("a derived state prop is read only", async () => {
  const log = [], { it, mod } = island(log);
  const p = joined(`<main><vk-t data-vk-b='${attr(n)}'>1</vk-t>${host({ double: { $vk: "state", x: ["+", n, n] } })}</main>`, mod);
  await settle();
  eq(it.props.double, 2, "value");
  eq(it.ctx.bound, [], "not bound");
  it.ctx.set("double", 10);
  await settle();
  eq(p.$("vk-t").textContent, "1", "no write");
});

test("dates are revived", async () => {
  const log = [], { it, mod } = island(log);
  joined(`<main>${host({ when: { $vk: "date", v: "2026-10-15T09:00:00Z" }, list: [{ at: { $vk: "date", v: "2026-10-16T00:00:00Z" } }] })}</main>`, mod);
  await settle();
  eq([Object.prototype.toString.call(it.props.when) === "[object Date]", it.props.when.toISOString(), it.props.list[0].at.getUTCDate()], [true, "2026-10-15T09:00:00.000Z", 16]);
});

test("live props: a patch with new server props updates without a remount; the same props, state or function props don't", async () => {
  const log = [], { it, mod } = island(log);
  const props = (t) => ({ title: t, count: { $vk: "state", ...n }, onSelect: { $vk: "fn", e: "onselect" } });
  const p = joined(`<main>${host(props("a"), ` data-vk-on-onselect="c1:3"`)}</main>`, mod);
  await settle();
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${host(props("a"), ` data-vk-on-onselect="c1:9"`)}</main>` }] });
  await settle();
  eq(log, [["mount", ["count", "onSelect", "title"]]], "nothing changed for the island");
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${host(props("b"), ` data-vk-on-onselect="c1:9"`)}</main>` }] });
  await settle();
  eq(log.map((l) => l[0]), ["mount", "update"], "one update");
  eq(it.props.title, "b", "the new prop");
  eq(it.props.count, 1, "the state's value, kept");
  it.props.onSelect("x");
  eq(p.ws().sent.filter((m) => m.type === "event").map((m) => m.target), ["c1:9"], "the function follows the host's handler");
});

test("once keeps the first props", async () => {
  const log = [], { mod } = island(log);
  const p = joined(`<main>${host({ title: "a" }, " data-vk-once")}</main>`, mod);
  await settle();
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${host({ title: "b" }, " data-vk-once")}</main>` }] });
  await settle();
  eq(log.map((l) => l[0]), ["mount"], "no update");
});

test("an island that leaves unmounts and stops following its states", async () => {
  const log = [], { mod } = island(log);
  const body = `<main><button data-vk-js-click='${attr([["state_set", { ...n, v: 3 }]])}'>3</button>${host({ count: { $vk: "state", ...n } })}</main>`;
  const p = joined(body, mod);
  await settle();
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main><button data-vk-js-click='${attr([["state_set", { ...n, v: 3 }]])}'>3</button></main>` }] });
  await settle();
  p.fire("button", "click");
  await settle();
  eq(log.map((l) => l[0]), ["mount", "unmount"]);
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
