// Fakes for the page's tests, which run under node --test with no
// browser: a document that records how each node was made and refuses
// every HTML sink, a window, timers on a clock the test moves, storage,
// and a daemon that answers /api/ as the real one does. No test reaches
// the network: the real fetch is replaced by one that fails the test.

import { createHmac, webcrypto } from "node:crypto";

globalThis.fetch = () => {
  throw new Error("a test reached for the network");
};

const sink = (name) => ({
  get() {
    throw new Error("HTML sink read: " + name);
  },
  set() {
    throw new Error("HTML sink used: " + name);
  },
});

class FakeNode {
  constructor(doc, nodeType) {
    this.ownerDocument = doc;
    this.nodeType = nodeType;
    this.parentNode = null;
    this.childNodes = [];
  }

  appendChild(c) {
    if (!(c instanceof FakeNode)) {
      throw new TypeError("appendChild of something that is not a node");
    }
    if (c.parentNode) {
      this.dropFocusIn(c);
      c.parentNode.childNodes = c.parentNode.childNodes.filter((x) => x !== c);
    }
    c.parentNode = this;
    this.childNodes.push(c);
    return c;
  }

  // like the DOM, taking a node out of the page takes focus from it
  dropFocusIn(c) {
    const a = this.ownerDocument.activeElement;
    for (let n = a; n; n = n.parentNode) {
      if (n === c) {
        this.ownerDocument.activeElement = null;
        return;
      }
    }
  }

  replaceChildren(...cs) {
    for (const c of this.childNodes) {
      this.dropFocusIn(c);
      c.parentNode = null;
    }
    this.childNodes = [];
    for (const c of cs) {
      if (typeof c === "string") {
        // The DOM makes a text node of a string here: record it as such.
        this.appendChild(this.ownerDocument.createTextNode(c));
      } else {
        this.appendChild(c);
      }
    }
  }

  get textContent() {
    return this.childNodes.map((c) => c.textContent).join("");
  }
}

for (const name of ["innerHTML", "outerHTML"]) {
  Object.defineProperty(FakeNode.prototype, name, sink(name));
}
FakeNode.prototype.insertAdjacentHTML = function () {
  throw new Error("HTML sink used: insertAdjacentHTML");
};
FakeNode.prototype.setHTMLUnsafe = function () {
  throw new Error("HTML sink used: setHTMLUnsafe");
};

class FakeText extends FakeNode {
  constructor(doc, data) {
    super(doc, 3);
    this.data = data;
  }

  get textContent() {
    return this.data;
  }
}

const attrNameRe = /^[a-z][a-z0-9-]*$/;

class FakeElement extends FakeNode {
  constructor(doc, tag) {
    super(doc, 1);
    this.tagName = tag.toUpperCase();
    this.attributes = new Map();
    this.listeners = new Map();
  }

  setAttribute(name, value) {
    if (!attrNameRe.test(name)) throw new Error("bad attribute name " + name);
    this.ownerDocument.log.push({ op: "setAttribute", name, value: String(value) });
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  addEventListener(type, f) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(f);
  }

  // dispatchEvent runs the listeners; a dialog's cancel that nobody
  // prevented closes it, as Escape does.
  dispatchEvent(ev) {
    ev.target = this;
    ev.defaultPrevented = false;
    ev.preventDefault = () => {
      ev.defaultPrevented = true;
    };
    for (const f of this.listeners.get(ev.type) ?? []) f(ev);
    return !ev.defaultPrevented;
  }

  // click is a user's click: a disabled button gets none.
  click() {
    if (!this.disabled) this.dispatchEvent({ type: "click" });
  }

  get disabled() {
    return this.attributes.has("disabled");
  }

  set disabled(v) {
    if (v) this.attributes.set("disabled", "");
    else this.attributes.delete("disabled");
  }

  // focus does nothing for a disabled element, as in a browser.
  focus() {
    if (this.disabled) return;
    this.ownerDocument.activeElement = this;
  }

  // A select's value: what was set, else the selected option, else the first.
  get value() {
    if (this._value !== undefined) return this._value;
    const opts = this.childNodes.filter((c) => c.tagName === "OPTION");
    const sel = opts.find((o) => o.attributes.has("selected")) ?? opts[0];
    return sel ? (sel.getAttribute("value") ?? "") : "";
  }

  set value(v) {
    this._value = String(v);
  }

  // A dialog, as showModal and close leave it: modal, no focus moved
  // into it (the page must place focus itself).
  showModal() {
    if (this.tagName !== "DIALOG") throw new Error("showModal on a " + this.tagName);
    this.open = true;
    this.ownerDocument.modal = this;
    this.ownerDocument.activeElement = this;
  }

  close() {
    if (!this.open) return;
    this.open = false;
    this.ownerDocument.modal = null;
    this.dispatchEvent({ type: "close" });
  }

  // pressEscape is Escape in an open dialog.
  pressEscape() {
    if (this.dispatchEvent({ type: "cancel" })) this.close();
  }

  get id() {
    return this.getAttribute("id") ?? "";
  }
}

// FakeDocument makes nodes and records how, and has no HTML sink that
// does not throw.
export class FakeDocument {
  constructor() {
    this.log = [];
    this.activeElement = null;
    this.visibilityState = "visible";
    this.listeners = new Map();
    this.body = new FakeElement(this, "body");
  }

  createElement(tag) {
    this.log.push({ op: "createElement", tag });
    return new FakeElement(this, tag);
  }

  createTextNode(s) {
    this.log.push({ op: "createTextNode", data: s });
    return new FakeText(this, s);
  }

  getElementById(id) {
    const walk = (n) => {
      if (n instanceof FakeElement && n.getAttribute("id") === id) return n;
      for (const c of n.childNodes) {
        const r = walk(c);
        if (r) return r;
      }
      return null;
    };
    return walk(this.body);
  }

  addEventListener(type, f) {
    this.listeners.set(type, f);
  }

  setVisible(v) {
    this.visibilityState = v ? "visible" : "hidden";
    this.listeners.get("visibilitychange")?.();
  }

  // shell builds index.html's body: the nodes the app fills in.
  shell() {
    for (const [tag, id] of [["p", "daemon-state"], ["nav", "page-nav"], ["p", "page-note"], ["div", "live-status"], ["div", "live-alert"], ["main", "main"], ["div", "page-dialog"]]) {
      const n = new FakeElement(this, tag);
      n.attributes.set("id", id);
      this.body.appendChild(n);
    }
    return this;
  }
}

for (const name of ["write", "writeln"]) {
  FakeDocument.prototype[name] = function () {
    throw new Error("HTML sink used: document." + name);
  };
}
Object.defineProperty(FakeDocument.prototype, "cookie", sink("document.cookie"));

// elements is every element under n, n included.
export function elements(n) {
  const out = [];
  const walk = (x) => {
    if (x instanceof FakeElement) out.push(x);
    for (const c of x.childNodes) walk(c);
  };
  walk(n);
  return out;
}

// texts is every text node's data under n.
export function texts(n) {
  const out = [];
  const walk = (x) => {
    if (x instanceof FakeText) out.push(x.data);
    for (const c of x.childNodes) walk(c);
  };
  walk(n);
  return out;
}

// Clock is a clock and timers that move only when the test says.
export class Clock {
  // maxFires bounds the timers one advance runs, and maxAtOnce those it
  // runs without the clock moving: a page that reschedules itself in a
  // hot loop fails the test at once instead of hanging it.
  constructor({ maxFires = 10_000, maxAtOnce = 1_000 } = {}) {
    this.maxFires = maxFires;
    this.maxAtOnce = maxAtOnce;
    this.t = 1_000_000;
    this.next = 1;
    this.timers = new Map();
    this.now = () => this.t;
    this.setTimeout = (f, ms) => {
      if (typeof f !== "function") throw new Error("a timer given a string");
      const id = this.next++;
      this.timers.set(id, { at: this.t + ms, f });
      return id;
    };
    this.clearTimeout = (id) => {
      this.timers.delete(id);
    };
  }

  // advance moves the clock by ms, running each timer due on the way, in
  // order, and letting the promises they start settle.
  async advance(ms) {
    const end = this.t + ms;
    let fired = 0;
    let atOnce = 0;
    for (;;) {
      await settle();
      let due = null;
      for (const [id, x] of this.timers) {
        if (x.at <= end && (due === null || x.at < due[1].at)) due = [id, x];
      }
      if (due === null) break;
      this.timers.delete(due[0]);
      atOnce = due[1].at > this.t ? 1 : atOnce + 1;
      fired++;
      if (fired > this.maxFires) throw new Error("clock: over " + this.maxFires + " timers in one advance of " + ms + " ms: a hot loop");
      if (atOnce > this.maxAtOnce) throw new Error("clock: over " + this.maxAtOnce + " timers at " + this.t + " with no time passing: a hot loop");
      this.t = Math.max(this.t, due[1].at);
      due[1].f();
    }
    this.t = end;
    await settle();
  }
}

// pending is the Web Crypto calls under way: they finish on node's
// thread pool, so settle waits for them by name, not by turns.
const pending = new Set();

function track(p) {
  pending.add(p);
  const done = () => pending.delete(p);
  p.then(done, done);
  return p;
}

// crypto is node's Web Crypto, its calls tracked for settle.
export const crypto = {
  getRandomValues: (a) => webcrypto.getRandomValues(a),
  subtle: {
    importKey: (...a) => track(webcrypto.subtle.importKey(...a)),
    sign: (...a) => track(webcrypto.subtle.sign(...a)),
  },
};

// settle lets every pending promise run, and every tracked crypto call
// end, until nothing is left to run.
export async function settle() {
  for (let round = 0; round < 1000; round++) {
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setImmediate(r));
    }
    if (pending.size === 0) {
      return;
    }
    await Promise.allSettled([...pending]);
  }
  throw new Error("settle: the page never went quiet");
}

// FakeStorage is localStorage; broken makes every access throw.
export class FakeStorage {
  constructor(init, broken) {
    this.m = new Map(Object.entries(init ?? {}));
    this.broken = broken ?? false;
  }

  getItem(k) {
    if (this.broken) throw new Error("SecurityError");
    return this.m.has(k) ? this.m.get(k) : null;
  }

  setItem(k, v) {
    if (this.broken) throw new Error("QuotaExceededError");
    this.m.set(k, String(v));
  }

  removeItem(k) {
    if (this.broken) throw new Error("SecurityError");
    this.m.delete(k);
  }
}

// FakeWindow is the window: its address and history.
export class FakeWindow {
  constructor(hash, origin) {
    this.origin = origin ?? "http://127.0.0.1:7171";
    this.location = { hash: hash ?? "", pathname: "/", origin: this.origin, host: this.origin.replace(/^http:\/\//, "") };
    this.replaced = [];
    this.listeners = new Map();
    this.history = {
      replaceState: (_s, _t, url) => {
        this.replaced.push(url);
        const i = url.indexOf("#");
        this.location.hash = i < 0 ? "" : url.slice(i);
      },
    };
  }

  addEventListener(type, f) {
    this.listeners.set(type, f);
  }

  navigate(hash) {
    this.location.hash = hash;
    this.listeners.get("hashchange")?.();
  }
}

export function token(c) {
  return c.repeat(43);
}

// CODE is the one-time code a FakeDaemon starts with, good once.
export const CODE = "c".repeat(43);

// mac is the proof's MAC, computed here apart from the page's code.
export function mac(tok, origin, nonce) {
  return createHmac("sha256", tok).update("caboose-web-proof-v1\n" + origin + "\n" + nonce).digest("base64url");
}

// FakeDaemon answers /api/ as the daemon does, for its token, and records
// every request. mode changes what answers:
//   "ok"        the daemon
//   "down"      no answer (fetch rejects)
//   "squatter"  a page of HTML on every path, status 200
//   "wrongmac"  proofs answered with another token's MAC
//   "squatter401" the API's JSON shape, 401 on every path, the proof too
//   "busy"      proofs refused as too many (429), as when something else
//               on the machine spends the daemon's proof limit
// redeemBusy refuses redeems as too many while set. codes are the
// one-time codes it takes, each once.
export class FakeDaemon {
  // log, when given, is shared with other fakes, so a test that puts one
  // server in another's place sees every request in order; now, when
  // given, stamps each request with the test's clock.
  constructor(tok, origin, { log = null, now = null } = {}) {
    this.token = tok;
    this.origin = origin ?? "http://127.0.0.1:7171";
    this.mode = "ok";
    this.requests = [];
    this.log = log;
    this.now = now;
    this.status = { daemon: { pid: 1, version: "v1.2.3", protocol: 1, started: "2026-10-18T00:00:00Z", host_execs: 0 }, envs: [{ name: "default", sandbox: "running", link: "linked" }] };
    this.answers = {};
    // handlers[method](params) -> {status?, body} or a promise of one:
    // an answer made when the request comes in.
    this.handlers = {};
    this.hold = null; // a promise status waits on, to test overlap
    this.proofHold = null; // a promise every proof's answer waits on, to test overlap
    this.codes = new Set([CODE]);
    this.redeemBusy = false;
    this.fetch = (url, init) => this.serve(url, init);
  }

  // withToken is the requests that carried a token.
  withToken() {
    return this.requests.filter((r) => r.token !== undefined);
  }

  calls(method) {
    return this.requests.filter((r) => r.method === method);
  }

  async serve(url, init) {
    const method = String(url).replace(/^\/api\//, "");
    const headers = init.headers ?? {};
    const body = JSON.parse(init.body);
    // proofKey is the token a proof's answer was made with, null when it
    // answered none.
    const rec = { method, token: headers["X-Caboose-Token"], body, init, server: this, at: this.now?.(), proofKey: null };
    this.requests.push(rec);
    this.log?.push(rec);
    if (this.mode === "down") {
      throw new TypeError("Failed to fetch");
    }
    if (this.mode === "squatter") {
      return response(200, "<!doctype html><title>hi</title>", "text/html");
    }
    if (this.mode === "squatter401") {
      return json(401, { error: { kind: "unauthorized", message: "run caboose ui" } });
    }
    if (method === "proof" && this.mode === "busy") {
      return json(429, { error: { kind: "too_many", message: "too many requests: try again in a moment" } });
    }
    if (method === "proof") {
      if (this.proofHold) await this.proofHold;
      rec.at = this.now?.(); // a proof's time is its answer's
      const key = this.mode === "wrongmac" ? token("z") : this.token;
      rec.proofKey = key;
      return json(200, { result: { mac: mac(key, this.origin, body.nonce) } });
    }
    if (method === "redeem") {
      if (this.redeemBusy) {
        return json(429, { error: { kind: "too_many", message: "too many requests: try again in a moment" } });
      }
      const ok = this.codes.delete(body.code);
      if (!ok || Object.keys(body).length !== 1) {
        return json(401, { error: { kind: "bad_code", message: "this address's code is used or out of date: run `caboose ui` again" } });
      }
      return json(200, { result: { token: this.token } });
    }
    if (rec.token !== this.token) {
      return json(401, { error: { kind: "unauthorized", message: "the page's token is missing or out of date: run `caboose ui`" } });
    }
    if (Object.hasOwn(this.handlers, method)) {
      const a = await this.handlers[method](body);
      return json(a.status ?? 200, a.body);
    }
    if (Object.hasOwn(this.answers, method)) {
      const a = this.answers[method];
      return json(a.status ?? 200, a.body);
    }
    if (method === "ping") {
      return json(200, { result: {} });
    }
    if (method === "status") {
      if (this.hold) await this.hold;
      return json(200, { result: this.status });
    }
    return json(404, { error: { kind: "unknown_method", message: "no such method" } });
  }
}

function response(status, text, type) {
  return {
    status,
    headers: { get: (k) => (k.toLowerCase() === "content-type" ? type : null) },
    text: async () => text,
  };
}

export function json(status, v) {
  return response(status, JSON.stringify(v), "application/json; charset=utf-8");
}
