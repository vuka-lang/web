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
  let ws = null, joined = false, ref = 0, attempt = 0, timer = 0;

  const state = (s) => document.documentElement.setAttribute("data-vk-state", s);
  const emit = (name, detail) => document.dispatchEvent(new CustomEvent(name, { detail }));

  function connect() {
    clearTimeout(timer);
    state("connecting");
    ws = new WebSocket(url);
    ws.onopen = () => ws.send(JSON.stringify({ type: "join", ref: ++ref }));
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
        page(m.html);
        emit("vk:patched", { ids: ["c0"] });
        break;
      case "patch":
        (m.updates || []).forEach(apply);
        if (b && b.form && !m.error && b.form.isConnected) b.form.reset();
        if (m.error) fail(m.error, m.ref);
        emit("vk:patched", { ids: (m.updates || []).map((u) => u.id) });
        break;
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

  function apply(u) {
    if (u.id === "c0") return page(u.html);
    const el = document.querySelector('[data-vk-id="' + u.id + '"]');
    const next = fragment(u.html).firstElementChild;
    listenIn(u.html);
    if (el && next) morph(el, next);
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

  window.vuka = { connect, state: () => document.documentElement.getAttribute("data-vk-state") };
  window.addEventListener("online", () => { if (!ws) { attempt = 0; connect(); } });
  connect();
})();
