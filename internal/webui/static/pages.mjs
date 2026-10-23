// The pages: the overview, the requests with their answers and the
// confirmation, and an environment's permissions, grants, egress rules,
// checkpoints and link log. createPages returns the hook app.mjs takes: render, calls.
//
// A poll must not disturb the person. app.mjs renders on every status
// poll, so each page keeps its nodes: a card is rebuilt only when its data
// changed, a list is refilled only when its order or members changed, the
// element that had focus gets it back, a select keeps its value, times
// are updated in place, and the confirmation is a <dialog> in a host
// outside <main>, which no render touches.
//
// Every node is made by dom.mjs. What a sandbox wrote is shown through
// sandboxText and sandboxBlock, as text, whole, never linkified.

import { IDS, byId, el, navLink, replace, sandboxBlock, sandboxText } from "./dom.mjs";
import * as M from "./model.mjs";

const envNameRe = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const MAX_DETAILS = 20; // requests per environment fetched in full
const DECIDED_LIMIT = 50;
const FAST_MS = 2000;
const SLOW_MS = 10000;
const LOG_LINES = 200;

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const isStr = (v) => typeof v === "string";

function same(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

// contains is whether node is root or inside it.
function contains(root, node) {
  for (let n = node; n; n = n.parentNode) {
    if (n === root) return true;
  }
  return false;
}

// active is the focused element, if any.
function active() {
  return byId(IDS.main)?.ownerDocument?.activeElement ?? null;
}

// The page's title for a route, as app.mjs words it.
function titleOf(route) {
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

// box is a node whose content is rebuilt only when its signature
// changes; its times tick on every render.
function box(tag, attrs) {
  return { node: el(tag, attrs), sig: undefined, ticks: [] };
}

function setBox(b, sig, build) {
  if (b.sig !== sig) {
    b.sig = sig;
    b.ticks = [];
    const nodes = build(b.ticks);
    replace(b.node, nodes);
  }
  for (const t of b.ticks) t();
}

function setText(node, text) {
  if (node.textValue !== text) {
    node.textValue = text;
    replace(node, text);
  }
}

// keyedList keeps one node per key in container: an item whose signature
// is unchanged keeps its node, a changed one is rebuilt (keeping its
// state, and the focus if it had it), and the container is refilled only
// when the order or the members changed.
function keyedList(container, emptyNode) {
  let cache = new Map();
  return {
    get cache() {
      return cache;
    },
    update(items) {
      const act = active();
      const next = new Map();
      let toFocus = null;
      for (const item of items) {
        const old = cache.get(item.key);
        if (old && old.sig === item.sig) {
          next.set(item.key, old);
          continue;
        }
        let focusName = null;
        if (old && act && contains(old.node, act)) {
          focusName = Object.keys(old.ctrls).find((k) => old.ctrls[k] === act) ?? null;
        }
        const state = old ? old.state : {};
        const built = item.build(state);
        const entry = { key: item.key, sig: item.sig, state, node: built.node, ctrls: built.ctrls ?? {}, ticks: built.ticks ?? [] };
        next.set(item.key, entry);
        if (focusName !== null && entry.ctrls[focusName]) toFocus = entry.ctrls[focusName];
      }
      cache = next;
      const want = items.length === 0 ? [emptyNode] : items.map((i) => next.get(i.key).node);
      const have = Array.from(container.childNodes);
      if (have.length !== want.length || want.some((n, i) => n !== have[i])) {
        replace(container, want);
      }
      if (toFocus) toFocus.focus();
      else if (act && contains(container, act) && active() !== act) act.focus();
      for (const e of next.values()) for (const t of e.ticks) t();
      return [...next.values()];
    },
  };
}

function dl(rows) {
  return el("dl", null, rows.map(([t, ...d]) => [el("dt", null, t), el("dd", null, ...d)]));
}

// dataTable is a table of heads and rows, each row its header cell then
// its data cells; a cell is what it shows, or {cell, class} for a cell
// with a class, and "" past the heads is no cell. Every data cell carries
// its column's head as data-label, which a narrow window shows beside it
// when app.css stacks the cells; the roles keep the table a table to
// assistive technology once the CSS makes its parts blocks.
function dataTable(heads, rows) {
  const cell = (row, i) => {
    const c = row[i];
    if (i >= heads.length && c === "") return "";
    const has = c !== null && typeof c === "object" && Object.hasOwn(c, "cell");
    const attrs = { class: has ? c.class : null, "data-label": i > 0 && i < heads.length ? heads[i] : null };
    const content = has ? c.cell : c;
    return i === 0 ? el("th", { ...attrs, scope: "row", role: "rowheader" }, content) : el("td", { ...attrs, role: "cell" }, content);
  };
  return el(
    "table",
    { role: "table" },
    el("thead", { role: "rowgroup" }, el("tr", { role: "row" }, heads.map((h) => el("th", { scope: "col", role: "columnheader" }, h)))),
    el(
      "tbody",
      { role: "rowgroup" },
      rows.map((row) => el("tr", { role: "row" }, row.map((_, i) => cell(row, i)).filter((c) => c !== ""))),
    ),
  );
}

function copyCmd(cmd) {
  return el("code", { class: "cmd" }, cmd);
}

function envLink(name) {
  return envNameRe.test(name) ? navLink("#/env/" + name, name) : sandboxText(name);
}

export function createPages(deps) {
  const now = deps.now;
  const later = deps.setTimeout;
  const unlater = deps.clearTimeout;
  let call = null; // the app's call, from the last render
  let dialogOpen = false;
  const messages = new Map(); // route key -> a static page

  function timeEl(iso, mode, ticks) {
    if (M.time(iso) === null) return "";
    const n = el("time", { datetime: iso, title: M.absoluteTime(iso) });
    let last = null;
    const tick = () => {
      const s = mode === "left" ? M.countdown(iso, now()) : M.relativeTime(iso, now());
      if (s !== last) {
        last = s;
        replace(n, s);
      }
    };
    tick();
    ticks.push(tick);
    return n;
  }

  function announce(msg) {
    const n = byId(IDS.status);
    if (n) replace(n, msg);
  }

  function alertMsg(msg) {
    const n = byId(IDS.alert);
    if (n) replace(n, msg);
  }

  // ---- the confirmation ----

  // confirm opens the dialog for spec:
  //   title, action (the button's words), opener (the button to give
  //   focus back to), lines [{label, text}] (the host's text),
  //   load() -> {diff, hash, ...} (optional: the diff, fetched now),
  //   loadedLines(loaded) -> [{label, text}] (optional: the host's text
  //   that came with the diff), diffNodes(loaded) -> nodes (optional: the
  //   diff shown otherwise than as loaded.diff), typed(loaded) -> {label,
  //   name} or null (optional: a name to type out before the action is
  //   enabled), send(loaded, typed) -> result, done(result).
  function confirm(spec) {
    const host = byId(IDS.dialogHost);
    if (dialogOpen || !host) return;
    dialogOpen = true;
    let sending = false;
    let loaded = null;
    let loadNo = 0;

    const cancel = el("button", { type: "button", class: "btn" }, "Cancel");
    const action = el("button", { type: "button", class: "btn btn-act", disabled: true, "aria-label": spec.actionLabel ?? spec.action }, spec.actionNodes ?? spec.action);
    const problem = el("p", { class: "warning", role: "alert" });
    const diffBox = el("div", { class: "diff-box" });
    const loadedBox = el("div");
    const typedBox = el("div");
    let typedInput = null;
    let typedName = null;
    // mismatch is whether a name to type is not typed yet.
    const mismatch = () => typedInput !== null && typedInput.value !== typedName;
    const lines = (spec.lines ?? []).map((l) => el("p", null, el("strong", null, l.label + ": "), l.text));
    const dialog = el(
      "dialog",
      { "aria-labelledby": IDS.dialogTitle, class: "confirm" },
      el("h2", { id: IDS.dialogTitle }, spec.title),
      lines,
      loadedBox,
      diffBox,
      typedBox,
      problem,
      el("div", { class: "buttons" }, cancel, action),
    );
    replace(host, dialog);

    function setProblem(msg) {
      replace(problem, msg);
    }

    function showDiff(diff) {
      replace(diffBox, el("pre", { class: "diff", "aria-label": "The change to config.toml" }, diff.map((l) => el("span", { class: "diff-line " + M.diffLineClass(l) }, l))));
    }

    async function load() {
      const n = ++loadNo;
      action.disabled = true;
      loaded = null;
      replace(loadedBox);
      replace(typedBox);
      typedInput = null;
      replace(diffBox, el("p", null, "Loading the change from the daemon..."));
      try {
        const r = await spec.load();
        if (n !== loadNo || !dialogOpen) return;
        loaded = r;
        if (spec.loadedLines) replace(loadedBox, spec.loadedLines(r).map((l) => el("p", null, el("strong", null, l.label + ": "), l.text)));
        if (spec.diffNodes) replace(diffBox, spec.diffNodes(r));
        else showDiff(r.diff);
        const t = spec.typed ? spec.typed(r) : null;
        if (t) {
          typedName = t.name;
          typedInput = el("input", { type: "text", "aria-label": t.label });
          typedInput.addEventListener("input", () => {
            if (!sending && loaded !== null) action.disabled = mismatch();
          });
          replace(typedBox, el("label", null, t.label + " ", typedInput));
        }
        action.disabled = mismatch();
      } catch (e) {
        if (n !== loadNo || !dialogOpen) return;
        replace(diffBox);
        setProblem("Could not load the change: " + String(e?.message ?? e));
      }
    }

    function close() {
      if (dialog.open) dialog.close();
    }

    function cleanup() {
      if (!dialogOpen) return;
      dialogOpen = false;
      replace(host);
      if (spec.opener) spec.opener.focus();
    }

    cancel.addEventListener("click", () => {
      if (!sending) close();
    });
    dialog.addEventListener("cancel", (ev) => {
      if (sending) ev.preventDefault();
    });
    dialog.addEventListener("close", cleanup);
    action.addEventListener("click", async () => {
      if (sending || action.disabled) return;
      sending = true;
      cancel.disabled = true;
      action.disabled = true;
      setProblem("");
      try {
        const res = await spec.send(loaded, typedInput?.value);
        sending = false;
        close();
        cleanup();
        spec.done(res);
      } catch (e) {
        sending = false;
        if (!dialogOpen) return;
        cancel.disabled = false;
        const kind = e?.kind;
        if (kind === "stale_diff" && spec.load) {
          setProblem("The configuration changed while you were looking at it; review the new change.");
          await load();
          return;
        }
        setProblem(String(e?.message ?? e));
        action.disabled = kind === "already_answered" || kind === "not_owner" || kind === "unauthorized" || mismatch();
        if (action.disabled) replace(cancel, "Close");
      }
    });

    dialog.showModal();
    cancel.focus();
    if (spec.load) load();
    else action.disabled = false;
  }

  // ---- requests ----

  function guard(state) {
    state.guarded = true;
    state.apply?.();
    if (state.timer !== undefined) unlater(state.timer);
    state.timer = later(() => {
      state.timer = undefined;
      state.guarded = false;
      state.apply?.();
    }, M.GUARD_MS);
  }

  function sandboxSays(view) {
    const fields = M.askFields(view);
    if (fields.length === 0) return null;
    return el(
      "div",
      { class: "sandbox-says", role: "group", "aria-label": "What the sandbox says about request " + view.id },
      el("p", { class: "sandbox-label" }, el("strong", null, "The sandbox says:"), " written inside the sandbox, not checked by caboose"),
      dl(
        fields.map((f) => {
          if (f.list) return [f.label, el("ul", null, f.list.map((o) => el("li", null, sandboxText(o))))];
          if (f.label === "Session") return [f.label, sandboxText(f.text)];
          return [f.label, sandboxBlock(f.text)];
        }),
      ),
    );
  }

  function heading(env, view, ticks) {
    return el(
      "h3",
      null,
      "#" + view.id + " ",
      envLink(env),
      " ",
      el("span", { class: "kind" }, sandboxText(view.kind)),
      " ",
      el("small", null, timeEl(view.created, "ago", ticks), view.state === "pending" && M.time(view.expires) !== null ? [", expires ", timeEl(view.expires, "left", ticks)] : ""),
    );
  }

  function effectNode(view) {
    return el(
      "div",
      null,
      el("p", { class: "effect" }, isStr(view.effect) && view.effect !== "" ? view.effect : "(the host described no effect)"),
      isStr(view.key) && view.key !== "" ? el("p", null, "Asked for: ", sandboxText(view.key)) : "",
      Number.isSafeInteger(view.attempts) && view.attempts > 1 ? el("p", null, "Asked " + view.attempts + " times."): "",
    );
  }

  function pendingCard(env, view, detail) {
    const sig = JSON.stringify([view, detail?.offers ?? null]);
    return {
      key: M.cardKey(env, view.id),
      sig,
      build(state) {
        const ticks = [];
        const ctrls = {};
        const buttons = [];
        const choices = M.answerControls({ request: view, offers: detail?.offers });
        const btn = (name, text, label, onClick) => {
          const b = el("button", { type: "button", class: "btn", "aria-label": label }, text);
          b.addEventListener("click", () => {
            if (state.guarded || state.done || b.disabled) return;
            onClick(b);
          });
          ctrls[name] = b;
          buttons.push(b);
          return b;
        };
        const open = (choice, opener) => openAnswer(env, view, detail, choice, opener);
        const parts = [];
        for (const c of choices) {
          if (c.id === "hours") {
            const dflt = c.choices.includes(8) ? 8 : c.choices[0];
            const cur = c.choices.includes(state.hours) ? state.hours : dflt;
            const select = el(
              "select",
              { "aria-label": "Hours to allow request " + view.id + " for" },
              c.choices.map((h) => el("option", { value: String(h), selected: h === cur }, M.hoursText(h))),
            );
            select.addEventListener("change", () => {
              state.hours = Number(select.value);
            });
            ctrls.select = select;
            parts.push(
              el(
                "span",
                { class: "answer-group" },
                btn("hours", c.label, "Allow request " + view.id + " for the hours chosen", (b) => open({ ...c, hours: M.clampHours(select.value, c.min, c.max) }, b)),
                " ",
                select,
              ),
            );
          } else {
            parts.push(btn(c.id, c.label, c.label + " request " + view.id, (b) => open(c, b)));
          }
        }
        state.apply = () => {
          for (const b of buttons) b.disabled = state.guarded === true || state.done === true;
        };
        state.apply();
        const node = el(
          "article",
          { class: "card request", "aria-label": "Request " + view.id + " of " + env },
          heading(env, view, ticks),
          effectNode(view),
          sandboxSays(view),
          el("div", { class: "answers", role: "group", "aria-label": "Answers for request " + view.id }, parts.flatMap((p, i) => (i === 0 ? [p] : [" ", p]))),
        );
        return { node, ctrls, ticks };
      },
    };
  }

  function decidedCard(env, view) {
    return {
      key: M.cardKey(env, view.id),
      sig: JSON.stringify(view),
      build() {
        const ticks = [];
        const by = isStr(view.decided_by) && view.decided_by !== "" ? [" by ", sandboxText(view.decided_by)] : "";
        const node = el(
          "article",
          { class: "card decided", "aria-label": "Request " + view.id + " of " + env + ", " + M.stateText(view) },
          heading(env, view, ticks),
          effectNode(view),
          el("p", null, el("strong", null, M.stateText(view)), by, M.time(view.decided_at) !== null ? [", ", timeEl(view.decided_at, "ago", ticks)] : ""),
          isStr(view.reason) && view.reason !== "" ? el("p", null, "Reason: ", view.reason) : "",
          sandboxSays(view),
        );
        return { node, ticks };
      },
    };
  }

  // openAnswer is the confirmation of one answer to a request.
  function openAnswer(env, view, detail, choice, opener) {
    const id = view.id;
    const offer = M.offerOf(detail, choice.scope ?? "");
    const lines = [{ label: "Request " + id + " asks", text: isStr(view.effect) && view.effect !== "" ? view.effect : "(the host described no effect)" }];
    if (choice.verdict === "deny") {
      lines.push({ label: "Denying", text: "tells the sandbox no; nothing changes on this machine." });
    } else if (offer) {
      lines.push({ label: "Allowing", text: M.effectText(offer.effect, choice.hours) });
    }
    const spec = {
      title: M.confirmTitle(choice, view),
      action: M.actionLabel(choice, view),
      actionLabel: M.actionLabel(choice, view),
      actionNodes: ((ps) => [ps[0], sandboxText(ps[1]), ps[2]])(M.actionParts(choice, view)),
      opener,
      lines,
      send: (loaded) => call("answer", M.answerParams(env, id, choice, loaded?.offer ?? null)),
      done: (res) => {
        announce(M.announcement(res, choice, id));
        if (isStr(res?.open_error) && res.open_error !== "") alertMsg("Request " + id + " is allowed, but its URL did not open: " + res.open_error);
        const card = requestsList.cache.get(M.cardKey(env, id));
        if (card) {
          card.state.done = true;
          card.state.apply?.();
        }
      },
    };
    if (choice.scope === "always") {
      spec.load = async () => {
        const r = await call("request", { env, id });
        const fresh = M.offerOf(r, "always");
        if (!fresh || r?.request?.state !== "pending") throw new Error("the host no longer offers this answer for request " + id);
        return { diff: fresh.diff, offer: fresh };
      };
    }
    confirm(spec);
  }

  const requestsRoot = el("div", { class: "page-requests" });
  const requestsIntro = el("p", null);
  const pendingHead = el("h2", null, "Pending");
  const pendingBox = el("div", { class: "cards" });
  const decidedHead = el("h2", null, "Decided");
  const decidedBox = el("div", { class: "cards" });
  const requestsList = keyedList(pendingBox, el("p", null, "No requests are waiting."));
  const decidedList = keyedList(decidedBox, el("p", null, "Nothing decided yet."));
  const requestsNote = el("p", null);
  replace(requestsRoot, requestsNote, pendingHead, pendingBox, decidedHead, decidedBox);
  let prevKeys = null;

  function servedEnvs(ctx) {
    const all = Array.isArray(ctx.status?.envs) ? ctx.status.envs : [];
    return all.filter((e) => M.servedHere(e)).map((e) => e.name).filter((n) => ctx.route.env === null || n === ctx.route.env);
  }

  function updateRequests(ctx) {
    const envs = servedEnvs(ctx);
    const elsewhere = (ctx.status?.envs ?? []).filter((e) => isObj(e) && isStr(e.name) && !M.servedHere(e) && (ctx.route.env === null || e.name === ctx.route.env));
    setText(requestsNote, elsewhere.length > 0 ? "Not listed, served by another daemon: " + elsewhere.map((e) => e.name).join(", ") : ctx.route.env !== null && envs.length === 0 ? "This daemon serves no environment of that name." : "");
    const items = [];
    const decided = [];
    for (const env of envs) {
      for (const view of M.requestList(ctx.data.get("pending:" + env))) {
        if (view.state !== "pending") continue;
        const d = ctx.data.get("req:" + env + ":" + view.id);
        items.push({ env, view, detail: d?.request?.id === view.id ? d : null });
      }
      for (const view of M.requestList(ctx.data.get("all:" + env))) {
        if (view.state !== "pending") decided.push({ env, view });
      }
    }
    const sorted = M.sortPending(items);
    const loaded = envs.length > 0 && envs.every((e) => ctx.data.has("pending:" + e));
    setText(pendingHead, "Pending (" + sorted.length + ")");
    const entries = requestsList.update(sorted.map((i) => pendingCard(i.env, i.view, i.detail)));
    if (loaded) {
      const keys = sorted.map((i) => M.cardKey(i.env, i.view.id));
      for (const k of M.movedCards(prevKeys, keys)) {
        const e = entries.find((x) => x.key === k);
        if (e) guard(e.state);
      }
      prevKeys = keys;
    } else {
      prevKeys = null;
    }
    decidedList.update(M.sortDecided(decided).slice(0, DECIDED_LIMIT).map((i) => decidedCard(i.env, i.view)));
  }

  // ---- overview ----

  const overviewRoot = el("div", { class: "page-overview" });
  const daemonBox = box("div");
  const envsBox = el("div", { class: "cards" });
  const envsList = keyedList(envsBox, el("p", null, "No environments."));
  replace(overviewRoot, el("h2", null, "Daemon"), daemonBox.node, el("h2", null, "Environments"), envsBox);

  // factRows are the rows env_details gives: isolation, image, sessions,
  // resources and forwarded ports. Everything in them came from the
  // daemon or the sandbox, so it is shown as text, isolated.
  function factRows(d) {
    const rows = [];
    if (d === null) return rows;
    if (d.isolation !== "") rows.push(["Isolation", sandboxText(d.isolation)]);
    if (d.image) rows.push(["Image", sandboxText(M.imageText(d.image))]);
    rows.push(["Sessions", d.sessions.length === 0 ? "none" : d.sessions.map((x, i) => [i > 0 ? ", " : "", sandboxText(x)])]);
    const mem = M.memoryText(d.resources);
    if (mem !== "") rows.push(["Memory", mem]);
    const vn = M.vnodesText(d.resources);
    if (vn !== "") rows.push(["Mac files held by the VM", vn]);
    rows.push(["Forwarded ports", d.ports.length === 0 ? "none" : d.ports.map((p, i) => [i > 0 ? ", " : "", el("code", null, M.portText(p))])]);
    return rows;
  }

  function envCard(e, details) {
    const d = M.detailsOf(details);
    const sig = JSON.stringify([e, d]);
    return {
      key: e.name,
      sig,
      build() {
        const ticks = [];
        const name = e.name;
        const ok = envNameRe.test(name);
        if (!M.servedHere(e)) {
          const owner = M.ownerKnown(e.owner) ? M.ownerText(e.owner) : sandboxText(String(e.owner));
          return { node: el("article", { class: "card env", "aria-label": "Environment " + name }, el("h3", null, envLink(name)), dl([["Served by", ["another daemon: ", owner]]])), ticks };
        }
        const rows = [["Sandbox", e.sandbox ?? ""]];
        rows.push(["Link", [e.link ?? "", M.time(e.since) !== null ? [", since ", timeEl(e.since, "ago", ticks)] : ""]]);
        if (isStr(e.last_error) && e.last_error !== "") rows.push(["Last error", sandboxText(e.last_error)]);
        if (isStr(e.trust) && e.trust !== "") rows.push(["Trust level", e.trust]);
        const pending = Number.isSafeInteger(e.pending) ? e.pending : 0;
        rows.push(["Pending requests", ok ? navLink("#/requests/" + name, pending + (pending === 1 ? " request" : " requests")) : String(pending)]);
        const cp = isStr(e.checkpoints_line) && e.checkpoints_line !== "" ? e.checkpoints_line : M.checkpointLine(e.checkpoints);
        if (cp !== "") rows.push(["Checkpoints", sandboxText(cp)]);
        if (Number.isSafeInteger(e.mods_missing) && e.mods_missing > 0) rows.push(["Sessions without the mod", String(e.mods_missing)]);
        rows.push(...factRows(d));
        const node = el(
          "article",
          { class: "card env", "aria-label": "Environment " + name },
          el("h3", null, envLink(name)),
          dl(rows),
          ok ? el("p", null, "Copy to a terminal: ", copyCmd("caboose -e " + name + " restart"), " or ", copyCmd("caboose -e " + name + " apply")) : "",
        );
        return { node, ticks };
      },
    };
  }

  function updateOverview(ctx) {
    const d = ctx.status?.daemon ?? {};
    setBox(daemonBox, JSON.stringify(d), (ticks) => {
      const web = isObj(d.web) ? (d.web.listening ? "listening on " + d.web.addr : "not listening" + (isStr(d.web.error) && d.web.error !== "" ? ": " + d.web.error : "")) : "";
      return dl([
        ["Version", sandboxText(String(d.version ?? ""))],
        ["Kind", d.dev ? "the checkout's daemon" : "the release daemon"],
        ["Protocol", String(d.protocol ?? "")],
        ["Started", M.time(d.started) !== null ? timeEl(d.started, "ago", ticks) : ""],
        ["Host commands running", String(d.host_execs ?? 0)],
        web !== "" ? ["Web page", web] : null,
      ].filter((r) => r !== null));
    });
    envsList.update((ctx.status?.envs ?? []).filter((e) => isObj(e) && isStr(e.name)).map((e) => envCard(e, ctx.data.get("details:" + e.name))));
  }

  // ---- an environment ----

  const envPages = new Map();

  function permissionsNodes(p, env) {
    const rows = [["Trust level", p.trust ?? ""]];
    rows.push(["Isolation", (p.isolation ?? "") + (p.profile ? " (" + p.profile + ")" : "")]);
    const he = isObj(p.host_exec) ? p.host_exec : {};
    rows.push(["Host commands", he.effective ? "allowed" : "off" + (he.why ? " (" + he.why + ")" : "")]);
    const ou = isObj(p.open_urls) ? p.open_urls : {};
    rows.push(["Opening URLs", [String(ou.mode ?? ""), ou.why ? " (" + ou.why + ")" : "", Array.isArray(ou.allow) && ou.allow.length > 0 ? ["; opened without asking: ", ou.allow.join(", ")] : ""]]);
    const eg = isObj(p.egress) ? p.egress : {};
    if (p.isolation === "vm") {
      rows.push(["Outbound connections", [eg.on ? "limited by caboose" : "off", eg.network ? "; network: " + eg.network : "", eg.default ? "; default: " + eg.default : ""]]);
      rows.push(["Allowed hosts", Array.isArray(eg.allow) && eg.allow.length > 0 ? eg.allow.join(", ") : "none"]);
      rows.push(["Denied hosts", Array.isArray(eg.deny) && eg.deny.length > 0 ? eg.deny.join(", ") : "none"]);
    } else {
      rows.push(["Outbound connections", "not limited by caboose under this isolation yet"]);
    }
    if (isObj(p.mods)) rows.push(["Claude Code sessions", p.mods.sessions + " running, " + p.mods.with_mod + " with the mod"]);
    const roots = Array.isArray(p.roots) ? p.roots.filter(isObj) : [];
    const aliases = Array.isArray(p.aliases) ? p.aliases.filter(isObj) : [];
    // What the sandbox was created with and only a restart changes, where
    // it differs from config.toml, which the rest of this section shows.
    const drift = Array.isArray(p.drift) ? p.drift.filter((d) => isStr(d) && d !== "") : [];
    return [
      drift.length === 0
        ? ""
        : el(
            "div",
            { class: "warning" },
            el("p", null, "The running sandbox was created with other settings than these:"),
            el("ul", null, drift.map((d) => el("li", null, d))),
            el("p", null, "These take effect at ", copyCmd("caboose -e " + env + " restart"), ", which ends its sessions."),
          ),
      dl(rows),
      el("h3", null, "Roots"),
      roots.length === 0
        ? el("p", null, "No roots.")
        : dataTable(
            ["Name", "Host path", "In the sandbox", "Access"],
            roots.map((r) => [r.name ?? "", r.host ?? "", r.container ?? "", r.read_only ? "read-only" : Array.isArray(r.writable) && r.writable.length > 0 ? "writable: " + r.writable.join(", ") : "read-only"]),
          ),
      el("h3", null, "Aliases"),
      aliases.length === 0
        ? el("p", null, "No aliases.")
        : dataTable(
            ["Name", "Command", "Directory", "Policy"],
            aliases.map((a) => [[a.name ?? "", a.description ? el("small", null, " " + a.description) : ""], el("code", null, Array.isArray(a.command) ? a.command.join(" ") : ""), a.cwd ?? "", a.policy ?? ""]),
          ),
    ];
  }

  function grantCard(env, g) {
    const sig = JSON.stringify({ ...g, left_seconds: 0 });
    return {
      key: g.id,
      sig,
      build() {
        const ticks = [];
        const b = el("button", { type: "button", class: "btn", "aria-label": "Revoke grant " + g.id }, "Revoke");
        b.addEventListener("click", () => openRevoke(env, g, b));
        const node = el(
          "article",
          { class: "card grant", "aria-label": "Grant " + g.id },
          el("h4", null, "Grant " + g.id + " ", el("small", null, sandboxText(g.kind))),
          el("p", null, "For ", sandboxText(g.key)),
          el("p", null, g.once ? "Used once, " : "", M.time(g.expires) !== null ? timeEl(g.expires, "left", ticks) : M.grantLeft(g, now()) + " s left"),
          b,
        );
        return { node, ctrls: { revoke: b }, ticks };
      },
    };
  }

  function openRevoke(env, g, opener) {
    confirm({
      title: "Revoke grant " + g.id + "?",
      action: "Revoke grant " + g.id,
      opener,
      lines: [
        { label: "Grant", text: [sandboxText(g.kind), " ", sandboxText(g.key)] },
        { label: "Revoking", text: "ends the grant now; the sandbox has to ask again." },
      ],
      send: () => call("revoke", { env, grant: g.id }),
      done: () => announce("Grant " + g.id + " revoked"),
    });
  }

  // ---- egress rules ----

  function egressEntry(env, list, entry) {
    return {
      key: list + ":" + entry,
      sig: entry,
      build() {
        const b = el("button", { type: "button", class: "btn", "aria-label": "Remove " + entry + " from " + (list === "allow" ? "egress_allow" : "egress_deny") }, "Remove");
        b.addEventListener("click", () => openEgress(env, "remove", list, entry, b));
        return { node: el("li", null, el("code", null, entry), " ", b), ctrls: { remove: b } };
      },
    };
  }

  // openEgress is the confirmation of one egress edit: the daemon's diff
  // and effect, fetched as the dialog opens, then the edit with its hash.
  function openEgress(env, op, list, entry, opener, done) {
    const w = M.egressWords(op, list, entry);
    confirm({
      title: w.title,
      action: w.action,
      opener,
      lines: [{ label: "Where", text: "config.toml on this machine, in the vm profile environment " + env + " runs under" }],
      load: async () => {
        const r = M.egressResult(await call("edit_egress", M.egressParams(env, op, list, entry)));
        if (r === null) throw new Error("the daemon's answer was not understood");
        return r;
      },
      loadedLines: (r) => [{ label: "Effect", text: r.effect }],
      send: (loaded) => call("edit_egress", M.egressParams(env, op, list, entry, loaded.hash)),
      done: (res) => {
        announce(M.egressResult(res)?.written ? w.action + ": done" : w.action + ": the daemon did not say it was written");
        done?.();
      },
    });
  }

  function egressSection(env) {
    const allowBox = el("ul", { class: "egress-list", "aria-label": "egress_allow" });
    const denyBox = el("ul", { class: "egress-list", "aria-label": "egress_deny" });
    const allowList = keyedList(allowBox, el("li", null, "No entries."));
    const denyList = keyedList(denyBox, el("li", null, "No entries."));
    const input = el("input", { type: "text", "aria-label": "Egress entry to add" });
    const which = el("select", { "aria-label": "List to add the entry to" }, el("option", { value: "allow", selected: true }, "allow (egress_allow)"), el("option", { value: "deny" }, "deny (egress_deny)"));
    const add = el("button", { type: "button", class: "btn btn-act" }, "Add");
    const hint = el("p", { role: "status" });
    add.addEventListener("click", () => {
      const entry = input.value.trim();
      const h = M.egressHint(entry);
      setText(hint, h);
      if (entry === "" || !call) return;
      const list = which.value === "deny" ? "deny" : "allow";
      openEgress(env, "add", list, entry, add, () => {
        input.value = "";
        setText(hint, "");
      });
    });
    const node = el(
      "section",
      null,
      el("h2", null, "Egress rules"),
      el("p", null, "What the outbound proxy lets through without asking (egress_allow) and never lets through (egress_deny). Each change shows the edit to config.toml first."),
      el("h3", null, "Allowed (egress_allow)"),
      allowBox,
      el("h3", null, "Denied (egress_deny)"),
      denyBox,
      el("h3", null, "Add an entry"),
      el("div", { class: "answers" }, input, " ", which, " ", add),
      hint,
    );
    return {
      node,
      update(lists) {
        allowList.update(lists.allow.map((e) => egressEntry(env, "allow", e)));
        denyList.update(lists.deny.map((e) => egressEntry(env, "deny", e)));
      },
    };
  }

  function checkpointsNodes(r, env, ticks) {
    const paths = Array.isArray(r.paths) ? r.paths.filter(isObj) : [];
    if (paths.length === 0) return [el("p", null, "No checkpoint paths.")];
    const out = [];
    for (const p of paths) {
      const cps = Array.isArray(p.checkpoints) ? p.checkpoints.filter(isObj) : [];
      const newest = cps.reduce((a, c) => (M.time(c.finished ?? c.started) ?? 0) > (M.time(a?.finished ?? a?.started) ?? 0) ? c : a, null);
      out.push(
        el("h3", null, p.path ?? ""),
        el(
          "p",
          null,
          p.resolved ? "Resolved." : "Not resolved" + (p.reason ? ": " + p.reason : "."),
          newest ? [" Newest ", timeEl(newest.finished ?? newest.started, "ago", ticks), newest.method ? ", " + M.methodText(newest.method) : ""] : "",
        ),
        isStr(p.shrunk) && p.shrunk !== "" ? el("p", { class: "warning" }, "Warning: this path shrank: " + p.shrunk) : "",
        cps.length === 0
          ? el("p", null, "No checkpoints yet.")
          : dataTable(
              ["ID", "Taken", "Reason", "Method", "Entries"],
              cps.map((c) => [c.id ?? "", timeEl(c.finished ?? c.started, "ago", ticks), c.reason ?? "", M.methodText(c.method), String(c.entries ?? ""), c.shrunk ? { cell: "Warning: shrank from " + c.shrunk, class: "warning" } : ""]),
            ),
      );
    }
    out.push(el("p", null, "To restore, copy into a terminal: ", copyCmd("caboose -e " + env + " checkpoint restore ID --to DIR")));
    return out;
  }

  function detailsNodes(d) {
    if (d === null) return [el("p", null, "Loading...")];
    const out = [dl(factRows(d))];
    for (const e of d.errors) out.push(el("p", { class: "warning" }, "Could not get ", sandboxText(e.part), ": ", sandboxText(e.message)));
    out.push(el("h3", null, "Kept in the sandbox's home"));
    if (d.keep.length === 0) {
      out.push(el("p", null, "No keep entries."));
    } else {
      out.push(
        dataTable(
          ["Path", "Synced"],
          d.keep.map((k) => [sandboxText(k.path), k.sync === true ? "yes" : "no"]),
        ),
      );
    }
    return out;
  }

  function envPage(env) {
    const root = el("div", { class: "page-env" });
    const perm = box("div");
    const grantsBox = el("div", { class: "cards" });
    const grantsList = keyedList(grantsBox, el("p", null, "No grants in force."));
    const cps = box("div");
    const det = box("div");
    const egBox = box("div");
    const eg = egressSection(env);
    const logPre = el("pre", { class: "log sandbox" });
    const logNote = el("p", null);
    const refresh = el("button", { type: "button", class: "btn" }, "Refresh the log");
    const log = { busy: false, started: false };
    async function loadLog() {
      if (log.busy || !call) return;
      log.busy = true;
      setText(logNote, "Loading the link log...");
      try {
        const r = await call("link_log", { env, lines: LOG_LINES });
        const { lines, truncated } = M.logLines(r);
        setText(logNote, truncated ? "Showing the last " + lines.length + " lines; the log holds more." : lines.length === 0 ? "The log is empty." : "");
        setText(logPre, lines.join("\n"));
      } catch (e) {
        setText(logNote, "The daemon could not give the link log: " + String(e?.message ?? e));
      } finally {
        log.busy = false;
      }
    }
    refresh.addEventListener("click", () => {
      loadLog();
    });
    replace(
      root,
      el("section", null, el("h2", null, "Permissions"), perm.node),
      el("section", null, el("h2", null, "Grants"), grantsBox),
      egBox.node,
      el("section", null, el("h2", null, "Checkpoints"), cps.node),
      el("section", null, el("h2", null, "Details"), det.node),
      el("section", null, el("h2", null, "Link log"), refresh, logNote, logPre),
      el("p", null, navLink("#/env/" + env + "/doctor", "Doctor of " + env)),
    );
    return {
      root,
      update(ctx) {
        if (!log.started) {
          log.started = true;
          loadLog();
        }
        const p = ctx.data.get("perm:" + env);
        setBox(perm, JSON.stringify(p ?? null), () => (isObj(p) ? permissionsNodes(p, env) : [el("p", null, "Loading...")]));
        const lists = M.egressLists(p);
        setBox(egBox, lists === null ? "off" : "vm", () => (lists === null ? [] : [eg.node]));
        if (lists !== null) eg.update(lists);
        const g = ctx.data.get("grants:" + env);
        const list = isObj(g) && Array.isArray(g.grants) ? g.grants.map(M.asGrant).filter((x) => x !== null) : [];
        grantsList.update(list.map((x) => grantCard(env, x)));
        const dd = ctx.data.get("details:" + env);
        setBox(det, JSON.stringify(dd ?? null), () => detailsNodes(M.detailsOf(dd)));
        const c = ctx.data.get("cps:" + env);
        setBox(cps, JSON.stringify(c ?? null), (ticks) => (isObj(c) ? checkpointsNodes(c, env, ticks) : [el("p", null, "Loading...")]));
      },
    };
  }

  // ---- doctor ----

  const doctors = new Map();

  function doctorPage(env) {
    const root = el("div", { class: "page-doctor" });
    const run = el("button", { type: "button", class: "btn btn-act" }, "Run the doctor");
    const note = el("p", { role: "status" });
    const out = box("div");
    const st = { busy: false, result: null };
    function show() {
      setBox(out, JSON.stringify(st.result), (ticks) => {
        if (st.result === null) return [el("p", null, "Not run yet. It runs only when asked: it looks into the sandbox and asks the container engine.")];
        const r = st.result;
        const nodes = [el("p", null, "Ran ", M.time(r.ran_at) !== null ? timeEl(r.ran_at, "ago", ticks) : "just now", ". ", M.doctorSummary(r))];
        if (r.as === "daemon") nodes.push(el("p", null, "This ran in the daemon's environment, which can differ from a terminal's; to check from a terminal, copy ", copyCmd("caboose -e " + env + " doctor"), " there."));
        if (r.truncated === true) nodes.push(el("p", { class: "warning" }, "Some findings were left out of the limit."));
        for (const g of M.groupFindings(r)) {
          nodes.push(
            el(
              "section",
              { class: "doctor-" + g.level },
              el("h2", null, g.title + " (" + g.findings.length + ")"),
              el(
                "ul",
                null,
                g.findings.map((f) =>
                  el("li", null, el("strong", null, sandboxText(f.label)), ": ", sandboxText(f.message), f.fix !== "" ? el("p", null, "To fix, copy: ", el("code", { class: "cmd sandbox" }, f.fix)) : ""),
                ),
              ),
            ),
          );
        }
        return nodes;
      });
    }
    run.addEventListener("click", async () => {
      if (st.busy || !call) return;
      st.busy = true;
      run.disabled = true;
      replace(note, "Running the doctor...");
      try {
        const r = await call("doctor", { env });
        st.result = isObj(r) && Array.isArray(r.findings) ? { findings: r.findings, ran_at: r.ran_at, as: r.as, truncated: r.truncated } : null;
        replace(note, st.result === null ? "The daemon's answer was not understood." : "");
      } catch (e) {
        replace(note, "The doctor could not run: " + String(e?.message ?? e));
      } finally {
        st.busy = false;
        run.disabled = false;
        show();
      }
    });
    replace(root, el("p", null, "Each check's result and the command that fixes it. ", navLink("#/env/" + env, "Back to " + env)), run, note, out.node);
    show();
    return { root, update: show };
  }

  // ---- proposals ----

  const proposalsRoot = el("div", { class: "page-proposals" });
  const proposalsBox = el("div");
  const proposalsEnvs = keyedList(proposalsBox, el("p", null, "No environments."));
  // hidden are the proposals deleted or applied here, env + "\n" + file,
  // left out until the daemon's list no longer has them.
  const hidden = new Set();
  // appliedNotes is, per environment, what the last apply said.
  const appliedNotes = new Map();
  // started is, per environment, the apply job proposal_build started
  // here, until status shows it.
  const started = new Map();
  replace(
    proposalsRoot,
    el("p", null, "What sessions in each sandbox propose to change. Each change is shown as it would be written; the sandbox's own words are shown apart, under \u201cThe sandbox says\u201d."),
    proposalsBox,
  );

  function proposalEnvs(ctx) {
    const all = Array.isArray(ctx.status?.envs) ? ctx.status.envs : [];
    return all.filter((e) => isObj(e) && isStr(e.name) && envNameRe.test(e.name)).map((e) => e.name);
  }

  function partNodes(p) {
    return p.parts.map((pt) =>
      el(
        "div",
        { class: "part" },
        el("p", null, el("strong", null, M.partLabel(pt.kind) + ": "), pt.summary),
        pt.diff.length > 0 ? el("pre", { class: "diff", "aria-label": M.partDiffLabel(pt.kind) }, pt.diff.map((l) => el("span", { class: "diff-line " + M.diffLineClass(l) }, l))) : "",
      ),
    );
  }

  function noticeNodes(p) {
    const out = [];
    if (p.refusals.length > 0) out.push(el("ul", { class: "refusals", "aria-label": "What caboose refuses" }, p.refusals.map((r) => el("li", { class: "warning" }, r))));
    if (p.warnings.length > 0) out.push(el("ul", { class: "warnings", "aria-label": "What to know first" }, p.warnings.map((w) => el("li", null, w))));
    return out;
  }

  function proposalSays(p) {
    return el(
      "div",
      { class: "sandbox-says", role: "group", "aria-label": "What the sandbox says about proposal " + p.file },
      el("p", { class: "sandbox-label" }, el("strong", null, "The sandbox says:"), " written inside the sandbox, not checked by caboose"),
      dl([
        ["Title", sandboxText(p.title)],
        ["Reason", p.reason === "" ? "" : sandboxBlock(p.reason)],
      ]),
    );
  }

  // proposalCard is p's card; while a job applies it (applying), it
  // offers nothing.
  function proposalCard(env, p, applying) {
    return {
      key: p.file,
      sig: JSON.stringify([p, applying]),
      build() {
        const ticks = [];
        const del = el("button", { type: "button", class: "btn", "aria-label": "Delete proposal " + p.file + " of " + env }, "Delete");
        del.addEventListener("click", () => openDeleteProposal(env, p, del));
        const ctrls = { del };
        let buttons = [del];
        let why = "";
        if (applying) {
          buttons = [];
          why = el("p", null, "Being applied now: its image is being built.");
        } else if (p.web_apply) {
          const apply = el("button", { type: "button", class: "btn btn-act", "aria-label": "Apply proposal " + p.file + " of " + env }, p.needs_build ? "Build and apply" : "Apply");
          apply.addEventListener("click", () => openApplyProposal(env, p, apply));
          ctrls.apply = apply;
          buttons.unshift(apply);
        } else if (p.refusals.length > 0) {
          why = el("p", null, "It cannot be applied: delete it, or ask the session for another.");
        } else {
          why = el("p", null, "Not applied from this page: ", p.web_apply_why, ". To apply it, copy ", copyCmd(M.applyCommand(env)), " into a terminal.");
        }
        const node = el(
          "article",
          { class: "card proposal", "aria-label": "Proposal " + p.file + " of " + env },
          el("h3", null, sandboxText(p.file), " ", el("small", null, M.time(p.modified) !== null ? ["proposed ", timeEl(p.modified, "ago", ticks)] : "")),
          partNodes(p),
          noticeNodes(p),
          proposalSays(p),
          why,
          el("div", { class: "answers" }, buttons),
        );
        return { node, ctrls, ticks };
      },
    };
  }

  function envProposals(ctx, env) {
    const r = ctx.data.get("proposals:" + env);
    const loaded = ctx.data.has("proposals:" + env);
    const list = M.proposalList(r);
    const files = new Set(list.map((p) => p.file));
    for (const k of hidden) {
      const [e, f] = k.split("\n");
      if (e === env && !files.has(f)) hidden.delete(k);
    }
    const shown = list.filter((p) => !hidden.has(env + "\n" + p.file));
    const shownJob = M.envJob(ctx.status, env);
    if (shownJob && started.has(env) && M.latestJob(shownJob, started.get(env)) === shownJob) started.delete(env);
    const job = M.latestJob(shownJob, started.get(env) ?? null);
    const applyJob = job?.kind === "apply" ? job : null;
    const applying = applyJob?.state === "running" ? applyJob.proposal : "";
    return {
      key: env,
      sig: JSON.stringify([loaded, shown, r?.more === true, appliedNotes.get(env) ?? null, applyJob]),
      build(state) {
        const box = el("div", { class: "proposal-list" });
        state.list = keyedList(box, el("p", null, loaded ? "No pending proposals." : "Loading..."));
        const notes = appliedNotes.get(env);
        const node = el(
          "section",
          { "aria-label": "Proposals of " + env },
          el("h2", null, "Environment ", envLink(env)),
          applyJob ? jobBox(applyJob) : "",
          notes ? el("div", { role: "status", class: "applied" }, notes.map((n) => el("p", null, n))) : "",
          r?.more === true ? el("p", { class: "warning" }, "There are more than the page lists: ", copyCmd(M.applyCommand(env)), " shows all of them.") : "",
          box,
        );
        state.list.update(shown.map((p) => proposalCard(env, p, p.file === applying)));
        return { node };
      },
    };
  }

  // jobBox is an apply job as status says it: running with the last lines
  // of what it says, done with its notes, or failed with its error. The
  // lines are the build's, which says what the sandbox proposed: shown
  // apart, as the link's log is.
  function jobBox(j) {
    const out = j.lines.length > 0 && j.state !== "done" ? el("pre", { class: "log sandbox", "aria-label": "What applying " + j.proposal + " says" }, j.lines.join("\n")) : "";
    return el(
      "div",
      { role: "status", class: "job job-" + j.state, "aria-label": "Applying proposal " + j.proposal },
      el("p", j.state === "failed" ? { class: "warning" } : null, M.applyJobText(j)),
      j.notes.map((n) => el("p", null, n)),
      out,
    );
  }

  function updateProposals(ctx) {
    proposalsEnvs.update(proposalEnvs(ctx).map((env) => envProposals(ctx, env)));
  }

  function openDeleteProposal(env, p, opener) {
    confirm({
      title: "Delete proposal " + p.file + "?",
      action: "Delete " + p.file,
      opener,
      lines: [
        { label: "Proposal", text: [sandboxText(p.file), " of environment ", env] },
        { label: "Deleting", text: "removes it from the sandbox's proposals; nothing in it is applied." },
      ],
      send: () => call("proposal_delete", M.proposalDeleteParams(env, p.file, p.hash)),
      done: () => {
        hidden.add(env + "\n" + p.file);
        announce("Deleted proposal " + p.file);
      },
    });
  }

  // openApplyProposal is the confirmation of applying a proposal: the
  // proposal fetched again as the dialog opens, its whole diff shown, and
  // its name typed when it takes one; then applied with that hash.
  // One that builds the image first (needs_build) is sent to
  // proposal_build, which starts a job: the page follows it in status.
  function openApplyProposal(env, p, opener) {
    const build = p.needs_build;
    confirm({
      title: (build ? "Build and apply proposal " : "Apply proposal ") + p.file + " to " + env + "?",
      action: (build ? "Build and apply " : "Apply ") + p.file,
      opener,
      lines: build
        ? [
            { label: "Where", text: "this machine: environment " + env + "'s image is built with the packages first, which can take minutes, and then its config.toml is written" },
            { label: "If it does not build", text: "nothing is written, and the proposal is left pending." },
            { label: "The sandbox", text: "keeps the image it runs until its next restart." },
          ]
        : [{ label: "Where", text: "this machine's files of environment " + env + ": its config.toml, or its Dockerfile" }],
      load: async () => {
        const fresh = M.proposalList(await call("proposals", { env })).find((x) => x.file === p.file);
        if (!fresh) throw new Error("it is no longer pending");
        if (!fresh.web_apply) throw new Error("it can no longer be applied from this page: " + fresh.web_apply_why);
        if (fresh.needs_build !== build) throw new Error("it changed: close this, and look at it again");
        return { diff: [], hash: fresh.hash, p: fresh };
      },
      loadedLines: (r) => [
        { label: "Proposal", text: sandboxText(r.p.title) },
        ...r.p.warnings.map((w) => ({ label: "To know first", text: w })),
      ],
      diffNodes: (r) => partNodes(r.p),
      typed: (r) => (r.p.confirm.kind === "type_name" ? { label: "To confirm, type the name " + r.p.confirm.name, name: r.p.confirm.name } : null),
      send: (loaded, typed) =>
        build ? call("proposal_build", M.proposalBuildParams(env, p.file, loaded.hash, typed)) : call("proposal_apply", M.proposalApplyParams(env, p.file, loaded.hash, typed)),
      done: (res) => {
        if (build) {
          const j = M.asJob(res?.job);
          if (j) started.set(env, j);
          appliedNotes.delete(env);
          announce("Building the image for proposal " + p.file);
          return;
        }
        const notes = M.applyNotes(res);
        hidden.add(env + "\n" + p.file);
        appliedNotes.set(env, ["Applied " + p.file + ".", ...(notes ?? ["The daemon did not say it was applied."])]);
        announce("Applied proposal " + p.file);
      },
    });
  }

  function staticPage(key, ...children) {
    if (!messages.has(key)) messages.set(key, el("div", null, ...children));
    return messages.get(key);
  }

  // ---- the hook ----

  function render(ctx) {
    call = ctx.call;
    const route = ctx.route;
    const title = titleOf(route);
    if (route === null) return { title, body: [staticPage("none", el("p", null, "There is no such page."))] };
    switch (route.page) {
      case "overview":
        updateOverview(ctx);
        return { title, body: [overviewRoot] };
      case "requests":
        updateRequests(ctx);
        return { title, body: [requestsRoot] };
      case "env": {
        const e = (ctx.status?.envs ?? []).find((x) => isObj(x) && x.name === route.env);
        if (!e) return { title, body: [staticPage("noenv", el("p", null, "There is no such environment."))] };
        if (!M.servedHere(e)) return { title, body: [staticPage("other:" + route.env, el("p", null, "Another daemon serves this environment: " + M.ownerText(e.owner) + "."))] };
        if (!envPages.has(route.env)) envPages.set(route.env, envPage(route.env));
        const pg = envPages.get(route.env);
        pg.update(ctx);
        return { title, body: [pg.root] };
      }
      case "proposals":
        updateProposals(ctx);
        return { title, body: [proposalsRoot] };
      case "doctor":
      {
        const e = (ctx.status?.envs ?? []).find((x) => isObj(x) && x.name === route.env);
        if (!e) return { title, body: [staticPage("noenv", el("p", null, "There is no such environment."))] };
        if (!M.servedHere(e)) return { title, body: [staticPage("other:" + route.env, el("p", null, "Another daemon serves this environment: " + M.ownerText(e.owner) + "."))] };
        if (!doctors.has(route.env)) doctors.set(route.env, doctorPage(route.env));
        const pg = doctors.get(route.env);
        pg.update();
        return { title, body: [pg.root] };
      }
    }
    return { title, body: [staticPage("none", el("p", null, "There is no such page."))] };
  }

  function calls(ctx) {
    const route = ctx.route;
    if (!route || !ctx.status) return [];
    const out = [];
    if (route.page === "overview") {
      for (const env of servedEnvs(ctx)) out.push({ key: "details:" + env, method: "env_details", params: { env }, every: SLOW_MS });
    } else if (route.page === "requests") {
      for (const env of servedEnvs(ctx)) {
        out.push({ key: "pending:" + env, method: "requests", params: { env }, every: FAST_MS });
        out.push({ key: "all:" + env, method: "requests", params: { env, all: true, limit: DECIDED_LIMIT }, every: SLOW_MS });
        const pend = M.requestList(ctx.data.get("pending:" + env)).filter((v) => v.state === "pending");
        for (const i of M.sortPending(pend.map((view) => ({ env, view }))).slice(0, MAX_DETAILS)) {
          out.push({ key: "req:" + env + ":" + i.view.id, method: "request", params: { env, id: i.view.id }, every: SLOW_MS });
        }
      }
    } else if (route.page === "proposals") {
      for (const env of proposalEnvs(ctx)) out.push({ key: "proposals:" + env, method: "proposals", params: { env }, every: SLOW_MS });
    } else if (route.page === "env" && servedEnvs(ctx).includes(route.env)) {
      const env = route.env;
      out.push({ key: "perm:" + env, method: "permissions", params: { env }, every: SLOW_MS });
      out.push({ key: "grants:" + env, method: "grants", params: { env }, every: SLOW_MS });
      out.push({ key: "cps:" + env, method: "checkpoints", params: { env }, every: SLOW_MS });
      out.push({ key: "details:" + env, method: "env_details", params: { env }, every: SLOW_MS });
    }
    return out;
  }

  return { render, calls };
}
