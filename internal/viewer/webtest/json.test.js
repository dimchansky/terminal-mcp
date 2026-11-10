import { test } from "node:test";
import assert from "node:assert/strict";
import { parseRecord, field, str } from "../static/json.js";

const enc = (s) => new TextEncoder().encode(s);

test("keeps key order and values", () => {
  const r = parseRecord(enc('{"b":1,"a":"x","n":null,"t":true,"arr":[1,{"k":2}]}\n'));
  assert.deepEqual(r.issues, []);
  assert.deepEqual(r.value.entries.map((e) => e.key), ["b", "a", "n", "t", "arr"]);
  assert.equal(str(field(r.value, "a")), "x");
  assert.equal(field(r.value, "arr").items[1].entries[0].value.text, "2");
});

test("reports duplicate keys and keeps both values", () => {
  const r = parseRecord(enc('{"args":{"path":"a"},"args":{"path":"b"}}'));
  assert.match(r.issues.join(), /duplicate key "args"/);
  assert.equal(r.value.entries.length, 2);
});

test("reports integers that cannot be represented exactly", () => {
  const r = parseRecord(enc('{"n":9007199254740993,"ok":9007199254740991,"f":0.1,"e":1e400}'));
  assert.equal(r.issues.length, 2);
  assert.equal(field(r.value, "n").text, "9007199254740993");
  assert.equal(field(r.value, "ok").unsafe, false);
  assert.equal(field(r.value, "f").unsafe, false);
});

test("reports lone surrogates and invalid UTF-8", () => {
  assert.match(parseRecord(enc('{"s":"\\ud800x"}')).issues.join(), /lone surrogate/);
  assert.equal(parseRecord(enc('{"s":"\\ud83d\\ude00"}')).issues.length, 0);
  const bad = parseRecord(new Uint8Array([0x7b, 0x22, 0x73, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d]));
  assert.equal(bad.validUTF8, false);
  assert.match(bad.issues.join(), /invalid UTF-8/);
});

test("a byte order mark is kept and reported", () => {
  const r = parseRecord(new Uint8Array([0xef, 0xbb, 0xbf, ...new TextEncoder().encode('{"a":1}\n')]));
  assert.equal(r.text.startsWith("\uFEFF"), true);
  assert.match(r.issues.join(), /byte order mark/);
});

test("number exactness", () => {
  const r = parseRecord(enc('{"a":-0,"b":1E5,"c":1e20,"d":1e-400,"e":0e5,"f":12345678901234567890}'));
  assert.deepEqual(r.issues, ["number 1e-400 cannot be represented exactly", "number 12345678901234567890 cannot be represented exactly"]);
});

test("CRLF endings and escaped duplicate keys", () => {
  assert.deepEqual(parseRecord(enc('{"a":1}\r\n')).issues, []);
  assert.match(parseRecord(enc('{"a":1,"\\u0061":2}')).issues.join(), /duplicate key "a"/);
  assert.match(parseRecord(enc('{"\\ud800":1}')).issues.join(), /lone surrogate/);
});

test("malformed JSON keeps the text", () => {
  const r = parseRecord(enc("{not json}\n"));
  assert.equal(r.value, null);
  assert.equal(r.text, "{not json}\n");
  assert.match(r.issues.join(), /not valid JSON/);
});

test("deep nesting is reported, not a crash", () => {
  const r = parseRecord(enc("[".repeat(100) + "]".repeat(100)));
  assert.equal(r.value, null);
  assert.match(r.issues.join(), /not parsed: nesting/);
});
