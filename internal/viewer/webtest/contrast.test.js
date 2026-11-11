// The page's colour tokens meet WCAG contrast in both themes. The tokens
// are read from app.css itself, so a changed colour is checked here.

import { readFileSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";

const css = readFileSync(new URL("../static/app.css", import.meta.url), "utf8");

// tokens parses the custom properties of one :root block.
function tokens(block) {
  const out = {};
  for (const m of block.matchAll(/--([a-z0-9-]+):\s*([^;]+);/g)) { out[m[1]] = m[2].trim(); }
  return out;
}

const light = tokens(/:root\s*\{([^}]*)\}/.exec(css)[1]);
const dark = tokens(/@media \(prefers-color-scheme: dark\)\s*\{\s*:root\s*\{([^}]*)\}/.exec(css)[1]);

function rgba(hex) {
  const n = (i) => parseInt(hex.slice(i, i + 2), 16);
  return [n(1), n(3), n(5), hex.length === 9 ? n(7) / 255 : 1];
}

// over composites a colour over an opaque one.
function over(fg, bg) {
  const [r, g, b, a] = rgba(fg);
  const [R, G, B] = rgba(bg);
  const mix = (x, y) => Math.round(x * a + y * (1 - a));
  return "#" + [mix(r, R), mix(g, G), mix(b, B)].map((v) => v.toString(16).padStart(2, "0")).join("");
}

function luminance(hex) {
  const [r, g, b] = rgba(hex).slice(0, 3).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function ratio(a, b) {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
}

function hue(hex) {
  const [r, g, b] = rgba(hex).slice(0, 3).map((v) => v / 255);
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  if (max === min) { return 0; }
  const d = max - min;
  const h = max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return (h * 60 + 360) % 360;
}

const hueGap = (a, b) => { const d = Math.abs(hue(a) - hue(b)); return Math.min(d, 360 - d); };

const NAMES = ["bg", "fg", "muted", "line", "ok", "bad", "warn", "unknown", "live", "seen", "focus", "selected-bg", "selected-bar",
  "unread-bg", "unread-fg", "code-bg", "del", "ins", "esc"];
const TEXT = ["fg", "muted", "ok", "bad", "warn", "unknown", "live", "seen", "esc"];

for (const [name, t] of [["light", light], ["dark", dark]]) {
  test(name + " theme: every token is defined", () => {
    for (const n of NAMES) {
      assert.ok(t[n], "--" + n + " is missing");
      assert.match(t[n], /^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$/, "--" + n + " must be #rrggbb or #rrggbbaa");
    }
    for (const n of [...TEXT, "bg", "focus", "selected-bar", "unread-bg", "unread-fg"]) {
      assert.equal(rgba(t[n])[3], 1, "--" + n + " must be opaque");
    }
  });

  test(name + " theme: text and status colours read at 4.5:1", () => {
    const code = over(t["code-bg"], t.bg);
    const selected = over(t["selected-bg"], t.bg);
    for (const n of TEXT) {
      assert.ok(ratio(t[n], t.bg) >= 4.5, `--${n} on --bg: ${ratio(t[n], t.bg).toFixed(2)}`);
      assert.ok(ratio(t[n], code) >= 4.5, `--${n} on --code-bg: ${ratio(t[n], code).toFixed(2)}`);
    }
    assert.ok(ratio(t.fg, selected) >= 4.5, "--fg on the selected row");
    assert.ok(ratio(t.muted, selected) >= 4.5, "--muted on the selected row");
    assert.ok(ratio(t["unread-fg"], t["unread-bg"]) >= 4.5, "unread count");
    // Diff lines are tints over the code background, with escape marks.
    for (const n of ["del", "ins"]) {
      const line = over(t[n], code);
      assert.ok(ratio(t.fg, line) >= 4.5, "--fg on --" + n);
      assert.ok(ratio(t.esc, line) >= 4.5, "--esc on --" + n);
    }
  });

  test(name + " theme: focus and the selected bar stand out at 3:1", () => {
    assert.ok(ratio(t.focus, t.bg) >= 3, "--focus");
    assert.ok(ratio(t["selected-bar"], t.bg) >= 3, "--selected-bar");
  });

  test(name + " theme: an error, a warning and an unknown outcome differ in hue", () => {
    assert.ok(hueGap(t.warn, t.unknown) >= 30, "warn vs unknown");
    assert.ok(hueGap(t.bad, t.warn) >= 30, "bad vs warn");
    assert.ok(hueGap(t.bad, t.unknown) >= 30, "bad vs unknown");
  });
}

test("no rule outside the token blocks names a colour or redefines a token", () => {
  const rest = css.replace(/:root\s*\{[^}]*\}/g, "").replace(/\/\*[\s\S]*?\*\//g, "").replace(/content:[^;]*;/g, "");
  assert.doesNotMatch(rest, /#[0-9a-fA-F]{3,8}\b|\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color-mix|light-dark|color)\(/);
  assert.doesNotMatch(rest, /:\s*(red|white|black|blue|green|gray|grey|orange|yellow|purple)\b/);
  for (const n of NAMES) { assert.doesNotMatch(rest, new RegExp("--" + n + "\\s*:"), "--" + n + " redefined outside the token blocks"); }
});
