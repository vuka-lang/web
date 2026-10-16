// The component loader: a page of an app whose components have scripts
// (web.Script) loads it. It finds each instance of a scripted component on
// the page — the elements its markup renders carry data-vk-s="<scope>"; an
// instance's root is one with no ancestor of the same scope, or a live
// component's root (data-vk-id) — imports the component's module the first
// time one renders, and runs its hooks: mounted on page load, on a live
// join and when a patch or navigation brings it; updated after a patch
// that keeps it; destroyed when it leaves the page. ui.JS.Call reaches a
// component's own methods through it before vuka.register's and window's.
(() => {
  "use strict";
  const info = /*MANIFEST*/null;
  if (window.vukaComponents) return window.vukaComponents.update(info);
  let m = info;
  const mods = new Map(); // module URL → Promise of its hook objects
  const insts = new Map(); // root element → instance
  const islandInsts = new Map(); // island host element → island instance
  let islandIO = null; // shared IntersectionObserver for client:visible
  const islandCSS = new Set(); // island stylesheet URLs already linked
  const missing = new Set(); // hosts of islands the build doesn't have
  let global = null;

  const load = (url) => {
    if (!mods.has(url)) mods.set(url, import(url).then((x) => [].concat(x.default || [])));
    return mods.get(url);
  };
  const scopeOf = (el) => el.getAttribute("data-vk-s");
  const sel = (id) => '[data-vk-s="' + id + '"]';

  // roots are the instances' roots on the page, in document order.
  function roots() {
    const out = [];
    for (const el of document.querySelectorAll("[data-vk-s]")) {
      const id = scopeOf(el);
      if (!m.m[id]) continue;
      const up = el.parentElement && el.parentElement.closest(sel(id));
      if (!up || el.hasAttribute("data-vk-id")) out.push(el);
    }
    return out;
  }

  function each(inst, hook) {
    for (const h of inst.hooks || []) {
      if (h && typeof h[hook] === "function") {
        try { h[hook].call(h, inst.el, inst.ctx); } catch (err) { console.error("vuka: " + inst.name + " " + hook + ":", err); }
      }
    }
  }

  // ctx is an instance's own object, its hooks' second argument.
  function context(inst) {
    const ons = new Map();
    inst.ons = ons;
    return {
      el: inst.el,
      push: (name, payload) => window.vuka && window.vuka.push
        ? window.vuka.push(inst.el, name, payload)
        : Promise.reject(new Error("vuka: ctx.push needs a live page")),
      // emit forwards an island component's event to the handler bound on the
      // tag (on<Emit> → a data-vk-emit-<name> handler id); payload is the
      // emit's argument, delivered as JSON to the server handler.
      emit: (name, payload) => {
        const norm = String(name).toLowerCase().replace(/[^a-z0-9]/g, "");
        const id = inst.el && inst.el.getAttribute("data-vk-on-" + norm);
        if (id && window.vuka && window.vuka.emit) return window.vuka.emit(id, payload);
        return Promise.reject(new Error("vuka: no island handler for emit " + name));
      },
      on: (name, fn) => {
        if (!ons.has(name)) ons.set(name, []);
        ons.get(name).push(fn);
      },
      ref: (name) => {
        const all = [inst.el, ...inst.el.querySelectorAll('[data-vk-ref="' + name + '"]')];
        return all.find((x) => x.getAttribute("data-vk-ref") === name && scopeOf(x) === inst.id) || null;
      },
    };
  }

  function mount(el, id, url) {
    const inst = { id, el, live: el && el.getAttribute("data-vk-id"), name: id, hooks: null, ctx: null, ons: null, queue: [] };
    if (el) insts.set(el, inst);
    inst.ready = load(url).then((hooks) => {
      if (el && insts.get(el) !== inst) return;
      inst.hooks = hooks;
      inst.ctx = context(inst);
      each(inst, "mounted");
      inst.queue.splice(0).forEach((d) => deliver(inst, d));
    }, (err) => console.error("vuka: loading " + url + ":", err));
    return inst;
  }

  function destroy(inst) {
    insts.delete(inst.el);
    if (inst.hooks) each(inst, "destroyed");
  }

  // sync follows the page: instances gone are destroyed (an element the
  // morph kept for another component's instance too), new ones mounted,
  // and after a patch (not a join) those it touched are updated.
  function sync(d) {
    for (const [el, inst] of insts) {
      if (!el.isConnected || scopeOf(el) !== inst.id || el.getAttribute("data-vk-id") !== inst.live) destroy(inst);
    }
    const kept = new Set(insts.keys());
    for (const el of roots()) if (!insts.has(el)) mount(el, scopeOf(el), m.m[scopeOf(el)]);
    syncIslands(d); // arm new island hosts, unmount gone ones, re-push live props
    if (!d || d.join) return;
    const patched = (d.ids || []).map((id) => id === "c0" ? document.body : document.querySelector('[data-vk-id="' + id + '"]')).filter(Boolean);
    for (const el of kept) {
      const inst = insts.get(el);
      if (inst && inst.hooks && patched.some((p) => p.contains(el) || el.contains(p))) each(inst, "updated");
    }
  }

  function deliver(inst, d) {
    for (const fn of (inst.ons && inst.ons.get(d.name)) || []) {
      try { fn(d.payload); } catch (err) { console.error("vuka: " + inst.name + " on " + d.name + ":", err); }
    }
  }

  // instanceOf is the instance of the component whose markup el is: the
  // one of el's scope that holds it.
  function instanceOf(el) {
    const s = el && el.closest && el.closest("[data-vk-s]");
    if (!s) return null;
    const id = scopeOf(s);
    for (let x = s; x; x = x.parentElement) {
      const inst = insts.get(x);
      if (inst && inst.id === id) return inst;
    }
    return null;
  }

  // method is the function ui.JS.Call(name) runs from el, when el's
  // component (else the app-wide script) exports one: called as
  // method(root, ctx, ...args), this its hook object, ctx carrying the
  // command's event and element.
  function method(el, name) {
    for (const inst of [instanceOf(el), global]) {
      if (!inst || !(m.f[inst.id] || []).includes(name)) continue;
      return (c, ...args) => inst.ready.then(() => {
        const h = (inst.hooks || []).find((x) => x && typeof x[name] === "function");
        if (!h) throw new Error("no method " + name);
        return h[name].call(h, inst.el || document.body, Object.assign(Object.create(inst.ctx), { event: c.event, target: c.el }), ...args);
      });
    }
    return null;
  }

  // --- islands ---
  // An island host carries data-vk-island (its name), data-vk-client (its
  // hydration strategy) and data-vk-props (JSON). roots() ignores them (their
  // data-vk-s scope "iN" is not in m.m), so the island branch owns them.
  function islandRoots() {
    return m.i ? [...document.querySelectorAll("[data-vk-island]")] : [];
  }

  // An island whose host left, or now hosts another island (a navigation
  // morphed it into the new page's: another name, or another live
  // instance's id), unmounts; the new one mounts.
  function syncIslands(d) {
    for (const [el, inst] of islandInsts) {
      const vid = el.getAttribute("data-vk-id") || "";
      if (!el.isConnected || el.getAttribute("data-vk-island") !== inst.name || vid && inst.vid && vid !== inst.vid) unmountIsland(inst);
      else if (vid) inst.vid = vid;
    }
    for (const el of islandRoots()) if (!islandInsts.has(el)) armIsland(el);
    if (d) updateIslands();
  }

  // ensureIslandCSS links a framework island build's stylesheets (Tailwind +
  // a UI library's CSS) once, as soon as a page carries an island, so its
  // components are styled before they hydrate.
  function ensureIslandCSS() {
    for (const href of m.ic || []) {
      if (islandCSS.has(href)) continue;
      islandCSS.add(href);
      const l = document.createElement("link");
      l.rel = "stylesheet";
      l.href = href;
      l.setAttribute("data-vk-island-css", "");
      document.head.appendChild(l);
    }
  }

  function armIsland(el) {
    ensureIslandCSS();
    const name = el.getAttribute("data-vk-island");
    const spec = m.i && m.i[name];
    if (!spec) { missingIsland(el, name); return; }
    const inst = { el, id: scopeOf(el) || name, name, vid: el.getAttribute("data-vk-id") || "", spec, ons: null, state: "armed", handle: null, props: {}, refs: {}, fns: new Map(), key: ["", ""] };
    islandInsts.set(el, inst);
    const go = () => hydrateIsland(inst);
    switch (el.getAttribute("data-vk-client") || "visible") {
      case "load": case "only": go(); break;
      case "idle": (window.requestIdleCallback || ((f) => setTimeout(f, 1)))(go); break;
      case "media": {
        const mq = window.matchMedia(el.getAttribute("data-vk-media") || "all");
        if (mq.matches) { go(); break; }
        const on = () => { if (mq.matches) { mq.removeEventListener("change", on); go(); } };
        mq.addEventListener("change", on); inst.mq = [mq, on]; break;
      }
      default: {
        const rm = el.getAttribute("data-vk-rootmargin");
        if (rm) { const obs = new IntersectionObserver((es) => { for (const e of es) if (e.isIntersecting) { obs.disconnect(); go(); } }, { rootMargin: rm }); obs.observe(el); inst.obs = obs; break; }
        if (!islandIO) islandIO = new IntersectionObserver((es) => { for (const e of es) if (e.isIntersecting) { const i = islandInsts.get(e.target); islandIO.unobserve(e.target); if (i) hydrateIsland(i); } });
        islandIO.observe(el);
      }
    }
  }

  // Under vuka run an island the build doesn't have says so on the page —
  // its build failed, or it isn't built yet — and mounts once a build has it.
  const boxOf = (el) => [...el.children].find((c) => c.hasAttribute("data-vk-missing"));
  function missingIsland(el, name) {
    el.setAttribute("data-vk-error", "unknown island");
    if (!m.d) return;
    missing.add(el);
    let box = boxOf(el);
    if (!box) {
      box = document.createElement("div");
      box.setAttribute("data-vk-missing", "");
      box.setAttribute("style", "display:block;margin:4px 0;padding:8px 12px;border:1px dashed #dc2626;border-radius:6px;" +
        "background:#fef2f2;color:#991b1b;font:13px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace");
      el.insertBefore(box, el.firstChild);
    }
    const text = "Island " + name + " isn't built" + (m.x ? ": the island build failed — see the toolbar or the terminal" : " yet");
    if (box.textContent !== text) box.textContent = text;
  }
  function recheckMissing() {
    for (const el of missing) {
      const name = el.getAttribute("data-vk-island");
      if (!el.isConnected) missing.delete(el);
      else if (!m.i[name]) missingIsland(el, name);
      else {
        missing.delete(el);
        const box = boxOf(el);
        if (box) box.remove();
        el.removeAttribute("data-vk-error");
        armIsland(el);
      }
    }
  }

  // An island's props are JSON (data-vk-props); tagged values are turned
  // back into what the component gets: {"$vk":"date"} a Date, {"$vk":"fn"} a
  // function calling the host's live handler or running its commands, and
  // {"$vk":"state"} a client state's value, kept current, which ctx.set
  // writes back (a derived state, "x", is read only).
  const tag = (v) => v && typeof v === "object" && !Array.isArray(v) && typeof v.$vk === "string" ? v.$vk : "";
  const clone = (v) => v === undefined ? v : typeof structuredClone === "function" ? structuredClone(v) : JSON.parse(JSON.stringify(v));
  function revive(v) {
    if (Array.isArray(v)) return v.map(revive);
    if (!v || typeof v !== "object") return v;
    if (tag(v) === "date") return new Date(v.v);
    const out = {};
    for (const k in v) out[k] = revive(v[k]);
    return out;
  }
  const states = () => window.vuka && window.vuka.states;
  function stateRef(v) { return v.x !== undefined ? v.x : v; }

  // islandCall is a function prop: it pushes the host's handler for it
  // (data-vk-on-<e>) with its first argument, or runs its commands
  // (data-vk-js-<e>).
  function islandCall(inst, e) {
    if (inst.fns.has(e)) return inst.fns.get(e);
    const fn = (...args) => {
      const el = inst.el, id = el.getAttribute("data-vk-on-" + e), js = el.getAttribute("data-vk-js-" + e);
      const payload = args[0] instanceof Event ? undefined : args[0];
      if (id && window.vuka && window.vuka.emit) return window.vuka.emit(id, payload === undefined ? undefined : clone(payload));
      if (js && window.vuka && window.vuka.run) {
        try { return Promise.resolve(window.vuka.run(el, JSON.parse(js), null)); } catch (err) { return Promise.reject(err); }
      }
      return Promise.reject(new Error("vuka: island " + inst.name + " has no handler for " + e));
    };
    inst.fns.set(e, fn);
    return fn;
  }

  // islandProps reads the host's props: what the component gets, and the
  // state props, each following its state.
  function islandProps(inst, raw) {
    let json = {};
    try { json = JSON.parse(raw || "{}"); } catch (_) {}
    const props = {}, refs = {};
    for (const k in json) {
      const v = json[k];
      switch (tag(v)) {
        case "fn": props[k] = islandCall(inst, v.e); break;
        case "state": refs[k] = v; props[k] = states() ? clone(states().read(stateRef(v))) : v.i; break;
        default: props[k] = revive(v);
      }
    }
    return { props, refs };
  }

  // plainKey is the props' JSON without its state and function props: what
  // a live re-render changes when it changes the server's props.
  function plainKey(raw) {
    let json = {};
    try { json = JSON.parse(raw || "{}"); } catch (_) { return raw; }
    const out = {}, refs = {};
    for (const k of Object.keys(json).sort()) {
      const t = tag(json[k]);
      if (t === "state") refs[k] = json[k];
      else if (t !== "fn") out[k] = json[k];
    }
    return [JSON.stringify(out), JSON.stringify(refs)];
  }

  // watchStates follows an island's state props: a change updates the
  // component (once per batch of writes).
  function watchStates(inst) {
    (inst.unwatch || []).forEach((u) => u());
    inst.unwatch = [];
    const st = states();
    if (!st) return;
    for (const k in inst.refs) {
      inst.unwatch.push(st.watch(stateRef(inst.refs[k]), (v) => {
        inst.props[k] = clone(v);
        if (inst.pending) return;
        inst.pending = true;
        Promise.resolve().then(() => { inst.pending = false; pushProps(inst); });
      }, inst.el));
    }
  }

  function pushProps(inst) {
    if (inst.state !== "mounted" || !inst.handle || typeof inst.handle.update !== "function") return;
    try { inst.handle.update({ ...inst.props }); } catch (err) { console.error("vuka: island " + inst.name + " update:", err); }
  }

  // A page backed by its island: ctx.live, what the component's useLive()
  // reads. Each method (data-vk-methods) is a function returning a promise,
  // with its own pending and error — on a live page a live event
  // (data-vk-on-m-<name>), on a stateless one a call posted to the page
  // (data-vk-call) with its signed props (data-vk-sig). Each field
  // (data-vk-fields: handle → {w: wire name, d: debounce ms when bound}) is
  // a handle: get(), value and, when bound, set(v[, {now}]) — debounced, the
  // sets of one wait sent as one update (data-vk-on-m-_set, or the call's
  // set), the value the browser's until the server's answer to it, and its
  // error the message the answer has for it (fields[w], or a field inside
  // it: "city: required"). A set answered 422 with fields (its validate:
  // tags, or an Invalid) keeps what was typed, shown with its error, until
  // the field is set again or a method succeeds; any other refusal puts
  // the server's value back. A failed call rejects with an Error carrying
  // status and fields. Calls go one at a time, in order; a method sends the
  // sets waiting first.
  // A deferred field (data-vk-fields' p: defer: on the server) isn't in the
  // first render: its value is undefined, its pending true, until the server
  // sends it — after a live page joins, or the answer to the load a
  // stateless page posts once it mounts — and its error is its loader's
  // (data-vk-defer: {p: [wire names pending], e: {wire name: error}}).
  // pending and error are the page's; subscribe tells the framework of any
  // change; view(read) is the object for its hook, read called on each read.
  function liveHandle(inst) {
    const el = inst.el;
    const attr = (a, d) => { try { return JSON.parse(el.getAttribute(a) || d); } catch (_) { return JSON.parse(d); } };
    const names = attr("data-vk-methods", "[]"), fields = attr("data-vk-fields", "{}");
    const own = new Set(["pending", "error", "subscribe", "methods", "snapshot", "version", "view", "update", "navigate", "patch"]);
    let snap = { pending: false, error: null }, ver = 0, busy = 0, lastErr = null, chain = null;
    let queued = null, batch = null, timer = null;
    const subs = new Set(), per = {}, local = {}, methods = {}, ferr = {}, held = {};
    let dpend = new Set(), derr = {};
    const readDefer = () => {
      const d = attr("data-vk-defer", "{}");
      dpend = new Set(d.p || []);
      derr = d.e || {};
    };
    readDefer();
    const notify = () => {
      ver++;
      snap = { pending: busy > 0, error: lastErr };
      subs.forEach((f) => { try { f(snap); } catch (err) { console.error(err); } });
    };
    const fail = (e) => {
      const err = new Error(e.message || "error");
      err.vuka = e; err.status = e.status; err.fields = e.fields || null;
      throw err;
    };
    const msgFor = (fs, w) => {
      if (!fs) return null;
      if (w in fs) return fs[w];
      for (const k in fs) if (k.startsWith(w + ".")) return k.slice(w.length + 1) + ": " + fs[k];
      return null;
    };
    const post = (body) => fetch(el.getAttribute("data-vk-call"), {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ ...body, props: el.getAttribute("data-vk-props") || "{}", sig: el.getAttribute("data-vk-sig") || "" }),
    }).then((r) => r.json().catch(() => ({ error: { status: r.status, message: r.statusText || "error" } }))).then((j) => {
      if (j.redirect) { location.assign(j.redirect); return; }
      if (j.error) fail(j.error);
      el.setAttribute("data-vk-props", j.props);
      el.setAttribute("data-vk-sig", j.sig);
      if (j.defer) el.setAttribute("data-vk-defer", JSON.stringify(j.defer));
      updateIslands();
    });
    // load asks a stateless page's server for its deferred fields, first
    // of its calls; a refused load is each pending field's error.
    const load = () => run(() => post({ method: "", load: true })).catch((err) => {
      const e = err && err.vuka || { message: err && err.message || "error" }, d = { p: [], e: { ...derr } };
      dpend.forEach((w) => { d.e[w] = e; });
      el.setAttribute("data-vk-defer", JSON.stringify(d));
      readDefer();
      notify();
    });
    const emit = (key, data) => {
      const id = el.getAttribute("data-vk-on-m-" + key);
      if (!id || !window.vuka || !window.vuka.emit) return Promise.reject(new Error("vuka: " + inst.name + " has no method " + key));
      return window.vuka.emit(id, data).then(() => {
        const e = el.getAttribute("data-vk-err");
        if (e) fail(JSON.parse(e));
      }, (err) => fail({ message: err && err.message || "error" }));
    };
    const send = (name, args, set) => el.getAttribute("data-vk-call")
      ? post(set ? { method: "", set } : { method: name, args: clone(args) })
      : set ? emit("_set", set) : emit(name.toLowerCase(), clone(args));
    let calls = 0; // sent or waiting to be
    const run = (fn) => {
      const go = () => { try { return Promise.resolve(fn()); } catch (err) { return Promise.reject(err); } };
      const p = calls++ ? chain.then(go) : go();
      chain = p.then(() => { calls--; }, () => { calls--; });
      return p;
    };
    const follow = (st, p) => {
      busy++;
      if (st) st.pending = true;
      notify();
      return p.then((r) => {
        busy--; lastErr = null;
        if (st) {
          st.pending = false; st.error = null;
          for (const w in held) { delete held[w]; delete local[w]; delete ferr[w]; }
        }
        notify();
        return r;
      }, (err) => {
        busy--; lastErr = err.vuka || { message: err.message };
        if (st) { st.pending = false; st.error = lastErr; }
        notify();
        throw err;
      });
    };
    const flush = () => {
      if (timer) { clearTimeout(timer); timer = null; }
      if (!queued) return;
      const set = queued, b = batch;
      queued = batch = null;
      const p = follow(null, run(() => send("", null, set)));
      const settle = () => { for (const w in set) { delete ferr[w]; if (local[w] === set[w]) delete local[w]; } notify(); };
      const refused = (err) => {
        const e = err && err.vuka || {}, keep = e.status === 422 && !!e.fields;
        for (const w in set) {
          ferr[w] = msgFor(e.fields, w);
          if (local[w] !== set[w]) continue;
          if (keep) held[w] = true;
          else delete local[w];
        }
        notify();
      };
      p.then(settle, refused);
      p.then(b.res, b.rej);
    };
    const setField = (w, d, v, o) => {
      local[w] = v;
      delete held[w];
      (queued = queued || {})[w] = v;
      if (!batch) {
        batch = {};
        batch.p = new Promise((res, rej) => { batch.res = res; batch.rej = rej; });
        batch.p.catch(() => {});
      }
      const p = batch.p;
      notify();
      if (timer) clearTimeout(timer);
      if ((o && o.now) || !d) flush();
      else timer = setTimeout(flush, d);
      return p;
    };
    const cur = (w) => w in local ? local[w] : inst.props[w];
    for (const n of names) {
      per[n] = { pending: false, error: null };
      methods[n] = (...args) => { flush(); return follow(per[n], run(() => send(n, args))); };
    }
    const view = (read) => {
      const r = read || (() => {});
      const v = {};
      for (const n of names) {
        const fn = (...args) => methods[n](...args);
        Object.defineProperty(fn, "pending", { get: () => (r(), per[n].pending) });
        Object.defineProperty(fn, "error", { get: () => (r(), per[n].error) });
        v[n] = fn;
      }
      for (const k in fields) {
        if (own.has(k) || k in v) continue;
        const w = fields[k].w, d = fields[k].d, bound = typeof d === "number";
        const f = { get: () => (r(), cur(w)) };
        Object.defineProperty(f, "value", { enumerable: true, get: f.get, set: bound ? (x) => { setField(w, d, x); } : undefined });
        if (bound) {
          f.set = (x, o) => setField(w, d, x, o);
          Object.defineProperty(f, "error", { enumerable: true, get: () => (r(), w in ferr ? ferr[w] : null) });
        } else if (fields[k].p) {
          Object.defineProperty(f, "pending", { enumerable: true, get: () => (r(), dpend.has(w) && !(w in derr)) });
          Object.defineProperty(f, "error", { enumerable: true, get: () => (r(), derr[w] || null) });
        }
        v[k] = f;
      }
      for (const k in navigation) if (!(k in v)) v[k] = navigation[k];
      Object.defineProperty(v, "pending", { enumerable: true, get: () => (r(), snap.pending) });
      Object.defineProperty(v, "error", { enumerable: true, get: () => (r(), snap.error) });
      return v;
    };
    inst.loadDeferred = () => { if (el.getAttribute("data-vk-call") && dpend.size > 0) load(); };
    const h = view(null);
    Object.assign(h, {
      methods, view, update: () => { readDefer(); notify(); },
      subscribe: (fn) => { subs.add(fn); return () => subs.delete(fn); },
      snapshot: () => snap, version: () => ver,
    });
    return h;
  }

  // go moves the page to href for an island (live.navigate / live.patch,
  // vuka.navigate / vuka.patch): over the live connection as a
  // data-vk-link would, else a page load; opts.replace takes the place of
  // the current history entry. Only http and https URLs are followed.
  function go(href, patch, opts) {
    const o = { ...(opts || {}), patch };
    if (window.vuka && window.vuka.navigate) return window.vuka.navigate(href, o);
    let u = null;
    try { u = new URL(String(href), location.href); } catch (_) {}
    if (!u || (u.protocol !== "http:" && u.protocol !== "https:")) return Promise.reject(new Error("vuka: refused to load a URL that isn't http or https"));
    o.replace ? location.replace(u.href) : location.assign(u.href);
    return new Promise(() => {});
  }
  const navigation = { navigate: (href, opts) => go(href, false, opts), patch: (href, opts) => go(href, true, opts) };

  // viewer is $vuka: the visitor as the server knows them (data-vk-viewer),
  // and navigate / patch.
  function viewer(el) {
    let v = null;
    try { v = JSON.parse(el.getAttribute("data-vk-viewer") || "null"); } catch (_) {}
    v = v || { user: null, roles: [], perms: [] };
    const perms = v.perms || [];
    v.can = (p) => perms.some((g) => g === "*" || g === p || g.endsWith(".*") && p.startsWith(g.slice(0, -1)));
    return Object.assign(v, navigation);
  }

  // syncURL keeps the address's query parameters of url:"name" props.
  function syncURL(inst) {
    let map = null;
    try { map = JSON.parse(inst.el.getAttribute("data-vk-url") || "null"); } catch (_) {}
    if (!map || typeof URL !== "function") return;
    const u = new URL(location.href);
    for (const k in map) {
      const v = inst.props[k];
      if (v == null || v === "" || v === false) u.searchParams.delete(map[k]);
      else u.searchParams.set(map[k], String(v));
    }
    if (u.href !== location.href) history.replaceState(history.state, "", u.pathname + u.search + u.hash);
  }

  function hydrateIsland(inst) {
    if (inst.state !== "armed") return;
    inst.state = "loading";
    const [moduleURL, adapterURL] = inst.spec;
    const ctx = context(inst);
    // the host's props as it mounts: patches may land while its module loads
    const go = () => {
      const raw = inst.el.getAttribute("data-vk-props") || "{}";
      const { props, refs } = islandProps(inst, raw);
      inst.props = props;
      inst.refs = refs;
      inst.key = plainKey(raw);
      // set writes a state prop back: the state, and what reads it.
      ctx.set = (name, v) => {
        const r = inst.refs[name];
        if (!r || r.x !== undefined || !states()) return;
        inst.props[name] = clone(v);
        states().write(r, clone(v));
      };
      ctx.bound = Object.keys(refs).filter((k) => refs[k].x === undefined);
      ctx.vuka = viewer(inst.el);
      if (["data-vk-methods", "data-vk-fields", "data-vk-call"].some((a) => inst.el.hasAttribute(a))) ctx.live = inst.live = liveHandle(inst);
      return props;
    };
    const want = adapterURL ? [import(moduleURL), import(adapterURL)] : [import(moduleURL)];
    Promise.all(want).then((mods) => {
      if (!inst.el.isConnected || islandInsts.get(inst.el) !== inst) return;
      const props = go();
      let handle;
      if (adapterURL) {
        const comp = mods[0].default || mods[0];
        const mount = mods[1].mount || (mods[1].default && mods[1].default.mount);
        handle = mount(comp, inst.el, { ...props }, ctx);
      } else {
        const mount = mods[0].mount || (mods[0].default && mods[0].default.mount);
        if (typeof mount !== "function") throw new Error("island " + inst.name + " has no mount export");
        handle = mount(inst.el, { ...props }, ctx);
      }
      inst.handle = handle || {};
      inst.state = "mounted";
      inst.dkey = inst.el.getAttribute("data-vk-defer") || "";
      watchStates(inst);
      if (inst.loadDeferred) inst.loadDeferred();
    }, (err) => { inst.state = "armed"; console.error("vuka: island " + inst.name + ":", err); }).catch((err) => console.error("vuka: island " + inst.name + ":", err));
  }

  // updateIslands follows a patch: a mounted island whose server props
  // changed gets update() with no remount (unless data-vk-once); its state
  // and function props are the browser's and don't count.
  function updateIslands() {
    for (const [el, inst] of islandInsts) {
      if (inst.state !== "mounted") continue;
      const raw = el.getAttribute("data-vk-props") || "";
      const key = plainKey(raw), dkey = el.getAttribute("data-vk-defer") || "";
      if (dkey !== inst.dkey) {
        inst.dkey = dkey;
        if (inst.live && key[0] === inst.key[0] && key[1] === inst.key[1]) inst.live.update();
      }
      if (key[0] === inst.key[0] && key[1] === inst.key[1]) continue;
      const refsChanged = key[1] !== inst.key[1];
      inst.key = key;
      if (el.hasAttribute("data-vk-once") && !refsChanged) continue;
      const { props, refs } = islandProps(inst, raw);
      if (el.hasAttribute("data-vk-once")) for (const k in props) if (!(k in refs)) props[k] = inst.props[k];
      inst.props = props;
      if (refsChanged) { inst.refs = refs; watchStates(inst); }
      if (inst.live) inst.live.update();
      pushProps(inst);
      syncURL(inst);
    }
  }

  function unmountIsland(inst) {
    islandInsts.delete(inst.el);
    if (inst.obs) inst.obs.disconnect();
    if (inst.mq) inst.mq[0].removeEventListener("change", inst.mq[1]);
    (inst.unwatch || []).forEach((u) => u());
    inst.unwatch = [];
    if (inst.handle && typeof inst.handle.unmount === "function") {
      try { inst.handle.unmount(); } catch (err) { console.error("vuka: island " + inst.name + " unmount:", err); }
    }
  }

  function update(next) {
    m = { m: next.m || {}, f: next.f || {}, g: next.g, e: next.e, i: next.i || {}, ic: next.ic || [], d: !!next.d, x: !!next.x };
    recheckMissing();
  }
  update(m);

  document.addEventListener("vk:patched", (e) => sync(e.detail), false);
  // PushEvent: a vk:event on an instance's root, not bubbling; seen here in
  // the capture phase, and kept for an instance whose module is loading.
  document.addEventListener("vk:event", (e) => {
    const inst = insts.get(e.target), d = e.detail || {};
    if (inst) { if (inst.hooks) deliver(inst, d); else inst.queue.push(d); return; }
    const isl = islandInsts.get(e.target);
    if (isl && isl.ons) deliver(isl, d);
  }, true);
  if (m.e && typeof EventSource === "function") {
    new EventSource(m.e).onmessage = (e) => { try { update(JSON.parse(e.data)); } catch (_) {} };
  }
  window.vukaComponents = { update, method, instances: () => [...insts.values()] };

  const start = () => {
    if (m.g) {
      global = mount(null, "", m.g);
      global.el = document.body;
      global.name = "app";
    }
    sync(null);
  };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
