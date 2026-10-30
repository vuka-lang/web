// A small DOM for running live.js in node with no dependencies: an HTML
// parser and serializer, the node methods the runtime uses, CSS selectors
// (tag, #id, .class, [attr], [attr=v], :not(…), descendant and child
// combinators, lists), events with capture and bubbling, and virtual timers.
// It is no browser: no layout, no CSS, no default actions.
"use strict";
const vm = require("vm"), fs = require("fs");

const VOID = new Set(["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"]);
const FIELDS = new Set(["INPUT", "TEXTAREA", "SELECT", "BUTTON", "OPTION"]);

class Event {
  constructor(type, init = {}) {
    Object.assign(this, init);
    this.type = type;
    this.bubbles = !!init.bubbles;
    this.defaultPrevented = false;
    this.stopped = false;
  }
  preventDefault() { this.defaultPrevented = true; }
  stopPropagation() { this.stopped = true; }
  stopImmediatePropagation() { this.stopped = this.immediate = true; }
}
class CustomEvent extends Event {
  constructor(type, init = {}) { super(type, init); this.detail = init.detail === undefined ? null : init.detail; }
}
class KeyboardEvent extends Event {}

class Node {
  constructor(doc) {
    this.ownerDocument = doc;
    this.parentNode = null;
    this.childNodes = [];
    this.listeners = {};
  }
  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get nextSibling() {
    const p = this.parentNode;
    return p ? p.childNodes[p.childNodes.indexOf(this) + 1] || null : null;
  }
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n.nodeType === 9;
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  insertBefore(n, ref) {
    if (n.nodeType === 11) {
      for (const c of [...n.childNodes]) this.insertBefore(c, ref);
      return n;
    }
    if (n.parentNode) n.parentNode.removeChild(n);
    const i = ref ? this.childNodes.indexOf(ref) : this.childNodes.length;
    this.childNodes.splice(i < 0 ? this.childNodes.length : i, 0, n);
    n.parentNode = this;
    return n;
  }
  appendChild(n) { return this.insertBefore(n, null); }
  append(...ns) { ns.forEach((n) => this.appendChild(typeof n === "string" ? new Text(this.ownerDocument, n) : n)); }
  removeChild(n) {
    const i = this.childNodes.indexOf(n);
    if (i >= 0) this.childNodes.splice(i, 1);
    n.parentNode = null;
    return n;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  replaceWith(n) {
    const p = this.parentNode;
    p.insertBefore(n, this);
    p.removeChild(this);
  }
  get children() { return this.childNodes.filter((c) => c.nodeType === 1); }
  get firstElementChild() { return this.children[0] || null; }
  get textContent() { return this.childNodes.filter((c) => c.nodeType !== 8).map((c) => c.textContent).join(""); }
  set textContent(v) {
    this.childNodes.forEach((c) => { c.parentNode = null; });
    this.childNodes = [];
    if (v !== "") this.appendChild(new Text(this.ownerDocument, String(v)));
  }
  querySelectorAll(sel) {
    const list = parseSelector(sel), out = [];
    const walk = (n) => n.children.forEach((c) => { if (list.some((s) => matchComplex(c, s))) out.push(c); walk(c); });
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  addEventListener(type, fn, opt) {
    (this.listeners[type] ||= []).push({ fn, capture: opt === true || !!(opt && opt.capture) });
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter((l) => l.fn !== fn);
  }
  dispatchEvent(e) {
    e.target = this;
    const path = [];
    for (let n = this.parentNode; n; n = n.parentNode) path.push(n);
    const call = (n, capture) => {
      e.currentTarget = n;
      for (const l of [...(n.listeners[e.type] || [])]) if (!e.immediate && (capture === null || l.capture === capture)) l.fn.call(n, e);
    };
    for (let i = path.length - 1; i >= 0 && !e.stopped; i--) call(path[i], true);
    if (!e.stopped) call(this, null);
    if (e.bubbles) for (let i = 0; i < path.length && !e.stopped; i++) call(path[i], false);
    return !e.defaultPrevented;
  }
}

class Text extends Node {
  constructor(doc, v) { super(doc); this.nodeType = 3; this.nodeValue = v; }
  get textContent() { return this.nodeValue; }
  set textContent(v) { this.nodeValue = v; }
  cloneNode(deep, doc) { return new Text(doc || this.ownerDocument, this.nodeValue); }
}

class Comment extends Node {
  constructor(doc, v) { super(doc); this.nodeType = 8; this.data = v; }
  get textContent() { return this.data; }
  get nodeValue() { return this.data; }
  set nodeValue(v) { this.data = v; }
  cloneNode(deep, doc) { return new Comment(doc || this.ownerDocument, this.data); }
}

class Fragment extends Node {
  constructor(doc) { super(doc); this.nodeType = 11; }
  cloneNode(deep, doc) { const f = new Fragment(doc || this.ownerDocument); if (deep) this.childNodes.forEach((c) => f.appendChild(c.cloneNode(true, doc))); return f; }
}

class Element extends Node {
  constructor(doc, tag) {
    super(doc);
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.attributes = [];
    if (this.tagName === "TEMPLATE") this.content = new Fragment(doc);
    if (FIELDS.has(this.tagName)) {
      let value = null, checked = null;
      Object.defineProperty(this, "value", {
        enumerable: true,
        get: () => value !== null ? value : this.tagName === "TEXTAREA" ? this.textContent : this.getAttribute("value") || "",
        set: (v) => { value = String(v); },
      });
      let selected = null;
      Object.defineProperty(this, "selected", {
        get: () => selected !== null ? selected : this.hasAttribute("selected"),
        set: (v) => { selected = !!v; },
      });
      Object.defineProperty(this, "checked", {
        get: () => checked !== null ? checked : this.hasAttribute("checked"),
        set: (v) => { checked = !!v; },
      });
    }
  }
  get id() { return this.getAttribute("id") || ""; }
  get options() { return this.tagName === "SELECT" ? this.querySelectorAll("option") : undefined; }
  get className() { return this.getAttribute("class") || ""; }
  set className(v) { this.setAttribute("class", String(v)); }
  get type() { return (this.getAttribute("type") || (this.tagName === "BUTTON" ? "submit" : "text")).toLowerCase(); }
  get form() { return this.parentElement && this.parentElement.closest("form"); }
  getAttribute(n) { const a = this.attributes.find((a) => a.name === n); return a ? a.value : null; }
  hasAttribute(n) { return this.attributes.some((a) => a.name === n); }
  setAttribute(n, v) {
    const a = this.attributes.find((a) => a.name === n);
    if (a) a.value = String(v);
    else this.attributes.push({ name: n, value: String(v) });
  }
  removeAttribute(n) { this.attributes = this.attributes.filter((a) => a.name !== n); }
  get classList() {
    const el = this;
    const list = () => el.className.split(/\s+/).filter(Boolean);
    const set = (l) => el.setAttribute("class", l.join(" "));
    return {
      add: (...cs) => { const l = list(); cs.forEach((c) => { if (!l.includes(c)) l.push(c); }); set(l); },
      remove: (...cs) => { if (el.hasAttribute("class")) set(list().filter((c) => !cs.includes(c))); },
      toggle: (c) => { const on = !list().includes(c); on ? this.classList.add(c) : this.classList.remove(c); return on; },
      contains: (c) => list().includes(c),
    };
  }
  matches(sel) { return parseSelector(sel).some((s) => matchComplex(this, s)); }
  closest(sel) {
    const list = parseSelector(sel);
    for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (list.some((s) => matchComplex(n, s))) return n;
    return null;
  }
  set innerHTML(html) {
    const into = this.content || this;
    into.textContent = "";
    parse(this.ownerDocument, html, into);
  }
  get innerHTML() { return (this.content || this).childNodes.map(serialize).join(""); }
  get outerHTML() { return serialize(this); }
  cloneNode(deep, doc) {
    const el = (doc || this.ownerDocument).createElement(this.tagName.toLowerCase());
    this.attributes.forEach((a) => el.setAttribute(a.name, a.value));
    if (deep) {
      this.childNodes.forEach((c) => el.appendChild(c.cloneNode(true, doc)));
      if (this.content) this.content.childNodes.forEach((c) => el.content.appendChild(c.cloneNode(true, doc)));
    }
    return el;
  }
  focus() { this.ownerDocument.activeElement = this; this.ownerDocument.focused.push(this); }
  blur() { if (this.ownerDocument.activeElement === this) this.ownerDocument.activeElement = this.ownerDocument.body; }
  select() {}
  reset() {}
  scrollIntoView(opt) { this.ownerDocument.scrolled.push([this, opt]); }
}

class Document extends Node {
  constructor() {
    super(null);
    this.ownerDocument = this;
    this.nodeType = 9;
    this.focused = [];
    this.scrolled = [];
    this.documentElement = this.createElement("html");
    this.appendChild(this.documentElement);
    this.head = this.createElement("head");
    this.body = this.createElement("body");
    this.documentElement.append(this.head, this.body);
    this.activeElement = this.body;
    this.title = "";
  }
  createElement(tag) { return new Element(this, tag); }
  createTextNode(v) { return new Text(this, v); }
  createComment(v) { return new Comment(this, v); }
  // createTreeWalker walks root's descendants, in document order, of the
  // kinds whatToShow names (1 elements, 4 text, 128 comments).
  createTreeWalker(root, whatToShow) {
    const all = [], walk = (n) => n.childNodes.forEach((c) => { if (whatToShow & (1 << (c.nodeType - 1))) all.push(c); walk(c); });
    walk(root.nodeType === 9 ? root.documentElement : root);
    let i = 0;
    return { nextNode: () => all[i++] || null };
  }
  importNode(n, deep) { return n.cloneNode(deep, this); }
  getElementById(id) { return this.querySelectorAll("*").find((e) => e.id === id) || null; }
  execCommand() { return true; }
}

function decode(s) {
  return s.replace(/&(#\d+|#x[\da-f]+|amp|lt|gt|quot|apos);/gi, (m, e) =>
    e[0] === "#" ? String.fromCodePoint(e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : +e.slice(1))
      : { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" }[e.toLowerCase()]);
}
const esc = (s, attr) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(attr ? /"/g : /$^/, "&#34;");

function parse(doc, html, into) {
  const stack = [into];
  const top = () => stack[stack.length - 1];
  const re = /<!--[\s\S]*?-->|<\/([a-zA-Z][\w-]*)\s*>|<([a-zA-Z][\w-]*)((?:\s+[^\s=/>]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*(\/?)>|([^<]+|<)/g;
  let m;
  while ((m = re.exec(html))) {
    if (m[0].startsWith("<!--")) {
      const parent = top();
      (parent.content || parent).appendChild(doc.createComment(m[0].slice(4, -3)));
    } else if (m[1]) {
      const tag = m[1].toUpperCase();
      for (let i = stack.length - 1; i > 0; i--) if (stack[i].tagName === tag) { stack.length = i; break; }
    } else if (m[2]) {
      const el = doc.createElement(m[2]);
      const ar = /([^\s=/>]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g;
      let a;
      while ((a = ar.exec(m[3]))) el.setAttribute(a[1].toLowerCase(), decode(a[2] ?? a[3] ?? a[4] ?? ""));
      const parent = top();
      (parent.content || parent).appendChild(el);
      if (!VOID.has(m[2].toLowerCase()) && !m[4]) stack.push(el);
    } else if (m[5]) {
      const parent = top();
      (parent.content || parent).appendChild(doc.createTextNode(decode(m[5])));
    }
  }
}

function serialize(n) {
  if (n.nodeType === 3) return esc(n.nodeValue);
  if (n.nodeType === 8) return "<!--" + n.data + "-->";
  if (n.nodeType === 11) return n.childNodes.map(serialize).join("");
  const tag = n.tagName.toLowerCase();
  const attrs = n.attributes.map((a) => " " + a.name + (a.value === "" ? "" : '="' + esc(a.value, true) + '"')).join("");
  if (VOID.has(tag)) return "<" + tag + attrs + ">";
  return "<" + tag + attrs + ">" + n.innerHTML + "</" + tag + ">";
}

// Selectors: a list of complex selectors, each compounds joined by " " or
// ">", matched right to left.
const cache = new Map();
function parseSelector(sel) {
  if (cache.has(sel)) return cache.get(sel);
  const out = split(sel, ",").map((s) => {
    const parts = [];
    let comb = " ", buf = "", depth = 0, quote = "";
    const flush = () => { if (buf) { parts.push({ comb, c: compound(buf) }); buf = ""; comb = " "; } };
    let escaped = false;
    for (const ch of s.trim()) {
      if (escaped) { buf += ch; escaped = false; continue; }
      if (ch === "\\") { buf += ch; escaped = true; continue; }
      if (quote) { buf += ch; if (ch === quote) quote = ""; continue; }
      if (ch === '"' || ch === "'") quote = ch;
      if (ch === "(" || ch === "[") depth++;
      if (ch === ")" || ch === "]") depth--;
      if (depth === 0 && (ch === " " || ch === ">")) { flush(); if (ch === ">") comb = ">"; continue; }
      buf += ch;
    }
    flush();
    return parts;
  });
  cache.set(sel, out);
  return out;
}
function split(s, sep) {
  const out = [];
  let buf = "", depth = 0, quote = "", escaped = false;
  for (const ch of s) {
    if (escaped) { buf += ch; escaped = false; continue; }
    if (ch === "\\") { buf += ch; escaped = true; continue; }
    if (quote) { buf += ch; if (ch === quote) quote = ""; continue; }
    if (ch === '"' || ch === "'") quote = ch;
    if (ch === "(" || ch === "[") depth++;
    if (ch === ")" || ch === "]") depth--;
    if (ch === sep && depth === 0) { out.push(buf); buf = ""; continue; }
    buf += ch;
  }
  out.push(buf);
  return out;
}
function compound(s) {
  const tests = [];
  const re = /^(\*|[a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[\s*((?:[\w-]|\\.)+)\s*(?:([~^$*|]?)=\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|([^\]\s]+)))?\s*\]|:not\(((?:[^()]|\([^()]*\))*)\)/g;
  let m, at = 0;
  while ((m = re.exec(s))) {
    if (m.index !== at) throw new SyntaxError("selector " + s);
    at = re.lastIndex;
    const [, tag, id, cls, rawAttr, op, v1, v2, v3, not] = m;
    const attr = rawAttr && unescape(rawAttr);
    if (tag) { const t = tag.toUpperCase(); if (t !== "*") tests.push((el) => el.tagName === t); }
    else if (id) tests.push((el) => el.id === id);
    else if (cls) tests.push((el) => el.classList.contains(cls));
    else if (attr) {
      const v = v1 ?? v2 ?? v3, want = v === undefined ? undefined : unescape(v);
      const test = {
        "": (h) => h === want, "^": (h) => want !== "" && h.startsWith(want), "$": (h) => want !== "" && h.endsWith(want),
        "*": (h) => want !== "" && h.includes(want), "~": (h) => h.split(/\s+/).includes(want), "|": (h) => h === want || h.startsWith(want + "-"),
      }[op || ""];
      tests.push((el) => el.hasAttribute(attr) && (want === undefined || test(el.getAttribute(attr))));
    } else if (not !== undefined) {
      const inner = parseSelector(not);
      tests.push((el) => !inner.some((c) => matchComplex(el, c)));
    }
  }
  if (at !== s.length) throw new SyntaxError("selector " + s);
  return (el) => tests.every((t) => t(el));
}
// unescape decodes a selector's CSS escapes: \ and up to six hex digits
// (and one space after them), or \ and any other character.
const unescape = (s) => s.replace(/\\(?:([\da-fA-F]{1,6})\s?|([\s\S]))/g, (_, hex, ch) => hex ? String.fromCodePoint(parseInt(hex, 16)) : ch);
function matchComplex(el, parts, i = parts.length - 1) {
  if (!parts[i].c(el)) return false;
  if (i === 0) return true;
  if (parts[i].comb === ">") return !!el.parentElement && matchComplex(el.parentElement, parts, i - 1);
  for (let p = el.parentElement; p; p = p.parentElement) if (matchComplex(p, parts, i - 1)) return true;
  return false;
}

// parseDocument is DOMParser's parseFromString: a whole document's head
// and body, its title.
function parseDocument(html) {
  const d = new Document(), tmp = d.createElement("div");
  parse(d, html, tmp);
  const top = tmp.children.find((c) => c.tagName === "HTML");
  const move = (from, to) => [...from.childNodes].forEach((c) => to.appendChild(c));
  if (top) {
    for (const c of top.children) {
      if (c.tagName === "HEAD") move(c, d.head);
      else if (c.tagName === "BODY") move(c, d.body);
    }
  } else move(tmp, d.body);
  const t = d.head.querySelector("title");
  d.title = t ? t.textContent : "";
  return d;
}

// page loads live.js into a fresh context over body: options.live gives
// the script a token (it connects), options.build the build that rendered
// it (it navigates over HTTP when it isn't live), options.root its root
// layout, options.dev marks it rendered in development, options.confirm answers window.confirm, options.vuka is
// window.vuka before the runtime loads, options.url the page's address
// (http://test/ by default). The page's fetches wait in fetches until
// reply answers one.
function page(file, body, options = {}) {
  const doc = new Document();
  doc.readyState = "complete"; // the page has loaded: the runtime asks for its parts at once
  doc.body.innerHTML = body;
  const timers = [];
  let now = 0, seq = 0;
  const setTimeout = (fn, ms) => { const id = ++seq; timers.push({ id, at: now + (ms || 0), fn }); return id; };
  const clearTimeout = (id) => { const i = timers.findIndex((t) => t.id === id); if (i >= 0) timers.splice(i, 1); };
  const tick = (ms) => {
    const until = now + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at || a.id - b.id);
      if (!timers.length || timers[0].at > until) break;
      const t = timers.shift();
      now = t.at;
      t.fn();
    }
    now = until;
  };
  const sockets = [], logs = [], confirms = [];
  // The address and the session history: loads are the page loads asked
  // for, entries what pushState added.
  const at = new URL(options.url || "http://test/"), loads = [], entries = [];
  const location = {
    get href() { return at.href; }, get origin() { return at.origin; }, get pathname() { return at.pathname; },
    get search() { return at.search; }, get hash() { return at.hash; }, set hash(h) { at.hash = h; },
    reload() { loads.push(["reload"]); }, assign(u) { loads.push(["assign", u]); }, replace(u) { loads.push(["replace", u]); },
  };
  const history = {
    state: null,
    scrollRestoration: "auto",
    pushState(st, _, u) { this.state = st; at.href = new URL(u, at.href).href; entries.push(at.href); },
    replaceState(st, _, u) { this.state = st; if (u) at.href = new URL(u, at.href).href; },
  };
  const scrolls = [];
  const copied = [];
  class WebSocket {
    constructor(url) { this.url = url; this.sent = []; sockets.push(this); }
    send(s) { this.sent.push(JSON.parse(s)); }
    close() {}
  }
  class FormData {
    constructor(form) {
      this.entries = form.querySelectorAll("input[name], textarea[name], select[name]")
        .filter((f) => !(f.type === "checkbox" || f.type === "radio") || f.checked)
        .map((f) => [f.getAttribute("name"), f.value]);
    }
    [Symbol.iterator]() { return this.entries[Symbol.iterator](); }
  }
  const dataset = options.live ? { vkToken: "tok" } : {};
  if (options.build) dataset.vkBuildId = options.build;
  if (options.root) dataset.vkRoot = options.root;
  const script = {
    src: "http://test/_vuka/live.js",
    dataset,
    hasAttribute: (n) => !!options.live && n === "data-vk-token" || !!options.dev && n === "data-vk-dev",
  };
  // fetch: each call waits in fetches, {url, headers, aborted}, until
  // reply(i, {status, headers, body, url}) answers it (or fails it, with
  // error); an aborted one rejects. With stream set, the body arrives in
  // pieces: reply returns {push(text), end()}, body the first piece.
  const fetches = [];
  class AbortController {
    constructor() { const s = this.signal = { aborted: false, fns: [] }; this.abort = () => { if (!s.aborted) { s.aborted = true; s.fns.forEach((f) => f()); } }; }
  }
  const fetch = (url, init = {}) => new Promise((resolve, reject) => {
    const f = { url: new URL(url, at.href).href, headers: init.headers || {}, aborted: false, resolve, reject };
    fetches.push(f);
    if (init.signal) init.signal.fns.push(() => { f.aborted = true; reject(Object.assign(new Error("aborted"), { name: "AbortError" })); });
  });
  const reply = (i, r = {}) => {
    const f = typeof i === "number" ? fetches[i] : i;
    if (r.error) return f.reject(new Error(r.error));
    const hs = { "content-type": "text/html; charset=utf-8" };
    for (const k in r.headers || {}) hs[k.toLowerCase()] = r.headers[k];
    const url = r.url ? new URL(r.url, at.href).href : f.url;
    const res = { status: r.status || 200, url, redirected: url !== f.url, headers: { get: (k) => k.toLowerCase() in hs ? hs[k.toLowerCase()] : null }, text: () => Promise.resolve(r.body || "") };
    if (!r.stream) return f.resolve(res);
    const pieces = [], waiting = [];
    let ended = false;
    const give = () => {
      while (waiting.length && (pieces.length || ended)) {
        const w = waiting.shift();
        w(pieces.length ? { done: false, value: new TextEncoder().encode(pieces.shift()) } : { done: true });
      }
    };
    const s = { push: (t) => { pieces.push(t); give(); }, end: () => { ended = true; give(); }, canceled: false };
    if (r.body) pieces.push(r.body);
    res.body = { getReader: () => ({ read: () => new Promise((w) => { waiting.push(w); give(); }), cancel: () => { s.canceled = true; ended = true; give(); return Promise.resolve(); } }) };
    f.resolve(res);
    return s;
  };
  class DOMParser { parseFromString(html) { return parseDocument(html); } }
  Object.defineProperty(doc, "currentScript", { value: script, configurable: true });
  const ctx = {
    document: doc, Element, Node, Event, CustomEvent, KeyboardEvent, URL, FormData, WebSocket, fetch, AbortController, DOMParser, TextDecoder, TextEncoder,
    setTimeout, clearTimeout, requestAnimationFrame: (fn) => setTimeout(fn, 16),
    location, history, scrollX: 0, scrollY: 0, scrollTo: (x, y) => scrolls.push([x, y]),
    sessionStorage: { getItem: () => null, setItem() {} },
    navigator: { clipboard: { writeText: (t) => { copied.push(t); return Promise.resolve(); } } },
    isSecureContext: true,
    confirm: (msg) => { confirms.push(msg); return options.confirm !== false; },
    console: {
      log: (...a) => logs.push(a.join(" ")), info: (...a) => logs.push("info " + a.join(" ")), warn: (...a) => logs.push("warn " + a.join(" ")),
      error: (...a) => logs.push("error " + a.join(" ")),
    },
  };
  ctx.window = ctx;
  if (options.vuka) ctx.vuka = options.vuka; // what a page's script set up first
  Object.assign(ctx, options.globals || {}); // what else the browser has (getComputedStyle, matchMedia)
  ctx.listeners = {};
  ctx.addEventListener = (t, fn) => { (ctx.listeners[t] ||= []).push(fn); };
  ctx.dispatchEvent = (e) => { (ctx.listeners[e.type] || []).forEach((fn) => fn(e)); return !e.defaultPrevented; };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(file, "utf8"), ctx, { filename: file });
  // Scripts the page loads after the runtime (the component loader), each
  // dynamic import resolved from options.modules.
  ctx.__import = (url) => url in (options.modules || {}) ? Promise.resolve(options.modules[url]) : Promise.reject(new Error("no module " + url));
  for (const s of options.scripts || []) vm.runInContext(s.replace(/\bimport\(/g, "__import("), ctx);
  const $ = (sel) => doc.querySelector(sel);
  // fire dispatches an event at the element sel names, as a browser would.
  const fire = (sel, type, init = {}) => {
    const el = typeof sel === "string" ? $(sel) : sel;
    if (!el) throw new Error("no element " + sel);
    const E = "key" in init ? KeyboardEvent : Event;
    const e = new E(type, { bubbles: !["focus", "blur", "mouseenter", "mouseleave"].includes(type), ...init });
    el.dispatchEvent(e);
    return e;
  };
  // server delivers a server message to the page's socket (joined first).
  const ws = () => sockets[sockets.length - 1];
  const server = (m) => ws().onmessage({ data: JSON.stringify(m) });
  // pop goes back or forward to an entry: its address and state, then popstate.
  const pop = (url, state) => {
    at.href = new URL(url, at.href).href;
    history.state = state;
    ctx.dispatchEvent(Object.assign(new Event("popstate"), { state }));
  };
  return { doc, $, fire, tick, ws, server, pop, sockets, logs, confirms, copied, loads, entries, scrolls, history, location, window: ctx, options, fetches, reply };
}

module.exports = { page };
