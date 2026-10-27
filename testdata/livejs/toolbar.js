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
  class EventSource { constructor(url) { this.url = url; streams.push(this); } }
  class XMLHttpRequest { open() {} addEventListener() {} }
  const p = page(file, o.body || "<main></main>", {
    build: o.build, url: o.url,
    globals: {
      EventSource, XMLHttpRequest, Request: class {},
      MutationObserver: class { observe() {} takeRecords() { return []; } },
      CSS: { escape: (s) => s },
      setInterval: () => 0,
      fetch: (url, init) => {
        gets.push(String(url));
        if (o.fetch && !String(url).includes("/_vuka/")) return o.fetch(url, init);
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
  const send = async (st) => { streams[0].onmessage({ data: JSON.stringify(st) }); await settle(); };
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
