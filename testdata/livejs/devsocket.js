// Tests of the development socket's page side (devsocket.js) in node, over
// dom.js: node devsocket.js live.js ../../devsocket.js
"use strict";
const fs = require("fs"), vm = require("vm");
const { page } = require("./dom.js");
const [file, socketFile] = process.argv.slice(2);
const socketSrc = fs.readFileSync(socketFile, "utf8");
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const settle = () => new Promise((r) => setImmediate(r));

function eq(got, want, what) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${what || "value"}\n   got  ${g}\n   want ${w}`);
}

function open() {
  const socks = [];
  class WebSocket {
    constructor(url) { this.url = url; socks.push(this); }
    msg(c, v) { this.onmessage({ data: JSON.stringify({ c, v }) }); }
  }
  const p = page(file, "<main></main>", { globals: { WebSocket, Math: Object.assign(Object.create(Math), { random: () => 1 }) } });
  vm.runInContext(socketSrc, p.window);
  return { p, socks, dev: p.window.__vukaDev, last: () => socks[socks.length - 1] };
}

test("no socket until a listener; one socket for every listener, opened on the page's host", () => {
  const { dev, socks, last } = open();
  eq(socks.length, 0, "before a listener");
  dev.on("css", () => {});
  dev.on("build", () => {});
  eq(socks.length, 1, "after two");
  eq(last().url.endsWith("/_vuka/dev") && last().url.startsWith("ws://"), true, "the URL");
});

test("each channel's values reach its listeners; a listener failing stops no other", () => {
  const { dev, last, p } = open();
  const css = [], build = [];
  dev.on("css", () => { throw new Error("boom"); });
  dev.on("css", (v) => css.push(v));
  dev.on("build", (v) => build.push(v));
  last().msg("css", "a");
  last().msg("build", "b");
  eq([css, build], [["a"], ["b"]], "the values");
  eq(p.logs.filter((l) => l.includes("boom")).length, 1, "the failure logged");
});

test("a listener arriving late has the channel's value, after the script calling it", async () => {
  const { dev, last } = open();
  dev.on("css", () => {});
  last().msg("css", "a");
  const got = [];
  dev.on("css", (v) => got.push(v));
  eq(got, [], "not during the call");
  await settle();
  eq(got, ["a"], "then");
});

test("a second copy of the script changes nothing", () => {
  const { dev, p } = open();
  vm.runInContext(socketSrc, p.window);
  eq(p.window.__vukaDev === dev, true, "the same socket");
});

test("a lost connection tells the down listeners and comes back, waiting longer each time, up to a limit", () => {
  const { dev, socks, last, p } = open();
  let downs = 0;
  const got = [];
  dev.on("build", (v) => got.push(v), () => downs++);
  last().onclose();
  eq([downs, socks.length], [1, 1], "down, waiting");
  p.tick(99);
  eq(socks.length, 1, "not yet");
  p.tick(1);
  eq(socks.length, 2, "again at 100ms");
  last().onclose(); // refused: the server isn't back
  p.tick(199);
  eq(socks.length, 2, "not yet");
  p.tick(1);
  eq(socks.length, 3, "again at 200ms");
  for (let i = 0; i < 12; i++) { last().onclose(); p.tick(5000); }
  const n = socks.length;
  eq(n, 15, "every attempt within the limit");
  last().onopen();
  last().msg("build", "new");
  eq(got, ["new"], "the new server's value");
  last().onclose();
  p.tick(100);
  eq(socks.length, n + 1, "connected again, the wait starts over");
});

test("back in view, a tab waiting to reconnect doesn't wait", () => {
  const { dev, socks, last, p } = open();
  dev.on("css", () => {});
  last().onclose();
  Object.defineProperty(p.doc, "visibilityState", { value: "visible", configurable: true });
  p.doc.dispatchEvent(new p.window.Event("visibilitychange"));
  eq(socks.length, 2, "connected at once");
  p.tick(10000);
  eq(socks.length, 2, "and no timer left");
  p.doc.dispatchEvent(new p.window.Event("visibilitychange"));
  eq(socks.length, 2, "connected: nothing to do");
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
