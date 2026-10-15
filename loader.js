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

  function syncIslands(d) {
    for (const [el, inst] of islandInsts) if (!el.isConnected) unmountIsland(inst);
    for (const el of islandRoots()) if (!islandInsts.has(el)) armIsland(el);
    if (d && !d.join) updateIslands();
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
    if (!spec) { el.setAttribute("data-vk-error", "unknown island"); return; }
    const inst = { el, id: scopeOf(el) || name, name, spec, ons: null, state: "armed", handle: null, props: {}, refs: {}, fns: new Map(), key: ["", ""] };
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

  function hydrateIsland(inst) {
    if (inst.state !== "armed") return;
    inst.state = "loading";
    const [moduleURL, adapterURL] = inst.spec;
    const ctx = context(inst);
    const raw = inst.el.getAttribute("data-vk-props") || "{}";
    const go = () => {
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
      return props;
    };
    const want = adapterURL ? [import(moduleURL), import(adapterURL)] : [import(moduleURL)];
    Promise.all(want).then((mods) => {
      if (!inst.el.isConnected || !islandInsts.has(inst.el)) return;
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
      watchStates(inst);
    }, (err) => { inst.state = "armed"; console.error("vuka: island " + inst.name + ":", err); }).catch((err) => console.error("vuka: island " + inst.name + ":", err));
  }

  // updateIslands follows a patch: a mounted island whose server props
  // changed gets update() with no remount (unless data-vk-once); its state
  // and function props are the browser's and don't count.
  function updateIslands() {
    for (const [el, inst] of islandInsts) {
      if (inst.state !== "mounted") continue;
      const raw = el.getAttribute("data-vk-props") || "";
      const key = plainKey(raw);
      if (key[0] === inst.key[0] && key[1] === inst.key[1]) continue;
      const refsChanged = key[1] !== inst.key[1];
      inst.key = key;
      if (el.hasAttribute("data-vk-once") && !refsChanged) continue;
      const { props, refs } = islandProps(inst, raw);
      if (el.hasAttribute("data-vk-once")) for (const k in props) if (!(k in refs)) props[k] = inst.props[k];
      inst.props = props;
      if (refsChanged) { inst.refs = refs; watchStates(inst); }
      pushProps(inst);
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
    m = { m: next.m || {}, f: next.f || {}, g: next.g, e: next.e, i: next.i || {}, ic: next.ic || [] };
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
