// Visible escaping of characters that would otherwise be invisible or would
// change how surrounding text reads: controls, format characters (bidi
// overrides, zero-width joiners, tags), line and paragraph separators,
// default-ignorable and unassigned or private code points, spaces other
// than U+0020, lone surrogates, and the replacement character standing for
// undecodable bytes. Text is never interpreted; escapes are separate
// segments the renderer styles.

const SPECIAL = /^[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Cs}\p{Co}\p{Cn}\p{Default_Ignorable_Code_Point}\p{Zs}]$/u;
const NAMED = { 0x09: "\\t", 0x0a: "\\n", 0x0d: "\\r", 0x1b: "\\e", 0x00: "\\0" };

function hex(c, width) { return c.toString(16).toUpperCase().padStart(width, "0"); }

// label returns the visible escape for a code point, or null when the
// character is shown as is. In multiline text, newline and tab stay.
// invalidUTF8 says the text came from bytes that were not valid UTF-8, so
// U+FFFD stands for undecodable bytes rather than a recorded character.
export function label(c, multiline = false, invalidUTF8 = false) {
  if (c === 0x20) { return null; }
  if (multiline && (c === 0x0a || c === 0x09)) { return null; }
  if (c === 0xfffd) { return invalidUTF8 ? "\\uFFFD (undecodable bytes)" : "\\uFFFD"; }
  if (c < 0x20 || c === 0x7f) { return NAMED[c] || "\\x" + hex(c, 2); }
  // U+FFFC stands for an embedded object and shows as an empty box.
  if (c !== 0xfffc && !SPECIAL.test(String.fromCodePoint(c))) { return null; }
  const code = c > 0xffff ? "\\u{" + hex(c, 5) + "}" : "\\u" + hex(c, 4);
  return c >= 0xd800 && c <= 0xdfff ? code + " (lone surrogate)" : code;
}

// segments splits s into plain text and escapes:
// [{text}] or [{escape: "\\u202E"}], merging adjacent text.
export function segments(s, multiline = false, invalidUTF8 = false) {
  const out = [];
  let text = "";
  for (let i = 0; i < s.length; i++) {
    let c = s.charCodeAt(i);
    let width = 1;
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      const d = s.charCodeAt(i + 1);
      if (d >= 0xdc00 && d <= 0xdfff) { c = 0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00); width = 2; }
    }
    const l = label(c, multiline, invalidUTF8);
    if (l === null) {
      text += s.slice(i, i + width);
    } else {
      if (text) { out.push({ text }); text = ""; }
      out.push({ escape: l });
    }
    i += width - 1;
  }
  if (text) { out.push({ text }); }
  return out;
}

// hidden reports whether s contains anything segments would escape.
export function hidden(s, multiline = false) {
  return segments(s, multiline).some((x) => x.escape !== undefined);
}
