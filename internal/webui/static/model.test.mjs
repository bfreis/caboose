import { test } from "node:test";
import assert from "node:assert/strict";

import * as M from "./model.mjs";

const H = "a".repeat(64);
const NOW = Date.parse("2026-10-18T12:00:00Z");
const iso = (ms) => new Date(ms).toISOString();

function result(offers, state = "pending") {
  return { request: { id: 7, kind: "url", state }, offers };
}

const ONCE = { scope: "once", effect: "once" };
const HOURS = { scope: "hours", effect: "for {hours} hours", hours_min: 1, hours_max: 24 };
const ALWAYS = { scope: "always", effect: "forever", diff: ["+x"], hash: H };

test("answer controls come from the offers: Deny always, the rest as offered", () => {
  assert.deepEqual(M.answerControls(result([ONCE, HOURS, ALWAYS])).map((c) => c.label), ["Deny", "Allow once", "Allow for", "Allow always"]);
  assert.deepEqual(M.answerControls(result([ONCE])).map((c) => c.id), ["deny", "once"]);
  assert.deepEqual(M.answerControls(result([])).map((c) => c.id), ["deny"]);
  assert.deepEqual(M.answerControls(result(undefined)).map((c) => c.id), ["deny"]);
  // Not pending: nothing to answer.
  assert.deepEqual(M.answerControls(result([ONCE], "allowed")), []);
  assert.deepEqual(M.answerControls(null), []);
});

test("an offer of the wrong shape is left out, never trusted", () => {
  const bad = [
    { scope: "forever", effect: "x" },
    { scope: "once" },
    { scope: "once", effect: "" },
    { scope: "hours", effect: "x", hours_min: 5, hours_max: 1 },
    { scope: "hours", effect: "x", hours_min: 1, hours_max: 9999 },
    { scope: "always", effect: "x", diff: [], hash: "nothex" },
    null,
    "once",
  ];
  assert.deepEqual(M.answerControls(result(bad)).map((c) => c.id), ["deny"]);
  // The same scope twice: the first.
  assert.equal(M.offersOf(result([ONCE, { ...ONCE, effect: "second" }])).length, 1);
  // An always offer's diff keeps only strings.
  assert.deepEqual(M.offerOf(result([{ ...ALWAYS, diff: ["+a", 5, null, "-b"] }]), "always").diff, ["+a", "-b"]);
});

test("hours are clamped to the bounds", () => {
  assert.deepEqual(M.hoursChoices(1, 168), [1, 4, 8, 24, 72, 168]);
  assert.deepEqual(M.hoursChoices(1, 24), [1, 4, 8, 24]);
  assert.deepEqual(M.hoursChoices(5, 30), [8, 24]);
  assert.deepEqual(M.hoursChoices(25, 71), [25, 71], "none of the list inside: the bounds");
  assert.deepEqual(M.hoursChoices(10, 10), [10]);
  assert.deepEqual(M.hoursChoices(0, 10), []);
  assert.deepEqual(M.hoursChoices(1, 169), []);
  assert.deepEqual(M.hoursChoices(5, 2), []);
  assert.equal(M.clampHours(500, 1, 24), 24);
  assert.equal(M.clampHours(0, 4, 24), 4);
  assert.equal(M.clampHours("8", 1, 24), 8);
  assert.equal(M.clampHours("junk", 3, 24), 3);
  assert.equal(M.clampHours(999, 1, 9999), 168);
  const c = M.answerControls(result([HOURS])).find((x) => x.id === "hours");
  assert.deepEqual(c.choices, [1, 4, 8, 24]);
});

test("diff lines are classed by their first character", () => {
  assert.equal(M.diffLineClass("+ added"), "diff-add");
  assert.equal(M.diffLineClass("- removed"), "diff-del");
  assert.equal(M.diffLineClass("  context"), "diff-ctx");
  assert.equal(M.diffLineClass("@@ hunk"), "diff-other");
  assert.equal(M.diffLineClass(""), "diff-other");
  assert.equal(M.diffLineClass(5), "diff-other");
});

test("relative times and countdowns", () => {
  assert.equal(M.relativeTime(iso(NOW - 2000), NOW), "just now");
  assert.equal(M.relativeTime(iso(NOW - 30000), NOW), "30 s ago");
  assert.equal(M.relativeTime(iso(NOW - 5 * 60000), NOW), "5 min ago");
  assert.equal(M.relativeTime(iso(NOW - 3 * 3600000), NOW), "3 h ago");
  assert.equal(M.relativeTime(iso(NOW - 3 * 86400000), NOW), "3 d ago");
  assert.equal(M.relativeTime(iso(NOW + 90000), NOW), "in 1 min");
  assert.equal(M.relativeTime("0001-01-01T00:00:00Z", NOW), "", "the zero time is no time");
  assert.equal(M.relativeTime("junk", NOW), "");
  assert.equal(M.countdown(iso(NOW + 65 * 60000), NOW), "1 h 5 min left");
  assert.equal(M.countdown(iso(NOW + 20000), NOW), "20 s left");
  assert.equal(M.countdown(iso(NOW - 1), NOW), "expired");
  assert.equal(M.countdown(undefined, NOW), "");
});

test("pending requests sort newest first, across environments", () => {
  const v = (id, at) => ({ id, created: iso(NOW - at) });
  const items = [
    { env: "b", view: v(3, 5000) },
    { env: "a", view: v(9, 60000) },
    { env: "a", view: v(4, 5000) },
    { env: "c", view: v(1, 1000) },
  ];
  assert.deepEqual(M.sortPending(items).map((i) => i.env + i.view.id), ["c1", "a4", "b3", "a9"]);
  assert.deepEqual(items.map((i) => i.view.id), [3, 9, 4, 1], "the input is not reordered");
});

test("which cards moved", () => {
  assert.equal(M.movedCards(null, ["a/1", "a/2"]).size, 0, "the first time nothing moved");
  assert.deepEqual([...M.movedCards(["a/2", "a/1"], ["a/2", "a/1"])], []);
  assert.deepEqual([...M.movedCards(["a/2", "a/1"], ["a/3", "a/2", "a/1"])], ["a/3", "a/2", "a/1"]);
  assert.deepEqual([...M.movedCards(["a/3", "a/2"], ["a/3", "a/2", "a/1"])], ["a/1"]);
  assert.deepEqual([...M.movedCards(["a/3", "a/2", "a/1"], ["a/3", "a/1"])], ["a/1"]);
});

test("answer params: a deny carries no scope, hours are clamped, always carries the fresh hash", () => {
  assert.deepEqual(M.answerParams("dev", 7, { verdict: "deny" }, null), { env: "dev", id: 7, verdict: "deny" });
  assert.deepEqual(M.answerParams("dev", 7, { verdict: "deny", scope: "once", hours: 4 }, null), { env: "dev", id: 7, verdict: "deny" });
  assert.deepEqual(M.answerParams("dev", 7, { verdict: "allow", scope: "once" }, null), { env: "dev", id: 7, verdict: "allow", scope: "once" });
  assert.deepEqual(M.answerParams("dev", 7, { verdict: "allow", scope: "hours", hours: 500, min: 1, max: 24 }, null), { env: "dev", id: 7, verdict: "allow", scope: "hours", hours: 24 });
  assert.deepEqual(M.answerParams("dev", 7, { verdict: "allow", scope: "always" }, ALWAYS), { env: "dev", id: 7, verdict: "allow", scope: "always", confirm_diff: H });
  assert.throws(() => M.answerParams("dev", 7, { verdict: "allow", scope: "always" }, null));
  assert.throws(() => M.answerParams("dev", 7, { verdict: "allow", scope: "always" }, { scope: "always", hash: "x" }));
  assert.throws(() => M.answerParams("dev", 7, { verdict: "allow", scope: "forever" }, null));
});

test("titles, button words and announcements name the action", () => {
  const view = { id: 12, key: "api.example.com" };
  const h8 = { verdict: "allow", scope: "hours", hours: 8 };
  assert.equal(M.confirmTitle(h8, view), "Allow request 12 for 8 hours?");
  assert.equal(M.confirmTitle({ verdict: "deny" }, view), "Deny request 12?");
  assert.equal(M.actionLabel(h8, view), "Allow api.example.com for 8 hours");
  assert.equal(M.actionLabel({ verdict: "deny" }, view), "Deny api.example.com");
  assert.equal(M.actionLabel({ verdict: "allow", scope: "once" }, { id: 3 }), "Allow request 3 once");
  assert.equal(M.announcement({ request: { id: 12 } }, h8, 12), "Request 12 allowed for 8 hours");
  assert.equal(M.announcement(null, { verdict: "deny" }, 12), "Request 12 denied");
  assert.equal(M.announcement({}, { verdict: "allow", scope: "once" }, 5), "Request 5 allowed once");
  assert.equal(M.hoursText(1), "1 hour");
  assert.equal(M.effectText("opens for {hours} hours", 8), "opens for 8 hours");
});

test("what the sandbox said is listed in order, absent fields left out", () => {
  assert.deepEqual(M.askFields({ id: 1 }), []);
  const f = M.askFields({ session: "s1", ask: { context: "c", options: ["a", "", 3, "b"], meanwhile: "m" } });
  assert.deepEqual(f.map((x) => x.label), ["Context", "Options", "Done meanwhile", "Session"]);
  assert.deepEqual(f[1].list, ["a", "b"]);
});

test("an environment another daemon owns is not served here", () => {
  assert.equal(M.servedHere({ name: "a" }), true);
  assert.equal(M.servedHere({ name: "a", owner: "" }), true);
  assert.equal(M.servedHere({ name: "a", owner: "the release daemon" }), false);
  assert.equal(M.servedHere(null), false);
});

test("grants count down from their expiry", () => {
  assert.equal(M.grantLeft({ expires: iso(NOW + 90000) }, NOW), 90);
  assert.equal(M.grantLeft({ expires: iso(NOW - 5) }, NOW), 0);
  assert.equal(M.grantLeft({ left_seconds: 40 }, NOW), 40);
  assert.equal(M.asGrant({ id: "g12", kind: "url", key: "k" })?.id, "g12");
  assert.equal(M.asGrant({ id: "x12", kind: "url", key: "k" }), null);
});

test("requests of the wrong shape are dropped", () => {
  const r = M.requestList({ requests: [{ id: 1, kind: "a", state: "pending" }, { id: 0, kind: "a", state: "p" }, { kind: "a" }, null, "x"] });
  assert.deepEqual(r.map((x) => x.id), [1]);
  assert.deepEqual(M.requestList(null), []);
  assert.deepEqual(M.requestList({ requests: "no" }), []);
});

test("formatBytes", () => {
  assert.equal(M.formatBytes(0), "0 B");
  assert.equal(M.formatBytes(1536), "1.5 KiB");
  assert.equal(M.formatBytes(1610612736), "1.5 GiB");
  assert.equal(M.formatBytes(-1), "");
  assert.equal(M.formatBytes("x"), "");
});

test("ownerText, imageText, portText and resources", () => {
  assert.match(M.ownerText("release"), /7171/);
  assert.match(M.ownerText("dev"), /7172/);
  assert.equal(M.imageText({ state: "switched", why: "other profile" }).includes("other profile"), true);
  assert.equal(M.portText({ port: 3000, url: "javascript:x" }), "http://localhost:3000");
  assert.equal(M.portText({ port: 0 }), "");
  assert.equal(M.memoryText({}), "");
  assert.equal(M.vnodesText({ vnodes: 5 }), "5");
});

test("groupFindings puts problems first and drops what is not a finding", () => {
  const g = M.groupFindings({ findings: [{ level: "ok", label: "a", message: "m" }, { level: "problem", label: "b", message: "m", fix: "f" }, { level: "bogus", label: "c", message: "m" }, null, { level: "note", label: 1, message: "m" }] });
  assert.deepEqual(g.map((x) => x.level), ["problem", "ok"]);
  assert.equal(g[0].findings[0].fix, "f");
  assert.deepEqual(M.groupFindings(null), []);
  assert.equal(M.doctorSummary({ findings: [] }), "No findings.");
});

test("detailsOf drops malformed entries", () => {
  const d = M.detailsOf({ env: "a", sessions: ["x", 1], ports: [{ port: 99999 }, { port: 80 }], keep: [{ path: 1 }, { path: "~/a", sync: true }], errors: [{ part: "p" }] });
  assert.deepEqual([d.sessions.length, d.ports.length, d.keep.length, d.errors.length], [1, 1, 1, 0]);
  assert.equal(M.detailsOf(null), null);
});

test("egress lists come from a vm environment's permissions, strings only", () => {
  assert.equal(M.egressLists({ isolation: "container", egress: { allow: ["a"] } }), null);
  assert.equal(M.egressLists(null), null);
  assert.deepEqual(M.egressLists({ isolation: "vm", egress: { allow: ["a.example", 3, ""], deny: null } }), { allow: ["a.example"], deny: [] });
});

test("egress hints are hints: names, patterns, CIDRs and addresses pass", () => {
  for (const ok of ["git.corp.example", "*.corp.example", "10.0.0.0/8", "10.1.2.3", "fd00::/8"]) assert.equal(M.egressHint(ok), "", ok);
  for (const bad of ["", "a b", "a,b", "x".repeat(261), "<b>"]) assert.notEqual(M.egressHint(bad), "", bad);
});

test("egress params name one entry, and the hash only when confirming", () => {
  assert.deepEqual(M.egressParams("e", "add", "allow", "a.example"), { env: "e", add_allow: ["a.example"] });
  assert.deepEqual(M.egressParams("e", "remove", "deny", "b.example", H), { env: "e", remove_deny: ["b.example"], confirm_diff: H });
  assert.throws(() => M.egressParams("e", "add", "other", "a"));
  assert.throws(() => M.egressParams("e", "drop", "allow", "a"));
  assert.throws(() => M.egressParams("e", "add", "allow", "a", "nothex"));
  assert.deepEqual(M.egressWords("add", "deny", "x.example"), { title: "Add x.example to egress_deny?", action: "Add x.example to egress_deny" });
});

test("an egress result is used only in its shape", () => {
  assert.deepEqual(M.egressResult({ diff: ["+a"], hash: H, effect: "e", written: true }), { diff: ["+a"], hash: H, effect: "e", written: true });
  for (const bad of [null, { diff: [], hash: H, effect: "e" }, { diff: ["+a"], hash: "x", effect: "e" }, { diff: [1], hash: H, effect: "e" }, { diff: ["+a"], hash: H }]) assert.equal(M.egressResult(bad), null);
});

test("a proposal of the wrong shape is left out; one the page cannot apply says so", () => {
  const good = { file: "a.toml", title: "t", reason: "r", parts: [{ kind: "root", summary: "s", diff: ["+a"] }], refusals: [], warnings: [], web_apply: true, confirm: { kind: "type_name", name: "a" }, hash: H };
  assert.equal(M.proposalList({ proposals: [good] }).length, 1);
  assert.deepEqual(M.proposalList({ proposals: [{ ...good, file: "../x.toml" }, { ...good, file: "x" }, { ...good, file: ".x.toml" }, null, "a"] }), []);
  assert.deepEqual(M.proposalList(null), []);
  const parts = M.asProposal({ ...good, parts: [{ kind: "exec", summary: "s" }, { kind: "alias", summary: 1 }, good.parts[0]] }).parts;
  assert.deepEqual(parts.map((p) => p.kind), ["root"]);
  for (const bad of [{ hash: "x" }, { refusals: ["no"] }, { parts: [] }, { parts: [{ kind: "packages", summary: "s", diff: ["+ x"] }] }, { parts: [good.parts[0], { kind: "section", summary: "s", diff: ["+ x"] }] }]) {
    const p = M.asProposal({ ...good, ...bad });
    assert.equal(p.web_apply, false, JSON.stringify(bad));
    assert.notEqual(p.web_apply_why, "");
  }
  assert.deepEqual(M.asProposal({ ...good, confirm: { kind: "type_name", name: "" } }).confirm, { kind: "none", name: "" });
});

test("a writable part is a part the page shows, applies and labels", () => {
  const w = { file: "w.toml", title: "t", reason: "r", parts: [{ kind: "writable", summary: "make /work/dev/x writable", diff: ['+dev = { host = "~/dev", writable = ["x"] }'] }], refusals: [], warnings: [], web_apply: true, confirm: { kind: "type_name", name: "dev" }, hash: H };
  const p = M.asProposal(w);
  assert.deepEqual(p.parts.map((x) => x.kind), ["writable"]);
  assert.equal(p.web_apply, true);
  assert.deepEqual(p.confirm, { kind: "type_name", name: "dev" });
  assert.match(M.partLabel("writable"), /may write/);
  assert.equal(M.partDiffLabel("writable"), "The change to config.toml");
});

test("proposal_apply's params carry a hash, and the typed name only when there is one", () => {
  assert.deepEqual(M.proposalApplyParams("dev", "a.toml", H, "a"), { env: "dev", file: "a.toml", hash: H, typed: "a" });
  assert.deepEqual(M.proposalApplyParams("dev", "a.toml", H, undefined), { env: "dev", file: "a.toml", hash: H });
  assert.throws(() => M.proposalApplyParams("dev", "a.toml", "nothex", "a"));
  assert.deepEqual(M.proposalDeleteParams("dev", "a.toml", H), { env: "dev", file: "a.toml", hash: H });
  assert.throws(() => M.proposalDeleteParams("dev", "a.toml", ""));
  assert.equal(M.applyCommand("default"), "caboose apply");
  assert.equal(M.applyCommand("dev"), "caboose -e dev apply");
  assert.deepEqual(M.applyNotes({ applied: true, notes: ["a", 1] }), ["a"]);
  assert.equal(M.applyNotes({ applied: false }), null);
});

test("packages are applied from the page as a build, alone or with a root, and only with needs_build", () => {
  const pk = { kind: "packages", summary: "the packages of [apko.default]", diff: ["+ graphviz"] };
  const root = { kind: "root", summary: "mount it", diff: ["+a"] };
  const good = { file: "p.toml", title: "t", reason: "r", parts: [pk], refusals: [], warnings: [], web_apply: true, confirm: { kind: "none" }, needs_build: true, hash: H };
  assert.equal(M.asProposal(good).web_apply, true);
  assert.equal(M.asProposal(good).needs_build, true);
  assert.equal(M.asProposal({ ...good, parts: [pk, root], confirm: { kind: "type_name", name: "a" } }).web_apply, true);
  assert.equal(M.asProposal({ ...good, parts: [root, pk] }).web_apply, true);
  for (const bad of [{ needs_build: false }, { parts: [root] }, { parts: [pk, { kind: "section", summary: "s", diff: [] }] }, { parts: [pk, root, root] }]) {
    const p = M.asProposal({ ...good, ...bad });
    assert.equal(p.web_apply, false, JSON.stringify(bad));
    assert.notEqual(p.web_apply_why, "");
  }
  assert.deepEqual(M.proposalBuildParams("dev", "p.toml", H, ""), { env: "dev", file: "p.toml", hash: H });
});

test("a job is used only in its shape, its strings bounded and its lists strings only", () => {
  const run = { kind: "apply", state: "running", since: "2026-10-18T12:00:00Z", proposal: "p.toml", lines: ["a", 1, "b"] };
  assert.deepEqual(M.asJob(run), { kind: "apply", state: "running", since: run.since, finished: "", error: "", proposal: "p.toml", lines: ["a", "b"], notes: [] });
  const many = M.asJob({ ...run, lines: Array.from({ length: 50 }, (_, i) => "line " + i + " " + "x".repeat(300)) });
  assert.equal(many.lines.length, 20);
  assert.ok(many.lines[19].startsWith("line 49"), "the last lines");
  assert.ok(many.lines.every((l) => l.length <= 200));
  assert.deepEqual(M.asJob({ ...run, state: "done", notes: ["n", {}, "restart"] }).notes, ["n", "restart"]);
  assert.deepEqual(M.asJob({ ...run, notes: ["early"] }).notes, [], "notes only once done");
  assert.equal(M.asJob({ kind: "restart", state: "running", since: run.since }).proposal, "");
  for (const bad of [null, [], { ...run, kind: "exec" }, { ...run, state: "queued" }, { ...run, since: 1 }, { ...run, proposal: "../x.toml" }, { ...run, proposal: undefined }]) {
    assert.equal(M.asJob(bad), null, JSON.stringify(bad));
  }
  const status = { envs: [{ name: "dev", job: run }, { name: "x", job: { kind: "nope" } }] };
  assert.equal(M.envJob(status, "dev").proposal, "p.toml");
  assert.equal(M.envJob(status, "x"), null);
  assert.equal(M.envJob(null, "dev"), null);
  const older = M.asJob({ ...run, since: "2026-10-18T11:00:00Z", state: "done" });
  const newer = M.asJob(run);
  assert.equal(M.latestJob(older, newer), newer, "status lags what the page started");
  assert.equal(M.latestJob(newer, older), newer);
  assert.equal(M.latestJob(null, newer), newer);
  assert.equal(M.latestJob(older, null), older);
  assert.match(M.applyJobText(newer), /building the image/);
  assert.match(M.applyJobText({ ...newer, state: "failed", error: "the build failed" }), /failed: the build failed/);
});
