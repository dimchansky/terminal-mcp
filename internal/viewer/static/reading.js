// When the owner counts as reading (auto-switch waits) and when what is on
// screen counts as seen. Pure: the page gathers the facts from the DOM.

// isReading: auto-switch waits while the owner is not at the live tail,
// selects text, has a block, row or group open, works with a control,
// looks at the archive, names a chat, or has the conversation list open
// over the timeline (they are choosing a chat).
export function isReading(f) {
  return !f.follow || !!f.selecting || !!f.reading || !!f.working || !!f.archive || !!f.renaming || !!f.panel;
}

// canSee: entries count as seen only while the tab is visible, the live
// view is shown and nothing covers the timeline.
export function canSee(f) {
  return f.visible && !f.archive && !f.panel;
}

// closeEffects says what closing the conversation list does, by why it
// closes: {focus, seen, resume}. focus returns keyboard focus to Chats;
// seen marks what is now in view as seen; resume lets a waiting switch go
// ahead. Choosing a chat does neither (the chosen chat is marked seen once
// shown); a click on a header control lets that control decide, so no
// switch goes first; a reset (sign-out, an ended session) only clears state.
export function closeEffects(reason) {
  switch (reason) {
    case "escape": case "toggle": return { focus: true, seen: true, resume: true };
    case "lane": return { focus: true, seen: false, resume: false };
    case "outside": case "resize": return { focus: false, seen: true, resume: true };
    case "bar": return { focus: false, seen: true, resume: false };
    default: return { focus: false, seen: false, resume: false }; // "reset"
  }
}
