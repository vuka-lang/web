// The Vuka live runtime: it connects a page rendered with stateful components
// to its session over a WebSocket, sends the events its elements declare
// (data-vk-on-<event>), and morphs the page into what the server sends back,
// keeping focus, selection and what the user is typing. It also runs JS
// commands (data-vk-js-<event>): changes to the page made in the browser,
// with or without a live connection. Links marked data-vk-link move a live
// page to another URL over its connection, and a page that isn't live in
// place over HTTP.
(() => {
  "use strict";
  const script = document.currentScript;
  if (!script || window.vuka && window.vuka.connect) return;
  let live = script.hasAttribute("data-vk-token"); // a page with no live parts has none, until a navigation reaches one
  const src = new URL(script.src, location.href);
  const url = (src.protocol === "https:" ? "wss://" : "ws://") + src.host + src.pathname.replace(/\.js$/, "") + "?t=";
  let token = script.dataset.vkToken || ""; // the page's: a navigation brings the next one
  let toolbar = script.dataset.vkToolbar; // development: the page's toolbar record, which the socket's messages join
  const build = script.dataset.vkBuildId || ""; // the build that rendered the page: a navigation over HTTP names it
  const root = script.dataset.vkRoot || "-"; // the page's root layout: a page under another is another document
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
    ws = new WebSocket(url + encodeURIComponent(token) + (toolbar ? "&tb=" + toolbar : ""));
    ws.onopen = join;
    ws.onmessage = (e) => receive(JSON.parse(e.data));
    ws.onclose = (e) => {
      ws = null;
      joined = false;
      if (nav) return load(nav.href, nav.pop || nav.replace); // the page it was going to, the long way
      pending.forEach((p) => clearTimeout(p.timer));
      pending.clear();
      busy.forEach((b) => release(b.el));
      busy.clear();
      [...preds.keys()].forEach((r) => settle(r, true)); // no reply comes
      replies.forEach((w) => w.reject(new Error("live: the connection closed")));
      replies.clear();
      if (e.code === 4001) return reload(e.reason);
      if (e.code === 4002) return idle();
      if (e.code === 4004 || e.code === 1000) return state("closed");
      state("disconnected");
      timer = setTimeout(connect, Math.min(10000, 250 * 2 ** attempt++) * (0.5 + Math.random()));
    };
  }

  // join asks for the page in protocol v2 (render trees); a server that
  // doesn't speak it answers in v1 (HTML).
  const join = () => ws.send(JSON.stringify({ type: "join", ref: ++ref, v: 2 }));

  // idle follows the server closing a page that sat idle (dropping its
  // session): it connects again at once when the page is in view, else
  // when it is shown again.
  let sleeping = false;
  function idle() {
    state("idle");
    if (document.visibilityState !== "hidden") return void connect();
    sleeping = true;
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
    const b = m.ref && busy.get(m.ref), n = nav && m.ref === nav.ref ? nav : null;
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
        markActive();
        emit("vk:patched", { ids: ["c0"], join: true });
        pushed(m.js);
        waiting.splice(0).forEach((f) => f());
        break;
      case "patch": {
        let ups = m.updates || [];
        if (m.trees) {
          try {
            const html = trees.apply(m.trees, false);
            ups = m.trees.map((e) => ({ id: e.id, html: html[e.id] }));
          } catch (err) { // out of step with the server: start over
            fail("live: " + err.message);
            if (n) return load(n.href, n.pop || n.replace);
            joined = false;
            join();
            break;
          }
        }
        const done = [];
        for (const u of ups) if (!done.some((el) => el && el.isConnected && el.contains(byID(u.id)))) done.push(apply(u));
        if (b && b.form && !m.error && b.form.isConnected && !b.form.hasAttribute("data-vk-form")) b.form.reset();
        if (b && b.form && b.form.isConnected && b.form.hasAttribute("data-vk-form") && !b.aborted) submitted(b.form);
        if (m.error) fail(m.error, m.ref);
        if (n) arrived(n, m);
        else markActive();
        emit("vk:patched", { ids: ups.map((u) => u.id) });
        if (!m.error) pushed(m.js);
        break;
      }
      case "error":
        if (n) return load(n.href, n.pop || n.replace);
        fail(m.error, m.ref);
        break;
      case "redirect":
        if (b && b.form) leaving = true; // a submit's: its changes are saved
        load(m.url, n && (n.pop || n.replace));
        break;
    }
    if (b) {
      busy.delete(m.ref);
      release(b.el);
    }
    if (m.ref && m.type !== "redirect") settle(m.ref, m.type === "error" || !!m.error);
    const w = m.ref && replies.get(m.ref);
    if (w) {
      replies.delete(m.ref);
      if (m.type === "error" || m.error) w.reject(new Error(m.error || "live: " + m.type));
      else w.resolve();
    }
  }

  // release ends an element's wait for a reply; one a busy command marked
  // stays busy until its mark is undone.
  // When the wait set aria-busy before the mark, the mark puts back none.
  function release(el) {
    const m = marked.get(el);
    if (!m) return el.removeAttribute("aria-busy");
    m.recs.forEach((r) => { if (r.el === el && r.name === "aria-busy") r.before = null; });
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
      head(doc);
      next = doc.body;
    } else {
      next = fragment(html);
    }
    listenIn(html);
    children(document.body, next);
    bindIn(document.body, html);
  }

  // head takes a page's title, and the elements of its head the document
  // lacks (a stylesheet, a script — run once added — a meta tag).
  function head(doc) {
    if (doc.querySelector("title") && doc.title !== document.title) document.title = doc.title;
    const have = new Set(Array.from(document.head.children, (e) => e.outerHTML));
    for (const e of Array.from(doc.head.children)) {
      if (e.tagName === "TITLE" || have.has(e.outerHTML)) continue;
      let n = document.importNode(e, true);
      if (e.tagName === "SCRIPT") {
        n = document.createElement("script");
        for (const a of e.attributes) n.setAttribute(a.name, a.value);
        n.textContent = e.textContent;
      }
      document.head.appendChild(n);
    }
  }

  const byID = (id) => id === "c0" ? document.body : document.querySelector('[data-vk-id="' + id + '"]');

  // apply morphs an instance into its new HTML and returns its element.
  function apply(u) {
    if (u.id === "c0") return page(u.html), document.body;
    const el = byID(u.id);
    const next = fragment(u.html).firstElementChild;
    listenIn(u.html);
    if (el && next) morph(el, next);
    const now = el && el.isConnected ? el : byID(u.id);
    bindIn(now, u.html);
    return now;
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
    const vf = el.tagName === "FORM" && el.hasAttribute("data-vk-form") ? el : null;
    if (vf && e.type === "submit" && !joined) return; // no connection: the form posts to the page
    prevent(el, e);
    const keys = el.getAttribute("data-vk-key");
    if (keys && e.key !== undefined && !keys.split(/\s+/).includes(e.key)) return;
    if (!joined) return; // the join's render will replace what was clicked
    const msg = { type: "event", target: el.getAttribute("data-vk-on-" + e.type), event: e.type };
    if (typeof e.key === "string") msg.key = e.key;
    const v = valueOf(el);
    if (v !== undefined) msg.value = v;
    if (vf) {
      if (!formEvent(vf, t, e, msg)) return;
      if (e.type !== "input" && e.type !== "submit") { // leaving a field, a choice: at once
        flush();
        return void send(msg);
      }
    } else if (e.type === "submit") msg.form = formOf(e.target.closest("form") || el, e.submitter);
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

  // Struct forms (<ui.Form for>, data-vk-form): the server decides what
  // shows of their checks, from what the runtime tells it — which fields the
  // visitor used (left with something in them, or changed while showing an
  // error), kept in the form's _vk_used field, and a sequence number on each
  // change, so a change arriving after a submit can't undo its reply. A
  // model.lazy field (data-vk-lazy) syncs on change, not on input. A submit
  // sent while one is in flight is dropped (data-vk-submit="prevent", the
  // default), sent with the first's reply ignored ("abort"), or sent
  // ("allow").
  let formSeq = 0;
  const filled = (f) => checkable(f) ? f.checked : f.value !== "";

  function formEvent(form, t, e, msg) {
    const f = t && isField(t) && t.form === form ? t : null;
    if (e.type === "input" && f && f.hasAttribute("data-vk-lazy")) return false;
    if (e.type === "change" && f && !checkable(f) && f.tagName !== "SELECT" && !f.hasAttribute("data-vk-lazy")) return false; // focusout sends it
    if (f && f.name && (e.type === "focusout" && filled(f) || f.getAttribute("aria-invalid") === "true" || e.type === "change" && checkable(f))) {
      const used = form.querySelector('input[name="_vk_used"]');
      if (used) {
        const names = used.value ? used.value.split(",") : [];
        if (!names.includes(f.name)) used.value = names.concat(f.name).join(",");
      }
    }
    if (e.type === "submit") {
      const policy = form.getAttribute("data-vk-submit") || "prevent";
      for (const b of busy.values()) {
        if (b.form !== form) continue;
        if (policy === "prevent") return false;
        if (policy === "abort") b.aborted = true;
      }
    }
    msg.form = formOf(form, e.type === "submit" ? e.submitter : undefined);
    msg.form._vk_seq = [String(++formSeq)];
    return true;
  }

  // submitted follows a struct form's submit reply: a failed submit moves
  // focus to the error summary (or the first field in error) and starts the
  // page's title with "Error: "; a successful one takes the prefix off.
  function submitted(form) {
    const id = form.getAttribute("data-vk-form");
    const bad = document.getElementById(id + "-summary") || form.querySelector('[aria-invalid="true"]');
    const t = document.title;
    if (bad) {
      bad.focus();
      if (!t.startsWith("Error: ")) document.title = "Error: " + t;
    } else if (t.startsWith("Error: ")) document.title = t.slice(7);
  }

  // An error summary's links move focus to the field, its label in view.
  document.addEventListener("click", (e) => {
    const a = e.target instanceof Element && e.target.closest("[data-vk-summary] a[href^='#']");
    if (!a || e.button || e.metaKey || e.ctrlKey || e.shiftKey) return;
    const el = document.getElementById(decodeURIComponent(a.getAttribute("href").slice(1)));
    if (!el) return;
    e.preventDefault();
    const label = el.id && document.querySelector('label[for="' + CSS.escape(el.id) + '"]');
    const legend = el.closest("fieldset") && el.closest("fieldset").querySelector("legend");
    (legend || label || el).scrollIntoView();
    el.focus({ preventScroll: true });
  });

  // Leaving with unsaved changes: a form with data-vk-confirm-leave has
  // changes not saved while the server says so (data-vk-dirty: its values
  // differ from the ones it was loaded with) or one of its fields shows
  // what the server didn't send it. The browser asks before the page goes
  // (beforeunload), and the runtime before a link or navigate moves it;
  // a form's own submit leaves without asking.
  let leaving = false;
  function differs(el) {
    if (checkable(el)) return el.checked !== el.hasAttribute("checked");
    if (el.tagName === "SELECT") {
      const marked = Array.from(el.options).filter((o) => o.hasAttribute("selected"));
      if (!marked.length && !el.multiple) return el.selectedIndex > 0;
      return Array.from(el.options).some((o) => o.selected !== o.hasAttribute("selected"));
    }
    return el.value !== (el.tagName === "TEXTAREA" ? el.textContent : el.getAttribute("value") || "");
  }
  function unsaved() {
    for (const f of document.querySelectorAll("form[data-vk-confirm-leave]")) {
      if (f.hasAttribute("data-vk-dirty")) return f;
      for (const el of f.elements) if (isField(el) && el.name && el.type !== "hidden" && !el.disabled && differs(el)) return f;
    }
    return null;
  }
  // confirmLeave asks the visitor whether to leave a form's changes; true
  // to go on.
  function confirmLeave() {
    const f = unsaved();
    if (!f || window.confirm(f.getAttribute("data-vk-confirm-leave"))) {
      if (f) leaving = true;
      return true;
    }
    return false;
  }
  window.addEventListener("beforeunload", (e) => {
    if (leaving || !unsaved()) return;
    e.preventDefault();
    e.returnValue = "";
  });
  window.addEventListener("pageshow", () => { leaving = false; });
  document.addEventListener("submit", (e) => { if (!e.defaultPrevented) leaving = true; }); // a form posting: the page goes on purpose

  // Saved: a form saved in the last 2s (data-vk-saved, the ms left) asks
  // the server again once the time is up, so what Saved showed goes.
  document.addEventListener("vk:patched", () => {
    document.querySelectorAll("form[data-vk-saved]").forEach((f) => {
      const ms = f.getAttribute("data-vk-saved");
      if (f.vkSaved && f.vkSaved.ms === ms) return;
      if (f.vkSaved) clearTimeout(f.vkSaved.timer);
      f.vkSaved = { ms, timer: setTimeout(() => {
        f.vkSaved = null;
        if (f.isConnected && f.hasAttribute("data-vk-saved")) f.dispatchEvent(new CustomEvent("vksaved", { bubbles: true }));
      }, +ms) };
    });
  });

  // Checks before a submit sent without a connection: the rules a control's
  // data-vk-check lists — the field's own, in the server's order, with the
  // server's messages — run in the browser, and a submit breaking one shows
  // its errors and summary as the server's 422 would, without the round
  // trip. Only what the browser decides exactly as the server is checked:
  // a value it can't read the same way (letters in a number, a character
  // not a digit where only digits go) is the server's to judge, and the
  // server judges every submit that goes.
  const goSpace = new RegExp("^[\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]+|[\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]+$", "g");
  function checkValue(form, el, name, spec) {
    const all = Array.from(form.elements).filter((x) => x.name === name && !x.disabled);
    const clean = (v) => spec.t ? v.replace(goSpace, "") : v;
    if (spec.k === "b") return all.some((x) => checkable(x) && x.checked);
    if (spec.k === "l") {
      const out = [];
      for (const x of all) {
        if (checkable(x)) { if (x.checked) out.push(clean(x.value)); }
        else if (x.tagName === "SELECT") for (const o of x.selectedOptions) out.push(clean(o.value));
      }
      return out.filter((v) => v !== "");
    }
    const choices = all.filter(checkable);
    if (choices.length) {
      const c = choices.find((x) => x.checked);
      return clean(c ? c.value : "");
    }
    return all.length ? clean(all[all.length - 1].value) : "";
  }
  const numberRe = { i: /^[+-]?\d+$/, u: /^\+?\d+$/, f: /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/ };
  // verdict is the first rule of spec the value breaks, null when it breaks
  // none, undefined when the browser can't tell.
  function verdict(form, el, name, spec) {
    const v = checkValue(form, el, name, spec);
    let size = 0, zero = false;
    switch (spec.k) {
      case "b": zero = !v; break;
      case "l": size = v.length; zero = !size; break;
      case "s":
        if (spec.d && !/^[\p{Nd} ]*$/u.test(v)) return undefined;
        size = Array.from(v).length;
        zero = v === "";
        break;
      default: {
        const t = v.replace(/,/g, "");
        if (t === "") { size = 0; zero = true; break; }
        if (!numberRe[spec.k] || !numberRe[spec.k].test(t)) return undefined;
        size = Number(t);
        if (spec.k !== "f" && !Number.isSafeInteger(size)) return undefined;
        zero = size === 0;
      }
    }
    for (const r of spec.r || []) {
      const p = r[1];
      switch (r[0]) {
        case "omitempty": if (zero) return null; break;
        case "required": if (zero) return r; break;
        case "email": {
          if (v === "") break;
          const at = v.lastIndexOf("@");
          if (at <= 0 || !v.slice(at + 1).includes(".")) return r;
          break;
        }
        case "oneof": if (!p.includes(v)) return r; break;
        case "min": if (spec.k !== "b" && size < p) return r; break;
        case "max": if (spec.k !== "b" && size > p) return r; break;
        case "len": if (spec.k !== "b" && size !== p) return r; break;
        default: return undefined;
      }
    }
    return null;
  }
  const formWords = (form) => { try { return JSON.parse(form.getAttribute("data-vk-words") || "{}"); } catch (_) { return {}; } };
  // the control a check's element stands for (a group's first) and where
  // its error goes: before the control, or the group's choices
  function checkSite(el) {
    if (isField(el)) return { ctl: el, before: el.closest(".vk-check") || el, link: el };
    const ctl = el.querySelector("input,select,textarea");
    return { ctl, before: el.querySelector(".vk-choices") || el, link: el };
  }
  function clientErrors(form) {
    form.querySelectorAll("[data-vk-client]").forEach((p) => dropClient(p));
  }
  function dropClient(p) {
    const at = p.vkAt || {};
    if (at.described) {
      const ids = (at.described.getAttribute("aria-describedby") || "").split(/\s+/).filter((id) => id && id !== p.id);
      ids.length ? at.described.setAttribute("aria-describedby", ids.join(" ")) : at.described.removeAttribute("aria-describedby");
    }
    if (at.invalid) at.invalid.removeAttribute("aria-invalid");
    if (at.group) at.group.classList.remove("vk-field--invalid");
    if (at.server) p.replaceWith(at.server);
    else p.remove();
  }
  function showClient(form, el, rule) {
    const site = checkSite(el);
    if (!site.ctl || !site.before.parentNode) return null;
    const id = site.ctl.id || "";
    const p = document.createElement("p");
    p.className = "vk-field__error";
    p.id = id + "-error";
    p.setAttribute("data-vk-error", rule[3] || "");
    p.setAttribute("data-vk-client", "");
    const sr = document.createElement("span");
    sr.className = "vk-sr-only";
    sr.textContent = formWords(form).error || "Error:";
    p.append(sr, " " + rule[2]);
    const at = p.vkAt = {};
    const old = id && document.getElementById(id + "-error");
    if (old && form.contains(old)) {
      at.server = old;
      old.replaceWith(p);
    } else {
      site.before.parentNode.insertBefore(p, site.before);
      const d = isField(el) ? site.ctl : el;
      const ids = (d.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
      if (!ids.includes(p.id)) { d.setAttribute("aria-describedby", ids.concat(p.id).join(" ")); at.described = d; }
    }
    if (isField(el) && site.ctl.getAttribute("aria-invalid") !== "true") { site.ctl.setAttribute("aria-invalid", "true"); at.invalid = site.ctl; }
    const g = el.closest(".vk-field");
    if (g && !g.classList.contains("vk-field--invalid")) { g.classList.add("vk-field--invalid"); at.group = g; }
    return { id, text: rule[2] };
  }
  function precheck(form) {
    clientErrors(form);
    const shown = [];
    for (const el of form.querySelectorAll("[data-vk-check]")) {
      let spec;
      try { spec = JSON.parse(el.getAttribute("data-vk-check")); } catch (_) { continue; }
      const name = el.getAttribute("data-vk-name") || el.name;
      const r = name && verdict(form, el, name, spec);
      if (!r) continue;
      const s = showClient(form, el, r);
      if (s) shown.push(s);
    }
    if (!shown.length) return true;
    const id = form.getAttribute("data-vk-form"), w = formWords(form);
    const old = document.getElementById(id + "-summary");
    const sum = document.createElement("div");
    sum.className = "vk-summary";
    sum.id = id + "-summary";
    sum.tabIndex = -1;
    sum.setAttribute("data-vk-summary", "");
    sum.setAttribute("data-vk-client", "");
    const alert = document.createElement("div"), h = document.createElement("h2"), ul = document.createElement("ul");
    alert.setAttribute("role", "alert");
    h.className = "vk-summary__title";
    h.textContent = w.problem || "There is a problem";
    ul.className = "vk-summary__list";
    for (const s of shown) {
      const li = document.createElement("li"), a = document.createElement("a");
      a.href = "#" + s.id;
      a.textContent = s.text;
      li.append(a);
      ul.append(li);
    }
    alert.append(h, ul);
    sum.append(alert);
    if (old && form.contains(old)) {
      sum.vkAt = { server: old };
      old.replaceWith(sum);
    } else {
      let at = form.firstChild;
      while (at && (at.nodeType !== 1 || at.tagName === "INPUT" && at.type === "hidden")) at = at.nextSibling;
      form.insertBefore(sum, at);
    }
    sum.focus();
    if (!document.title.startsWith("Error: ")) document.title = "Error: " + document.title;
    return false;
  }
  document.addEventListener("submit", (e) => {
    const form = e.target;
    if (joined || !form || form.tagName !== "FORM" || !form.hasAttribute("data-vk-form")) return;
    if (e.submitter && e.submitter.name === "_vk_intent") return;
    if (precheck(form)) return;
    e.preventDefault();
    e.stopImmediatePropagation();
  }, true);
  // a field showing a check's error drops it once it passes
  const recheck = (e) => {
    const t = e.target;
    if (joined || !(t instanceof Element) || !isField(t) || !t.form || !t.form.hasAttribute("data-vk-form")) return;
    const el = t.hasAttribute("data-vk-check") ? t : t.closest("[data-vk-check]");
    const site = el && checkSite(el);
    const err = site && site.ctl && document.getElementById(site.ctl.id + "-error");
    if (!err || !err.hasAttribute("data-vk-client")) return;
    let spec;
    try { spec = JSON.parse(el.getAttribute("data-vk-check")); } catch (_) { return; }
    if (verdict(t.form, el, el.getAttribute("data-vk-name") || el.name, spec) === null) dropClient(err);
  };
  document.addEventListener("input", recheck, true);
  document.addEventListener("change", recheck, true);

  function flush() {
    pending.forEach((p) => { clearTimeout(p.timer); send(p.msg); });
    pending.clear();
  }

  function send(msg, el, form) {
    if (!ws || !joined || nav) return; // a page on its way out sends nothing
    msg.ref = ++ref;
    const f = document.activeElement;
    sentField = f && isField(f) ? { el: f, shows: fieldShows(f) } : null;
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
  // takes the server's value when that changed since the last render; one
  // with focus only when it still shows what it showed when the page last
  // sent an event (a form cleared once sent), else it keeps what the user
  // is typing.
  function morph(el, next) {
    if (el.tagName !== next.tagName) return el.replaceWith(document.importNode(next, true));
    if (el.hasAttribute("data-vk-ignore") && el.id && el.id === next.id) return;
    // An island host owns its children (the client-mounted component). Update
    // its attributes — a changed data-vk-props drives the island's update() —
    // but never morph into its subtree.
    if (el.hasAttribute("data-vk-island")) return attrs(el, next);
    if (!isField(el)) {
      if (el.tagName === "FORM" && next.hasAttribute("data-vk-gen") && el.getAttribute("data-vk-gen") !== next.getAttribute("data-vk-gen")) {
        // the form was loaded or reset: its fields take the server's values
        const was = regen;
        regen = el;
        try {
          attrs(el, next);
          return keptText(el, next) || children(el, next);
        } finally { regen = was; }
      }
      attrs(el, next);
      return keptText(el, next) || children(el, next);
    }
    const seen = fieldState(el), before = serverValue(el), fresh = regen && regen.contains(el);
    const typing = !fresh && el === document.activeElement && !(sentField && sentField.el === el && sentField.shows === fieldShows(el));
    attrs(el, next);
    if (el.tagName === "TEXTAREA") {
      if (el.textContent !== next.textContent) el.textContent = next.textContent;
    } else {
      children(el, next);
    }
    if (typing) restore(el, seen);
    else if (fresh || serverValue(el) !== before) takeServer(el);
  }

  // regen is a form being morphed whose generation changed.
  let regen = null;

  // sentField is the focused field when the page last sent an event, and
  // what it showed.
  let sentField = null;
  const fieldShows = (el) => checkable(el) || el.tagName === "SELECT" ? JSON.stringify(fieldState(el)) : el.value;

  const keyOf = (n) => n.nodeType === 1 ? n.id || n.getAttribute("data-vk-id") || "" : "";
  const same = (a, b) => a.nodeType === b.nodeType && (a.nodeType !== 1 || a.tagName === b.tagName);

  // ahead is an unkeyed node of n's kind past cur, when cur isn't the next
  // node's: the nodes before it are gone (a branch that changed element),
  // so the nodes after — a field with focus — stay the same elements.
  function ahead(cur, n) {
    if (keyOf(cur) || (n.nextSibling && same(cur, n.nextSibling))) return null;
    for (let c = cur.nextSibling; c && !keyOf(c); c = c.nextSibling) if (!foreign.has(c) && same(c, n)) return c;
    return null;
  }

  function children(el, next) {
    const body = el === document.body;
    if (body) adoptForeign();
    const skip = (c) => { while (c && foreign.has(c)) c = c.nextSibling; return c; };
    let cur = skip(el.firstChild);
    for (const n of Array.from(next.childNodes)) {
      let match = null;
      const k = keyOf(n);
      if (k) match = Array.from(el.children).find((c) => !foreign.has(c) && keyOf(c) === k && c.tagName === n.tagName) || null;
      if (!match && cur && same(cur, n)) match = cur;
      if (!match && cur && !k && (match = ahead(cur, n))) {
        while (cur !== match) {
          const gone = cur;
          cur = cur.nextSibling;
          if (!foreign.has(gone)) el.removeChild(gone);
        }
      }
      if (match) {
        if (match !== cur) el.insertBefore(match, cur);
        if (n.nodeType === 1) morph(match, n);
        else if (match.nodeValue !== n.nodeValue) match.nodeValue = n.nodeValue;
        cur = skip(match.nextSibling);
      } else {
        el.insertBefore(document.importNode(n, true), cur);
      }
    }
    while (cur) {
      const gone = cur;
      cur = cur.nextSibling;
      if (!foreign.has(gone)) el.removeChild(gone);
    }
    if (body && bodyWatch) bodyWatch.takeRecords();
  }

  // foreign holds the body's children the page's scripts put there — a
  // component's teleported content and its anchors, a date picker's
  // calendar: a morph of the body leaves them where they are, so the
  // component that owns them can still take them out.
  const foreign = new WeakSet();
  const markForeign = (ms) => ms.forEach((r) => r.addedNodes.forEach((n) => foreign.add(n)));
  const bodyWatch = typeof MutationObserver === "function" ? new MutationObserver(markForeign) : null;
  const adoptForeign = () => bodyWatch && markForeign(bodyWatch.takeRecords());

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
    if (checkable(el)) el.indeterminate = el.hasAttribute("data-vk-mixed");
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
    if (ops.some((o) => o[0] === "push")) prevent(el, e); // else a submit goes on: a plain form post
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
    try {
      if (rec && !r) return steps(el, ops, e);
      const outer = rec;
      rec = r || [];
      try { return steps(el, ops, e); } finally { rec = outer; }
    } finally { if (dirty.size) flushStates(); }
  }

  function steps(el, ops, e) {
    for (let i = 0; i < ops.length; i++) {
      const [name, a = {}] = ops[i];
      if (dirty.size && !/^state_/.test(name)) flushStates(); // the chain's writes land before its next other step
      if (name === "call") {
        const out = call(el, a, e), rest = ops.slice(i + 1), r = rec;
        if (out === false) return false;
        if (out === stop) return refuse(el, rest, r), false;
        if (out && typeof out.then === "function") {
          out.then((v) => { if (v !== false) run(el, rest, e, r); },
            (err) => { failed(el, a.fn, err); refuse(el, rest, r); });
          return;
        }
      } else if (name === "if" || name === "else_if" || name === "else") {
        const j = name === "if" ? branch(el, ops, i) : end(ops, i);
        if (rec && ops.slice(i + 1, j).some((o) => o[0] === "push")) rec.length = 0; // a skipped push still ends a segment
        i = j;
      }
      else if (name === "debounce" || name === "throttle") {
        const rest = ops.slice(i + 1), r = rec;
        return pace(name, el, e, a.ms || 0, () => run(el, rest, e, r));
      } else if (name === "key") {
        if (!e || !(a.keys || []).includes(e.key)) return;
      } else if (name === "confirm") {
        if (!window.confirm(a.msg)) return false;
      } else if (name === "busy") {
        busyOn(el, a, ops.slice(i + 1).some((o) => o[0] === "push" && (o[1] || {}).h));
      } else if (name !== "end") {
        if (jsOps[name]) jsOps[name](el, a, e);
        else warn("unknown JS command " + name);
      }
    }
  }

  // Calling JavaScript: a method of the component's own script (the
  // component loader's), a function registered with vuka.register, else
  // one at a dotted path on window, called as fn(ctx, ...args) — never
  // eval'd.
  const fns = new Map(), stop = {};
  function call(el, a, e) {
    const path = String(a.fn || "");
    const own = window.vukaComponents && window.vukaComponents.method(el, path);
    let fn = own || fns.get(path), self = window;
    if (!fn && /^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$/.test(path)) {
      fn = window;
      for (const k of path.split(".")) { self = fn; fn = fn == null ? undefined : fn[k]; }
    }
    if (typeof fn !== "function") {
      warn("no JavaScript function " + path);
      return failed(el, path, "no such function");
    }
    const ctx = { el: a.to ? targets(el, a.to)[0] || null : el, event: e,
      root: el && el.closest("[data-vk-id]") || document, target: (to) => targets(el, to) };
    try {
      return fn.apply(own || fns.has(path) ? undefined : self, [ctx, ...(a.args || []).map((v) => arg(el, v))]);
    } catch (err) {
      return failed(el, path, err);
    }
  }
  function failed(el, fn, err) {
    const error = String(err && err.message || err);
    console.error("vuka:", fn + ": " + error);
    (el && el.isConnected ? el : document).dispatchEvent(new CustomEvent("vk:error", { bubbles: true, detail: { error, fn } }));
    return stop;
  }
  // arg resolves a read — {"$read": a condition} — to what the browser
  // holds now: a value, checked state or attribute, else whether it holds.
  function arg(el, v) {
    if (!v || typeof v !== "object" || Object.keys(v).length !== 1 || !v.$read) return v;
    const c = v.$read;
    if (c.and || c.or || c.not) return holds(el, c);
    const t = targets(el, c.to)[0];
    if ("exists" in c) return !!t === !!c.exists;
    if (!t) return null;
    if ("value_eq" in c || "is" in c) return holds(el, c);
    if ("checked" in c) return !!t.checked;
    if ("attr" in c) return t.getAttribute(c.attr);
    return read(t);
  }
  // refuse ends a chain a call stopped: the next push's predictions (what
  // the chain changed since its last push) are undone and the push fails
  // without being sent.
  function refuse(el, rest, r) {
    const p = rest.find((o) => o[0] === "push");
    if (p) settle(track(el, p[1] || {}, r ? r.splice(0) : [], 0, r && r.busy && r.busy.splice(0)), true);
  }

  // Optimistic updates: a sent push keeps what its chain changed before it
  // (its predictions) until its reply. A reply with an error, no reply in
  // time, or a lost connection undoes them (but not what the server has
  // changed since) and runs on_error; a reply without one runs on_ok.
  let local = 0;
  function track(el, a, recs, sent, marks) {
    const id = sent || "local" + ++local;
    const root = el && el.closest("[data-vk-id]");
    const p = { el, a, recs, marks: marks || [], root: root && root.getAttribute("data-vk-id") };
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
    p.marks.forEach(unbusy); // usable again before its hooks run
    const hooks = error ? p.a.on_error : p.a.on_ok;
    if (hooks) run(el && el.isConnected ? el : p.root && byID(p.root) || document.body, hooks, null, []);
  }
  // undo puts back what a prediction changed where the server's value is
  // still the one recorded and the element still shows what it set.
  const fieldValue = (el) => checkable(el) ? el.checked : el.value;
  function undo(r) {
    const el = r.el, t = running.get(el);
    if (t) t.finish();
    if (r.field) {
      if (serverValue(el) !== r.server || fieldValue(el) !== r.after) return;
      if (checkable(el)) el.checked = r.before;
      else el.value = r.before;
      return;
    }
    const name = r.cls ? "class" : r.name, m = touched.get(el), k = m && m.get(name);
    if (!k || k.server !== r.server || (r.cls ? el.classList.contains(r.cls) : val(el, name)) !== r.after) return;
    if (r.cls) el.classList[r.before ? "add" : "remove"](r.cls);
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
  // el's instance, not the instances inside it), by attribute (the same, or
  // the page's), selector, closest ancestor, or selector inside el. A ref,
  // sel or attribute of several parts finds what any part does, in page
  // order, each once.
  function targets(el, to) {
    if (!to) return el ? [el] : [];
    try {
      if (to.ref !== undefined) {
        const names = [].concat(to.ref).map(String);
        return scoped(el, "[data-vk-ref]", false).filter((x) => names.includes(x.getAttribute("data-vk-ref")));
      }
      if (to.attr) return scoped(el, attrSel(to.attr), !!to.attr.page);
      if (to.sel) return Array.from(document.querySelectorAll([].concat(to.sel).join(", ")));
      if (to.closest) { const c = el && el.closest(to.closest); return c ? [c] : []; }
      if (to.inner) return el ? Array.from(el.querySelectorAll(to.inner)) : [];
    } catch (err) { warn(err.message); }
    return [];
  }
  // scoped is the elements matching sel in el's instance (or, with page or
  // on a static page, the document), the instance's root among them.
  function scoped(el, sel, page) {
    const scope = !page && el && el.closest("[data-vk-id]");
    const all = Array.from((scope || document).querySelectorAll(sel));
    if (!scope) return all;
    if (scope.matches(sel)) all.unshift(scope);
    return all.filter((x) => x === scope || x.parentElement.closest("[data-vk-id]") === scope);
  }
  // attrSel is an attr target's selector, every name and value escaped: a
  // value is data, never selector syntax.
  const esc = (s) => window.CSS && CSS.escape ? CSS.escape(String(s)) : cssEscape(String(s));
  const matchOps = { prefix: "^=", suffix: "$=", contains: "*=", word: "~=" };
  function attrSel(a) {
    const name = esc(a.name), is = a.is || [];
    if (!is.length) return "[" + name + "]";
    return is.map((v) => {
      const [k, s] = v && typeof v === "object" ? Object.entries(v)[0] || [] : ["", v];
      if (k && (!matchOps[k] || !s)) return null; // an unknown or empty matcher matches nothing
      return "[" + name + (k ? matchOps[k] : "=") + '"' + esc(s) + '"]';
    }).filter(Boolean).join(", ") || ":not(*)";
  }
  // cssEscape is CSS.escape where a browser lacks it (CSSOM's algorithm).
  function cssEscape(s) {
    let out = "";
    for (let i = 0; i < s.length; i++) {
      const c = s.charCodeAt(i);
      if (c === 0) out += "\uFFFD";
      else if (c <= 0x1f || c === 0x7f || (c >= 0x30 && c <= 0x39 && (i === 0 || i === 1 && s[0] === "-"))) out += "\\" + c.toString(16) + " ";
      else if (i === 0 && s === "-") out += "\\-";
      else if (c >= 0x80 || c === 0x2d || c === 0x5f || c >= 0x30 && c <= 0x39 || c >= 0x41 && c <= 0x5a || c >= 0x61 && c <= 0x7a) out += s[i];
      else out += "\\" + s[i];
    }
    return out;
  }

  // holds tests a condition: and, or and not by their parts; a read by
  // every element its target names, or with some by at least one. A read
  // of no element fails, except exists.
  const read = (t) => isField(t) ? t.value : t.getAttribute("value") || "";
  function holds(el, c) {
    if (c.and) return c.and.every((k) => holds(el, k || {}));
    if (c.or) return c.or.some((k) => holds(el, k || {}));
    if (c.not) return !holds(el, c.not);
    const ts = targets(el, c.to);
    if ("exists" in c) return !!ts.length === !!c.exists;
    if (!ts.length) return false;
    return c.some ? ts.some((t) => passes(t, c)) : ts.every((t) => passes(t, c));
  }
  function passes(t, c) {
    if ("checked" in c) return t.tagName === "INPUT" && !!t.checked === !!c.checked;
    if ("value_eq" in c) return read(t) === String(c.value_eq);
    if ("value" in c) return !!read(t) === !!c.value;
    if ("is" in c) return t.matches(c.is);
    if ("attr" in c) return t.hasAttribute(c.attr);
    return true;
  }

  // Busy: an element marked busy (aria-busy) while what its event started
  // is under way; its fields readonly (still submitted), its buttons and
  // links aria-disabled and out of the tab order, and clicks and submits on
  // it ignored. Each change is recorded as a command's is, and put back
  // when it is undone: by the reply to a push of the chain (before its
  // hooks), its timeout, or the page coming back from the back/forward
  // cache.
  const marked = new Map(); // el → its mark: {recs, timer}
  const pressable = "button, a[href], input[type=submit], input[type=button], input[type=reset], input[type=image]";
  function busyOn(el, a, later) {
    const r = rec;
    for (const t of targets(el, a.to)) {
      if (marked.has(t)) continue;
      const m = { recs: [], fresh: true }; // fresh: the submit a click marking it goes on to is let through
      marked.set(t, m);
      setTimeout(() => { m.fresh = false; }, 0);
      rec = m.recs; // its changes are its own, no push's predictions
      try {
        const set = (x, name, v) => { if (x.getAttribute(name) !== v && !(name === "readonly" && x.hasAttribute(name))) touch(x, name, () => x.setAttribute(name, v)); };
        touch(t, "aria-busy", () => t.setAttribute("aria-busy", "true")); // recorded even when set: a reply's wait may have set it
        for (const x of [t, ...t.querySelectorAll("input, textarea, select, button, a[href]")]) {
          if (x.matches(pressable)) { set(x, "aria-disabled", "true"); set(x, "tabindex", "-1"); }
          else if (isField(x)) set(x, "readonly", "");
        }
      } finally { rec = r; }
      if (a.timeout > 0) m.timer = setTimeout(() => unbusy(m), a.timeout);
      if (!r) continue;
      const p = !later && r.sent && preds.get(r.sent);
      if (p) p.marks.push(m); // a push before it, still waiting
      else (r.busy ||= []).push(m); // the next push's
    }
  }
  function unbusy(m) {
    for (const [t, x] of marked) if (x === m) marked.delete(t);
    clearTimeout(m.timer);
    for (let i = m.recs.length; i--;) undo(m.recs[i]);
  }
  const unbusyAll = () => [...new Set(marked.values())].forEach(unbusy);
  const busyAt = (t, submit) => {
    for (; t; t = t.parentElement) if (marked.has(t) && !(submit && marked.get(t).fresh)) return true;
    return false;
  };
  for (const type of ["click", "submit"]) {
    document.addEventListener(type, (e) => {
      const t = e.target instanceof Element ? e.target : e.target && e.target.parentElement;
      if (!busyAt(t, type === "submit")) return;
      e.preventDefault();
      e.stopImmediatePropagation ? e.stopImmediatePropagation() : e.stopPropagation();
    }, true);
  }
  window.addEventListener("pageshow", (e) => { if (e.persisted) unbusyAll(); });

  // Reconciliation: per element, each attribute (or its text, TEXT) a
  // command changed, with the server's value then and the command's. A
  // morph that renders it as before puts the command's value back; one that
  // renders it differently wins, and it is the server's again. A running
  // chain also notes the value before each change (rec), for a push to undo.
  const touched = new WeakMap(), TEXT = "#text";
  const val = (el, name) => name === TEXT ? el.textContent : el.getAttribute(name);
  // A prediction's record: the value before, after, and the server's then.
  function touch(el, name, change, cls) {
    let m = touched.get(el);
    if (!m) touched.set(el, (m = new Map()));
    if (!m.has(name)) m.set(name, { server: val(el, name) });
    const server = m.get(name).server, has = (c) => el.classList.contains(c);
    const recs = !rec ? [] : cls ? cls.map((c) => ({ el, cls: c, before: has(c), server }))
      : [rec.find((r) => r.el === el && r.name === name) || { el, name, before: val(el, name), server }];
    recs.forEach((r) => { if (!rec.includes(r)) rec.push(r); });
    change();
    m.get(name).value = val(el, name);
    recs.forEach((r) => { r.after = r.cls ? has(r.cls) : val(el, name); });
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
    const r = !on && rec && { el, name: "style", before: val(el, "style") }; // hidden at the end
    if (r) rec.push(r);
    animate(el, a.transition, a.time, on ? () => show(el, a) : null, on ? null : () => {
      hide(el);
      if (r) Object.assign(r, { after: val(el, "style"), server: touched.get(el).get("style").server });
    });
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

  // Client state (ui.State): values the browser keeps, by key, each set
  // up from the initial value its references carry ("i"). The page's parts
  // bound to them — an attribute (data-vk-b-<name>), a <vk-t>'s text
  // (data-vk-b), a <vk-show>'s children (data-vk-show) — hold expressions of
  // states, evaluated again when a state they read changes (a struct
  // state's field changes only what reads it). A chain's writes are batched:
  // each binding they reach updates once, before the chain's next other
  // step and at its end. A morph renders the initial values back; the
  // bindings then put the browser's back.
  const store = new Map(), subs = new Map(), dirty = new Set(), bound = new WeakMap();
  const sinit = (r) => { if (!store.has(r.s)) store.set(r.s, r.i === undefined ? null : r.i); return r; };
  const at = (v, p) => { for (const k of p || []) v = v == null ? undefined : v[k]; return v; };
  const sread = (r) => at(store.get(sinit(r).s), r.p);
  const equal = (a, b) => a === b || a !== null && b !== null && typeof a === "object" && typeof b === "object" && JSON.stringify(a) === JSON.stringify(b);
  function swrite(r, v) {
    if (!r.s || equal(sread(r), v)) return;
    const p = r.p || [], put = (o, i) => i === p.length ? v : { ...(o && typeof o === "object" ? o : {}), [p[i]]: put(o && o[p[i]], i + 1) };
    store.set(r.s, put(store.get(r.s), 0));
    dirty.add([r.s, ...p].join("\0"));
  }
  // Paths reach each other when one starts the other: a struct's write
  // reaches its fields' readers, a field's write the struct's.
  const reach = (a, b) => a.every((k, i) => i >= b.length || k === b[i]);
  function flushStates() {
    const ds = [...dirty].map((d) => d.split("\0")), todo = new Set();
    dirty.clear();
    for (const d of ds) (subs.get(d[0]) || []).forEach((b) => { if (b.deps.some((p) => p[0] === d[0] && reach(p, d))) todo.add(b); });
    todo.forEach((b) => b.el.isConnected ? put(b) : unsub(b));
  }
  function ev(x) {
    if (x === null || typeof x !== "object") return x;
    if (!Array.isArray(x)) return "s" in x ? sread(x) : x.v;
    const [op, a, b] = x;
    switch (op) {
      case "!": return !ev(a);
      case "neg": return -ev(a);
      case "&&": return ev(a) && ev(b);
      case "||": return ev(a) || ev(b);
      case "==": return equal(ev(a), ev(b));
      case "!=": return !equal(ev(a), ev(b));
      case "<": return ev(a) < ev(b);
      case "<=": return ev(a) <= ev(b);
      case ">": return ev(a) > ev(b);
      case ">=": return ev(a) >= ev(b);
      case "+": return ev(a) + ev(b);
      case "-": return ev(a) - ev(b);
      case "len": return size(ev(a));
      case "class": { const out = {}; for (const k in a) out[k] = !!ev(a[k]); return out; }
    }
    warn("unknown state expression " + op);
  }
  // size is Go's len: a string's UTF-8 bytes, a list's items, a map's keys.
  function size(v) {
    if (typeof v === "string") { let n = 0; for (const ch of v) { const c = ch.codePointAt(0); n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4; } return n; }
    return v == null ? 0 : Array.isArray(v) ? v.length : typeof v === "object" ? Object.keys(v).length : 0;
  }
  // deps are the state paths an expression reads, each state set up.
  function deps(x, out = []) {
    if (Array.isArray(x)) x.slice(1).forEach((y) => deps(y, out));
    else if (x && typeof x === "object") {
      if ("s" in x) out.push([sinit(x).s, ...(x.p || [])]);
      else if (!("v" in x)) for (const k in x) deps(x[k], out);
    }
    return out;
  }
  const unsub = (b) => b.deps.forEach((d) => { const s = subs.get(d[0]); if (s) s.delete(b); });
  // bindIn binds every element in root, whose HTML is html, that has
  // bindings, and unbinds those that lost theirs.
  function bindIn(root, html) {
    if (root) for (const el of root.querySelectorAll("input[data-vk-mixed]")) el.indeterminate = true;
    if (!root || !subs.size && !/ data-vk-(b|show)/.test(html)) return;
    for (const el of [root, ...root.querySelectorAll("*")]) if (bound.has(el) || Array.from(el.attributes).some((a) => /^data-vk-(b|show)(-|$)/.test(a.name))) bind(el);
  }
  function bind(el) {
    const old = bound.get(el) || [], list = [];
    for (const a of Array.from(el.attributes)) {
      const m = /^data-vk-(b|show)(?:-(.+))?$/.exec(a.name);
      if (!m) continue;
      let x;
      try { x = JSON.parse(a.value); } catch (_) { warn("bad binding " + a.name); continue; }
      const kind = m[1] === "show" ? "show" : m[2] ? "attr" : "text", name = m[2];
      const b = old.find((o) => o.kind === kind && o.name === name) || { el, kind, name };
      b.x = x;
      list.push(b);
    }
    old.forEach(unsub);
    list.forEach((b) => { b.deps = deps(b.x); b.deps.forEach((d) => { if (!subs.has(d[0])) subs.set(d[0], new Set()); subs.get(d[0]).add(b); }); });
    list.length ? bound.set(el, list) : bound.delete(el);
    list.forEach((b) => put(b, true));
  }
  // put shows a binding's value; again (after a morph or at first) puts
  // it back whatever the element shows, and a <vk-show> without a transition.
  function put(b, again) {
    const v = ev(b.x), el = b.el;
    if (!again && equal(v, b.last)) return;
    b.last = v;
    if (b.kind === "fn") return b.fn(v);
    if (b.kind === "show") return showSet(b, !!v, again);
    if (b.kind === "text") {
      const t = v == null ? "" : String(v);
      if (el.textContent !== t) el.textContent = t;
      return;
    }
    const name = b.name;
    if (name === "class" && v && typeof v === "object") {
      for (const k in v) words(k).forEach((c) => el.classList[v[k] ? "add" : "remove"](c));
      return;
    }
    const s = v === true ? (/^aria-/.test(name) ? "true" : "") : v === false ? (/^aria-/.test(name) ? "false" : null) : v == null ? null : String(v);
    if (!attrOK(name, s)) return;
    if (s === null) el.removeAttribute(name);
    else if (el.getAttribute(name) !== s) el.setAttribute(name, s);
    if (name === "value" && isField(el) && el.value !== (s || "")) el.value = s || "";
    if (name === "checked" && checkable(el)) el.checked = s !== null;
  }

  // ui.Show: a <vk-show> (display: contents) around elements shown while
  // its expression holds. Its children carry the phases for CSS: data-closed
  // while hidden, data-enter appearing, data-leave disappearing; the
  // <vk-show> is hidden once their transitions or animations end (or their
  // longest one's time passes) — at once, in the same batch, when they have
  // none to wait for. A child with the popover attribute is shown with
  // showPopover instead, in the top layer; when the browser closes it (Escape,
  // a click outside) the state shown is written back: false, or true for a
  // negated one (a derived expression stays as it is). Reduced motion skips
  // the wait.
  const reduced = () => typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
  function showSet(b, on, now) {
    const w = b.el, kids = Array.from(w.children), pops = kids.filter((k) => k.hasAttribute("popover"));
    const mark = (n, v) => kids.forEach((k) => v ? k.setAttribute(n, "") : k.removeAttribute(n));
    const wrap = (shown) => {
      const rest = (w.getAttribute("style") || "").replace(displayRe, "$1").replace(/^\s*;|;\s*$/g, "").trim();
      if (shown) { w.removeAttribute("hidden"); w.setAttribute("style", (rest ? rest + "; " : "") + "display: contents"); }
      else { w.setAttribute("hidden", ""); rest ? w.setAttribute("style", rest) : w.removeAttribute("style"); }
    };
    const pop = (shown) => pops.forEach((p) => { try { shown ? p.showPopover() : p.hidePopover(); } catch (_) {} });
    const visible = (shown) => { if (pops.length) { wrap(true); pop(shown); } else wrap(shown); };
    if (b.cancel) b.cancel();
    const phase = b.phase = {};
    if (now || reduced() || !on && pops.length && !pops.some(popOpen)) {
      mark("data-enter", false);
      mark("data-leave", false);
      mark("data-closed", !on);
      return visible(on);
    }
    if (on) {
      mark("data-leave", false);
      mark("data-closed", true);
      mark("data-enter", true);
      visible(true);
      frame(() => { if (b.phase === phase) { mark("data-closed", false); ended(b, kids, () => mark("data-enter", false)); } });
    } else {
      mark("data-enter", false);
      mark("data-leave", true);
      if (!kids.some((k) => longest(k)[1] > 0)) {
        mark("data-leave", false);
        mark("data-closed", true);
        return visible(false);
      }
      frame(() => { if (b.phase === phase) { mark("data-closed", true); ended(b, kids, () => { mark("data-leave", false); visible(false); }); } });
    }
  }
  const popOpen = (p) => { try { return p.matches(":popover-open"); } catch (_) { return false; } };
  // A popover the browser closed: the state its <vk-show> shows is written
  // back. A click that closed it by landing on its trigger (the same pointer
  // gesture) then doesn't write that state again, whichever comes first: the
  // close or the click.
  let gesture = 0, downAt = -Infinity;
  const dismissed = new Map(); // state key → the gesture that closed its popover
  document.addEventListener("pointerdown", () => { gesture++; downAt = Date.now(); dismissed.clear(); }, true);
  document.addEventListener("keydown", () => dismissed.clear(), true);
  const redismiss = (a, e) => e && e.type === "click" && a.s != null && dismissed.get(a.s) === gesture;
  document.addEventListener("toggle", (e) => {
    const p = e.target;
    if (e.newState !== "closed" || !p || !p.hasAttribute || !p.hasAttribute("popover") || !p.parentElement || popOpen(p)) return;
    const b = (bound.get(p.parentElement) || []).find((o) => o.kind === "show");
    if (!b || !b.last) return;
    const x = b.x, neg = Array.isArray(x) && x[0] === "!" && x.length === 2, r = neg ? x[1] : x;
    if (!r || typeof r !== "object" || Array.isArray(r) || !("s" in r)) return; // derived: not written
    swrite(r, neg);
    if (Date.now() - downAt < 1000) dismissed.set(r.s, gesture); // closed by a pointer, not a key
    flushStates();
  }, true);

  // ended calls done once each element's transitions and animations have
  // ended (or their time has passed), unless the binding moves on first.
  const ms = (list) => String(list || "0s").split(",").map((t) => parseFloat(t) * (/ms\s*$/.test(t) ? 1 : 1000) || 0);
  function longest(el) {
    if (typeof getComputedStyle !== "function") return [0, 0];
    const cs = getComputedStyle(el), max = (d, l) => Math.max(0, ...ms(d).map((x, i, a) => x + ms(l)[i % ms(l).length] || 0));
    const dur = (d) => Math.max(0, ...ms(d));
    return [Math.max(dur(cs.transitionDuration), dur(cs.animationDuration)), Math.max(max(cs.transitionDuration, cs.transitionDelay), max(cs.animationDuration, cs.animationDelay))];
  }
  function ended(b, els, done) {
    const ls = new Map(els.map((el) => [el, longest(el)]).filter(([, t]) => t[1] > 0));
    if (!ls.size) return done();
    let timer = 0;
    const off = () => { clearTimeout(timer); els.forEach((el) => { el.removeEventListener("transitionend", on); el.removeEventListener("animationend", on); }); b.cancel = null; };
    function on(e) {
      const el = e.currentTarget || e.target, t = ls.get(el);
      if (e.target !== el || !t || (e.elapsedTime || 0) * 1000 + 1 < t[0]) return;
      ls.delete(el);
      if (!ls.size) { off(); done(); }
    }
    els.forEach((el) => { el.addEventListener("transitionend", on); el.addEventListener("animationend", on); });
    timer = setTimeout(() => { off(); done(); }, Math.max(...[...ls.values()].map((t) => t[1])) + 50);
    b.cancel = off;
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
  // The attribute commands change no event handler attribute (on…) or
  // srcdoc, and set a URL attribute to no javascript:, data: or vbscript:
  // URL: what would run script in the page. The server's commands are
  // checked when built; these come from anywhere (an attribute's, Exec).
  const urlAttrs = new Set(["href", "src", "action", "formaction", "xlink:href", "poster", "data"]);
  const scriptURL = (v) => /^(javascript|data|vbscript):/i.test(String(v).replace(/[\u0000-\u0020\u007f]/g, ""));
  function attrOK(name, ...values) {
    const n = String(name).toLowerCase();
    if (n.startsWith("on") || n === "srcdoc" || urlAttrs.has(n) && values.some((v) => v != null && scriptURL(v))) {
      warn("refused to change attribute " + name);
      return false;
    }
    return true;
  }
  const jsOps = {
    add_class: classes("add"),
    remove_class: classes("remove"),
    toggle_class: classes("toggle"),
    set_attr: each((t, a) => attrOK(a.name, a.value) && touch(t, a.name, () => t.setAttribute(a.name, a.value))),
    remove_attr: each((t, a) => attrOK(a.name) && touch(t, a.name, () => t.removeAttribute(a.name))),
    toggle_attr: each((t, a) => attrOK(a.name, ...(a.values || [])) && touch(t, a.name, () => {
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
      const r = rec && { el: t, field: true, before: fieldValue(t), server: serverValue(t) };
      if (r) rec.push(r);
      if (t.type === "checkbox" || t.type === "radio") t.checked = a.value === "true";
      else t.value = a.value;
      if (r) r.after = fieldValue(t);
      t.dispatchEvent(new Event("input", { bubbles: true }));
      t.dispatchEvent(new Event("change", { bubbles: true }));
    }),
    state_set: (el, a, e) => { if (!redismiss(a, e)) swrite(a, a.v); },
    state_toggle: (el, a, e) => { if (!redismiss(a, e)) swrite(a, !sread(a)); },
    state_inc: (el, a) => swrite(a, (+sread(a) || 0) + (a.by || 0)),
    patch: (el, a) => { move(String(a.href), true, null, !!a.replace); },
    navigate: (el, a) => { move(String(a.href), false); },
    // push sends an event to a handler as data-vk-on-<event> would.
    // Its predictions are what the chain changed since its last push.
    push: (el, a, e) => {
      const recs = rec ? rec.splice(0) : [];
      if (!a.h) return; // rendered without a session
      if (!live) return warn("a push command needs a live page");
      if (!joined) return settle(track(el, a, recs, 0, rec && rec.busy && rec.busy.splice(0)), true); // not sent: it fails at once
      const type = a.event || (e ? e.type : "push");
      const msg = { type: "event", target: a.h, event: type };
      if (e && typeof e.key === "string") msg.key = e.key;
      const v = "value" in a ? a.value : el && valueOf(el);
      if (v !== undefined && v !== null) msg.value = typeof v === "string" ? v : JSON.stringify(v);
      const form = type === "submit" ? (e && e.target.closest("form")) || (el && el.closest("form")) : null;
      if (form) msg.form = formOf(form, e && e.submitter);
      flush();
      const marks = rec && rec.busy ? rec.busy.splice(0) : [];
      const id = send(msg, el, form);
      if (id) {
        track(el, a, recs, id, marks);
        if (rec) rec.sent = id;
      } else marks.forEach(unbusy);
    },
  };

  // Navigation: a link marked data-vk-link, a patch or navigate command, and
  // the back and forward buttons move a live page over its connection; the
  // reply is applied as any patch, then the address, scroll and focus
  // follow. What can't go that way — no connection, another origin, a URL
  // that isn't a page, a failure — is an ordinary page load. A patch stays
  // on the page (its history entries share the page's id); a navigate
  // makes a new one. Only http and https URLs are followed or loaded,
  // whoever names them (a link, a command, a redirect from the server): a
  // javascript:, data: or other URL is refused, as it would run in the
  // page or leave the browser.
  let nav = null, here = location.pathname + location.search, saving = 0;
  const newPage = () => Math.random().toString(36).slice(2);
  let pageID = newPage();
  // httpURL is href resolved against the page, when it is http or https.
  function httpURL(href) {
    let u;
    try { u = new URL(String(href), location.href); } catch (_) { return null; }
    return u.protocol === "http:" || u.protocol === "https:" ? u : null;
  }
  function load(href, replace) {
    const u = httpURL(href);
    if (!u) return fail("live: refused to load a URL that isn't http or https");
    replace ? location.replace(u.href) : location.assign(u.href);
  }
  const save = () => history.replaceState({ ...history.state, x: scrollX, y: scrollY }, "");

  // go moves the page to href; pop is the history state back or forward
  // brought, the address being href already; replace takes the place of
  // the current entry instead of adding one. It returns a promise settled
  // when the move is applied: rejected for a refused URL or when another
  // move takes its place; a page load leaves it unsettled.
  function go(href, patch, pop, replace) {
    const u = httpURL(href);
    if (!u) {
      const msg = "live: refused to load a URL that isn't http or https";
      fail(msg);
      return Promise.reject(new Error(msg));
    }
    const at = u.pathname + u.search, again = !!(pop || replace);
    if (!pop && !patch && at !== here && !leaving && !confirmLeave()) {
      return Promise.reject(Object.assign(new Error("live: the visitor stayed, the form's changes not saved"), { name: "AbortError" }));
    }
    if (!pop && at === here && u.hash) {
      replace ? location.replace(u.href) : location.hash = u.hash;
      return Promise.resolve({ url: u.href, patch: true });
    }
    if (u.origin !== location.origin || (live ? !joined : !build)) {
      load(u.href, again);
      return new Promise(() => {});
    }
    manual();
    if (!live) return fetchGo(u, at, patch, pop, replace);
    if (!pop) save();
    flush();
    supersede();
    return new Promise((resolve, reject) => {
      nav = { ref: ++ref, href: u.href, patch, pop, replace, resolve, reject, timer: setTimeout(() => load(u.href, again), 10000) };
      document.documentElement.setAttribute("data-vk-navigating", at); // where to, for the development toolbar
      ws.send(JSON.stringify({ type: "navigate", ref: nav.ref, url: at, patch: patch || undefined }));
    });
  }

  // Over HTTP: a page that isn't live fetches the URL with Vk-Navigate (its
  // build) and patches the body in place with the reply, the route having
  // run as for a page load (nav.go). Its head is merged, scroll and focus
  // follow as over a connection, and a live page reached so joins, the
  // session its render parked adopted. What can't be patched loads: a
  // reply naming another build or root layout, one that isn't a page (a
  // redirect elsewhere, a file, JSON), a page whose body runs scripts of
  // its own, a route asking for it (web.FullLoad), no reply in 10s, a
  // network failure. Another move aborts the one in flight.
  function fetched(href, ahead) {
    const ctl = typeof AbortController === "function" ? new AbortController() : null;
    const headers = { "Vk-Navigate": build };
    if (ahead) headers.Purpose = "prefetch"; // the server parks a live page's session as long as the runtime keeps the reply
    const p = fetch(href, { headers, credentials: "same-origin", signal: ctl ? ctl.signal : undefined })
      .then((res) => res.text().then((html) => ({ res, html })));
    p.abort = () => { if (ctl) ctl.abort(); };
    return p;
  }

  function fetchGo(u, at, patch, pop, replace) {
    const again = !!(pop || replace);
    if (!pop) save();
    supersede();
    const key = u.href.replace(/#.*$/, "");
    return new Promise((resolve, reject) => {
      const n = nav = { href: u.href, patch, pop, replace, resolve, reject };
      const fail = (to) => {
        if (nav !== n) return;
        clearTimeout(n.timer);
        nav = null;
        load(to || u.href, again);
      };
      n.timer = setTimeout(() => { if (n.abort) n.abort(); fail(); }, 10000);
      document.documentElement.setAttribute("data-vk-navigating", at); // where to, for the development toolbar
      let req = fetchedAhead(key);
      if (!req) {
        req = fetched(key, false);
        n.abort = req.abort;
      }
      req.then((r) => { if (nav === n) landed(n, r, fail); }, () => fail());
    });
  }

  // landed applies a navigation's reply over HTTP, or loads the URL.
  function landed(n, r, fail) {
    const res = r.res, h = (k) => res.headers.get(k) || "";
    const final = res.url ? httpURL(res.url) : null;
    const moved = res.redirected && final ? final.href : null; // a redirect the fetch followed
    if (final && final.origin !== location.origin) return fail();
    if (h("Vk-Reload") || h("Vk-Page") !== root || !/^text\/html/i.test(h("Content-Type"))) return fail(moved);
    if (moved) n.href = moved;
    toolbar = h("X-Vuka-Toolbar") || toolbar;
    try { page(r.html); } catch (err) { console.error("vuka: live: " + err.message); return fail(n.href); }
    arrived(n, {});
    emit("vk:patched", { ids: ["c0"] });
    const tok = h("Vk-Live");
    if (tok) { // a live page: its session waits, parked, for this join
      token = tok;
      live = true;
      connect();
    }
  }

  // supersede ends the navigation in flight: another takes its place.
  function supersede() {
    if (!nav) return;
    clearTimeout(nav.timer);
    if (nav.abort) nav.abort();
    nav.reject(Object.assign(new Error("live: another navigation took its place"), { name: "AbortError" }));
    nav = null;
  }

  // manual has the runtime restore scroll on back and forward, not the
  // browser, from the moment it moves the page in place.
  let restoring = false; // the runtime restores scroll, not the browser
  function manual() {
    restoring = true;
    try { if ("scrollRestoration" in history) history.scrollRestoration = "manual"; } catch (_) {}
  }
  window.addEventListener("pagehide", () => { try { if (restoring) history.scrollRestoration = "auto"; } catch (_) {} }); // a document loaded on the entry later
  window.addEventListener("pageshow", (e) => { if (e.persisted && restoring) manual(); });

  // move is go for the runtime's own callers, who don't wait for it.
  const move = (href, patch, pop, replace) => { go(href, patch, pop, replace).catch(() => {}); };
  // navigate is window.vuka.navigate: go for a script or an island,
  // opts {patch, replace}.
  const navigate = (href, opts) => go(href, !!(opts && opts.patch), null, !!(opts && opts.replace));

  // arrived follows a navigation's applied reply: the next token, the
  // address, scroll (the top, the #hash, or where the entry was) and focus
  // (a new page's [autofocus], main, or live root). A new page drops the
  // optimistic pushes of the old one still waiting, their hooks unrun.
  function arrived(n, m) {
    nav = null;
    clearTimeout(n.timer);
    document.documentElement.removeAttribute("data-vk-navigating");
    if (m.token) token = m.token;
    if (typeof m.title === "string" && m.title) document.title = m.title;
    const u = new URL(n.href);
    here = u.pathname + u.search;
    if (!n.patch) {
      preds.forEach((p) => clearTimeout(p.timer));
      preds.clear();
      unbusyAll();
      pageID = n.pop && n.pop.vk || newPage();
    }
    if (n.pop) history.replaceState({ ...history.state, vk: pageID }, "");
    else if (n.replace) history.replaceState({ vk: pageID }, "", u.href);
    else history.pushState({ vk: pageID }, "", u.href);
    markActive();
    const t = u.hash && document.getElementById(decodeURIComponent(u.hash.slice(1)));
    if (n.pop && n.pop.y != null) scrollTo(n.pop.x || 0, n.pop.y);
    else if (t) t.scrollIntoView();
    else if (!n.patch) {
      scrollTo(0, 0);
      const main = document.querySelector("main"); // the page's area, when it scrolls in a layout
      if (main && main.scrollTop) main.scrollTop = 0;
    }
    if (!n.patch) {
      const f = document.querySelector("[autofocus]") || document.querySelector("main") || document.body.querySelector("[data-vk-id]") || document.body;
      if (!f.matches(focusable) && !f.hasAttribute("tabindex")) touch(f, "tabindex", () => f.setAttribute("tabindex", "-1"));
      f.focus({ preventScroll: true });
    }
    window.dispatchEvent(new CustomEvent("vk:navigate", { detail: { url: u.href, patch: !!n.patch } }));
    n.resolve({ url: u.href, patch: !!n.patch });
  }

  // push sends an event to a method of el's live component by name (a
  // component script's ctx.push): one payload at most, a value or an
  // object of form fields. Sent once the page is joined; the promise
  // settles with the reply.
  const replies = new Map(), waiting = [];
  function push(el, name, payload) {
    const root = el && el.closest("[data-vk-id]");
    if (!live || !root) return Promise.reject(new Error("live: push needs a live component"));
    return new Promise((resolve, reject) => {
      const go = () => {
        const msg = { type: "event", target: root.getAttribute("data-vk-id") + ":" + name, event: "push" };
        if (payload && typeof payload === "object") {
          msg.form = {};
          for (const [k, v] of Object.entries(payload)) msg.form[k] = [].concat(v).map(String);
        } else if (payload !== undefined && payload !== null) msg.value = String(payload);
        const id = send(msg, null, null);
        if (id) replies.set(id, { resolve, reject });
        else reject(new Error("live: not connected"));
      };
      if (joined) go();
      else waiting.push(go);
    });
  }

  // emit sends an island's event to an already-registered handler id (a
  // data-vk-emit-<name> value, "cN:idx"), with the emit's argument as a JSON
  // payload the server binds into the handler's parameter (ui.Event.Data).
  function emitIsland(target, payload) {
    if (!live) return Promise.reject(new Error("live: emit needs a live page"));
    return new Promise((resolve, reject) => {
      const go = () => {
        const msg = { type: "event", target, event: "emit" };
        if (payload !== undefined) msg.data = payload;
        const id = send(msg, null, null);
        if (id) replies.set(id, { resolve, reject });
        else reject(new Error("live: not connected"));
      };
      if (joined) go();
      else waiting.push(go);
    });
  }

  // register names a function for call; a page's script that runs first
  // queues its calls on window.vuka.q.
  const register = (name, fn) => { fns.set(String(name), fn); };
  ((window.vuka && window.vuka.q) || []).forEach((r) => register(...r));
  // states is client state for islands: an island's state prop reads a
  // state (or a derived expression), follows it, and writes it back.
  const states = {
    read: (x) => ev(x),
    write: (r, v) => { if (r && r.s && !r.x) { swrite(r, v); flushStates(); } },
    watch: (x, fn, el) => {
      const b = { el: el || document.documentElement, kind: "fn", x, fn, deps: deps(x) };
      b.last = ev(x);
      b.deps.forEach((d) => { if (!subs.has(d[0])) subs.set(d[0], new Set()); subs.get(d[0]).add(b); });
      return () => unsub(b);
    },
  };
  // Active links: a link with data-vk-active ("exact" or "prefix", ui.Link's
  // Active) is the current page's while the address's path is its href's
  // (or, prefix, below it): aria-current="page", and data-vk-active-class's
  // class. Set as the page loads, after every navigation and render, so a
  // layout's menu follows the page without rendering again. An element
  // marked data-vk-active-within is "true" while it holds a current link,
  // "" when not; a <details> so marked opens as a current link comes inside
  // it, the page loading included, and is the visitor's to close (a morph
  // keeps it open, as it keeps a group the visitor opened: openKept).
  const within = new WeakMap();
  function markActive() {
    if (!document.querySelectorAll) return;
    const path = location.pathname;
    document.querySelectorAll("a[data-vk-active]").forEach((a) => {
      let u;
      try { u = new URL(a.getAttribute("href") || "", location.href); } catch (_) { return; }
      const at = u.pathname, base = at.endsWith("/") ? at : at + "/";
      const on = u.origin === location.origin && (path === at || a.getAttribute("data-vk-active") === "prefix" && path.startsWith(base));
      if (on && a.getAttribute("aria-current") !== "page") a.setAttribute("aria-current", "page");
      else if (!on && a.getAttribute("aria-current") === "page") a.removeAttribute("aria-current");
      const c = a.getAttribute("data-vk-active-class");
      if (c) c.split(/\s+/).filter(Boolean).forEach((k) => a.classList.toggle(k, on));
    });
    document.querySelectorAll("[data-vk-active-within]").forEach((g) => {
      const on = !!g.querySelector('a[data-vk-active][aria-current="page"]'), v = on ? "true" : "";
      if (g.getAttribute("data-vk-active-within") !== v) g.setAttribute("data-vk-active-within", v);
      if (on && !within.get(g) && g.tagName === "DETAILS" && !g.open) touch(g, "open", () => { g.open = true; });
      within.set(g, on);
    });
  }

  // openKept: a <details> the visitor opens or closes stays so through the
  // morphs that render it as before, as an attribute a command changed
  // does; a render changing its open is the server's.
  document.addEventListener("toggle", (e) => {
    const d = e.target;
    if (!d || d.tagName !== "DETAILS") return;
    let m = touched.get(d);
    if (!m) touched.set(d, (m = new Map()));
    if (!m.has("open")) m.set("open", { server: d.open ? null : "" });
    m.get("open").value = d.open ? "" : null;
  }, true);

  // Prefetch: a link the browser will load as a document — a ui.Link
  // (data-vk-link) on a page without a live connection, or one marked
  // data-vk-prefetch — is fetched ahead, on hover (after a moment), focus
  // or touch, or once in view with data-vk-prefetch="viewport", so it
  // opens at once. Only same-origin http(s) GETs of another page, never a
  // download, a link with a target, or one marked data-vk-prefetch="off";
  // none when the visitor asks to save data. The browser keeps the
  // response for the navigation (speculation rules, else
  // <link rel=prefetch>); after 30s it is dropped. The fetch is a page's
  // HTTP render: a live page's session starts when the page is opened. A
  // link moving a live page over its connection isn't fetched: the
  // server renders only what changes below the layouts kept. A link the
  // runtime follows over HTTP (a page that isn't live) prefetches the
  // navigation's reply itself, at most a few at a time, and a click
  // within 30s uses it instead of fetching again.
  const prefetched = new Map(); // href → when
  const navAhead = new Map(); // href → {p, at}: navigation replies fetched ahead
  let aheadFlying = 0;
  const maxAhead = 3;
  const inPlace = (a) => !live && build && a.hasAttribute("data-vk-link") && !a.hasAttribute("data-vk-reload");
  // fetchedAhead takes the reply fetched ahead for href, once.
  function fetchedAhead(href) {
    const e = navAhead.get(href);
    if (!e) return null;
    navAhead.delete(href);
    return Date.now() - e.at < 30000 ? e.p : null;
  }
  const speculation = typeof HTMLScriptElement !== "undefined" && HTMLScriptElement.supports && HTMLScriptElement.supports("speculationrules");
  function prefetchable(a) {
    if (!a || !a.matches("a[href]") || a.hasAttribute("download") || /^(?!_self$)./.test(a.getAttribute("target") || "")) return null;
    const mode = a.getAttribute("data-vk-prefetch");
    if (mode === "off" || mode === "false" || mode === null && !a.hasAttribute("data-vk-link")) return null;
    if (mode === null && live && joined) return null; // it moves over the connection
    if (navigator.connection && navigator.connection.saveData) return null;
    const u = httpURL(a.getAttribute("href"));
    if (!u || u.origin !== location.origin || u.pathname + u.search === location.pathname + location.search) return null;
    u.hash = "";
    return u.href;
  }
  function prefetch(a) {
    const href = prefetchable(a);
    if (!href || Date.now() - (prefetched.get(href) || -Infinity) < 30000) return;
    if (inPlace(a)) {
      if (aheadFlying >= maxAhead) return;
      prefetched.set(href, Date.now());
      const p = fetched(href, true), done = () => { aheadFlying--; };
      aheadFlying++;
      p.then(done, done);
      navAhead.set(href, { p, at: Date.now() });
      setTimeout(() => { prefetched.delete(href); if (navAhead.has(href) && navAhead.get(href).p === p) navAhead.delete(href); }, 30000);
      return;
    }
    prefetched.set(href, Date.now());
    let el;
    if (speculation) {
      el = document.createElement("script");
      el.type = "speculationrules";
      el.textContent = JSON.stringify({ prefetch: [{ source: "list", urls: [href] }] });
    } else {
      el = document.createElement("link");
      el.rel = "prefetch";
      el.href = href;
    }
    document.head.appendChild(el);
    setTimeout(() => { el.remove(); prefetched.delete(href); }, 30000);
  }
  let hovering = null;
  const linkOf = (e) => { const t = e.target instanceof Element ? e.target : null; return t && t.closest("a[href]"); };
  document.addEventListener("mouseover", (e) => {
    const a = linkOf(e);
    if (!a || hovering && hovering.a === a) return;
    if (hovering) clearTimeout(hovering.timer);
    hovering = { a, timer: setTimeout(() => prefetch(a), 80) };
  }, { passive: true });
  document.addEventListener("mouseout", (e) => {
    if (hovering && linkOf(e) === hovering.a && !hovering.a.contains(e.relatedTarget)) { clearTimeout(hovering.timer); hovering = null; }
  }, { passive: true });
  document.addEventListener("focusin", (e) => prefetch(linkOf(e)));
  document.addEventListener("touchstart", (e) => prefetch(linkOf(e)), { passive: true });
  const inView = typeof IntersectionObserver === "function" && new IntersectionObserver((es) => es.forEach((e) => {
    if (e.isIntersecting) { inView.unobserve(e.target); prefetch(e.target); }
  }));
  const watchViewport = () => { if (inView && document.querySelectorAll) document.querySelectorAll('a[data-vk-prefetch="viewport"]').forEach((a) => inView.observe(a)); };
  document.addEventListener("vk:patched", watchViewport);

  window.vuka = { connect, tree, register, push, emit: emitIsland, states, navigate, run: (el, ops, e) => run(el, ops, e || null), state: () => document.documentElement.getAttribute("data-vk-state") };
  if (bodyWatch) bodyWatch.observe(document.body, { childList: true });
  const page0 = document.documentElement.outerHTML;
  listenIn(page0);
  bindIn(document.body, page0);
  markActive();
  watchViewport();
  // A link moves over the connection, or in place over HTTP on a page that
  // isn't live; data-vk-replace (VukaLink's replace) also takes the place of
  // the current history entry when it loads. data-vk-reload leaves the
  // link to the browser.
  document.addEventListener("click", (e) => {
    const t = e.target instanceof Element ? e.target : e.target && e.target.parentElement;
    const a = t && t.closest("a[data-vk-link]"), href = a && a.getAttribute("href");
    const taken = joined || !live && build;
    if (!href || !taken && !a.hasAttribute("data-vk-replace") || a.hasAttribute("data-vk-reload") || e.defaultPrevented || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey ||
      /^(?!_self$)./.test(a.getAttribute("target") || "") || a.hasAttribute("download")) return;
    const u = new URL(href, location.href);
    if (u.origin !== location.origin || u.pathname + u.search === here && u.hash) return;
    e.preventDefault();
    move(u.href, a.getAttribute("data-vk-link") === "patch", null, a.hasAttribute("data-vk-replace"));
  });
  if (!live && !build) return;
  history.replaceState({ ...history.state, vk: pageID }, "");
  window.addEventListener("popstate", (e) => {
    markActive();
    const st = e.state || {};
    if (location.pathname + location.search === here && (!st.vk || st.vk === pageID)) return; // a #hash
    move(location.href, st.vk === pageID, st);
  });
  window.addEventListener("scroll", () => { clearTimeout(saving); saving = setTimeout(save, 200); }, { passive: true });
  window.addEventListener("online", () => { if (live && !ws) { attempt = 0; connect(); } });
  document.addEventListener("visibilitychange", () => {
    if (live && sleeping && document.visibilityState !== "hidden" && !ws) { sleeping = false; attempt = 0; connect(); }
  });
  if (live) connect();
})();
