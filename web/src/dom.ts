// The one shared DOM primitive (#1592 Phase 5 PR8), lifted out of modals.ts in
// #2788 so a view module can build elements without importing the modal
// builders — modals.ts now composes such a module (dirpicker.ts), and leaving
// `h` there would have made that an import cycle.
//
// CSP-safe like the rest of the client: createElement + addEventListener only,
// no innerHTML with markup and no inline handlers, so the daemon's
// default-src 'self' policy holds.

/** Minimal hyperscript (shared by the modal builders, the directory picker, and
 *  the projects/tasks panes): create an element, apply props, append children —
 *  CSP-safe createElement, no innerHTML. */
export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") {
      el.className = value as string;
    } else {
      (el as unknown as Record<string, unknown>)[key] = value;
    }
  }
  for (const child of children) {
    el.append(child);
  }
  return el;
}
