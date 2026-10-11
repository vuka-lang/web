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

  function update(next) {
    m = { m: next.m || {}, f: next.f || {}, g: next.g, e: next.e };
  }
  update(m);

  document.addEventListener("vk:patched", (e) => sync(e.detail), false);
  // PushEvent: a vk:event on an instance's root, not bubbling; seen here in
  // the capture phase, and kept for an instance whose module is loading.
  document.addEventListener("vk:event", (e) => {
    const inst = insts.get(e.target), d = e.detail || {};
    if (!inst) return;
    if (inst.hooks) deliver(inst, d);
    else inst.queue.push(d);
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
