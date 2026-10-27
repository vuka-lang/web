// Tests of the GraphiQL panel's language half (toolbar_graphiql.js) in
// node: completion at the cursor, what accepting an item inserts,
// formatting, generated operations and the variables' JSON, over a schema
// read by introspection:
//
//	node graphiql.js ../../toolbar_graphiql.js schema.json formatted.json
//
// formatted.json gets the formatting corpus, before and after, which the
// Go test parses.
"use strict";
const fs = require("fs"), path = require("path");
const [file, schemaFile, outFile] = process.argv.slice(2);
const g = require(path.resolve(file));
const schema = g.makeSchema(JSON.parse(fs.readFileSync(schemaFile, "utf8")).data.__schema);
const tests = [];
const test = (name, fn) => tests.push({ name, fn });

function eq(got, want, what) {
  const a = JSON.stringify(got), b = JSON.stringify(want);
  if (a !== b) throw new Error(`${what || "value"}\n   got  ${a}\n   want ${b}`);
}
// at is what completes at | in doc, ranked by the word typed.
function at(doc) {
  const pos = doc.indexOf("|"), src = doc.replace("|", "");
  const ctx = g.analyze(src, schema, pos).ctx;
  return ctx && { word: ctx.word, labels: g.rank(ctx.items, ctx.word).map((x) => x.label), ctx, src };
}
const has = (r, ...ls) => { for (const l of ls) if (!r || !r.labels.includes(l)) throw new Error(`no ${l} in ${JSON.stringify(r && r.labels)}`); };
const lacks = (r, ...ls) => { for (const l of ls) if (r && r.labels.includes(l)) throw new Error(`${l} in ${JSON.stringify(r.labels)}`); };
// insert is doc with label accepted at |, the cursor after it shown as |.
function insert(doc, label) {
  const r = at(doc);
  const item = r.ctx.items.find((x) => x.label === label);
  if (!item) throw new Error(`no ${label} in ${JSON.stringify(r.labels)}`);
  const out = g.applyItem(r.src, r.ctx, item, schema);
  return out.text.slice(0, out.cursor) + "|" + out.text.slice(out.cursor);
}

test("operation keywords at the top", () => {
  eq(at("|").labels, ["query", "mutation", "subscription", "fragment"]);
  eq(at("mu|").labels, ["mutation"]);
});

test("root fields, fuzzy", () => {
  has(at("{ |"), "node", "search", "user", "users", "__typename");
  eq(at("{ usr|").labels.slice(0, 2), ["user", "users"]);
  eq(at("query { srch| }").labels[0], "search");
  has(at("mutation { |"), "createUser");
  lacks(at("mutation { |"), "users");
  eq(at("{ |").labels.at(-1), "old", "deprecated last");
});

test("nested fields: the type at the cursor", () => {
  const r = at("{ user(id: 1) { friends(first: 2) { addr| } } }");
  eq([r.labels, r.word], [["address"], "addr"]);
  has(at("{ user(id: 1) { address { | } } }"), "city", "street");
  has(at("query Q { a: user(id: 1) { n| } }"), "name");
});

test("fragments and inline fragments on interfaces and unions", () => {
  has(at("{ node(id: 1) { ... | } }"), "on");
  has(at("{ node(id: 1) { ... on | } }"), "Node", "User", "Post");
  has(at("{ node(id: 1) { ... on Post { | } } }"), "title", "author");
  eq(at('{ search(text: "a") { | } }').labels, ["__typename"]);
  has(at('{ search(text: "a") { ... on | } }'), "User", "Post");
  has(at('{ search(text: "a") { ... on User { | } } }'), "name", "friends");
  const doc = "{ user(id: 1) { ...| } }\nfragment UserBits on User { name }\nfragment PostBits on Post { title }";
  has(at(doc), "on", "UserBits");
  lacks(at(doc), "PostBits");
  has(at("fragment F on | "), "User", "Node", "SearchResult");
});

test("arguments: types and required marks", () => {
  eq(at("{ users(|) }").labels, ["first", "filter"]);
  const id = at("{ user(|) }").ctx.items.find((x) => x.label === "id");
  eq([id.detail, id.required], ["ID!", true]);
  eq(at('{ search(text: "x", |) }').labels, ["kinds"]);
});

test("enum values, booleans and null", () => {
  eq(at('{ search(text: "x", kinds: [|]) }').labels, ["USER", "POST"]);
  eq(at("{ users(filter: {status: |}) }").labels, ["ACTIVE", "null", "PAST_DUE"]);
  eq(at("{ users(filter: {status: P|}) }").labels, ["PAST_DUE"]);
  has(at("{ user(id: 1) { friends(first: 1) @include(if: |) } }"), "true", "false");
});

test("input object fields inside { }", () => {
  eq(at("{ users(filter: {|}) }").labels, ["name", "status", "tags", "near"]);
  eq(at('{ users(filter: {name: "a", |}) }').labels, ["status", "tags", "near"]);
  eq(at("{ users(filter: {near: {|}}) }").labels, ["lat", "lng"]);
  eq(at('mutation { createUser(in: {name: "a" e|}) }').labels, ["email"]);
});

test("variables: the declared ones of the type wanted", () => {
  eq(at("query Q($n: Int, $id: ID!, $f: UserFilter) { users(first: $|) }").labels, ["$n"]);
  eq(at("query Q($n: Int, $f: UserFilter) { users(filter: $|) }").labels, ["$f"]);
  has(at("query Q($x: |) { a }"), "Int", "UserFilter", "Status");
  lacks(at("query Q($x: |) { a }"), "User", "Query");
  eq(at("query Q($s: Status = |) { a }").labels, ["ACTIVE", "null", "PAST_DUE"]);
});

test("directives", () => {
  has(at("{ user(id: 1) { name @| } }"), "@include", "@skip");
  lacks(at("{ user(id: 1) { name @| } }"), "@deprecated");
});

test("nothing inside strings and comments", () => {
  eq(at("{ users # us| \n }"), null);
  eq(at('{ search(text: "us|") }'), null);
});

test("a field with required arguments gets () with the cursor inside", () => {
  eq(insert("{ us| }", "user"), "{ user(|) {\n  \n} }");
  eq(insert("{\n  user(id: 1) {\n    fri|\n  }\n}", "friends"), "{\n  user(id: 1) {\n    friends(|) {\n      \n    }\n  }\n}");
});

test("an object field gets { }, a scalar nothing", () => {
  eq(insert("{\n  user(id: 1) {\n    addr|\n  }\n}", "address"), "{\n  user(id: 1) {\n    address {\n      |\n    }\n  }\n}");
  eq(insert("{\n  user(id: 1) {\n    na|\n  }\n}", "name"), "{\n  user(id: 1) {\n    name|\n  }\n}");
  eq(insert("{\n  us|er(id: 1) { a }\n}", "user"), "{\n  user|(id: 1) { a }\n}");
});

test("arguments insert name: — an input object one with {}", () => {
  eq(insert("{ users(fi|) }", "first"), "{ users(first: |) }");
  eq(insert("{ users(fil|) }", "filter"), "{ users(filter: {|}) }");
  eq(insert("{ users(filter: {sta|}) }", "status"), "{ users(filter: {status: |}) }");
  eq(insert("{ users(filter: {status: |}) }", "ACTIVE"), "{ users(filter: {status: ACTIVE|}) }");
  eq(insert("query($n: Int) { users(first: $|) }", "$n"), "query($n: Int) { users(first: $n|) }");
});

test("roles and hover info", () => {
  const src = "query Q($id: ID!) { user(id: $id) { name ...F } }";
  const a = g.analyze(src, schema);
  const role = (text) => a.roles[a.toks.findIndex((t) => src.slice(t.s, t.e) === text)];
  eq(["query", "Q", "$id", "ID", "user", "id", "name", "...", "F"].map(role), ["kw", "def", "var", "type", "field", "arg", "field", "p", "frag"]);
  const user = a.info[a.toks.findIndex((t) => src.slice(t.s, t.e) === "user")];
  eq([user.kind, user.parent, user.def.name], ["field", "Query", "user"]);
  eq(a.ops.map((o) => [o.kind, o.name, o.vars.map((v) => g.typeText(v.type))]), [["query", "Q", ["ID!"]]]);
});

test("several operations: each one's range", () => {
  const src = 'query A { users { name } }\n\nmutation B { createUser(in: {name: "a", email: "b"}) { id } }\n{ node(id: 1) { id } }';
  const a = g.analyze(src, schema);
  eq(a.ops.map((o) => [o.kind, o.name, src.slice(o.s, o.e).slice(0, 5), src.slice(o.s, o.e).slice(-3)]),
    [["query", "A", "query", "} }"], ["mutation", "B", "mutat", "} }"], ["query", null, "{ nod", "} }"]]);
});

test("generated operations: variables, wired arguments, a selection", () => {
  const user = g.operationFor(schema, "query", schema.types.Query.fields.find((x) => x.name === "user"));
  eq(user.text, "query User($id: ID!) {\n  user(id: $id) {\n    id\n    name\n    email\n    status\n  }\n}\n");
  eq(user.text.slice(user.sel, user.sel + 3), "id\n", "the cursor on the selection");
  eq(user.variables, { id: "" });
  const search = g.operationFor(schema, "query", schema.types.Query.fields.find((x) => x.name === "search"));
  eq(search.text, "query Search($text: String!, $kinds: [Kind!]) {\n  search(text: $text, kinds: $kinds) {\n    __typename\n  }\n}\n");
  eq(g.operationFor(schema, "mutation", schema.types.Mutation.fields[0]).variables, { in: { name: "", email: "" } });
  eq(g.operationFor(schema, "subscription", schema.types.Subscription.fields[0]).text.split("\n")[0], "subscription UserAdded {");
  const old = g.operationFor(schema, "query", schema.types.Query.fields.find((x) => x.name === "old"));
  eq([old.text, old.text.slice(old.sel, old.sel + 3)], ["query Old {\n  old\n}\n", "old"]);
});

// ---- formatting

function format(src) {
  const r = g.format(src);
  if (r.error) throw new Error(r.error + " at " + r.pos);
  return r.text;
}

const corpus = [
  `{ users { name } }`,
  `query Users($first: Int = 10, $filter: UserFilter = {name: "a", status: ACTIVE}) { users(first: $first, filter: $filter) { id name status } }`,
  `# the people
query People($id: ID!, $withFriends: Boolean = false) {
  user(id: $id) { ...UserBits friends(first: 5) @include(if: $withFriends) { id, name } }
}

# a fragment
fragment UserBits on User { id name email address { city street } }`,
  `{ node(id: "1") { id ... on User { name } ... on Post { title author { name } } } }`,
  `query Search { search(text: "a very long text to search for, which is long enough", kinds: [USER, POST]) { __typename ... on User { name } } }`,
  `mutation Create { createUser(in: {name: "someone with a long name", email: "someone@example.com", status: ACTIVE}) { id } }`,
  `query Deep { users(filter: {name: "a", tags: ["x", "y", "z"], near: {lat: 1.5, lng: -2e3}, status: ACTIVE}) { id } }`,
  `query Block {
  search(text: """
    a block string
      kept as it is
  """) { __typename }
}`,
  `query Aliases { first: user(id: 1) { n: name } second: user(id: 2) { n: name } }`,
  `query Commented($id: ID!) { # on the head
  user(id: $id) { # on a brace
    # before name
    name # after name

    email
    # at the end
  }
}`,
  `subscription Added { userAdded { id } }`,
  `query D($a: [Kind!]! = [USER, POST], $b: GeoInput = {lat: 1, lng: 2}) { users(first: 1, filter: {near: $b}) @skip(if: false) { id } search(text: "x", kinds: $a) { __typename } }`,
  `query Inline { ... on Query { users { id } } ... @include(if: true) { old } }`,
];

test("formatting: canonical, idempotent, the same document", () => {
  const out = [];
  const sig = (s) => g.lex(s).filter((t) => t.k !== "cmt" && !(t.k === "p" && t.v === ",")).map((t) => t.k === "block" ? "BLOCK" : s.slice(t.s, t.e));
  const cmts = (s) => g.lex(s).filter((t) => t.k === "cmt").map((t) => s.slice(t.s, t.e).trim());
  for (const src of corpus) {
    const once = format(src), what = JSON.stringify(src.slice(0, 40));
    eq(format(once), once, "idempotent for " + what);
    for (const line of once.split("\n")) if (line.length > 80 && !line.includes('"')) throw new Error("past 80 columns: " + line);
    eq(sig(once), sig(src), "the same tokens for " + what);
    eq(cmts(once), cmts(src), "the comments of " + what);
    out.push({ src, formatted: once });
  }
  fs.writeFileSync(outFile, JSON.stringify(out));
});

test("formatting: the shapes", () => {
  eq(format("{users{name}}"), "{\n  users {\n    name\n  }\n}\n");
  eq(format("query Q { user(id: 1) { name } }\nquery R { users { id } }"), "query Q {\n  user(id: 1) {\n    name\n  }\n}\n\nquery R {\n  users {\n    id\n  }\n}\n");
  eq(format('{ search(text: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", kinds: [USER, POST, USER]) { __typename } }'),
    '{\n  search(\n    text: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"\n    kinds: [USER, POST, USER]\n  ) {\n    __typename\n  }\n}\n');
  eq(format("{ users(filter: {near: {lat: 1, lng: 2}}) { id } }"), "{\n  users(filter: { near: { lat: 1, lng: 2 } }) {\n    id\n  }\n}\n");
  eq(format('mutation { createUser(in: {name: "someone with a long name", email: "someone@example.com"}) { id } }'),
    'mutation {\n  createUser(\n    in: { name: "someone with a long name", email: "someone@example.com" }\n  ) {\n    id\n  }\n}\n', "an argument wraps, its object fits");
  eq(format('mutation { createUser(in: {name: "someone with a fairly long name", email: "someone@example.com"}) { id } }'),
    'mutation {\n  createUser(\n    in: {\n      name: "someone with a fairly long name"\n      email: "someone@example.com"\n    }\n  ) {\n    id\n  }\n}\n', "the object wraps too");
  eq(format("{ a } # end"), "{\n  a\n} # end\n");
  eq(format("{ a\n\n  b }"), "{\n  a\n\n  b\n}\n", "a blank line between fields kept");
  eq(g.format("{ a(").error != null, true, "a syntax error");
});

test("formatting a selection formats the definitions it touches", () => {
  const src = "query A { a { b } }\n\nquery B { c { d } }\n";
  const s = src.indexOf("c {");
  const r = g.formatRange(src, s, s + 3);
  eq(src.slice(0, r.from) + r.text + src.slice(r.to), "query A { a { b } }\n\nquery B {\n  c {\n    d\n  }\n}\n");
});

test("the cursor stays on its token", () => {
  const src = "{users(first:1){name email}}", out = format(src);
  const np = g.sameToken(src, out, src.indexOf("email") + 2);
  eq(out.slice(np - 2, np + 3), "email");
  eq(g.sameToken(src, out, src.indexOf("1") + 1), out.indexOf("1") + 1);
});

// ---- variables

test("variables: keys, input fields and enum values complete", () => {
  const vars = g.analyze("query Q($id: ID!, $filter: UserFilter, $kinds: [Kind!]) { a }", schema).ops[0].vars;
  const c = (doc) => { const pos = doc.indexOf("|"); return g.jsonContext(doc.replace("|", ""), pos, vars, schema).items.map((x) => x.label); };
  eq(c("{|}"), ["id", "filter", "kinds"]);
  eq(c('{"id": "1", "|"}'), ["filter", "kinds"]);
  eq(c('{"filter": {"|"}}'), ["name", "status", "tags", "near"]);
  eq(c('{"filter": {"status": |}}'), ["ACTIVE", "PAST_DUE", "null"]);
  eq(c('{"kinds": [|]}'), ["USER", "POST"]);
  eq(c('{"filter": {"near": {"|"}}}'), ["lat", "lng"]);
});

test("variables: JSON errors at their position", () => {
  eq(g.checkJSON('{"a": 1,\n "b": }').pos, 15);
  eq(g.checkJSON('{"a": 1 "b": 2}').message, 'Expected "," or "}", found ""b""');
  eq(g.checkJSON("{a: 1}").message, 'Expected a key in quotes, found "a"');
  eq(g.checkJSON(' {"a": [1, 2, {"b": null}]} '), null);
  eq(g.checkJSON(""), null);
  eq(g.jsonPaths('{"f": {"tags": [1, "x"]}}').filter((p) => !p.key).map((p) => p.path), ["f", "f.tags", "f.tags[0]", "f.tags[1]"]);
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try { await t.fn(); } catch (e) { failed++; console.log(`FAIL ${t.name}: ${e.message}`); }
  }
  console.log(`${tests.length - failed}/${tests.length} passed`);
  if (failed) process.exit(1);
})();
