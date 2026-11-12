// When the owner counts as reading (auto-switch waits) and when what is on
// screen counts as seen. Pure: the page gathers the facts from the DOM.

// isReading: auto-switch waits while the owner is not at the live tail,
// selects text, has a block, row or group open, works with a control,
// looks at the archive, names a chat, has the conversation list open over
// the timeline (they are choosing a chat), or filters the history (a
// switch would show another chat through the same filters).
export function isReading(f) {
  return !f.follow || !!f.selecting || !!f.reading || !!f.working || !!f.archive || !!f.renaming || !!f.panel || !!f.filtering;
}

// canSee: entries count as seen only while the tab is visible, the live
// view is shown and nothing covers the timeline.
export function canSee(f) {
  return f.visible && !f.archive && !f.panel;
}

// closeEffects says what closing the conversation list does, by why it
// closes: {focus, seen, resume}. focus returns keyboard focus to Chats;
// seen marks what is now in view (the whole timeline, at its end) as seen;
// resume lets a waiting switch go ahead. Choosing a chat does neither (the
// chosen chat is marked seen once shown); a click on a header control lets
// that control decide, so no switch goes first; a reset (sign-out, an
// ended session) only clears state.
export function closeEffects(reason) {
  switch (reason) {
    case "escape": case "toggle": return { focus: true, seen: true, resume: true };
    case "lane": return { focus: true, seen: false, resume: false };
    case "outside": case "resize": return { focus: false, seen: true, resume: true };
    case "bar": return { focus: false, seen: true, resume: false };
    default: return { focus: false, seen: false, resume: false }; // "reset"
  }
}

// seenEntries picks the timeline entries (in order, day separators left
// out) that count as seen now: those shown, or every entry once the last
// one is shown, because the owner has reached the end of the timeline. New
// activity above the view (a chat opens at its tail) is then not left
// unread. Items a filter hides are not entries, so they stay unread.
export function seenEntries(entries, isShown) {
  if (!entries.length) { return []; }
  return isShown(entries[entries.length - 1]) ? entries : entries.filter(isShown);
}

// settleStep advances holding the view at the timeline's tail by one
// animation frame, given the scroll height and the frame's time (ms): the
// hold ends once the height has stayed the same for two frames (cards near
// the tail have their real size), or two seconds after its first frame, so
// it never runs on.
export const settleStart = Object.freeze({ last: -1, stable: 0, since: -1, done: false });

export function settleStep(s, height, now) {
  const since = s.since < 0 ? now : s.since;
  const stable = height === s.last ? s.stable + 1 : 0;
  return { last: height, stable, since, done: stable >= 2 || now - since >= 2000 };
}
