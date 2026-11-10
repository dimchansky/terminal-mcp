import { test } from "node:test";
import assert from "node:assert/strict";
import { reconcile } from "../static/reconcile.js";

// A parent that counts how often existing children are moved.
function fakeParent(children) {
  const p = { childNodes: [...children], moves: 0 };
  p.removeChild = (c) => { p.childNodes.splice(p.childNodes.indexOf(c), 1); };
  p.insertBefore = (n, ref) => {
    const at = p.childNodes.indexOf(n);
    if (at >= 0) { p.moves++; p.childNodes.splice(at, 1); }
    const i = ref ? p.childNodes.indexOf(ref) : p.childNodes.length;
    p.childNodes.splice(i, 0, n);
  };
  return p;
}

test("unchanged entries are never moved", () => {
  const [ctl, a, b, c, d] = ["ctl", "a", "b", "c", "d"].map((name) => ({ name }));
  const p = fakeParent([ctl, a, b, c]);
  reconcile(p, [ctl, a, b, c, d]); // Appended.
  assert.equal(p.moves, 0);
  const n = { name: "new-b" };
  reconcile(p, [ctl, a, n, c, d]); // One entry rebuilt.
  assert.equal(p.moves, 0);
  reconcile(p, [ctl, c, d]); // Entries removed.
  assert.equal(p.moves, 0);
  const z = { name: "z" };
  reconcile(p, [ctl, z, c, d]); // Prepended (an earlier page).
  assert.equal(p.moves, 0);
  assert.deepEqual(p.childNodes.map((x) => x.name), ["ctl", "z", "c", "d"]);
});
