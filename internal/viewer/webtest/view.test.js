import { test } from "node:test";
import assert from "node:assert/strict";
import { h, materialize, textOf } from "../static/view.js";
import { icon, pattern, shortLabels } from "../static/identicon.js";
import { toolClass, mayAutoSwitch, isPolling } from "../static/classes.js";

// A minimal document: enough to observe what materialize creates.
function fakeDocument() {
  const make = (tag, ns) => ({ tag, ns, attrs: {}, children: [], setAttribute(n, v) { this.attrs[n] = v; }, appendChild(c) { this.children.push(c); } });
  return { createElement: (t) => make(t), createElementNS: (ns, t) => make(t, ns), createTextNode: (text) => ({ text }) };
}

test("hostile text stays text", () => {
  const el = materialize(fakeDocument(), h("p", { class: "x" }, "<img src=x onerror=alert(1)>"));
  assert.deepEqual(el.children, [{ text: "<img src=x onerror=alert(1)>" }]);
});

test("unsafe elements and foreign ids are refused; aria booleans are explicit", () => {
  for (const tag of ["script", "iframe", "object", "form", "base", "meta", "style", "a", "img"]) {
    assert.throws(() => materialize(fakeDocument(), h(tag, {})), /not allowed/);
  }
  assert.throws(() => materialize(fakeDocument(), h("div", { id: "ended-reason" })), /tm-/);
  const el = materialize(fakeDocument(), h("button", { "aria-expanded": true, "aria-selected": false, hidden: true,
    disabled: false, "aria-label": null, "aria-live": undefined, id: null }));
  assert.deepEqual(el.attrs, { "aria-expanded": "true", "aria-selected": "false", hidden: "" });
});

test("unsafe attributes are refused", () => {
  for (const name of ["href", "src", "style", "onclick", "srcdoc"]) {
    assert.throws(() => materialize(fakeDocument(), h("a", { [name]: "javascript:alert(1)" })), /not allowed/);
  }
});

test("identicons are deterministic SVG with a text-free grid", () => {
  const digest = new Uint8Array(32).map((_, i) => i * 37);
  assert.deepEqual(pattern(digest), pattern(digest));
  const p = pattern(digest);
  for (let row = 0; row < 5; row++) {
    assert.equal(p.cells[row * 5], p.cells[row * 5 + 4]);
    assert.equal(p.cells[row * 5 + 1], p.cells[row * 5 + 3]);
  }
  const el = materialize(fakeDocument(), icon(digest));
  assert.equal(el.ns, "http://www.w3.org/2000/svg");
  assert.equal(textOf(icon(null)), "");
});

test("tool names never reach the prototype", () => {
  for (const name of ["constructor", "__proto__", "toString", "hasOwnProperty"]) {
    assert.equal(toolClass(name), "unknown");
  }
});

test("all fifteen identicon bits are used", () => {
  for (let bit = 0; bit < 15; bit++) {
    const d = new Uint8Array(32);
    d[2 + (bit >> 3)] = 1 << (bit & 7);
    assert.equal(pattern(d).cells.filter(Boolean).length, (bit % 3) === 2 ? 1 : 2, "bit " + bit);
  }
});

test("short labels are fast and cut at code points", () => {
  const ids = Array.from({ length: 300 }, (_, i) => "p".repeat(4000) + i);
  const start = Date.now();
  const labels = shortLabels(ids);
  assert.ok(Date.now() - start < 1000);
  assert.equal(new Set(labels.values()).size, 300);
  assert.equal(shortLabels(["😀😀😀😀😀😀😀😀😀😀a", "😀😀😀😀😀😀😀😀😀😀b"]).get("😀😀😀😀😀😀😀😀😀😀a"), "😀😀😀😀😀😀😀😀😀😀a");
});

test("short labels never collide", () => {
  const labels = shortLabels(["abcdefgh1234", "abcdefgh5678", "zzz"]);
  assert.equal(labels.get("abcdefgh1234"), "abcdefgh1…");
  assert.equal(labels.get("abcdefgh5678"), "abcdefgh5…");
  assert.equal(labels.get("zzz"), "zzz");
});

test("tool classes", () => {
  assert.equal(toolClass("terminal_start"), "launch");
  assert.equal(isPolling("terminal_read"), true);
  assert.equal(mayAutoSwitch("file_receive_result"), false);
  assert.equal(mayAutoSwitch("brand_new_tool"), false);
  assert.equal(mayAutoSwitch("file_read"), true);
});
