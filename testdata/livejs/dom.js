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
  get textContent() { return this.childNodes.map((c) => c.textContent).join(""); }
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
      for (const l of [...(n.listeners[e.type] || [])]) if (capture === null || l.capture === capture) l.fn.call(n, e);
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
  cloneNode() { return new Text(this.ownerDocument, this.nodeValue); }
}

class Fragment extends Node {
  constructor(doc) { super(doc); this.nodeType = 11; }
  cloneNode(deep) { const f = new Fragment(this.ownerDocument); if (deep) this.childNodes.forEach((c) => f.appendChild(c.cloneNode(true))); return f; }
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
      Object.defineProperty(this, "checked", {
        get: () => checked !== null ? checked : this.hasAttribute("checked"),
        set: (v) => { checked = !!v; },
      });
    }
  }
  get id() { return this.getAttribute("id") || ""; }
  get className() { return this.getAttribute("class") || ""; }
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
  cloneNode(deep) {
    const el = this.ownerDocument.createElement(this.tagName.toLowerCase());
    this.attributes.forEach((a) => el.setAttribute(a.name, a.value));
    if (deep) {
      this.childNodes.forEach((c) => el.appendChild(c.cloneNode(true)));
      if (this.content) this.content.childNodes.forEach((c) => el.content.appendChild(c.cloneNode(true)));
    }
    return el;
  }
  focus() { this.ownerDocument.activeElement = this; this.ownerDocument.focused.push(this); }
  blur() { if (this.ownerDocument.activeElement === this) this.ownerDocument.activeElement = this.ownerDocument.body; }
  select() {}
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
  importNode(n, deep) { return n.cloneNode(deep); }
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
    if (m[1]) {
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
    for (const ch of s.trim()) {
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
  let buf = "", depth = 0, quote = "";
  for (const ch of s) {
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
  const re = /^(\*|[a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[\s*([\w-]+)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\]\s]+)))?\s*\]|:not\(((?:[^()]|\([^()]*\))*)\)/g;
  let m, at = 0;
  while ((m = re.exec(s))) {
    if (m.index !== at) throw new SyntaxError("selector " + s);
    at = re.lastIndex;
    const [, tag, id, cls, attr, v1, v2, v3, not] = m;
    if (tag) { const t = tag.toUpperCase(); if (t !== "*") tests.push((el) => el.tagName === t); }
    else if (id) tests.push((el) => el.id === id);
    else if (cls) tests.push((el) => el.classList.contains(cls));
    else if (attr) {
      const v = v1 ?? v2 ?? v3;
      tests.push((el) => v === undefined ? el.hasAttribute(attr) : el.getAttribute(attr) === v);
    } else if (not !== undefined) {
      const inner = parseSelector(not);
      tests.push((el) => !inner.some((c) => matchComplex(el, c)));
    }
  }
  if (at !== s.length) throw new SyntaxError("selector " + s);
  return (el) => tests.every((t) => t(el));
}
function matchComplex(el, parts, i = parts.length - 1) {
  if (!parts[i].c(el)) return false;
  if (i === 0) return true;
  if (parts[i].comb === ">") return !!el.parentElement && matchComplex(el.parentElement, parts, i - 1);
  for (let p = el.parentElement; p; p = p.parentElement) if (matchComplex(p, parts, i - 1)) return true;
  return false;
}

// page loads live.js into a fresh context over body: options.live gives
// the script a token (it connects), options.confirm answers window.confirm,
// options.vuka is window.vuka before the runtime loads.
function page(file, body, options = {}) {
  const doc = new Document();
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
  const script = {
    src: "http://test/_vuka/live.js",
    dataset: options.live ? { vkToken: "tok" } : {},
    hasAttribute: (n) => !!options.live && n === "data-vk-token",
  };
  Object.defineProperty(doc, "currentScript", { value: script, configurable: true });
  const ctx = {
    document: doc, Element, Node, Event, CustomEvent, KeyboardEvent, URL, FormData, WebSocket,
    setTimeout, clearTimeout, requestAnimationFrame: (fn) => setTimeout(fn, 16),
    location: { href: "http://test/", reload() {}, assign() {} },
    sessionStorage: { getItem: () => null, setItem() {} },
    navigator: { clipboard: { writeText: (t) => { copied.push(t); return Promise.resolve(); } } },
    isSecureContext: true,
    confirm: (msg) => { confirms.push(msg); return options.confirm !== false; },
    console: {
      log: (...a) => logs.push(a.join(" ")), warn: (...a) => logs.push("warn " + a.join(" ")),
      error: (...a) => logs.push("error " + a.join(" ")),
    },
  };
  ctx.window = ctx;
  if (options.vuka) ctx.vuka = options.vuka; // what a page's script set up first
  ctx.listeners = {};
  ctx.addEventListener = (t, fn) => { (ctx.listeners[t] ||= []).push(fn); };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(file, "utf8"), ctx, { filename: file });
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
  return { doc, $, fire, tick, ws, server, sockets, logs, confirms, copied, window: ctx, options };
}

module.exports = { page };
