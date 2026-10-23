// The page's calls to its daemon: POST /api/METHOD with a JSON body, the
// token in a header, and an answer that must have the API's one shape,
// {"result": ...} or {"error": {"kind", "message"}}. And the proof: the
// daemon shows it knows the token before the page sends it anywhere; and
// the redeem, which trades the one-time code the page was opened with
// for the token.

// TOKEN_HEADER is the header the token travels in. No cookie is ever
// used.
export const TOKEN_HEADER = "X-Caboose-Token";

// PROOF_DOMAIN starts every message a proof is a MAC of.
export const PROOF_DOMAIN = "caboose-web-proof-v1\n";

// The most of an answer the page reads.
const MAX_ANSWER = 4 << 20;

const tokenRe = /^[A-Za-z0-9_-]{43}$/;
const codeRe = /^[A-Za-z0-9_-]{43}$/;
const methodRe = /^[a-z][a-z0-9_]{0,63}$/;

// validToken reports whether s has a token's form.
export function validToken(s) {
  return typeof s === "string" && tokenRe.test(s);
}

// validCode reports whether s has a one-time code's form.
export function validCode(s) {
  return typeof s === "string" && codeRe.test(s);
}

// ApiError is a call that did not succeed. kind is the daemon's error
// kind, or one of the page's own:
//   network     no answer at all (the daemon is not running)
//   bad_answer  an answer that is not the API's (something else listens)
//   proof       the server did not prove it knows the token
export class ApiError extends Error {
  constructor(kind, message, status) {
    super(message);
    this.name = "ApiError";
    this.kind = kind;
    this.status = status ?? 0;
  }
}

// base64url is bytes in base64url without padding.
export function base64url(bytes) {
  let s = "";
  for (const b of bytes) {
    s += String.fromCharCode(b);
  }
  return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

// proofMAC is the MAC a server that knows token answers a proof of
// origin and nonce with.
export async function proofMAC(subtle, token, origin, nonce) {
  const enc = new TextEncoder();
  const key = await subtle.importKey("raw", enc.encode(token), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = await subtle.sign("HMAC", key, enc.encode(PROOF_DOMAIN + origin + "\n" + nonce));
  return base64url(new Uint8Array(sig));
}

function sameString(a, b) {
  if (typeof a !== "string" || typeof b !== "string" || a.length !== b.length) {
    return false;
  }
  let d = 0;
  for (let i = 0; i < a.length; i++) {
    d |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return d === 0;
}

function plainObject(v) {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

// createApi is the page's client of its daemon. fetch is the browser's
// (or a test's), origin the page's own, crypto the Web Crypto object.
export function createApi({ fetch, origin, crypto }) {
  // post sends one request and returns the answer's result, or throws an
  // ApiError.
  async function post(method, params, token) {
    if (!methodRe.test(method)) {
      throw new ApiError("bad_request", "no such method");
    }
    const headers = { "Content-Type": "application/json" };
    if (token !== null) {
      headers[TOKEN_HEADER] = token;
    }
    let res;
    try {
      res = await fetch("/api/" + method, {
        method: "POST",
        headers,
        body: JSON.stringify(params ?? {}),
        credentials: "omit",
        cache: "no-store",
        redirect: "error",
        mode: "same-origin",
        referrerPolicy: "no-referrer",
      });
    } catch {
      throw new ApiError("network", "no answer");
    }
    return answer(res);
  }

  async function answer(res) {
    const status = typeof res?.status === "number" ? res.status : 0;
    const bad = () => new ApiError("bad_answer", "an answer that is not caboose's", status);
    let type = "";
    let length = null;
    try {
      type = String(res.headers.get("content-type") ?? "").toLowerCase();
      length = res.headers.get("content-length");
    } catch {
      throw bad();
    }
    if (!type.startsWith("application/json") || (length !== null && Number(length) > MAX_ANSWER)) {
      throw bad();
    }
    let body;
    try {
      const s = await res.text();
      if (s.length > MAX_ANSWER) {
        throw bad();
      }
      body = JSON.parse(s);
    } catch {
      throw bad();
    }
    if (!plainObject(body)) {
      throw bad();
    }
    const keys = Object.keys(body);
    if (status === 200 && keys.length === 1 && keys[0] === "result") {
      return body.result;
    }
    const e = body.error;
    if (status >= 400 && keys.length === 1 && plainObject(e) && typeof e.kind === "string" && typeof e.message === "string") {
      throw new ApiError(e.kind.slice(0, 64), e.message.slice(0, 1024), status);
    }
    throw bad();
  }

  // prove asks the server to prove it knows token, and resolves if it
  // does; it throws an ApiError ("proof", or why no proof came) if not:
  // "busy" when the answer was a 429, since proofs are limited across
  // the daemon and anything on this machine can spend them. The token
  // itself is never sent.
  async function prove(token) {
    const subtle = crypto?.subtle;
    if (!subtle || typeof crypto.getRandomValues !== "function") {
      throw new ApiError("no_crypto", "this browser has no Web Crypto");
    }
    const raw = new Uint8Array(32);
    crypto.getRandomValues(raw);
    const nonce = base64url(raw);
    const want = await proofMAC(subtle, token, origin, nonce);
    let res;
    try {
      res = await post("proof", { nonce }, null);
    } catch (e) {
      // Whoever answers has proved nothing: its error, a 401 included,
      // is no reason to drop or doubt the token, only not to send it.
      if (e instanceof ApiError && e.kind === "network") throw e;
      if (e instanceof ApiError && e.status === 429) {
        throw new ApiError("busy", "too many proofs were asked for: trying again", 429);
      }
      throw new ApiError("proof", "the server did not prove it knows the token");
    }
    if (!plainObject(res) || !sameString(res.mac, want)) {
      throw new ApiError("proof", "the server did not prove it knows the token");
    }
  }

  // redeem trades a one-time code for the token, which it resolves to;
  // it throws an ApiError if not: a 401 (kind bad_code) when the code is
  // used, out of date or not the daemon's. The answer is checked like
  // any other; a token it holds is still only a candidate, proven before
  // the page sends it anywhere.
  async function redeem(code) {
    if (!validCode(code)) {
      throw new ApiError("bad_request", "no code");
    }
    const res = await post("redeem", { code }, null);
    if (!plainObject(res) || Object.keys(res).length !== 1 || !validToken(res.token)) {
      throw new ApiError("bad_answer", "an answer that is not caboose's", 200);
    }
    return res.token;
  }

  // call runs a daemon method with the token.
  async function call(method, params, token) {
    if (!validToken(token)) {
      throw new ApiError("bad_request", "no token");
    }
    return post(method, params, token);
  }

  return { prove, redeem, call };
}
