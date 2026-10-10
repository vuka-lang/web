// The Vuka live runtime: it connects a page rendered with stateful components
// to its session over a WebSocket, sends the events its elements declare
// (data-vk-on-<event>), and morphs the page into what the server sends back,
// keeping focus, selection and what the user is typing.
(() => {
  "use strict";
  const script = document.currentScript;
  if (!script || window.vuka) return;
  const src = new URL(script.src, location.href);
  const url = (src.protocol === "https:" ? "wss://" : "ws://") + src.host +
    src.pathname.replace(/\.js$/, "") + "?t=" + encodeURIComponent(script.dataset.vkToken || "");
  const nobubble = new Set(["focus", "blur", "mouseenter", "mouseleave", "load", "error", "scroll"]);
  const listening = new Set();
  const busy = new Map(); // ref → {el, form}: elements waiting for their reply
  const pending = new Map(); // el → {timer, msg}: debounced events
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
        for (const u of ups) if (!done.some((el) => el.isConnected && el.contains(byID(u.id)))) done.push(apply(u));
        if (b && b.form && !m.error && b.form.isConnected) b.form.reset();
        if (m.error) fail(m.error, m.ref);
        emit("vk:patched", { ids: ups.map((u) => u.id) });
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
  // name, finding the element whose data-vk-on-<type> handles it.
  function listenIn(html) {
    for (const m of html.matchAll(/ data-vk-on-([\w-]+)=/g)) {
      if (listening.has(m[1])) continue;
      listening.add(m[1]);
      document.addEventListener(m[1], handle, true);
    }
  }

  function handle(e) {
    const attr = "data-vk-on-" + e.type;
    let el = e.target instanceof Element ? e.target : e.target && e.target.parentElement;
    if (el && nobubble.has(e.type)) el = el.hasAttribute(attr) ? el : null;
    else el = el && el.closest("[" + attr + "]");
    if (!el) return;
    if (e.type === "submit" || e.type === "click" && (el.tagName === "A" ||
        el.tagName === "BUTTON" && el.form && !el.form.hasAttribute("data-vk-on-submit"))) e.preventDefault();
    const keys = el.getAttribute("data-vk-key");
    if (keys && e.key !== undefined && !keys.split(/\s+/).includes(e.key)) return;
    if (!joined) return; // the join's render will replace what was clicked
    const msg = { type: "event", target: el.getAttribute(attr), event: e.type };
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
      return children(el, next);
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
      if (!next.hasAttribute(a.name) && !(waiting && a.name === "aria-busy")) el.removeAttribute(a.name);
    }
    for (const a of Array.from(next.attributes)) {
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    }
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

  window.vuka = { connect, tree, state: () => document.documentElement.getAttribute("data-vk-state") };
  window.addEventListener("online", () => { if (!ws) { attempt = 0; connect(); } });
  connect();
})();
