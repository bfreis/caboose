import { test } from "node:test";
import assert from "node:assert/strict";

import { FakeDocument, elements, texts } from "./fakes.test.mjs";
import { IDS, ID_PREFIXES, byId, el, itemId, navLink, parseRoute, replace, routeHash, sandboxBlock, sandboxText, useDocument } from "./dom.mjs";

function fresh() {
  const d = new FakeDocument().shell();
  useDocument(d);
  return d;
}

test("IDS and ID_PREFIXES are frozen", () => {
  assert.ok(Object.isFrozen(IDS));
  assert.ok(Object.isFrozen(ID_PREFIXES));
  assert.throws(() => {
    "use strict";
    IDS.evil = "context";
  });
  assert.equal(IDS.evil, undefined);
});

test("el refuses handlers, links, styles and sources", () => {
  fresh();
  for (const name of ["onclick", "onerror", "onload", "ONCLICK", "href", "src", "style", "srcdoc", "action", "formaction", "xlink:href", "data-x", "Class", "", "aria-", "aria-label x", "constructor", "__proto__", "toString"]) {
    assert.throws(() => el("p", { [name]: "x" }), /dom: refused/, name);
  }
});

test("el refuses an id, name or for that is not the page's", () => {
  fresh();
  for (const v of ["context", "<img src=x onerror=alert(1)>", "cookie", "location", "req", "req-", "req-12345678901", "evil-12", "REQ-1", " req-1", "req-1 ", 12, true, {}]) {
    for (const name of ["id", "name", "for", "aria-labelledby", "aria-describedby"]) {
      assert.throws(() => el("input", { [name]: v }), /dom: refused/, name + "=" + String(v));
    }
  }
  const p = el("p", { id: IDS.title });
  assert.equal(p.getAttribute("id"), IDS.title);
  assert.equal(el("label", { for: "req-12" }).getAttribute("for"), "req-12");
  assert.equal(el("p", { "aria-labelledby": IDS.title }).getAttribute("aria-labelledby"), IDS.title);
});

test("itemId takes a known prefix and a number", () => {
  assert.equal(itemId("req", 12), "req-12");
  assert.equal(itemId("grant", 0), "grant-0");
  for (const [p, n] of [["evil", 1], ["req", -1], ["req", 1.5], ["req", "12"], ["req", 1e11], ["req", Number.NaN], ["req-x", 1]]) {
    assert.throws(() => itemId(p, n), /dom: no item id/);
  }
});

test("el refuses elements that load, run or navigate", () => {
  fresh();
  for (const tag of ["script", "iframe", "img", "a", "form", "object", "embed", "link", "style", "svg", "math", "template", "base", "meta", "SCRIPT", "", "p onclick"]) {
    assert.throws(() => el(tag, null), /dom: refused/, tag);
  }
});

test("el checks each attribute's form", () => {
  fresh();
  assert.throws(() => el("p", { class: "a\"b" }));
  assert.throws(() => el("p", { class: "x;color:red" }));
  assert.throws(() => el("input", { type: "image" }));
  assert.throws(() => el("p", { role: "button" }));
  assert.throws(() => el("p", { tabindex: "0; x" }));
  assert.throws(() => el("button", { disabled: "false" }));
  assert.throws(() => el("p", { title: { toString: () => "x" } }));
  assert.throws(() => el("p", "class"));
  assert.throws(() => el("p", ["class", "x"]));
  const b = el("button", { type: "button", disabled: true, hidden: false, class: "answer deny", "aria-label": "Deny request 12" });
  assert.equal(b.getAttribute("disabled"), "");
  assert.equal(b.getAttribute("hidden"), null);
  assert.equal(b.getAttribute("class"), "answer deny");
  assert.equal(b.getAttribute("aria-label"), "Deny request 12");
  assert.equal(el("p", { title: null, id: undefined }).attributes.size, 0);
});

test("a string child goes in as a text node, never as markup", () => {
  const d = fresh();
  const s = "<b>bold</b> & <script>alert(1)</script>";
  const p = el("p", null, s, 12, null, false, undefined, ["a", ["b"]]);
  assert.equal(p.childNodes.length, 4);
  assert.deepEqual(texts(p), [s, "12", "a", "b"]);
  assert.deepEqual(elements(p).map((e) => e.tagName), ["P"]);
  assert.deepEqual(d.log.filter((x) => x.op === "createTextNode").map((x) => x.data), [s, "12", "a", "b"]);
});

test("el takes no child it did not make", () => {
  const d = fresh();
  assert.throws(() => el("p", null, { nodeType: 1, tagName: "IMG" }), /dom: refused/);
  assert.throws(() => el("p", null, d.createElement("img")), /dom: refused/);
  assert.throws(() => el("p", null, Symbol("x")), /dom: refused/);
  assert.throws(() => el("p", null, true), /dom: refused/);
  assert.throws(() => replace(byId(IDS.main), { nodeType: 3, data: "x" }), /dom: refused/);
});

test("the fake document refuses every HTML sink", () => {
  const d = fresh();
  const p = el("p", null);
  assert.throws(() => {
    p.innerHTML = "<b>x</b>";
  }, /HTML sink/);
  assert.throws(() => p.outerHTML, /HTML sink/);
  assert.throws(() => p.insertAdjacentHTML("beforeend", "<b>"), /HTML sink/);
  assert.throws(() => d.write("<b>"), /HTML sink/);
  assert.throws(() => d.cookie, /HTML sink/);
});

const hostile = [
  "<img src=x onerror=alert(1)>",
  "‮evil‬ and ⁧rtl⁩",
  "javascript:alert(1)",
  "<a href=\"javascript:alert(1)\">x</a>",
  "x".repeat(10 * 1024),
  "\"><svg onload=alert(1)>",
];

test("hostile text renders as text nodes only", () => {
  const d = fresh();
  // A request as a page would draw it: its context, session and the
  // sandbox's own words, all a sandbox's.
  for (const h of hostile) {
    const before = d.log.length;
    const art = el(
      "article",
      { id: itemId("req", 12), "aria-label": "Request 12: " + h },
      el("h2", null, "Request 12 from ", sandboxText(h)),
      sandboxBlock(h),
      el("button", { type: "button", value: h, title: h, "aria-label": "Deny request 12" }, "Deny"),
    );
    replace(byId(IDS.main), art);
    const made = d.log.slice(before);
    assert.deepEqual(
      made.filter((x) => x.op === "createElement").map((x) => x.tag),
      ["bdi", "h2", "pre", "button", "article"],
    );
    for (const x of made.filter((x) => x.op === "setAttribute")) {
      assert.ok(["id", "aria-label", "class", "type", "value", "title"].includes(x.name), x.name);
    }
    assert.equal(texts(byId(IDS.main)).filter((t) => t === h).length, 2);
    assert.ok(elements(byId(IDS.main)).every((e) => !["IMG", "A", "SCRIPT", "SVG"].includes(e.tagName)));
    assert.ok(elements(byId(IDS.main)).every((e) => ![...e.attributes.keys()].some((k) => k.startsWith("on") || k === "href" || k === "src")));
    assert.equal(sandboxBlock(h).getAttribute("class"), "sandbox sandbox-block");
    assert.equal(sandboxText(h).tagName, "BDI");
  }
});

test("hostile text names no node", () => {
  fresh();
  for (const h of hostile) {
    assert.throws(() => el("article", { id: h }));
    assert.throws(() => el("input", { name: h }));
    assert.throws(() => navLink("#/env/" + h, "x"));
    assert.throws(() => navLink(h, "x"));
  }
});

test("routes", () => {
  const cases = {
    "": { page: "overview", env: null },
    "#": { page: "overview", env: null },
    "#/": { page: "overview", env: null },
    "#/requests": { page: "requests", env: null },
    "#/requests/default": { page: "requests", env: "default" },
    "#/proposals": { page: "proposals", env: null },
    "#/env/dev": { page: "env", env: "dev" },
    "#/env/my_env-2/doctor": { page: "doctor", env: "my_env-2" },
  };
  for (const [h, r] of Object.entries(cases)) {
    assert.deepEqual(parseRoute(h), r, h);
  }
  for (const h of ["#/env", "#/env/", "#/env/Dev", "#/env/-x", "#/env/a/b", "#/env/a/doctor/x", "#/requests/", "#/requests/a/b", "#/proposals/x", "#/x", "#x", "/", "#//", "#/env/" + "a".repeat(33), "#/env/a%2fb", "#/env/..", "#token=x", null, 1]) {
    assert.equal(parseRoute(h), null, String(h));
  }
  for (const h of ["#/", "#/requests", "#/requests/default", "#/proposals", "#/env/dev", "#/env/dev/doctor"]) {
    assert.equal(routeHash(parseRoute(h)), h);
  }
  assert.throws(() => routeHash({ page: "env", env: null }));
  assert.throws(() => routeHash({ page: "env", env: "<x>" }));
  assert.throws(() => routeHash({ page: "evil", env: null }));
  assert.throws(() => routeHash(null));
});

test("navLink sets an href only to one of the page's routes", () => {
  const d = fresh();
  const a = navLink("#/env/dev", "dev", { "aria-current": "page" });
  assert.equal(a.tagName, "A");
  assert.equal(a.getAttribute("href"), "#/env/dev");
  assert.equal(a.getAttribute("aria-current"), "page");
  assert.deepEqual(texts(a), ["dev"]);
  const hrefs = d.log.filter((x) => x.op === "setAttribute" && x.name === "href").map((x) => x.value);
  assert.deepEqual(hrefs, ["#/env/dev"]);
  for (const h of ["javascript:alert(1)", "https://evil.example/", "//evil.example", "#/x", "", "#", "/static/app.mjs", "#/env/dev\"onclick=\"x"]) {
    assert.throws(() => navLink(h, "x"), /dom: refused/, h);
  }
  assert.throws(() => navLink("#/", "x", { onclick: "x" }), /dom: refused/);
  assert.throws(() => navLink("#/", "x", { href: "javascript:x" }), /dom: refused/);
  assert.throws(() => navLink("#/", { nodeType: 1 }), /dom: refused/);
});

test("byId takes only the page's ids", () => {
  fresh();
  assert.equal(byId(IDS.main).tagName, "MAIN");
  assert.throws(() => byId("context"));
});

test("th scope takes only col and row", () => {
  fresh();
  assert.throws(() => el("th", { scope: "x" }));
  assert.throws(() => el("th", { scope: "" }));
  assert.equal(el("th", { scope: "col" }).getAttribute("scope"), "col");
  assert.equal(el("th", { scope: "row" }).getAttribute("scope"), "row");
});
