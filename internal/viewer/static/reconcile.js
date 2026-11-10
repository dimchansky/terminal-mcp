// reconcile makes parent's children exactly `nodes`, in order, moving as
// little as possible: stale children are removed first, and a child that is
// already in place is never touched, so focus, selection, scroll positions
// and open blocks inside unchanged entries survive.
export function reconcile(parent, nodes) {
  const wanted = new Set(nodes);
  for (const child of [...parent.childNodes]) {
    if (!wanted.has(child)) { parent.removeChild(child); }
  }
  for (let i = 0; i < nodes.length; i++) {
    const at = parent.childNodes[i] || null;
    if (at !== nodes[i]) { parent.insertBefore(nodes[i], at); }
  }
}
