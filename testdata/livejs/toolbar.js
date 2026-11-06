// Tests of the development toolbar's build state (toolbar.js) in node, over
// dom.js: node toolbar.js live.js ../../toolbar.js
"use strict";
const fs = require("fs"), vm = require("vm");
const { page } = require("./dom.js");
const [file, toolbarFile] = process.argv.slice(2);
const toolbarSrc = fs.readFileSync(toolbarFile, "utf8");
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}

// open runs the toolbar of a page served under vuka run; send hands it a
// build state as the server's stream would.
function open(o = {}) {
  const streams = [], gets = [];
  const __vukaDev = { on(channel, f, down) { streams.push({ channel, onmessage: f, down }); } };
  class XMLHttpRequest { open() {} addEventListener() {} }
  const p = page(file, o.body || "<main></main>", {
    build: o.build, url: o.url,
    globals: {
      __vukaDev, XMLHttpRequest, Request: class {},
      MutationObserver: class { observe() {} takeRecords() { return []; } },
      CSS: { escape: (s) => s },
      setInterval: () => 0,
      fetch: (url, init) => {
        gets.push(String(url));
        if (o.fetch && !String(url).includes("/_vuka/")) return o.fetch(url, init);
        if (o.panels && String(url).includes("/_vuka/toolbar/r/")) return Promise.resolve({ headers: { get: () => null }, json: () => Promise.resolve({ html: o.panels }) });
        return Promise.resolve({ headers: { get: () => null }, json: () => Promise.resolve({ ms: "3ms", queries: 0 }) });
      },
    },
  });
  const w = p.window;
  w.Element.prototype.attachShadow = function () { const r = this.ownerDocument.createElement("vk-shadow"); this.appendChild(r); return r; };
  Object.defineProperty(p.doc, "currentScript", {
    value: { src: "http://test/_vuka/toolbar.js", dataset: { vkToolbar: "r1" }, hasAttribute: (n) => n === "data-vk-build" },
    configurable: true,
  });
  vm.runInContext(toolbarSrc, w);
  const $ = (s) => p.doc.querySelector("vuka-toolbar vk-shadow " + s);
  const send = async (st) => { streams[0].onmessage(JSON.stringify(st)); await settle(); };
  return { $, send, streams, gets, p };
}

const viteFailed = { failed: true, islands: true, output: "vite islands (web/islands/Levels.vue): vite build: exit status 1\n[vite:vue] Unexpected token\n" };

test("an island build failing: the handle, the chip and Vite's output", async () => {
  const t = open();
  await settle();
  eq(t.streams.length, 1, "the build stream");
  await t.send({ state: "ok", assets: viteFailed });
  eq(t.$(".state").textContent, "island build failed", "the handle");
  eq(t.$(".handle").classList.contains("err"), true, "in red");
  eq([t.$(".chip").hidden, t.$(".chip").textContent], [false, "island build failed"], "the chip");
  eq([t.$(".build-out").hidden, t.$(".build-out").textContent], [false, viteFailed.output], "the output");
});

test("a later build that succeeds clears it", async () => {
  const t = open();
  await settle();
  await t.send({ state: "ok", assets: viteFailed });
  await t.send({ state: "ok" });
  eq(t.$(".state").textContent.includes("failed"), false, "the handle");
  eq([t.$(".chip").hidden, t.$(".build-out").hidden], [true, true], "the chip and the output");
});

test("the app's own build failing comes first; a rebuild shows over it", async () => {
  const t = open();
  await settle();
  await t.send({ state: "failed", output: "main.go:3: undefined: x\n", assets: viteFailed });
  eq(t.$(".state").textContent, "build failed", "the handle");
  eq(t.$(".build-out").textContent, "main.go:3: undefined: x\n", "Go's output");
  await t.send({ state: "building", since: new Date().toISOString(), assets: viteFailed });
  eq(t.$(".chip").textContent, "rebuilding", "the chip");
  eq(t.$(".build-out").textContent, viteFailed.output, "the island output stays");
});

test("styles or scripts failing say asset build failed", async () => {
  const t = open();
  await settle();
  await t.send({ state: "ok", assets: { failed: true, output: "app.css: bad\n" } });
  eq(t.$(".state").textContent, "asset build failed", "the handle");
});

test("a navigation over HTTP shows as a page: its fetch's record", async () => {
  const doc = `<!doctype html><html><head><title>b</title></head><body><main><h1>book</h1></main></body></html>`;
  const t = open({
    body: `<main><a id="go" href="/books/7" data-vk-link>go</a></main>`, build: "b1", url: "http://test/shelf",
    fetch: (url) => Promise.resolve({ url: "http://test/books/7", redirected: false, status: 200,
      headers: { get: (k) => ({ "x-vuka-toolbar": "r2", "vk-page": "-", "content-type": "text/html" })[k.toLowerCase()] || null },
      text: () => Promise.resolve(doc) }),
  });
  await settle();
  t.p.fire("#go", "click");
  await settle();
  const opts = t.$("select").querySelectorAll("option").map((o) => [o.getAttribute("value"), o.textContent]);
  eq([t.p.$("h1") && t.p.$("h1").textContent, opts.some(([v, l]) => v === "r2" && l.includes("page /books/7")), t.gets.some((u) => u.endsWith("/_vuka/toolbar/r/r2"))],
    ["book", true, true], "the page's record follows the navigation");
});

test("the States panel: each instance's states, live, editable; a Derived read only", async () => {
  const j = (v) => JSON.stringify(v).replace(/'/g, "&#39;");
  const tab = { s: "c1:ft:1", i: "a", n: "Tabs.tab" }, opened = { s: "c1:fo:1", i: false, n: "Tabs.open" };
  const wide = { s: "c1:sw:1", i: false, n: "Tabs.wide", x: ["==", tab, "b"] };
  const t = open({
    body: `<main data-vk-id="c1"><button id="b" data-vk-js-click='${j([["state_set", { ...tab, v: "b" }]])}'>b</button>
      <p id="p" data-vk-b-title='${j(tab)}' data-vk-b-hidden='${j(["!", opened])}' data-vk-b-data-wide='${j(wide)}' title="a" hidden></p></main>`,
    panels: `<nav class="tabs" role="tablist"><button type="button" role="tab" class="tab" data-tab="Request">Request</button></nav><section class="panel" data-panel="Request" hidden></section>`,
  });
  await settle();
  t.p.fire(t.$(".handle"), "click");
  await settle();
  await settle();
  const rows = () => t.$('section[data-panel="States"]').querySelectorAll("tr").map((r) => [r.querySelector("code").textContent, (r.querySelector(".sv").value ?? r.querySelector(".sv").textContent), r.querySelector(".by").textContent]);
  eq(t.$('[data-tab="States"]').textContent, "States 3", "the tab");
  eq(rows(), [["open", "false", ""], ["tab", '"a"', ""], ["wide", "false", ""]], "the states, by name");
  t.p.fire("#b", "click");
  eq(rows()[1], ["tab", '"b"', "click #b"], "an event's write, flashed with the event");
  eq(rows()[2][1], "true", "and the Derived following it");
  const input = t.$('input.sv[data-key="c1:fo:1"]');
  input.value = "true";
  t.p.fire(input, "change");
  eq([t.p.$("#p").hasAttribute("hidden"), rows()[0][2]], [false, "the toolbar"], "edited in place: the page follows");
  eq(t.$('section[data-panel="States"]').querySelectorAll("input").length, 2, "a Derived isn't editable");
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
