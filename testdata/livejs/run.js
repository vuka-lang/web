// Runs live.js's protocol v2 tree code over fixtures, in node, with no
// browser: node run.js live.js fixtures.json… Each fixture is a sequence of
// steps, a server message and what the client must make of it: the page's
// HTML (c0's, every instance in place), the HTML of named instances, or an
// error.
"use strict";
const vm = require("vm"), fs = require("fs");

function load(file) {
  const noop = () => {};
  const window = { addEventListener: noop };
  const ctx = {
    window, URL, console, setTimeout, clearTimeout,
    document: {
      currentScript: { src: "http://test/_vuka/live.js", dataset: {} },
      documentElement: { setAttribute: noop, getAttribute: () => null },
      addEventListener: noop, dispatchEvent: noop,
    },
    location: { href: "http://test/" },
    WebSocket: class { send() {} },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(file, "utf8"), ctx, { filename: file });
  return window.vuka.tree;
}

const tree = load(process.argv[2]);
let failed = 0, steps = 0;
for (const file of process.argv.slice(3)) {
  for (const fx of JSON.parse(fs.readFileSync(file, "utf8"))) {
    let t = tree(), broken = false;
    fx.steps.forEach((st, i) => {
      if (broken) return;
      steps++;
      const where = `${file}: ${fx.name}: step ${i}${st.name ? " (" + st.name + ")" : ""}`;
      const bad = (msg) => { failed++; broken = true; console.log("FAIL " + where + ": " + msg); };
      let out;
      try {
        out = t.apply(st.msg.trees || [], st.msg.type === "render");
      } catch (err) {
        if (!st.error) bad("threw " + err.message);
        t = tree(); // a client starts over after an error: the next step is a render
        return;
      }
      if (st.error) return bad("no error");
      let page;
      try { page = t.page(); } catch (err) { return bad("page: " + err.message); }
      if (st.page !== undefined && page !== st.page) bad(`page\n got  ${page}\n want ${st.page}`);
      for (const id in st.html || {}) if (out[id] !== st.html[id]) bad(`${id}\n got  ${out[id]}\n want ${st.html[id]}`);
    });
  }
}
console.log(`${steps} steps, ${failed} failed`);
process.exit(failed ? 1 : 0);
