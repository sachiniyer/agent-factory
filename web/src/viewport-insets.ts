// The visible area above a soft keyboard, as CSS variables (#6431).
//
// A fixed overlay is laid out against the LAYOUT viewport, which a soft keyboard
// does not shrink on current Chrome (interactive-widget resizes-visual) or iOS
// Safari — only the visual viewport shrinks. A centred modal therefore ends up
// partly under the keyboard, and its primary action with it. The keybar already
// follows the visual viewport; this publishes the same numbers so overlays can.

export interface ViewportLike { height: number; offsetTop: number }

/** How far the visual viewport's top and bottom edges sit inside the layout
 *  viewport, in CSS px (both >= 0). With no keyboard and no pinch both are 0. */
export function viewportInsets(innerHeight: number, vv: ViewportLike): { top: number; bottom: number } {
  const top = Math.max(0, vv.offsetTop);
  const bottom = Math.max(0, innerHeight - (top + vv.height));
  return { top: Math.round(top), bottom: Math.round(bottom) };
}

/** Publishes --af-vv-top and --af-vv-bottom on `root` and keeps them current.
 *  Returns a disposer. A browser without visualViewport leaves both at 0. */
export function trackViewportInsets(root: HTMLElement, win: Window = window): () => void {
  const vv = win.visualViewport;
  const apply = () => {
    const { top, bottom } = vv ? viewportInsets(win.innerHeight, vv) : { top: 0, bottom: 0 };
    root.style.setProperty("--af-vv-top", `${top}px`);
    root.style.setProperty("--af-vv-bottom", `${bottom}px`);
  };
  apply();
  if (!vv) return () => {};
  vv.addEventListener("resize", apply);
  vv.addEventListener("scroll", apply);
  win.addEventListener("resize", apply);
  return () => {
    vv.removeEventListener("resize", apply);
    vv.removeEventListener("scroll", apply);
    win.removeEventListener("resize", apply);
  };
}
