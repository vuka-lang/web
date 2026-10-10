// The Vuka live runtime: it connects a page rendered with stateful components
// to its session over a WebSocket, sends the events its elements declare
// (data-vk-on-<event>), and morphs the page into what the server sends back,
// keeping focus, selection and what the user is typing. It also runs JS
// commands (data-vk-js-<event>): changes to the page made in the browser,
// with or without a live connection.
(() => {
  "use strict";
  const script = document.currentScript;
  if (!script || window.vuka && window.vuka.connect) return;
  const live = script.hasAttribute("data-vk-token"); // a page with no live parts has none
  const src = new URL(script.src, location.href);
  const url = (src.protocol === "https:" ? "wss://" : "ws://") + src.host +
    src.pathname.replace(/\.js$/, "") + "?t=" + encodeURIComponent(script.dataset.vkToken || "");
  const nobubble = new Set(["focus", "blur", "mouseenter", "mouseleave", "load", "error", "scroll"]);
  const listening = new Set();
  const busy = new Map(); // ref → {el, form}: elements waiting for their reply
  const pending = new Map(); // el → {timer, msg}: debounced events
  const preds = new Map(); // ref → a sent push's predictions and hooks
  let rec = null; // what the running chain changed since its last push
  let ws = null, joined = false, ref = 0, attempt = 0, timer = 0, trees = null;

  const state = (s) => document.documentElement.setAttribute("data-vk-state", s);
  const emit = (name, detail) => document.dispatchEvent(new CustomEvent(name, { detail }));

  function connect() {
    clearTimeout(timer);
    state("connecting");
    ws = new WebSocket(url);
    ws.onopen = join;
    ws.onmessage = (e) => receive(JSON.parse(e.data));
    ws.onclose = (e) => {
      ws = null;
      joined = false;
      pending.forEach((p) => clearTimeout(p.timer));
      pending.clear();
      busy.forEach((b) => b.el.removeAttribute("aria-busy"));
      busy.clear();
      [...preds.keys()].forEach((r) => settle(r, true)); // no reply comes
      if (e.code === 4001) return reload(e.reason);
      if (e.code === 4004 || e.code === 1000) return state("closed");
      state("disconnected");
      timer = setTimeout(connect, Math.min(10000, 250 * 2 ** attempt++) * (0.5 + Math.random()));
    };
  }

  // join asks for the page in protocol v2 (render trees); a server that
  // doesn't speak it answers in v1 (HTML).
  const join = () => ws.send(JSON.stringify({ type: "join", ref: ++ref, v: 2 }));

  // reload fetches the page again for a fresh token, at most once in 10s.
  function reload(reason) {
    let last = 0;
    try { last = +sessionStorage.getItem("vk-reload") || 0; sessionStorage.setItem("vk-reload", Date.now()); } catch (_) {}
    if (Date.now() - last < 10000) {
      state("closed");
      return fail(reason || "live: the page can't reconnect");
    }
    location.reload();
  }

  function receive(m) {
    const b = m.ref && busy.get(m.ref);
    switch (m.type) {
      case "render":
        joined = true;
        attempt = 0;
        state("connected");
        document.documentElement.setAttribute("data-vk-protocol", m.trees ? "2" : "1");
        if (m.trees) {
          trees = tree();
          try { page(trees.apply(m.trees, true).c0); } catch (err) { fail("live: " + err.message); }
        } else {
          trees = null;
          page(m.html);
        }
        emit("vk:patched", { ids: ["c0"] });
        pushed(m.js);
        break;
      case "patch": {
        let ups = m.updates || [];
        if (m.trees) {
          try {
            const html = trees.apply(m.trees, false);
            ups = m.trees.map((e) => ({ id: e.id, html: html[e.id] }));
          } catch (err) { // out of step with the server: start over
            fail("live: " + err.message);
            joined = false;
            join();
            break;
          }
        }
        const done = [];
        for (const u of ups) if (!done.some((el) => el && el.isConnected && el.contains(byID(u.id)))) done.push(apply(u));
        if (b && b.form && !m.error && b.form.isConnected) b.form.reset();
        if (m.error) fail(m.error, m.ref);
        emit("vk:patched", { ids: ups.map((u) => u.id) });
        if (!m.error) pushed(m.js);
        break;
      }
      case "error":
        fail(m.error, m.ref);
        break;
      case "redirect":
        location.assign(m.url);
        break;
    }
    if (b) {
      busy.delete(m.ref);
      b.el.removeAttribute("aria-busy");
    }
    if (m.ref && m.type !== "redirect") settle(m.ref, m.type === "error" || !!m.error);
  }

  function fail(message, ref) {
    console.error("vuka:", message);
    emit("vk:error", { message, ref });
  }

  // page puts the page's HTML (a whole document, or the body's content) into
  // the body.
  function page(html) {
    let next;
    if (/<(html|body)[\s>]/i.test(html)) {
      const doc = new DOMParser().parseFromString(html, "text/html");
      if (doc.querySelector("title") && doc.title !== document.title) document.title = doc.title;
      next = doc.body;
    } else {
      next = fragment(html);
    }
    listenIn(html);
    children(document.body, next);
  }

  const byID = (id) => id === "c0" ? document.body : document.querySelector('[data-vk-id="' + id + '"]');

  // apply morphs an instance into its new HTML and returns its element.
  function apply(u) {
    if (u.id === "c0") return page(u.html), document.body;
    const el = byID(u.id);
    const next = fragment(u.html).firstElementChild;
    listenIn(u.html);
    if (el && next) morph(el, next);
    return el && el.isConnected ? el : byID(u.id);
  }

  function fragment(html) {
    const t = document.createElement("template");
    t.innerHTML = html;
    return t.content;
  }

  // Events: one capturing listener on the document per event type the pages
  // name, finding the nearest element whose data-vk-js-<type> (JS commands)
  // or data-vk-on-<type> (a handler on the server) handles it. An element
  // with both runs its commands first; a confirm the user declines among
  // them keeps the event from the server.
  function listenIn(html) {
    for (const m of html.matchAll(/ data-vk-(?:on|js)-([\w-]+)=/g)) {
      if (listening.has(m[1])) continue;
      listening.add(m[1]);
      document.addEventListener(m[1], handle, true);
    }
  }

  function owner(el, type) {
    const on = "data-vk-on-" + type, js = "data-vk-js-" + type;
    if (!el) return null;
    if (nobubble.has(type)) return el.hasAttribute(on) || el.hasAttribute(js) ? el : null;
    return el.closest("[" + on + "],[" + js + "]");
  }

  // prevent keeps an event the server answers from doing what the browser
  // would: a submit, a link followed, a form's button submitting it.
  function prevent(el, e) {
    if (e.type === "submit" || e.type === "click" && (el.tagName === "A" ||
        el.tagName === "BUTTON" && el.form && !el.form.hasAttribute("data-vk-on-submit"))) e.preventDefault();
  }

  function handle(e) {
    const t = e.target instanceof Element ? e.target : e.target && e.target.parentElement;
    const el = owner(t, e.type);
    if (!el || el.hasAttribute("data-vk-js-" + e.type) && commands(el, e) === false) return;
    if (!el.hasAttribute("data-vk-on-" + e.type)) return;
    prevent(el, e);
    const keys = el.getAttribute("data-vk-key");
    if (keys && e.key !== undefined && !keys.split(/\s+/).includes(e.key)) return;
    if (!joined) return; // the join's render will replace what was clicked
    const msg = { type: "event", target: el.getAttribute("data-vk-on-" + e.type), event: e.type };
    if (typeof e.key === "string") msg.key = e.key;
    const v = valueOf(el);
    if (v !== undefined) msg.value = v;
    if (e.type === "submit") msg.form = formOf(e.target.closest("form") || el, e.submitter);
    const wait = +el.getAttribute("data-vk-debounce");
    if (wait > 0 && e.type !== "submit") {
      const p = pending.get(el);
      if (p) clearTimeout(p.timer);
      pending.set(el, { msg, timer: setTimeout(() => { pending.delete(el); send(msg); }, wait) });
      return;
    }
    flush();
    send(msg, el, e.type === "submit" ? e.target.closest("form") : null);
  }

  function flush() {
    pending.forEach((p) => { clearTimeout(p.timer); send(p.msg); });
    pending.clear();
  }

  function send(msg, el, form) {
    if (!ws || !joined) return;
    msg.ref = ++ref;
    if (el && (form || msg.event === "click")) {
      busy.set(msg.ref, { el, form });
      el.setAttribute("aria-busy", "true");
    }
    ws.send(JSON.stringify(msg));
    return msg.ref;
  }

  const isField = (el) => el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT";
  const checkable = (el) => el.tagName === "INPUT" && (el.type === "checkbox" || el.type === "radio");

  function valueOf(el) {
    if (el.tagName === "INPUT" && el.type === "checkbox") return el.checked ? "true" : "false";
    if (isField(el) || el.tagName === "BUTTON" || el.tagName === "OPTION") return el.value;
  }

  function formOf(form, submitter) {
    let fd;
    try { fd = new FormData(form, submitter || undefined); } catch (_) { fd = new FormData(form); }
    const out = {};
    for (const [k, v] of fd) if (typeof v === "string") (out[k] ||= []).push(v);
    return out;
  }

  // morph makes el match next in place: attributes and children are updated,
  // children matched by id or data-vk-id, else by position and tag. A field
  // keeps what the user sees while it has focus; otherwise it takes the
  // server's value when that changed since the last render.
  function morph(el, next) {
    if (el.tagName !== next.tagName) return el.replaceWith(document.importNode(next, true));
    if (el.hasAttribute("data-vk-ignore") && el.id && el.id === next.id) return;
    if (!isField(el)) {
      attrs(el, next);
      return keptText(el, next) || children(el, next);
    }
    const focused = el === document.activeElement;
    const seen = fieldState(el), before = serverValue(el);
    attrs(el, next);
    if (el.tagName === "TEXTAREA") {
      if (el.textContent !== next.textContent) el.textContent = next.textContent;
    } else {
      children(el, next);
    }
    if (focused) restore(el, seen);
    else if (serverValue(el) !== before) takeServer(el);
  }

  const keyOf = (n) => n.nodeType === 1 ? n.id || n.getAttribute("data-vk-id") || "" : "";
  const same = (a, b) => a.nodeType === b.nodeType && (a.nodeType !== 1 || a.tagName === b.tagName);

  function children(el, next) {
    let cur = el.firstChild;
    for (const n of Array.from(next.childNodes)) {
      let match = null;
      const k = keyOf(n);
      if (k) match = Array.from(el.children).find((c) => keyOf(c) === k && c.tagName === n.tagName) || null;
      if (!match && cur && same(cur, n)) match = cur;
      if (match) {
        if (match !== cur) el.insertBefore(match, cur);
        if (n.nodeType === 1) morph(match, n);
        else if (match.nodeValue !== n.nodeValue) match.nodeValue = n.nodeValue;
        cur = match.nextSibling;
      } else {
        el.insertBefore(document.importNode(n, true), cur);
      }
    }
    while (cur) {
      const gone = cur;
      cur = cur.nextSibling;
      el.removeChild(gone);
    }
  }

  function attrs(el, next) {
    let waiting = false;
    busy.forEach((b) => { waiting = waiting || b.el === el; });
    for (const a of Array.from(el.attributes)) {
      if (!next.hasAttribute(a.name) && !(waiting && a.name === "aria-busy") && !/^data-vk-(pending|failed)$/.test(a.name)) el.removeAttribute(a.name);
    }
    for (const a of Array.from(next.attributes)) {
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    }
    keep(el, next);
  }

  function serverValue(el) {
    if (checkable(el)) return el.hasAttribute("checked") ? "1" : "";
    if (el.tagName === "TEXTAREA") return el.textContent;
    if (el.tagName === "SELECT") return Array.from(el.options).filter((o) => o.hasAttribute("selected")).map((o) => o.value).join("\0");
    return el.getAttribute("value");
  }

  function takeServer(el) {
    if (checkable(el)) {
      el.checked = el.hasAttribute("checked");
    } else if (el.tagName === "SELECT") {
      let any = false;
      for (const o of el.options) any = (o.selected = o.hasAttribute("selected")) || any;
      if (!any && !el.multiple && el.options.length) el.selectedIndex = 0;
    } else {
      const v = (el.tagName === "TEXTAREA" ? el.textContent : el.getAttribute("value")) || "";
      if (el.value !== v) el.value = v;
    }
  }

  function fieldState(el) {
    if (checkable(el)) return el.checked;
    if (el.tagName === "SELECT") return Array.from(el.options).map((o) => o.selected ? o.value : null);
    return { value: el.value, start: el.selectionStart, end: el.selectionEnd };
  }

  // restore puts back what a focused field showed: its value and caret.
  function restore(el, seen) {
    if (checkable(el)) {
      el.checked = seen;
    } else if (el.tagName === "SELECT") {
      const chosen = new Set(seen.filter((v) => v !== null));
      for (const o of el.options) o.selected = chosen.has(o.value);
    } else if (el.value !== seen.value) {
      el.value = seen.value;
      try { el.setSelectionRange(seen.start, seen.end); } catch (_) {}
    }
  }

  // Protocol v2: the server sends each instance's render tree, frames of
  // statics (sent once per connection, by id) around dynamics (markup,
  // nested frames, blocks of item frames, instance references), then only
  // the changes to it. tree() keeps a connection's trees and turns them back
  // into each changed instance's HTML for morph, as nexus and LiveView do.
  function tree() {
    const statics = new Map(), strs = new Map(), held = new Map(); // id → frame, or {html}
    const bad = (what) => { throw new Error("bad tree: " + what); };
    const frame = (o) => {
      if (o.s) statics.set(o.t, o.s);
      const s = statics.get(o.t);
      if (!s || !Array.isArray(o.d) || s.length !== o.d.length + 1) bad("statics " + o.t);
      return { t: o.t, d: o.d.map(dyn) };
    };
    const dyn = (v) => {
      if (typeof v === "string") return v;
      if (!v || typeof v !== "object") bad("dynamic");
      if (v.r !== undefined) {
        if (v.v !== undefined) strs.set(v.r, v.v);
        return strs.has(v.r) ? strs.get(v.r) : bad("string " + v.r);
      }
      if (v.c) return { c: v.c.map(frame) };
      if (v.id !== undefined) return { id: v.id };
      return frame(v);
    };
    const update = (f, u) => {
      if (!f || !f.d) bad("change of a non-frame");
      for (const k in u) {
        if (!(+k < f.d.length)) bad("dynamic " + k);
        f.d[+k] = change(f.d[+k], u[k]);
      }
      return f;
    };
    const change = (old, x) => {
      if (x && x.u) return update(old, x.u);
      if (x && x.k) {
        if (!old || !old.c) bad("steps on a non-block");
        const out = [];
        let i = 0;
        for (const st of x.k) {
          if (typeof st === "number") {
            if (st > 0) {
              if (i + st > old.c.length) bad("past the items");
              for (let n = 0; n < st; n++) out.push(old.c[i++]);
            } else i -= st;
          } else if (st.m !== undefined) {
            if (!old.c[st.m]) bad("item " + st.m);
            out.push(st.u ? update(old.c[st.m], st.u) : old.c[st.m]);
          } else if (st.u) {
            if (i >= old.c.length) bad("past the items");
            out.push(update(old.c[i++], st.u));
          } else out.push(frame(st));
        }
        return { c: out };
      }
      if (x && x.p) {
        if (typeof old !== "string") bad("token patch of a non-string");
        return patchText(old, x.p);
      }
      return dyn(x);
    };
    const html = (id, seen) => {
      const h = held.get(id);
      if (!h || seen.has(id)) bad("instance " + id);
      if (h.html !== undefined) return h.html;
      seen.add(id);
      const out = frameHTML(h, seen);
      seen.delete(id);
      return out;
    };
    const frameHTML = (f, seen) => {
      const s = statics.get(f.t);
      let out = s[0];
      f.d.forEach((d, i) => {
        if (typeof d === "string") out += d;
        else if (d.c) d.c.forEach((it) => { out += frameHTML(it, seen); });
        else if (d.id !== undefined) out += html(d.id, seen);
        else out += frameHTML(d, seen);
        out += s[i + 1];
      });
      return out;
    };
    // apply takes a message's entries and returns the new HTML of each
    // instance they name; render (true) starts over.
    const apply = (entries, render) => {
      if (render) { statics.clear(); strs.clear(); held.clear(); }
      for (const e of entries) {
        if (e.html !== undefined) held.set(e.id, { html: e.html });
        else if (e.full) held.set(e.id, frame(e.tree));
        else if (held.has(e.id) && held.get(e.id).d) update(held.get(e.id), e.tree && e.tree.u);
        else bad("change of instance " + e.id);
      }
      const out = {};
      for (const e of entries) out[e.id] = html(e.id, new Set());
      if (render || entries.some((e) => e.full)) prune();
      return out;
    };
    // prune drops the instances c0 no longer reaches.
    const prune = () => {
      const keep = new Set(), walk = (d) => {
        if (!d || typeof d !== "object") return;
        if (d.id !== undefined) { if (!keep.has(d.id)) { keep.add(d.id); walk(held.get(d.id)); } }
        else (d.c || d.d || []).forEach(walk);
      };
      keep.add("c0");
      walk(held.get("c0"));
      for (const id of held.keys()) if (!keep.has(id)) held.delete(id);
    };
    return { apply, page: () => html("c0", new Set()) };
  }

  // patchText applies a token patch: both strings cut before every "<" and
  // after every ">", '"' and "&#34;"; n copies the next n old tokens, -n
  // skips them, a string is inserted, [p, n] copies n old tokens from p.
  function patchText(old, steps) {
    const toks = old.split(/(?=<)|(?<=>|"|&#34;)/);
    let out = "", i = 0;
    for (const st of steps) {
      if (typeof st === "string") out += st;
      else if (Array.isArray(st)) out += toks.slice(st[0], st[0] + st[1]).join("");
      else if (st > 0) { out += toks.slice(i, i + st).join(""); i += st; }
      else i -= st;
    }
    return out;
  }

  // JS commands: data-vk-js-<event>='[[name, args], …]' changes the page in
  // the browser, in order; a render or patch may carry commands to run once
  // it is applied ("js": [{"id", "ops"}]), on that instance's root.
  const warned = new Set();
  const warn = (what) => { if (!warned.has(what)) { warned.add(what); console.warn("vuka: " + what); } };

  function commands(el, e) {
    let ops;
    try { ops = JSON.parse(el.getAttribute("data-vk-js-" + e.type)); } catch (_) {}
    if (!Array.isArray(ops)) return warn("bad JS commands in data-vk-js-" + e.type);
    if (e.type === "submit" || ops.some((o) => o[0] === "push")) prevent(el, e);
    return run(el, ops, e);
  }

  function pushed(list) {
    for (const p of list || []) {
      const root = byID(p.id);
      if (root) try { run(root, p.ops || [], null, []); } catch (err) { fail("live: a pushed command failed: " + err.message); }
    }
  }

  // run runs ops on behalf of el (this) for event e (null when pushed); false
  // means a confirm was declined or a call stopped the chain. A chain records
  // what its steps change (rec) until a push takes the record as its
  // predictions; a chain run from inside one (exec) shares its record, and r
  // starts a record of its own (or carries one across a call it waits for).
  function run(el, ops, e, r) {
    if (rec && !r) return steps(el, ops, e);
    const outer = rec;
    rec = r || [];
    try { return steps(el, ops, e); } finally { rec = outer; }
  }

  function steps(el, ops, e) {
    for (let i = 0; i < ops.length; i++) {
      const [name, a = {}] = ops[i];
      if (name === "call") {
        const out = call(el, a, e), rest = ops.slice(i + 1), r = rec;
        if (out === false) return false;
        if (out === stop) return refuse(el, rest, r), false;
        if (out && typeof out.then === "function") {
          out.then((v) => { if (v !== false) run(el, rest, e, r); },
            (err) => { fail("live: " + a.fn + ": " + (err && err.message || err)); refuse(el, rest, r); });
          return;
        }
      } else if (name === "if") i = branch(el, ops, i);
      else if (name === "else_if" || name === "else") i = end(ops, i);
      else if (name === "debounce" || name === "throttle") {
        const rest = ops.slice(i + 1), r = rec;
        return pace(name, el, e, a.ms || 0, () => run(el, rest, e, r));
      } else if (name === "key") {
        if (!e || !(a.keys || []).includes(e.key)) return;
      } else if (name === "confirm") {
        if (!window.confirm(a.msg)) return false;
      } else if (name !== "end") {
        if (jsOps[name]) jsOps[name](el, a, e);
        else warn("unknown JS command " + name);
      }
    }
  }

  // Calling JavaScript: a function registered with vuka.register, else one
  // at a dotted path on window, called as fn(ctx, ...args) — never eval'd.
  const fns = new Map(), stop = {};
  function call(el, a, e) {
    const path = String(a.fn || "");
    let fn = fns.get(path), self = window;
    if (!fn && /^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$/.test(path)) {
      fn = window;
      for (const k of path.split(".")) { self = fn; fn = fn == null ? undefined : fn[k]; }
    }
    if (typeof fn !== "function") {
      warn("no JavaScript function " + path);
      fail("live: no JavaScript function " + path);
      return stop;
    }
    const at = a.to ? targets(el, a.to)[0] || null : el;
    const root = at && at.closest("[data-vk-id]") || document;
    const ctx = { el: at, event: e, root, target: (to) => targets(at, to) };
    try {
      return fn.apply(fns.has(path) ? undefined : self, [ctx, ...(a.args || []).map((v) => arg(el, v))]);
    } catch (err) {
      fail("live: " + path + ": " + (err && err.message || err));
      return stop;
    }
  }
  // arg resolves a read marker — {"$read": {to, value|checked|attr}} — to
  // what the browser holds now.
  function arg(el, v) {
    if (!v || typeof v !== "object" || !v.$read) return v;
    const c = v.$read, t = targets(el, c.to)[0];
    if (!t) return null;
    if ("checked" in c) return !!t.checked;
    if ("attr" in c) return t.getAttribute(c.attr);
    return read(t);
  }
  // refuse ends a chain a call stopped: the next push's predictions (what
  // the chain changed since its last push) are undone and the push fails
  // without being sent.
  function refuse(el, rest, r) {
    const p = rest.find((o) => o[0] === "push");
    if (p) settle(track(el, p[1] || {}, r ? r.splice(0) : []), true);
  }

  // Optimistic updates: a sent push keeps what its chain changed before it
  // (its predictions) until its reply. A reply with an error, no reply in
  // time, or a lost connection undoes them (but not what the server has
  // changed since) and runs on_error; a reply without one runs on_ok.
  let local = 0;
  function track(el, a, recs, sent) {
    const id = sent || "local" + ++local;
    const root = el && el.closest("[data-vk-id]");
    const p = { el, a, recs, root: root && root.getAttribute("data-vk-id") };
    preds.set(id, p);
    if (el) {
      el.removeAttribute("data-vk-failed");
      el.setAttribute("data-vk-pending", "");
    }
    if (sent) p.timer = setTimeout(() => { fail("live: no reply to event " + id, id); settle(id, true); }, a.timeout || 10000);
    return id;
  }
  function settle(id, error) {
    const p = preds.get(id);
    if (!p) return;
    preds.delete(id);
    clearTimeout(p.timer);
    const el = p.el;
    if (el && ![...preds.values()].some((q) => q.el === el)) el.removeAttribute("data-vk-pending");
    if (error) {
      if (!p.a.keep) for (let i = p.recs.length; i--;) undo(p.recs[i]);
      if (el) el.setAttribute("data-vk-failed", "");
    }
    const hooks = error ? p.a.on_error : p.a.on_ok;
    if (hooks) run(el && el.isConnected ? el : p.root && byID(p.root) || document.body, hooks, null, []);
  }
  // undo puts back what a prediction changed, unless the server has changed
  // it since (the reconciliation record is gone, or the field's server value
  // moved).
  function undo(r) {
    const el = r.el, t = running.get(el);
    if (t) t.finish();
    if (r.field) {
      if (serverValue(el) !== r.server) return;
      if (checkable(el)) el.checked = r.before;
      else el.value = r.before;
      return;
    }
    const name = r.cls ? "class" : r.name, m = touched.get(el), k = m && m.get(name);
    if (!k) return;
    if (r.cls) el.classList[r.had ? "add" : "remove"](r.cls);
    else if (name === TEXT) el.textContent = r.before;
    else if (r.before === null) el.removeAttribute(name);
    else el.setAttribute(name, r.before);
    k.value = val(el, name);
    if (k.value === k.server) m.delete(name);
    if (!m.size) touched.delete(el);
  }

  // branch returns the index after which the branch of the if at i that
  // holds starts, or its end when none does; end, from an else_if or else
  // reached while running a branch, the end of its block.
  function branch(el, ops, i) {
    for (let j = i, depth = 0; j < ops.length; j++) {
      const n = ops[j][0];
      if (j > i && n === "if") depth++;
      else if (n === "end" && depth-- === 0) return j;
      else if (depth === 0 && (n === "else" || (n === "if" || n === "else_if") && holds(el, (ops[j][1] || {}).cond || {}))) return j;
    }
    return ops.length;
  }
  function end(ops, i) {
    for (let j = i + 1, depth = 0; j < ops.length; j++) {
      if (ops[j][0] === "if") depth++;
      else if (ops[j][0] === "end" && depth-- === 0) return j;
    }
    return ops.length;
  }

  // targets finds what a command acts on: el, or elements by ref (within
  // el's instance, not the instances inside it), selector, closest ancestor,
  // or selector inside el.
  function targets(el, to) {
    if (!to) return el ? [el] : [];
    try {
      if (to.ref !== undefined) {
        const scope = el && el.closest("[data-vk-id]");
        const own = (x) => !scope || x === scope || x.parentElement.closest("[data-vk-id]") === scope;
        const all = Array.from((scope || document).querySelectorAll("[data-vk-ref]"));
        if (scope && scope.hasAttribute("data-vk-ref")) all.unshift(scope);
        return all.filter((x) => x.getAttribute("data-vk-ref") === String(to.ref) && own(x));
      }
      if (to.sel) return Array.from(document.querySelectorAll(to.sel));
      if (to.closest) { const c = el && el.closest(to.closest); return c ? [c] : []; }
      if (to.inner) return el ? Array.from(el.querySelectorAll(to.inner)) : [];
    } catch (err) { warn(err.message); }
    return [];
  }

  // holds tests a condition on the first element its target names; only
  // exists holds without one.
  const read = (t) => isField(t) ? t.value : t.getAttribute("value") || "";
  function holds(el, c) {
    const t = targets(el, c.to)[0];
    if ("exists" in c) return !!t === !!c.exists;
    if (!t) return false;
    if ("checked" in c) return t.tagName === "INPUT" && !!t.checked === !!c.checked;
    if ("value_eq" in c) return read(t) === String(c.value_eq);
    if ("value" in c) return !!read(t) === !!c.value;
    if ("is" in c) return t.matches(c.is);
    if ("attr" in c) return t.hasAttribute(c.attr);
    return true;
  }

  // Reconciliation: per element, each attribute (or its text, TEXT) a
  // command changed, with the server's value then and the command's. A
  // morph that renders it as before puts the command's value back; one that
  // renders it differently wins, and it is the server's again. A running
  // chain also notes the value before each change (rec), for a push to undo.
  const touched = new WeakMap(), TEXT = "#text";
  const val = (el, name) => name === TEXT ? el.textContent : el.getAttribute(name);
  function note(el, name) {
    if (rec && !rec.some((r) => r.el === el && r.name === name)) rec.push({ el, name, before: val(el, name) });
  }
  function touch(el, name, change, cls) {
    if (cls) { if (rec) cls.forEach((c) => rec.push({ el, cls: c, had: el.classList.contains(c) })); } else note(el, name);
    let m = touched.get(el);
    if (!m) touched.set(el, (m = new Map()));
    if (!m.has(name)) m.set(name, { server: val(el, name) });
    change();
    m.get(name).value = val(el, name);
  }
  function keptText(el, next) {
    const m = touched.get(el), r = m && m.get(TEXT);
    if (!r) return false;
    if (next.textContent === r.server) {
      if (el.textContent !== r.value) el.textContent = r.value;
      return true;
    }
    m.delete(TEXT);
    if (!m.size) touched.delete(el);
    return false;
  }
  function keep(el, next) {
    const m = touched.get(el);
    if (m) {
      m.forEach((r, name) => {
        if (name === TEXT) return;
        if (next.getAttribute(name) !== r.server) return m.delete(name);
        if (r.value === null) el.removeAttribute(name);
        else if (el.getAttribute(name) !== r.value) el.setAttribute(name, r.value);
      });
      if (!m.size) touched.delete(el);
    }
    const t = running.get(el); // a transition's classes outlive a morph
    if (t) t.cls.forEach((c) => el.classList.add(c));
  }

  const words = (v) => Array.isArray(v) ? v.flatMap(words) : String(v || "").split(/\s+/).filter(Boolean);
  const classes = (op) => (el, a) => targets(el, a.to).forEach((t) => touch(t, "class", () => words(a.names).forEach((c) => t.classList[op](c)), words(a.names)));

  // Display: show and hide edit the style attribute's display (what a
  // morph re-renders and a command must keep); show also removes hidden.
  const displayRe = /(^|;)\s*display\s*:[^;]*;?/i;
  const display = (el) => (/(?:^|;)\s*display\s*:\s*([^;]*)/i.exec(el.getAttribute("style") || "") || [])[1] || "";
  function setDisplay(el, v) {
    touch(el, "style", () => {
      const rest = (el.getAttribute("style") || "").replace(displayRe, "$1").replace(/^\s*;|;\s*$/g, "").trim();
      const style = v ? (rest ? rest + "; " : "") + "display: " + v : rest;
      if (style) el.setAttribute("style", style);
      else el.removeAttribute("style");
    });
  }
  const shown = (el) => !el.hasAttribute("hidden") && display(el).trim() !== "none" &&
    (typeof getComputedStyle !== "function" || getComputedStyle(el).display !== "none");
  function show(el, a) {
    if (el.hasAttribute("hidden")) touch(el, "hidden", () => el.removeAttribute("hidden"));
    setDisplay(el, a.display || "");
    if (!a.display && typeof getComputedStyle === "function" && getComputedStyle(el).display === "none") setDisplay(el, "block");
  }
  const hide = (el) => setDisplay(el, "none");
  const visibly = (el, a, on) => {
    if (!a.transition) return on ? show(el, a) : hide(el);
    if (!on) note(el, "style"); // hidden at the transition's end
    animate(el, a.transition, a.time, on ? () => show(el, a) : null, on ? null : () => hide(el));
  };

  // Transitions: during and from go on, a frame later from gives way to to,
  // and after time all three come off. Their classes are the browser's
  // alone; a new transition on an element finishes the one running there.
  const running = new WeakMap();
  const frame = (fn) => typeof requestAnimationFrame === "function"
    ? requestAnimationFrame(() => requestAnimationFrame(fn)) : setTimeout(fn, 16);
  function animate(el, tr, time, before, after) {
    const prev = running.get(el);
    if (prev) prev.finish();
    const [during, from, to] = [0, 1, 2].map((i) => words((tr || [])[i]));
    let done = false, timer = 0;
    const r = { cls: new Set([...during, ...from]), finish() {
      if (done) return;
      done = true;
      clearTimeout(timer);
      running.delete(el);
      [...during, ...from, ...to].forEach((c) => el.classList.remove(c));
      if (after) after();
    } };
    running.set(el, r);
    r.cls.forEach((c) => el.classList.add(c));
    if (before) before();
    frame(() => {
      if (done) return;
      from.forEach((c) => { el.classList.remove(c); r.cls.delete(c); });
      to.forEach((c) => { el.classList.add(c); r.cls.add(c); });
      timer = setTimeout(r.finish, time == null ? 200 : time);
    });
  }

  const focusStack = [];
  const focusable = "a[href], button:not([disabled]), input:not([disabled]):not([type=hidden]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";
  const first = (t) => t.querySelector("[autofocus]") || t.querySelector(focusable);

  // Debounce and throttle run the steps after them at a pace kept per
  // element and event type. A debounced step waiting in a form runs before
  // the form's submit; throttling a field's input still sends its last one.
  const paced = new WeakMap(), debouncing = new Set();
  function pace(kind, el, e, ms, fn) {
    const type = e ? e.type : "";
    if (type === "submit" && kind === "debounce") e.preventDefault();
    let m = paced.get(el);
    if (!m) paced.set(el, (m = {}));
    const p = m[kind + " " + type] ||= { el, type };
    if (kind === "debounce") {
      clearTimeout(p.timer);
      p.run = () => { clearTimeout(p.timer); debouncing.delete(p); fn(); };
      debouncing.add(p);
      p.timer = setTimeout(p.run, ms);
      return;
    }
    if (p.closed) {
      if (type === "submit") e.preventDefault();
      if (type === "input" || type === "change") p.last = fn;
      return;
    }
    p.closed = true;
    const reopen = () => {
      const last = p.last;
      p.last = null;
      if (!last) return void (p.closed = false);
      last();
      setTimeout(reopen, ms);
    };
    setTimeout(reopen, ms);
    fn();
  }
  document.addEventListener("submit", (e) => {
    debouncing.forEach((p) => { if (p.type !== "submit" && e.target.contains(p.el)) p.run(); });
  }, true);

  function copy(text, el) {
    const fallback = () => {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("style", "position: fixed; opacity: 0");
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (_) {}
      ta.remove();
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).catch(fallback);
    else fallback();
    if (el) {
      el.setAttribute("data-copied", "");
      setTimeout(() => el.removeAttribute("data-copied"), 1500);
    }
  }

  const each = (fn) => (el, a, e) => targets(el, a.to).forEach((t) => fn(t, a, e));
  const jsOps = {
    add_class: classes("add"),
    remove_class: classes("remove"),
    toggle_class: classes("toggle"),
    set_attr: each((t, a) => touch(t, a.name, () => t.setAttribute(a.name, a.value))),
    remove_attr: each((t, a) => touch(t, a.name, () => t.removeAttribute(a.name))),
    toggle_attr: each((t, a) => touch(t, a.name, () => {
      const [on, off] = a.values || [];
      if (off === undefined) t.hasAttribute(a.name) ? t.removeAttribute(a.name) : t.setAttribute(a.name, on == null ? "" : on);
      else t.setAttribute(a.name, t.getAttribute(a.name) === on ? off : on);
    })),
    show: each((t, a) => visibly(t, a, true)),
    hide: each((t, a) => visibly(t, a, false)),
    toggle: each((t, a) => visibly(t, a, !shown(t))),
    transition: each((t, a) => animate(t, a.transition, a.time)),
    focus: (el, a) => { const t = targets(el, a.to)[0]; if (t) t.focus(); },
    focus_first: (el, a) => { const t = targets(el, a.to)[0], f = t && first(t); if (f) f.focus(); },
    push_focus: (el, a) => { const t = a.to ? targets(el, a.to)[0] : document.activeElement; if (t) focusStack.push(t); },
    pop_focus: () => { const t = focusStack.pop(); if (t && t.isConnected) t.focus(); },
    dispatch: each((t, a) => t.dispatchEvent(new CustomEvent(a.event, { bubbles: a.bubbles !== false, detail: a.detail }))),
    exec: each((t, a, e) => {
      let ops;
      try { ops = JSON.parse(t.getAttribute(a.attr) || "[]"); } catch (_) {}
      if (Array.isArray(ops)) run(t, ops, e);
      else warn("bad JS commands in " + a.attr);
    }),
    copy: (el, a) => {
      const t = targets(el, a.to)[0];
      if (t) copy(isField(t) ? t.value : t.textContent.trim(), el);
    },
    copy_text: (el, a) => copy(String(a.text), el),
    scroll_to: (el, a) => { const t = targets(el, a.to)[0]; if (t) t.scrollIntoView({ block: "nearest", behavior: "smooth" }); },
    set_text: each((t, a) => touch(t, TEXT, () => { t.textContent = a.text == null ? "" : String(a.text); })),
    set_value: each((t, a) => {
      if (!isField(t)) return;
      if (rec) rec.push({ el: t, field: true, before: checkable(t) ? t.checked : t.value, server: serverValue(t) });
      if (t.type === "checkbox" || t.type === "radio") t.checked = a.value === "true";
      else t.value = a.value;
      t.dispatchEvent(new Event("input", { bubbles: true }));
      t.dispatchEvent(new Event("change", { bubbles: true }));
    }),
    // push sends an event to a handler as data-vk-on-<event> would.
    // Its predictions are what the chain changed since its last push.
    push: (el, a, e) => {
      if (!a.h) return; // rendered without a session
      if (!live) return warn("a push command needs a live page");
      const recs = rec ? rec.splice(0) : [];
      if (!joined) return settle(track(el, a, recs), true); // not sent: it fails at once
      const type = a.event || (e ? e.type : "push");
      const msg = { type: "event", target: a.h, event: type };
      if (e && typeof e.key === "string") msg.key = e.key;
      const v = "value" in a ? a.value : el && valueOf(el);
      if (v !== undefined && v !== null) msg.value = typeof v === "string" ? v : JSON.stringify(v);
      const form = type === "submit" ? (e && e.target.closest("form")) || (el && el.closest("form")) : null;
      if (form) msg.form = formOf(form, e && e.submitter);
      flush();
      const id = send(msg, el, form);
      if (id) track(el, a, recs, id);
    },
  };

  // register names a function for call; a page's script that runs first
  // queues its calls on window.vuka.q.
  const register = (name, fn) => { fns.set(String(name), fn); };
  ((window.vuka && window.vuka.q) || []).forEach((r) => register(...r));
  window.vuka = { connect, tree, register, run: (el, ops, e) => run(el, ops, e || null), state: () => document.documentElement.getAttribute("data-vk-state") };
  listenIn(document.documentElement.outerHTML);
  if (!live) return;
  window.addEventListener("online", () => { if (!ws) { attempt = 0; connect(); } });
  connect();
})();
