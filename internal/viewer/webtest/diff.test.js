import { test } from "node:test";
import assert from "node:assert/strict";
import { lineDiff } from "../static/diff.js";

const show = (d) => d.ops.map((o) => o.op + o.text).join("|");

test("one changed line", () => {
  assert.equal(show(lineDiff("timeout := 5 * time.Second\n", "timeout := 10 * time.Second\n")),
    "-timeout := 5 * time.Second|+timeout := 10 * time.Second");
});

test("multiline edits keep every line", () => {
  const d = lineDiff("a\nb\nc\nd", "a\nB\nc\nd\ne");
  assert.equal(show(d), " a|-b|+B| c| d|+e");
});

test("empty sides and newline endings", () => {
  assert.equal(show(lineDiff("", "x\n")), "+x");
  assert.equal(show(lineDiff("x\n", "")), "-x");
  const d = lineDiff("x", "x\n");
  assert.equal(d.oldEndsWithNewline, false);
  assert.equal(d.newEndsWithNewline, true);
});

test("too large inputs fall back", () => {
  const big = Array.from({ length: 3000 }, (_, i) => "l" + i).join("\n");
  assert.equal(lineDiff(big, big + "x"), null);
});
