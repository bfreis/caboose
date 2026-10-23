// The page's decisions as pure functions, no DOM: what an answer's
// controls are, the hours it may take, how a diff line is classed, how
// long ago and how long left, which cards moved, and the params an answer
// carries. Everything that comes from the daemon is shape-checked here and
// left out when it is not of the expected shape, never trusted.

export const HOURS = Object.freeze([1, 4, 8, 24, 72, 168]);
export const MAX_HOURS = 168;
// GUARD_MS is how long a card that moved keeps its answer buttons off.
export const GUARD_MS = 1000;

const SCOPES = ["once", "hours", "always"];

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const isInt = (v) => Number.isSafeInteger(v);
const isStr = (v) => typeof v === "string";
const hashRe = /^[0-9a-f]{64}$/;

// time is the ms of an ISO time, or null for a missing or zero one (the
// daemon's zero time is the year 1).
export function time(iso) {
  if (!isStr(iso)) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) && t > 0 ? t : null;
}

// hoursChoices is the hours a select offers: HOURS within min..max, or
// the bounds themselves when none falls inside.
export function hoursChoices(min, max) {
  if (!isInt(min) || !isInt(max) || min < 1 || max > MAX_HOURS || min > max) return [];
  const within = HOURS.filter((h) => h >= min && h <= max);
  if (within.length > 0) return within;
  return min === max ? [min] : [min, max];
}

// clampHours is h held to min..max (and to 1..MAX_HOURS).
export function clampHours(h, min, max) {
  const lo = isInt(min) ? Math.max(1, min) : 1;
  const hi = isInt(max) ? Math.min(MAX_HOURS, max) : MAX_HOURS;
  const n = Number(h);
  if (!Number.isFinite(n)) return lo;
  return Math.min(hi, Math.max(lo, Math.round(n)));
}

// offerOf is the valid offer of a scope in a request result, or null.
export function offerOf(result, scope) {
  for (const o of offersOf(result)) {
    if (o.scope === scope) return o;
  }
  return null;
}

// offersOf is the result's offers that are of the expected shape, once
// per scope; the others are left out.
export function offersOf(result) {
  const out = [];
  if (!isObj(result) || !Array.isArray(result.offers)) return out;
  for (const o of result.offers) {
    if (!isObj(o) || !SCOPES.includes(o.scope) || !isStr(o.effect) || o.effect === "") continue;
    if (out.some((x) => x.scope === o.scope)) continue;
    const offer = { scope: o.scope, effect: o.effect };
    if (o.scope === "hours") {
      if (hoursChoices(o.hours_min, o.hours_max).length === 0) continue;
      offer.hours_min = o.hours_min;
      offer.hours_max = o.hours_max;
    }
    if (o.scope === "always") {
      if (!isStr(o.hash) || !hashRe.test(o.hash)) continue;
      offer.hash = o.hash;
      offer.diff = Array.isArray(o.diff) ? o.diff.filter(isStr) : [];
    }
    out.push(offer);
  }
  return out;
}

// answerControls is the answers a request gets, in order: Deny (always,
// for a pending request), Allow once, Allow for (with its hours), Allow
// always, the last three only when the host offers them. result is a
// request's result, {request, offers}.
export function answerControls(result) {
  if (!isObj(result) || !isObj(result.request) || result.request.state !== "pending") return [];
  const out = [{ id: "deny", verdict: "deny", label: "Deny" }];
  const once = offerOf(result, "once");
  if (once) out.push({ id: "once", verdict: "allow", scope: "once", label: "Allow once", effect: once.effect });
  const hours = offerOf(result, "hours");
  if (hours) {
    out.push({ id: "hours", verdict: "allow", scope: "hours", label: "Allow for", effect: hours.effect, min: hours.hours_min, max: hours.hours_max, choices: hoursChoices(hours.hours_min, hours.hours_max) });
  }
  const always = offerOf(result, "always");
  if (always) out.push({ id: "always", verdict: "allow", scope: "always", label: "Allow always", effect: always.effect });
  return out;
}

// hoursText is "1 hour" or "8 hours".
export function hoursText(h) {
  return h === 1 ? "1 hour" : h + " hours";
}

// effectText is an offer's effect with its hours in.
export function effectText(effect, hours) {
  return isStr(effect) ? effect.split("{hours}").join(isInt(hours) ? String(hours) : "{hours}") : "";
}

const KEY_SHOWN = 60;

function subject(view) {
  const key = isStr(view?.key) && view.key !== "" ? view.key : "";
  if (key === "") return "request " + view?.id;
  return key.length > KEY_SHOWN ? key.slice(0, KEY_SHOWN) + "..." : key;
}

// confirmTitle is the dialog's title, naming the action.
export function confirmTitle(choice, view) {
  const n = "request " + view?.id;
  switch (choice.scope) {
    case "once":
      return "Allow " + n + " once?";
    case "hours":
      return "Allow " + n + " for " + hoursText(choice.hours) + "?";
    case "always":
      return "Allow " + n + " always?";
  }
  return "Deny " + n + "?";
}

// actionParts is the confirm button's words as [before, subject, after];
// the subject is what the sandbox named, which the page isolates.
export function actionParts(choice, view) {
  const s = subject(view);
  switch (choice.scope) {
    case "once":
      return ["Allow ", s, " once"];
    case "hours":
      return ["Allow ", s, " for " + hoursText(choice.hours)];
    case "always":
      return ["Allow ", s, " always"];
  }
  return ["Deny ", s, ""];
}

// actionLabel is the confirm button, the action in full, as plain text.
export function actionLabel(choice, view) {
  return actionParts(choice, view).join("");
}

// answerParams is the params of an answer. A deny carries no scope. An
// "always" carries confirm_diff, the hash of the offer given as fresh,
// which the caller fetched when the dialog opened; no other offer will
// do, and without one this throws.
export function answerParams(env, id, choice, fresh) {
  const p = { env, id };
  if (choice.verdict === "deny") {
    p.verdict = "deny";
    return p;
  }
  p.verdict = "allow";
  p.scope = choice.scope;
  if (choice.scope === "hours") {
    p.hours = clampHours(choice.hours, choice.min, choice.max);
  } else if (choice.scope === "always") {
    if (!isObj(fresh) || fresh.scope !== "always" || !isStr(fresh.hash) || !hashRe.test(fresh.hash)) {
      throw new Error("an always answer needs the hash of the diff just fetched");
    }
    p.confirm_diff = fresh.hash;
  } else if (choice.scope !== "once") {
    throw new Error("no such scope");
  }
  return p;
}

// diffLineClass is the class of a diff line, by its first character.
export function diffLineClass(line) {
  switch (isStr(line) ? line[0] : "") {
    case "+":
      return "diff-add";
    case "-":
      return "diff-del";
    case " ":
      return "diff-ctx";
  }
  return "diff-other";
}

function span(sec) {
  if (sec < 60) return sec + " s";
  const min = Math.floor(sec / 60);
  if (min < 60) return min + " min";
  const h = Math.floor(min / 60);
  if (h < 48) return h + " h" + (min % 60 > 0 && h < 6 ? " " + (min % 60) + " min" : "");
  return Math.floor(h / 24) + " d";
}

// relativeTime is "5 min ago" (or "in 5 min") for an ISO time, "" for a
// missing one.
export function relativeTime(iso, now) {
  const t = time(iso);
  if (t === null) return "";
  const sec = Math.round((now - t) / 1000);
  if (Math.abs(sec) < 5) return "just now";
  return sec > 0 ? span(sec) + " ago" : "in " + span(-sec);
}

// countdown is the time left until an ISO time: "1 h 5 min left", or
// "expired"; "" for a missing time.
export function countdown(iso, now) {
  const t = time(iso);
  if (t === null) return "";
  const sec = Math.ceil((t - now) / 1000);
  return sec <= 0 ? "expired" : span(sec) + " left";
}

// absoluteTime is an ISO time as the title of a <time>.
export function absoluteTime(iso) {
  const t = time(iso);
  return t === null ? "" : new Date(t).toISOString();
}

// asRequest is a request view if it has the shape, else null.
export function asRequest(v) {
  if (!isObj(v) || !isInt(v.id) || v.id < 1 || !isStr(v.kind) || !isStr(v.state)) return null;
  return v;
}

// requestList is the valid views of a requests result.
export function requestList(result) {
  if (!isObj(result) || !Array.isArray(result.requests)) return [];
  return result.requests.map(asRequest).filter((r) => r !== null);
}

// sortPending orders items ({env, view}) newest first; the number breaks
// a tie, then the environment.
export function sortPending(items) {
  const at = (i) => time(i.view.created) ?? 0;
  return [...items].sort((a, b) => at(b) - at(a) || b.view.id - a.view.id || (a.env < b.env ? -1 : a.env > b.env ? 1 : 0));
}

// sortDecided orders items by when they were decided, newest first.
export function sortDecided(items) {
  const at = (i) => time(i.view.decided_at) ?? time(i.view.created) ?? 0;
  return [...items].sort((a, b) => at(b) - at(a) || b.view.id - a.view.id);
}

// cardKey names a request across environments.
export function cardKey(env, id) {
  return env + "/" + id;
}

// movedCards is the keys in next whose place is not what it was in prev,
// new ones included. With no prev (the first time) nothing moved.
export function movedCards(prev, next) {
  const moved = new Set();
  if (prev === null) return moved;
  next.forEach((k, i) => {
    if (prev[i] !== k) moved.add(k);
  });
  return moved;
}

// askFields is what the sandbox wrote in a request, as labelled fields
// in order; absent ones are left out.
export function askFields(view) {
  const out = [];
  const ask = isObj(view?.ask) ? view.ask : {};
  const text = (label, v) => {
    if (isStr(v) && v !== "") out.push({ label, text: v });
  };
  text("Context", ask.context);
  if (Array.isArray(ask.options)) {
    const list = ask.options.filter((o) => isStr(o) && o !== "");
    if (list.length > 0) out.push({ label: "Options", list });
  }
  text("Recommendation", ask.recommendation);
  text("Done meanwhile", ask.meanwhile);
  text("Session", view?.session);
  return out;
}

// stateText is a request's state in words.
export function stateText(view) {
  switch (view.state) {
    case "pending":
      return "pending";
    case "allowed":
      return "allowed" + (view.scope === "hours" && isInt(view.hours) ? " for " + hoursText(view.hours) : view.scope ? " " + view.scope : "");
    case "denied":
      return "denied";
    case "expired":
      return "expired";
  }
  return isStr(view.state) ? view.state : "";
}

// announcement is what the live region says after an answer; result is
// the answer's result and choice the one made.
export function announcement(result, choice, id) {
  const n = isInt(result?.request?.id) ? result.request.id : id;
  if (choice.verdict === "deny") return "Request " + n + " denied";
  switch (choice.scope) {
    case "once":
      return "Request " + n + " allowed once";
    case "hours":
      return "Request " + n + " allowed for " + hoursText(choice.hours);
    case "always":
      return "Request " + n + " allowed always";
  }
  return "Request " + n + " answered";
}

// checkpointLine is the one line the overview gives an environment's
// checkpoints, from its status.
export function checkpointLine(cp) {
  if (!isObj(cp)) return "";
  const paths = Array.isArray(cp.paths) ? cp.paths.filter(isObj) : [];
  const parts = [paths.length === 1 ? "1 path" : paths.length + " paths"];
  if (isStr(cp.last_error) && cp.last_error !== "") parts.push("last error: " + cp.last_error);
  if (cp.low_space === true) parts.push("low disk space");
  if (isStr(cp.exposed) && cp.exposed !== "") parts.push(cp.exposed);
  if (isStr(cp.slow) && cp.slow !== "") parts.push(cp.slow);
  return parts.join("; ");
}

// methodText says a checkpoint method plainly.
export function methodText(m) {
  switch (m) {
    case "clonefile":
      return "clonefile (instant copy-on-write)";
    case "reflink":
      return "reflink (instant copy-on-write)";
    case "copy-and-link":
      return "copy and link (slower: walks every file)";
  }
  return isStr(m) ? m : "";
}

// servedHere is whether this daemon serves an environment: one whose
// status names another owner does not.
export function servedHere(env) {
  if (!isObj(env) || !isStr(env.name)) return false;
  return env.owner === undefined || env.owner === null || env.owner === "";
}

// grantLeft is the seconds a grant has left now, from its expiry.
export function grantLeft(g, now) {
  const t = time(g.expires);
  if (t !== null) return Math.max(0, Math.ceil((t - now) / 1000));
  return isInt(g.left_seconds) ? Math.max(0, g.left_seconds) : 0;
}

// asGrant is a grant if it has the shape, else null.
export function asGrant(g) {
  if (!isObj(g) || !isStr(g.id) || !/^g[0-9]{1,18}$/.test(g.id) || !isStr(g.kind) || !isStr(g.key)) return null;
  return g;
}

// ---- details and doctor ----

const nonNeg = (v) => Number.isSafeInteger(v) && v >= 0;

// formatBytes is a byte count in binary units: "512 B", "1.5 GiB".
export function formatBytes(n) {
  if (!nonNeg(n)) return "";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return (u === 0 || v >= 100 ? Math.round(v) : Math.round(v * 10) / 10) + " " + units[u];
}

// ownerText says which daemon serves an environment, with its address,
// from the status's owner ("release" or "dev"); anything else is shown
// as it is, and the caller isolates it.
export function ownerText(owner) {
  if (owner === "release") return "the release daemon (127.0.0.1:7171)";
  if (owner === "dev") return "the checkout's daemon (127.0.0.1:7172)";
  return isStr(owner) ? owner : "";
}

// ownerKnown is whether ownerText names a daemon the host knows.
export function ownerKnown(owner) {
  return owner === "release" || owner === "dev";
}

// imageText is the image's state and why, in words.
export function imageText(image) {
  if (!isObj(image) || !isStr(image.state)) return "";
  const words = { current: "current", stale: "stale: rebuilt at the next restart", switched: "switched: another image than the one the sandbox runs", unknown: "unknown" };
  const s = Object.hasOwn(words, image.state) ? words[image.state] : image.state;
  return isStr(image.why) && image.why !== "" ? s + " (" + image.why + ")" : s;
}

// memoryText is the sandbox's memory, with its limit when known; "" when
// not known.
export function memoryText(r) {
  if (!isObj(r) || !nonNeg(r.memory_bytes) || r.memory_bytes === 0) return "";
  const lim = nonNeg(r.memory_limit_bytes) && r.memory_limit_bytes > 0 ? " of " + formatBytes(r.memory_limit_bytes) : "";
  return formatBytes(r.memory_bytes) + lim;
}

// vnodesText is the Mac files a VM holds against the limit; "" when not
// known.
export function vnodesText(r) {
  if (!isObj(r) || !nonNeg(r.vnodes) || r.vnodes === 0) return "";
  return String(r.vnodes) + (nonNeg(r.vnodes_max) && r.vnodes_max > 0 ? " of " + r.vnodes_max : "");
}

// portText is a forwarded port as the address to type: text, never a
// link, since what listens there is the sandbox's.
export function portText(p) {
  return isObj(p) && Number.isSafeInteger(p.port) && p.port >= 1 && p.port <= 65535 ? "http://localhost:" + p.port : "";
}

// detailsOf is an env_details result cut to the expected shapes, or null.
export function detailsOf(r) {
  if (!isObj(r) || !isStr(r.env)) return null;
  const list = (v, ok) => (Array.isArray(v) ? v.filter(ok) : []);
  return {
    env: r.env,
    isolation: isStr(r.isolation) ? r.isolation : "",
    image: isObj(r.image) && isStr(r.image.state) ? { state: r.image.state, why: isStr(r.image.why) ? r.image.why : "" } : null,
    sessions: list(r.sessions, isStr),
    resources: isObj(r.resources) ? r.resources : null,
    ports: list(r.ports, (p) => portText(p) !== ""),
    keep: list(r.keep, (k) => isObj(k) && isStr(k.path)),
    errors: list(r.errors, (e) => isObj(e) && isStr(e.part) && isStr(e.message)),
  };
}

// logLines is the lines of a link_log result, and whether the log held
// more.
export function logLines(r) {
  if (!isObj(r) || !Array.isArray(r.lines)) return { lines: [], truncated: false };
  return { lines: r.lines.filter(isStr), truncated: r.truncated === true };
}

export const LEVELS = Object.freeze(["problem", "unchecked", "note", "ok"]);
const LEVEL_TITLES = { problem: "Problems", unchecked: "Not checked", note: "Notes", ok: "Passed" };

// groupFindings is a doctor result's findings grouped by level, problems
// first, then not checked, notes and passed; the order within a level is
// the daemon's. A finding of another level or shape is left out.
export function groupFindings(result) {
  const out = LEVELS.map((level) => ({ level, title: LEVEL_TITLES[level], findings: [] }));
  const fs = isObj(result) && Array.isArray(result.findings) ? result.findings : [];
  for (const f of fs) {
    if (!isObj(f) || !isStr(f.label) || !isStr(f.message)) continue;
    const g = out.find((x) => x.level === f.level);
    if (g) g.findings.push({ label: f.label, message: f.message, fix: isStr(f.fix) ? f.fix : "" });
  }
  return out.filter((g) => g.findings.length > 0);
}

// doctorSummary counts the findings by level, in words.
export function doctorSummary(result) {
  const g = groupFindings(result);
  if (g.length === 0) return "No findings.";
  return g.map((x) => x.findings.length + " " + x.title.toLowerCase()).join(", ");
}

// ---- egress rules ----

// MAX_EGRESS_ENTRY is the longest entry the daemon takes, in bytes.
export const MAX_EGRESS_ENTRY = 260;

// egressLists are a permissions result's egress_allow and egress_deny,
// strings only, or null when the environment does not run under vm.
export function egressLists(perm) {
  if (!isObj(perm) || perm.isolation !== "vm") return null;
  const eg = isObj(perm.egress) ? perm.egress : {};
  const strs = (l) => (Array.isArray(l) ? l.filter((x) => isStr(x) && x !== "") : []);
  return { allow: strs(eg.allow), deny: strs(eg.deny) };
}

const nameRe = /^(\*\.)?([a-z0-9_-]{1,63}\.)*[a-z0-9_-]{1,63}\.?$/i;
const cidrRe = /^[0-9a-f:.]+(\/[0-9]{1,3})?$/i;

// egressHint is "" for an entry that looks like one egress_allow takes (a
// name, *.name, a CIDR or an address), else what looks wrong. A hint only:
// the daemon decides.
export function egressHint(entry) {
  if (!isStr(entry) || entry === "") return "Type a host name, *.name, a CIDR or an address.";
  if (entry.length > MAX_EGRESS_ENTRY) return "That is longer than an entry can be.";
  if (/[\s,]/.test(entry)) return "One entry at a time: no spaces or commas.";
  if (!nameRe.test(entry) && !cidrRe.test(entry)) return "That does not look like a host name, *.name, a CIDR or an address; the daemon will say.";
  return "";
}

const LIST_KEYS = { allow: "egress_allow", deny: "egress_deny" };

// egressParams are edit_egress's params for adding (op "add") or removing
// entry in list ("allow" or "deny"), with confirm_diff when hash is one.
export function egressParams(env, op, list, entry, hash) {
  if (!Object.hasOwn(LIST_KEYS, list) || (op !== "add" && op !== "remove")) throw new Error("no such egress edit");
  const p = { env, [op + "_" + list]: [entry] };
  if (hash !== undefined && hash !== null) {
    if (!isStr(hash) || !hashRe.test(hash)) throw new Error("bad confirmation hash");
    p.confirm_diff = hash;
  }
  return p;
}

// egressWords are the confirmation's words for an egress edit: its title
// and the action button's text.
export function egressWords(op, list, entry) {
  const key = LIST_KEYS[list] ?? "";
  const action = op === "add" ? "Add " + entry + " to " + key : "Remove " + entry + " from " + key;
  return { title: action + "?", action };
}

// egressResult is edit_egress's result if it has the shape the page
// needs (a diff of lines, its hash, the effect), else null.
export function egressResult(r) {
  if (!isObj(r) || !Array.isArray(r.diff) || r.diff.length === 0 || !r.diff.every(isStr) || !isStr(r.hash) || !hashRe.test(r.hash) || !isStr(r.effect)) return null;
  return { diff: r.diff, hash: r.hash, effect: r.effect, written: r.written === true };
}

const PART_KINDS = ["section", "packages", "root", "alias", "writable"];
const fileRe = /^[^/.\u0000][^/\u0000]{0,254}$/;

// appliableParts reports whether parts are what the page applies: one
// part, or packages with a root; packages, and only they, build the image
// first (needs_build), which proposal_build does.
function appliableParts(parts, needsBuild) {
  const kinds = parts.map((x) => x.kind).sort();
  const packages = kinds.includes("packages");
  if (packages !== needsBuild) return false;
  return kinds.length === 1 || (kinds.length === 2 && kinds[0] === "packages" && kinds[1] === "root");
}

// asProposal is one proposal of a proposals result if it has the shape
// the page needs, else null. A proposal the page may apply has its hash,
// no refusals and at least one part; anything less is shown as one it
// may not apply.
export function asProposal(p) {
  if (!isObj(p) || !isStr(p.file) || !fileRe.test(p.file) || !p.file.endsWith(".toml")) return null;
  const strs = (l) => (Array.isArray(l) ? l.filter(isStr) : []);
  const parts = (Array.isArray(p.parts) ? p.parts : [])
    .filter((x) => isObj(x) && PART_KINDS.includes(x.kind) && isStr(x.summary))
    .map((x) => ({ kind: x.kind, summary: x.summary, diff: strs(x.diff) }));
  const confirm = isObj(p.confirm) && p.confirm.kind === "type_name" && isStr(p.confirm.name) && p.confirm.name !== "" ? { kind: "type_name", name: p.confirm.name } : { kind: "none", name: "" };
  const out = {
    file: p.file,
    modified: isStr(p.modified) ? p.modified : "",
    title: isStr(p.title) ? p.title : "",
    reason: isStr(p.reason) ? p.reason : "",
    parts,
    refusals: strs(p.refusals),
    warnings: strs(p.warnings),
    web_apply: p.web_apply === true,
    web_apply_why: isStr(p.web_apply_why) ? p.web_apply_why : "",
    confirm,
    needs_build: p.needs_build === true,
    hash: isStr(p.hash) && hashRe.test(p.hash) ? p.hash : "",
  };
  if (out.web_apply && (out.hash === "" || out.refusals.length > 0 || !appliableParts(parts, out.needs_build))) {
    out.web_apply = false;
    if (out.web_apply_why === "") out.web_apply_why = "the daemon's answer was not one the page can apply";
  }
  return out;
}

// proposalList is a proposals result's proposals that have the shape.
export function proposalList(r) {
  if (!isObj(r) || !Array.isArray(r.proposals)) return [];
  return r.proposals.map(asProposal).filter((p) => p !== null);
}

// applyCommand is caboose's apply command for env, to copy.
export function applyCommand(env) {
  return env === "default" ? "caboose apply" : "caboose -e " + env + " apply";
}

// partLabel names a part's kind for a person.
export function partLabel(kind) {
  switch (kind) {
    case "section":
      return "The image: a section in the Dockerfile";
    case "packages":
      return "The image: its packages";
    case "root":
      return "A host directory mounted into the sandbox ([roots])";
    case "alias":
      return "A host command the sandbox may run ([alias])";
    case "writable":
      return "A part of a root the sandbox may write ([roots], writable)";
  }
  return "A change";
}

// partDiffLabel is what a part's diff changes.
export function partDiffLabel(kind) {
  switch (kind) {
    case "section":
      return "The change to the Dockerfile";
    case "packages":
      return "The change to the packages";
  }
  return "The change to config.toml";
}

// proposalApplyParams are proposal_apply's params: the hash of the
// proposal as the dialog showed it, and the name typed when it takes one.
export function proposalApplyParams(env, file, hash, typed) {
  if (!isStr(hash) || !hashRe.test(hash)) throw new Error("bad proposal hash");
  const p = { env, file, hash };
  if (isStr(typed) && typed !== "") p.typed = typed;
  return p;
}

// proposalDeleteParams are proposal_delete's params: the hash of the
// proposal as the list showed it, so that what is deleted is what was seen.
export function proposalDeleteParams(env, file, hash) {
  if (!isStr(hash) || !hashRe.test(hash)) throw new Error("bad proposal hash");
  return { env, file, hash };
}

// proposalBuildParams are proposal_build's params: proposal_apply's.
export const proposalBuildParams = proposalApplyParams;

const JOB_KINDS = ["apply"];
const JOB_STATES = ["running", "done", "failed"];
// The most of a job the page keeps: daemonproto's bounds.
const MAX_JOB_LINES = 20;
const MAX_JOB_LINE = 200;
const MAX_JOB_NOTES = 8;
const MAX_JOB_TEXT = 4096;

// asJob is an environment's job, from status or a job result, if it has
// the shape the page needs, else null: its strings bounded, anything else
// in its lists dropped.
export function asJob(j) {
  if (!isObj(j) || !JOB_KINDS.includes(j.kind) || !JOB_STATES.includes(j.state) || !isStr(j.since)) return null;
  const strs = (l, n, max) => (Array.isArray(l) ? l.filter(isStr).slice(-n).map((x) => x.slice(0, max)) : []);
  const proposal = isStr(j.proposal) && fileRe.test(j.proposal) ? j.proposal : "";
  if (j.kind === "apply" && proposal === "") return null;
  return {
    kind: j.kind,
    state: j.state,
    since: j.since,
    finished: isStr(j.finished) ? j.finished : "",
    error: isStr(j.error) ? j.error.slice(0, MAX_JOB_TEXT) : "",
    proposal,
    lines: strs(j.lines, MAX_JOB_LINES, MAX_JOB_LINE),
    notes: j.state === "done" ? strs(j.notes, MAX_JOB_NOTES, MAX_JOB_TEXT) : [],
  };
}

// envJob is env's job in a status result, or null.
export function envJob(status, env) {
  const e = isObj(status) && Array.isArray(status.envs) ? status.envs.find((x) => isObj(x) && x.name === env) : null;
  return e ? asJob(e.job) : null;
}

// latestJob is the later of a job status shows and one the page started,
// by when each started: status lags the start by a poll.
export function latestJob(shown, started) {
  if (!started) return shown;
  if (!shown) return started;
  const a = Date.parse(shown.since);
  const b = Date.parse(started.since);
  return Number.isFinite(a) && Number.isFinite(b) && b > a ? started : shown;
}

// applyJobText is what an apply job says of itself, in a line.
export function applyJobText(j) {
  switch (j.state) {
    case "running":
      return "Applying proposal " + j.proposal + ": building the image with its packages first. Nothing is written unless it builds.";
    case "done":
      return "Applied proposal " + j.proposal + ".";
  }
  return "Applying proposal " + j.proposal + " failed: " + (j.error === "" ? "the daemon did not say why" : j.error);
}

// applyNotes is proposal_apply's notes, when it says it applied.
export function applyNotes(r) {
  if (!isObj(r) || r.applied !== true) return null;
  return Array.isArray(r.notes) ? r.notes.filter(isStr) : [];
}
