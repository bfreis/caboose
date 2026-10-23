// The page's entry: the one-time code from the address, traded for the
// token, the proof before the token is sent anywhere, the router, and the
// polling. Everything it
// reaches of the browser comes in through createApp's deps, so a test
// drives it with fakes; the last lines start it in a browser.

import { ApiError, createApi, validCode, validToken } from "./api.mjs";
import { createPages } from "./pages.mjs";
import { IDS, byId, el, navLink, parseRoute, replace, routeHash, sandboxText, useDocument } from "./dom.mjs";

// STORAGE_KEY is where the token is kept, in this tab's sessionStorage:
// never the origin's lasting storage, which whatever answers on this
// origin while the daemon is down could read back. sessionStorage is this
// tab's alone, and goes with it; the daemon makes a new token every time
// it starts, so a token kept from before lets nothing in once it is back.
export const STORAGE_KEY = "caboose.token";

// POLL_MS is how often status (and a page's own calls, by default) is
// asked for while the page is visible; SLOW_MS while the daemon fails.
export const POLL_MS = 2000;
export const SLOW_MS = 10000;
// BUSY_MIN_MS and BUSY_MAX_MS bound the wait before a proof is asked for
// again after a 429: it doubles from the first to the second.
export const BUSY_MIN_MS = 1000;
export const BUSY_MAX_MS = 30000;
// BUSY_SUSPECT_ROUNDS is how many proofs in a row refused as too many make
// the page add that something else may answer: a server that answers 429
// to every proof never proves anything, and the daemon's own limit lets a
// proof through within seconds.
export const BUSY_SUSPECT_ROUNDS = 5;
// REFUSED_ROUNDS is how many status polls in a row may be refused with a
// 401 after their proof verified before the token is dropped: the
// daemon's own refusal comes only when the token was replaced between a
// proof and its call, and the next proof then fails.
export const REFUSED_ROUNDS = 2;

// The pages a fragment's to= may name.
const toRe = /^(requests|proposals|requests\/[a-z0-9][a-z0-9_-]{0,31}|env\/[a-z0-9][a-z0-9_-]{0,31})$/;
const codeFragRe = /^#code=([^&]*)(?:&to=([^&]*))?$/;

// parseFragment is what the address's fragment holds:
//   {kind: "code", code, route}  #code=C, optionally &to=PAGE
//   {kind: "bad"}                a fragment that holds or looks to hold a
//                                code or a token, but is not that form
//                                (a token is never taken from an address)
//   {kind: "route", route}       a route (route null when unknown)
// A to= that is not one of the pages it may name leaves the overview.
export function parseFragment(hash) {
  const h = typeof hash === "string" ? hash : "";
  const m = codeFragRe.exec(h);
  if (m !== null) {
    if (!validCode(m[1])) {
      return { kind: "bad" };
    }
    const to = m[2];
    const route = to !== undefined && toRe.test(to) ? parseRoute("#/" + to) : null;
    return { kind: "code", code: m[1], route: route ?? { page: "overview", env: null } };
  }
  if (/(?:token|code)=/i.test(h)) {
    return { kind: "bad" };
  }
  return { kind: "route", route: parseRoute(h) };
}

// tokenStore keeps the token in storage, if the browser lets it: get
// returns a stored token of the right form or null; set reports whether
// the token is kept.
export function tokenStore(getStorage) {
  let s = null;
  try {
    s = getStorage();
  } catch {
    s = null;
  }
  let broken = !s;
  return {
    get broken() {
      return broken;
    },
    get() {
      if (broken) return null;
      try {
        const v = s.getItem(STORAGE_KEY);
        return validToken(v) ? v : null;
      } catch {
        broken = true;
        return null;
      }
    },
    set(t) {
      if (broken) return false;
      try {
        s.setItem(STORAGE_KEY, t);
        return s.getItem(STORAGE_KEY) === t;
      } catch {
        broken = true;
        return false;
      }
    },
    // remove forgets the stored token if it is t.
    remove(t) {
      if (broken) return;
      try {
        if (s.getItem(STORAGE_KEY) === t) {
          s.removeItem(STORAGE_KEY);
        }
      } catch {
        broken = true;
      }
    },
  };
}

// busyText is what the page says while proofs are refused as too many,
// after rounds refusals in a row, the page's origin answering at host.
export function busyText(rounds, host) {
  const again = " The page tries again on its own, and sends nothing until a proof succeeds.";
  if (rounds >= BUSY_SUSPECT_ROUNDS) {
    return "caboose's daemon has refused proofs as too many for a while, or something else answers on " + host + " and refuses every proof." + again + " If this lasts, run caboose ui.";
  }
  return "caboose's daemon is refusing proofs for now: too many were asked for, perhaps by something else on this machine." + again;
}

// The page's states, other than "ok", each with what it says.
const STATES = ["starting", "ok", "no_token", "squatter", "busy", "down", "no_crypto"];

function statusShape(s) {
  return s !== null && typeof s === "object" && Array.isArray(s.envs) && s.daemon !== null && typeof s.daemon === "object";
}

// createApp is the page, on deps:
//   win       location, history, addEventListener (the window)
//   doc       the document: nodes, visibilityState, addEventListener
//   fetch, crypto, storage (a function returning sessionStorage)
//   setTimeout, clearTimeout
//   page      {render(ctx) -> {title, body}, calls?(ctx) -> [{key,
//             method, params, every}], onPending?(env, n)}
export function createApp(deps) {
  const { win, doc, page } = deps;
  useDocument(doc);
  const api = createApi({ fetch: deps.fetch, origin: win.location.origin, crypto: deps.crypto });
  const store = tokenStore(deps.storage);

  let token = null; // the token in use: the stored one, or a redeemed one once proven and pinged
  let candidate = null; // a redeemed token, not proven and pinged yet
  let code = null; // the address's one-time code, not redeemed yet
  let redeeming = null; // the redeem under way, which every caller waits on
  let codeFailed = false; // the last code opened nothing: it was used or out of date
  let state = "starting";
  let busyMs = 0; // the last wait after a 429 to a proof, 0 once one verified
  let busyRounds = 0; // the status polls in a row refused as busy, 0 after any other outcome
  let refusedRounds = 0; // the status polls in a row refused with a 401 after their proof verified
  let route = { page: "overview", env: null };
  let status = null;
  let pending = null; // env name -> pending count, from the last status
  let data = new Map(); // a page's call key -> its last result
  let memoryOnly = false;
  let rendered = false;
  let last = null; // the title, h1 and body nodes main holds
  let stopped = false;
  const timers = new Map(); // key -> timer
  const busy = new Set(); // keys with a call in flight

  const visible = () => doc.visibilityState !== "hidden";

  // send calls method with t, each call after a proof of its own: the
  // server proves it knows t, with a fresh nonce, just before the call
  // that carries t goes out. No proof is reused, by a later call or a
  // concurrent one, so whatever took over the port since the last call
  // is never sent the token.
  async function send(t, method, params) {
    await api.prove(t);
    return api.call(method, params, t);
  }

  // takeCode trades the address's code for a candidate token, once: a
  // code the daemon refuses (used, out of date, never its) is dropped and
  // said; one that met no answer or too many redeems is kept for the next
  // round, until the daemon answers it. Any other failure drops it too.
  async function takeCode() {
    if (code === null) return;
    if (redeeming === null) {
      const c = code;
      redeeming = (async () => {
        try {
          const t = await api.redeem(c);
          if (code === c) {
            code = null;
            candidate = t;
            codeFailed = false;
          }
        } catch (e) {
          const refused = e instanceof ApiError && e.status === 401;
          const later = e instanceof ApiError && (e.kind === "network" || e.status === 429);
          if (code === c && !later) {
            code = null;
            codeFailed = refused;
          }
          if (!refused) throw e;
        } finally {
          redeeming = null;
        }
      })();
    }
    await redeeming;
  }

  // credential is the token to send, or null when there is none: a
  // redeemed token replaces the stored one only once the server proved
  // it knows it and a ping with it succeeded.
  async function credential() {
    await takeCode();
    if (candidate !== null) {
      const c = candidate;
      try {
        await send(c, "ping", {});
        if (candidate === c) {
          candidate = null;
          token = c;
          memoryOnly = !store.set(c);
        }
      } catch (e) {
        if (e instanceof ApiError && (e.kind === "proof" || e.status === 401)) {
          if (candidate === c) candidate = null; // stale or forged: the stored token stays
        } else {
          throw e;
        }
      }
    }
    if (token === null) {
      token = store.get();
      memoryOnly = false;
    }
    return token;
  }

  // forget drops t, and its stored copy. The storage is this tab's alone,
  // so no other page can have stored another token in it meanwhile.
  function forget(t) {
    if (token === t) token = null;
    store.remove(t);
  }

  function setState(s) {
    if (!STATES.includes(s)) throw new Error("app: no state " + s);
    if (stopped) return;
    const was = state;
    state = s;
    if (was !== s) render(false);
  }

  function fail(e, t) {
    if (!(e instanceof ApiError)) {
      e = new ApiError("internal", "the page failed: " + String(e?.message ?? e).slice(0, 200));
    }
    // A token the daemon no longer takes (caboose ui --new-token) shows
    // the same way in every tab, whichever way each tab meets it: a call
    // refused with a 401 after its proof verified (the token replaced in
    // between) is tried again, and the proof that comes first then fails
    // as it does in every other tab, so all say the same thing. The token
    // is kept, as a failed proof keeps it: dropping it here would have one
    // tab say to open the page while the others say it is out of date.
    // Only a server that proves the token and refuses it round after round
    // has it dropped.
    if (e.status === 401 && t !== null) {
      if (refusedRounds < REFUSED_ROUNDS) return "retry";
      forget(t);
      return setState("no_token");
    }
    switch (e.kind) {
      case "no_crypto":
        return setState("no_crypto");
      case "proof":
        return setState("squatter");
      case "bad_answer":
        return setState("squatter");
      case "busy":
        showAlert(busyText(busyRounds, win.location.host));
        setState("busy");
        return "busy";
      case "network":
        showAlert("caboose's daemon is not answering; run caboose ui.");
        return setState("down");
    }
    showAlert(e.message);
    return undefined;
  }

  // again asks for status at once after a page call or an answer failed
  // in a way a poll decides: a 401 after its proof.
  function again(r) {
    if (r !== "retry") return;
    pollNow();
  }

  function showAlert(msg) {
    const n = byId(IDS.alert);
    if (n) replace(n, msg);
  }

  function announce(msg) {
    const n = byId(IDS.status);
    if (n) replace(n, msg);
  }

  // pollStatus asks for status, at most one at a time, and schedules the
  // next.
  async function pollStatus() {
    if (busy.has("status") || stopped) return;
    busy.add("status");
    let next = POLL_MS;
    let t = null;
    try {
      t = await credential();
      if (t === null) {
        setState("no_token");
        next = null;
        return;
      }
      const s = await send(t, "status", {});
      if (!statusShape(s)) {
        throw new ApiError("bad_answer", "a status that is not caboose's", 200);
      }
      busyMs = 0;
      busyRounds = 0;
      refusedRounds = 0;
      if (state !== "ok") {
        showAlert("");
        state = "ok";
      }
      onStatus(s);
    } catch (e) {
      busyRounds = e instanceof ApiError && e.kind === "busy" ? busyRounds + 1 : 0;
      // A failed proof has no status: a 401 is always a call refused, whatever
      // kind its body names.
      refusedRounds = e instanceof ApiError && e.status === 401 ? refusedRounds + 1 : 0;
      const suspect = busyRounds === BUSY_SUSPECT_ROUNDS;
      const r = fail(e, t);
      if (suspect) render(false); // a page not shown yet says it in main
      if (r === "busy") {
        busyMs = Math.min(busyMs === 0 ? BUSY_MIN_MS : busyMs * 2, BUSY_MAX_MS);
        next = busyMs;
      } else {
        next = r === "retry" ? 0 : state === "no_crypto" || state === "no_token" ? null : SLOW_MS;
      }
    } finally {
      busy.delete("status");
      if (next !== null) schedule("status", next, pollStatus);
    }
  }

  function onStatus(s) {
    const was = pending;
    pending = new Map();
    for (const e of s.envs) {
      const n = Number.isSafeInteger(e?.pending) && e.pending > 0 ? e.pending : 0;
      if (typeof e?.name === "string") pending.set(e.name, n);
    }
    status = s;
    if (was !== null) {
      let added = 0;
      for (const [name, n] of pending) {
        const before = was.get(name) ?? 0;
        if (n > before) {
          added += n - before;
          page.onPending?.(name, n - before);
        }
      }
      if (added > 0) announce(added === 1 ? "1 new request" : added + " new requests");
    }
    render(false);
    startCalls();
  }

  // The current page's own calls, each on its own clock.
  function pageCalls() {
    if (typeof page.calls !== "function") return [];
    const list = page.calls(ctx());
    return Array.isArray(list) ? list : [];
  }

  function startCalls() {
    for (const c of pageCalls()) {
      if (!timers.has("call:" + c.key) && !busy.has("call:" + c.key)) {
        schedule("call:" + c.key, 0, () => runCall(c));
      }
    }
  }

  async function runCall(c) {
    const key = "call:" + c.key;
    if (busy.has(key) || stopped || state !== "ok") return;
    busy.add(key);
    let next = c.every ?? POLL_MS;
    let t = null;
    try {
      t = await credential();
      if (t === null) return;
      data.set(c.key, await send(t, c.method, c.params ?? {}));
      render(false);
    } catch (e) {
      again(fail(e, t));
      next = SLOW_MS;
    } finally {
      busy.delete(key);
      if (state === "ok" && pageCalls().some((x) => x.key === c.key)) schedule(key, next, () => runCall(c));
    }
  }

  function schedule(key, ms, f) {
    const old = timers.get(key);
    if (old !== undefined) deps.clearTimeout(old);
    timers.delete(key);
    if (stopped || !visible()) return;
    timers.set(
      key,
      deps.setTimeout(() => {
        timers.delete(key);
        f();
      }, ms),
    );
  }

  // pollNow asks for status at once, unless one is in flight.
  function pollNow() {
    const old = timers.get("status");
    if (old !== undefined) deps.clearTimeout(old);
    timers.delete("status");
    pollStatus();
  }

  function clearTimers() {
    for (const id of timers.values()) deps.clearTimeout(id);
    timers.clear();
  }

  function ctx() {
    return { route, status, data, call };
  }

  // call runs a method for the page (an answer, a revoke): the same
  // token, proof and failures as the polls.
  async function call(method, params) {
    const t = await credential();
    if (t === null) {
      setState("no_token");
      throw new ApiError("unauthorized", "no token", 401);
    }
    try {
      return await send(t, method, params);
    } catch (e) {
      again(fail(e, t));
      throw e;
    }
  }

  function message(...parts) {
    return { title: "caboose", body: [el("p", null, ...parts)] };
  }

  function view() {
    const ui = el("code", null, "caboose ui");
    switch (state) {
      case "no_token":
        if (codeFailed) return message("This address's code is used or out of date: run ", ui, " again.");
        return message("Open this page with ", ui, " in a terminal.");
      case "squatter":
        return message("The page's token is out of date (caboose's daemon makes a new one every time it starts), or something else answers on " + win.location.host + ": what answers could not prove it knows the token, so the page sends it nothing. Run ", ui, " again.");
      case "no_crypto":
        return message("This browser cannot check that the daemon is caboose's (it has no Web Crypto), so the page sends it nothing: use a current Safari, Chrome or Firefox.");
      case "starting":
        return message("Connecting to caboose's daemon.");
      case "busy":
        if (!rendered) return message(busyText(busyRounds, win.location.host));
        break;
      case "down":
        if (!rendered) return message("caboose's daemon is not answering; run ", ui, ".");
    }
    return page.render(ctx());
  }

  // The header's parts are replaced only when what they say changed, so
  // a poll does not take focus from a link in the nav.
  const headerSigs = new Map();
  function changed(key, sig) {
    if (headerSigs.get(key) === sig) return false;
    headerSigs.set(key, sig);
    return true;
  }

  function renderHeader() {
    const d = byId(IDS.daemon);
    if (d && changed("daemon", JSON.stringify([state, status?.daemon?.version, status?.daemon?.dev]))) {
      const info = state === "ok" && status !== null ? status.daemon : null;
      const version = typeof info?.version === "string" ? info.version : null;
      replace(d, version === null ? "" : ["daemon ", sandboxText(version), info.dev ? ", the checkout's daemon" : ", served by the release daemon"]);
    }
    const nav = byId(IDS.nav);
    let n = 0;
    for (const v of pending?.values() ?? []) n += v;
    if (nav && changed("nav", n + "/" + route?.page)) {
      const link = (hash, label, page) => el("li", null, navLink(hash, label, route?.page === page ? { "aria-current": "page" } : null));
      replace(nav, el("ul", null, link("#/", "Overview", "overview"), link("#/requests", "Requests (" + n + ")", "requests"), link("#/proposals", "Proposals", "proposals")));
    }
    const note = byId(IDS.note);
    const warn = memoryOnly || store.broken;
    if (note && changed("note", warn)) replace(note, warn ? "This browser does not let the page keep its token: a reload needs caboose ui again." : "");
  }

  // render draws the page; focus moves to its h1, as on a route change.
  function render(focus) {
    if (stopped) return;
    renderHeader();
    const main = byId(IDS.main);
    if (!main) return;
    let v;
    try {
      v = view();
    } catch (e) {
      v = message("This page could not be shown: " + String(e?.message ?? e).slice(0, 200));
    }
    const body = Array.isArray(v.body) ? v.body : [];
    // A render that is not a route change leaves main alone when the
    // title and the body's nodes are the ones already there: a page that
    // keeps its nodes keeps its focus, its selects and its place.
    if (!focus && last !== null && last.title === v.title && last.h1.parentNode === main && last.body.length === body.length && body.every((n, i) => n === last.body[i])) {
      if (state === "ok") rendered = true;
      return;
    }
    const h1 = el("h1", { id: IDS.title, tabindex: -1 }, v.title);
    replace(main, h1, body);
    last = { title: v.title, h1, body };
    if (state === "ok") rendered = true;
    if (focus) h1.focus();
  }

  // address handles the fragment: a code is taken out of the address at
  // once, before anything else runs, and is redeemed by the next round.
  function address() {
    const f = parseFragment(win.location.hash);
    if (f.kind === "route") {
      route = f.route;
      return false;
    }
    if (f.kind === "code") {
      route = f.route;
      code = f.code;
      codeFailed = false;
    }
    win.history.replaceState(null, "", win.location.pathname + (route === null ? "#/" : routeHash(route)));
    return f.kind === "code";
  }

  function onHash() {
    const fresh = address();
    data = new Map();
    clearTimers();
    render(true);
    if (fresh && state !== "no_crypto") {
      if (state === "no_token") setState("starting");
      pollStatus();
    } else if (state === "ok") {
      schedule("status", POLL_MS, pollStatus);
      startCalls();
    } else if (state !== "no_crypto" && state !== "no_token") {
      pollNow();
    }
  }

  function onVisibility() {
    if (!visible()) {
      clearTimers();
      return;
    }
    if (state === "no_crypto" || state === "no_token") return;
    pollNow();
  }

  function start() {
    address();
    win.addEventListener("hashchange", onHash);
    doc.addEventListener("visibilitychange", onVisibility);
    const c = deps.crypto;
    if (!c?.subtle || typeof c.getRandomValues !== "function") {
      state = "no_crypto";
      render(false);
      return;
    }
    render(false);
    if (visible()) pollStatus();
  }

  function stop() {
    stopped = true;
    clearTimers();
  }

  return {
    start,
    stop,
    call,
    get state() {
      return state;
    },
    get route() {
      return route;
    },
    get token() {
      return token;
    },
  };
}

// In a browser, start.
if (globalThis.window !== undefined && globalThis.window === globalThis && globalThis.document !== undefined) {
  const w = globalThis.window;
  createApp({
    win: w,
    doc: w.document,
    fetch: (...a) => w.fetch(...a),
    crypto: w.crypto,
    storage: () => w.sessionStorage,
    setTimeout: (f, ms) => w.setTimeout(f, ms),
    clearTimeout: (id) => w.clearTimeout(id),
    page: createPages({
      now: () => Date.now(),
      setTimeout: (f, ms) => w.setTimeout(f, ms),
      clearTimeout: (id) => w.clearTimeout(id),
    }),
  }).start();
}
