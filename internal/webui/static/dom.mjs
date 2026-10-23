// The only module that makes the page's nodes. Everything shown goes
// through el(): a string child becomes a text node, never markup, and an
// attribute is set only when its name is on a short list and its value
// has the form that name takes. What a sandbox wrote (a request's
// context, a session id, an error quoting one) can therefore never become
// an element, a handler, a link or a node's id.

// IDS is every id (and name, and for) a node may carry: the page's own
// constants, so no text from a sandbox ever names a node, which would
// shadow a property of document or window. A node per item takes an id
// of a prefix in ID_PREFIXES and a number the host assigned: itemId().
export const IDS = Object.freeze({
  header: "page-header",
  daemon: "daemon-state",
  nav: "page-nav",
  main: "main",
  title: "page-title",
  status: "live-status",
  alert: "live-alert",
  note: "page-note",
  dialogHost: "page-dialog",
  dialogTitle: "dialog-title",
});

// ID_PREFIXES are the prefixes of a per-item id ("req-12").
export const ID_PREFIXES = Object.freeze(["req", "grant", "prop", "check"]);

const idValues = new Set(Object.values(IDS));
const itemIdRe = /^([a-z]+)-([0-9]{1,10})$/;

// itemId is the id of an item the host numbered: prefix-n.
export function itemId(prefix, n) {
  if (!ID_PREFIXES.includes(prefix) || !Number.isSafeInteger(n) || n < 0) {
    throw new Error("dom: no item id for that prefix and number");
  }
  const id = prefix + "-" + String(n);
  if (!validId(id)) {
    throw new Error("dom: no item id for that prefix and number");
  }
  return id;
}

function validId(v) {
  if (idValues.has(v)) {
    return true;
  }
  const m = itemIdRe.exec(v);
  return m !== null && ID_PREFIXES.includes(m[1]);
}

// The elements el() makes: none that loads, runs or navigates anything
// (no a, script, iframe, img, form, link, style, svg, template).
const TAGS = new Set([
  "article", "bdi", "button", "caption", "code", "dd", "details", "dialog", "div", "dl", "dt",
  "em", "footer", "h1", "h2", "h3", "h4", "header", "label", "li", "main", "nav", "ol", "option",
  "p", "pre", "section", "select", "small", "span", "strong", "summary", "table", "tbody",
  "td", "textarea", "th", "thead", "time", "tr", "ul", "input",
]);

const INPUT_TYPES = new Set(["button", "submit", "checkbox", "radio", "number", "text", "search"]);
const TH_SCOPES = new Set(["col", "row"]);
const ROLES = new Set(["status", "alert", "group", "region", "list", "listitem", "note", "alertdialog", "dialog", "table", "rowgroup", "row", "columnheader", "rowheader", "cell"]);
const classRe = /^[a-z][a-z0-9-]*( [a-z][a-z0-9-]*)*$/;
const intRe = /^-?[0-9]{1,10}$/;
const ariaRe = /^aria-[a-z]{2,20}$/;
// aria-* that name other nodes: they take ids, like id itself.
const ARIA_IDREFS = new Set(["aria-labelledby", "aria-describedby", "aria-controls", "aria-owns", "aria-activedescendant", "aria-details", "aria-errormessage", "aria-flowto"]);

// check is each attribute's rule: the value it may take, as a string, or
// an exception. Free text (title, value, aria-label) is an attribute
// value set with setAttribute, which is never parsed.
const ATTRS = {
  class: (v) => matching(v, classRe),
  id: (v) => anId(v),
  name: (v) => anId(v),
  for: (v) => anId(v),
  type: (v) => oneOf(v, INPUT_TYPES),
  scope: (v) => oneOf(v, TH_SCOPES),
  role: (v) => oneOf(v, ROLES),
  value: (v) => text(v),
  title: (v) => text(v),
  datetime: (v) => text(v),
  "data-label": (v) => text(v),
  tabindex: (v) => matching(String(v), intRe),
  colspan: (v) => matching(String(v), intRe),
  min: (v) => matching(String(v), intRe),
  max: (v) => matching(String(v), intRe),
  disabled: flag,
  selected: flag,
  hidden: flag,
};

function refuse(what) {
  throw new Error("dom: refused " + what);
}

function text(v) {
  if (typeof v === "string") {
    return v;
  }
  if (typeof v === "number" && Number.isFinite(v)) {
    return String(v);
  }
  return refuse("a value that is not text");
}

function matching(v, re) {
  if (typeof v !== "string" || !re.test(v)) {
    refuse("an attribute value of the wrong form");
  }
  return v;
}

function oneOf(v, set) {
  if (typeof v !== "string" || !set.has(v)) {
    refuse("an attribute value not on its list");
  }
  return v;
}

function anId(v) {
  if (typeof v !== "string" || !validId(v)) {
    refuse("an id that is not one of the page's");
  }
  return v;
}

const FLAG = Symbol("flag");

function flag(v) {
  if (typeof v !== "boolean") {
    refuse("a flag that is not true or false");
  }
  return v ? FLAG : null;
}

function attrRule(name) {
  if (Object.hasOwn(ATTRS, name)) {
    return ATTRS[name];
  }
  if (ariaRe.test(name)) {
    return ARIA_IDREFS.has(name) ? anId : text;
  }
  return refuse("the attribute " + JSON.stringify(String(name).slice(0, 40)));
}

// made is every node this module made, the only ones el() takes as
// children: an object from an answer's JSON can never pass for one.
const made = new WeakSet();

let doc = globalThis.document;

// useDocument sets the document nodes are made in (the tests' fake).
export function useDocument(d) {
  doc = d;
}

// el makes an element: tag one of TAGS, attrs on the list above, and
// children nodes this module made or strings, which become text nodes.
// null, undefined and false children are skipped and arrays flattened.
export function el(tag, attrs, ...children) {
  if (typeof tag !== "string" || !TAGS.has(tag)) {
    refuse("the element " + JSON.stringify(String(tag).slice(0, 20)));
  }
  const sets = [];
  if (attrs !== null && attrs !== undefined) {
    if (typeof attrs !== "object" || Array.isArray(attrs)) {
      refuse("attributes that are not an object");
    }
    for (const name of Object.keys(attrs)) {
      const v = attrs[name];
      if (v === null || v === undefined) {
        continue;
      }
      const s = attrRule(name)(v);
      if (s !== null) {
        sets.push([name, s === FLAG ? "" : s]);
      }
    }
  }
  const node = doc.createElement(tag);
  for (const [name, v] of sets) {
    node.setAttribute(name, v);
  }
  made.add(node);
  append(node, children);
  return node;
}

function append(node, children) {
  for (const c of nodes(children, [])) {
    node.appendChild(c);
  }
}

// nodes is children as the nodes they stand for.
function nodes(children, out) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) {
      continue;
    }
    if (Array.isArray(c)) {
      nodes(c, out);
    } else if (typeof c === "string" || typeof c === "number") {
      out.push(textNode(c));
    } else if (made.has(c)) {
      out.push(c);
    } else {
      refuse("a child that is neither text nor a node of the page's");
    }
  }
  return out;
}

function textNode(s) {
  const t = doc.createTextNode(String(s));
  made.add(t);
  return t;
}

// sandboxText is an inline value a sandbox wrote, isolated so a
// right-to-left run cannot reorder the host's text around it.
export function sandboxText(s) {
  return el("bdi", { class: "sandbox" }, text(s));
}

// sandboxBlock is a long text a sandbox wrote, whole, in a block that
// scrolls (app.css bounds its height) and is isolated as sandboxText is.
export function sandboxBlock(s) {
  return el("pre", { class: "sandbox sandbox-block" }, text(s));
}

// replace puts children in place of everything in parent, which is a
// node of the page's or of index.html.
export function replace(parent, ...children) {
  parent.replaceChildren(...nodes(children, []));
}

// byId is the node of index.html (or the page) with one of IDS's ids.
export function byId(id) {
  anId(id);
  return doc.getElementById(id);
}

// The routes, in the fragment. NAME is an environment's name.
const envNameRe = /^[a-z0-9][a-z0-9_-]{0,31}$/;

// parseRoute is the route a fragment names ("#/", "#/requests",
// "#/requests/NAME", "#/env/NAME", "#/env/NAME/doctor", "#/proposals"),
// as {page, env}, or null for anything else. An empty fragment is the
// overview.
export function parseRoute(hash) {
  if (typeof hash !== "string") {
    return null;
  }
  if (hash === "" || hash === "#" || hash === "#/") {
    return { page: "overview", env: null };
  }
  if (!hash.startsWith("#/")) {
    return null;
  }
  const parts = hash.slice(2).split("/");
  const env = (n) => (typeof n === "string" && envNameRe.test(n) ? n : null);
  switch (parts[0]) {
    case "requests":
      if (parts.length === 1) {
        return { page: "requests", env: null };
      }
      if (parts.length === 2 && env(parts[1])) {
        return { page: "requests", env: parts[1] };
      }
      return null;
    case "proposals":
      return parts.length === 1 ? { page: "proposals", env: null } : null;
    case "env":
      if (parts.length === 2 && env(parts[1])) {
        return { page: "env", env: parts[1] };
      }
      if (parts.length === 3 && env(parts[1]) && parts[2] === "doctor") {
        return { page: "doctor", env: parts[1] };
      }
      return null;
  }
  return null;
}

// routeHash is a route's fragment, the inverse of parseRoute.
export function routeHash(route) {
  const ok = route !== null && typeof route === "object" && (route.env === null || envNameRe.test(route.env ?? ""));
  if (ok) {
    switch (route.page) {
      case "overview":
        if (route.env === null) return "#/";
        break;
      case "requests":
        return route.env === null ? "#/requests" : "#/requests/" + route.env;
      case "proposals":
        if (route.env === null) return "#/proposals";
        break;
      case "env":
        if (route.env !== null) return "#/env/" + route.env;
        break;
      case "doctor":
        if (route.env !== null) return "#/env/" + route.env + "/doctor";
        break;
    }
  }
  return refuse("a route that is not one of the page's");
}

// navLink is a link to one of the page's routes, given as its fragment:
// the one way an href is ever set.
export function navLink(hash, label, attrs) {
  const route = parseRoute(hash);
  if (route === null || hash === "" || hash === "#") {
    refuse("a link that is not one of the page's routes");
  }
  const a = doc.createElement("a");
  a.setAttribute("href", routeHash(route));
  for (const [name, v] of Object.entries(attrs ?? {})) {
    if (name !== "class" && name !== "aria-current" && name !== "aria-label") {
      refuse("the link attribute " + JSON.stringify(String(name).slice(0, 40)));
    }
    if (v !== null && v !== undefined) {
      a.setAttribute(name, attrRule(name)(v));
    }
  }
  made.add(a);
  append(a, [label]);
  return a;
}
