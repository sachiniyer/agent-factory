/** Keys an in-flight tab create. The connection generation is part of it so a request
 *  abandoned by a disconnect cannot block the same session and kind after reconnect. */
export function tabCreateKey(generation: number, sessionId: string, kind: string): string {
  return `${generation}\u0000${sessionId}\u0000${kind}`;
}

/** Holds the physical key that fired a tab create until it is released. The create
 *  can move focus into the new terminal while the key is still down, and its
 *  repeats would otherwise be typed into that shell. */
export function shortcutRepeatGuard() {
  let held: string | null = null;
  return {
    arm(code: string): void {
      held = code;
    },
    release(code: string): void {
      if (held === code) held = null;
    },
    swallow(e: { code: string; repeat: boolean }): boolean {
      if (held !== e.code) return false;
      if (e.repeat) return true;
      held = null;
      return false;
    },
  };
}
