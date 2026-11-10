import { test } from "node:test";
import assert from "node:assert/strict";
import { segments, hidden } from "../static/text.js";

test("controls, bidi and invisible characters become visible escapes", () => {
  const s = "a\u202Eb\u200Bc\x1bd\u0000e\u00ADf";
  assert.deepEqual(segments(s), [
    { text: "a" }, { escape: "\\u202E" }, { text: "b" }, { escape: "\\u200B" }, { text: "c" },
    { escape: "\\e" }, { text: "d" }, { escape: "\\0" }, { text: "e" }, { escape: "\\u00AD" }, { text: "f" },
  ]);
});

test("newlines and tabs stay in multiline text only", () => {
  assert.deepEqual(segments("a\nb\tc", true), [{ text: "a\nb\tc" }]);
  assert.deepEqual(segments("a\nb", false), [{ text: "a" }, { escape: "\\n" }, { text: "b" }]);
});

test("lone surrogates and undecodable bytes are marked; pairs are kept", () => {
  assert.match(segments("x\ud800")[1].escape, /lone surrogate/);
  assert.equal(segments("\ufffd")[0].escape, "\\uFFFD");
  assert.match(segments("\ufffd", false, true)[0].escape, /undecodable/);
  assert.deepEqual(segments("😀"), [{ text: "😀" }]);
  assert.equal(hidden("plain <script>alert(1)</script>"), false);
});

test("every invisible or format class is escaped", () => {
  const cases = ["\u2028", "\u2029", "\u206A", "\uFFF9", "\uFFFC", "\u{E0100}", "\u180B", "\u{1D173}", "\u2065",
    "\uE000", "\u00A0", "\u2003", "\u202F", "\u3000", "\u{E0041}", "\uFE0F", "\u0085", "\u{10FFFF}"];
  for (const c of cases) {
    assert.equal(hidden("a" + c + "b"), true, "U+" + c.codePointAt(0).toString(16));
  }
  assert.equal(hidden("plain ASCII and ünïcödé, 中文, emoji 😀"), false);
});
