// The DOM-focus half of a keyboard-shortcut gesture (#4360): when the gesture's
// overlay closes, focus returns to the element that held it when the shortcut
// fired — or to the rail, which unlike a button is a landmark the document
// listener still owns. The rail fallback must be VERIFIED, not assumed: at
// phone width the rail is a closed drawer (visibility:hidden) where focus() is
// a no-op, and without the check the just-hidden picker item keeps DOM focus —
// a BUTTON the next shortcut then dispatches to and the native-control guard
// swallows. Blurring drops DOM focus to body, where rail keys still route.

/** Restores DOM focus after a shortcut-owned overlay (the new-tab picker)
 *  cancels: prefers the element focused when the shortcut fired, then the
 *  rail. A rail that cannot take focus (missing, or a hidden phone drawer)
 *  falls through to blurring whatever still holds it. */
export function restoreShortcutFocus(navigationTarget: HTMLElement | null, rail: HTMLElement | null): void {
  if (navigationTarget?.isConnected && navigationTarget !== document.body) {
    navigationTarget.focus({ preventScroll: true });
  }
  if (document.activeElement === navigationTarget && navigationTarget !== document.body) {
    return;
  }
  if (rail) {
    rail.tabIndex = -1;
    rail.focus({ preventScroll: true });
    if (document.activeElement === rail) {
      return;
    }
  }
  (document.activeElement as HTMLElement | null)?.blur();
}
