// The "Install app" affordance (feat: PWA).
//
// Follows the canonical beforeinstallprompt flow: preventDefault the event to stop
// the browser's own mini-infobar, stash it, reveal our button, and on click call the
// stashed event's prompt() and read userChoice. The stashed event is single-use —
// prompt() can only be called once per event — so it is dropped after use and the
// button hides with it.
//
// THE VISIBILITY RULE IS THE WHOLE DESIGN, and it is subtractive: the button starts
// hidden and is revealed only by a beforeinstallprompt that actually fired. That one
// signal already encodes every condition worth checking, so none of them need
// special-casing here:
//
//   - not installable (no manifest, no worker) → never fires → stays hidden
//   - already installed                        → never fires → stays hidden
//   - INSECURE CONTEXT                         → never fires → stays hidden
//
// That last one is the reason this file does no secure-context probing of its own.
// Reaching the daemon over plain HTTP on a Tailscale address (http://100.x) is not a
// secure context, so Chrome never fires the event and the button never appears —
// which is CORRECT, not a bug to route around. There is no way to install from an
// insecure origin, so an "Install app" button there could only lie. Installing works
// from http://localhost (localhost is a secure context by definition) or from any
// HTTPS origin; docs/web.md spells this out for users who go looking for the button.
//
// Firefox and iOS Safari never fire beforeinstallprompt at all, so they simply never
// see the button and use the browser's own add-to-home-screen. Nothing to do.

/** The Chromium-only event behind the install flow. Not in lib.dom, so it is declared
 *  here to the shape the spec gives it. */
interface BeforeInstallPromptEvent extends Event {
  readonly platforms: readonly string[];
  readonly userChoice: Promise<{ outcome: "accepted" | "dismissed"; platform: string }>;
  prompt(): Promise<void>;
}

/** localStorage key for "the user closed our install affordance". Kept in
 *  localStorage (not sessionStorage) so a dismissal survives reloads — the point is
 *  that it never nags again, and a per-tab memory would nag on every new tab. */
const DISMISS_KEY = "af-install-dismissed";

/** The inputs that decide whether the affordance is on screen. */
export interface InstallVisibility {
  /** a beforeinstallprompt fired and its event is stashed and unused */
  stashed: boolean;
  /** the user closed the affordance (persisted) */
  dismissed: boolean;
  /** an appinstalled fired this session */
  installed: boolean;
}

/** Whether the affordance should be visible. Pure, so the rule is unit-testable
 *  without a DOM or a Chromium (install.test.ts) — the DOM wiring around it is
 *  covered by the Playwright selftest against a real browser. */
export function shouldShowInstall(state: InstallVisibility): boolean {
  return state.stashed && !state.dismissed && !state.installed;
}

/** Reads the persisted dismissal, treating blocked storage as "not dismissed" —
 *  matching theme.ts, where an unavailable store degrades to the default rather than
 *  taking anything down. */
export function readInstallDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISS_KEY) === "1";
  } catch {
    return false;
  }
}

/** Persists the dismissal (best-effort; a blocked store just means it may reappear
 *  next load, which is a nag but not a break). */
export function persistInstallDismissed(): void {
  try {
    localStorage.setItem(DISMISS_KEY, "1");
  } catch {
    // storage blocked — the dismissal still applies for this page's lifetime
  }
}

/**
 * The appbar's install affordance: an "Install app" button plus its own dismiss.
 *
 * Construct ONCE, at the composition root, and mount `el` wherever it belongs.
 * It listens on `window`, and index.ts drops and rebuilds the AppShell on every
 * logout/login cycle — building this in the shell's constructor would stack a fresh
 * pair of window listeners on each cycle, and the stashed event would go stale
 * against whichever copy happened to have it.
 */
export class InstallAffordance {
  /** The mountable root. Hidden until a beforeinstallprompt says otherwise. */
  readonly el: HTMLElement;

  private stashed: BeforeInstallPromptEvent | null = null;
  private dismissed = readInstallDismissed();
  private installed = false;

  constructor() {
    const go = document.createElement("button");
    go.type = "button";
    go.className = "af-ghost af-install__go";
    go.textContent = "Install app";
    go.addEventListener("click", () => void this.promptNow());

    const dismiss = document.createElement("button");
    dismiss.type = "button";
    dismiss.className = "af-install__dismiss";
    dismiss.textContent = "×";
    dismiss.setAttribute("aria-label", "Dismiss install");
    dismiss.title = "Dismiss";
    dismiss.addEventListener("click", () => this.dismiss());

    this.el = document.createElement("div");
    this.el.className = "af-install";
    this.el.append(go, dismiss);
    this.sync();

    window.addEventListener("beforeinstallprompt", (event) => {
      // Suppress Chrome's own mini-infobar so ours is the only affordance, and keep
      // the event for the click — without preventDefault it cannot be replayed later.
      event.preventDefault();
      this.stashed = event as BeforeInstallPromptEvent;
      this.sync();
    });

    window.addEventListener("appinstalled", () => {
      // Installed by our button or by the browser's own menu — either way there is
      // nothing left to offer, and the stashed event is spent.
      this.installed = true;
      this.stashed = null;
      this.sync();
    });
  }

  /** Shows the browser's install dialog for the stashed event, then retires it. */
  private async promptNow(): Promise<void> {
    const event = this.stashed;
    if (!event) {
      return;
    }
    // Drop the reference BEFORE awaiting: prompt() is single-use per event, and a
    // double click would otherwise call it twice and throw.
    this.stashed = null;
    this.sync();
    try {
      await event.prompt();
      await event.userChoice;
    } catch {
      // The browser refused to show the dialog (already handled, or spent). The
      // event is gone either way and the button is already hidden.
    }
    // Deliberately NOT persisting a dismissal on an "dismissed" outcome: that was the
    // browser's dialog, not our affordance. Chrome may fire beforeinstallprompt again
    // later, and if it does the user gets the button back — declining a dialog once
    // is not the same as asking us to stop offering.
  }

  /** The user closed our affordance: hide it and remember, so it never nags again. */
  private dismiss(): void {
    this.dismissed = true;
    persistInstallDismissed();
    this.sync();
  }

  private sync(): void {
    this.el.hidden = !shouldShowInstall({
      stashed: this.stashed !== null,
      dismissed: this.dismissed,
      installed: this.installed,
    });
  }
}
