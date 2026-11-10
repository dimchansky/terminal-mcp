// Line diff of one requested replacement (old_text → new_text). It shows the
// requested change, not the file's result, and never invents file line
// numbers. When a lossless line diff is not possible within the size
// limit, lineDiff returns null and the caller shows Before/After in full.

const MAX_CELLS = 4_000_000;

// lines splits text into lines; the last element records whether the text
// ended with a newline.
export function lines(text) {
  const parts = text.split("\n");
  const endsWithNewline = parts.length > 1 && parts[parts.length - 1] === "";
  if (endsWithNewline) { parts.pop(); }
  return { parts: text === "" ? [] : parts, endsWithNewline };
}

// lineDiff returns [{op: " " | "-" | "+", text}] and the newline endings,
// or null when it cannot be computed losslessly.
export function lineDiff(oldText, newText) {
  const a = lines(oldText);
  const b = lines(newText);
  const n = a.parts.length;
  const m = b.parts.length;
  if ((n + 1) * (m + 1) > MAX_CELLS) { return null; }
  // Longest common subsequence table, filled from the end.
  const w = m + 1;
  const t = new Uint32Array((n + 1) * w);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      t[i * w + j] = a.parts[i] === b.parts[j] ? t[(i + 1) * w + j + 1] + 1 : Math.max(t[(i + 1) * w + j], t[i * w + j + 1]);
    }
  }
  const ops = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && a.parts[i] === b.parts[j]) {
      ops.push({ op: " ", text: a.parts[i] }); i++; j++;
    } else if (i < n && (j === m || t[(i + 1) * w + j] >= t[i * w + j + 1])) {
      ops.push({ op: "-", text: a.parts[i] }); i++; // Removed lines before added ones.
    } else {
      ops.push({ op: "+", text: b.parts[j] }); j++;
    }
  }
  const result = { ops, oldEndsWithNewline: a.endsWithNewline, newEndsWithNewline: b.endsWithNewline };
  return rebuild(result, " -") === oldText && rebuild(result, " +") === newText ? result : null;
}

function rebuild(d, keep) {
  const parts = d.ops.filter((o) => keep.includes(o.op)).map((o) => o.text);
  const ends = keep === " -" ? d.oldEndsWithNewline : d.newEndsWithNewline;
  return parts.join("\n") + (ends ? "\n" : "");
}
