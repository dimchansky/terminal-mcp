// A lossless reader for audit records. JSON.parse would silently keep the
// last of duplicate keys, round unsafe integers and accept lone surrogates;
// the owner must see exactly what was recorded, so this reader keeps key
// order, keeps number text, and reports everything a typed view could
// misrepresent. A record with issues is shown as raw text first.

const MAX_DEPTH = 64;

// decode turns record bytes into text; invalid UTF-8 is reported, and the
// text then holds U+FFFD where bytes could not be decoded.
export function decode(bytes) {
  // ignoreBOM keeps a byte order mark as U+FEFF: no byte is dropped.
  try {
    return { text: new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes), validUTF8: true };
  } catch (e) {
    return { text: new TextDecoder("utf-8", { ignoreBOM: true }).decode(bytes), validUTF8: false };
  }
}

// parseRecord parses one JSONL record (bytes, trailing newline allowed).
// Nodes: {kind:"object", entries:[{key, value}]}, {kind:"array", items},
// {kind:"string", value, lone}, {kind:"number", text, value, unsafe}
// (renderers always show a number's text, never its rounded value),
// {kind:"bool", value}, {kind:"null"}. issues lists every lossy aspect.
export function parseRecord(bytes) {
  const { text, validUTF8 } = decode(bytes);
  const issues = [];
  if (!validUTF8) { issues.push("invalid UTF-8"); }
  if (text.startsWith("\uFEFF")) { issues.push("starts with a byte order mark"); }
  let value = null;
  try {
    const p = new Parser(text.replace(/\r?\n$/, ""), issues);
    value = p.parse();
  } catch (e) {
    issues.push((e instanceof DepthError ? "not parsed: " : "not valid JSON: ") + e.message);
    value = null;
  }
  return { text, validUTF8, value, issues };
}

class DepthError extends Error {}

class Parser {
  constructor(s, issues) { this.s = s; this.i = 0; this.issues = issues; }

  fail(msg) { throw new Error(msg + " at offset " + this.i); }

  ws() {
    while (this.i < this.s.length) {
      const c = this.s.charCodeAt(this.i);
      if (c !== 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0d) { break; }
      this.i++;
    }
  }

  parse() {
    this.ws();
    const v = this.value(0);
    this.ws();
    if (this.i !== this.s.length) { this.fail("trailing data"); }
    return v;
  }

  value(depth) {
    if (depth > MAX_DEPTH) { throw new DepthError("nesting deeper than " + MAX_DEPTH); }
    const c = this.s[this.i];
    if (c === "{") { return this.object(depth); }
    if (c === "[") { return this.array(depth); }
    if (c === "\"") { return this.string(); }
    if (c === "t" || c === "f") { return this.literal(c === "t" ? "true" : "false", { kind: "bool", value: c === "t" }); }
    if (c === "n") { return this.literal("null", { kind: "null" }); }
    return this.number();
  }

  literal(word, node) {
    if (this.s.startsWith(word, this.i)) { this.i += word.length; return node; }
    this.fail("unexpected character");
  }

  object(depth) {
    this.i++;
    const entries = [];
    const seen = new Set();
    this.ws();
    if (this.s[this.i] === "}") { this.i++; return { kind: "object", entries }; }
    for (;;) {
      this.ws();
      if (this.s[this.i] !== "\"") { this.fail("expected a key"); }
      const key = this.string();
      if (seen.has(key.value)) { this.issues.push("duplicate key " + JSON.stringify(key.value)); }
      seen.add(key.value);
      this.ws();
      if (this.s[this.i] !== ":") { this.fail("expected ':'"); }
      this.i++;
      this.ws();
      entries.push({ key: key.value, keyLone: key.lone, value: this.value(depth + 1) });
      this.ws();
      if (this.s[this.i] === ",") { this.i++; continue; }
      if (this.s[this.i] === "}") { this.i++; return { kind: "object", entries }; }
      this.fail("expected ',' or '}'");
    }
  }

  array(depth) {
    this.i++;
    const items = [];
    this.ws();
    if (this.s[this.i] === "]") { this.i++; return { kind: "array", items }; }
    for (;;) {
      this.ws();
      items.push(this.value(depth + 1));
      this.ws();
      if (this.s[this.i] === ",") { this.i++; continue; }
      if (this.s[this.i] === "]") { this.i++; return { kind: "array", items }; }
      this.fail("expected ',' or ']'");
    }
  }

  string() {
    this.i++;
    let out = "";
    let lone = false;
    for (;;) {
      if (this.i >= this.s.length) { this.fail("unterminated string"); }
      const c = this.s[this.i];
      if (c === "\"") { this.i++; break; }
      if (c.charCodeAt(0) < 0x20) { this.fail("control character in string"); }
      if (c !== "\\") { out += c; this.i++; continue; }
      const e = this.s[this.i + 1];
      this.i += 2;
      const simple = { "\"": "\"", "\\": "\\", "/": "/", b: "\b", f: "\f", n: "\n", r: "\r", t: "\t" };
      if (e in simple) { out += simple[e]; continue; }
      if (e !== "u") { this.fail("invalid escape"); }
      const hex = this.s.slice(this.i, this.i + 4);
      if (!/^[0-9a-fA-F]{4}$/.test(hex)) { this.fail("invalid \\u escape"); }
      this.i += 4;
      out += String.fromCharCode(parseInt(hex, 16));
    }
    for (let k = 0; k < out.length; k++) {
      const u = out.charCodeAt(k);
      if (u >= 0xd800 && u <= 0xdbff) {
        const next = out.charCodeAt(k + 1);
        if (next >= 0xdc00 && next <= 0xdfff) { k++; continue; }
        lone = true;
      } else if (u >= 0xdc00 && u <= 0xdfff) {
        lone = true;
      }
    }
    if (lone) { this.issues.push("lone surrogate in a string"); }
    return { kind: "string", value: out, lone };
  }

  number() {
    const m = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(this.s.slice(this.i));
    if (!m) { this.fail("unexpected character"); }
    this.i += m[0].length;
    const text = m[0];
    const value = Number(text);
    // unsafe: a JavaScript number cannot hold what the text says: an
    // integer beyond 2^53 written as digits, an overflow, or a non-zero
    // value that underflows to zero. The text itself is always kept.
    const integer = !/[.eE]/.test(text);
    const zero = /^-?0(?:\.0+)?(?:[eE][+-]?\d+)?$/.test(text);
    const unsafe = !Number.isFinite(value) || (integer && !Number.isSafeInteger(value)) || (value === 0 && !zero);
    if (unsafe) { this.issues.push("number " + text + " cannot be represented exactly"); }
    return { kind: "number", text, value, unsafe };
  }
}

// field returns the first value of key in an object node (null if absent).
export function field(node, key) {
  if (!node || node.kind !== "object") { return null; }
  for (const e of node.entries) { if (e.key === key) { return e.value; } }
  return null;
}

// str returns a string node's value, or null.
export function str(node) { return node && node.kind === "string" ? node.value : null; }
