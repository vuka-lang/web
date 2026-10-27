// The development toolbar's GraphiQL panel: an editor for GraphQL documents
// and their variables and headers, completing at the cursor from the
// app's schema (read by introspection), highlighting, checked as it is
// typed by the app's own engine, with hover, a schema explorer, the
// schema's operations ready to run, formatting, history and tabs. The
// toolbar loads it when the panel first shows; development only.
//
// The language half (lexing, the analysis behind completion, hover and
// highlighting, formatting, operations, the variables' JSON) is plain
// functions, exported for node's tests; the editor half needs a DOM.
(() => {
  "use strict";

  // ---- Schema ----------------------------------------------------------

  // The introspection query the panel reads the schema with.
  const INTROSPECTION = `query VukaToolbarSchema {
  __schema {
    queryType { name } mutationType { name } subscriptionType { name }
    types {
      kind name description
      fields(includeDeprecated: true) { name description args { ...V } type { ...T } isDeprecated deprecationReason }
      inputFields { ...V }
      interfaces { name }
      possibleTypes { name }
      enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
    }
    directives { name description locations args { ...V } }
  }
}
fragment V on __InputValue { name description type { ...T } defaultValue }
fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } }`;

  // makeSchema indexes an introspection result's __schema.
  function makeSchema(s) {
    const types = Object.create(null);
    for (const t of s.types) {
      types[t.name] = {
        kind: t.kind, name: t.name, description: t.description || "",
        fields: t.fields || [], inputFields: t.inputFields || [], enumValues: t.enumValues || [],
        interfaces: (t.interfaces || []).map((x) => x.name), possibleTypes: (t.possibleTypes || []).map((x) => x.name),
      };
    }
    return {
      types,
      query: s.queryType && s.queryType.name,
      mutation: s.mutationType && s.mutationType.name,
      subscription: s.subscriptionType && s.subscriptionType.name,
      directives: s.directives || [],
    };
  }

  const NN = (t) => ({ kind: "NON_NULL", ofType: t });
  const typeText = (t) => !t ? "" : t.kind === "NON_NULL" ? typeText(t.ofType) + "!" : t.kind === "LIST" ? "[" + typeText(t.ofType) + "]" : t.name;
  const named = (t) => { while (t && t.ofType) t = t.ofType; return t ? t.name : null; };
  const nullable = (t) => t && t.kind === "NON_NULL" ? t.ofType : t;
  const required = (a) => !!a.type && a.type.kind === "NON_NULL" && a.defaultValue == null;
  const typenameField = { name: "__typename", description: "The name of the object's type.", args: [], type: NN({ kind: "SCALAR", name: "String" }) };

  // parseTypeRef reads a type as a variable declares it: "[ID!]!".
  function parseTypeRef(src) {
    let i = 0;
    const s = src.replace(/\s+/g, "");
    function one() {
      let t;
      if (s[i] === "[") { i++; t = { kind: "LIST", ofType: one() }; if (s[i] === "]") i++; }
      else { const m = /^[_A-Za-z]\w*/.exec(s.slice(i)); if (!m) return null; i += m[0].length; t = { kind: "NAMED", name: m[0] }; }
      if (s[i] === "!") { i++; t = NN(t); }
      return t;
    }
    return s ? one() : null;
  }

  const rootOf = (schema, op) => op === "mutation" ? schema.mutation : op === "subscription" ? schema.subscription : schema.query;
  const typeOf = (schema, name) => (name && schema.types[name]) || null;
  function fieldOf(schema, typeName, name) {
    if (name === "__typename") return typenameField;
    const t = typeOf(schema, typeName);
    return (t && t.fields.find((f) => f.name === name)) || null;
  }
  const composite = (t) => !!t && (t.kind === "OBJECT" || t.kind === "INTERFACE" || t.kind === "UNION");
  const inputType = (t) => !!t && (t.kind === "SCALAR" || t.kind === "ENUM" || t.kind === "INPUT_OBJECT");
  // possible is what a fragment on a value of type name may be on.
  function possible(schema, name) {
    const t = typeOf(schema, name);
    if (!t) return [];
    if (t.kind === "OBJECT") return [name, ...t.interfaces];
    return [name, ...t.possibleTypes];
  }

  // ---- Lexing ----------------------------------------------------------

  // lex is src's tokens, whitespace left out: k is the kind — name, var
  // ($id), dir (@skip), spread, str, block (a block string), num, cmt,
  // p (punctuation, its char in v) and bad; s and e their offsets.
  function lex(src) {
    const out = [];
    let i = 0;
    const n = src.length;
    while (i < n) {
      const c = src[i];
      if (c === " " || c === "\t" || c === "\n" || c === "\r" || c === "﻿") { i++; continue; }
      const s = i;
      if (c === "#") {
        while (i < n && src[i] !== "\n" && src[i] !== "\r") i++;
        out.push({ k: "cmt", s, e: i });
      } else if (c === '"') {
        if (src.startsWith('"""', i)) {
          i += 3;
          while (i < n && !src.startsWith('"""', i)) i += src.startsWith('\\"""', i) ? 4 : 1;
          const closed = i < n;
          i = Math.min(n, i + 3);
          out.push({ k: "block", s, e: i, open: !closed });
        } else {
          i++;
          while (i < n && src[i] !== '"' && src[i] !== "\n") i += src[i] === "\\" ? 2 : 1;
          const closed = src[i] === '"';
          if (closed) i++;
          out.push({ k: "str", s, e: Math.min(i, n), open: !closed });
        }
      } else if (c === "$" || c === "@") {
        i++;
        while (i < n && /\w/.test(src[i])) i++;
        out.push({ k: c === "$" ? "var" : "dir", s, e: i, v: src.slice(s + 1, i) });
      } else if (c === "." && src.startsWith("...", i)) {
        i += 3;
        out.push({ k: "spread", s, e: i });
      } else if (/[_A-Za-z]/.test(c)) {
        while (i < n && /\w/.test(src[i])) i++;
        out.push({ k: "name", s, e: i, v: src.slice(s, i) });
      } else if (/[-0-9]/.test(c)) {
        const m = /^-?\d*(\.\d+)?([eE][+-]?\d+)?/.exec(src.slice(i));
        i += Math.max(1, m[0].length);
        out.push({ k: "num", s, e: i });
      } else if ("{}()[]:=!|&,".includes(c)) {
        i++;
        out.push({ k: "p", s, e: i, v: c });
      } else {
        i++;
        out.push({ k: "bad", s, e: i });
      }
    }
    return out;
  }

  // ---- Analysis --------------------------------------------------------

  // analyze walks src's tokens as a GraphQL document over schema, however
  // incomplete: each token's role (for highlighting) and what it names
  // (for hover), the operations and fragments, the brackets' pairs, and —
  // given a cursor — what may be typed there (ctx: the range a completion
  // replaces, the word typed, the items).
  function analyze(src, schema, cursor) {
    const toks = lex(src);
    const roles = new Array(toks.length).fill("");
    const info = new Array(toks.length).fill(null);
    const ops = [], frags = [];
    const next = (i) => {
      for (let j = i + 1; j < toks.length; j++) if (toks[j].k !== "cmt" && !(toks[j].k === "p" && toks[j].v === ",")) return j;
      return -1;
    };
    // The fragments the document defines, for spreads before them.
    for (let i = 0, depth = 0; i < toks.length; i++) {
      const t = toks[i];
      if (t.k === "p" && "{([".includes(t.v)) depth++;
      else if (t.k === "p" && "})]".includes(t.v)) depth = Math.max(0, depth - 1);
      else if (depth === 0 && t.k === "name" && t.v === "fragment") {
        const nm = next(i), on = nm >= 0 ? next(nm) : -1, ty = on >= 0 ? next(on) : -1;
        if (nm >= 0 && toks[nm].k === "name") frags.push({ name: toks[nm].v, on: ty >= 0 && toks[on].v === "on" && toks[ty].k === "name" ? toks[ty].v : null, s: t.s, e: src.length });
      }
    }
    const pairs = new Map();
    {
      const open = [];
      for (let i = 0; i < toks.length; i++) {
        const t = toks[i];
        if (t.k !== "p") continue;
        if ("{([".includes(t.v)) open.push(i);
        else if ("})]".includes(t.v)) {
          const want = { "}": "{", ")": "(", "]": "[" }[t.v];
          for (let j = open.length - 1; j >= 0; j--) {
            if (toks[open[j]].v === want) { pairs.set(open[j], i); pairs.set(i, open[j]); open.length = j; break; }
          }
        }
      }
    }

    const stack = [{ k: "doc" }];
    let op = null; // the operation being walked
    let ctx = null, ctxDone = cursor == null;
    const top = () => stack[stack.length - 1];

    const snapshot = (tok, none) => {
      ctxDone = true;
      if (none) { ctx = null; return; }
      let word = "", from = cursor, to = cursor;
      if (tok) { word = src.slice(tok.s, cursor); from = tok.s; to = tok.e; }
      ctx = { from, to, word, items: candidates(top(), word) };
    };

    function candidates(f, word) {
      if (word.startsWith("@")) return directives(f);
      if (word.startsWith("$")) return valuePos(f) ? values(f.valueType, true).filter((x) => x.kind === "var") : [];
      switch (f.k) {
        case "doc": return keywords();
        case "vars":
          if (f.sub === "type") return Object.values(schema.types).filter((t) => inputType(t) && !t.name.startsWith("__")).map(typeItem);
          if (f.sub === "default") return values(f.valueType, false);
          return [];
        case "frag":
          if (f.sub === "on") return [{ label: "on", kind: "kw", insert: "on " }];
          if (f.sub === "type") return Object.values(schema.types).filter((t) => composite(t) && !t.name.startsWith("__")).map(typeItem);
          return [];
        case "sel":
          if (f.spread === "dots") {
            const can = new Set(possible(schema, f.type));
            const fr = frags.filter((x) => !x.on || !typeOf(schema, f.type) || can.has(x.on) || possible(schema, x.on).includes(f.type))
              .map((x) => ({ label: x.name, detail: x.on ? "on " + x.on : "", kind: "frag", insert: x.name }));
            return [{ label: "on", kind: "kw", detail: "inline fragment", insert: "on " }, ...fr];
          }
          if (f.spread === "on") return possible(schema, f.type).map((n) => typeItem(schema.types[n])).filter(Boolean);
          return fields(f.type);
        case "args":
          if (f.sub === "name") return argItems(f.owner ? f.owner.def.args : [], f.given, false);
          if (f.sub === "value") return values(f.valueType, true);
          return [];
        case "obj":
          if (f.sub === "name") { const t = typeOf(schema, f.type); return argItems(t ? t.inputFields : [], f.given, true); }
          if (f.sub === "value") return values(f.valueType, true);
          return [];
        case "list": return values(f.valueType, true);
      }
      return [];
    }
    const valuePos = (f) => (f.k === "args" || f.k === "obj") && f.sub === "value" || f.k === "list" || f.k === "vars" && f.sub === "default";
    function keywords() {
      const out = [{ label: "query", kind: "kw", detail: "operation", insert: "query " }];
      if (schema.mutation) out.push({ label: "mutation", kind: "kw", detail: "operation", insert: "mutation " });
      if (schema.subscription) out.push({ label: "subscription", kind: "kw", detail: "operation", insert: "subscription " });
      out.push({ label: "fragment", kind: "kw", detail: "fragment", insert: "fragment " });
      return out;
    }
    function typeItem(t) {
      return t && { label: t.name, detail: t.kind.toLowerCase().replace("_", " "), doc: t.description, kind: "type", insert: t.name };
    }
    function fields(typeName) {
      const t = typeOf(schema, typeName);
      if (!t) return [];
      const out = (t.kind === "UNION" ? [] : t.fields).map((f) => ({
        label: f.name, kind: "field", field: f, insert: f.name, doc: f.description, deprecated: f.isDeprecated,
        detail: (f.args.length ? "(" + f.args.map((a) => a.name + ": " + typeText(a.type)).join(", ") + ") " : "") + typeText(f.type),
      }));
      out.push({ label: "__typename", kind: "field", field: typenameField, insert: "__typename", detail: "String!", doc: typenameField.description });
      return out;
    }
    function argItems(args, given, input) {
      return args.filter((a) => !given.has(a.name)).map((a) => ({
        label: a.name, kind: input ? "input" : "arg", arg: a, insert: a.name + ": ", retrigger: true, required: required(a), deprecated: a.isDeprecated,
        detail: typeText(a.type) + (a.defaultValue != null ? " = " + a.defaultValue : ""), doc: a.description,
      }));
    }
    function values(t, withVars) {
      const out = [];
      const n = named(t), def = typeOf(schema, n), base = nullable(t);
      if (def && def.kind === "ENUM") for (const v of def.enumValues) out.push({ label: v.name, kind: "enum", insert: v.name, detail: n, doc: v.description, deprecated: v.isDeprecated });
      if (n === "Boolean" && !(base && base.kind === "LIST")) out.push({ label: "true", kind: "atom", insert: "true", detail: "Boolean" }, { label: "false", kind: "atom", insert: "false", detail: "Boolean" });
      if (def && def.kind === "INPUT_OBJECT" && !(base && base.kind === "LIST")) out.push({ label: "{ }", kind: "snippet", insert: "{}", back: 1, retrigger: true, detail: n });
      if (withVars && op) for (const v of op.vars) if (!n || named(v.type) === n) out.push({ label: "$" + v.name, detail: typeText(v.type), kind: "var", insert: "$" + v.name });
      if (t && t.kind !== "NON_NULL") out.push({ label: "null", kind: "atom", insert: "null" });
      return out;
    }
    function directives(f) {
      let loc = "FIELD";
      if (f.k === "op") loc = (op && op.kind || "query").toUpperCase();
      else if (f.k === "frag") loc = "FRAGMENT_DEFINITION";
      else if (f.k === "sel" && f.spread === "frag") loc = "FRAGMENT_SPREAD";
      else if (f.k === "sel" && (f.spread === "cond" || f.spread === "dots")) loc = "INLINE_FRAGMENT";
      else if (f.k === "vars") loc = "VARIABLE_DEFINITION";
      return schema.directives.filter((d) => !d.locations || d.locations.includes(loc)).map((d) => {
        const req = d.args.filter(required);
        return {
          label: "@" + d.name, kind: "dir", dir: d, doc: d.description, detail: d.args.length ? "(" + d.args.map((a) => a.name + ": " + typeText(a.type)).join(", ") + ")" : "",
          insert: "@" + d.name + (req.length ? "(" + req[0].name + ": )" : ""), back: req.length ? 1 : 0, retrigger: req.length > 0,
        };
      });
    }

    function valueDone(f) {
      if (f.k === "args" || f.k === "obj") { f.sub = "name"; f.arg = null; }
      else if (f.k === "vars") f.sub = "var";
    }
    // value walks a value token in f's value position: true when it was one.
    function value(i, f) {
      const t = toks[i];
      if (t.k === "var") {
        roles[i] = "var";
        const d = op && op.vars.find((v) => v.name === t.v);
        info[i] = { kind: "var", name: t.v, type: d ? d.type : null };
        valueDone(f);
      } else if (t.k === "name") {
        if (t.v === "true" || t.v === "false" || t.v === "null") roles[i] = "atom";
        else { roles[i] = "enum"; const n = named(f.valueType); if (n) info[i] = { kind: "enum", type: n, name: t.v }; }
        valueDone(f);
      } else if (t.k === "str" || t.k === "block") { roles[i] = "str"; valueDone(f); }
      else if (t.k === "num") { roles[i] = "num"; valueDone(f); }
      else if (t.k === "p" && t.v === "{") stack.push({ k: "obj", type: named(f.valueType), given: new Set(), sub: "name", valueType: null });
      else if (t.k === "p" && t.v === "[") { const lt = nullable(f.valueType); stack.push({ k: "list", valueType: lt && lt.kind === "LIST" ? lt.ofType : f.valueType }); }
      else return false;
      return true;
    }
    // close ends the innermost frame of kinds a closing bracket ends.
    function close(i, kinds) {
      for (let j = stack.length - 1; j > 0; j--) {
        if (!kinds.includes(stack[j].k)) continue;
        const f = stack[j];
        stack.length = j;
        if (f.k === "sel") {
          const p = top();
          if (p.k === "op" || p.k === "frag") {
            if (p.def) p.def.e = toks[i].e;
            stack.pop();
            if (p.k === "op") op = null;
          }
        } else if (f.k === "obj" || f.k === "list") valueDone(top());
        return;
      }
    }
    const finishVar = (f) => {
      if (f.cur) { f.cur.type = parseTypeRef(f.cur.typeText); if (op) op.vars.push(f.cur); f.cur = null; }
    };

    for (let i = 0; i < toks.length; i++) {
      const t = toks[i];
      if (!ctxDone) {
        if (t.s >= cursor) snapshot(null);
        else if (t.e >= cursor && (t.k === "name" || t.k === "var" || t.k === "dir")) snapshot(t);
        else if (t.e >= cursor && (t.k === "cmt" || t.open || (t.k === "str" || t.k === "block") && t.e > cursor)) snapshot(null, true);
      }
      if (t.k === "cmt") { roles[i] = "cmt"; continue; }
      if (t.k === "p" && t.v === ",") { roles[i] = "p"; continue; }
      if (t.k === "bad") { roles[i] = "bad"; continue; }
      const f = top();
      if (t.k === "p") roles[i] = "p";
      switch (f.k) {
        case "doc":
          if (t.k === "name" && (t.v === "query" || t.v === "mutation" || t.v === "subscription")) {
            roles[i] = "kw";
            op = { kind: t.v, name: null, s: t.s, e: src.length, vars: [] };
            ops.push(op);
            stack.push({ k: "op", sub: "head", def: op });
          } else if (t.k === "name" && t.v === "fragment") {
            roles[i] = "kw";
            stack.push({ k: "frag", sub: "name", def: frags.find((x) => x.s === t.s) || {} });
          } else if (t.k === "p" && t.v === "{") {
            op = { kind: "query", name: null, s: t.s, e: src.length, vars: [], short: true };
            ops.push(op);
            stack.push({ k: "op", sub: "body", def: op });
            stack.push({ k: "sel", type: schema.query });
          } else if (t.k === "str" || t.k === "block") roles[i] = "str";
          break;
        case "op":
          if (t.k === "name" && f.sub === "head" && !op.name) { roles[i] = "def"; op.name = t.v; info[i] = { kind: "op", op }; }
          else if (t.k === "p" && t.v === "(") stack.push({ k: "vars", sub: "var", cur: null });
          else if (t.k === "dir") { roles[i] = "dir"; info[i] = { kind: "dir", def: schema.directives.find((d) => d.name === t.v) }; }
          else if (t.k === "p" && t.v === "{") { f.sub = "body"; stack.push({ k: "sel", type: rootOf(schema, op.kind) }); }
          break;
        case "vars":
          if (f.sub === "default" && value(i, f)) break;
          if (t.k === "var") {
            finishVar(f);
            roles[i] = "var";
            f.cur = { name: t.v, typeText: "", s: t.s };
            info[i] = { kind: "vardef", name: t.v };
            f.sub = "colon";
          } else if (t.k === "p" && t.v === ":" && f.sub === "colon") f.sub = "type";
          else if (t.k === "name" && (f.sub === "type" || f.sub === "typed")) { roles[i] = "type"; info[i] = { kind: "type", name: t.v }; if (f.cur) f.cur.typeText += t.v; f.sub = "typed"; }
          else if (t.k === "p" && "[]!".includes(t.v) && (f.sub === "type" || f.sub === "typed")) { if (f.cur) f.cur.typeText += t.v; f.sub = t.v === "[" ? "type" : "typed"; }
          else if (t.k === "p" && t.v === "=") { finishVar(f); const d = op && op.vars[op.vars.length - 1]; f.sub = "default"; f.valueType = d ? d.type : null; }
          else if (t.k === "dir") roles[i] = "dir";
          else if (t.k === "p" && t.v === ")") { finishVar(f); stack.pop(); }
          break;
        case "frag":
          if (t.k === "name" && f.sub === "name") { roles[i] = "def"; f.sub = "on"; info[i] = { kind: "frag", name: t.v }; }
          else if (t.k === "name" && f.sub === "on" && t.v === "on") { roles[i] = "kw"; f.sub = "type"; }
          else if (t.k === "name" && f.sub === "type") { roles[i] = "type"; info[i] = { kind: "type", name: t.v }; f.on = t.v; f.sub = "head"; }
          else if (t.k === "dir") roles[i] = "dir";
          else if (t.k === "p" && t.v === "{") { f.sub = "body"; stack.push({ k: "sel", type: f.on }); }
          break;
        case "sel":
          if (t.k === "name") {
            if (f.spread === "dots") {
              if (t.v === "on") { roles[i] = "kw"; f.spread = "on"; }
              else { roles[i] = "frag"; info[i] = { kind: "frag", name: t.v }; f.spread = "frag"; f.owner = null; }
            } else if (f.spread === "on") {
              roles[i] = "type"; info[i] = { kind: "type", name: t.v }; f.cond = t.v; f.spread = "cond";
            } else {
              const n = next(i);
              if (n >= 0 && toks[n].k === "p" && toks[n].v === ":" && !f.alias) { roles[i] = "alias"; f.alias = true; f.spread = null; break; }
              f.alias = false;
              f.spread = null;
              const def = fieldOf(schema, f.type, t.v);
              roles[i] = "field";
              if (def) info[i] = { kind: "field", parent: f.type, def };
              f.last = def;
              f.owner = def ? { kind: "field", def } : null;
            }
          } else if (t.k === "spread") { roles[i] = "p"; f.spread = "dots"; f.owner = null; f.last = null; }
          else if (t.k === "dir") {
            roles[i] = "dir";
            const d = schema.directives.find((x) => x.name === t.v);
            info[i] = { kind: "dir", def: d };
            f.owner = d ? { kind: "dir", def: d } : null;
          } else if (t.k === "p" && t.v === "(") stack.push({ k: "args", owner: f.owner, given: new Set(), sub: "name" });
          else if (t.k === "p" && t.v === "{") {
            let type = null;
            if (f.spread === "cond") type = f.cond;
            else if (f.spread === "dots") type = f.type;
            else if (f.last) type = named(f.last.type);
            f.spread = null; f.last = null; f.owner = null;
            stack.push({ k: "sel", type });
          } else if (t.k === "p" && t.v === "}") close(i, ["sel", "obj"]);
          break;
        case "args":
        case "obj":
          if (f.sub === "value" && value(i, f)) break;
          if (t.k === "name" && f.sub === "name") {
            const defs = f.k === "args" ? (f.owner ? f.owner.def.args : []) : ((typeOf(schema, f.type) || {}).inputFields || []);
            const d = defs.find((a) => a.name === t.v);
            roles[i] = "arg";
            if (d) info[i] = { kind: f.k === "args" ? "arg" : "input", def: d, owner: f.owner, parent: f.type };
            f.given.add(t.v);
            f.arg = d;
            f.sub = "colon";
          } else if (t.k === "p" && t.v === ":") { f.sub = "value"; f.valueType = f.arg ? f.arg.type : null; }
          else if (t.k === "p" && t.v === ")") close(i, ["args", "vars"]);
          else if (t.k === "p" && t.v === "}") close(i, ["obj", "sel"]);
          else if (t.k === "p" && t.v === "]") close(i, ["list"]);
          break;
        case "list":
          if (value(i, f)) break;
          if (t.k === "p" && t.v === "]") close(i, ["list"]);
          else if (t.k === "p" && t.v === "}") close(i, ["obj", "sel"]);
          else if (t.k === "p" && t.v === ")") close(i, ["args", "vars"]);
          break;
      }
    }
    if (!ctxDone) snapshot(null);
    // Right after a value or a name, nothing completes until a space.
    if (ctx && !ctx.word && cursor > 0 && !/\s/.test(src[cursor - 1])) {
      const prev = toks.findLast((t) => t.e === cursor);
      if (prev && prev.k !== "p" && prev.k !== "spread") ctx.items = [];
    }
    return { toks, roles, info, ops, frags, pairs, ctx };
  }

  // ---- Matching --------------------------------------------------------

  // fuzzy scores label against word: null when the word's letters don't
  // appear in it in order; matched are the label's indices matched.
  function fuzzy(word, label) {
    if (!word) return { score: 0, matched: [] };
    const w = word.toLowerCase(), l = label.toLowerCase();
    if (l.startsWith(w)) return { score: 1000 - label.length, matched: [...Array(w.length).keys()] };
    let score = 0, j = 0, prev = -2;
    const matched = [];
    for (let i = 0; i < l.length && j < w.length; i++) {
      if (l[i] !== w[j]) continue;
      const boundary = i === 0 || label[i] !== l[i] || /[_$@.]/.test(label[i - 1]);
      score += 10 + (boundary ? 20 : 0) + (prev === i - 1 ? 15 : 0);
      matched.push(i);
      prev = i;
      j++;
    }
    if (j < w.length) return null;
    return { score: score - label.length, matched };
  }

  // rank is items matching word, best first, deprecated ones last; the
  // schema's order until something is typed.
  function rank(items, word) {
    const out = [];
    for (const it of items) {
      const m = fuzzy(word, it.label);
      if (m) out.push({ ...it, score: m.score - (it.deprecated ? 500 : 0), matched: m.matched });
    }
    return word ? out.sort((a, b) => b.score - a.score) : out.sort((a, b) => (a.deprecated ? 1 : 0) - (b.deprecated ? 1 : 0));
  }

  // ---- Inserting a completion ------------------------------------------

  const lineIndent = (src, pos) => {
    const start = src.lastIndexOf("\n", pos - 1) + 1;
    return /^[ \t]*/.exec(src.slice(start))[0];
  };

  // applyItem is src with item replacing what ctx covers: the text, the
  // cursor after it, and whether completion should open again there. A
  // field with required arguments gets () with the cursor inside, an
  // object field { } on lines of their own.
  function applyItem(src, ctx, item, schema) {
    let text = item.insert, back = item.back || 0, retrigger = !!item.retrigger;
    const after = src.slice(ctx.to);
    if (item.kind === "field" && item.field) {
      const f = item.field, ind = lineIndent(src, ctx.from);
      const needsArgs = f.args.some(required) && !/^\s*\(/.test(after);
      const needsSel = composite(typeOf(schema, named(f.type))) && !/^\s*[({]/.test(after);
      if (needsArgs) { text += "()"; back = 1; retrigger = true; }
      if (needsSel) {
        const sel = " {\n" + ind + "  \n" + ind + "}";
        text += sel;
        back = needsArgs ? 1 + sel.length : ind.length + 2;
      }
    } else if ((item.kind === "arg" || item.kind === "input") && /^\s*:/.test(after)) {
      text = item.label;
      retrigger = false;
    } else if ((item.kind === "arg" || item.kind === "input") && item.arg) {
      const t = typeOf(schema, named(item.arg.type)), base = nullable(item.arg.type);
      if (t && t.kind === "INPUT_OBJECT" && base.kind !== "LIST") { text += "{}"; back = 1; }
    }
    return { text: src.slice(0, ctx.from) + text + src.slice(ctx.to), cursor: ctx.from + text.length - back, retrigger };
  }

  // ---- Formatting ------------------------------------------------------

  const WIDTH = 80;

  // parseDoc reads a document's tokens into the tree the printer prints:
  // definitions, selections, arguments and values, each with the comments
  // before it (and one on its line after the token it follows), and
  // whether a blank line came before it. A syntax error throws {pos,
  // message}.
  function parseDoc(src, toks) {
    const sig = [];
    const lineAt = [];
    { let n = 0; for (let i = 0; i <= src.length; i++) { lineAt.push(n); if (src[i] === "\n") n++; } }
    const blankBefore = (pos) => {
      let i = pos - 1, nl = 0;
      while (i >= 0 && /\s/.test(src[i])) { if (src[i] === "\n") nl++; i--; }
      return i >= 0 && nl >= 2;
    };
    let pending = [];
    for (const t of toks) {
      if (t.k === "p" && t.v === ",") continue;
      if (t.k === "cmt") {
        const prev = sig[sig.length - 1];
        if (prev && lineAt[prev.e] === lineAt[t.s] && !prev.trail && !pending.length) prev.trail = src.slice(t.s, t.e).trimEnd();
        else pending.push({ text: src.slice(t.s, t.e).trimEnd(), blank: blankBefore(t.s) });
        continue;
      }
      sig.push({ ...t, text: src.slice(t.s, t.e), lead: pending, blank: blankBefore(t.s) });
      pending = [];
    }
    const tail = pending;
    let i = 0;
    const peek = () => sig[i];
    const is = (v) => !!sig[i] && sig[i].k === "p" && sig[i].v === v;
    const isName = (v) => !!sig[i] && sig[i].k === "name" && (v == null || sig[i].v === v);
    const found = () => sig[i] ? `, found "${sig[i].text}"` : " at the end";
    const fail = (msg) => { throw { pos: sig[i] ? sig[i].s : src.length, message: msg }; };
    const take = () => sig[i++];
    const expect = (v) => { if (!is(v)) fail(`Expected "${v}"` + found()); return take(); };
    const expectName = () => { if (!isName()) fail("Expected a name" + found()); return take(); };
    const node = (first, o) => Object.assign(o, { lead: first.lead, blank: first.blank });
    const trailOf = (t) => t.trail || "";

    function type() {
      let s;
      if (is("[")) { take(); s = "[" + type() + "]"; expect("]"); }
      else s = expectName().v;
      if (is("!")) { take(); s += "!"; }
      return s;
    }
    function val() {
      const t = peek();
      if (!t) fail("Expected a value at the end");
      if (t.k === "var" || t.k === "num" || t.k === "name") { take(); return { k: "raw", text: t.text, trail: trailOf(t), lead: t.lead }; }
      if (t.k === "str" || t.k === "block") { take(); return { k: "raw", text: t.text, block: t.k === "block", trail: trailOf(t), lead: t.lead }; }
      if (is("[")) {
        const open = take(), items = [];
        while (!is("]")) { if (!peek()) fail('Expected "]" at the end'); items.push(val()); }
        const end = take();
        return { k: "list", items, trail: trailOf(end), lead: open.lead, inner: trailOf(open), end: end.lead };
      }
      if (is("{")) {
        const open = take(), fields = [];
        while (!is("}")) {
          if (!peek()) fail('Expected "}" at the end');
          const n = expectName(), colon = expect(":");
          fields.push(node(n, { name: n.text, value: val(), colonTrail: trailOf(n) + trailOf(colon) }));
        }
        const end = take();
        return { k: "obj", fields, trail: trailOf(end), lead: open.lead, inner: trailOf(open), end: end.lead };
      }
      fail(`Unexpected "${t.text}"`);
    }
    function args() {
      const open = take(), list = [];
      while (!is(")")) {
        if (!peek()) fail('Expected ")" at the end');
        const n = expectName(), colon = expect(":");
        list.push(node(n, { name: n.text, value: val(), colonTrail: trailOf(n) + trailOf(colon) }));
      }
      const end = take();
      return { list, inner: trailOf(open), end: end.lead, trail: trailOf(end) };
    }
    function dirs() {
      const out = [];
      while (peek() && peek().k === "dir") {
        const d = take();
        out.push({ name: d.text, args: is("(") ? args() : null, trail: trailOf(d) });
      }
      return out;
    }
    function selSet() {
      const open = expect("{"), items = [];
      while (!is("}")) {
        if (!peek()) fail('Expected "}" at the end');
        items.push(selection());
      }
      const end = take();
      return { items, inner: trailOf(open), end: end.lead, trail: trailOf(end) };
    }
    function selection() {
      const first = peek();
      if (first.k === "spread") {
        take();
        if (isName("on")) { take(); const ty = expectName(); return node(first, { k: "inline", on: ty.v, dirs: dirs(), sel: selSet() }); }
        if (isName()) { const n = take(); return node(first, { k: "spread", name: n.v, trail: trailOf(n), dirs: dirs() }); }
        return node(first, { k: "inline", on: null, dirs: dirs(), sel: selSet() });
      }
      let alias = null, n = expectName();
      if (is(":")) { take(); alias = n.v; n = expectName(); }
      const f = node(first, { k: "field", alias, name: n.v, trail: trailOf(n) });
      if (is("(")) f.args = args();
      f.dirs = dirs();
      if (is("{")) f.sel = selSet();
      return f;
    }
    function definition() {
      const first = peek();
      if (first.k === "str" || first.k === "block") fail("A description belongs to a schema, not to a query");
      if (is("{")) return node(first, { k: "op", op: null, sel: selSet() });
      if (isName("query") || isName("mutation") || isName("subscription")) {
        const kw = take();
        const d = node(first, { k: "op", op: kw.v, name: null });
        if (isName()) d.name = take().v;
        if (is("(")) {
          const open = take(), list = [];
          while (!is(")")) {
            const v = peek();
            if (!v || v.k !== "var") fail("Expected a variable" + found());
            take();
            expect(":");
            const vd = node(v, { name: v.text, type: type() });
            if (is("=")) { take(); vd.def = val(); }
            vd.dirs = dirs();
            list.push(vd);
          }
          const end = take();
          d.vars = { list, inner: trailOf(open), end: end.lead, trail: trailOf(end) };
        }
        d.dirs = dirs();
        d.sel = selSet();
        return d;
      }
      if (isName("fragment")) {
        take();
        const n = expectName();
        if (!isName("on")) fail('Expected "on"' + found());
        take();
        const on = expectName();
        return node(first, { k: "frag", name: n.v, on: on.v, dirs: dirs(), sel: selSet() });
      }
      fail(`Unexpected "${first.text}"`);
    }
    const defs = [];
    while (i < sig.length) defs.push(definition());
    return { defs, tail };
  }

  // A block string keeps its value: its lines move with the indentation
  // (a block string's value leaves out their common indentation).
  function reindentBlock(text, ind) {
    const lines = text.split("\n");
    if (lines.length === 1) return text;
    const rest = lines.slice(1);
    let close = null;
    if (/^\s*"""$/.test(rest[rest.length - 1])) close = rest.pop();
    const indents = rest.filter((l) => l.trim()).map((l) => /^[ \t]*/.exec(l)[0].length);
    const cut = indents.length ? Math.min(...indents) : 0;
    return [lines[0], ...rest.map((l) => l.trim() ? ind + "  " + l.slice(cut) : ""), ...(close != null ? [ind + '"""'] : [])].join("\n");
  }

  // printDoc prints a parsed document in the canonical style: 2-space
  // indentation, a field a line, arguments (and variables, input objects,
  // lists) on one line while it fits in 80 columns, else one a line;
  // comments where they were, a blank line between definitions.
  function printDoc(doc) {
    const comments = (lead, ind) => (lead || []).map((c) => ind + c.text + "\n").join("");
    // inline is v on one line, or null when it can't be (a comment, a
    // block string over lines).
    function inline(v) {
      if (v.trail || (v.lead && v.lead.length)) return null;
      if (v.k === "raw") return v.block && v.text.includes("\n") ? null : v.text;
      if (v.inner || (v.end && v.end.length)) return null;
      if (v.k === "list") {
        const parts = [];
        for (const x of v.items) { const s = inline(x); if (s == null) return null; parts.push(s); }
        return "[" + parts.join(", ") + "]";
      }
      const parts = [];
      for (const f of v.fields) {
        if (f.lead.length || f.colonTrail) return null;
        const s = inline(f.value);
        if (s == null) return null;
        parts.push(f.name + ": " + s);
      }
      return parts.length ? "{ " + parts.join(", ") + " }" : "{}";
    }
    // value prints v starting at column col, on lines indented ind.
    function value(v, ind, col) {
      const one = inline(v);
      if (one != null && col + one.length <= WIDTH) return one;
      if (v.k === "raw") return v.block ? reindentBlock(v.text, ind) : v.text;
      const inner = ind + "  ";
      let s = (v.k === "list" ? "[" : "{") + (v.inner ? " " + v.inner : "") + "\n";
      if (v.k === "obj") {
        for (const x of v.fields) {
          s += comments(x.lead, inner);
          const head = inner + x.name + ":" + (x.colonTrail ? " " + x.colonTrail + "\n" + inner + "  " : " ");
          s += head + value(x.value, inner, head.length - head.lastIndexOf("\n") - 1) + (x.value.trail ? " " + x.value.trail : "") + "\n";
        }
      } else {
        for (const x of v.items) {
          s += comments(x.lead, inner);
          s += inner + value({ ...x, lead: [], trail: "" }, inner, inner.length) + (x.trail ? " " + x.trail : "") + "\n";
        }
      }
      return s + comments(v.end, inner) + ind + (v.k === "list" ? "]" : "}");
    }
    // argList prints arguments (or variable definitions) opening at col.
    function argList(a, ind, col, each) {
      let fits = !a.inner && !(a.end && a.end.length);
      const parts = [];
      for (const x of a.list) {
        if (x.lead.length || x.colonTrail) fits = false;
        const s = fits ? each(x, null) : null;
        if (s == null) fits = false; else parts.push(s);
      }
      const one = "(" + parts.join(", ") + ")";
      if (fits && col + one.length <= WIDTH) return one;
      const inner = ind + "  ";
      let s = "(" + (a.inner ? " " + a.inner : "") + "\n";
      for (const x of a.list) s += comments(x.lead, inner) + inner + each(x, inner) + "\n";
      return s + comments(a.end, inner) + ind + ")";
    }
    const argEach = (x, inner) => {
      if (inner == null) { const v = inline(x.value); return v == null ? null : x.name + ": " + v; }
      const head = x.name + ":" + (x.colonTrail ? " " + x.colonTrail + "\n" + inner + "  " : " ");
      const col = x.colonTrail ? inner.length + 2 : inner.length + head.length;
      return head + value(x.value, inner, col) + (x.value.trail ? " " + x.value.trail : "");
    };
    const varEach = (x, inner) => {
      let s = x.name + ": " + x.type;
      if (x.def) {
        if (inner == null) { const v = inline(x.def); if (v == null) return null; s += " = " + v; }
        else s += " = " + value(x.def, inner, inner.length + s.length + 3) + (x.def.trail ? " " + x.def.trail : "");
      }
      if (x.dirs.length) { const d = directives(x.dirs, inner || "", 0); if (d == null) return null; s += d; }
      return s;
    };
    function directives(ds, ind, col) {
      let s = "";
      for (const d of ds) {
        if (d.trail) return null;
        s += " " + d.name;
        if (d.args) s += argList(d.args, ind, col + s.length, argEach);
      }
      return s;
    }
    function selSet(set, ind) {
      const inner = ind + "  ";
      let s = "{" + (set.inner ? " " + set.inner : "") + "\n";
      set.items.forEach((x, n) => {
        if (n > 0 && (x.lead.length ? x.lead[0].blank : x.blank)) s += "\n";
        s += comments(x.lead, inner) + inner + selection(x, inner) + "\n";
      });
      return s + comments(set.end, inner) + ind + "}" + (set.trail ? " " + set.trail : "");
    }
    function selection(x, ind) {
      if (x.k === "spread") return "..." + x.name + (directives(x.dirs, ind, ind.length) || "") + (x.trail ? " " + x.trail : "");
      if (x.k === "inline") {
        const head = "..." + (x.on ? " on " + x.on : "");
        return head + (directives(x.dirs, ind, ind.length + head.length) || "") + " " + selSet(x.sel, ind);
      }
      let s = (x.alias ? x.alias + ": " : "") + x.name;
      if (x.trail && (x.args || x.dirs.length || x.sel)) {
        // a comment after the name: the rest on the next line
        let rest = "";
        if (x.args) rest += argList(x.args, ind, ind.length, argEach);
        rest += directives(x.dirs, ind, ind.length + rest.length) || "";
        if (x.sel) rest += (rest ? " " : "") + selSet(x.sel, ind);
        return s + " " + x.trail + "\n" + ind + rest.replace(/^ /, "");
      }
      if (x.args) s += argList(x.args, ind, ind.length + s.length, argEach);
      s += directives(x.dirs, ind, ind.length + s.length) || "";
      if (x.sel) s += " " + selSet(x.sel, ind);
      else if (x.trail) s += " " + x.trail;
      return s;
    }
    const out = [];
    doc.defs.forEach((d, n) => {
      if (n > 0) out.push("\n");
      out.push(comments(d.lead, ""));
      if (d.k === "op" && !d.op) out.push(selSet(d.sel, ""));
      else if (d.k === "op") {
        let s = d.op + (d.name ? " " + d.name : "");
        if (d.vars) s += argList(d.vars, "", s.length, varEach);
        s += directives(d.dirs, "", s.length) || "";
        out.push(s + " " + selSet(d.sel, ""));
      } else {
        const s = "fragment " + d.name + " on " + d.on;
        out.push(s + (directives(d.dirs, "", s.length) || "") + " " + selSet(d.sel, ""));
      }
      out.push("\n");
    });
    if (doc.tail.length) out.push((doc.defs.length ? "\n" : "") + comments(doc.tail, ""));
    return out.join("");
  }

  // format is src in the canonical style, or {error, pos} when it doesn't
  // parse.
  function format(src) {
    try {
      return { text: printDoc(parseDoc(src, lex(src))) };
    } catch (e) {
      if (e && e.message != null && e.pos != null) return { error: e.message, pos: e.pos };
      throw e;
    }
  }

  // formatRange formats the definitions [from, to) touches (with the
  // comments right before the first), and nothing else: the text, and the
  // range of src it replaces.
  function formatRange(src, from, to) {
    const a = analyze(src, EMPTY);
    const defs = [...a.ops, ...a.frags].map((d) => ({ s: d.s, e: d.e })).sort((x, y) => x.s - y.s);
    const hit = defs.filter((d) => d.s < Math.max(to, from + 1) && d.e > from);
    if (!hit.length || from === to) return { from: 0, to: src.length, ...format(src) };
    let s = hit[0].s;
    const e = hit[hit.length - 1].e;
    const m = /((?:[ \t]*#[^\n]*\n)+)[ \t]*$/.exec(src.slice(0, s));
    if (m) s -= m[0].length;
    const r = format(src.slice(s, e));
    if (r.error) return { from: s, to: e, error: r.error, pos: r.pos + s };
    return { from: s, to: e, text: r.text.replace(/\n$/, "") };
  }

  // sameToken maps a cursor in oldText onto newText by its token: the nth
  // token in both (commas aside), and the offset into it.
  function sameToken(oldText, newText, pos, lexer) {
    const sig = (ts) => ts.filter((t) => !(t.k === "p" && t.v === ","));
    const a = sig((lexer || lex)(oldText)), b = sig((lexer || lex)(newText));
    const n = a.findIndex((t) => t.e >= pos);
    if (n < 0) return newText.length;
    const t = a[n];
    if (!b[n]) return newText.length;
    if (pos < t.s) return b[n].s;
    return Math.min(b[n].s + (pos - t.s), b[n].e);
  }

  // ---- Operations -------------------------------------------------------

  // operationFor is a ready-to-run operation of the root field f: the
  // keyword, a name, a variable per argument, the field with them, and a
  // selection of its scalars (and of its objects, their id), formatted.
  // sel is where the selection's first field starts; variables a value
  // for each required one.
  function operationFor(schema, kind, f) {
    const name = f.name[0].toUpperCase() + f.name.slice(1);
    let s = kind + " " + name + (f.args.length ? "(" + f.args.map((a) => "$" + a.name + ": " + typeText(a.type)).join(", ") + ")" : "") + " {\n  " + f.name;
    if (f.args.length) s += "(" + f.args.map((a) => a.name + ": $" + a.name).join(", ") + ")";
    const lines = selectionOf(schema, named(f.type));
    if (lines) s += " {\n" + lines.map((l) => "    " + l + "\n").join("") + "  }";
    s += "\n}\n";
    const r = format(s);
    if (r.text) s = r.text;
    const at = s.indexOf("\n  " + f.name) + 3;
    const sel = lines ? s.indexOf("{\n    ", at) + 6 : at;
    return { text: s, sel, name, variables: Object.fromEntries(f.args.filter(required).map((a) => [a.name, placeholder(schema, a.type)])) };
  }
  function selectionOf(schema, typeName) {
    const t = typeOf(schema, typeName);
    if (!composite(t)) return null;
    if (t.kind === "UNION") return ["__typename"];
    const out = [];
    for (const f of t.fields) {
      if (f.args.some(required) || f.isDeprecated) continue;
      const ft = typeOf(schema, named(f.type));
      if (!ft) continue;
      if (ft.kind === "SCALAR" || ft.kind === "ENUM") out.push(f.name);
      else if (ft.kind !== "UNION" && composite(ft) && ft.fields.some((x) => x.name === "id")) out.push(f.name + " { id }");
    }
    return out.length ? out : ["__typename"];
  }
  function placeholder(schema, t) {
    const n = named(t), d = typeOf(schema, n), base = nullable(t);
    if (base && base.kind === "LIST") return [];
    if (d && d.kind === "ENUM") return d.enumValues.length ? d.enumValues[0].name : "";
    if (d && d.kind === "INPUT_OBJECT") return Object.fromEntries(d.inputFields.filter(required).map((x) => [x.name, placeholder(schema, x.type)]));
    if (n === "Int" || n === "Float") return 0;
    if (n === "Boolean") return false;
    return "";
  }

  // ---- JSON (variables, headers) -----------------------------------------

  // lexJSON is src's JSON tokens: key, str, num, atom, p, bad.
  function lexJSON(src) {
    const out = [];
    let i = 0;
    while (i < src.length) {
      const c = src[i];
      if (/\s/.test(c)) { i++; continue; }
      const s = i;
      if (c === '"') {
        i++;
        while (i < src.length && src[i] !== '"' && src[i] !== "\n") i += src[i] === "\\" ? 2 : 1;
        const closed = src[i] === '"';
        if (closed) i++;
        out.push({ k: "str", s, e: Math.min(i, src.length), open: !closed });
      } else if (/[-0-9]/.test(c)) {
        const m = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(src.slice(i));
        i += m ? m[0].length : 1;
        out.push({ k: m ? "num" : "bad", s, e: i });
      } else if (/[a-zA-Z]/.test(c)) {
        while (i < src.length && /\w/.test(src[i])) i++;
        const w = src.slice(s, i);
        out.push({ k: w === "true" || w === "false" || w === "null" ? "atom" : "bad", s, e: i, v: w });
      } else if ("{}[]:,".includes(c)) { i++; out.push({ k: "p", s, e: i, v: c }); }
      else { i++; out.push({ k: "bad", s, e: i }); }
    }
    for (let j = 0; j + 1 < out.length; j++) if (out[j].k === "str" && out[j + 1].k === "p" && out[j + 1].v === ":") out[j].k = "key";
    return out;
  }

  // checkJSON is the first error in src as JSON — {pos, end, message} —
  // or null (empty is fine).
  function checkJSON(src) {
    const toks = lexJSON(src);
    if (!toks.length) return null;
    let i = 0;
    const desc = () => toks[i] ? `"${src.slice(toks[i].s, toks[i].e)}"` : "the end";
    const fail = (msg) => { throw { pos: toks[i] ? toks[i].s : src.length, end: toks[i] ? toks[i].e : src.length, message: msg }; };
    function value() {
      const t = toks[i];
      if (!t) fail("Expected a value at the end");
      if (t.open) fail("The string isn't closed");
      if (t.k === "str" || t.k === "num" || t.k === "atom") { i++; return; }
      if (t.k === "p" && (t.v === "{" || t.v === "[")) {
        const obj = t.v === "{", closer = obj ? "}" : "]";
        i++;
        if (toks[i] && toks[i].v === closer) { i++; return; }
        for (;;) {
          if (obj) {
            if (!toks[i] || toks[i].k !== "key") fail(`Expected a key in quotes, found ${desc()}`);
            if (toks[i].open) fail("The string isn't closed");
            i += 2;
          }
          value();
          if (toks[i] && toks[i].v === ",") { i++; continue; }
          if (toks[i] && toks[i].v === closer) { i++; return; }
          fail(`Expected "," or "${closer}", found ${desc()}`);
        }
      }
      fail(`Unexpected ${desc()}`);
    }
    try {
      value();
      if (i < toks.length) fail(`Unexpected ${desc()} after the value`);
      return null;
    } catch (e) {
      if (e && e.pos != null) return e;
      throw e;
    }
  }

  // jsonPaths is each key's and value's path in src: "filter.tags[0]".
  function jsonPaths(src) {
    const toks = lexJSON(src), out = [], stack = [];
    const here = () => stack.map((f) => f.arr ? "[" + f.idx + "]" : "." + f.key).join("").replace(/^\./, "");
    for (const t of toks) {
      const f = stack[stack.length - 1];
      if (t.k === "key") {
        if (f) { try { f.key = JSON.parse(src.slice(t.s, t.e)); } catch { f.key = ""; } }
        out.push({ path: here(), s: t.s, e: t.e, key: true });
      } else if (t.k === "p" && (t.v === "{" || t.v === "[")) {
        if (stack.length) out.push({ path: here(), s: t.s, e: t.e });
        stack.push({ arr: t.v === "[", key: null, idx: 0 });
      } else if (t.k === "p" && (t.v === "}" || t.v === "]")) stack.pop();
      else if (t.k === "p" && t.v === ",") { if (f && f.arr) f.idx++; }
      else if (t.k !== "p" && stack.length) out.push({ path: here(), s: t.s, e: t.e });
    }
    return out;
  }

  // jsonContext is what may be typed at cursor in a variables object: the
  // operation's variables as its keys, an input object's fields below, an
  // enum's values, booleans and null as values.
  function jsonContext(src, cursor, varDefs, schema) {
    const toks = lexJSON(src), stack = []; // {arr, key, keys}
    const itemOf = (t) => { const b = nullable(t); return b && b.kind === "LIST" ? b.ofType : t; };
    const inputField = (t, key) => { const d = typeOf(schema, named(t)); const f = d && d.inputFields.find((x) => x.name === key); return f ? f.type : null; };
    const varType = (key) => { const d = varDefs.find((v) => v.name === key); return d ? d.type : null; };
    // typeAt is the type of the container stack[j] (the root, the
    // variables, has none).
    function typeAt(j) {
      if (j <= 0) return null;
      const parent = stack[j - 1];
      if (parent.arr) return itemOf(typeAt(j - 1));
      return j - 1 === 0 ? varType(parent.key) : inputField(typeAt(j - 1), parent.key);
    }
    function at(tok, expectKey) {
      const n = stack.length - 1, f = stack[n];
      if (!f) return null;
      let from = cursor, to = cursor, word = "";
      if (tok) { from = tok.s; to = tok.e; word = src.slice(tok.s + (src[tok.s] === '"' ? 1 : 0), cursor); }
      const items = [];
      if (expectKey && !f.arr) {
        const given = new Set(f.keys);
        if (n === 0) {
          for (const v of varDefs) if (!given.has(v.name)) items.push({ label: v.name, detail: typeText(v.type), kind: "var", insert: JSON.stringify(v.name) + ": ", required: !!v.type && v.type.kind === "NON_NULL" });
        } else {
          const d = typeOf(schema, named(typeAt(n)));
          if (d && d.kind === "INPUT_OBJECT") for (const x of d.inputFields) if (!given.has(x.name)) items.push({ label: x.name, detail: typeText(x.type), doc: x.description, kind: "input", insert: JSON.stringify(x.name) + ": ", required: required(x) });
        }
        return { from, to, word, items };
      }
      const t = f.arr ? itemOf(typeAt(n)) : n === 0 ? varType(f.key) : inputField(typeAt(n), f.key);
      const d = typeOf(schema, named(t));
      if (d && d.kind === "ENUM") for (const v of d.enumValues) items.push({ label: v.name, detail: d.name, kind: "enum", insert: JSON.stringify(v.name), doc: v.description, deprecated: v.isDeprecated });
      if (named(t) === "Boolean") items.push({ label: "true", kind: "atom", insert: "true" }, { label: "false", kind: "atom", insert: "false" });
      if (t && t.kind !== "NON_NULL") items.push({ label: "null", kind: "atom", insert: "null" });
      return { from, to, word, items };
    }
    let expectKey = false;
    for (const t of toks) {
      if (t.s >= cursor) return at(null, expectKey);
      if (t.e >= cursor && (t.k === "key" || t.k === "str")) return at(t, expectKey || t.k === "key");
      const f = stack[stack.length - 1];
      if (t.k === "p" && (t.v === "{" || t.v === "[")) { stack.push({ arr: t.v === "[", key: null, keys: [] }); expectKey = t.v === "{"; }
      else if (t.k === "p" && (t.v === "}" || t.v === "]")) { stack.pop(); expectKey = false; }
      else if (t.k === "p" && t.v === ",") expectKey = !!f && !f.arr;
      else if (t.k === "key") { if (f) { try { f.key = JSON.parse(src.slice(t.s, t.e)); } catch { f.key = null; } f.keys.push(f.key); } expectKey = false; }
    }
    return at(null, expectKey);
  }

  // ---- Highlighting ----------------------------------------------------

  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

  // paint is src as HTML: each token in a span of its role's class, the
  // marks ({s, e, cls}: errors, matching brackets) over what they cover.
  function paint(src, toks, roleOf, marks) {
    const cuts = new Set([0, src.length]);
    for (const t of toks) { cuts.add(t.s); cuts.add(t.e); }
    for (const m of marks) { cuts.add(m.s); cuts.add(m.e); }
    const at = [...cuts].sort((a, b) => a - b);
    let out = "", ti = 0;
    for (let n = 0; n + 1 < at.length; n++) {
      const s = at[n], e = at[n + 1];
      while (ti < toks.length && toks[ti].e <= s) ti++;
      const cls = [];
      if (ti < toks.length && toks[ti].s <= s) { const r = roleOf(ti); if (r) cls.push("t-" + r); }
      for (const m of marks) if (m.s <= s && m.e >= e && m.e > m.s) cls.push(m.cls);
      const text = esc(src.slice(s, e));
      out += cls.length ? `<span class="${cls.join(" ")}">${text}</span>` : text;
    }
    return out;
  }

  const EMPTY = { types: {}, directives: [], query: null };

  const api = {
    INTROSPECTION, makeSchema, typeText, named, parseTypeRef, lex, analyze, fuzzy, rank, applyItem,
    format, formatRange, sameToken, operationFor, lexJSON, checkJSON, jsonPaths, jsonContext, paint,
  };
  if (typeof module !== "undefined" && module.exports) { module.exports = api; return; }

  // ---- The editor --------------------------------------------------------

  const mac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  const mod = (e) => mac ? e.metaKey : e.ctrlKey;
  const modName = mac ? "⌘" : "Ctrl";
  const PAIRS = { "{": "}", "(": ")", "[": "]", '"': '"' };

  // Editor is a textarea over a highlighted copy of its text, the two in
  // step: the textarea keeps the browser's caret, selection, undo and
  // input methods; the copy shows tokens, matching brackets and marks.
  // Text is monospaced and never wraps, so a position's place is a
  // multiple of a character's size.
  class Editor {
    constructor(o) {
      this.o = o;
      const el = this.el = document.createElement("div");
      el.className = "ed " + (o.cls || "");
      el.innerHTML = `<div class="ed-gutter" aria-hidden="true"><div class="ed-nums"></div></div><div class="ed-main"><pre class="ed-hl" aria-hidden="true"></pre>` +
        `<textarea class="ed-ta" spellcheck="false" autocapitalize="off" autocomplete="off" wrap="off"></textarea><span class="ed-probe">${"x".repeat(100)}</span></div>`;
      this.ta = el.querySelector("textarea");
      this.hl = el.querySelector(".ed-hl");
      this.nums = el.querySelector(".ed-nums");
      this.probe = el.querySelector(".ed-probe");
      this.ta.setAttribute("aria-label", o.label || "Editor");
      if (o.placeholder) this.ta.placeholder = o.placeholder;
      this.ta.value = o.value || "";
      this.marks = [];
      this.lines = 0;
      const cursor = () => { this.paint(); o.onCursor && o.onCursor(); };
      this.ta.addEventListener("input", (e) => this.onInput(e));
      this.ta.addEventListener("keydown", (e) => this.onKey(e));
      this.ta.addEventListener("scroll", () => this.sync());
      this.ta.addEventListener("click", cursor);
      this.ta.addEventListener("keyup", (e) => { if (/^Arrow|Home|End|Page/.test(e.key)) cursor(); });
      this.ta.addEventListener("blur", () => setTimeout(() => { if (this.root().activeElement !== this.ta) this.closeList(); }, 150));
      this.ta.addEventListener("mousemove", (e) => this.onMove(e));
      this.ta.addEventListener("mouseleave", () => { clearTimeout(this.hoverT); o.hideTip && o.hideTip(); });
      this.ta.addEventListener("mousedown", (e) => {
        if (mod(e) && o.jump) { const p = this.posAt(e); if (p != null) { e.preventDefault(); o.jump(p); } }
      });
      this.paint();
    }
    root() { return this.el.getRootNode(); }
    get value() { return this.ta.value; }
    set value(v) { this.ta.value = v; this.closeList(); this.paint(); }
    focus() { this.ta.focus({ preventScroll: true }); }

    metrics() {
      if (!this.cw) {
        const w = this.probe.getBoundingClientRect().width;
        if (!w) return { cw: 7.2, lh: 18, pl: 10, pt: 8 };
        const cs = getComputedStyle(this.ta);
        this.cw = w / 100;
        this.lh = parseFloat(cs.lineHeight) || 18;
        this.pl = parseFloat(cs.paddingLeft) || 10;
        this.pt = parseFloat(cs.paddingTop) || 8;
      }
      return { cw: this.cw, lh: this.lh, pl: this.pl, pt: this.pt };
    }
    // coords is where offset pos is drawn, relative to the editor's box.
    coords(pos) {
      const v = this.ta.value, m = this.metrics();
      const start = v.lastIndexOf("\n", pos - 1) + 1;
      const line = (v.slice(0, start).match(/\n/g) || []).length;
      const main = this.ta.getBoundingClientRect(), box = this.el.getBoundingClientRect();
      return { x: main.left - box.left + m.pl + (pos - start) * m.cw - this.ta.scrollLeft, y: main.top - box.top + m.pt + line * m.lh - this.ta.scrollTop, lh: m.lh };
    }
    // posAt is the offset under a mouse event; null off the text.
    posAt(e) {
      const m = this.metrics(), r = this.ta.getBoundingClientRect();
      const line = Math.floor((e.clientY - r.top - m.pt + this.ta.scrollTop) / m.lh);
      const col = Math.floor((e.clientX - r.left - m.pl + this.ta.scrollLeft) / m.cw);
      const lines = this.ta.value.split("\n");
      if (line < 0 || line >= lines.length || col < 0 || col >= lines[line].length) return null;
      let pos = 0;
      for (let i = 0; i < line; i++) pos += lines[i].length + 1;
      return pos + col;
    }

    sync() {
      this.hl.style.transform = `translate(${-this.ta.scrollLeft}px, ${-this.ta.scrollTop}px)`;
      this.nums.style.transform = `translateY(${-this.ta.scrollTop}px)`;
      if (this.list) this.placeList();
    }
    paint() {
      const v = this.ta.value;
      const n = (v.match(/\n/g) || []).length + 1;
      if (n !== this.lines) {
        this.lines = n;
        let s = "";
        for (let i = 1; i <= n; i++) s += i + "\n";
        this.nums.textContent = s;
      }
      const caret = this.ta.selectionStart === this.ta.selectionEnd && this.root().activeElement === this.ta ? this.ta.selectionStart : -1;
      this.hl.innerHTML = this.o.paint(v, this.marks, caret) + "\n";
      this.sync();
    }
    setMarks(marks) { this.marks = marks; this.paint(); }

    // replace puts text over [from, to) as one undo step, and the
    // selection after it at [s, e].
    replace(from, to, text, s, e) {
      const ta = this.ta;
      this.applying = true;
      ta.focus({ preventScroll: true });
      ta.setSelectionRange(from, to);
      let ok = false;
      try { ok = document.execCommand(text ? "insertText" : "delete", false, text); } catch {}
      if (!ok || ta.value.slice(from, from + text.length) !== text) {
        ta.setRangeText(text, from, to, "end");
        ta.dispatchEvent(new Event("input", { bubbles: true }));
      }
      this.applying = false;
      const ss = s == null ? from + text.length : s;
      ta.setSelectionRange(ss, e == null ? ss : e);
      this.paint();
      this.o.onChange && this.o.onChange();
    }
    // setText makes the text text by replacing only what differs: one undo
    // step, the scroll kept.
    setText(text, s, e) {
      const old = this.ta.value;
      let a = 0;
      while (a < old.length && a < text.length && old[a] === text[a]) a++;
      let b = 0;
      while (b < old.length - a && b < text.length - a && old[old.length - 1 - b] === text[text.length - 1 - b]) b++;
      if (a === old.length && a === text.length) { this.ta.setSelectionRange(s, e == null ? s : e); this.paint(); return; }
      this.replace(a, old.length - b, text.slice(a, text.length - b), s, e);
    }

    onInput(e) {
      this.paint();
      if (this.applying) return;
      this.o.onChange && this.o.onChange();
      if (!this.o.complete) return;
      const c = this.ta.value[this.ta.selectionStart - 1];
      if (e.inputType && e.inputType.startsWith("insert") && c && /[\w$@"]/.test(c)) this.openList(false);
      else if (this.list) this.openList(false, true);
    }

    // ---- completion at the cursor
    openList(force, refine) {
      const pos = this.ta.selectionStart;
      if (pos !== this.ta.selectionEnd) return this.closeList();
      const ctx = this.o.complete(this.ta.value, pos);
      if (!ctx || !force && !refine && !/[\w$@]/.test(ctx.word)) return this.closeList();
      const word = ctx.word.replace(/^"/, "");
      const items = rank(ctx.items, word);
      if (!items.length || !force && items.length === 1 && items[0].label === word) return this.closeList();
      this.list = { ctx, items, at: 0 };
      if (!this.listEl) {
        this.listEl = document.createElement("div");
        this.listEl.className = "ed-list";
        this.listEl.addEventListener("mousedown", (ev) => {
          const li = ev.target.closest("[data-i]");
          ev.preventDefault();
          if (li) this.accept(+li.dataset.i);
        });
        this.el.appendChild(this.listEl);
      }
      const hi = (label, matched) => [...label].map((ch, i) => matched.includes(i) ? `<b>${esc(ch)}</b>` : esc(ch)).join("");
      this.listEl.innerHTML = `<ul role="listbox" aria-label="Completions">${items.slice(0, 200).map((x, i) => `<li role="option" data-i="${i}" class="k-${x.kind}${x.deprecated ? " dep" : ""}">` +
        `<span class="ed-icon" aria-hidden="true"></span><code class="ed-label">${hi(x.label, x.matched)}</code>${x.required ? '<span class="req" title="required">!</span>' : ""}` +
        `<span class="ed-detail">${esc(x.detail || "")}</span></li>`).join("")}</ul><div class="ed-doc"></div>`;
      this.listEl.hidden = false;
      this.mark();
      this.placeList();
    }
    placeList() {
      if (!this.list) return;
      const c = this.coords(this.list.ctx.from), box = this.el.getBoundingClientRect();
      const w = Math.min(460, box.width - 8);
      this.listEl.style.width = w + "px";
      this.listEl.style.left = Math.max(4, Math.min(c.x - 24, box.width - w - 4)) + "px";
      const below = c.y + c.lh + 2, h = this.listEl.offsetHeight;
      this.listEl.style.top = (below + h > box.height && c.y - h - 2 > 0 ? c.y - h - 2 : below) + "px";
    }
    mark() {
      const { at, items } = this.list;
      for (const li of this.listEl.querySelectorAll("li")) li.setAttribute("aria-selected", String(+li.dataset.i === at));
      const cur = this.listEl.querySelector(`li[data-i="${at}"]`);
      if (cur) cur.scrollIntoView({ block: "nearest" });
      const it = items[at], doc = this.listEl.querySelector(".ed-doc");
      doc.innerHTML = it && (it.doc || it.deprecated) ? (it.deprecated ? `<span class="dep-tag">deprecated</span> ` : "") + esc(it.doc || "") : "";
      doc.hidden = !doc.innerHTML;
    }
    closeList() { this.list = null; if (this.listEl) this.listEl.hidden = true; }
    accept(i) {
      const l = this.list;
      if (!l || !l.items[i]) return;
      this.closeList();
      const r = this.o.apply(this.ta.value, l.ctx, l.items[i]);
      this.setText(r.text, r.cursor);
      if (r.retrigger) setTimeout(() => this.openList(true), 0);
    }

    onKey(e) {
      const ta = this.ta, v = ta.value, s = ta.selectionStart, en = ta.selectionEnd;
      if (this.list) {
        const n = Math.min(this.list.items.length, 200);
        const move = (to) => { e.preventDefault(); this.list.at = to; this.mark(); };
        if (e.key === "ArrowDown") return move((this.list.at + 1) % n);
        if (e.key === "ArrowUp") return move((this.list.at + n - 1) % n);
        if (e.key === "PageDown") return move(Math.min(n - 1, this.list.at + 8));
        if (e.key === "PageUp") return move(Math.max(0, this.list.at - 8));
        if ((e.key === "Enter" || e.key === "Tab") && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) { e.preventDefault(); return this.accept(this.list.at); }
        if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); return this.closeList(); }
        if (/^(ArrowLeft|ArrowRight|Home|End)$/.test(e.key)) this.closeList();
      }
      if (this.o.keys && this.o.keys(e, this)) return;
      if (e.key === " " && e.ctrlKey && !e.altKey && this.o.complete) { e.preventDefault(); return this.openList(true); }
      if (e.key === "Escape" && this.o.onEscape) { this.o.onEscape(e); return; }
      if (e.key === "/" && mod(e) && this.o.comment) { e.preventDefault(); return this.toggleComment(); }
      if (e.altKey || e.metaKey || e.ctrlKey) return;
      const before = v[s - 1], after = v[s];
      if (e.key === "Tab") {
        e.preventDefault();
        if (e.shiftKey || s !== en && v.slice(s, en).includes("\n")) return this.indentLines(e.shiftKey ? -1 : 1);
        return this.replace(s, en, "  ");
      }
      if (e.key === "Enter") {
        e.preventDefault();
        const ind = lineIndent(v, s), opener = !!before && "{([".includes(before);
        if (opener && after === PAIRS[before]) return this.replace(s, en, "\n" + ind + "  \n" + ind, s + ind.length + 3);
        return this.replace(s, en, "\n" + ind + (opener ? "  " : ""));
      }
      if (e.key === "Backspace" && s === en && before && after && PAIRS[before] === after) { e.preventDefault(); return this.replace(s - 1, s + 1, ""); }
      if (e.key.length !== 1) return;
      const k = e.key;
      // typing a closer over the one auto-close put there
      if ((k === "}" || k === ")" || k === "]" || k === '"') && after === k && s === en) {
        if (k !== '"' || this.inString(s)) { e.preventDefault(); ta.setSelectionRange(s + 1, s + 1); this.paint(); return; }
      }
      if (PAIRS[k] && (s !== en || !after || /[\s)\]},:]/.test(after))) {
        if (k === '"' && (this.inString(s) || /\w/.test(before || ""))) return;
        e.preventDefault();
        return this.replace(s, en, k + v.slice(s, en) + PAIRS[k], s + 1, en + 1);
      }
      // a closer on a line of spaces goes back a level
      if ((k === "}" || k === ")" || k === "]") && s === en) {
        const ls = v.lastIndexOf("\n", s - 1) + 1;
        if (s - ls >= 2 && /^ +$/.test(v.slice(ls, s))) { e.preventDefault(); return this.replace(s - 2, s, k); }
      }
    }
    inString(pos) {
      const t = (this.o.lexer || lex)(this.ta.value).find((x) => x.s < pos && pos < x.e);
      return !!t && (t.k === "str" || t.k === "key");
    }
    indentLines(dir) {
      const ta = this.ta, v = ta.value, s = ta.selectionStart, e = ta.selectionEnd;
      const ls = v.lastIndexOf("\n", s - 1) + 1, le = v.indexOf("\n", Math.max(s, e - 1));
      const end = le < 0 ? v.length : le;
      const block = v.slice(ls, end).split("\n").map((l) => dir > 0 ? (l ? "  " + l : l) : l.replace(/^ {1,2}/, "")).join("\n");
      this.replace(ls, end, block, ls, ls + block.length);
    }
    toggleComment() {
      const ta = this.ta, v = ta.value, s = ta.selectionStart, e = ta.selectionEnd;
      const ls = v.lastIndexOf("\n", s - 1) + 1, le = v.indexOf("\n", Math.max(s, e - (e > s ? 1 : 0)));
      const end = le < 0 ? v.length : le;
      const lines = v.slice(ls, end).split("\n");
      const full = lines.filter((l) => l.trim());
      const all = full.length > 0 && full.every((l) => /^\s*#/.test(l));
      const ind = Math.min(...full.map((l) => /^ */.exec(l)[0].length), 1e9);
      const out = lines.map((l) => !l.trim() ? l : all ? l.replace(/^(\s*)# ?/, "$1") : l.slice(0, ind) + "# " + l.slice(ind)).join("\n");
      if (s === e) this.replace(ls, end, out, Math.max(ls, s + out.length - (end - ls)));
      else this.replace(ls, end, out, ls, ls + out.length);
    }

    onMove(e) {
      if (!this.o.hover || e.buttons) return;
      clearTimeout(this.hoverT);
      const p = this.posAt(e);
      if (p == null) { this.o.hideTip(); return; }
      this.hoverT = setTimeout(() => {
        const html = this.o.hover(this.ta.value, p);
        if (!html) return this.o.hideTip();
        const c = this.coords(p);
        this.o.showTip(html, this.el, c.x, c.y, c.lh);
      }, 200);
    }
  }

  // jsonTree is a result as HTML: each object and array foldable, long
  // arrays folded.
  function jsonTree(v, key, last, depth) {
    const k = key == null ? "" : `<span class="t-key">${esc(JSON.stringify(key))}</span>: `;
    const comma = last ? "" : ",";
    if (v === null || typeof v !== "object") {
      const cls = v === null || typeof v === "boolean" ? "t-atom" : typeof v === "number" ? "t-num" : "t-str";
      return `<div class="jl">${k}<span class="${cls}">${esc(JSON.stringify(v))}</span>${comma}</div>`;
    }
    const arr = Array.isArray(v), entries = arr ? v.map((x) => [null, x]) : Object.entries(v);
    const [o, c] = arr ? ["[", "]"] : ["{", "}"];
    if (!entries.length) return `<div class="jl">${k}${o}${c}${comma}</div>`;
    const folded = depth > 0 && arr && entries.length > 50;
    return `<div class="jn${folded ? " folded" : ""}"><div class="jl"><button type="button" class="jt" aria-label="Fold" tabindex="-1"></button>${k}${o}` +
      `<span class="jsum"> ${entries.length} ${arr ? "items" : "keys"} ${c}${comma}</span></div><div class="jc">` +
      entries.map(([kk, x], i) => jsonTree(x, kk, i === entries.length - 1, depth + 1)).join("") + `</div><div class="jl jend">${c}${comma}</div></div>`;
  }

  // ---- The panel -----------------------------------------------------------

  // create makes the panel: o.endpoint, o.root (the shadow root its styles
  // go in), o.shell (the toolbar's element, which full screen widens),
  // o.fetch (a request the toolbar lists), o.fetch0 (one it doesn't),
  // o.check (the toolbar's check URL), o.header, o.trace(id).
  function create(o) {
    if (!o.root.querySelector("style[data-gql]")) {
      const st = document.createElement("style");
      st.dataset.gql = "";
      st.textContent = CSS;
      o.root.appendChild(st);
    }
    const store = (() => { try { return window.localStorage; } catch { return null; } })();
    const key = "vk-graphiql:" + o.endpoint + ":";
    const load = (k, d) => { try { const v = store && store.getItem(key + k); return v == null ? d : JSON.parse(v); } catch { return d; } };
    const save = (k, v) => { try { store && store.setItem(key + k, JSON.stringify(v)); } catch {} };

    const el = document.createElement("div");
    el.className = "gql";
    el.innerHTML = `<div class="gql-bar">
        <button type="button" class="btn primary gql-run" title="Run the operation at the cursor (${modName}-Enter)">▶ Run</button>
        <select class="gql-op" aria-label="Operation to run" hidden></select>
        <button type="button" class="btn gql-format" title="Format the document, or the definitions selected (Shift-${modName}-P · Shift-Alt-F)">Format</button>
        <button type="button" class="btn gql-curl" title="Copy the request as a curl command">Copy as curl</button>
        <span class="gql-status" role="status"></span>
        <button type="button" class="linkish gql-trace" hidden>Show its trace</button>
        <span class="gql-schema dim"></span>
        <button type="button" class="btn gql-side-btn" aria-pressed="false" title="The schema's operations, its docs, and the history">Explorer</button>
        <button type="button" class="btn gql-full-btn" aria-pressed="false"></button>
      </div>
      <div class="gql-tabs" role="tablist" aria-label="Documents"></div>
      <div class="gql-main">
        <div class="gql-left">
          <div class="gql-q"></div>
          <div class="gql-problems" hidden></div>
          <div class="gql-sub">
            <div class="gql-subtabs" role="tablist"><button type="button" role="tab" data-sub="vars" aria-selected="true">Variables</button><button type="button" role="tab" data-sub="headers" aria-selected="false">Headers</button><span class="gql-sub-err"></span></div>
            <div class="gql-vars"></div><div class="gql-headers" hidden></div>
          </div>
        </div>
        <div class="gql-right"><div class="gql-errors" hidden></div><div class="gql-result" aria-label="Result"></div></div>
        <aside class="gql-side" hidden aria-label="Explorer">
          <div class="gql-side-tabs" role="tablist"><button type="button" role="tab" data-side="ops">Operations</button><button type="button" role="tab" data-side="docs">Docs</button><button type="button" role="tab" data-side="history">History</button></div>
          <input class="gql-search" type="search" aria-label="Search">
          <div class="gql-side-body"></div>
        </aside>
      </div>
      <div class="gql-tip" role="tooltip" hidden></div>`;
    const $ = (s) => el.querySelector(s);
    const status = $(".gql-status"), traceBtn = $(".gql-trace"), schemaEl = $(".gql-schema");
    const resultEl = $(".gql-result"), errorsEl = $(".gql-errors"), problemsEl = $(".gql-problems"), tip = $(".gql-tip");
    const side = $(".gql-side"), sideBody = $(".gql-side-body"), search = $(".gql-search"), opSelect = $(".gql-op");
    const hint = () => { resultEl.innerHTML = `<p class="dim gql-hint">Run: ${modName}-Enter · complete: Ctrl-Space · format: Shift-${modName}-P · full screen: Shift-${modName}-F</p>`; };
    hint();
    let schema = null, cache = { src: null }, lastID = "", checkSeq = 0, checkT = 0;
    // analysis is the query's analysis, kept while its text is the same.
    const analysis = (v) => cache.src === v && cache.schema === schema ? cache : (cache = { src: v, schema, ...analyze(v, schema || EMPTY) });

    // ---- documents in tabs
    let state = load("tabs", null);
    if (!state || !Array.isArray(state.tabs) || !state.tabs.length) state = { tabs: [{ id: 1, query: "{\n  \n}\n", variables: "", headers: "" }], active: 1, seq: 1 };
    const tabOf = () => state.tabs.find((t) => t.id === state.active) || state.tabs[0];
    const persist = () => save("tabs", state);

    // ---- editors
    function showTip(html, host, x, y, lh) {
      tip.innerHTML = html;
      tip.hidden = false;
      const hb = host.getBoundingClientRect(), b = el.getBoundingClientRect();
      const left = hb.left - b.left + x, top = hb.top - b.top + y;
      tip.style.left = Math.max(4, Math.min(left, b.width - tip.offsetWidth - 4)) + "px";
      tip.style.top = (top - tip.offsetHeight - 4 > 0 ? top - tip.offsetHeight - 4 : top + lh + 4) + "px";
    }
    const hideTip = () => { tip.hidden = true; };

    const q = new Editor({
      label: "Query", cls: "gql-query", value: tabOf().query, comment: true,
      paint: (v, marks, caret) => {
        const a = analysis(v), m = marks.slice();
        if (caret >= 0) {
          const i = a.toks.findIndex((t) => t.k === "p" && "{}()[]".includes(t.v) && (t.s === caret || t.e === caret));
          if (i >= 0 && a.pairs.has(i)) { const j = a.pairs.get(i); m.push({ s: a.toks[i].s, e: a.toks[i].e, cls: "match" }, { s: a.toks[j].s, e: a.toks[j].e, cls: "match" }); }
        }
        return paint(v, a.toks, (i) => a.roles[i], m);
      },
      complete: (v, pos) => schema ? analyze(v, schema, pos).ctx : null,
      apply: (v, ctx, item) => applyItem(v, ctx, item, schema),
      hover: (v, pos) => hoverAt(v, pos), showTip, hideTip,
      jump: (pos) => { const it = infoAt(q.value, pos); if (it) openDocsFor(it); },
      onChange: () => { tabOf().query = q.value; tabOf().title = ""; persist(); scheduleCheck(); drawOps(); drawTabs(); },
      onCursor: () => { drawOps(); },
      keys: (e) => commonKeys(e), onEscape: (e) => leaveFull(e),
    });
    $(".gql-q").appendChild(q.el);

    const varDefs = () => { const op = opAtCursor(); return op ? op.vars : []; };
    const jsonEditor = (label, cls, value, onChange, complete) => new Editor({
      label, cls, value, lexer: lexJSON, placeholder: "{ }",
      paint: (v, marks) => { const ts = lexJSON(v); return paint(v, ts, (i) => ts[i].k, marks); },
      complete, apply: (v, ctx, item) => ({ text: v.slice(0, ctx.from) + item.insert + v.slice(ctx.to), cursor: ctx.from + item.insert.length }),
      onChange, keys: (e) => commonKeys(e), onEscape: (e) => leaveFull(e),
    });
    const vars = jsonEditor("Variables", "gql-vars-ed", tabOf().variables, () => { tabOf().variables = vars.value; persist(); scheduleCheck(); },
      (v, pos) => schema ? jsonContext(v, pos, varDefs(), schema) : null);
    const headers = jsonEditor("Headers", "gql-headers-ed", tabOf().headers, () => { tabOf().headers = headers.value; persist(); checkHeaders(); }, null);
    $(".gql-vars").appendChild(vars.el);
    $(".gql-headers").appendChild(headers.el);
    for (const b of el.querySelectorAll("[data-sub]")) b.addEventListener("click", () => {
      for (const x of el.querySelectorAll("[data-sub]")) x.setAttribute("aria-selected", String(x === b));
      $(".gql-vars").hidden = b.dataset.sub !== "vars";
      $(".gql-headers").hidden = b.dataset.sub !== "headers";
    });

    // ---- the keys every part of the panel shares
    function commonKeys(e) {
      const k = e.key.length === 1 ? e.key.toLowerCase() : e.key;
      if (mod(e) && k === "Enter" && !e.shiftKey) { e.preventDefault(); run(); return true; }
      if (mod(e) && e.shiftKey && k === "p") { e.preventDefault(); formatFocused(); return true; }
      if (e.altKey && e.shiftKey && !mod(e) && e.code === "KeyF") { e.preventDefault(); formatFocused(); return true; }
      if (mod(e) && e.shiftKey && (k === "f" || e.code === "KeyF")) { e.preventDefault(); toggleFull(); return true; }
      return false;
    }
    el.addEventListener("keydown", (e) => {
      if (e.target.closest(".ed")) return;
      if (commonKeys(e)) return;
      if (e.key === "Escape" && e.target !== search) leaveFull(e);
    });

    // ---- the schema
    async function loadSchema() {
      schemaEl.textContent = "reading the schema…";
      const r = await o.fetch0(o.endpoint, { method: "POST", headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ query: INTROSPECTION, operationName: "VukaToolbarSchema" }) }).then((x) => x.json()).catch(() => null);
      const s = r && r.data && r.data.__schema;
      if (!s) { schemaEl.textContent = "no schema: " + ((r && r.errors && r.errors[0].message) || "the app isn't answering"); return; }
      schema = makeSchema(s);
      searchIndex = null;
      const n = Object.keys(schema.types).filter((x) => !x.startsWith("__")).length;
      schemaEl.textContent = n + " types";
      schemaEl.title = "Read by introspection; read again when the app rebuilds";
      el.dataset.schema = "ready";
      q.paint();
      drawSide();
      drawOps();
      scheduleCheck(0);
    }

    // ---- the operations in the document
    function opAtCursor() {
      const a = analysis(q.value), pos = q.ta.selectionStart;
      if (!a.ops.length) return null;
      return a.ops.find((x) => x.s <= pos && pos <= x.e) || a.ops.filter((x) => x.s <= pos).pop() || a.ops[0];
    }
    function drawOps() {
      const a = analysis(q.value);
      opSelect.hidden = a.ops.length < 2;
      if (opSelect.hidden) return;
      const keep = opSelect.value, ops = a.ops.filter((x) => x.name);
      const html = `<option value="">the one at the cursor</option>` + ops.map((x) => `<option value="${esc(x.name)}">${esc(x.kind + " " + x.name)}</option>`).join("");
      if (opSelect.dataset.html !== html) { opSelect.innerHTML = html; opSelect.dataset.html = html; opSelect.value = ops.some((x) => x.name === keep) ? keep : ""; }
    }

    // ---- the app's checks as the query is typed
    function scheduleCheck(delay) {
      clearTimeout(checkT);
      checkT = setTimeout(check, delay == null ? 220 : delay);
    }
    function locPos(text, line, col) {
      let pos = 0;
      for (let l = 1; l < line; l++) { const n = text.indexOf("\n", pos); if (n < 0) return text.length; pos = n + 1; }
      return Math.min(text.length, pos + col - 1);
    }
    function spanAt(text, pos) {
      const t = lex(text).find((x) => x.s <= pos && pos < x.e);
      return t ? { s: t.s, e: t.e } : { s: Math.max(0, Math.min(pos, text.length - 1)), e: Math.min(text.length, pos + 1) };
    }
    async function check() {
      const text = q.value, vtext = vars.value;
      const varErr = checkJSON(vtext);
      const vmarks = varErr ? [{ s: varErr.pos, e: Math.max(varErr.end, varErr.pos + 1), cls: "squiggle", msg: varErr.message }] : [];
      if (!text.trim() || !schema) { q.setMarks([]); vars.setMarks(vmarks); drawProblems([], vmarks); return; }
      let variables = {};
      if (vtext.trim() && !varErr) { try { variables = JSON.parse(vtext); } catch {} }
      if (!variables || typeof variables !== "object" || Array.isArray(variables)) variables = {};
      const op = opAtCursor();
      const seq = ++checkSeq;
      const r = await o.fetch0(o.check, { method: "POST", headers: { "Content-Type": "application/json", [o.header]: "1" },
        body: JSON.stringify({ query: text, operationName: (op && op.name) || undefined, variables: vtext.trim() ? variables : undefined }) })
        .then((x) => x.ok ? x.json() : null).catch(() => null);
      if (seq !== checkSeq || q.value !== text || vars.value !== vtext) return;
      const qmarks = [];
      for (const e of (r && r.errors) || []) {
        const vm = /^Variable "\$(\w+)"/.exec(e.message);
        if (vm && /got invalid value|was not provided|must not be null/.test(e.message)) {
          // the variables' values: marked in their JSON
          const at = /at "([^"]+)"/.exec(e.message), path = at ? at[1] : vm[1];
          const ps = jsonPaths(vtext);
          const hit = ps.find((p) => p.path === path && !p.key) || ps.find((p) => p.path === path) || ps.find((p) => p.path === vm[1]);
          vmarks.push(hit ? { s: hit.s, e: hit.e, cls: "squiggle", msg: e.message } : { s: 0, e: Math.min(1, vtext.length), cls: "squiggle", msg: e.message });
          continue;
        }
        const loc = e.locations && e.locations[0];
        const sp = loc ? spanAt(text, locPos(text, loc.line, loc.column)) : { s: 0, e: 0 };
        qmarks.push({ ...sp, cls: "squiggle", msg: e.message, line: loc && loc.line, col: loc && loc.column });
      }
      q.setMarks(qmarks);
      vars.setMarks(vmarks);
      drawProblems(qmarks, vmarks);
    }
    function checkHeaders() {
      const err = checkJSON(headers.value);
      headers.setMarks(err ? [{ s: err.pos, e: Math.max(err.end, err.pos + 1), cls: "squiggle", msg: err.message }] : []);
      subErr();
    }
    let varProblems = [];
    function subErr() {
      const h = checkJSON(headers.value);
      const msgs = varProblems.map((m) => "Variables: " + m.msg).concat(h ? ["Headers: " + h.message] : []);
      const se = $(".gql-sub-err");
      se.textContent = msgs[0] || "";
      se.title = msgs.join("\n");
    }
    function drawProblems(qm, vm) {
      varProblems = vm;
      subErr();
      problemsEl.marks = qm;
      problemsEl.hidden = !qm.length;
      problemsEl.innerHTML = qm.length ? `<ul>${qm.map((m, i) => {
        const dym = /Did you mean (.+)\?/.exec(m.msg);
        const fixes = dym ? [...dym[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]) : [];
        return `<li><button type="button" class="linkish gql-goto" data-i="${i}">${m.line ? m.line + ":" + m.col : "—"}</button> ${esc(m.msg)}` +
          fixes.map((f) => ` <button type="button" class="btn small gql-fix" data-i="${i}" data-fix="${esc(f)}">use ${esc(f)}</button>`).join("") + `</li>`;
      }).join("")}</ul>` : "";
    }
    problemsEl.addEventListener("click", (e) => {
      const b = e.target.closest(".gql-goto, .gql-fix"), m = b && problemsEl.marks[+b.dataset.i];
      if (!m) return;
      if (b.classList.contains("gql-fix")) return q.replace(m.s, m.e, b.dataset.fix);
      q.focus();
      q.ta.setSelectionRange(m.s, m.e);
      q.paint();
    });

    // ---- hover
    function infoAt(v, pos) {
      if (!schema) return null;
      const a = analysis(v), i = a.toks.findIndex((t) => t.s <= pos && pos < t.e);
      return i >= 0 ? a.info[i] : null;
    }
    function hoverAt(v, pos) {
      const parts = q.marks.filter((m) => m.s <= pos && pos < Math.max(m.e, m.s + 1)).map((m) => `<p class="tip-err">${esc(m.msg)}</p>`);
      const it = infoAt(v, pos), d = it && describe(it);
      if (d) parts.push(d + `<p class="tip-hint dim">${modName}-click opens it in the docs</p>`);
      return parts.join("");
    }
    const sig = (f) => f.args && f.args.length ? "(" + f.args.map((a) => a.name + ": " + typeText(a.type)).join(", ") + ")" : "";
    function describe(it) {
      const dep = (x) => x && x.isDeprecated ? `<p class="dep-tag">deprecated${x.deprecationReason ? ": " + esc(x.deprecationReason) : ""}</p>` : "";
      const doc = (d) => d ? `<p>${esc(d)}</p>` : "";
      switch (it.kind) {
        case "field": return `<code>${esc(it.parent)}.<b>${esc(it.def.name)}</b>${esc(sig(it.def))}: ${esc(typeText(it.def.type))}</code>${doc(it.def.description)}${dep(it.def)}`;
        case "arg": case "input": return `<code><b>${esc(it.def.name)}</b>: ${esc(typeText(it.def.type))}${it.def.defaultValue != null ? " = " + esc(it.def.defaultValue) : ""}</code>${doc(it.def.description)}`;
        case "type": { const t = schema.types[it.name]; return t ? `<code>${esc(t.kind.toLowerCase().replace("_", " "))} <b>${esc(t.name)}</b></code>${doc(t.description)}` : ""; }
        case "enum": { const t = schema.types[it.type], v = t && t.enumValues.find((x) => x.name === it.name); return `<code>${esc(it.type)}.<b>${esc(it.name)}</b></code>${doc(v && v.description)}${dep(v)}`; }
        case "dir": return it.def ? `<code><b>@${esc(it.def.name)}</b>${esc(sig(it.def))}</code>${doc(it.def.description)}` : "";
        case "var": return `<code><b>$${esc(it.name)}</b>${it.type ? ": " + esc(typeText(it.type)) : ""}</code>`;
      }
      return "";
    }

    // ---- running
    function request() {
      const vErr = checkJSON(vars.value);
      if (vErr) return { error: "The variables aren't JSON: " + vErr.message };
      const variables = vars.value.trim() ? JSON.parse(vars.value) : undefined;
      if (variables !== undefined && (!variables || typeof variables !== "object" || Array.isArray(variables))) return { error: "The variables are no JSON object." };
      const hErr = checkJSON(headers.value);
      if (hErr) return { error: "The headers aren't JSON: " + hErr.message };
      const hdrs = headers.value.trim() ? JSON.parse(headers.value) : {};
      if (!hdrs || typeof hdrs !== "object" || Array.isArray(hdrs)) return { error: "The headers are no JSON object." };
      const a = analysis(q.value);
      let operationName = opSelect.hidden ? "" : opSelect.value;
      if (!operationName && a.ops.length > 1) {
        const op = opAtCursor();
        if (op && !op.name) return { error: "The document has several operations: name the one at the cursor to run it." };
        operationName = op ? op.name : "";
      }
      return { body: { query: q.value, variables, operationName: operationName || undefined }, headers: hdrs };
    }
    async function run() {
      const req = request();
      if (req.error) { errorsEl.hidden = true; resultEl.innerHTML = `<p class="err">${esc(req.error)}</p>`; return; }
      status.innerHTML = `<span class="spin"></span> running…`;
      traceBtn.hidden = true;
      const t0 = performance.now();
      let res, text;
      try {
        res = await o.fetch(o.endpoint, { method: "POST", headers: { ...req.headers, "Content-Type": "application/json", Accept: "application/graphql-response+json, application/json" }, body: JSON.stringify(req.body) });
        text = await res.text();
      } catch {
        status.textContent = "";
        resultEl.innerHTML = `<p class="err">The app isn't answering.</p>`;
        return;
      }
      const ms = Math.round(performance.now() - t0);
      let parsed = null;
      try { parsed = JSON.parse(text); } catch {}
      const tone = res.ok && parsed && !parsed.errors ? "ok" : parsed && parsed.data ? "warn" : "err";
      status.innerHTML = `<span class="st ${tone}">${res.status}</span> ${ms} ms · ${text.length < 1024 ? text.length + " B" : (text.length / 1024).toFixed(1) + " KB"}`;
      lastID = res.headers.get(o.header) || "";
      traceBtn.hidden = !lastID;
      drawResult(parsed, text);
      const op = req.body.operationName || (opAtCursor() && opAtCursor().name) || "";
      const h = load("history", []);
      const entry = { query: q.value, variables: vars.value, headers: headers.value, op, at: Date.now(), status: res.status, ms };
      if (h.length && h[0].query === entry.query && h[0].variables === entry.variables) h[0] = entry; else h.unshift(entry);
      save("history", h.slice(0, 50));
      if (sideTab === "history") drawSide();
    }
    function drawResult(parsed, text) {
      if (!parsed) { errorsEl.hidden = true; resultEl.innerHTML = `<pre class="raw">${esc(text)}</pre>`; return; }
      const errs = parsed.errors || [];
      errorsEl.hidden = !errs.length;
      errorsEl.innerHTML = errs.length ? `<ul>${errs.map((e) => {
        const loc = e.locations && e.locations[0], code = e.extensions && e.extensions.code;
        return `<li>${code ? `<span class="code">${esc(code)}</span> ` : ""}${esc(e.message)}` +
          (e.path ? ` <span class="dim">at</span> <code>${esc(e.path.join("."))}</code>` : "") +
          (loc ? ` <button type="button" class="linkish gql-loc" data-line="${loc.line}" data-col="${loc.column}">${loc.line}:${loc.column}</button>` : "") + `</li>`;
      }).join("")}</ul>` : "";
      resultEl.innerHTML = `<div class="jtree">${jsonTree(parsed, null, true, 0)}</div>`;
    }
    resultEl.addEventListener("click", (e) => { const t = e.target.closest(".jt"); if (t) t.closest(".jn").classList.toggle("folded"); });
    errorsEl.addEventListener("click", (e) => {
      const b = e.target.closest(".gql-loc");
      if (!b) return;
      const p = locPos(q.value, +b.dataset.line, +b.dataset.col);
      q.focus();
      q.ta.setSelectionRange(p, p);
      q.paint();
    });
    $(".gql-run").addEventListener("click", run);
    traceBtn.addEventListener("click", () => { if (lastID) o.trace(lastID); });

    // ---- formatting
    const lineCol = (v, pos) => { const b = v.slice(0, pos); return (b.match(/\n/g) || []).length + 1 + ":" + (pos - b.lastIndexOf("\n")); };
    let flashT = 0;
    function flash(msg) {
      status.innerHTML = `<span class="err">${esc(msg)}</span>`;
      clearTimeout(flashT);
      flashT = setTimeout(() => { if (status.textContent === msg) status.textContent = ""; }, 5000);
    }
    function formatQuery() {
      const v = q.value, s = q.ta.selectionStart, e = q.ta.selectionEnd;
      if (!v.trim()) return true;
      const r = s !== e ? formatRange(v, s, e) : { from: 0, to: v.length, ...format(v) };
      if (r.error) { flash("Can't format: " + r.error + " at " + lineCol(v, r.pos)); return false; }
      const text = v.slice(0, r.from) + r.text + v.slice(r.to);
      q.setText(text, sameToken(v, text, s), s === e ? undefined : sameToken(v, text, e));
      return true;
    }
    function formatJSON(ed, quiet) {
      const v = ed.value;
      if (!v.trim()) return;
      const err = checkJSON(v);
      if (err) { if (!quiet) flash("Can't format: " + err.message + " at " + lineCol(v, err.pos)); return; }
      const text = JSON.stringify(JSON.parse(v), null, 2);
      ed.setText(text, sameToken(v, text, ed.ta.selectionStart, lexJSON));
    }
    // formatFocused formats the variables or the headers when they have
    // the focus; else the query (or the definitions selected) and, when it
    // formats, the variables and headers with it.
    function formatFocused() {
      const a = o.root.activeElement;
      if (a === vars.ta) return formatJSON(vars);
      if (a === headers.ta) return formatJSON(headers);
      if (formatQuery()) { formatJSON(vars, true); formatJSON(headers, true); }
      q.focus(); // formatting the others moved the focus; the query's caret stays where formatting put it
    }
    $(".gql-format").addEventListener("mousedown", (e) => e.preventDefault()); // keep the focus where it is
    $(".gql-format").addEventListener("click", formatFocused);

    // ---- copy as curl
    $(".gql-curl").addEventListener("click", async () => {
      const req = request();
      if (req.error) { flash(req.error); return; }
      const sh = (s) => "'" + String(s).replace(/'/g, "'\\''") + "'";
      const cmd = ["curl " + sh(new URL(o.endpoint, location.href).href), "-H " + sh("Content-Type: application/json"),
        ...Object.entries(req.headers).map(([k, v]) => "-H " + sh(k + ": " + v)), "--data-raw " + sh(JSON.stringify(req.body))].join(" \\\n  ");
      el.dataset.curl = cmd;
      const b = $(".gql-curl");
      try { await navigator.clipboard.writeText(cmd); b.textContent = "Copied"; } catch { b.textContent = "Copy failed"; }
      setTimeout(() => { b.textContent = "Copy as curl"; }, 1400);
    });

    // ---- tabs
    const tabsEl = $(".gql-tabs");
    const titleOf = (t) => { const m = /\b(query|mutation|subscription)\s+(\w+)/.exec(t.query); return m ? m[2] : /\w/.test(t.query) ? "Untitled" : "New"; };
    function drawTabs() {
      const html = state.tabs.map((t) => `<span class="gql-tab" role="tab" aria-selected="${t.id === state.active}" data-id="${t.id}">` +
        `<button type="button" class="gql-tab-name">${esc(titleOf(t))}</button>` +
        (state.tabs.length > 1 ? `<button type="button" class="gql-tab-x" aria-label="Close ${esc(titleOf(t))}">×</button>` : "") + `</span>`).join("") +
        `<button type="button" class="gql-tab-add" aria-label="New tab" title="New tab">+</button>`;
      if (tabsEl.innerHTML !== html) tabsEl.innerHTML = html;
    }
    function switchTab(id) {
      state.active = id;
      const t = tabOf();
      q.value = t.query; vars.value = t.variables || ""; headers.value = t.headers || "";
      persist(); drawTabs(); drawOps(); checkHeaders(); scheduleCheck(0);
      hint(); errorsEl.hidden = true; status.textContent = ""; traceBtn.hidden = true;
    }
    function newTab(query, variables, headersText) {
      const id = state.seq = Math.max(state.seq || 0, ...state.tabs.map((t) => t.id)) + 1;
      state.tabs.push({ id, query: query == null ? "{\n  \n}\n" : query, variables: variables || "", headers: headersText == null ? tabOf().headers || "" : headersText });
      switchTab(id);
    }
    tabsEl.addEventListener("click", (e) => {
      if (e.target.closest(".gql-tab-add")) { newTab(); q.focus(); q.ta.setSelectionRange(4, 4); return; }
      const tab = e.target.closest(".gql-tab");
      if (!tab) return;
      const id = +tab.dataset.id;
      if (e.target.closest(".gql-tab-x")) {
        const i = state.tabs.findIndex((t) => t.id === id);
        state.tabs.splice(i, 1);
        if (state.active === id) switchTab(state.tabs[Math.max(0, i - 1)].id); else { persist(); drawTabs(); }
      } else if (id !== state.active) switchTab(id);
    });

    // ---- the explorer: operations, docs, history
    let sideTab = load("side", "ops"), sideOpen = !!load("sideOpen", false), docPath = [], searchIndex = null;
    function setSide(open) {
      sideOpen = open;
      side.hidden = !open;
      $(".gql-side-btn").setAttribute("aria-pressed", String(open));
      el.classList.toggle("with-side", open);
      save("sideOpen", open);
      if (open) drawSide();
    }
    $(".gql-side-btn").addEventListener("click", () => setSide(!sideOpen));
    for (const b of el.querySelectorAll("[data-side]")) b.addEventListener("click", () => { sideTab = b.dataset.side; save("side", sideTab); search.value = ""; drawSide(); });
    search.addEventListener("input", () => drawSide());
    search.addEventListener("keydown", (e) => { if (e.key === "Escape" && search.value) { e.stopPropagation(); search.value = ""; drawSide(); } });

    function drawSide() {
      for (const b of el.querySelectorAll("[data-side]")) b.setAttribute("aria-selected", String(b.dataset.side === sideTab));
      if (!sideOpen) return;
      search.placeholder = sideTab === "ops" ? "Filter the operations" : sideTab === "docs" ? "Search types and fields" : "Filter the history";
      if (sideTab === "history") return drawHistory();
      if (!schema) { sideBody.innerHTML = `<p class="dim">${esc(schemaEl.textContent)}</p>`; return; }
      if (sideTab === "ops") return drawOperations();
      drawDocs();
    }
    function drawOperations() {
      const term = search.value.trim();
      sideBody.innerHTML = [["Query", "query"], ["Mutation", "mutation"], ["Subscription", "subscription"]].map(([label, kind]) => {
        const t = schema.types[rootOf(schema, kind)];
        const fs = t ? t.fields.filter((f) => !term || fuzzy(term, f.name)) : [];
        return `<section class="gql-ops" data-kind="${kind}"><h5>${label} <span class="count">${fs.length}</span></h5>` + (!t ? `<p class="dim">The schema has no ${kind}.</p>` :
          `<ul>${fs.map((f) => `<li class="gql-opitem${f.isDeprecated ? " dep" : ""}" data-kind="${kind}" data-field="${esc(f.name)}">` +
            `<code class="gql-opsig"><b>${esc(f.name)}</b>${esc(sig(f))}: <span class="t-type">${esc(typeText(f.type))}</span></code>` +
            (f.isDeprecated ? ` <span class="dep-tag">deprecated</span>` : "") + (f.description ? `<p class="dim">${esc(f.description)}</p>` : "") +
            (kind === "subscription" ? `<p class="note-sm">The panel can't run a subscription: the endpoint answers queries and mutations over HTTP.</p>` : "") +
            `<div class="gql-opbtns"><button type="button" class="btn small gql-op-insert" title="Insert it at the cursor">Insert</button><button type="button" class="btn small gql-op-tab">New tab</button></div></li>`).join("")}</ul>`) + `</section>`;
      }).join("");
    }
    sideBody.addEventListener("click", (e) => {
      const item = e.target.closest(".gql-opitem");
      if (item && e.target.closest(".gql-op-insert, .gql-op-tab")) {
        const t = schema.types[rootOf(schema, item.dataset.kind)], f = t && t.fields.find((x) => x.name === item.dataset.field);
        if (f) insertOperation(item.dataset.kind, f, !!e.target.closest(".gql-op-tab"));
        return;
      }
      const link = e.target.closest("[data-type]");
      if (link) { e.preventDefault(); docPath.push({ type: link.dataset.type, field: link.dataset.fieldname || null }); search.value = ""; drawDocs(); return; }
      if (e.target.closest(".gql-back")) { docPath.pop(); drawDocs(); return; }
      const ins = e.target.closest(".gql-insert-field");
      if (ins) { insertField(ins.dataset.type, ins.dataset.name); return; }
      const h = e.target.closest(".gql-hist");
      if (h) { const x = load("history", [])[+h.dataset.i]; if (x) newTab(x.query, x.variables, x.headers); return; }
      if (e.target.closest(".gql-hist-clear")) { save("history", []); drawHistory(); }
    });
    // insertOperation puts a ready-to-run operation of root field f in a
    // new tab, or at the cursor (after the definition the cursor is in),
    // its required variables into the variables, the cursor on its
    // selection.
    function insertOperation(kind, f, inTab) {
      const gen = operationFor(schema, kind, f);
      if (inTab) {
        newTab(gen.text, Object.keys(gen.variables).length ? JSON.stringify(gen.variables, null, 2) : "");
        q.focus();
        q.ta.setSelectionRange(gen.sel, gen.sel);
        q.paint();
        return;
      }
      const v = q.value, pos = q.ta.selectionStart, a = analysis(v);
      if (!v.trim() || /^\s*\{\s*\}\s*$/.test(v)) q.setText(gen.text, gen.sel);
      else {
        const inside = [...a.ops, ...a.frags].find((x) => x.s < pos && pos < x.e);
        const at = inside ? inside.e : pos, before = v.slice(0, at);
        const pre = !before.trim() ? "" : before.endsWith("\n\n") ? "" : before.endsWith("\n") ? "\n" : "\n\n";
        const post = v.slice(at).trim() && !v.slice(at).startsWith("\n\n") ? "\n" : "";
        q.replace(at, at, pre + gen.text + post, at + pre.length + gen.sel);
      }
      q.focus();
      if (Object.keys(gen.variables).length) {
        let cur = {};
        if (vars.value.trim()) { if (checkJSON(vars.value)) return; cur = JSON.parse(vars.value); }
        if (!cur || typeof cur !== "object" || Array.isArray(cur)) return;
        for (const [k, x] of Object.entries(gen.variables)) if (!(k in cur)) cur[k] = x;
        vars.setText(JSON.stringify(cur, null, 2), 0);
        q.focus();
      }
    }
    function insertField(typeName, name) {
      const v = q.value, pos = q.ta.selectionStart;
      const t = schema.types[typeName], f = t && t.fields.find((x) => x.name === name);
      const pre = pos > 0 && !/\s/.test(v[pos - 1]) && !/[{(]/.test(v[pos - 1]) ? " " : "";
      const r = applyItem(v, { from: pos, to: pos, word: "" }, { kind: "field", field: f || null, insert: pre + name }, schema);
      q.setText(r.text, r.cursor);
      q.focus();
    }
    const link = (t) => { const n = named(t); return esc(typeText(t)).replace(esc(n), `<a href="#" data-type="${esc(n)}" class="t-type">${esc(n)}</a>`); };
    function buildIndex() {
      const out = [];
      for (const t of Object.values(schema.types)) {
        if (t.name.startsWith("__")) continue;
        out.push({ label: t.name, type: t.name, kind: t.kind.toLowerCase().replace("_", " ") });
        for (const f of t.fields) out.push({ label: t.name + "." + f.name, name: f.name, type: t.name, field: f.name, detail: typeText(f.type) });
        for (const f of t.inputFields) out.push({ label: t.name + "." + f.name, name: f.name, type: t.name, field: f.name, detail: typeText(f.type) });
        for (const v of t.enumValues) out.push({ label: t.name + "." + v.name, name: v.name, type: t.name, kind: "enum value" });
      }
      return out;
    }
    function drawDocs() {
      const term = search.value.trim();
      if (term) {
        searchIndex = searchIndex || buildIndex();
        const hits = [];
        for (const x of searchIndex) {
          const m = fuzzy(term, x.name || x.label);
          if (m) hits.push({ ...x, score: m.score + (x.name ? 0 : 50) });
        }
        hits.sort((a, b) => b.score - a.score);
        sideBody.innerHTML = `<p class="dim">${hits.length} match${hits.length === 1 ? "" : "es"}</p><ul class="gql-hits">` + hits.slice(0, 80).map((x) =>
          `<li><a href="#" data-type="${esc(x.type)}"${x.field ? ` data-fieldname="${esc(x.field)}"` : ""}>${esc(x.label)}</a> <span class="dim">${esc(x.detail || x.kind)}</span></li>`).join("") + `</ul>`;
        return;
      }
      const at = docPath[docPath.length - 1];
      if (!at) {
        const groups = {};
        for (const t of Object.values(schema.types)) if (!t.name.startsWith("__")) (groups[t.kind] = groups[t.kind] || []).push(t.name);
        const roots = [["Query", schema.query], ["Mutation", schema.mutation], ["Subscription", schema.subscription]].filter((x) => x[1]);
        sideBody.innerHTML = `<h5>Roots</h5><ul class="gql-list">${roots.map(([l, n]) => `<li>${l}: <a href="#" data-type="${esc(n)}" class="t-type">${esc(n)}</a></li>`).join("")}</ul>` +
          Object.keys(groups).sort().map((k) => `<h5>${esc(k.toLowerCase().replace("_", " "))} <span class="count">${groups[k].length}</span></h5><ul class="gql-list">` +
            groups[k].sort().map((n) => `<li><a href="#" data-type="${esc(n)}" class="t-type">${esc(n)}</a></li>`).join("") + `</ul>`).join("");
        return;
      }
      const prev = docPath[docPath.length - 2];
      const back = `<button type="button" class="linkish gql-back">‹ ${prev ? esc(prev.type + (prev.field ? "." + prev.field : "")) : "Schema"}</button>`;
      const t = schema.types[at.type];
      if (!t) { sideBody.innerHTML = back + `<p class="dim">The schema has no type ${esc(at.type)}.</p>`; return; }
      const dep = (x) => x.isDeprecated ? ` <span class="dep-tag" title="${esc(x.deprecationReason || "")}">deprecated</span>` : "";
      const reason = (x) => x.isDeprecated && x.deprecationReason ? `<p class="dim">${esc(x.deprecationReason)}</p>` : "";
      const argsList = (args) => `<ul class="gql-fields">${args.map((a) => `<li><code><b>${esc(a.name)}</b>: ${link(a.type)}${a.defaultValue != null ? " = " + esc(a.defaultValue) : ""}</code>${required(a) ? ' <span class="req">required</span>' : ""}${a.description ? `<p class="dim">${esc(a.description)}</p>` : ""}</li>`).join("")}</ul>`;
      if (at.field) {
        const f = t.fields.find((x) => x.name === at.field) || t.inputFields.find((x) => x.name === at.field);
        if (f) {
          sideBody.innerHTML = back + `<h4 class="gql-title"><a href="#" data-type="${esc(t.name)}" class="t-type">${esc(t.name)}</a>.${esc(f.name)}${dep(f)}</h4>` +
            `<p><code>${esc(f.name)}${f.args && f.args.length ? "(…)" : ""}: ${link(f.type)}</code></p>` + (f.description ? `<p>${esc(f.description)}</p>` : "") + reason(f) +
            (f.args && f.args.length ? `<h5>Arguments</h5>` + argsList(f.args) : "") +
            (t.fields.includes(f) ? `<p><button type="button" class="btn small gql-insert-field" data-type="${esc(t.name)}" data-name="${esc(f.name)}">Insert at the cursor</button></p>` : "");
          return;
        }
      }
      let html = back + `<h4 class="gql-title"><span class="dim">${esc(t.kind.toLowerCase().replace("_", " "))}</span> <span class="t-type">${esc(t.name)}</span></h4>` + (t.description ? `<p>${esc(t.description)}</p>` : "");
      if (t.interfaces.length) html += `<p class="dim">implements ${t.interfaces.map((n) => `<a href="#" data-type="${esc(n)}" class="t-type">${esc(n)}</a>`).join(", ")}</p>`;
      if (t.fields.length) html += `<h5>Fields</h5><ul class="gql-fields">` + t.fields.map((f) => `<li${f.isDeprecated ? ' class="dep"' : ""}>` +
        `<button type="button" class="ins gql-insert-field" data-type="${esc(t.name)}" data-name="${esc(f.name)}" title="Insert at the cursor" aria-label="Insert ${esc(f.name)} at the cursor">+</button>` +
        `<code><a href="#" data-type="${esc(t.name)}" data-fieldname="${esc(f.name)}" class="t-field">${esc(f.name)}</a>${f.args.length ? "(" + f.args.map((a) => esc(a.name) + ": " + link(a.type)).join(", ") + ")" : ""}: ${link(f.type)}</code>${dep(f)}` +
        (f.description ? `<p class="dim">${esc(f.description)}</p>` : "") + reason(f) + `</li>`).join("") + `</ul>`;
      if (t.inputFields.length) html += `<h5>Input fields</h5>` + argsList(t.inputFields);
      if (t.enumValues.length) html += `<h5>Values</h5><ul class="gql-fields">` + t.enumValues.map((v) => `<li${v.isDeprecated ? ' class="dep"' : ""}><code><b>${esc(v.name)}</b></code>${dep(v)}${v.description ? `<p class="dim">${esc(v.description)}</p>` : ""}${reason(v)}</li>`).join("") + `</ul>`;
      if (t.possibleTypes.length) html += `<h5>Possible types</h5><ul class="gql-list">` + t.possibleTypes.map((n) => `<li><a href="#" data-type="${esc(n)}" class="t-type">${esc(n)}</a></li>`).join("") + `</ul>`;
      sideBody.innerHTML = html;
    }
    function openDocsFor(it) {
      let target = null;
      if (it.kind === "field") target = { type: it.parent, field: it.def.name };
      else if (it.kind === "type") target = { type: it.name };
      else if (it.kind === "enum") target = { type: it.type };
      else if (it.kind === "input") target = { type: it.parent, field: it.def.name };
      else if (it.kind === "arg") target = { type: named(it.def.type) };
      else if (it.kind === "var" && it.type) target = { type: named(it.type) };
      if (!target) return;
      docPath = [target];
      sideTab = "docs";
      save("side", sideTab);
      search.value = "";
      setSide(true);
    }
    function drawHistory() {
      const term = search.value.trim().toLowerCase();
      const rows = load("history", []).map((x, i) => ({ x, i })).filter(({ x }) => !term || x.query.toLowerCase().includes(term));
      sideBody.innerHTML = rows.length ? `<ul class="gql-histlist">${rows.map(({ x, i }) => `<li><button type="button" class="gql-hist" data-i="${i}" title="Open in a new tab">` +
        `<code>${esc(x.op || x.query.replace(/\s+/g, " ").trim().slice(0, 60))}</code><span class="dim">${esc(new Date(x.at).toLocaleString())} · ${x.status} · ${x.ms} ms</span></button></li>`).join("")}</ul>` +
        `<p><button type="button" class="linkish gql-hist-clear">Clear the history</button></p>` : `<p class="dim">What you run is kept here, in this browser, for this app.</p>`;
    }

    // ---- full screen
    let full = !!load("full", false), visible = true;
    function applyFull() {
      const on = full && visible;
      o.shell.classList.toggle("gql-full", on);
      el.classList.toggle("is-full", on);
      const b = $(".gql-full-btn");
      b.setAttribute("aria-pressed", String(full));
      b.textContent = full ? "Exit full screen" : "Full screen";
      b.title = full ? `Back to the drawer (Esc · Shift-${modName}-F)` : `Full screen (Shift-${modName}-F)`;
      for (const x of [q, vars, headers]) x.cw = 0;
      requestAnimationFrame(() => { for (const x of [q, vars, headers]) x.sync(); });
    }
    function toggleFull() {
      const a = o.root.activeElement;
      const keep = a && a.tagName === "TEXTAREA" && el.contains(a) ? [a, a.selectionStart, a.selectionEnd, a.scrollTop, a.scrollLeft] : null;
      full = !full;
      save("full", full);
      applyFull();
      if (keep) { keep[0].focus({ preventScroll: true }); keep[0].setSelectionRange(keep[1], keep[2]); keep[0].scrollTop = keep[3]; keep[0].scrollLeft = keep[4]; }
    }
    function leaveFull(e) {
      if (!full || !visible) return false;
      e.preventDefault();
      e.stopPropagation();
      toggleFull();
      return true;
    }
    $(".gql-full-btn").addEventListener("click", toggleFull);

    if (typeof ResizeObserver !== "undefined") new ResizeObserver(() => el.classList.toggle("wide", el.offsetWidth >= 1100)).observe(el);

    drawTabs();
    setSide(sideOpen);
    drawOps();
    checkHeaders();
    applyFull();
    loadSchema();

    let saved = null;
    return {
      el,
      // save and restore keep the editors' focus, caret and scroll while
      // the toolbar moves the panel into another record's drawer.
      save() {
        const a = o.root.activeElement;
        saved = { focus: a && el.contains(a) ? a : null, scroll: [q, vars, headers].map((x) => [x.ta.scrollTop, x.ta.scrollLeft]), result: resultEl.scrollTop, side: sideBody.scrollTop };
        if (saved.focus && saved.focus.tagName === "TEXTAREA") saved.sel = [saved.focus.selectionStart, saved.focus.selectionEnd];
      },
      restore() {
        if (!saved) return;
        [q, vars, headers].forEach((x, i) => { x.ta.scrollTop = saved.scroll[i][0]; x.ta.scrollLeft = saved.scroll[i][1]; x.sync(); });
        resultEl.scrollTop = saved.result;
        sideBody.scrollTop = saved.side;
        if (saved.focus) { saved.focus.focus({ preventScroll: true }); if (saved.sel) saved.focus.setSelectionRange(saved.sel[0], saved.sel[1]); }
        saved = null;
      },
      visible(on) { if (visible !== on) { visible = on; applyFull(); } },
      refreshSchema: loadSchema,
      editors: { q, vars, headers },
    };
  }

  const CSS = `
.gql { --kw: #a626a4; --def: #4078f2; --field: #1c2330; --alias: #986801; --arg: #b07600; --type: #0b7f8c; --var: #d1453b; --str: #3f8f3f; --num: #986801; --cmt: #8b93a1; --enum: #0072a8; --dir: #a626a4; --punct: #6b7383;
  display: flex; flex-direction: column; gap: 6px; flex: 1; min-height: 440px; position: relative; }
@media (prefers-color-scheme: dark) {
  .gql { --kw: #d38aea; --def: #6aa2ff; --field: #e4e8ef; --alias: #e5c07b; --arg: #e5c07b; --type: #56d4dd; --var: #ef7b7b; --str: #98c379; --num: #d19a66; --cmt: #7d8799; --enum: #61afef; --dir: #d38aea; --punct: #8e97a8; }
}
.gql.is-full { min-height: 0; }
.gql-bar { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.gql-bar .gql-schema { margin-left: auto; }
.gql .btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; font-weight: 600; }
.gql .btn.primary:hover { filter: brightness(1.08); }
.gql .btn.small { padding: 0 6px; font-size: 11px; }
.gql .btn[aria-pressed="true"] { background: var(--accent-bg); border-color: var(--accent); }
.gql-op { height: 22px; max-width: 200px; border: 1px solid var(--line); border-radius: 5px; background: var(--bg); color: var(--fg); font-size: 11px; }
.gql-op[hidden] { display: none; }
.gql-status { display: inline-flex; align-items: center; gap: 5px; color: var(--dim); }
.gql-status .err { color: var(--err); }
.st { padding: 0 6px; border-radius: 8px; font-weight: 700; }
.st.ok { background: var(--accent-bg); color: var(--accent); }
.st.warn { background: var(--warn-bg); color: var(--warn); }
.st.err { background: var(--err-bg); color: var(--err); }
.gql-tabs { display: flex; align-items: flex-end; gap: 2px; border-bottom: 1px solid var(--line); overflow-x: auto; flex-shrink: 0; }
.gql-tab { display: inline-flex; align-items: center; border: 1px solid transparent; border-bottom: 0; border-radius: 5px 5px 0 0; }
.gql-tab[aria-selected="true"] { border-color: var(--line); background: var(--bg2); box-shadow: inset 0 2px var(--accent); }
.gql-tab button { border: 0; background: none; padding: 4px 8px; white-space: nowrap; }
.gql-tab .gql-tab-x { padding: 4px 6px 4px 0; color: var(--dim); }
.gql-tab-add { border: 0; background: none; padding: 3px 8px; color: var(--dim); font-size: 14px; }
.gql-main { flex: 1; min-height: 0; display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 8px; position: relative; }
.gql.with-side.wide .gql-main { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) 340px; }
@media (max-width: 720px) { .gql-main { grid-template-columns: 1fr; grid-auto-rows: minmax(280px, auto); } }
.gql-left, .gql-right { display: flex; flex-direction: column; gap: 6px; min-height: 0; min-width: 0; }
.gql-q { flex: 3; min-height: 160px; display: flex; }
.gql-sub { flex: 1.3; min-height: 110px; display: flex; flex-direction: column; }
.gql-subtabs, .gql-side-tabs { display: flex; gap: 2px; align-items: center; }
.gql-subtabs button, .gql-side-tabs button { border: 0; background: none; padding: 3px 8px; color: var(--dim); font-weight: 600; }
.gql-subtabs button[aria-selected="true"], .gql-side-tabs button[aria-selected="true"] { color: var(--fg); box-shadow: inset 0 -2px var(--accent); }
.gql-sub-err { margin-left: auto; color: var(--err); font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 65%; }
.gql-vars, .gql-headers { flex: 1; min-height: 0; display: flex; }
.gql-vars[hidden], .gql-headers[hidden] { display: none; }
.gql-problems { max-height: 92px; overflow: auto; border: 1px solid var(--err); border-radius: 6px; background: var(--err-bg); color: var(--err); }
.gql-problems[hidden], .gql-errors[hidden] { display: none; }
.gql-problems ul, .gql-errors ul { margin: 0; padding: 4px 8px; list-style: none; }
.gql-problems li, .gql-errors li { padding: 1px 0; }
.gql-problems .linkish { color: inherit; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.gql-errors { max-height: 35%; overflow: auto; border: 1px solid var(--err); border-radius: 6px; background: var(--err-bg); }
.gql-errors .code { padding: 0 5px; border-radius: 6px; background: var(--bg); color: var(--err); font-size: 10.5px; font-weight: 700; }
.gql-result { flex: 1; min-height: 120px; overflow: auto; padding: 8px 8px 8px 22px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg2); font: 12px/18px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.gql-result .raw { margin: 0; white-space: pre-wrap; }
.gql-result .err { margin: 0; color: var(--err); font-family: ui-sans-serif, system-ui, sans-serif; }
.gql-hint { margin: 0; font-family: ui-sans-serif, system-ui, sans-serif; }
.jtree { white-space: pre; }
.jc { padding-left: 16px; }
.jl { position: relative; }
.jt { position: absolute; left: -15px; top: 3px; width: 12px; height: 12px; padding: 0; border: 0; background: none; color: var(--dim); }
.jt::before { content: "▾"; display: block; font-size: 10px; line-height: 12px; }
.jn.folded > .jl > .jt::before { content: "▸"; }
.jsum { display: none; color: var(--dim); }
.jn.folded > .jc, .jn.folded > .jend { display: none; }
.jn.folded > .jl > .jsum { display: inline; }
.gql-side { position: absolute; top: 0; right: 0; bottom: 0; width: min(340px, 100%); display: flex; flex-direction: column; gap: 6px; padding: 6px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); box-shadow: var(--shadow); z-index: 3; }
.gql.wide .gql-side { position: static; width: auto; box-shadow: none; }
.gql-side[hidden] { display: none; }
.gql-search { height: 26px; padding: 0 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); font: inherit; }
.gql-side-body { flex: 1; overflow: auto; min-height: 0; }
.gql-side-body h5 { margin: 10px 0 4px; font-size: 11px; color: var(--dim); text-transform: uppercase; letter-spacing: .04em; }
.gql-side-body .count { padding: 0 5px; border-radius: 8px; background: var(--bg3); text-transform: none; }
.gql-side-body ul { margin: 0; padding: 0; list-style: none; }
.gql-side-body a { color: var(--type); text-decoration: none; }
.gql-side-body a.t-field { color: var(--def); }
.gql-side-body a:hover { text-decoration: underline; }
.gql-side-body p { margin: 2px 0 4px; }
.gql-title { margin: 6px 0; font-size: 13px; }
.gql-fields li { position: relative; padding: 4px 0 4px 20px; border-bottom: 1px solid var(--line); }
.gql-opitem { padding: 5px 0; border-bottom: 1px solid var(--line); }
.gql-fields li.dep code, .gql-opitem.dep .gql-opsig b { text-decoration: line-through; opacity: .7; }
.gql-fields .ins { position: absolute; left: 0; top: 5px; width: 15px; height: 15px; padding: 0; border: 1px solid var(--line); border-radius: 4px; background: var(--bg2); font-size: 11px; line-height: 12px; color: var(--dim); }
.gql-fields .ins:hover { color: var(--accent); border-color: var(--accent); }
.gql-opsig { white-space: normal; overflow-wrap: anywhere; }
.gql-opbtns { display: flex; gap: 4px; margin-top: 3px; }
.note-sm { color: var(--warn); font-size: 11px; }
.dep-tag { display: inline-block; padding: 0 5px; border-radius: 6px; background: var(--warn-bg); color: var(--warn); font-size: 10.5px; font-weight: 600; }
.req { color: var(--err); font-size: 10.5px; font-weight: 700; }
.gql-hits li, .gql-list li { padding: 2px 0; }
.gql-histlist button { display: flex; flex-direction: column; align-items: flex-start; width: 100%; padding: 4px 2px; border: 0; border-bottom: 1px solid var(--line); background: none; text-align: left; }
.gql-histlist button:hover { background: var(--bg2); }
.gql-histlist code { max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.gql-tip { position: absolute; z-index: 6; max-width: 420px; padding: 6px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); box-shadow: var(--shadow); pointer-events: none; }
.gql-tip[hidden] { display: none; }
.gql-tip p { margin: 4px 0 0; }
.gql-tip code { white-space: normal; overflow-wrap: anywhere; }
.tip-err { color: var(--err); }
.tip-hint { font-size: 10.5px; }
.ed { position: relative; flex: 1; min-width: 0; min-height: 0; display: flex; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); }
.ed:focus-within { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
.ed-gutter { flex: none; width: 34px; overflow: hidden; border-right: 1px solid var(--line); border-radius: 6px 0 0 6px; background: var(--bg2); }
.ed-nums { padding: 8px 6px 8px 0; text-align: right; white-space: pre; color: var(--dim); opacity: .7; }
.ed-main { position: relative; flex: 1; min-width: 0; overflow: hidden; }
.ed-hl, .ed-ta, .ed-nums, .ed-probe { font: 12px/18px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; tab-size: 2; letter-spacing: 0; font-variant-ligatures: none; }
.ed-hl { position: absolute; top: 0; left: 0; margin: 0; padding: 8px 10px; min-width: 100%; white-space: pre; color: var(--field); pointer-events: none; }
.ed-ta { position: absolute; inset: 0; width: 100%; height: 100%; margin: 0; padding: 8px 10px; border: 0; outline: 0; resize: none; white-space: pre; overflow: auto; background: transparent; color: transparent; caret-color: var(--fg); -webkit-text-fill-color: transparent; }
.ed-ta::placeholder { color: var(--dim); -webkit-text-fill-color: var(--dim); }
.ed-ta::selection { background: rgba(76, 141, 246, .3); }
.ed-probe { position: absolute; top: 0; left: 0; visibility: hidden; white-space: pre; }
.t-kw { color: var(--kw); font-weight: 600; } .t-def, .t-frag { color: var(--def); font-weight: 600; } .t-field { color: var(--field); } .t-alias { color: var(--alias); }
.t-arg, .t-key { color: var(--arg); } .t-type { color: var(--type); } .t-var { color: var(--var); } .t-str { color: var(--str); } .t-num { color: var(--num); }
.t-cmt { color: var(--cmt); font-style: italic; } .t-enum { color: var(--enum); } .t-dir { color: var(--dir); } .t-p { color: var(--punct); } .t-atom { color: var(--kw); } .t-bad { color: var(--err); }
.match { border-radius: 2px; background: var(--accent-bg); box-shadow: 0 0 0 1px var(--accent); }
.squiggle { text-decoration: underline wavy var(--err); text-decoration-skip-ink: none; text-underline-offset: 3px; }
.ed-list { position: absolute; z-index: 5; display: flex; flex-direction: column; max-height: 260px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); box-shadow: var(--shadow); overflow: hidden; }
.ed-list[hidden] { display: none; }
.ed-list ul { flex: 1; min-height: 0; overflow: auto; margin: 0; padding: 3px 0; list-style: none; }
.ed-list li { display: flex; align-items: center; gap: 6px; padding: 1px 8px; cursor: pointer; white-space: nowrap; }
.ed-list li[aria-selected="true"] { background: var(--accent-bg); }
.ed-list li.dep .ed-label { text-decoration: line-through; opacity: .7; }
.ed-label b { color: var(--accent); }
.ed-detail { margin-left: auto; padding-left: 12px; max-width: 60%; overflow: hidden; text-overflow: ellipsis; color: var(--dim); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; }
.ed-icon { flex: none; width: 14px; height: 14px; border-radius: 3px; background: var(--dim); color: #fff; font-size: 9px; font-weight: 700; line-height: 14px; text-align: center; }
.k-field .ed-icon { background: var(--def); } .k-field .ed-icon::before { content: "F"; }
.k-arg .ed-icon, .k-input .ed-icon { background: var(--arg); } .k-arg .ed-icon::before, .k-input .ed-icon::before { content: "A"; }
.k-enum .ed-icon { background: var(--enum); } .k-enum .ed-icon::before { content: "E"; }
.k-type .ed-icon { background: var(--type); } .k-type .ed-icon::before { content: "T"; }
.k-var .ed-icon { background: var(--var); } .k-var .ed-icon::before { content: "$"; }
.k-dir .ed-icon { background: var(--dir); } .k-dir .ed-icon::before { content: "@"; }
.k-kw .ed-icon, .k-atom .ed-icon { background: var(--kw); } .k-kw .ed-icon::before, .k-atom .ed-icon::before { content: "K"; }
.k-frag .ed-icon { background: var(--def); } .k-frag .ed-icon::before { content: "…"; }
.k-snippet .ed-icon::before { content: "{"; }
.ed-doc { max-height: 70px; overflow: auto; padding: 5px 8px; border-top: 1px solid var(--line); background: var(--bg2); color: var(--dim); white-space: normal; }
.ed-doc[hidden] { display: none; }
`;

  window.__vukaGraphiQL = { create, ...api };
})();
