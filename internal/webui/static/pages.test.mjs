import { test } from "node:test";
import assert from "node:assert/strict";

import { Clock, FakeDaemon, FakeDocument, FakeStorage, FakeWindow, crypto as trackedCrypto, elements, settle, texts, token } from "./fakes.test.mjs";
import { STORAGE_KEY, createApp } from "./app.mjs";
import { IDS, useDocument } from "./dom.mjs";
import { createPages } from "./pages.mjs";

const T = token("a");
const ORIGIN = "http://127.0.0.1:7171";
const NOW = Date.parse("2026-10-18T12:00:00Z");
const iso = (ms) => new Date(ms).toISOString();
const HASH_POLLED = "1".repeat(64);
const HASH_FRESH = "2".repeat(64);
const HASH_NEWER = "3".repeat(64);

let live = null;

function view(id, over = {}) {
  return {
    id,
    kind: "url",
    key: "api.example.com",
    effect: "Opens https://api.example.com/ in your browser",
    state: "pending",
    created: iso(NOW - id * 1000),
    expires: iso(NOW + 3600000),
    ask: { context: "I need the docs" },
    ...over,
  };
}

function offers(hash) {
  return [
    { scope: "once", effect: "Opens the URL once" },
    { scope: "hours", effect: "Opens it, and allows the host for {hours} hours", hours_min: 1, hours_max: 24 },
    { scope: "always", effect: "Adds the host to config.toml", diff: ["  open_urls_allow = [", '+  "https://api.example.com",', "-  \"old\"", "  ]"], hash },
  ];
}

// setup starts the app on the requests page with a daemon that has the
// given pending requests per environment (a mutable world).
function setup(o = {}) {
  live?.stop();
  const clock = new Clock();
  const doc = new FakeDocument().shell();
  const win = new FakeWindow(o.hash ?? "#/requests", ORIGIN);
  const daemon = new FakeDaemon(T, ORIGIN);
  const world = { pending: { default: [view(1), view(2)] }, decided: { default: [] }, hash: HASH_POLLED, detailHash: null, ...(o.world ?? {}) };
  daemon.status.envs = Object.keys(world.pending).map((name) => ({ name, sandbox: "running", link: "linked", pending: world.pending[name].length }));
  daemon.handlers.status = () => {
    const envs = Object.keys(world.pending).map((name) => ({ name, sandbox: "running", link: "linked", pending: world.pending[name].length, ...(world.envExtra?.[name] ?? {}) }));
    return { body: { result: { daemon: daemon.status.daemon, envs } } };
  };
  daemon.handlers.env_details = ({ env }) => (world.details ? world.details(env) : { body: { result: { env, sessions: [], ports: [], keep: [] } } });
  daemon.handlers.requests = ({ env, all }) => ({ body: { result: { requests: all ? world.decided[env] ?? [] : world.pending[env] ?? [] } } });
  daemon.handlers.request = ({ env, id }) => {
    const v = (world.pending[env] ?? []).find((x) => x.id === id);
    if (!v) return { status: 404, body: { error: { kind: "not_found", message: "no such request" } } };
    return { body: { result: { request: v, offers: offers(world.fresh ? world.fresh() : world.hash) } } };
  };
  const storage = new FakeStorage({ [STORAGE_KEY]: T });
  useDocument(doc); // the pages make their nodes when created
  const pages = createPages({ now: () => NOW + (clock.t - 1_000_000), setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout });
  const app = createApp({ win, doc, fetch: daemon.fetch, crypto: trackedCrypto, storage: () => storage, now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout, page: pages });
  live = app;
  const main = () => doc.getElementById(IDS.main);
  const s = { clock, doc, win, daemon, world, app, main };
  s.buttons = (root = main()) => elements(root).filter((e) => e.tagName === "BUTTON");
  s.button = (label, root = main()) => {
    const b = s.buttons(root).find((x) => x.getAttribute("aria-label") === label || texts(x).join("") === label);
    assert.ok(b, "no button " + label + " in " + s.buttons(root).map((x) => x.getAttribute("aria-label") ?? texts(x).join("")).join(" | "));
    return b;
  };
  s.card = (id, env = "default") => elements(main()).find((e) => e.tagName === "ARTICLE" && e.getAttribute("aria-label") === "Request " + id + " of " + env);
  s.dialog = () => elements(doc.getElementById(IDS.dialogHost)).find((e) => e.tagName === "DIALOG") ?? null;
  s.run = async (ms) => {
    await s.clock.advance(ms);
    await settle();
  };
  // until runs the clock in small steps until fn holds, and stops there.
  s.until = async (fn) => {
    for (let i = 0; i < 100 && !fn(); i++) await s.run(50);
    assert.ok(fn(), "never happened");
  };
  s.start = async () => {
    app.start();
    await s.run(5000);
  };
  return s;
}

const labelOf = (b) => b.getAttribute("aria-label") ?? texts(b).join("");

test("the requests page lists pending newest first across environments, each card in order", async () => {
  const s = setup({ world: { pending: { default: [view(1), view(5, { created: iso(NOW - 500) })], dev: [view(3, { created: iso(NOW - 100) })] } } });
  await s.start();
  const order = elements(s.main()).filter((e) => e.tagName === "ARTICLE").map((e) => e.getAttribute("aria-label"));
  assert.deepEqual(order, ["Request 3 of dev", "Request 5 of default", "Request 1 of default"]);
  // Card order: heading, host's effect, the sandbox block, the answers.
  const card = s.card(1);
  const kids = card.childNodes.map((c) => c.tagName + "." + (c.getAttribute("class") ?? ""));
  assert.deepEqual(kids.slice(0, 1), ["H3."]);
  assert.match(kids[1], /^DIV\./);
  assert.match(texts(card.childNodes[1])[0], /Opens https:\/\/api.example.com/);
  assert.match(kids[2], /sandbox-says/);
  assert.match(kids[3], /answers/);
  assert.match(texts(card.childNodes[2]).join(" "), /The sandbox says:.*not checked/);
  assert.equal(elements(s.main()).filter((e) => e.tagName === "H1").length, 1);
});

test("a card's answers are the host's offers", async () => {
  const s = setup();
  await s.start();
  const labels = s.buttons(s.card(1)).map(labelOf);
  assert.deepEqual(labels, ["Deny request 1", "Allow once request 1", "Allow request 1 for the hours chosen", "Allow always request 1"]);
  const select = elements(s.card(1)).find((e) => e.tagName === "SELECT");
  assert.deepEqual(elements(select).filter((e) => e.tagName === "OPTION").map((o) => o.getAttribute("value")), ["1", "4", "8", "24"]);
  assert.equal(select.value, "8");
  // No approve-all, no multi-select anywhere.
  assert.ok(!s.buttons().some((b) => /approve|all requests|allow all/i.test(labelOf(b))));
  assert.equal(elements(s.main()).filter((e) => e.tagName === "INPUT" || e.getAttribute("type") === "checkbox").length, 0);
});

test("hostile sandbox text is shown as text nodes only, whole, never linkified", async () => {
  const evil = [
    "<img src=x onerror=alert(1)>",
    "‮evil‬ reversed",
    "A".repeat(10 * 1024),
    "javascript:alert(1)",
    "see https://evil.example/ and <a href=\"javascript:alert(1)\">x</a>",
  ];
  const s = setup({ world: { pending: { default: [view(1, { session: "<b>s</b>", ask: { context: evil[0], options: [evil[1], evil[3]], recommendation: evil[2], meanwhile: evil[4] } })] } } });
  await s.start();
  const card = s.card(1);
  const tags = new Set(elements(card).map((e) => e.tagName));
  for (const bad of ["IMG", "SCRIPT", "IFRAME", "B", "SVG", "STYLE"]) assert.ok(!tags.has(bad), bad);
  const all = texts(card);
  for (const e of evil) assert.ok(all.includes(e), "missing as one text node: " + e.slice(0, 30));
  assert.ok(all.includes("<b>s</b>"));
  // Each is inside the sandbox's block (bdi or pre.sandbox), nowhere host-styled.
  const sandboxed = elements(card).filter((e) => (e.getAttribute("class") ?? "").includes("sandbox") && e.tagName !== "DIV");
  assert.ok(sandboxed.length >= 6);
  for (const e of evil) assert.ok(sandboxed.some((n) => texts(n).includes(e)), "not isolated: " + e.slice(0, 30));
  // No attribute anywhere carries the text, and none is a link or handler.
  const attrs = s.doc.log.filter((l) => l.op === "setAttribute");
  assert.ok(!attrs.some((l) => /onerror|javascript:|<img|evil/i.test(l.value)));
  assert.ok(!attrs.some((l) => l.name === "href" && l.value !== "#/" && !l.value.startsWith("#/")));
});

test("every answer, Deny included, opens the confirmation and sends nothing until confirmed", async () => {
  const s = setup();
  await s.start();
  for (const label of ["Deny request 1", "Allow once request 1", "Allow request 1 for the hours chosen", "Allow always request 1"]) {
    s.button(label, s.card(1)).click();
    await s.run(0);
    const d = s.dialog();
    assert.ok(d && d.open, label + " opened no dialog");
    assert.equal(s.doc.modal, d);
    assert.equal(s.daemon.calls("answer").length, 0, label + " answered without a confirmation");
    d.pressEscape();
    await s.run(0);
    assert.equal(s.dialog(), null);
  }
  assert.equal(s.daemon.calls("answer").length, 0);
});

test("the dialog: Cancel is focused on open, Escape cancels and focus goes back to the button", async () => {
  const s = setup();
  await s.start();
  const opener = s.button("Deny request 2", s.card(2));
  opener.focus();
  opener.click();
  await s.run(0);
  const d = s.dialog();
  assert.equal(s.doc.activeElement, s.buttons(d).find((b) => texts(b).join("") === "Cancel"), "Cancel is not focused");
  assert.equal(d.getAttribute("aria-labelledby"), IDS.dialogTitle);
  assert.match(texts(d).join(" "), /Deny request 2\?/);
  assert.match(texts(d).join(" "), /Opens https:\/\/api.example.com/, "the host's effect is shown");
  d.pressEscape();
  await s.run(0);
  assert.equal(s.dialog(), null);
  assert.equal(s.doc.activeElement, opener);
  assert.equal(s.daemon.calls("answer").length, 0);
});

test("Deny sends no scope; once and hours send theirs, the hours clamped", async () => {
  const s = setup();
  s.daemon.handlers.answer = ({ id }) => ({ body: { result: { request: { ...view(id), state: "denied" }, recorded: true } } });
  await s.start();
  s.button("Deny request 1", s.card(1)).click();
  await s.run(0);
  const act = () => s.buttons(s.dialog()).find((b) => texts(b).join("").startsWith("Deny") || texts(b).join("").startsWith("Allow"));
  assert.equal(texts(act()).join(""), "Deny api.example.com");
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("answer")[0].body, { env: "default", id: 1, verdict: "deny" });
  assert.equal(s.dialog(), null, "the dialog closes on success");
  assert.equal(texts(s.doc.getElementById(IDS.status)).join(""), "Request 1 denied");

  const sel = elements(s.card(2)).find((e) => e.tagName === "SELECT");
  sel.value = "24";
  sel.dispatchEvent({ type: "change" });
  s.button("Allow request 2 for the hours chosen", s.card(2)).click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /Allow request 2 for 24 hours\?/);
  assert.match(texts(s.dialog()).join(" "), /allows the host for 24 hours/);
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("answer")[1].body, { env: "default", id: 2, verdict: "allow", scope: "hours", hours: 24 });
});

test("always: the dialog fetches the request afresh and answers with that offer's hash, never the polled one", async () => {
  const s = setup();
  s.world.hash = HASH_POLLED;
  s.daemon.handlers.answer = ({ id }) => ({ body: { result: { request: { ...view(id), state: "allowed", scope: "always" }, recorded: true } } });
  await s.start();
  const before = s.daemon.calls("request").length;
  assert.ok(before > 0, "the page polled request details");
  // The world changes: the diff the host would write now has another hash.
  s.world.hash = HASH_FRESH;
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  assert.equal(s.daemon.calls("request").length, before + 1, "the dialog fetched the request");
  const d = s.dialog();
  assert.match(texts(d).join("\n"), /allow = \[|open_urls_allow/);
  const lines = elements(d).filter((e) => (e.getAttribute("class") ?? "").includes("diff-line"));
  assert.deepEqual(lines.map((e) => e.getAttribute("class")), ["diff-line diff-ctx", "diff-line diff-add", "diff-line diff-del", "diff-line diff-ctx"]);
  s.buttons(d).find((b) => texts(b).join("") === "Allow api.example.com always").click();
  await s.run(0);
  assert.equal(s.daemon.calls("answer")[0].body.confirm_diff, HASH_FRESH);
  assert.notEqual(s.daemon.calls("answer")[0].body.confirm_diff, HASH_POLLED);
});

test("the action is disabled until the diff loads; both buttons are disabled while sending", async () => {
  const s = setup();
  let releaseRequest;
  let releaseAnswer;
  await s.start();
  s.daemon.handlers.request = () => new Promise((r) => {
    releaseRequest = () => r({ body: { result: { request: view(1), offers: offers(HASH_FRESH) } } });
  });
  s.daemon.handlers.answer = () => new Promise((r) => {
    releaseAnswer = () => r({ body: { result: { request: view(1, { state: "allowed" }), recorded: true } } });
  });
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  const d = s.dialog();
  const cancel = s.buttons(d).find((b) => texts(b).join("") === "Cancel");
  const act = s.buttons(d).find((b) => texts(b).join("") === "Allow api.example.com always");
  assert.equal(act.disabled, true, "enabled before the diff loaded");
  act.click();
  await s.run(0);
  assert.equal(s.daemon.calls("answer").length, 0, "answered before the diff loaded");
  assert.equal(cancel.disabled, false);
  releaseRequest();
  await s.run(0);
  assert.equal(act.disabled, false);
  act.click();
  await s.run(0);
  assert.equal(act.disabled, true, "action enabled while sending");
  assert.equal(cancel.disabled, true, "Cancel enabled while sending");
  act.click();
  d.pressEscape();
  await s.run(0);
  assert.equal(s.daemon.calls("answer").length, 1, "a second click sent a second answer");
  assert.ok(s.dialog(), "Escape closed the dialog while sending");
  releaseAnswer();
  await s.run(0);
  assert.equal(s.dialog(), null);
  assert.equal(texts(s.doc.getElementById(IDS.status)).join(""), "Request 1 allowed always");
});

test("stale_diff shows the new diff and the next confirm carries its hash", async () => {
  const s = setup();
  await s.start();
  s.world.hash = HASH_FRESH;
  let n = 0;
  s.daemon.handlers.answer = () => {
    n++;
    if (n === 1) {
      s.world.hash = HASH_NEWER;
      s.world.pending.default = [view(1, { attempts: 2 }), view(2)];
      return { status: 409, body: { error: { kind: "stale_diff", message: "config.toml changed" } } };
    }
    return { body: { result: { request: view(1, { state: "allowed" }), recorded: true } } };
  };
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  const act = () => s.buttons(s.dialog()).find((b) => texts(b).join("") === "Allow api.example.com always");
  act().click();
  await s.run(0);
  const d = s.dialog();
  assert.ok(d, "the dialog stays open");
  assert.match(texts(d).join(" "), /configuration changed.*review the new change/);
  assert.equal(act().disabled, false);
  assert.equal(s.buttons(d).find((b) => texts(b).join("") === "Cancel").disabled, false);
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("answer").map((r) => r.body.confirm_diff), [HASH_FRESH, HASH_NEWER]);
  assert.equal(s.dialog(), null);
});

test("an answer that fails shows the daemon's message and keeps the dialog", async () => {
  const s = setup();
  s.daemon.handlers.answer = () => ({ status: 409, body: { error: { kind: "already_answered", message: "request 1 was already answered by cli" } } });
  await s.start();
  s.button("Deny request 1", s.card(1)).click();
  await s.run(0);
  const act = s.buttons(s.dialog()).find((b) => texts(b).join("") === "Deny api.example.com");
  act.click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /already answered by cli/);
  assert.equal(act.disabled, true);
});

test("reflow guard: cards that moved have their buttons off for one second", async () => {
  const s = setup();
  await s.start();
  assert.ok(s.buttons(s.card(1)).every((b) => !b.disabled), "the first listing is not guarded");
  s.world.pending.default = [view(10, { created: iso(NOW + 1000) }), view(1), view(2)];
  await s.until(() => s.card(10));
  // 10 is new at the top; 1 and 2 moved down.
  for (const id of [10, 1, 2]) {
    const c = s.card(id);
    assert.ok(s.buttons(c).every((b) => b.disabled), "card " + id + " is not guarded");
  }
  await s.run(1000);
  for (const id of [10, 1, 2]) assert.ok(s.buttons(s.card(id)).every((b) => !b.disabled), "card " + id + " stays guarded");
  // A click during the guard does nothing.
  s.world.pending.default = [view(5, { created: iso(NOW + 5000) }), ...s.world.pending.default];
  await s.until(() => s.card(5));
  assert.ok(s.buttons(s.card(1)).every((b) => b.disabled));
  s.button("Deny request 1", s.card(1)).click();
  await s.run(0);
  assert.equal(s.dialog(), null);
});

test("a card whose place did not change is not guarded", async () => {
  const s = setup();
  await s.start();
  s.world.pending.default = [view(1), view(2), view(9, { created: iso(NOW - 99000) })];
  await s.until(() => s.card(9));
  assert.ok(s.buttons(s.card(1)).every((b) => !b.disabled));
  assert.ok(s.buttons(s.card(2)).every((b) => !b.disabled));
  assert.ok(s.buttons(s.card(9)).every((b) => b.disabled), "the new card is guarded");
});

test("a poll does not move focus, reset a select or rebuild a card whose data did not change", async () => {
  const s = setup();
  await s.start();
  const card = s.card(2);
  const deny = s.button("Deny request 2", card);
  const select = elements(card).find((e) => e.tagName === "SELECT");
  select.value = "24";
  select.dispatchEvent({ type: "change" });
  deny.focus();
  const main = s.main();
  const h1 = elements(main).find((e) => e.tagName === "H1");
  const writes = s.doc.log.length;
  await s.run(6000);
  assert.equal(s.card(2), card);
  assert.equal(s.doc.activeElement, deny);
  assert.equal(select.value, "24");
  assert.equal(elements(main).find((e) => e.tagName === "H1"), h1, "main was replaced by a poll");
  assert.ok(s.daemon.calls("status").length >= 3);
  assert.equal(s.doc.log.filter((l, i) => i >= writes && l.op === "createElement" && l.tag === "article").length, 0, "a card was rebuilt");
});

test("a poll that changes a card's data rebuilds it and keeps its focus and its select", async () => {
  const s = setup();
  await s.start();
  elements(s.card(2)).find((e) => e.tagName === "SELECT").value = "24";
  elements(s.card(2)).find((e) => e.tagName === "SELECT").dispatchEvent({ type: "change" });
  s.button("Allow once request 2", s.card(2)).focus();
  const old = s.card(2);
  s.world.pending.default = [view(1), view(2, { attempts: 3 })];
  await s.run(2000);
  const now = s.card(2);
  assert.notEqual(now, old, "the card was not rebuilt");
  assert.match(texts(now).join(" "), /Asked 3 times/);
  assert.equal(s.doc.activeElement, s.button("Allow once request 2", now));
  assert.equal(elements(now).find((e) => e.tagName === "SELECT").value, "24");
});

test("a poll that reorders the list keeps the focused button focused", async () => {
  const s = setup();
  await s.start();
  const b = s.button("Deny request 2", s.card(2));
  b.focus();
  s.world.pending.default = [view(7, { created: iso(NOW + 9000) }), view(1), view(2)];
  await s.run(2000);
  assert.equal(s.doc.activeElement, b);
});

test("a poll does not close an open dialog", async () => {
  const s = setup();
  await s.start();
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  const d = s.dialog();
  const cancel = s.buttons(d).find((b) => texts(b).join("") === "Cancel");
  assert.equal(s.doc.activeElement, cancel);
  s.world.pending.default = [view(8, { created: iso(NOW + 9000) }), view(2)];
  await s.run(6000);
  assert.equal(s.dialog(), d);
  assert.ok(d.open);
  assert.equal(s.doc.activeElement, cancel);
  assert.equal(s.doc.modal, d);
});

test("times are updated in place", async () => {
  const s = setup();
  await s.start();
  const t = elements(s.card(1)).find((e) => e.tagName === "TIME");
  const before = texts(t).join("");
  await s.run(120000);
  const again = elements(s.card(1)).find((e) => e.tagName === "TIME");
  assert.equal(again, t, "the time node was replaced");
  assert.notEqual(texts(t).join(""), before);
  assert.ok(t.getAttribute("title").startsWith("2026-10-18T"));
});

test("decided requests are listed with who decided and when", async () => {
  const s = setup({ world: { decided: { default: [view(4, { state: "allowed", scope: "hours", hours: 8, decided_by: "cli", decided_at: iso(NOW - 120000) }), view(3, { state: "denied", decided_by: "web", decided_at: iso(NOW - 60000) })] } } });
  await s.start();
  const d = [...elements(s.main())].filter((e) => e.tagName === "ARTICLE" && (e.getAttribute("class") ?? "").includes("decided"));
  assert.deepEqual(d.map((e) => e.getAttribute("aria-label")), ["Request 3 of default, denied", "Request 4 of default, allowed for 8 hours"]);
  assert.equal(s.buttons(d[0]).length, 0);
  assert.match(texts(d[0]).join(" "), /denied.*web.*1 min ago/);
});

test("the environment page: permissions, grants with revoke, checkpoints, link log", async () => {
  const s = setup({ hash: "#/env/default" });
  s.daemon.handlers.permissions = () => ({ body: { result: { env: "default", trust: "fallible", isolation: "container", roots: [{ name: "dev", host: "/Users/x/dev", container: "/work/dev", writable: ["."] }], aliases: [{ name: "build", command: ["make", "all"], policy: "ask" }], host_exec: { configured: true, effective: true }, open_urls: { mode: "ask" }, egress: { on: false }, mods: { sessions: 2, with_mod: 1 } } } });
  s.daemon.handlers.grants = () => ({ body: { result: { grants: [{ id: "g3", kind: "url", key: "api.example.com", created: iso(NOW - 1000), expires: iso(NOW + 3 * 3600000), left_seconds: 10800 }] } } });
  s.daemon.handlers.checkpoints = () => ({ body: { result: { env: "default", paths: [{ path: "/Users/x/dev/a", key: "k", resolved: true, shrunk: "fewer entries than before", checkpoints: [{ id: "c1", finished: iso(NOW - 600000), reason: "schedule", method: "copy-and-link", entries: 10, bytes: 1, linked: 0, skipped: 0 }] }] } } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: ["link up", "<b>x</b>"] } } });
  await s.start();
  const txt = texts(s.main()).join(" ");
  assert.ok(txt.indexOf("Trust level") < txt.indexOf("Isolation"), "trust first");
  assert.match(txt, /not limited by caboose under this isolation yet/);
  assert.match(txt, /\/work\/dev/);
  assert.match(txt, /2 h 59 min left/);
  assert.match(txt, /slower: walks every file/);
  assert.match(txt, /Warning: this path shrank/);
  assert.match(txt, /caboose -e default checkpoint restore ID --to DIR/);
  assert.ok(texts(s.main()).includes("link up\n<b>x</b>"));
  assert.doesNotMatch(txt, /take effect at/, "no drift, no note");
  assert.ok(elements(s.main()).filter((e) => e.tagName === "TH").every((e) => ["col", "row"].includes(e.getAttribute("scope"))));
  // Every table keeps its roles, and every cell its column's head as the
  // label a narrow window shows beside it (app.css).
  const tables = elements(s.main()).filter((e) => e.tagName === "TABLE");
  assert.equal(tables.length, 3);
  for (const t of tables) {
    assert.equal(t.getAttribute("role"), "table");
    const [thead, tbody] = t.childNodes;
    assert.deepEqual([thead.tagName, tbody.tagName, thead.getAttribute("role"), tbody.getAttribute("role")], ["THEAD", "TBODY", "rowgroup", "rowgroup"]);
    const headRow = thead.childNodes[0];
    assert.equal(headRow.getAttribute("role"), "row");
    const heads = headRow.childNodes.map((h) => {
      assert.equal(h.getAttribute("role"), "columnheader");
      return texts(h).join("");
    });
    assert.ok(tbody.childNodes.length > 0);
    for (const tr of tbody.childNodes) {
      assert.equal(tr.getAttribute("role"), "row");
      tr.childNodes.forEach((c, i) => {
        assert.deepEqual([c.tagName, c.getAttribute("role")], i === 0 ? ["TH", "rowheader"] : ["TD", "cell"]);
        assert.equal(c.getAttribute("data-label"), i > 0 && i < heads.length ? heads[i] : null, heads[i]);
      });
    }
  }
  // Revoke goes through the dialog.
  s.button("Revoke grant g3").click();
  await s.run(0);
  assert.ok(s.dialog().open);
  assert.equal(s.daemon.calls("revoke").length, 0);
  s.daemon.handlers.revoke = () => ({ body: { result: { revoked: "g3" } } });
  s.buttons(s.dialog()).find((b) => texts(b).join("") === "Revoke grant g3").click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("revoke")[0].body, { env: "default", grant: "g3" });
  assert.equal(texts(s.doc.getElementById(IDS.status)).join(""), "Grant g3 revoked");
  // The link log refreshes on demand; a failing call shows the daemon's words.
  const n = s.daemon.calls("link_log").length;
  s.daemon.handlers.link_log = () => ({ status: 404, body: { error: { kind: "unknown_method", message: "no such method" } } });
  s.button("Refresh the log").click();
  await s.run(0);
  assert.equal(s.daemon.calls("link_log").length, n + 1);
  assert.match(texts(s.main()).join(" "), /could not give the link log: no such method/);
});

test("the environment page says what takes effect at a restart, as text", async () => {
  const s = setup({ hash: "#/env/default" });
  s.daemon.handlers.permissions = () => ({ body: { result: { env: "default", trust: "hostile", isolation: "vm", host_exec: {}, open_urls: {}, egress: { on: true }, drift: ["the VM was created with trust fallible; the configuration says hostile", "<img src=x onerror=alert(1)>", 7] } } });
  await s.start();
  const txt = texts(s.main()).join(" ");
  assert.match(txt, /created with trust fallible; the configuration says hostile/);
  assert.match(txt, /take effect at\s+caboose -e default restart/);
  assert.ok(texts(s.main()).includes("<img src=x onerror=alert(1)>"), "shown as text");
  assert.ok(!elements(s.main()).some((e) => e.tagName === "IMG"));
  const items = elements(s.main()).filter((e) => e.tagName === "LI" && /^(the VM|<img)/.test(texts(e).join("")));
  assert.equal(items.length, 2);
  assert.ok(!elements(s.main()).some((e) => e.tagName === "LI" && texts(e).join("") === "7"), "a non-string entry is dropped");
  const code = elements(s.main()).find((e) => e.tagName === "CODE" && texts(e).join("") === "caboose -e default restart");
  assert.ok(code, "the command to copy");
});

test("the environment page keeps its nodes across polls", async () => {
  const s = setup({ hash: "#/env/default" });
  s.daemon.handlers.permissions = () => ({ body: { result: { env: "default", trust: "fallible", isolation: "vm", host_exec: {}, open_urls: {}, egress: { on: true } } } });
  s.daemon.handlers.grants = () => ({ body: { result: { grants: [{ id: "g3", kind: "url", key: "k", created: iso(NOW), expires: iso(NOW + 3600000), left_seconds: 3600 }] } } });
  s.daemon.handlers.checkpoints = () => ({ body: { result: { env: "default", paths: [] } } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: [] } } });
  await s.start();
  const b = s.button("Revoke grant g3");
  b.focus();
  const grant = b.parentNode;
  await s.run(25000);
  assert.equal(s.button("Revoke grant g3"), b);
  assert.equal(s.doc.activeElement, b);
  assert.equal(b.parentNode, grant);
});

test("the overview shows each environment and links to its requests", async () => {
  const s = setup({ hash: "#/", world: { pending: { default: [view(1)], dev: [] }, envExtra: { dev: { owner: "release" } } } });
  await s.start();
  const txt = texts(s.main()).join(" ");
  assert.match(txt, /v1.2.3/);
  assert.match(txt, /another daemon:\s+the release daemon \(127.0.0.1:7171\)/);
  assert.equal(s.daemon.calls("env_details").filter((r) => r.body.env === "dev").length, 0);
  assert.match(txt, /caboose -e default restart/);
  const links = elements(s.main()).filter((e) => e.tagName === "A").map((e) => e.getAttribute("href"));
  assert.ok(links.includes("#/requests/default"));
  assert.ok(links.includes("#/env/default"));
});

test("an environment another daemon owns is not listed on the requests page", async () => {
  const s = setup({ world: { pending: { default: [view(1)], dev: [view(2)] }, envExtra: { dev: { owner: "the release daemon" } } } });
  await s.start();
  assert.ok(s.card(1));
  assert.equal(s.card(2, "dev"), undefined);
  assert.equal(s.daemon.calls("requests").filter((r) => r.body.env === "dev").length, 0);
  assert.match(texts(s.main()).join(" "), /served by another daemon: dev/);
});

// isolatedIn: some bdi or .sandbox node (not a plain div) under root holds exactly text.
const isolatedIn = (root, text) => elements(root).some((e) => e.tagName !== "DIV" && (e.tagName === "BDI" || (e.getAttribute("class") ?? "").split(" ").includes("sandbox")) && texts(e).join("") === text);

test("every daemon-relayed value is isolated: key, session, kind, decided_by, last_error, the dialog's key, the revoke line, the log", async () => {
  const k = "k‮ey.example";
  const s = setup({
    world: {
      pending: { default: [view(1, { key: k, kind: "k‮ind", session: "s‮ess" })] },
      decided: { default: [view(4, { state: "allowed", scope: "once", decided_by: "d‮by", decided_at: iso(NOW - 1000) })] },
      envExtra: { default: { last_error: "e‮rr" } },
    },
  });
  s.daemon.handlers.grants = () => ({ body: { result: { grants: [{ id: "g3", kind: "gk‮", key: "gkey‮", created: iso(NOW - 1000), expires: iso(NOW + 3600000), left_seconds: 3600 }] } } });
  s.daemon.handlers.permissions = () => ({ body: { result: { env: "default", trust: "fallible", isolation: "container" } } });
  s.daemon.handlers.checkpoints = () => ({ body: { result: { env: "default", paths: [] } } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: ["log‮line"] } } });
  await s.start();
  const card = s.card(1);
  assert.ok(isolatedIn(card, k), "key");
  assert.ok(isolatedIn(card, "s‮ess"), "session");
  assert.ok(isolatedIn(card, "k‮ind"), "kind");
  assert.ok(isolatedIn(s.main(), "d‮by"), "decided_by");
  // The confirmation's button names the key inside a bdi, and keeps a plain aria-label.
  s.button("Allow once request 1", card).click();
  await s.run(0);
  const act = s.buttons(s.dialog()).find((b) => b.getAttribute("aria-label") === "Allow " + k + " once");
  assert.ok(act, "aria-label is the plain words");
  assert.ok(isolatedIn(act, k), "button key");
  s.dialog().pressEscape();
  await s.run(0);
  // The environment page.
  s.win.navigate("#/env/default");
  await s.run(5000);
  assert.ok(isolatedIn(s.main(), "gkey‮"), "grant key");
  assert.ok(isolatedIn(s.main(), "gk‮"), "grant kind");
  assert.ok(isolatedIn(s.main(), "log‮line"), "log");
  s.button("Revoke grant g3").click();
  await s.run(0);
  assert.ok(isolatedIn(s.dialog(), "gk‮"), "revoke kind");
  assert.ok(isolatedIn(s.dialog(), "gkey‮"), "revoke key");
});

test("last_error on the overview is isolated", async () => {
  const s = setup({ hash: "#/", world: { envExtra: { default: { last_error: "e‮rr" } } } });
  await s.start();
  assert.ok(isolatedIn(s.main(), "e‮rr"));
});

test("a diff that failed to load leaves the action disabled", async () => {
  const s = setup();
  await s.start();
  s.daemon.handlers.request = () => ({ status: 500, body: { error: { kind: "internal", message: "boom" } } });
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  const act = s.buttons(s.dialog()).find((b) => b.getAttribute("aria-label") === "Allow api.example.com always");
  assert.equal(act.disabled, true);
  act.click();
  await s.run(0);
  assert.equal(s.daemon.calls("answer").length, 0);
});

test("a fresh request that is no longer pending does not enable the action", async () => {
  const s = setup();
  await s.start();
  s.daemon.handlers.request = () => ({ body: { result: { request: view(1, { state: "allowed" }), offers: offers(HASH_FRESH) } } });
  s.button("Allow always request 1", s.card(1)).click();
  await s.run(0);
  const act = s.buttons(s.dialog()).find((b) => b.getAttribute("aria-label") === "Allow api.example.com always");
  assert.equal(act.disabled, true);
  act.click();
  await s.run(0);
  assert.equal(s.daemon.calls("answer").length, 0);
});

// ---- details, doctor ----

const DETAILS = (env) => ({
  body: {
    result: {
      env,
      isolation: "vm",
      image: { state: "stale", why: "layer changed" },
      sessions: ["main", "work"],
      resources: { memory_bytes: 1610612736, memory_limit_bytes: 4294967296, vnodes: 1200, vnodes_max: 5000 },
      ports: [{ port: 3000, url: "http://127.0.0.1:3000" }],
      keep: [{ path: "~/.config/gh", sync: true }, { path: "~/.cache/x", sync: false }],
      errors: [{ part: "resources", message: "engine did not answer" }],
    },
  },
});

test("the overview card shows the status fields and env_details facts, ports as text", async () => {
  const s = setup({ hash: "#/", world: { envExtra: { default: { trust: "hostile", checkpoints_line: "1 path, 2 checkpoints", mods_missing: 2 } }, details: DETAILS } });
  await s.start();
  const txt = texts(s.main()).join(" ");
  assert.match(txt, /Trust level hostile/);
  assert.match(txt, /1 path, 2 checkpoints/);
  assert.match(txt, /Sessions without the mod 2/);
  assert.match(txt, /Isolation vm/);
  assert.match(txt, /stale: rebuilt at the next restart \(layer changed\)/);
  assert.match(txt, /main\s+,\s+work/);
  assert.match(txt, /1.5 GiB of 4 GiB/);
  assert.match(txt, /1200 of 5000/);
  assert.ok(texts(s.main()).includes("http://localhost:3000"));
  assert.ok(!elements(s.main()).some((e) => e.tagName === "A" && /localhost/.test(e.getAttribute("href") ?? "")));
});

test("the environment page shows details, keep entries and errors, and the log's truncation", async () => {
  const s = setup({ hash: "#/env/default", world: { details: DETAILS } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: ["a", "b"], truncated: true } } });
  await s.start();
  const txt = texts(s.main()).join(" ");
  assert.match(txt, /Could not get\s+resources\s+:\s+engine did not answer/);
  const rows = elements(s.main()).filter((e) => e.tagName === "TR").map((r) => texts(r).join("|"));
  assert.ok(rows.includes("~/.config/gh|yes"));
  assert.ok(rows.includes("~/.cache/x|no"));
  assert.match(txt, /Showing the last 2 lines/);
  assert.ok(texts(s.main()).includes("a\nb"));
  const links = elements(s.main()).filter((e) => e.tagName === "A").map((e) => e.getAttribute("href"));
  assert.ok(links.includes("#/env/default/doctor"));
});

test("env_details is polled every 10 s while visible, one at a time, and not while hidden", async () => {
  const s = setup({ hash: "#/", world: { details: DETAILS } });
  await s.start();
  const n = s.daemon.calls("env_details").length;
  assert.ok(n >= 1);
  s.doc.setVisible(false);
  await s.run(60000);
  assert.ok(s.daemon.calls("env_details").length <= n + 1, "no polls while hidden");
  const m = s.daemon.calls("env_details").length;
  s.doc.setVisible(true);
  await s.run(25000);
  assert.ok(s.daemon.calls("env_details").length > m);
});

test("the doctor page runs only on Run, never on a poll, and groups findings problems first", async () => {
  const s = setup({ hash: "#/env/default/doctor" });
  const findings = [
    { level: "ok", label: "docker", message: "reachable" },
    { level: "note", label: "git", message: "no identity" },
    { level: "problem", label: "link", message: "not running", fix: "caboose -e default restart" },
    { level: "unchecked", label: "vm", message: "no vm here" },
  ];
  s.daemon.handlers.doctor = () => ({ body: { result: { findings, ran_at: iso(NOW - 60000), as: "daemon" } } });
  await s.start();
  await s.run(60000);
  assert.equal(s.daemon.calls("doctor").length, 0);
  assert.equal(s.daemon.calls("env_details").length, 0);
  s.button("Run the doctor").click();
  await s.run(0);
  assert.equal(s.daemon.calls("doctor").length, 1);
  assert.deepEqual(s.daemon.calls("doctor")[0].body, { env: "default" });
  await s.run(60000);
  assert.equal(s.daemon.calls("doctor").length, 1);
  const heads = elements(s.main()).filter((e) => e.tagName === "H2").map((e) => texts(e).join(""));
  assert.deepEqual(heads, ["Problems (1)", "Not checked (1)", "Notes (1)", "Passed (1)"]);
  const txt = texts(s.main()).join(" ");
  assert.match(txt, /caboose -e default doctor/);
  assert.ok(texts(s.main()).includes("caboose -e default restart"));
  assert.ok(elements(s.main()).some((e) => e.tagName === "TIME"));
});

test("hostile strings in details and doctor render as isolated text", async () => {
  const bad = '<img src=x onerror=alert(1)>\u202e';
  const s = setup({ hash: "#/env/default", world: { details: (env) => ({ body: { result: { env, isolation: bad, image: { state: bad, why: bad }, sessions: [bad], ports: [{ port: 80, url: bad }], keep: [{ path: bad, sync: true }], errors: [{ part: bad, message: bad }] } } }) } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: [bad], truncated: false } } });
  await s.start();
  assert.ok(!elements(s.main()).some((e) => e.tagName === "IMG"));
  assert.ok(isolatedIn(s.main(), bad), "session or isolation");
  assert.ok(isolatedIn(s.main(), bad + " (" + bad + ")"), "image");
  s.win.navigate("#/env/default/doctor");
  await s.run(5000);
  s.daemon.handlers.doctor = () => ({ body: { result: { findings: [{ level: "problem", label: bad, message: bad, fix: bad }], ran_at: iso(NOW), as: "daemon" } } });
  s.button("Run the doctor").click();
  await s.run(0);
  assert.ok(!elements(s.main()).some((e) => e.tagName === "IMG"));
  assert.ok(isolatedIn(s.main(), bad));
  assert.ok(elements(s.main()).some((e) => e.tagName === "CODE" && texts(e).join("") === bad));
});

function egressSetup(o = {}) {
  const s = setup({ hash: "#/env/default" });
  const lists = { allow: ["*.corp.example"], deny: [], ...(o.lists ?? {}) };
  s.daemon.handlers.permissions = () => ({ body: { result: { env: "default", trust: "fallible", isolation: o.isolation ?? "vm", profile: "vm.default", roots: [], aliases: [], host_exec: {}, open_urls: { mode: "ask" }, egress: { on: true, allow: lists.allow, deny: lists.deny } } } });
  s.daemon.handlers.grants = () => ({ body: { result: { grants: [] } } });
  s.daemon.handlers.checkpoints = () => ({ body: { result: { env: "default", paths: [] } } });
  s.daemon.handlers.link_log = () => ({ body: { result: { lines: [] } } });
  s.egressHash = HASH_FRESH;
  s.daemon.handlers.edit_egress = (p) => {
    if (p.confirm_diff === undefined) {
      return { body: { result: { diff: ["  egress = true", '- egress_allow = ["*.corp.example"]', '+ egress_allow = ["*.corp.example", "git.vpn.example"]'], hash: s.egressHash, effect: "allows connections to git.vpn.example from environment default <img src=x onerror=alert(1)>", written: false } } };
    }
    return { body: { result: { diff: ["+ x"], hash: p.confirm_diff, effect: "e", written: true } } };
  };
  s.input = () => elements(s.main()).find((e) => e.tagName === "INPUT" && e.getAttribute("aria-label") === "Egress entry to add");
  return s;
}

test("egress rules: shown under vm only, each entry with Remove", async () => {
  const c = egressSetup({ isolation: "container" });
  await c.start();
  assert.doesNotMatch(texts(c.main()).join(" "), /Egress rules/);
  assert.equal(c.input(), undefined);

  const s = egressSetup({ lists: { allow: ["*.corp.example", "10.0.0.0/8"], deny: ["ads.example"] } });
  await s.start();
  assert.match(texts(s.main()).join(" "), /Egress rules/);
  s.button("Remove *.corp.example from egress_allow");
  s.button("Remove 10.0.0.0/8 from egress_allow");
  s.button("Remove ads.example from egress_deny");
  assert.ok(s.input());
  assert.equal(s.daemon.calls("edit_egress").length, 0, "nothing is edited on a poll");
});

test("egress add: the dialog shows the daemon's diff and effect, then confirms with that hash", async () => {
  const s = egressSetup();
  await s.start();
  s.input().value = "git.vpn.example";
  s.button("Add").click();
  await s.run(0);
  const calls = s.daemon.calls("edit_egress");
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body, { env: "default", add_allow: ["git.vpn.example"] });
  const d = s.dialog();
  assert.ok(d.open);
  const txt = texts(d).join(" ");
  assert.match(txt, /Add git.vpn.example to egress_allow\?/);
  assert.match(txt, /allows connections to git.vpn.example from environment default/);
  assert.ok(!elements(d).some((e) => e.tagName === "IMG"), "the effect became markup");
  const lines = elements(d).filter((e) => (e.getAttribute("class") ?? "").includes("diff-line"));
  assert.deepEqual(lines.map((e) => e.getAttribute("class")), ["diff-line diff-ctx", "diff-line diff-del", "diff-line diff-add"]);
  s.buttons(d).find((b) => texts(b).join("") === "Add git.vpn.example to egress_allow").click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("edit_egress")[1].body, { env: "default", add_allow: ["git.vpn.example"], confirm_diff: HASH_FRESH });
  assert.equal(s.dialog(), null);
  assert.equal(s.input().value, "", "the entry stays after it was added");
  assert.match(texts(s.doc.getElementById(IDS.status)).join(""), /Add git.vpn.example to egress_allow: done/);
});

test("egress remove and deny: the params name the list; Cancel sends nothing", async () => {
  const s = egressSetup({ lists: { deny: ["ads.example"] } });
  await s.start();
  s.button("Remove ads.example from egress_deny").click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("edit_egress")[0].body, { env: "default", remove_deny: ["ads.example"] });
  s.buttons(s.dialog()).find((b) => texts(b).join("") === "Cancel").click();
  await s.run(0);
  assert.equal(s.dialog(), null);
  assert.equal(s.daemon.calls("edit_egress").length, 1);

  s.input().value = "bad.example";
  elements(s.main()).find((e) => e.tagName === "SELECT" && e.getAttribute("aria-label") === "List to add the entry to").value = "deny";
  s.button("Add").click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("edit_egress")[1].body, { env: "default", add_deny: ["bad.example"] });
});

test("egress: stale_diff reloads the diff and the next confirm carries the new hash", async () => {
  const s = egressSetup();
  await s.start();
  let n = 0;
  const show = s.daemon.handlers.edit_egress;
  s.daemon.handlers.edit_egress = (p) => {
    if (p.confirm_diff !== undefined && ++n === 1) {
      s.egressHash = HASH_NEWER;
      return { status: 409, body: { error: { kind: "stale_diff", message: "config.toml changed" } } };
    }
    return show(p);
  };
  s.input().value = "git.vpn.example";
  s.button("Add").click();
  await s.run(0);
  const act = () => s.buttons(s.dialog()).find((b) => texts(b).join("") === "Add git.vpn.example to egress_allow");
  act().click();
  await s.run(0);
  assert.ok(s.dialog(), "the dialog stays open");
  assert.match(texts(s.dialog()).join(" "), /review the new change/);
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("edit_egress").filter((c) => c.body.confirm_diff).map((c) => c.body.confirm_diff), [HASH_FRESH, HASH_NEWER]);
  assert.equal(s.dialog(), null);
});

test("egress: what the daemon refuses is its message, and the action stays off", async () => {
  const s = egressSetup();
  await s.start();
  s.daemon.handlers.edit_egress = () => ({ status: 400, body: { error: { kind: "bad_request", message: 'egress_allow: "not a host" is not a name, *.name, CIDR or address' } } });
  s.input().value = "not a host";
  s.button("Add").click();
  await s.run(0);
  assert.match(texts(s.main()).join(" "), /no spaces or commas/, "the hint");
  const d = s.dialog();
  assert.match(texts(d).join(" "), /is not a name, \*\.name, CIDR or address/);
  assert.equal(s.buttons(d).find((b) => texts(b).join("") === "Add not a host to egress_allow").disabled, true);
  // An empty entry opens nothing.
  s.buttons(d).find((b) => texts(b).join("") === "Cancel").click();
  await s.run(0);
  s.input().value = "  ";
  s.button("Add").click();
  await s.run(0);
  assert.equal(s.dialog(), null);
});

// ---- proposals ----

function proposal(file, over = {}) {
  return {
    file,
    modified: iso(NOW - 3600000),
    title: "Mount other",
    reason: "its docs",
    parts: [{ kind: "root", summary: "mount /home/u/src/other read-only at /work/other", diff: [" [roots]", ' dev = "~/dev"', '+other = "~/src/other"'] }],
    refusals: [],
    warnings: ["Everything under /home/u/src/other becomes readable from the sandbox."],
    web_apply: true,
    confirm: { kind: "type_name", name: "other" },
    needs_build: false,
    hash: HASH_POLLED,
    ...over,
  };
}

function proposalsSetup(list) {
  const s = setup({ hash: "#/proposals", world: { pending: { default: [], dev: [] } } });
  s.list = list;
  s.daemon.handlers.proposals = ({ env }) => ({ body: { result: { proposals: env === "default" ? s.list : [] } } });
  s.daemon.handlers.proposal_apply = () => ({ body: { result: { applied: true, notes: ["Wrote the roots to ~/.caboose/envs/default/config.toml: caboose restart mounts it."] } } });
  s.daemon.handlers.proposal_delete = () => ({ body: { result: null } });
  s.cardOf = (file) => elements(s.main()).find((e) => e.tagName === "ARTICLE" && e.getAttribute("aria-label") === "Proposal " + file + " of default");
  return s;
}

test("proposals: each with the host's parts and diff, the sandbox's words apart, Apply only where the page may", async () => {
  const s = proposalsSetup([
    proposal("other.toml"),
    proposal("dot.toml", { title: "Add graphviz", parts: [{ kind: "packages", summary: "the packages of [apko.default]", diff: ["+ graphviz"] }], web_apply: false, web_apply_why: "it is too long to show here", needs_build: true, confirm: { kind: "none" } }),
    proposal("ssh.toml", { parts: [], refusals: ["The root ~/.ssh is in ~/.ssh: that is where keys live."], warnings: [], web_apply: false, web_apply_why: "caboose refuses it", confirm: { kind: "none" } }),
  ]);
  await s.start();
  assert.equal(s.daemon.calls("proposals").length, 2, "one list per environment");
  const card = s.cardOf("other.toml");
  assert.ok(card);
  const lines = elements(card).filter((e) => (e.getAttribute("class") ?? "").includes("diff-line"));
  assert.deepEqual(lines.map((e) => e.getAttribute("class")), ["diff-line diff-ctx", "diff-line diff-ctx", "diff-line diff-add"]);
  assert.match(texts(card).join(" "), /read-only at \/work\/other/);
  assert.match(texts(card).join(" "), /The sandbox says:/);
  assert.ok(isolatedIn(card, "Mount other"));
  assert.ok(isolatedIn(card, "its docs"));
  assert.deepEqual(s.buttons(card).map(labelOf), ["Apply proposal other.toml of default", "Delete proposal other.toml of default"]);

  const dot = s.cardOf("dot.toml");
  assert.deepEqual(s.buttons(dot).map(labelOf), ["Delete proposal dot.toml of default"], "what the daemon says the page may not apply");
  assert.match(texts(dot).join(" "), /too long to show here/);
  assert.ok(elements(dot).some((e) => e.tagName === "CODE" && texts(e).join("") === "caboose apply"));

  const ssh = s.cardOf("ssh.toml");
  assert.deepEqual(s.buttons(ssh).map(labelOf), ["Delete proposal ssh.toml of default"], "a refused proposal offers only Delete");
  assert.match(texts(ssh).join(" "), /where keys live/);
  assert.equal(s.daemon.calls("proposal_apply").length + s.daemon.calls("proposal_delete").length, 0, "nothing on a poll");
});

test("proposals: hostile titles and reasons are text, whole, isolated", async () => {
  const evil = ["<img src=x onerror=alert(1)>", "‮evil‬ reversed", "R".repeat(8 * 1024), '<a href="javascript:alert(1)">x</a>'];
  const s = proposalsSetup([proposal("evil.toml", { title: evil[0], reason: evil[1] }), proposal("long.toml", { title: evil[3], reason: evil[2] })]);
  await s.start();
  const tags = new Set(elements(s.main()).map((e) => e.tagName));
  for (const bad of ["IMG", "SCRIPT", "IFRAME"]) assert.ok(!tags.has(bad), bad);
  for (const a of elements(s.main()).filter((e) => e.tagName === "A")) assert.match(a.getAttribute("href"), /^#\/env\/[a-z]+$/, "only the page's own links");
  for (const e of evil) assert.ok(isolatedIn(s.main(), e), "not isolated whole: " + e.slice(0, 20));
  // The sandbox's words are never inside the host's summary.
  const summaries = elements(s.cardOf("evil.toml")).filter((e) => e.tagName === "P" && texts(e).join("").startsWith("A host directory"));
  assert.ok(summaries.length === 1 && !texts(summaries[0]).join("").includes("<img"));
});

test("proposal apply: fetched again as the dialog opens, the whole diff, the name typed, then that hash", async () => {
  const s = proposalsSetup([proposal("other.toml")]);
  await s.start();
  s.list = [proposal("other.toml", { hash: HASH_FRESH, parts: [{ kind: "root", summary: "mount it", diff: ['+other = "~/src/other"', "-gone = 1"] }] })];
  const before = s.daemon.calls("proposals").length;
  s.button("Apply proposal other.toml of default").click();
  await s.run(0);
  assert.equal(s.daemon.calls("proposals").length, before + 1, "the dialog asks for it again");
  const d = s.dialog();
  assert.ok(d.open);
  const lines = elements(d).filter((e) => (e.getAttribute("class") ?? "").includes("diff-line"));
  assert.deepEqual(lines.map((e) => texts(e).join("")), ['+other = "~/src/other"', "-gone = 1"], "the diff as fetched now, not as polled");
  const act = () => s.buttons(d).find((b) => texts(b).join("") === "Apply other.toml");
  assert.equal(act().disabled, true, "not before the name is typed");
  const input = elements(d).find((e) => e.tagName === "INPUT");
  assert.match(input.getAttribute("aria-label"), /type the name other/);
  input.value = "othre";
  input.dispatchEvent({ type: "input" });
  assert.equal(act().disabled, true);
  input.value = "other";
  input.dispatchEvent({ type: "input" });
  assert.equal(act().disabled, false);
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("proposal_apply")[0].body, { env: "default", file: "other.toml", hash: HASH_FRESH, typed: "other" });
  assert.equal(s.dialog(), null);
  assert.match(texts(s.doc.getElementById(IDS.status)).join(""), /Applied proposal other.toml/);
  await s.run(2000);
  assert.match(texts(s.main()).join(" "), /caboose restart mounts it/);
  assert.equal(s.cardOf("other.toml"), undefined, "an applied proposal is not offered again");
});

test("proposal apply: stale_diff shows the new change, and the next apply carries its hash", async () => {
  const s = proposalsSetup([proposal("sec.toml", { parts: [{ kind: "section", summary: "the section foo", diff: ["+ RUN foo"] }], confirm: { kind: "none" } })]);
  await s.start();
  let n = 0;
  s.daemon.handlers.proposal_apply = (p) => {
    if (++n === 1) {
      s.list = [proposal("sec.toml", { hash: HASH_NEWER, parts: [{ kind: "section", summary: "the section foo", diff: ["+ RUN foo2"] }], confirm: { kind: "none" } })];
      return { status: 409, body: { error: { kind: "stale_diff", message: "the Dockerfile changed" } } };
    }
    return { body: { result: { applied: true, notes: ["rebuilt at the next caboose restart"] } } };
  };
  s.button("Apply proposal sec.toml of default").click();
  await s.run(0);
  assert.equal(elements(s.dialog()).filter((e) => e.tagName === "INPUT").length, 0, "a section takes no name");
  const act = () => s.buttons(s.dialog()).find((b) => texts(b).join("") === "Apply sec.toml");
  act().click();
  await s.run(0);
  assert.ok(s.dialog(), "the dialog stays open");
  assert.match(texts(s.dialog()).join(" "), /review the new change/);
  assert.ok(texts(s.dialog()).includes("+ RUN foo2"));
  act().click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("proposal_apply").map((c) => c.body.hash), [HASH_POLLED, HASH_NEWER]);
  assert.equal(s.dialog(), null);
});

test("proposal apply: one no longer applicable is refused in the dialog, with nothing sent", async () => {
  const s = proposalsSetup([proposal("other.toml")]);
  await s.start();
  s.list = [proposal("other.toml", { web_apply: false, web_apply_why: "caboose refuses it as it is", refusals: ["no"] })];
  s.button("Apply proposal other.toml of default").click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /can no longer be applied from this page: caboose refuses it/);
  assert.equal(s.buttons(s.dialog()).find((b) => texts(b).join("") === "Apply other.toml").disabled, true);
  assert.equal(s.daemon.calls("proposal_apply").length, 0);
});

test("proposal delete: confirmed, then sent by its file name and the hash shown; Cancel sends nothing", async () => {
  const s = proposalsSetup([proposal("other.toml")]);
  await s.start();
  s.button("Delete proposal other.toml of default").click();
  await s.run(0);
  s.buttons(s.dialog()).find((b) => texts(b).join("") === "Cancel").click();
  await s.run(0);
  assert.equal(s.daemon.calls("proposal_delete").length, 0);
  s.button("Delete proposal other.toml of default").click();
  await s.run(0);
  s.buttons(s.dialog()).find((b) => texts(b).join("") === "Delete other.toml").click();
  await s.run(0);
  assert.deepEqual(s.daemon.calls("proposal_delete")[0].body, { env: "default", file: "other.toml", hash: proposal("other.toml").hash });
  assert.match(texts(s.doc.getElementById(IDS.status)).join(""), /Deleted proposal other.toml/);
});

// ---- proposals that build the image ----

const PKG = { kind: "packages", summary: "the packages of [apko.default]", diff: ["+ graphviz"] };

function buildSetup() {
  const s = proposalsSetup([proposal("dot.toml", { title: "Add graphviz", parts: [PKG], warnings: [], needs_build: true, confirm: { kind: "none" } })]);
  s.job = null;
  s.world.envExtra = { default: {} };
  Object.defineProperty(s.world.envExtra, "default", { get: () => (s.job ? { job: s.job } : {}) });
  s.daemon.handlers.proposal_build = (p) => ({ body: { result: { job: { kind: "apply", state: "running", since: iso(NOW), proposal: p.file } } } });
  s.jobBox = () => elements(s.main()).find((e) => e.getAttribute("aria-label") === "Applying proposal dot.toml");
  s.approve = async () => {
    s.button("Apply proposal dot.toml of default").click();
    await s.run(0);
    s.buttons(s.dialog()).find((b) => texts(b).join("") === "Build and apply dot.toml").click();
    await s.run(0);
  };
  return s;
}

test("proposal build: one that no longer needs a build when the dialog opens is refused there, with nothing sent", async () => {
  const s = buildSetup();
  await s.start();
  s.list = [proposal("dot.toml", { title: "Add graphviz" })];
  s.button("Apply proposal dot.toml of default").click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /it changed: close this, and look at it again/);
  assert.equal(s.buttons(s.dialog()).find((b) => texts(b).join("") === "Build and apply dot.toml").disabled, true);
  assert.equal(s.daemon.calls("proposal_build").length, 0);
  assert.equal(s.daemon.calls("proposal_apply").length, 0);
});

test("proposal build: Build and apply sends proposal_build with the hash, then the page follows the job: its lines, then its notes", async () => {
  const s = buildSetup();
  await s.start();
  const card = s.cardOf("dot.toml");
  assert.deepEqual(texts(s.button("Apply proposal dot.toml of default", card)), ["Build and apply"]);
  s.button("Apply proposal dot.toml of default").click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /built with the packages first/);
  assert.match(texts(s.dialog()).join(" "), /keeps the image it runs until its next restart/);
  s.buttons(s.dialog()).find((b) => texts(b).join("") === "Build and apply dot.toml").click();
  await s.run(0);
  assert.equal(s.daemon.calls("proposal_apply").length, 0);
  assert.deepEqual(s.daemon.calls("proposal_build")[0].body, { env: "default", file: "dot.toml", hash: HASH_POLLED });
  assert.equal(s.dialog(), null);
  assert.match(texts(s.doc.getElementById(IDS.status)).join(""), /Building the image for proposal dot.toml/);
  await s.run(2000);
  assert.match(texts(s.jobBox()).join(" "), /building the image/, "shown before status has it");
  assert.equal(s.buttons(s.cardOf("dot.toml")).length, 0, "nothing offered while it is applied");

  s.job = { kind: "apply", state: "running", since: iso(NOW), proposal: "dot.toml", lines: ["#1 load build definition", "<img src=x onerror=alert(1)>"] };
  await s.until(() => texts(s.jobBox() ?? { childNodes: [] }).join(" ").includes("load build definition"));
  assert.ok(isolatedIn(s.jobBox(), "#1 load build definition\n<img src=x onerror=alert(1)>"), "the lines are text");
  assert.ok(!elements(s.main()).some((e) => e.tagName === "IMG"));

  s.job = { ...s.job, state: "done", finished: iso(NOW + 60000), lines: ["Wrote config.toml."], notes: ["Built the image with the packages of [apko.default]. The sandbox keeps its current image until its next restart (caboose restart, which ends running sessions)."] };
  s.list = [];
  await s.until(() => texts(s.jobBox() ?? { childNodes: [] }).join(" ").includes("Applied proposal dot.toml"));
  assert.match(texts(s.jobBox()).join(" "), /until its next restart \(caboose restart/);
  assert.ok(!texts(s.jobBox()).join(" ").includes("Wrote config.toml."), "a done job says its notes, not its lines");
});

test("proposal build: a failed job says why, and the proposal is offered again", async () => {
  const s = buildSetup();
  await s.start();
  await s.approve();
  s.job = { kind: "apply", state: "failed", since: iso(NOW), finished: iso(NOW), proposal: "dot.toml", lines: ["ERROR: graphviz: no such package"], error: "the build failed (exit 1): nothing was written, and the proposal is left pending" };
  await s.until(() => texts(s.jobBox() ?? { childNodes: [] }).join(" ").includes("failed"));
  const box = texts(s.jobBox()).join(" ");
  assert.match(box, /Applying proposal dot.toml failed: the build failed/);
  assert.match(box, /no such package/);
  assert.deepEqual(s.buttons(s.cardOf("dot.toml")).map(labelOf), ["Apply proposal dot.toml of default", "Delete proposal dot.toml of default"]);
});

test("proposal build: busy and stale are said in the dialog", async () => {
  const s = buildSetup();
  await s.start();
  let n = 0;
  s.daemon.handlers.proposal_build = () => {
    if (++n === 1) return { status: 409, body: { error: { kind: "busy", message: "a job already runs for default" } } };
    s.list = [proposal("dot.toml", { title: "Add graphviz", parts: [{ ...PKG, diff: ["+ graphviz", "+ jq"] }], warnings: [], needs_build: true, confirm: { kind: "none" }, hash: HASH_NEWER })];
    return { status: 409, body: { error: { kind: "stale_diff", message: "changed" } } };
  };
  await s.approve();
  assert.ok(s.dialog(), "the dialog stays open");
  assert.match(texts(s.dialog()).join(" "), /a job already runs/);
  const act = () => s.buttons(s.dialog()).find((b) => texts(b).join("") === "Build and apply dot.toml");
  assert.equal(act().disabled, false, "busy may be tried again");
  act().click();
  await s.run(0);
  assert.match(texts(s.dialog()).join(" "), /review the new change/);
  assert.ok(texts(s.dialog()).includes("+ jq"));
});
