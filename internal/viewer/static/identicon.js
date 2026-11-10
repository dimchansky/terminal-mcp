// Deterministic chat icons. The seed is a SHA-256 digest of the
// conversation key (identitySource, NUL, conversationId), computed locally;
// the server ID is not part of it, so an icon survives restarts. An icon is
// a recognition aid, not an identifier: it always sits next to a text label.

import { svg } from "./view.js";

// cells returns the 5×5 mirrored grid (row-major booleans) and a hue.
export function pattern(digest) {
  const bytes = digest instanceof Uint8Array ? digest : new Uint8Array(digest);
  const hue = ((bytes[0] << 8) | bytes[1]) % 360;
  const cells = [];
  for (let row = 0; row < 5; row++) {
    for (let col = 0; col < 5; col++) {
      const c = col < 3 ? col : 4 - col; // Mirror columns 3 and 4.
      const bit = row * 3 + c;
      cells.push(((bytes[2 + (bit >> 3)] >> (bit & 7)) & 1) === 1);
    }
  }
  return { hue, cells };
}

// icon returns the SVG view node for a digest; null gives the generic icon
// of a lane without a conversation ID.
export function icon(digest) {
  if (digest === null) {
    return svg("svg", { class: "identicon generic", viewBox: "0 0 5 5", width: 20, height: 20, "aria-hidden": "true" },
      svg("rect", { x: 1, y: 1, width: 3, height: 3, fill: "currentColor" }));
  }
  const { hue, cells } = pattern(digest);
  const fill = "hsl(" + hue + ", 55%, 48%)";
  const rects = [];
  cells.forEach((on, i) => {
    if (on) { rects.push(svg("rect", { x: i % 5, y: Math.floor(i / 5), width: 1, height: 1, fill })); }
  });
  return svg("svg", { class: "identicon", viewBox: "0 0 5 5", width: 20, height: 20, "aria-hidden": "true",
    "shape-rendering": "crispEdges" }, rects);
}

// conversationKey is the digest input of a lane.
export function conversationKey(identitySource, conversationId) {
  return new TextEncoder().encode(identitySource + "\u0000" + conversationId);
}

// shortLabels returns, for each distinct conversation ID, the shortest
// prefix of at least min code points that no other ID shares (the full ID
// if needed), so visible labels of different IDs never collide. The same ID
// under two identity sources gets the same label; lanes also show the
// source. Sorting makes it O(k log k · L).
export function shortLabels(ids, min = 8) {
  const sorted = [...new Set(ids)].map((id) => [...id]).sort((a, b) => (a.join("") < b.join("") ? -1 : 1));
  const common = (a, b) => { let n = 0; while (n < a.length && n < b.length && a[n] === b[n]) { n++; } return n; };
  const out = new Map();
  sorted.forEach((cps, i) => {
    const shared = Math.max(i > 0 ? common(cps, sorted[i - 1]) : 0, i + 1 < sorted.length ? common(cps, sorted[i + 1]) : 0);
    const n = Math.max(min, shared + 1);
    const id = cps.join("");
    out.set(id, n < cps.length ? cps.slice(0, n).join("") + "…" : id);
  });
  return out;
}
