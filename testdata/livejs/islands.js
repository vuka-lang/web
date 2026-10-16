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

function joined(body, mod, o = {}) {
  const p = page(file, body, { live: true, scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, ...o });
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

test("live.navigate / live.patch and vuka.navigate / vuka.patch go over the connection; the old page's islands unmount, the new page's mount", async () => {
  const log = [], { it, mod } = island(log);
  const methods = ` data-vk-methods='["search"]' data-vk-on-m-search="c1:4"`;
  const p = joined(`<main>${host({ q: "" }, methods)}</main>`, mod, { url: "http://test/a" });
  await settle();
  const { live, vuka } = it.ctx;
  eq([typeof live.navigate, typeof live.patch, typeof live.view().navigate, typeof vuka.navigate, typeof vuka.patch], ["function", "function", "function", "function", "function"], "there");
  const patched = live.patch("/a?q=x", { replace: true });
  const nav = p.ws().sent.filter((m) => m.type === "navigate");
  eq(nav.map(({ ref, ...m }) => m), [{ type: "navigate", url: "/a?q=x", patch: true }], "a patch");
  p.server({ type: "patch", ref: nav[0].ref, updates: [{ id: "c0", html: `<main>${host({ q: "x" }, methods)}</main>` }] });
  eq(await patched, { url: "http://test/a?q=x", patch: true }, "resolved");
  await settle();
  eq([log.map((l) => l[0]), it.props.q, p.entries], [["mount", "update"], "x", []], "the same island updated, the entry replaced");
  const moved = vuka.navigate("/b");
  const nav2 = p.ws().sent.filter((m) => m.type === "navigate")[1];
  p.server({ type: "patch", ref: nav2.ref, updates: [{ id: "c0", html: `<main><section>${host({ q: "b" })}</section></main>` }] });
  await moved;
  await settle();
  eq([log.map((l) => l[0]), it.props, p.location.pathname], [["mount", "update", "unmount", "mount"], { q: "b" }, "/b"], "a new page's island");
  await vuka.navigate("javascript:alert(1)").then(() => { throw new Error("resolved"); }, (err) => eq(/refused/.test(err.message), true, "refused"));
});

test("a navigation morphing an island's host into another island's (name or live id): the old unmounts, the new mounts", async () => {
  const log = [], a = island(log), b = island(log);
  const at = (name, id, props) => `<main><vk-island data-vk-s="p#1" data-vk-island="${name}" data-vk-client="load" data-vk-id="${id}" data-vk-props='${attr(props)}'></vk-island></main>`;
  const two = { m: {}, i: { Pick: ["/pick.js", ""], Other: ["/other.js", ""] } };
  const p = page(file, at("Pick", "c1", { q: "a" }), { live: true, url: "http://test/a", scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(two))], modules: { "/pick.js": a.mod, "/other.js": b.mod } });
  p.ws().onopen();
  p.server({ type: "render", ref: 1, html: at("Pick", "c1", { q: "a" }) });
  await settle();
  const host = p.$("vk-island");
  p.window.vuka.navigate("/b");
  let nav = p.ws().sent.filter((m) => m.type === "navigate").pop();
  p.server({ type: "patch", ref: nav.ref, updates: [{ id: "c0", html: at("Other", "c2", { t: "b" }) }] });
  await settle();
  eq([p.$("vk-island") === host, log.map((l) => l[0]), b.it.props], [true, ["mount", "unmount", "mount"], { t: "b" }], "another island in the same host");
  p.window.vuka.navigate("/b2");
  nav = p.ws().sent.filter((m) => m.type === "navigate").pop();
  p.server({ type: "patch", ref: nav.ref, updates: [{ id: "c0", html: at("Other", "c3", { t: "c" }) }] });
  await settle();
  eq([log.map((l) => l[0]), b.it.props], [["mount", "unmount", "mount", "unmount", "mount"], { t: "c" }], "the same island, another instance: mounts afresh");
  p.window.vuka.navigate("/b2?x=1", { patch: true });
  nav = p.ws().sent.filter((m) => m.type === "navigate").pop();
  p.server({ type: "patch", ref: nav.ref, updates: [{ id: "c0", html: at("Other", "c3", { t: "d" }) }] });
  await settle();
  eq([log.map((l) => l[0]).length, log[log.length - 1][0], b.it.props], [6, "update", { t: "d" }], "a patch keeping the instance: updated");
});

test("without the live runtime's navigate, live.navigate and vuka.navigate load the page", async () => {
  const log = [], { it, mod } = island(log);
  const p = page(file, `<main>${host({ q: "" }, ` data-vk-call="/a/_call"`)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod } });
  await settle();
  delete p.window.vuka.navigate;
  it.ctx.live.navigate("/b");
  it.ctx.vuka.patch("/c", { replace: true });
  let refused = "";
  await it.ctx.vuka.navigate("data:text/html,x").catch((err) => { refused = err.message; });
  eq([p.loads, /refused/.test(refused)], [[["assign", "http://test/b"], ["replace", "http://test/c"]], true]);
});

test("a live page's methods: live events with their arguments; pending, error from the re-render", async () => {
  const log = [], { it, mod } = island(log);
  const methods = ` data-vk-methods='["search"]' data-vk-on-m-search="c1:4" data-vk-url='{"q":"q"}'`;
  const p = joined(`<main>${host({ q: "", rows: [] }, methods)}</main>`, mod, { url: "http://test/applicants?x=1" });
  await settle();
  const live = it.ctx.live, seen = [];
  live.subscribe((s) => { if (seen[seen.length - 1] !== s.pending) seen.push(s.pending); });
  const done = live.search("gr", 2);
  eq(live.pending, true, "pending");
  const ev = p.ws().sent.find((m) => m.type === "event");
  eq([ev.target, ev.event, ev.data], ["c1:4", "emit", ["gr", 2]], "sent");
  p.server({ type: "patch", ref: ev.ref, updates: [{ id: "c0", html: `<main>${host({ q: "gr", rows: ["Grace"] }, methods)}</main>` }] });
  await done;
  eq([live.pending, live.error, seen], [false, null, [true, false]], "settled");
  eq(it.props, { q: "gr", rows: ["Grace"] }, "the new props");
  eq(p.location.search, "?x=1&q=gr", "the address follows q");
  const failing = live.search("g");
  const ev2 = p.ws().sent.filter((m) => m.type === "event")[1];
  p.server({ type: "patch", ref: ev2.ref, updates: [{ id: "c0", html: `<main>${host({ q: "gr", rows: ["Grace"] }, methods + ` data-vk-err='{"status":422,"message":"invalid","fields":{"q":"too short"}}'`)}</main>` }] });
  await failing.then(() => { throw new Error("resolved"); }, (err) => eq(err.vuka.fields, { q: "too short" }, "rejected"));
  eq(live.error.fields.q, "too short", "live.error");
});

test("a stateless page's methods: posted with the signed props; the answer's props update the island", async () => {
  const log = [], { it, mod } = island(log), posts = [];
  const fetch = (url, init) => {
    posts.push([url, JSON.parse(init.body)]);
    const body = JSON.parse(init.body);
    const out = body.method === "fail" ? { error: { status: 422, message: "bad", fields: { q: "no" } } }
      : body.method === "away" ? { redirect: "/x" } : { props: JSON.stringify({ q: body.args[0] }), sig: "s2" };
    return Promise.resolve({ status: 200, json: () => Promise.resolve(out) });
  };
  const methods = ` data-vk-methods='["search","fail","away"]' data-vk-call="/applicants" data-vk-sig="s1" data-vk-url='{"q":"q"}' data-vk-viewer='{"user":{"id":"7"},"roles":[],"perms":["orders.*"]}'`;
  const p = page(file, `<main>${host({ q: "" }, methods)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
  await settle();
  eq([it.ctx.vuka.user.id, it.ctx.vuka.can("orders.write"), it.ctx.vuka.can("users.read")], ["7", true, false], "the viewer");
  await it.ctx.live.search("ad");
  eq(posts[0], ["/applicants", { method: "search", args: ["ad"], props: '{"q":""}', sig: "s1" }], "posted");
  eq([it.props.q, p.$("vk-island").getAttribute("data-vk-sig"), p.location.search], ["ad", "s2", "?q=ad"], "updated, re-signed, the address");
  await it.ctx.live.search("b");
  eq(posts[1][1].sig, "s2", "the next call carries the new signature");
  await it.ctx.live.fail().then(() => { throw new Error("resolved"); }, () => {});
  eq(it.ctx.live.error, { status: 422, message: "bad", fields: { q: "no" } }, "the error");
  await it.ctx.live.away();
  eq(p.loads, [["assign", "/x"]], "a redirect navigates");
});

const lfields = ` data-vk-fields='${attr({ q: { w: "Q", d: 30 }, page: { w: "page", d: 30 }, sort: { w: "sort", d: 0 }, rows: { w: "rows" }, error: { w: "Error" } })}'`;

test("a live page's fields: handles; sets debounced into one update; read-only refused; the browser's value until the answer", async () => {
  const log = [], { it, mod } = island(log);
  const attrs = ` data-vk-methods='["remove"]' data-vk-on-m-remove="c1:4" data-vk-on-m-_set="c1:5"` + lfields;
  const props = (q, rows, page = 1) => ({ Q: q, page, sort: "", rows, Error: "" });
  const p = joined(`<main>${host(props("", ["Ada"]), attrs)}</main>`, mod);
  await settle();
  const reads = [];
  const live = it.ctx.live.view(() => reads.push(1));
  eq([live.q.get(), live.q.value, live.rows.value, typeof live.rows.set, typeof live.error], ["", "", ["Ada"], "undefined", "object"], "handles");
  eq(reads.length, 4, "each read tracked");
  eq(Object.getOwnPropertyDescriptor(live.rows, "value").set, undefined, "a read-only value has no setter");
  let threw = false;
  try { (() => { "use strict"; live.rows.value = []; })(); } catch (_) { threw = true; }
  eq(threw, true, "assigning a read-only value throws");
  const events = () => p.ws().sent.filter((m) => m.type === "event");
  live.q.value = "g";
  live.q.set("gr");
  live.page.set(2);
  eq([live.q.value, live.page.value, live.pending, events().length], ["gr", 2, false, 0], "the browser's values, nothing sent yet");
  p.tick(50);
  await settle();
  eq(events().map((m) => [m.target, m.data]), [["c1:5", { Q: "gr", page: 2 }]], "one update");
  eq(live.pending, true, "pending while it runs");
  live.q.set("gra");
  const ev = events()[0];
  p.server({ type: "patch", ref: ev.ref, updates: [{ id: "c0", html: `<main>${host(props("gr", ["Grace"], 2), attrs)}</main>` }] });
  await settle();
  eq([live.rows.value, live.q.value, live.page.value], [["Grace"], "gra", 2], "server props; the newer typing kept");
  const nowDone = live.q.set("grac", { now: true });
  await settle();
  eq(events().slice(1).map((m) => m.data), [{ Q: "grac" }], "now: sent at once, with what was waiting");
  const ev2 = events()[1];
  p.server({ type: "patch", ref: ev2.ref, updates: [{ id: "c0", html: `<main>${host(props("GRAC", [], 2), attrs)}</main>` }] });
  await nowDone;
  eq(live.q.value, "GRAC", "the server's answer wins once nothing newer is waiting");
  live.sort.set("name");
  await settle();
  eq(events().slice(2).map((m) => m.data), [{ sort: "name" }], "debounce=0 sends at once");
  const ev3 = events()[2];
  p.server({ type: "patch", ref: ev3.ref, updates: [{ id: "c0", html: `<main>${host(props("GRAC", [], 2), attrs + ` data-vk-err='{"status":403,"message":"sort: refused"}'`)}</main>` }] });
  await settle();
  eq([live.error && live.error.status, live.sort.value], [403, ""], "a refused set: the error, and the server's value back");
});

test("a method sends the sets waiting first, and has its own pending and error", async () => {
  const log = [], { it, mod } = island(log);
  const attrs = ` data-vk-methods='["remove","other"]' data-vk-on-m-remove="c1:4" data-vk-on-m-other="c1:6" data-vk-on-m-_set="c1:5"` + lfields;
  const p = joined(`<main>${host({ Q: "", page: 1, rows: [] }, attrs)}</main>`, mod);
  await settle();
  const live = it.ctx.live;
  live.q.set("x");
  const done = live.remove(7);
  eq([live.remove.pending, live.other.pending, live.pending], [true, false, true], "the method's pending");
  await settle();
  const events = () => p.ws().sent.filter((m) => m.type === "event");
  eq(events().map((m) => [m.target, m.data]), [["c1:5", { Q: "x" }]], "the set first; the method waits for it");
  p.server({ type: "patch", ref: events()[0].ref, updates: [{ id: "c0", html: `<main>${host({ Q: "x", page: 1, rows: [] }, attrs)}</main>` }] });
  await settle();
  eq(events().map((m) => [m.target, m.data]), [["c1:5", { Q: "x" }], ["c1:4", [7]]], "then the method");
  p.server({ type: "patch", ref: events()[1].ref, updates: [{ id: "c0", html: `<main>${host({ Q: "x", page: 1, rows: [] }, attrs + ` data-vk-err='{"status":422,"message":"in use"}'`)}</main>` }] });
  await done.then(() => { throw new Error("resolved"); }, () => {});
  eq([live.remove.pending, live.remove.error, live.other.error, live.error], [false, { status: 422, message: "in use" }, null, { status: 422, message: "in use" }], "the error, the method's and the page's");
});

test("a stateless page's fields: the set posted with the signed props, answered with new ones", async () => {
  const log = [], { it, mod } = island(log), posts = [];
  const fetch = (url, init) => {
    const body = JSON.parse(init.body);
    posts.push(body);
    const q = body.set ? body.set.Q : "";
    const out = body.set && body.set.rows ? { error: { status: 403, message: "rows: can't be set" } } : { props: JSON.stringify({ Q: q, rows: [q.toUpperCase()] }), sig: "s" + (posts.length + 1) };
    return Promise.resolve({ status: 200, json: () => Promise.resolve(out) });
  };
  const attrs = ` data-vk-methods='[]' data-vk-call="/levels" data-vk-sig="s1" data-vk-url='{"Q":"q"}'` + lfields;
  const p = page(file, `<main>${host({ Q: "", rows: [] }, attrs)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
  await settle();
  const live = it.ctx.live;
  live.q.set("a");
  live.q.set("ab");
  p.tick(50);
  await settle();
  eq(posts, [{ method: "", set: { Q: "ab" }, props: '{"Q":"","rows":[]}', sig: "s1" }], "one post");
  eq([live.rows.value, it.props.rows, p.$("vk-island").getAttribute("data-vk-sig"), p.location.search], [["AB"], ["AB"], "s2", "?q=ab"], "re-signed props, the component's and the address");
  await live.q.set("c", { now: true });
  eq(posts[1].sig, "s2", "the next set carries the new signature");
});

test("a page with fields and no methods: live is there, reads and sets (live and stateless)", async () => {
  const fattrs = ` data-vk-fields='${attr({ title: { w: "Title" }, q: { w: "Q", d: 30 } })}'`;
  {
    const log = [], { it, mod } = island(log);
    const attrs = ` data-vk-on-m-_set="c1:5"` + fattrs;
    const p = joined(`<main>${host({ Title: "Levels", Q: "" }, attrs)}</main>`, mod);
    await settle();
    const live = it.ctx.live;
    eq([live !== null && live !== undefined, live.title.value], [true, "Levels"], "live, read");
    live.q.set("ad", { now: true });
    await settle();
    const ev = p.ws().sent.filter((m) => m.type === "event");
    eq(ev.map((m) => [m.target, m.data]), [["c1:5", { Q: "ad" }]], "the set sent");
    p.server({ type: "patch", ref: ev[0].ref, updates: [{ id: "c0", html: `<main>${host({ Title: "Levels (1)", Q: "ad" }, attrs)}</main>` }] });
    await settle();
    eq([live.title.value, live.q.value], ["Levels (1)", "ad"], "Changed's answer");
  }
  {
    const log = [], { it, mod } = island(log), posts = [];
    const fetch = (url, init) => {
      const body = JSON.parse(init.body);
      posts.push(body);
      return Promise.resolve({ status: 200, json: () => Promise.resolve({ props: JSON.stringify({ Title: "Levels (" + body.set.Q + ")", Q: body.set.Q }), sig: "s2" }) });
    };
    const attrs = ` data-vk-call="/levels" data-vk-sig="s1"` + fattrs;
    const p = page(file, `<main>${host({ Title: "Levels", Q: "" }, attrs)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
    await settle();
    const live = it.ctx.live;
    eq(live.title.value, "Levels", "stateless: read");
    await live.q.set("x", { now: true });
    eq([posts[0].set, live.title.value, p.$("vk-island").getAttribute("data-vk-sig")], [{ Q: "x" }, "Levels (x)", "s2"], "stateless: posted, answered");
  }
});

test("an invalid set: the field's error, what was typed kept; cleared by the next good set (live)", async () => {
  const log = [], { it, mod } = island(log);
  const attrs = ` data-vk-methods='["remove"]' data-vk-on-m-remove="c1:4" data-vk-on-m-_set="c1:5"` + lfields;
  const props = (q, page = 1) => ({ Q: q, page, sort: "", rows: [], Error: "" });
  const p = joined(`<main>${host(props(""), attrs)}</main>`, mod);
  await settle();
  const live = it.ctx.live, events = () => p.ws().sent.filter((m) => m.type === "event");
  eq([live.q.error, live.rows.error], [null, undefined], "no error; a read-only field has none");
  live.q.set("toolong");
  const bad = live.page.set(0, { now: true });
  await settle();
  const err = '{"status":422,"message":"invalid: Q: at most 5 characters","fields":{"Q":"at most 5 characters"}}';
  p.server({ type: "patch", ref: events()[0].ref, updates: [{ id: "c0", html: `<main>${host(props(""), attrs + ` data-vk-err='${err}'`)}</main>` }] });
  await bad.then(() => { throw new Error("resolved"); }, (e) => eq([e.status, e.fields], [422, { Q: "at most 5 characters" }], "rejected with status and fields"));
  eq([events()[0].data, live.q.value, live.q.error, live.page.value, live.page.error], [{ Q: "toolong", page: 0 }, "toolong", "at most 5 characters", 0, null], "nothing applied: both kept, the error shown");
  live.q.set("ok", { now: true });
  await settle();
  eq(events().slice(1).map((m) => m.data), [{ Q: "ok" }], "the next set");
  p.server({ type: "patch", ref: events()[1].ref, updates: [{ id: "c0", html: `<main>${host(props("ok"), attrs)}</main>` }] });
  await settle();
  eq([live.q.value, live.q.error], ["ok", null], "cleared by a good set");
  const nested = live.q.set("x", { now: true });
  await settle();
  p.server({ type: "patch", ref: events()[2].ref, updates: [{ id: "c0", html: `<main>${host(props("ok"), attrs + ` data-vk-err='{"status":422,"message":"invalid","fields":{"Q.code":"required"}}'`)}</main>` }] });
  await nested.catch(() => {});
  eq([live.q.value, live.q.error], ["x", "code: required"], "a field inside it");
  const rm = live.remove(1);
  await settle();
  p.server({ type: "patch", ref: events()[3].ref, updates: [{ id: "c0", html: `<main>${host(props("ok"), attrs)}</main>` }] });
  await rm;
  eq([live.q.value, live.q.error], ["ok", null], "a method's success drops what was kept");
});

test("an invalid set and an invalid call, stateless: kept and shown; the call rejects with fields", async () => {
  const log = [], { it, mod } = island(log), posts = [];
  const fetch = (url, init) => {
    const body = JSON.parse(init.body);
    posts.push(body);
    const out = body.set && body.set.Q.length > 5 ? { error: { status: 422, message: "invalid", fields: { Q: "at most 5 characters" } } }
      : body.method === "remove" ? { error: { status: 422, message: "invalid", fields: { "level.code": "required" } } }
      : { props: JSON.stringify({ Q: body.set ? body.set.Q : "", rows: [] }), sig: "s" + (posts.length + 1) };
    return Promise.resolve({ status: 200, json: () => Promise.resolve(out) });
  };
  const attrs = ` data-vk-methods='["remove"]' data-vk-call="/levels" data-vk-sig="s1"` + lfields;
  const p = page(file, `<main>${host({ Q: "", rows: [] }, attrs)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
  await settle();
  const live = it.ctx.live;
  await live.q.set("toolong", { now: true }).catch(() => {});
  eq([live.q.value, live.q.error, live.error.fields], ["toolong", "at most 5 characters", { Q: "at most 5 characters" }], "kept and shown");
  await live.remove({ level: {} }).then(() => { throw new Error("resolved"); }, (e) => eq(e.fields, { "level.code": "required" }, "the call's fields"));
  eq(live.remove.error.fields, { "level.code": "required" }, "live.remove.error.fields");
  await live.q.set("ab", { now: true });
  eq([live.q.value, live.q.error], ["ab", null], "a good set clears it");
});

let current = null;
process.on("beforeExit", () => { if (current) { console.log("FAIL " + current + ": never finished"); process.exit(1); } });
// unbuilt opens a page whose island Levels the build doesn't have; the
// loader's stream (development) is captured to send it a later manifest.
function unbuilt(info) {
  const streams = [];
  class EventSource { constructor(url) { this.url = url; streams.push(this); } }
  const body = `<main><vk-island data-vk-s="p#2" data-vk-island="Levels" data-vk-client="load" data-vk-props='{}'></vk-island></main>`;
  const log = [], { mod } = island(log);
  const p = page(file, body, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/levels.js": mod }, globals: { EventSource } });
  return { p, log, streams, box: () => p.$("vk-island [data-vk-missing]") };
}

const dfields = ` data-vk-fields='${attr({ title: { w: "title" }, stats: { w: "stats", p: 1 }, rows: { w: "rows", p: 1 } })}'`;
const dstate = (p, e = {}) => ` data-vk-defer='${attr({ p, e })}'`;

test("deferred fields, live: pending and undefined until a patch brings them; a loader's error is the field's", async () => {
  const log = [], { it, mod } = island(log);
  const p = joined(`<main>${host({ title: "Orders" }, dfields + dstate(["stats", "rows"]))}</main>`, mod);
  await settle();
  const live = it.ctx.live, seen = [];
  live.subscribe(() => seen.push([live.stats.pending, live.rows.pending]));
  eq([live.stats.value, live.stats.pending, live.stats.error, live.rows.pending, "pending" in live.title], [undefined, true, null, true, false], "pending");
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${host({ title: "Orders", stats: { open: 3 } }, dfields + dstate(["rows"]))}</main>` }] });
  await settle();
  eq([live.stats.value, live.stats.pending, live.rows.value, live.rows.pending], [{ open: 3 }, false, undefined, true], "stats arrived");
  eq(it.props.stats, { open: 3 }, "and reached the component's props");
  p.server({ type: "patch", ref: 0, updates: [{ id: "c0", html: `<main>${host({ title: "Orders", stats: { open: 3 } }, dfields + dstate([], { rows: { message: "rows are down" } }))}</main>` }] });
  await settle();
  eq([live.rows.pending, live.rows.error, live.rows.value, live.error], [false, { message: "rows are down" }, undefined, null], "rows failed, the page didn't");
  eq(seen, [[false, true], [false, false]], "the component told each time");
  eq(log.filter((l) => l[0] === "mount").length, 1, "no remount");
});

test("deferred fields, stateless: loaded by a signed post once mounted, first of its calls", async () => {
  const log = [], { it, mod } = island(log), posts = [];
  let release;
  const fetch = (url, init) => {
    const body = JSON.parse(init.body);
    posts.push(body);
    const out = body.load ? { props: JSON.stringify({ title: "Orders", stats: { open: 3 } }), sig: "s2", defer: { p: [], e: { rows: { status: 500, message: "error" } } } }
      : { props: JSON.stringify({ title: body.args[0], stats: { open: 3 } }), sig: "s3" };
    return new Promise((r) => { const go = () => r({ status: 200, json: () => Promise.resolve(out) }); body.load ? (release = go) : go(); });
  };
  const attrs = ` data-vk-methods='["rename"]' data-vk-call="/orders" data-vk-sig="s1"` + dfields + dstate(["stats", "rows"]);
  const p = page(file, `<main>${host({ title: "Orders" }, attrs)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
  await settle();
  const live = it.ctx.live;
  eq(posts, [{ method: "", load: true, props: '{"title":"Orders"}', sig: "s1" }], "the load, posted with the signed props");
  eq([live.stats.pending, live.stats.value, live.pending], [true, undefined, false], "pending, not the page's");
  const renamed = live.rename("Sales");
  await settle();
  eq(posts.length, 1, "a call waits for the load");
  release();
  await renamed;
  eq([live.stats.value, live.stats.pending, live.rows.pending, live.rows.error], [{ open: 3 }, false, false, { status: 500, message: "error" }], "loaded");
  eq(posts[1].sig, "s2", "the call carries the load's signature");
  eq([live.title.value, live.stats.value], ["Sales", { open: 3 }], "after the call");
});

test("deferred fields, stateless: a refused load is each pending field's error", async () => {
  const log = [], { it, mod } = island(log);
  const fetch = () => Promise.resolve({ status: 403, json: () => Promise.resolve({ error: { status: 403, message: "the page expired: reload it" } }) });
  const attrs = ` data-vk-call="/orders" data-vk-sig="s1"` + dfields + dstate(["stats", "rows"]);
  page(file, `<main>${host({ title: "Orders" }, attrs)}</main>`, { scripts: [loaderSrc.replace("/*MANIFEST*/null", JSON.stringify(info))], modules: { "/pick.js": mod }, globals: { fetch } });
  await settle();
  await settle();
  const live = it.ctx.live;
  eq([live.stats.pending, live.stats.error, live.rows.error.status], [false, { status: 403, message: "the page expired: reload it" }, 403], "errors");
});

test("vuka run: an island the build doesn't have says its build failed", async () => {
  const { p, box } = unbuilt({ m: {}, i: { Pick: ["/pick.js", ""] }, e: "/_vuka/events", d: true, x: true });
  await settle();
  eq(p.$("vk-island").getAttribute("data-vk-error"), "unknown island", "flagged");
  eq(box() && box().textContent, "Island Levels isn't built: the island build failed — see the toolbar or the terminal", "the box");
});

test("vuka run: not built yet, then a build has it: the box goes and it mounts", async () => {
  const { p, log, streams, box } = unbuilt({ m: {}, i: {}, e: "/_vuka/events", d: true });
  await settle();
  eq(box() && box().textContent, "Island Levels isn't built yet", "the box");
  streams[0].onmessage({ data: JSON.stringify({ m: {}, i: {}, e: "/_vuka/events", d: true, x: true }) });
  eq(box().textContent.includes("the island build failed"), true, "the build failed since");
  streams[0].onmessage({ data: JSON.stringify({ m: {}, i: { Levels: ["/levels.js", ""] }, e: "/_vuka/events", d: true }) });
  await settle();
  eq([box(), p.$("vk-island").getAttribute("data-vk-error")], [null, null], "the box and the flag gone");
  eq(log, [["mount", []]], "mounted");
});

test("production: an unknown island shows no box", async () => {
  const { p, box } = unbuilt({ m: {}, i: { Pick: ["/pick.js", ""] } });
  await settle();
  eq(p.$("vk-island").getAttribute("data-vk-error"), "unknown island", "flagged");
  eq(box(), null, "no box");
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    current = t.name;
    try {
      await t.fn();
    } catch (err) {
      failed++;
      console.log("FAIL " + t.name + ": " + err.message);
    }
  }
  current = null;
  console.log(`${tests.length} tests, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
