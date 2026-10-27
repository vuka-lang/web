// The development toolbar of a Vuka web app: a handle on the right edge
// showing the page's request (time, queries), and a drawer of panels the
// app draws for it — or for a fetch or XHR call the page made, or after
// the live connection's messages. It lives in a shadow root, so the page's
// styles and its own never meet. Development only: a production app
// serves no such script.
(() => {
  "use strict";
  const script = document.currentScript;
  if (!script || window.__vukaToolbar) return;
  window.__vukaToolbar = true;
  const pageID = script.dataset.vkToolbar;
  const base = new URL(script.src, location.href).origin + "/_vuka/toolbar/";
  const header = "X-Vuka-Toolbar";
  const store = (() => { try { return window.sessionStorage; } catch { return null; } })();
  const remember = (k, v) => { try { store && store.setItem("vk-toolbar-" + k, v); } catch {} };
  const recall = (k) => { try { return store ? store.getItem("vk-toolbar-" + k) : null; } catch { return null; } };

  const host = document.createElement("vuka-toolbar");
  const root = host.attachShadow({ mode: "open" });
  root.innerHTML = `<style>${"__CSS__"}</style><div class="vk">
    <div class="dock"><button type="button" class="handle" title="Vuka development toolbar" aria-label="Open development toolbar" aria-expanded="false" aria-controls="vk-toolbar-drawer">
      <span class="mark" aria-hidden="true"></span><span class="state"></span>
    </button></div>
    <aside id="vk-toolbar-drawer" class="drawer" hidden aria-label="Development toolbar">
      <div class="bar">
        <span class="mark" aria-hidden="true"></span>
        <select aria-label="Request"></select>
        <span class="chip" hidden></span>
        <button type="button" class="close" aria-label="Close">×</button>
      </div>
      <pre class="build-out" hidden></pre>
      <div class="body"><p class="loading">Loading…</p></div>
    </aside></div>`;
  const $ = (s) => root.querySelector(s);
  const handle = $(".handle"), drawer = $(".drawer"), select = $("select"), body = $(".body");
  const chip = $(".chip"), buildOut = $(".build-out"), stateEl = $(".state");

  const calls = [{ id: pageID, label: "page " + location.pathname + location.search, page: true }];
  const browserErrors = []; // failures the server never saw: a push timing out, a dropped connection
  let page = null, build = { state: "" }, selected = pageID, tab = recall("tab") || "Request";
  let buildAt = 0;
  let current = pageID; // the record of the page on screen: a live navigation moves it
  let opened = pageID; // the record of the page the live connection opens from: a navigation over HTTP moves it
  const navRecords = new Map(); // URL → the record of a navigation over HTTP fetching it
  let going = ""; // where a live navigation in flight goes
  let wtab = recall("wtab") || "summary";
  const wires = new Map(); // "record:seq" → a socket message's frames, drawn: they don't change once sent

  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const get = (path) => fetch(base + path, { cache: "no-store" }).then((r) => r.json());

  // assetsFailed is what failed when the app's own build didn't but its
  // styles, scripts or islands did: the last good ones keep serving.
  const assetsFailed = () => build.state !== "failed" && build.assets && build.assets.failed
    ? (build.assets.islands ? "island build failed" : "asset build failed") : "";

  // The handle: the build when it isn't ok, else the page's time and queries.
  function drawHandle() {
    handle.className = "handle";
    if (going) {
      handle.classList.add("building");
      stateEl.innerHTML = `<span class="spin"></span> <span class="build-text">loading ${esc(going)}…</span>`;
      return;
    }
    if (build.state === "building") {
      const s = build.elapsed + (Date.now() - buildAt) / 1000;
      handle.classList.add("building");
      stateEl.innerHTML = `<span class="spin"></span> <span class="build-text">building… ${s.toFixed(1)}s</span>`;
      return;
    }
    if (build.state === "failed") {
      handle.classList.add("err");
      stateEl.innerHTML = `<span class="build-text">build failed</span>`;
      return;
    }
    if (assetsFailed()) {
      handle.classList.add("err");
      stateEl.innerHTML = `<span class="build-text">${assetsFailed()}</span>`;
      return;
    }
    if (!page) { stateEl.textContent = "…"; return; }
    if (page.tone) handle.classList.add(page.tone);
    const n = calls.filter((c) => !c.page && !c.cron).length;
    stateEl.innerHTML = `<span class="num">${esc(page.ms)}</span><span class="sep">·</span><span class="num">${page.queries} q</span>` +
      (n ? `<span class="calls" title="fetch and XHR calls">+${n}</span>` : "");
  }

  function drawBuild() {
    const assets = assetsFailed();
    chip.hidden = build.state !== "building" && build.state !== "failed" && !assets;
    chip.className = "chip " + (build.state === "failed" || assets && build.state !== "building" ? "err" : "building");
    chip.textContent = build.state === "failed" ? "build failed" : build.state === "building" ? "rebuilding" : assets;
    const out = build.state === "failed" ? build.output : assets ? build.assets.output : "";
    buildOut.hidden = !out;
    buildOut.textContent = out || "";
  }

  function drawSelect() {
    const option = (c) => `<option value="${esc(c.id)}"${c.id === selected ? " selected" : ""}>${esc(c.label)}</option>`;
    const crons = calls.filter((c) => c.cron);
    const html = calls.filter((c) => !c.cron).map(option).join("") +
      (crons.length ? `<optgroup label="Cron runs">${crons.map(option).join("")}</optgroup>` : "");
    if (select.innerHTML !== html) select.innerHTML = html;
  }

  // refreshCrons lists the app's latest cron runs, each a record of its own.
  async function refreshCrons() {
    const r = await get("crons").catch(() => null);
    if (!r || !r.runs) return;
    const runs = r.runs.map((x) => ({ id: x.id, label: x.label, cron: true }));
    const kept = runs.some((x) => x.id === selected) || !calls.some((c) => c.cron && c.id === selected);
    calls.splice(0, calls.length, ...calls.filter((c) => !c.cron), ...(kept ? runs : runs.concat(calls.filter((c) => c.id === selected))));
    drawSelect();
  }
  const pick = (id) => { selected = id; drawSelect(); body.innerHTML = `<p class="loading">Loading…</p>`; load(); };

  // open fetches the panels of the selected record into the drawer.
  let loading = null;
  async function load() {
    const id = selected;
    const r = await get("r/" + encodeURIComponent(id) + "?panels").catch(() => null);
    if (id !== selected) return;
    if (!r || r.error) {
      body.innerHTML = `<p class="gone">${esc((r && r.error) || "The app isn't answering.")}</p>`;
      return;
    }
    if (id === current) { page = r; drawHandle(); }
    const cron = calls.find((c) => c.id === id && c.cron);
    if (cron && !r.done) { clearTimeout(loading); loading = setTimeout(() => { refreshCrons(); load(); }, 400); } // a run in progress
    else if (cron) refreshCrons();
    const scroll = (body.querySelector(".panel:not([hidden])") || {}).scrollTop || 0;
    const open = [...body.querySelectorAll("details[open]")].map((d) => d.dataset.seq || d.dataset.ref);
    body.innerHTML = r.html;
    mountIDE();
    if (id === current) addBrowserErrors();
    for (const d of body.querySelectorAll("details")) if (open.includes(d.dataset.seq || d.dataset.ref)) { d.open = true; wire(d); }
    show(body.querySelector(`[data-tab="${CSS.escape(tab)}"]`) ? tab : "Request");
    const p = body.querySelector(".panel:not([hidden])");
    if (p) p.scrollTop = scroll;
  }
  const reload = () => { if (!drawer.hidden) { clearTimeout(loading); loading = setTimeout(load, 120); } };

  function show(name) {
    tab = name;
    for (const t of body.querySelectorAll(".tab")) t.setAttribute("aria-selected", String(t.dataset.tab === name));
    for (const p of body.querySelectorAll(".panel")) p.hidden = p.dataset.panel !== name;
  }

  // addBrowserErrors lists the live failures only the browser saw.
  function addBrowserErrors() {
    const live = body.querySelector('[data-panel="Live"] .live');
    if (!live) return;
    const seen = new Set([...live.querySelectorAll("details[data-err]")].map((d) => d.dataset.ref));
    for (const e of browserErrors) {
      if (e.ref && seen.has(String(e.ref))) continue;
      live.insertAdjacentHTML("afterbegin", `<details class="msg err"><summary><span class="type t-browser">browser</span>` +
        ` <code>${esc(e.message)}</code><span class="meta dim">${esc(e.at)}</span></summary></details>`);
    }
  }

  // wire draws a socket message's frames (Summary, Diff, Raw) when its entry
  // opens: fetched once, as they can be large.
  async function wire(d) {
    const box = d.querySelector(".wire[data-wire]");
    if (!box || box.dataset.drawn) return;
    const id = selected, key = id + ":" + box.dataset.wire;
    let html = wires.get(key);
    if (!html) {
      const r = await get("r/" + encodeURIComponent(id) + "?wire=" + encodeURIComponent(box.dataset.wire)).catch(() => null);
      if (!r || r.error) { box.innerHTML = `<p class="gone">${esc((r && r.error) || "The app isn't answering.")}</p>`; return; }
      html = r.html;
      if (wires.size > 200) wires.delete(wires.keys().next().value);
      wires.set(key, html);
    }
    if (!box.isConnected) {
      const again = body.querySelector(`details[data-seq="${CSS.escape(box.dataset.wire)}"]`);
      if (again && again.open) wire(again);
      return;
    }
    box.innerHTML = html;
    box.dataset.drawn = "1";
    showWire(box, wtab);
  }

  function showWire(box, name) {
    for (const t of box.querySelectorAll(".wtab")) t.setAttribute("aria-selected", String(t.dataset.wtab === name));
    for (const p of box.querySelectorAll(".wpanel")) p.hidden = p.dataset.wpanel !== name;
  }

  function toggle(open) {
    drawer.hidden = !open;
    $(".vk").classList.toggle("is-open", open);
    handle.setAttribute("aria-expanded", String(open));
    remember("open", open ? "1" : "");
    if (open) { load(); refreshCrons(); $(".close").focus(); }
    else handle.focus();
  }

  handle.addEventListener("click", () => toggle(drawer.hidden));
  $(".close").addEventListener("click", () => toggle(false));
  select.addEventListener("change", () => pick(select.value));
  select.addEventListener("focus", refreshCrons);
  body.addEventListener("click", (e) => {
    const t = e.target.closest(".tab");
    if (t) { show(t.dataset.tab); remember("tab", tab); }
    const w = e.target.closest(".wtab");
    if (w) { wtab = w.dataset.wtab; remember("wtab", wtab); showWire(w.closest(".wire"), wtab); }
    const run = e.target.closest(".cron-run");
    if (run) {
      run.disabled = true;
      fetch(base + "crons/run?name=" + encodeURIComponent(run.dataset.cron), { method: "POST", headers: { [header]: "1" } })
        .then((r) => r.json()).catch(() => ({ error: "The app isn't answering." }))
        .then(async (r) => {
          run.disabled = false;
          if (r.error) { run.title = r.error; run.textContent = "Busy"; setTimeout(() => { run.textContent = "Run now"; }, 1500); return; }
          await refreshCrons();
          pick(r.id);
        });
    }
    const open = e.target.closest(".cron-open");
    if (open) refreshCrons().then(() => pick(open.dataset.id));
    const c = e.target.closest(".copy");
    if (c) {
      const text = c.closest(".raw").querySelector("pre").textContent;
      const done = () => { c.textContent = "Copied"; setTimeout(() => { c.textContent = "Copy"; }, 1200); };
      if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, () => { c.textContent = "Copy failed"; });
    }
  });
  body.addEventListener("toggle", (e) => { if (e.target.matches && e.target.matches("details.msg") && e.target.open) wire(e.target); }, true);
  root.addEventListener("keydown", (e) => { if (e.key === "Escape") toggle(false); });

  // Each fetch and XHR call answered with a record joins the list.
  function track(method, url, id, status) {
    if (!id || calls.some((c) => c.id === id)) return;
    const u = new URL(url, location.href);
    calls.push({ id, label: `${method.toUpperCase()} ${u.origin === location.origin ? u.pathname + u.search : u.href} → ${status}` });
    drawSelect();
    drawHandle();
  }
  const fetch0 = window.fetch;
  const bare = (u) => new URL(u, location.href).href.replace(/#.*$/, "");
  window.fetch = function (input, init) {
    const p = fetch0.apply(this, arguments);
    const method = (init && init.method) || (input instanceof Request ? input.method : "GET");
    const url = input instanceof Request ? input.url : String(input);
    const nav = !!(init && init.headers && init.headers["Vk-Navigate"]); // the runtime's navigation over HTTP
    if (!url.includes("/_vuka/")) p.then((r) => {
      const id = r.headers.get(header);
      if (nav && id) { navRecords.set(bare(url), id); if (r.url) navRecords.set(bare(r.url), id); }
      track(method, url, id, r.status);
    }, () => {});
    return p;
  };
  const open0 = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.addEventListener("loadend", () => track(method, String(url), this.getResponseHeader(header), this.status));
    return open0.apply(this, arguments);
  };

  // The live connection's messages are the page's: refresh after each.
  document.addEventListener("vk:patched", () => { if (selected === current) reload(); });
  document.addEventListener("vk:error", (e) => {
    const d = e.detail || {};
    browserErrors.push({ message: d.message || String(d.error || "error"), ref: d.ref, at: new Date().toLocaleTimeString() });
    if (selected === current) reload();
  });

  // A live navigation moves the page over its socket: the handle says where
  // to while it is in flight, then follows the page on screen — the record
  // the server made of the navigation (the one it had, back or forward).
  const html = document.documentElement;
  let arriving = false; // the page landed: the handle says loading until its record is here
  new MutationObserver(() => {
    const to = html.getAttribute("data-vk-navigating");
    if (to != null) going = to || "…";
    else if (!arriving) going = "";
    drawHandle();
  }).observe(html, { attributes: true, attributeFilter: ["data-vk-navigating"] });
  // A navigation over HTTP is its fetch's record, which a live connection
  // the page then opens joins.
  window.addEventListener("vk:navigate", async (e) => {
    const d = e.detail || {}, u = new URL(d.url || location.href);
    arriving = true;
    const fetched = navRecords.get(bare(u.href));
    navRecords.delete(bare(u.href));
    let id = fetched;
    if (fetched) opened = fetched;
    else {
      const r = await get("r/" + encodeURIComponent(opened)).catch(() => null);
      id = (r && !r.error && r.now) || current;
    }
    const label = "page " + u.pathname + u.search;
    const follow = selected === current;
    let c = calls.find((x) => x.id === id);
    if (!c) calls.push(c = { id, label, page: true }); else Object.assign(c, { label, page: true });
    current = id;
    if (follow) selected = id;
    drawSelect();
    const p = await get("r/" + encodeURIComponent(id)).catch(() => null);
    if (p && !p.error && id === current) page = p;
    arriving = false;
    if (html.getAttribute("data-vk-navigating") == null) going = "";
    drawHandle();
    if (follow && !drawer.hidden) load();
  });

  // vuka run -watch's build: the server sends each change over one stream,
  // which reconnects by itself while the app swaps builds.
  function watchBuild() {
    const es = new EventSource(base + "build");
    es.onmessage = (e) => {
      const st = JSON.parse(e.data);
      st.elapsed = st.since ? Math.max(0, (Date.now() - Date.parse(st.since)) / 1000) : 0;
      const key = (b) => JSON.stringify([b.state, b.output, b.assets || null]);
      const changed = key(st) !== key(build);
      build = st;
      buildAt = Date.now();
      if (changed) { drawBuild(); drawHandle(); }
    };
    es.onerror = () => { if (build.state === "ok") { build = { state: "" }; drawHandle(); } }; // the app is swapping builds
  }
  setInterval(() => { if (build.state === "building") drawHandle(); }, 100);

  // The GraphiQL panel of an app serving web.GraphQL: a query editor
  // completing from the schema (read by introspection, which development
  // allows), variables, and the result; each run is a call of the list
  // above, its record's GraphQL panel the trace. One editor for the
  // drawer's life: it moves into each record's panel as records change.
  let ide = null;
  function mountIDE() {
    const slot = body.querySelector(".gql-slot");
    if (!slot) return;
    if (!ide) ide = makeIDE(slot.dataset.endpoint);
    slot.replaceWith(ide.el);
  }

  function makeIDE(endpoint) {
    const el = document.createElement("div");
    el.className = "gql";
    el.innerHTML = `<div class="gql-bar"><button type="button" class="btn gql-run" title="Run (Ctrl+Enter)">Run</button>
      <span class="gql-status dim"></span><button type="button" class="linkish gql-trace" hidden>Show its trace</button>
      <span class="gql-schema dim"></span></div>
      <div class="gql-panes"><div class="gql-edit">
        <div class="gql-area"><textarea class="gql-query mono" spellcheck="false" aria-label="Query" placeholder="{ … }  — Ctrl+Space completes"></textarea><ul class="gql-complete" role="listbox" hidden></ul></div>
        <label class="gql-label">Variables <textarea class="gql-vars mono" spellcheck="false" aria-label="Variables" placeholder="{}"></textarea></label>
      </div><pre class="gql-result mono" aria-label="Result"></pre></div>`;
    const q = el.querySelector(".gql-query"), vars = el.querySelector(".gql-vars"), out = el.querySelector(".gql-result");
    const list = el.querySelector(".gql-complete"), status = el.querySelector(".gql-status"), traceBtn = el.querySelector(".gql-trace");
    const schemaEl = el.querySelector(".gql-schema");
    q.value = recall("gql-query") || "{\n  \n}";
    vars.value = recall("gql-vars") || "";
    let schema = null, lastID = "";

    const introspection = `query VukaToolbarSchema { __schema { queryType { name } mutationType { name }
      types { name kind fields(includeDeprecated: true) { name description args { name type { ...T } } type { ...T } } } } }
      fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } }`;
    const typeText = (t) => !t ? "" : t.kind === "NON_NULL" ? typeText(t.ofType) + "!" : t.kind === "LIST" ? "[" + typeText(t.ofType) + "]" : t.name;
    const named = (t) => { while (t && t.ofType) t = t.ofType; return t && t.name; };
    async function loadSchema() {
      schemaEl.textContent = "reading the schema…";
      const r = await fetch0(endpoint, { method: "POST", headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ query: introspection }) }).then((x) => x.json()).catch(() => null);
      const s = r && r.data && r.data.__schema;
      if (!s) { schemaEl.textContent = "no schema: " + ((r && r.errors && r.errors[0].message) || "the app isn't answering"); return; }
      schema = { query: s.queryType && s.queryType.name, mutation: s.mutationType && s.mutationType.name, types: {} };
      for (const t of s.types) if (t.fields) schema.types[t.name] = t.fields;
      schemaEl.textContent = Object.keys(schema.types).filter((n) => !n.startsWith("__")).length + " types";
    }

    // where the cursor is: the type whose fields it may name, or the
    // field whose arguments, and the word typed so far.
    function context(text) {
      const stack = [];
      let last = "", prevWord = "", op = "query", args = null, i = 0, depth = 0, onType = "";
      while (i < text.length) {
        const c = text[i];
        if (c === "#") { while (i < text.length && text[i] !== "\n") i++; continue; }
        if (c === '"') {
          const block = text.startsWith('"""', i);
          i += block ? 3 : 1;
          while (i < text.length && !(block ? text.startsWith('"""', i) : text[i] === '"' || text[i] === "\n")) i += text[i] === "\\" ? 2 : 1;
          i += block ? 3 : 1;
          continue;
        }
        if (/[A-Za-z_]/.test(c)) {
          let j = i;
          while (j < text.length && /\w/.test(text[j])) j++;
          const w = text.slice(i, j);
          if (args === null) {
            if (depth === 0 && (w === "query" || w === "mutation" || w === "subscription")) op = w;
            else if (prevWord === "on") onType = w;
            else if (w !== "on" && w !== "fragment" && prevWord !== "fragment") last = w;
          }
          prevWord = w;
          i = j;
          continue;
        }
        if (c === "{") {
          let t = null;
          if (args === null) {
            if (onType) t = onType;
            else if (depth === 0) t = op === "mutation" ? schema.mutation : schema.query;
            else {
              const parent = stack[stack.length - 1];
              const f = parent && (schema.types[parent] || []).find((x) => x.name === last);
              t = f ? named(f.type) : null;
            }
          }
          stack.push(t); depth++; onType = ""; last = ""; prevWord = "";
        } else if (c === "}") { stack.pop(); depth = Math.max(0, depth - 1); last = ""; }
        else if (c === "(") args = last;
        else if (c === ")") args = null;
        else if (c === "." && text.startsWith("...", i)) { i += 3; continue; }
        i++;
      }
      const word = (text.match(/[_A-Za-z]\w*$/) || [""])[0];
      const type = stack[stack.length - 1];
      if (args !== null) {
        const f = type && (schema.types[type] || []).find((x) => x.name === args);
        return { word, items: f ? f.args.map((a) => ({ name: a.name, detail: typeText(a.type), insert: a.name + ": " })) : [] };
      }
      if (!type || depth === 0) return { word, items: [] };
      const items = (schema.types[type] || []).map((f) => ({ name: f.name, detail: (f.args.length ? "(…) " : "") + typeText(f.type), doc: f.description, insert: f.name }));
      items.push({ name: "__typename", detail: "String!", insert: "__typename" });
      return { word, items, type };
    }

    let choices = [], at = 0;
    function complete(force) {
      if (!schema) return;
      const before = q.value.slice(0, q.selectionStart);
      const cx = context(before);
      if (!force && !cx.word) { close(); return; }
      choices = cx.items.filter((x) => x.name.toLowerCase().startsWith(cx.word.toLowerCase()) && x.name !== cx.word);
      if (!choices.length) { close(); return; }
      at = 0;
      list.innerHTML = choices.map((x, i) => `<li role="option" data-i="${i}"><code>${esc(x.name)}</code> <span class="dim">${esc(x.detail)}</span></li>`).join("");
      list.hidden = false;
      mark();
      list.dataset.word = cx.word;
    }
    function mark() {
      for (const li of list.children) li.setAttribute("aria-selected", String(+li.dataset.i === at));
      const cur = list.children[at];
      if (cur) cur.scrollIntoView({ block: "nearest" });
    }
    function close() { list.hidden = true; choices = []; }
    function accept(i) {
      const c = choices[i];
      if (!c) return;
      const word = list.dataset.word || "", pos = q.selectionStart;
      q.setRangeText(c.insert, pos - word.length, pos, "end");
      close();
      q.focus();
      remember("gql-query", q.value);
    }

    async function run() {
      let variables;
      try { variables = vars.value.trim() ? JSON.parse(vars.value) : undefined; }
      catch (e) { out.textContent = "The variables aren't JSON: " + e.message; out.className = "gql-result mono err"; return; }
      status.textContent = "running…";
      traceBtn.hidden = true;
      const t0 = performance.now();
      let res;
      try {
        res = await window.fetch(endpoint, { method: "POST", headers: { "Content-Type": "application/json", Accept: "application/graphql-response+json, application/json" },
          body: JSON.stringify({ query: q.value, variables }) });
      } catch (e) { status.textContent = ""; out.textContent = "The app isn't answering."; out.className = "gql-result mono err"; return; }
      const text = await res.text();
      let shown = text;
      try { shown = JSON.stringify(JSON.parse(text), null, 2); } catch {}
      out.textContent = shown;
      out.className = "gql-result mono" + (/"errors"\s*:/.test(text) ? " has-errors" : "");
      status.textContent = res.status + " · " + Math.round(performance.now() - t0) + " ms";
      lastID = res.headers.get(header) || "";
      traceBtn.hidden = !lastID;
    }

    q.addEventListener("input", () => { remember("gql-query", q.value); complete(false); });
    vars.addEventListener("input", () => remember("gql-vars", vars.value));
    q.addEventListener("keydown", (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); run(); return; }
      if (e.ctrlKey && e.key === " ") { e.preventDefault(); complete(true); return; }
      if (list.hidden) {
        if (e.key === "Tab") { e.preventDefault(); q.setRangeText("  ", q.selectionStart, q.selectionEnd, "end"); }
        return;
      }
      if (e.key === "ArrowDown") { e.preventDefault(); at = (at + 1) % choices.length; mark(); }
      else if (e.key === "ArrowUp") { e.preventDefault(); at = (at + choices.length - 1) % choices.length; mark(); }
      else if (e.key === "Enter" || e.key === "Tab") { e.preventDefault(); accept(at); }
      else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(); }
    });
    vars.addEventListener("keydown", (e) => { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); run(); } });
    q.addEventListener("blur", () => setTimeout(close, 150));
    list.addEventListener("mousedown", (e) => { const li = e.target.closest("li"); if (li) { e.preventDefault(); accept(+li.dataset.i); } });
    el.querySelector(".gql-run").addEventListener("click", run);
    traceBtn.addEventListener("click", () => { if (lastID) { tab = "GraphQL"; remember("tab", tab); pick(lastID); } });
    loadSchema();
    return { el };
  }

  document.documentElement.appendChild(host);
  drawSelect();
  drawHandle();
  get("r/" + encodeURIComponent(pageID)).then((r) => { if (!r.error) { page = r; drawHandle(); } }, () => {});
  if (script.hasAttribute("data-vk-build")) watchBuild();
  // The page a browser opening the GraphQL endpoint gets: the GraphiQL panel.
  if (script.hasAttribute("data-vk-graphiql")) { tab = "GraphiQL"; toggle(true); }
  // Each page starts collapsed; opening is an explicit click or key press.
})();
