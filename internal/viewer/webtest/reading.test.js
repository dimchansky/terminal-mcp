import { test } from "node:test";
import assert from "node:assert/strict";
import { isReading, canSee, closeEffects, seenEntries } from "../static/reading.js";

const idle = { follow: true, selecting: false, reading: false, working: false, archive: false, renaming: false, panel: false };

test("auto-switch waits while the owner reads, works or chooses a chat", () => {
  assert.equal(isReading(idle), false);
  for (const k of ["selecting", "reading", "working", "archive", "renaming", "panel"]) {
    assert.equal(isReading({ ...idle, [k]: true }), true, k);
  }
  assert.equal(isReading({ ...idle, follow: false }), true, "scrolled up");
});

test("nothing is seen while the list covers the timeline, the tab is hidden or the archive is shown", () => {
  assert.equal(canSee({ visible: true, archive: false, panel: false }), true);
  assert.equal(canSee({ visible: true, archive: false, panel: true }), false);
  assert.equal(canSee({ visible: false, archive: false, panel: false }), false);
  assert.equal(canSee({ visible: true, archive: true, panel: false }), false);
});

test("closing the list resumes only when the owner returns to the same timeline", () => {
  assert.deepEqual(closeEffects("escape"), { focus: true, seen: true, resume: true });
  assert.deepEqual(closeEffects("toggle"), { focus: true, seen: true, resume: true });
  assert.deepEqual(closeEffects("lane"), { focus: true, seen: false, resume: false }, "the old chat's entries are not marked seen");
  assert.deepEqual(closeEffects("outside"), { focus: false, seen: true, resume: true });
  assert.deepEqual(closeEffects("resize"), { focus: false, seen: true, resume: true });
  assert.deepEqual(closeEffects("bar"), { focus: false, seen: true, resume: false }, "a header control acts before any switch");
  assert.deepEqual(closeEffects("reset"), { focus: false, seen: false, resume: false }, "signing out touches no client");
});

test("reaching the end of the timeline marks every entry it shows seen", () => {
  const shown = new Set(["c", "d"]);
  const isShown = (e) => shown.has(e);
  assert.deepEqual(seenEntries(["a", "b", "c", "d"], isShown), ["a", "b", "c", "d"], "the last entry is shown: new activity above the view is seen too");
  shown.delete("d");
  assert.deepEqual(seenEntries(["a", "b", "c", "d"], isShown), ["c"], "scrolled up: only what is shown");
  assert.deepEqual(seenEntries([], isShown), []);
});
