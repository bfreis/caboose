import { test } from "node:test";
import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";

import { CODE, Clock, FakeDaemon, crypto as trackedCrypto, FakeDocument, FakeStorage, FakeWindow, elements, json, mac, settle, texts, token } from "./fakes.test.mjs";
import { ApiError, createApi, proofMAC } from "./api.mjs";
import { BUSY_MAX_MS, BUSY_MIN_MS, BUSY_SUSPECT_ROUNDS, POLL_MS, REFUSED_ROUNDS, SLOW_MS, STORAGE_KEY, createApp, parseFragment, tokenStore } from "./app.mjs";
import { IDS, el, navLink, sandboxText } from "./dom.mjs";

const T = token("a"); // the daemon's token
const OLD = token("b"); // another, stored by an earlier visit
const ORIGIN = "http://127.0.0.1:7171";

// stubPage is the page of a test that names none: its route's title, and
// for the overview the environments status names, as links where a name
// can be one.
const envNameRe = /^[a-z0-9][a-z0-9_-]{0,31}$/;
function stubTitle(route) {
  if (route === null) return "No such page";
  switch (route.page) {
    case "overview":
      return "Overview";
    case "requests":
      return route.env === null ? "Requests" : "Requests of " + route.env;
    case "proposals":
      return "Proposals";
    case "env":
      return "Environment " + route.env;
    case "doctor":
      return "Doctor of " + route.env;
  }
  return "No such page";
}
const stubPage = {
  render(ctx) {
    const route = ctx.route;
    const title = stubTitle(route);
    const body = [];
    if (route?.page === "overview" && ctx.status !== null) {
      const items = ctx.status.envs.map((e) => {
        const name = typeof e?.name === "string" ? e.name : "";
        const label = envNameRe.test(name) ? navLink("#/env/" + name, name) : sandboxText(name);
        const pending = Number.isSafeInteger(e?.pending) && e.pending > 0 ? ": " + e.pending + " pending" : "";
        return el("li", null, label, pending);
      });
      body.push(el("ul", null, items));
    }
    return { title, body };
  },
};

// live is the app of the test running: one test's app never outlives it,
// since every app draws into the document dom.mjs was last given.
let live = null;

function setup(o = {}) {
  live?.stop();
  const clock = new Clock();
  const doc = new FakeDocument().shell();
  const win = new FakeWindow(o.hash ?? "", ORIGIN);
  const log = [];
  const daemon = new FakeDaemon(o.daemonToken ?? T, ORIGIN, { log, now: clock.now });
  // net.server is what answers on the page's address: a test may put
  // another server in the daemon's place.
  const net = { server: daemon };
  const storage = new FakeStorage(o.stored === undefined ? {} : { [STORAGE_KEY]: o.stored }, o.storageBroken);
  const app = createApp({
    win,
    doc,
    fetch: (url, init) => net.server.fetch(url, init),
    crypto: o.crypto === undefined ? trackedCrypto : o.crypto,
    storage: () => storage,
    now: clock.now,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    page: o.page ?? stubPage,
  });
  live = app;
  const main = () => texts(doc.getElementById(IDS.main)).join("");
  const region = (id) => texts(doc.getElementById(id)).join("");
  return { clock, doc, win, daemon, net, log, storage, app, main, region };
}

// assertProvenFirst checks that every request in requests that carries
// a token has a proof of its own: one that verified for that very token,
// answered by the same server at the same instant, and that no other
// request used. A proof carries no token.
function assertProvenFirst(daemon, requests = daemon.requests) {
  const unused = [];
  let n = 0;
  for (const r of requests) {
    if (r.method === "proof") {
      assert.equal(r.token, undefined, "a proof carries no token");
      if (r.proofKey !== null) unused.push(r);
      continue;
    }
    if (r.token === undefined) continue;
    const i = unused.findIndex((p) => p.proofKey === r.token && p.server === r.server && p.at === r.at);
    assert.ok(i >= 0, r.method + " sent the token with no proof of its own just before it");
    unused.splice(i, 1);
    n++;
  }
  return n;
}

test("parseFragment", () => {
  const C = CODE;
  assert.deepEqual(parseFragment("#code=" + C), { kind: "code", code: C, route: { page: "overview", env: null } });
  assert.deepEqual(parseFragment("#code=" + C + "&to=requests"), { kind: "code", code: C, route: { page: "requests", env: null } });
  assert.deepEqual(parseFragment("#code=" + C + "&to=requests/dev"), { kind: "code", code: C, route: { page: "requests", env: "dev" } });
  assert.deepEqual(parseFragment("#code=" + C + "&to=proposals"), { kind: "code", code: C, route: { page: "proposals", env: null } });
  assert.deepEqual(parseFragment("#code=" + C + "&to=env/dev"), { kind: "code", code: C, route: { page: "env", env: "dev" } });
  // to= names only the pages it may: anything else is the overview.
  for (const to of ["env/dev/doctor", "javascript:alert(1)", "../x", "env/Dev", "env/", "", "//evil.example", "requests/", "x"]) {
    assert.deepEqual(parseFragment("#code=" + C + "&to=" + to).route, { page: "overview", env: null }, to);
  }
  for (const h of ["#code=", "#code=" + C.slice(1), "#code=" + C + "a", "#code=" + "a".repeat(42) + "+", "#code=" + C + "&x=1", "#code=" + C + "&to=requests&to=proposals", "#x&code=" + C, "#/requests?code=" + C, "#CODE=" + C, "#code=" + C + "%20", "#to=requests&code=" + C]) {
    assert.deepEqual(parseFragment(h), { kind: "bad" }, h);
  }
  // A token is never taken from the address, in any form.
  for (const h of ["#token=" + T, "#token=" + T + "&to=requests", "#TOKEN=" + T, "#x&token=" + T, "#/requests?token=" + T, "#code=" + C + "&token=" + T]) {
    assert.deepEqual(parseFragment(h), { kind: "bad" }, h);
  }
  assert.deepEqual(parseFragment("#/env/dev"), { kind: "route", route: { page: "env", env: "dev" } });
  // An environment named like the fragment's words is a route still.
  assert.deepEqual(parseFragment("#/env/code"), { kind: "route", route: { page: "env", env: "code" } });
  assert.deepEqual(parseFragment("#/env/token"), { kind: "route", route: { page: "env", env: "token" } });
  assert.deepEqual(parseFragment("#/nope"), { kind: "route", route: null });
  assert.deepEqual(parseFragment(""), { kind: "route", route: { page: "overview", env: null } });
});

test("a good fragment is stripped at once, its code redeemed, and the token stored after its proof and ping", async () => {
  const s = setup({ hash: "#code=" + CODE + "&to=requests" });
  // What the address held when each request went out.
  const seen = [];
  const serve = s.daemon.fetch;
  s.daemon.fetch = (url, init) => (seen.push(s.win.location.hash), serve(url, init));
  s.app.start();
  // Before anything else ran: the address no longer holds the code,
  // and nothing but the redeem was sent.
  assert.deepEqual(s.win.replaced, ["/#/requests"]);
  assert.equal(s.win.location.hash, "#/requests");
  assert.deepEqual(s.daemon.requests.map((r) => r.method), ["redeem"].slice(0, s.daemon.requests.length));
  assert.ok(seen.every((h) => h === "#/requests"), "a request went out while the address held the code");
  assert.equal(s.storage.m.get(STORAGE_KEY), undefined);
  await settle();
  assert.deepEqual(s.daemon.requests.map((r) => r.method), ["redeem", "proof", "ping", "proof", "status"]);
  const redeem = s.daemon.calls("redeem")[0];
  assert.equal(redeem.token, undefined, "the redeem carries no token");
  assert.deepEqual(redeem.body, { code: CODE });
  assert.equal(redeem.init.method, "POST");
  assert.equal(redeem.init.mode, "same-origin");
  assert.equal(redeem.init.credentials, "omit");
  assertProvenFirst(s.daemon);
  // No address the page ever had held the token.
  assert.ok(s.win.replaced.every((u) => !u.includes(T) && !u.includes(CODE)));
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  assert.equal(s.app.state, "ok");
  assert.deepEqual(s.app.route, { page: "requests", env: null });
});

test("a redeemed token is not stored before its ping succeeds", async () => {
  const s = setup({ hash: "#code=" + CODE, stored: OLD });
  s.daemon.answers.ping = { status: 500, body: { error: { kind: "internal", message: "x" } } };
  s.app.start();
  await settle();
  assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
  // A ping refused as unauthorized (the token rotated since) drops it,
  // and the stored one stays.
  s.daemon.answers.ping = { status: 401, body: { error: { kind: "unauthorized", message: "x" } } };
  await s.clock.advance(SLOW_MS);
  assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
  assert.ok(s.daemon.calls("ping").length >= 2);
  assert.ok(s.daemon.calls("ping").every((r) => r.token === T));
});

test("a malformed fragment is ignored and stripped, and the stored token kept", async () => {
  for (const h of ["#code=short", "#code=" + CODE + "&evil=1", "#x&code=" + CODE, "#token=" + T, "#token=" + OLD + "&to=requests"]) {
    const s = setup({ hash: h, stored: T });
    s.app.start();
    assert.deepEqual(s.win.replaced, ["/#/"], h);
    assert.ok(!s.win.location.hash.includes("token") && !s.win.location.hash.includes("code"));
    await settle();
    assert.equal(s.storage.m.get(STORAGE_KEY), T);
    assert.equal(s.app.state, "ok");
    assert.ok(s.daemon.withToken().every((r) => r.token === T));
    assert.deepEqual(s.daemon.calls("redeem"), [], h);
  }
});

test("a forged or used code cannot replace a good stored token", async () => {
  // The daemon knows T; the fragment carries a code it never made, from
  // a URL something else had the browser open, or one already used.
  for (const c of [token("f"), CODE]) {
    const s = setup({ hash: "#code=" + c, stored: T });
    if (c === CODE) s.daemon.codes.clear();
    s.app.start();
    await settle();
    assert.equal(s.daemon.calls("redeem").length, 1);
    assert.equal(s.storage.m.get(STORAGE_KEY), T);
    assert.equal(s.app.state, "ok");
    assertProvenFirst(s.daemon);
  }
});

test("a code that opens nothing, with no stored token: the page says to run caboose ui again", async () => {
  const s = setup({ hash: "#code=" + CODE + "&to=requests" });
  s.daemon.codes.clear();
  s.app.start();
  await s.clock.advance(60_000);
  assert.equal(s.app.state, "no_token");
  assert.deepEqual(s.daemon.requests.map((r) => r.method), ["redeem"]);
  assert.match(s.main(), /This address's code is used or out of date: run caboose ui again\./);
  assert.ok(!s.win.location.hash.includes("code"));
  // A new code from caboose ui opens it.
  s.daemon.codes.add(token("d"));
  s.win.navigate("#code=" + token("d"));
  await settle();
  assert.equal(s.app.state, "ok");
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
});

test("a code is redeemed once, however many rounds wait on it", async () => {
  const s = setup({ hash: "#code=" + CODE });
  s.app.start();
  // A page call made before the first round settles waits on the same redeem.
  const p = s.app.call("ping", {});
  await settle();
  await p;
  await s.clock.advance(3 * POLL_MS);
  assert.equal(s.daemon.calls("redeem").length, 1);
  assert.equal(s.app.state, "ok");
  // The same address opened again is refused by the daemon: the page
  // keeps the token it has.
  s.win.navigate("#code=" + CODE);
  await settle();
  assert.equal(s.daemon.calls("redeem").length, 2);
  assert.equal(s.app.state, "ok");
  assert.equal(s.app.token, T);
});

test("a redeem refused as too many or with no answer keeps the code for the next round", async () => {
  const s = setup({ hash: "#code=" + CODE });
  s.daemon.redeemBusy = true;
  s.app.start();
  await settle();
  assert.equal(s.daemon.withToken().length, 0);
  assert.match(s.region(IDS.alert), /too many requests/);
  s.daemon.redeemBusy = false;
  s.daemon.mode = "down";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "down");
  s.daemon.mode = "ok";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "ok");
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  assert.equal(s.daemon.calls("redeem").length, 3);
});

test("a redeem answered with something that is not caboose's gives no token", async () => {
  for (const body of [{ result: { token: "short" } }, { result: { token: T, x: 1 } }, { result: "x" }]) {
    const s = setup({ hash: "#code=" + CODE, stored: OLD, daemonToken: OLD });
    // Whatever answers the redeem, the daemon answers the rest.
    const serve = s.daemon.serve.bind(s.daemon);
    s.daemon.fetch = (url, init) => (String(url).endsWith("/redeem") ? Promise.resolve(json(200, body)) : serve(url, init));
    s.app.start();
    await settle();
    assert.equal(s.app.state, "squatter", JSON.stringify(body));
    assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
    assert.equal(s.daemon.withToken().length, 0);
  }
});

test("no token goes out until a proof verifies: a wrong MAC", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "wrongmac";
  s.app.start();
  await s.clock.advance(60_000);
  assert.ok(s.daemon.calls("proof").length >= 2, "the proof is retried");
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.app.state, "squatter");
  assert.match(s.main(), /Something else answers on 127\.0\.0\.1:7171/);
  // Once the daemon is back, the proof verifies and the polls resume.
  s.daemon.mode = "ok";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "ok");
  assertProvenFirst(s.daemon);
});

test("no token goes out until a proof verifies: a page that is not the API", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "squatter";
  s.app.start();
  await s.clock.advance(60_000);
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.app.state, "squatter");
  assert.match(s.main(), /Something else answers/);
});

test("proofs refused as too many: the page says so, backs off, sends no token, and resumes", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "busy";
  s.app.start();
  await settle();
  assert.equal(s.app.state, "busy");
  assert.match(s.main(), /refusing proofs for now/);
  assert.doesNotMatch(s.main(), /Something else answers/);
  // The waits between proofs double from BUSY_MIN_MS, up to BUSY_MAX_MS.
  const at = () => s.daemon.calls("proof").length;
  for (const wait of [BUSY_MIN_MS, 2 * BUSY_MIN_MS, 4 * BUSY_MIN_MS]) {
    const n = at();
    await s.clock.advance(wait - 1);
    assert.equal(at(), n, "no proof before " + wait + " ms");
    await s.clock.advance(1);
    assert.equal(at(), n + 1, "a proof after " + wait + " ms");
  }
  await s.clock.advance(10 * BUSY_MAX_MS);
  const n = at();
  await s.clock.advance(BUSY_MAX_MS);
  assert.equal(at(), n + 1, "the wait stops growing at BUSY_MAX_MS");
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  // Once proofs are answered again, the polls resume at their pace.
  s.daemon.mode = "ok";
  await s.clock.advance(BUSY_MAX_MS);
  assert.equal(s.app.state, "ok");
  assert.equal(s.region(IDS.alert), "");
  assertProvenFirst(s.daemon);
  const m = s.daemon.calls("status").length;
  await s.clock.advance(POLL_MS);
  assert.equal(s.daemon.calls("status").length, m + 1);
});

test("the back-off after proofs refused as too many starts over once a poll succeeds", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "busy";
  s.app.start();
  await settle();
  // Refused at 0, 1, 3 and 7 s: the next wait would be 8 s.
  await s.clock.advance(7 * BUSY_MIN_MS);
  s.daemon.mode = "ok";
  await s.clock.advance(8 * BUSY_MIN_MS);
  assert.equal(s.app.state, "ok");
  // A failed poll asks for a proof again, which is refused as too many:
  // the wait after it is BUSY_MIN_MS again, not 16 s.
  s.daemon.answers.status = { status: 500, body: { error: { kind: "internal", message: "x" } } };
  await s.clock.advance(POLL_MS);
  delete s.daemon.answers.status;
  s.daemon.mode = "busy";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "busy");
  const n = s.daemon.calls("proof").length;
  await s.clock.advance(BUSY_MIN_MS);
  assert.equal(s.daemon.calls("proof").length, n + 1, "a proof BUSY_MIN_MS after the first refusal");
});

test("something answering 429 to every proof: after some rounds the page says something else may answer", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "busy";
  s.app.start();
  await settle();
  const said = () => s.main() + " " + s.region(IDS.alert);
  let wait = BUSY_MIN_MS;
  for (let round = 1; round < BUSY_SUSPECT_ROUNDS; round++) {
    assert.match(said(), /refusing proofs for now/, "round " + round);
    assert.doesNotMatch(said(), /something else answers/i, "round " + round);
    await s.clock.advance(wait);
    wait *= 2;
  }
  assert.equal(s.app.state, "busy");
  assert.match(s.main(), /or something else answers on 127\.0\.0\.1:7171 and refuses every proof/);
  assert.match(s.region(IDS.alert), /or something else answers on 127\.0\.0\.1:7171/);
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  // It stays said while the refusals go on.
  await s.clock.advance(3 * BUSY_MAX_MS);
  assert.match(s.main(), /or something else answers/);
  // Once a proof verifies the note goes, and the count starts over.
  s.daemon.mode = "ok";
  await s.clock.advance(BUSY_MAX_MS);
  assert.equal(s.app.state, "ok");
  assert.equal(s.region(IDS.alert), "");
  s.daemon.answers.status = { status: 500, body: { error: { kind: "internal", message: "x" } } };
  await s.clock.advance(POLL_MS);
  delete s.daemon.answers.status;
  s.daemon.mode = "busy";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "busy");
  assert.match(s.region(IDS.alert), /refusing proofs for now/);
  assert.doesNotMatch(s.region(IDS.alert), /something else answers/);
});

// busyFor refuses proofs as too many for rounds rounds in a row, from
// the start, and ends past the wait after the last one.
async function busyFor(s, rounds) {
  s.daemon.mode = "busy";
  s.app.start();
  await settle();
  let wait = BUSY_MIN_MS;
  for (let i = 1; i < rounds; i++) {
    await s.clock.advance(wait);
    wait *= 2;
  }
  assert.equal(s.daemon.calls("proof").length, rounds);
  return wait;
}

test("a poll that succeeds starts the count of busy rounds over", async () => {
  const s = setup({ stored: T });
  const wait = await busyFor(s, BUSY_SUSPECT_ROUNDS - 1);
  s.daemon.mode = "ok";
  await s.clock.advance(wait);
  assert.equal(s.app.state, "ok");
  // A tab shown again proves again, with no failure in between: refused,
  // it is the first round of a new count.
  s.daemon.mode = "busy";
  s.doc.setVisible(false);
  s.doc.setVisible(true);
  await settle();
  assert.equal(s.app.state, "busy");
  assert.match(s.region(IDS.alert), /refusing proofs for now/);
  assert.doesNotMatch(s.region(IDS.alert), /something else answers/);
});

test("a failure other than busy starts the count of busy rounds over", async () => {
  const s = setup({ stored: T });
  const wait = await busyFor(s, BUSY_SUSPECT_ROUNDS - 1);
  s.daemon.mode = "down";
  await s.clock.advance(wait);
  assert.equal(s.app.state, "down");
  s.daemon.mode = "busy";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "busy");
  assert.match(s.main(), /refusing proofs for now/);
  assert.doesNotMatch(s.main() + s.region(IDS.alert), /something else answers/);
});

test("the fake clock fails a hot loop at once instead of hanging", async () => {
  const still = new Clock({ maxFires: 100, maxAtOnce: 10 });
  const again = () => still.setTimeout(again, 0);
  still.setTimeout(again, 0);
  await assert.rejects(still.advance(1), /no time passing: a hot loop/);
  const tick = new Clock({ maxFires: 100, maxAtOnce: 10 });
  const next = () => tick.setTimeout(next, 1);
  tick.setTimeout(next, 1);
  await assert.rejects(tick.advance(1_000_000), /over 100 timers in one advance/);
  // The defaults leave room for the page's own timers.
  const page = new Clock();
  let fired = 0;
  const poll = () => {
    fired++;
    page.setTimeout(poll, POLL_MS);
  };
  page.setTimeout(poll, POLL_MS);
  await page.advance(1000 * POLL_MS);
  assert.equal(fired, 1000);
});

test("proofs refused as too many keep a redeemed token, which is used once one verifies", async () => {
  const s = setup({ hash: "#code=" + CODE, stored: OLD });
  s.daemon.mode = "busy";
  s.app.start();
  await s.clock.advance(5 * BUSY_MIN_MS);
  assert.equal(s.app.state, "busy");
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
  s.daemon.mode = "ok";
  await s.clock.advance(BUSY_MAX_MS);
  assert.equal(s.app.state, "ok");
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  assertProvenFirst(s.daemon);
});

test("a page already shown stays while proofs are refused as too many, with an alert", async () => {
  const s = setup({ stored: T });
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  // A failed poll asks for a proof again, which is refused as too many.
  s.daemon.answers.status = { status: 500, body: { error: { kind: "internal", message: "x" } } };
  await s.clock.advance(POLL_MS);
  delete s.daemon.answers.status;
  s.daemon.mode = "busy";
  await s.clock.advance(SLOW_MS);
  assert.equal(s.app.state, "busy");
  assert.match(s.region(IDS.alert), /refusing proofs for now/);
  assert.match(s.main(), /Overview/);
});

test("a server that proved nothing cannot drop the stored token: a 401 to the proof", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "squatter401";
  s.app.start();
  await s.clock.advance(60_000);
  assert.equal(s.daemon.withToken().length, 0);
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  assert.equal(s.app.state, "squatter");
  assert.match(s.main(), /Something else answers/);
});

test("no token goes out until a proof verifies: no answer", async () => {
  const s = setup({ stored: T });
  s.daemon.mode = "down";
  s.app.start();
  await settle();
  assert.equal(s.app.state, "down");
  assert.match(s.region(IDS.alert), /daemon is not answering; run caboose ui/);
  // Polls slow down: the next try comes after SLOW_MS, not POLL_MS.
  const n = s.daemon.requests.length;
  await s.clock.advance(SLOW_MS - 1);
  assert.equal(s.daemon.requests.length, n);
  await s.clock.advance(1);
  assert.equal(s.daemon.requests.length, n + 1);
  assert.equal(s.daemon.withToken().length, 0);
});

test("a proof is redone after any failed request", async () => {
  for (const failure of ["down", "squatter", "500"]) {
    const s = setup({ stored: T });
    s.app.start();
    await s.clock.advance(3 * POLL_MS);
    assert.equal(s.daemon.calls("proof").length, s.daemon.calls("status").length);
    if (failure === "500") {
      s.daemon.answers.status = { status: 500, body: { error: { kind: "internal", message: "x" } } };
    } else {
      s.daemon.mode = failure;
    }
    await s.clock.advance(POLL_MS);
    const failedAt = s.daemon.requests.length;
    s.daemon.mode = "ok";
    delete s.daemon.answers.status;
    await s.clock.advance(SLOW_MS);
    const after = s.daemon.requests.slice(failedAt);
    assert.equal(after[0]?.method, "proof", failure + ": " + after.map((r) => r.method).join(","));
    assert.equal(after[0].token, undefined);
    assert.equal(s.app.state, "ok", failure);
  }
});

test("a stand-in that takes the daemon's place is never sent the token", async () => {
  // What answers once the daemon stops: something that does not know the
  // token and answers proofs with another key, answers nothing, answers
  // a page of its own, or refuses everything.
  for (const kind of ["wrongmac", "down", "squatter", "squatter401", "busy"]) {
    const page = {
      render: () => ({ title: "Requests", body: ["x"] }),
      calls: (ctx) => (ctx.route?.page === "requests" ? [{ key: "reqs", method: "requests", params: { env: "default" }, every: POLL_MS }] : []),
    };
    const s = setup({ stored: T, hash: "#/requests", page });
    s.daemon.answers.requests = { body: { result: { n: 1 } } };
    s.app.start();
    await s.clock.advance(3 * POLL_MS);
    assert.equal(s.app.state, "ok", kind);
    assert.ok(s.daemon.withToken().length >= 4, kind);
    const standin = new FakeDaemon(token("z"), ORIGIN, { log: s.log, now: s.clock.now });
    standin.mode = kind;
    s.net.server = standin;
    // Polls go on across what a reused proof would have covered, and
    // well past it.
    for (let i = 0; i < 20; i++) await s.clock.advance(POLL_MS / 2);
    await s.clock.advance(60_000);
    assert.deepEqual(standin.withToken(), [], kind + ": the stand-in was sent the token");
    assert.ok(standin.calls("proof").length >= 2, kind + ": the page stopped asking");
    assert.ok(standin.requests.every((r) => r.method === "proof"), kind + ": " + standin.requests.map((r) => r.method).join(","));
    assert.notEqual(s.app.state, "ok", kind);
    // The daemon back: every call carries a proof of its own again.
    s.net.server = s.daemon;
    await s.clock.advance(SLOW_MS + BUSY_MAX_MS);
    assert.equal(s.app.state, "ok", kind);
    assert.ok(assertProvenFirst(s.daemon, s.log) >= 6, kind);
  }
});

test("calls under way at once each wait on a proof of their own", async () => {
  const page = {
    render: () => ({ title: "Requests", body: ["x"] }),
    calls: (ctx) => (ctx.route?.page === "requests" ? ["a", "b", "c"].map((k) => ({ key: k, method: "requests", params: { env: "default" }, every: POLL_MS })) : []),
  };
  const s = setup({ stored: T, hash: "#/requests", page });
  s.daemon.answers.requests = { body: { result: { n: 1 } } };
  s.app.start();
  await s.clock.advance(POLL_MS);
  assert.equal(s.app.state, "ok");
  // Proofs answered only once every call has asked for its own, and
  // page calls besides.
  let release;
  s.daemon.proofHold = new Promise((r) => {
    release = r;
  });
  const n = s.daemon.requests.length;
  s.app.call("ping", {}).catch(() => {});
  s.app.call("ping", {}).catch(() => {});
  await s.clock.advance(POLL_MS);
  const asked = s.daemon.requests.slice(n);
  assert.ok(asked.filter((r) => r.method === "proof").length >= 3, asked.map((r) => r.method).join(","));
  assert.deepEqual(asked.filter((r) => r.token !== undefined), [], "the token went out before its proof was answered");
  s.daemon.proofHold = null;
  release();
  await settle();
  const after = s.daemon.requests.slice(n);
  assert.ok(after.filter((r) => r.token !== undefined).length >= 3, after.map((r) => r.method).join(","));
  assertProvenFirst(s.daemon);
});

test("a token made new between two polls is not sent again until a proof verifies it", async () => {
  const s = setup({ stored: OLD, daemonToken: OLD });
  s.app.start();
  await s.clock.advance(POLL_MS);
  assert.equal(s.app.state, "ok");
  const n = s.daemon.requests.length;
  s.daemon.token = T;
  await s.clock.advance(60_000);
  assert.deepEqual(s.daemon.requests.slice(n).filter((r) => r.token !== undefined), []);
  assert.equal(s.app.state, "squatter");
  assert.match(s.main(), /the page's token is out of date/);
});

test("a proof is redone when the tab is shown again, and nothing is polled while hidden", async () => {
  const s = setup({ stored: T });
  s.app.start();
  await s.clock.advance(POLL_MS);
  s.doc.setVisible(false);
  const n = s.daemon.requests.length;
  await s.clock.advance(120_000);
  assert.equal(s.daemon.requests.length, n, "polled while hidden");
  s.doc.setVisible(true);
  await settle();
  assert.deepEqual(s.daemon.requests.slice(n).map((r) => r.method), ["proof", "status"]);
  // Even when hidden only briefly.
  s.doc.setVisible(false);
  await s.clock.advance(100);
  const m = s.daemon.requests.length;
  s.doc.setVisible(true);
  await settle();
  assert.deepEqual(s.daemon.requests.slice(m).map((r) => r.method), ["proof", "status"]);
});

test("one status request in flight at a time", async () => {
  const s = setup({ stored: T });
  let release;
  s.daemon.hold = new Promise((r) => {
    release = r;
  });
  s.app.start();
  await s.clock.advance(20_000);
  s.doc.setVisible(false);
  s.doc.setVisible(true);
  await s.clock.advance(20_000);
  assert.equal(s.daemon.calls("status").length, 1);
  s.daemon.hold = null;
  release();
  await s.clock.advance(POLL_MS);
  assert.equal(s.daemon.calls("status").length, 2);
});

test("a token replaced between a proof and its call: the page says what a failed proof says, and keeps the token", async () => {
  const s = setup({ stored: OLD, daemonToken: OLD });
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  // The next proof verifies with the old token; the token is made new
  // before the call that follows it.
  const server = s.daemon;
  let armed = true;
  s.net.server = {
    fetch: async (url, init) => {
      const res = await server.fetch(url, init);
      if (armed && String(url).endsWith("/proof")) {
        armed = false;
        server.token = T;
      }
      return res;
    },
  };
  const n = server.requests.length;
  await s.clock.advance(POLL_MS);
  assert.deepEqual(server.requests.slice(n, n + 3).map((r) => [r.method, r.token === OLD]), [
    ["proof", false],
    ["status", true],
    ["proof", false],
  ]);
  assert.equal(s.app.state, "squatter");
  assert.match(s.main(), /the page's token is out of date/);
  assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
  assertProvenFirst(server);
  // And it stays so, sending nothing with the old token.
  const m = server.requests.length;
  await s.clock.advance(60_000);
  assert.equal(s.app.state, "squatter");
  assert.deepEqual(server.requests.slice(m).filter((r) => r.token !== undefined), []);
});

test("a call refused with a 401 after its proof: status is asked for at once, and its proof decides", async () => {
  const page = {
    render: () => ({ title: "Requests", body: ["x"] }),
    calls: (ctx) => (ctx.route?.page === "requests" ? [{ key: "r", method: "requests", params: { env: "default" }, every: POLL_MS }] : []),
  };
  const s = setup({ stored: OLD, daemonToken: OLD, hash: "#/requests", page });
  s.daemon.answers.requests = { body: { result: { n: 1 } } };
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  // The token is made new while an answer is between its proof and
  // itself: the daemon refuses it as it would.
  s.daemon.handlers.answer = () => {
    s.daemon.token = T;
    return { status: 401, body: { error: { kind: "unauthorized", message: "run caboose ui" } } };
  };
  const n = s.daemon.requests.length;
  await assert.rejects(s.app.call("answer", { id: 1 }), (e) => e instanceof ApiError && e.status === 401);
  await settle();
  assert.deepEqual(s.daemon.requests.slice(n, n + 3).map((r) => r.method), ["proof", "answer", "proof"]);
  assert.equal(s.app.state, "squatter");
  assert.equal(s.storage.m.get(STORAGE_KEY), OLD);
  assert.deepEqual(s.daemon.requests.slice(n + 2).filter((r) => r.token !== undefined), []);
});

test("a server that proves the token and refuses it round after round has it dropped", async () => {
  const s = setup({ stored: OLD, daemonToken: OLD });
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  s.daemon.answers.status = { status: 401, body: { error: { kind: "unauthorized", message: "run caboose ui" } } };
  await s.clock.advance(POLL_MS);
  // The one that succeeded, then the two refused.
  assert.equal(REFUSED_ROUNDS, 2);
  assert.equal(s.daemon.calls("status").filter((r) => r.token === OLD).length, 3);
  assert.equal(s.storage.m.get(STORAGE_KEY), undefined);
  assert.equal(s.app.state, "no_token");
  assert.equal(s.main(), "cabooseOpen this page with caboose ui in a terminal.");
  const n = s.daemon.requests.length;
  await s.clock.advance(60_000);
  assert.equal(s.daemon.requests.length, n, "polled with no token");
});

test("a 401 whose body names the kind proof counts as a refused round: no hot loop", async () => {
  const s = setup({ stored: OLD, daemonToken: OLD });
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  s.daemon.answers.status = { status: 401, body: { error: { kind: "proof", message: "x" } } };
  await s.clock.advance(60_000);
  // The one that succeeded, then the two refused.
  assert.equal(s.daemon.calls("status").filter((r) => r.token === OLD).length, 3);
  assert.equal(s.app.state, "no_token");
});

test("a stored token that keeps changing is taken at once only once a round", async () => {
  // Every read of localStorage finds another token, none the daemon's:
  // each round's proof fails, and the page may try the stored one at
  // once, but not the one after it.
  let n = 0;
  const flipping = new FakeStorage({});
  flipping.getItem = () => token(String.fromCharCode(99 + (n++ % 20)));
  const s = setup({ daemonToken: T });
  s.storage.getItem = flipping.getItem;
  s.app.start();
  await s.clock.advance(60_000);
  const proofs = s.daemon.calls("proof").length;
  assert.ok(proofs <= 2 * (60_000 / SLOW_MS + 1), proofs + " proofs in 60 s");
  assert.ok(proofs >= 60_000 / SLOW_MS, proofs + " proofs in 60 s");
  assert.equal(s.app.state, "squatter");
  assert.deepEqual(s.daemon.withToken(), []);
});

// twoTabs is two pages of one origin open at once: one daemon, one
// localStorage, one clock. Tab A's requests go through a, so a test can
// change the daemon in the middle of one of A's rounds.
function twoTabs(o) {
  live?.stop();
  const clock = new Clock();
  const daemon = new FakeDaemon(o.daemonToken, ORIGIN, { now: clock.now });
  const storage = new FakeStorage({ [STORAGE_KEY]: o.stored });
  const tab = (fetch) =>
    createApp({
      win: new FakeWindow("#/requests", ORIGIN),
      doc: new FakeDocument().shell(),
      fetch,
      crypto: trackedCrypto,
      storage: () => storage,
      now: clock.now,
      setTimeout: clock.setTimeout,
      clearTimeout: clock.clearTimeout,
      page: stubPage,
    });
  const hooks = { afterProof: null };
  const a = tab(async (url, init) => {
    const res = await daemon.fetch(url, init);
    if (String(url).endsWith("/proof")) hooks.afterProof?.();
    return res;
  });
  const b = tab((url, init) => daemon.fetch(url, init));
  return { clock, daemon, storage, a, b, hooks, stop: () => (a.stop(), b.stop()) };
}

test("a token made new: two open tabs say the same, however each met it, and both take a new page's token", async () => {
  const w = twoTabs({ stored: OLD, daemonToken: OLD });
  try {
    w.a.start();
    w.b.start();
    await settle();
    assert.deepEqual([w.a.state, w.b.state], ["ok", "ok"]);
    // caboose ui --new-token lands between a proof of tab A's and its
    // call: A is refused with a 401, B's next proof fails.
    let raced = false;
    w.hooks.afterProof = () => {
      w.hooks.afterProof = null;
      w.daemon.token = T;
      raced = true;
    };
    const n = w.daemon.requests.length;
    await w.clock.advance(POLL_MS);
    assert.ok(raced);
    assert.ok(w.daemon.requests.slice(n).some((r) => r.method === "status" && r.token === OLD), "no call met the 401");
    await w.clock.advance(3 * SLOW_MS);
    assert.deepEqual([w.a.state, w.b.state], ["squatter", "squatter"]);
    assert.equal(w.storage.m.get(STORAGE_KEY), OLD);
    // A page opened with caboose ui stores the new token: both tabs take
    // it at their next round, each after a proof of its own.
    w.storage.setItem(STORAGE_KEY, T);
    await w.clock.advance(SLOW_MS);
    assert.deepEqual([w.a.state, w.b.state], ["ok", "ok"]);
    assertProvenFirst(w.daemon);
  } finally {
    w.stop();
  }
});

test("no token: nothing is sent, and the page says to run caboose ui", async () => {
  const s = setup();
  s.app.start();
  await s.clock.advance(60_000);
  assert.equal(s.daemon.requests.length, 0);
  assert.equal(s.app.state, "no_token");
  assert.match(s.main(), /Open this page with caboose ui in a terminal\./);
  // A fragment arriving later starts it.
  s.win.navigate("#code=" + CODE);
  assert.ok(!s.win.location.hash.includes("code"));
  await settle();
  assert.equal(s.app.state, "ok");
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
});

test("no Web Crypto: the token is never sent", async () => {
  for (const c of [null, {}, { getRandomValues: webcrypto.getRandomValues.bind(webcrypto) }]) {
    const s = setup({ hash: "#code=" + CODE, stored: T, crypto: c });
    s.app.start();
    assert.ok(!s.win.location.hash.includes("code"));
    await s.clock.advance(60_000);
    assert.equal(s.daemon.requests.length, 0);
    assert.equal(s.app.state, "no_crypto");
    assert.match(s.main(), /use a current Safari, Chrome or Firefox/);
  }
});

test("storage that fails keeps the token for the tab, and says a reload needs caboose ui", async () => {
  const s = setup({ hash: "#code=" + CODE, storageBroken: true });
  s.app.start();
  await s.clock.advance(3 * POLL_MS);
  assert.equal(s.app.state, "ok");
  assert.equal(s.app.token, T);
  assert.ok(s.daemon.calls("status").length >= 3);
  assert.match(s.region(IDS.note), /a reload needs caboose ui again/);
  // And with no storage at all.
  const t = tokenStore(() => {
    throw new Error("SecurityError");
  });
  assert.equal(t.broken, true);
  assert.equal(t.get(), null);
  assert.equal(t.set(T), false);
});

test("a stored token of the wrong form is not used", async () => {
  const s = setup({ stored: "<img src=x onerror=alert(1)>" });
  s.app.start();
  await settle();
  assert.equal(s.daemon.requests.length, 0);
  assert.equal(s.app.state, "no_token");
});

test("new requests are announced in the polite live region", async () => {
  const seen = [];
  const s = setup({ stored: T, page: { render: () => ({ title: "Overview", body: [] }), onPending: (env, n) => seen.push([env, n]) } });
  s.daemon.status.envs = [{ name: "default", sandbox: "running", link: "linked" }, { name: "dev", sandbox: "running", link: "linked", pending: 1 }];
  s.app.start();
  await settle();
  assert.equal(s.region(IDS.status), "");
  s.daemon.status = structuredClone(s.daemon.status);
  s.daemon.status.envs[0].pending = 2;
  await s.clock.advance(POLL_MS);
  assert.equal(s.region(IDS.status), "2 new requests");
  assert.deepEqual(seen, [["default", 2]]);
  s.daemon.status.envs[1].pending = 2;
  await s.clock.advance(POLL_MS);
  assert.equal(s.region(IDS.status), "1 new request");
  const nav = texts(s.doc.getElementById(IDS.nav)).join("|");
  assert.match(nav, /Requests \(4\)/);
});

test("a route change renders the page and moves focus to its h1", async () => {
  const s = setup({ stored: T });
  s.app.start();
  await settle();
  s.win.navigate("#/env/dev");
  const h1 = s.doc.activeElement;
  assert.equal(h1.tagName, "H1");
  assert.equal(h1.getAttribute("id"), IDS.title);
  assert.equal(h1.getAttribute("tabindex"), "-1");
  assert.deepEqual(texts(h1), ["Environment dev"]);
  const current = elements(s.doc.getElementById(IDS.nav)).filter((e) => e.getAttribute("aria-current") === "page");
  assert.equal(current.length, 0);
  s.win.navigate("#/requests/dev");
  assert.deepEqual(texts(s.doc.activeElement), ["Requests of dev"]);
  assert.deepEqual(texts(elements(s.doc.getElementById(IDS.nav)).find((e) => e.getAttribute("aria-current") === "page")), ["Requests (0)"]);
  s.win.navigate("#/nope");
  assert.deepEqual(texts(s.doc.activeElement), ["No such page"]);
  // One h1 on the page.
  assert.equal(elements(s.doc.body).filter((e) => e.tagName === "H1").length, 1);
});

test("a code fragment arriving while the page runs is stripped, redeemed and proven", async () => {
  const s = setup({ stored: OLD, daemonToken: T });
  s.app.start();
  await settle();
  assert.equal(s.app.state, "squatter"); // OLD proves nothing to a daemon of T
  s.win.navigate("#code=" + CODE + "&to=proposals");
  assert.equal(s.win.location.hash, "#/proposals");
  await settle();
  assert.equal(s.app.state, "ok");
  assert.equal(s.storage.m.get(STORAGE_KEY), T);
  assert.ok(s.daemon.requests.every((r) => r.token !== OLD));
});

test("every API request is a same-origin POST; only the proof and the redeem go without the token", async () => {
  const s = setup({ stored: T });
  s.app.start();
  await s.clock.advance(POLL_MS);
  for (const r of s.daemon.requests) {
    assert.equal(r.init.method, "POST");
    assert.equal(r.init.credentials, "omit");
    assert.equal(r.init.redirect, "error");
    assert.equal(r.init.mode, "same-origin");
    assert.equal(r.init.headers["Content-Type"], "application/json");
    if (r.method === "proof") {
      assert.equal(r.token, undefined);
      assert.match(r.body.nonce, /^[A-Za-z0-9_-]{43}$/);
      assert.deepEqual(Object.keys(r.body), ["nonce"]);
    } else {
      assert.equal(r.token, T);
    }
  }
  const nonces = s.daemon.calls("proof").map((r) => r.body.nonce);
  s.doc.setVisible(false);
  s.doc.setVisible(true);
  await settle();
  const again = s.daemon.calls("proof").map((r) => r.body.nonce);
  assert.equal(new Set(again).size, again.length, "a nonce was reused");
  assert.ok(again.length > nonces.length);
});

test("the proof's MAC is the daemon's, bound to the origin and the nonce", async () => {
  const n = "n".repeat(43);
  assert.equal(await proofMAC(webcrypto.subtle, T, ORIGIN, n), mac(T, ORIGIN, n));
  assert.notEqual(await proofMAC(webcrypto.subtle, T, "http://127.0.0.1:7172", n), mac(T, ORIGIN, n));
  // A server that answers for another origin does not prove itself.
  const d = new FakeDaemon(T, "http://127.0.0.1:9999");
  const api = createApi({ fetch: d.fetch, origin: ORIGIN, crypto: webcrypto });
  await assert.rejects(api.prove(T), (e) => e instanceof ApiError && e.kind === "proof");
  const ok = createApi({ fetch: new FakeDaemon(T, ORIGIN).fetch, origin: ORIGIN, crypto: webcrypto });
  await ok.prove(T);
  // A 429 to the proof is "busy", not a proof that failed.
  const busy = new FakeDaemon(T, ORIGIN);
  busy.mode = "busy";
  const b = createApi({ fetch: busy.fetch, origin: ORIGIN, crypto: webcrypto });
  await assert.rejects(b.prove(T), (e) => e instanceof ApiError && e.kind === "busy" && e.status === 429);
  assert.ok(busy.requests.every((r) => r.token === undefined));
});

test("answers that are not the API's shape are refused", async () => {
  const cases = [
    [json(200, { result: {}, extra: 1 }), "bad_answer"],
    [json(200, { error: { kind: "x", message: "y" } }), "bad_answer"],
    [json(200, [1]), "bad_answer"],
    [json(409, { error: { kind: "not_owner", message: "another daemon serves it" } }), "not_owner"],
    [json(500, { error: { kind: "internal", message: "x" } }), "internal"],
    [json(404, { result: {} }), "bad_answer"],
    [json(401, { error: { kind: 1, message: "x" } }), "bad_answer"],
    [{ status: 200, headers: { get: () => "text/html" }, text: async () => "{\"result\":{}}" }, "bad_answer"],
    [{ status: 200, headers: { get: () => "application/json" }, text: async () => "not json" }, "bad_answer"],
    [{ status: 200, headers: { get: (k) => (k === "content-length" ? String(64 << 20) : "application/json") }, text: async () => "{}" }, "bad_answer"],
  ];
  for (const [res, kind] of cases) {
    const api = createApi({ fetch: async () => res, origin: ORIGIN, crypto: webcrypto });
    await assert.rejects(api.call("status", {}, T), (e) => e instanceof ApiError && e.kind === kind, kind);
  }
  const api = createApi({ fetch: async () => json(200, { result: { ok: true } }), origin: ORIGIN, crypto: webcrypto });
  assert.deepEqual(await api.call("status", {}, T), { ok: true });
  await assert.rejects(api.call("status", {}, "short"));
  await assert.rejects(api.call("../x", {}, T));
});

test("hostile text from the daemon renders as text nodes only", async () => {
  const evil = "<img src=x onerror=alert(1)>";
  const s = setup({ stored: T });
  s.daemon.status.daemon.version = evil;
  s.daemon.status.envs = [
    { name: evil, sandbox: "running", link: "linked", pending: 1 },
    { name: "javascript:alert(1)", sandbox: "running", link: "linked" },
    { name: "‮evil", sandbox: "running", link: "linked" },
    { name: "x".repeat(10 * 1024), sandbox: "running", link: "linked" },
    { name: "dev", sandbox: "running", link: "linked" },
  ];
  s.app.start();
  await settle();
  assert.equal(s.app.state, "ok");
  s.daemon.answers.status = { status: 409, body: { error: { kind: "not_owner", message: evil } } };
  await s.clock.advance(POLL_MS);
  assert.equal(s.region(IDS.alert), evil);
  const all = elements(s.doc.body);
  assert.ok(all.every((e) => !["IMG", "SCRIPT", "IFRAME", "SVG"].includes(e.tagName)));
  for (const e of all) {
    for (const [k, v] of e.attributes) {
      assert.ok(!k.startsWith("on") && k !== "src" && k !== "style", k);
      if (k === "href") assert.match(v, /^#\/[a-z0-9/_-]*$/);
      if (k === "id") assert.ok(Object.values(IDS).includes(v), v);
    }
  }
  const t = texts(s.doc.body);
  assert.ok(t.includes(evil) && t.includes("javascript:alert(1)") && t.includes("‮evil") && t.includes("x".repeat(10 * 1024)));
  const links = all.filter((e) => e.tagName === "A").map((e) => e.getAttribute("href"));
  assert.deepEqual(links.sort(), ["#/", "#/env/dev", "#/proposals", "#/requests"]);
  // Sandbox text is isolated.
  const bdis = all.filter((e) => e.tagName === "BDI");
  assert.ok(bdis.some((b) => texts(b)[0] === evil));
});

test("a page's own calls are polled, one at a time, on their clock", async () => {
  const page = {
    render: (ctx) => ({ title: "Requests", body: [String(ctx.data.get("reqs")?.n ?? "none")] }),
    calls: (ctx) => (ctx.route?.page === "requests" ? [{ key: "reqs", method: "requests", params: { env: "default" }, every: 2000 }] : []),
  };
  const s = setup({ stored: T, hash: "#/requests", page });
  s.daemon.answers.requests = { body: { result: { n: 7 } } };
  s.app.start();
  await s.clock.advance(10_000);
  assert.match(s.main(), /7/);
  const n = s.daemon.calls("requests").length;
  assert.ok(n >= 4 && n <= 7, String(n));
  assert.ok(s.daemon.calls("requests").every((r) => r.token === T && r.body.env === "default"));
  s.win.navigate("#/");
  const m = s.daemon.calls("requests").length;
  await s.clock.advance(10_000);
  assert.equal(s.daemon.calls("requests").length, m);
  assertProvenFirst(s.daemon);
});
