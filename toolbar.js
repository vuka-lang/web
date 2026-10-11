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
    <button type="button" class="handle" title="Vuka development toolbar" aria-expanded="false">
      <span class="mark" aria-hidden="true"></span><span class="state"></span>
    </button>
    <aside class="drawer" hidden aria-label="Development toolbar">
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

  const calls = [{ id: pageID, label: "page " + location.pathname + location.search }];
  const browserErrors = []; // failures the server never saw: a push timing out, a dropped connection
  let page = null, build = { state: "" }, selected = pageID, tab = recall("tab") || "Request";
  let buildAt = 0;

  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const get = (path) => fetch(base + path, { cache: "no-store" }).then((r) => r.json());

  // The handle: the build when it isn't ok, else the page's time and queries.
  function drawHandle() {
    handle.className = "handle";
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
    if (!page) { stateEl.textContent = "…"; return; }
    if (page.tone) handle.classList.add(page.tone);
    const n = calls.length - 1;
    stateEl.innerHTML = `<span class="num">${esc(page.ms)}</span><span class="sep">·</span><span class="num">${page.queries} q</span>` +
      (n ? `<span class="calls" title="fetch and XHR calls">+${n}</span>` : "");
  }

  function drawBuild() {
    chip.hidden = build.state !== "building" && build.state !== "failed";
    chip.className = "chip " + (build.state === "failed" ? "err" : "building");
    chip.textContent = build.state === "failed" ? "build failed" : "rebuilding";
    buildOut.hidden = build.state !== "failed" || !build.output;
    buildOut.textContent = build.output || "";
  }

  function drawSelect() {
    const html = calls.map((c) => `<option value="${esc(c.id)}"${c.id === selected ? " selected" : ""}>${esc(c.label)}</option>`).join("");
    if (select.innerHTML !== html) select.innerHTML = html;
  }

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
    if (id === pageID) { page = r; drawHandle(); }
    const scroll = (body.querySelector(".panel:not([hidden])") || {}).scrollTop || 0;
    const open = [...body.querySelectorAll("details[open]")].map((d) => d.dataset.ref);
    body.innerHTML = r.html;
    if (id === pageID) addBrowserErrors();
    for (const d of body.querySelectorAll("details")) if (open.includes(d.dataset.ref)) d.open = true;
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

  function toggle(open) {
    drawer.hidden = !open;
    handle.setAttribute("aria-expanded", String(open));
    remember("open", open ? "1" : "");
    if (open) load();
  }

  handle.addEventListener("click", () => toggle(drawer.hidden));
  $(".close").addEventListener("click", () => toggle(false));
  select.addEventListener("change", () => { selected = select.value; body.innerHTML = `<p class="loading">Loading…</p>`; load(); });
  body.addEventListener("click", (e) => {
    const t = e.target.closest(".tab");
    if (t) { show(t.dataset.tab); remember("tab", tab); }
  });
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
  window.fetch = function (input, init) {
    const p = fetch0.apply(this, arguments);
    const method = (init && init.method) || (input instanceof Request ? input.method : "GET");
    const url = input instanceof Request ? input.url : String(input);
    if (!url.includes("/_vuka/")) p.then((r) => track(method, url, r.headers.get(header), r.status), () => {});
    return p;
  };
  const open0 = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.addEventListener("loadend", () => track(method, String(url), this.getResponseHeader(header), this.status));
    return open0.apply(this, arguments);
  };

  // The live connection's messages are the page's: refresh after each.
  document.addEventListener("vk:patched", () => { if (selected === pageID) reload(); });
  document.addEventListener("vk:error", (e) => {
    const d = e.detail || {};
    browserErrors.push({ message: d.message || String(d.error || "error"), ref: d.ref, at: new Date().toLocaleTimeString() });
    if (selected === pageID) reload();
  });

  // vuka run -watch's build: the server sends each change over one stream,
  // which reconnects by itself while the app swaps builds.
  function watchBuild() {
    const es = new EventSource(base + "build");
    es.onmessage = (e) => {
      const st = JSON.parse(e.data);
      st.elapsed = st.since ? Math.max(0, (Date.now() - Date.parse(st.since)) / 1000) : 0;
      const changed = st.state !== build.state || st.output !== build.output;
      build = st;
      buildAt = Date.now();
      if (changed) { drawBuild(); drawHandle(); }
    };
    es.onerror = () => { if (build.state === "ok") { build = { state: "" }; drawHandle(); } }; // the app is swapping builds
  }
  setInterval(() => { if (build.state === "building") drawHandle(); }, 100);

  document.documentElement.appendChild(host);
  drawSelect();
  drawHandle();
  get("r/" + encodeURIComponent(pageID)).then((r) => { if (!r.error) { page = r; drawHandle(); } }, () => {});
  if (script.hasAttribute("data-vk-build")) watchBuild();
  if (recall("open")) toggle(true);
})();
