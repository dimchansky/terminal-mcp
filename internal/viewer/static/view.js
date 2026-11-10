// View trees and their materializer. Renderers return plain objects;
// materialize turns them into DOM with createElement and textContent only,
// so recorded text can never become markup, script or a link. Attributes
// come from a fixed allowlist (no href, src, style or event handlers).

export const SVG = "http://www.w3.org/2000/svg";

// Elements renderers may create: no script, frame, form, style, base or
// meta elements, whatever a renderer computes.
const TAGS = new Set(["div", "span", "p", "pre", "code", "ul", "ol", "li", "dl", "dt", "dd", "table", "thead", "tbody",
  "tr", "th", "td", "button", "label", "input", "details", "summary", "section", "header", "nav", "main", "aside",
  "h1", "h2", "h3", "h4", "time", "mark", "del", "ins", "strong", "em", "small", "abbr", "kbd",
  "svg", "rect", "g", "circle", "path"]);
// Attributes; an id must carry the page's own prefix, so recorded text can
// never name or shadow page elements.
const ATTRS = new Set(["class", "id", "role", "title", "tabindex", "type", "hidden", "disabled", "for", "checked",
  "aria-label", "aria-live", "aria-hidden", "aria-expanded", "aria-selected", "aria-controls", "aria-describedby",
  "aria-current", "aria-pressed", "data-key", "data-lane", "data-action",
  "open", "viewBox", "width", "height", "x", "y", "fill", "shape-rendering"]);

const ARIA_STATES = new Set(["aria-hidden", "aria-expanded", "aria-selected", "aria-pressed", "aria-current"]);
const IDREFS = new Set(["id", "for", "aria-controls", "aria-describedby"]);

// h builds a view node: h("div", {class: "x"}, child, "text", ...).
// Strings become text nodes; null and false children are skipped.
export function h(tag, attrs, ...children) {
  return { tag, attrs: attrs || {}, children: children.flat(Infinity).filter((c) => c !== null && c !== false && c !== undefined) };
}

// svg builds an SVG view node.
export function svg(tag, attrs, ...children) {
  return { ...h(tag, attrs, ...children), ns: SVG };
}

// allowedAttribute reports whether an attribute may be set.
export function allowedAttribute(name) { return ATTRS.has(name); }

// materialize creates DOM nodes for a view tree in document doc.
export function materialize(doc, node) {
  if (typeof node === "string" || typeof node === "number") { return doc.createTextNode(String(node)); }
  if (!TAGS.has(node.tag)) { throw new Error("element not allowed: " + node.tag); }
  const el = node.ns ? doc.createElementNS(node.ns, node.tag) : doc.createElement(node.tag);
  for (const [name, value] of Object.entries(node.attrs)) {
    if (!ATTRS.has(name)) { throw new Error("attribute not allowed: " + name); }
    if (value === null || value === undefined) { continue; }
    if (typeof value === "boolean") {
      // Boolean ARIA states need "true"/"false"; other booleans are present or absent.
      if (ARIA_STATES.has(name)) { el.setAttribute(name, String(value)); } else if (value) { el.setAttribute(name, ""); }
      continue;
    }
    if (IDREFS.has(name) && !String(value).startsWith("tm-")) { throw new Error("ids must start with tm-: " + value); }
    el.setAttribute(name, String(value));
  }
  for (const child of node.children) { el.appendChild(materialize(doc, child)); }
  return el;
}

// textOf returns the concatenated text of a view tree (for tests).
export function textOf(node) {
  if (typeof node === "string" || typeof node === "number") { return String(node); }
  return node.children.map(textOf).join("");
}

// find returns the view nodes (depth first) whose class list includes cls.
export function find(node, cls) {
  if (typeof node !== "object") { return []; }
  const own = (node.attrs.class || "").split(" ").includes(cls) ? [node] : [];
  return own.concat(...node.children.map((c) => find(c, cls)));
}
