// Theme system (redesign PR1): first-class light/dark theming for the web client.
//
// The design tokens live in styles.css as CSS custom properties, with LIGHT as the
// :root default, a dark layer via @media (prefers-color-scheme: dark), and explicit
// :root[data-theme="light"] / [data-theme="dark"] blocks that let a user toggle WIN
// over the media query in both directions. This module owns the small runtime half:
//
//   - the persisted Auto/Light/Dark CHOICE (localStorage, best-effort),
//   - stamping `data-theme` on <html> so the CSS resolves the right layer, and
//   - deriving the xterm.js ITheme (which cannot read CSS vars) from the resolved
//     concrete mode, keyed to the same token values so the terminal matches chrome.
//
// The boot stamp (bootStampTheme) runs at the very top of index.ts BEFORE the app
// mounts, so an explicit dark/light choice is applied before first paint — no
// light/dark flash. It is CSP-safe: it ships inside the same-origin bundle, not as
// an inline <script> (the daemon's `default-src 'self'` blocks inline script).

import type { ITheme } from "@xterm/xterm";

/** The persisted user preference: Auto follows the OS, Light/Dark force a mode. */
export type ThemeChoice = "auto" | "light" | "dark";
/** The resolved concrete mode Auto collapses to for xterm + any JS that needs it. */
export type ThemeMode = "light" | "dark";

/** The Auto/Light/Dark cycle order, for the appbar toggle. */
export const THEME_CHOICES: readonly ThemeChoice[] = ["auto", "light", "dark"];

const STORAGE_KEY = "af-theme";

function isChoice(v: unknown): v is ThemeChoice {
  return v === "auto" || v === "light" || v === "dark";
}

/** Reads the saved choice, defaulting to Auto (follow the OS) when unset or when
 *  localStorage is unavailable (private mode / blocked). */
export function readThemeChoice(): ThemeChoice {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (isChoice(raw)) {
      return raw;
    }
  } catch {
    // storage blocked — fall through to the Auto default
  }
  return "auto";
}

/** Persists the choice (best-effort; a blocked store is a silent no-op). */
export function persistThemeChoice(choice: ThemeChoice): void {
  try {
    localStorage.setItem(STORAGE_KEY, choice);
  } catch {
    // storage blocked — the choice still applies this session, just not persisted
  }
}

/** Stamps `data-theme` on <html> for a choice: an explicit light/dark sets the
 *  attribute so its :root[data-theme=…] block wins over the media query; Auto
 *  removes the attribute so the media query decides. */
export function stampTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === "auto") {
    root.removeAttribute("data-theme");
  } else {
    root.setAttribute("data-theme", choice);
  }
}

/** The earliest boot stamp: read the saved choice and apply it before first paint
 *  so an explicit theme shows no light/dark flash. Returns the choice so the caller
 *  can seed its state without re-reading storage. Called at index.ts module top. */
export function bootStampTheme(): ThemeChoice {
  const choice = readThemeChoice();
  stampTheme(choice);
  return choice;
}

function prefersDark(): boolean {
  try {
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  } catch {
    // No matchMedia (very old / headless): the CSS :root default is LIGHT, so match
    // it here rather than guessing dark.
    return false;
  }
}

/** The concrete mode the DOM is actually rendering: the stamped explicit attribute
 *  wins, else the OS preference (Auto). Reads the live DOM so it's always in sync
 *  with what CSS resolved, without threading the choice through every caller. */
export function currentMode(): ThemeMode {
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "light" || attr === "dark") {
    return attr;
  }
  return prefersDark() ? "dark" : "light";
}

// --- xterm themes ----------------------------------------------------------
//
// xterm.js paints on a canvas and cannot read CSS custom properties, so its ITheme
// is derived here from the SAME token values styles.css uses. background/foreground
// are the term surface + primary text tokens; the ANSI palette is tuned per mode so
// agent output stays legible (dark: a GitHub-dark-ish palette on the deep term bg;
// light: darker, saturated hues that read on a near-white background).

/** Dark xterm theme: background = --af-bg-term (dark), foreground = --af-text (dark),
 *  selection = --af-accent-tint (dark). */
export const DARK_XTERM: ITheme = {
  background: "#0c1016",
  foreground: "#e7ecf3",
  cursor: "#e7ecf3",
  cursorAccent: "#0c1016",
  selectionBackground: "rgba(122, 162, 247, 0.2)",
  black: "#484f58",
  red: "#ff7b72",
  green: "#3fb950",
  yellow: "#d29922",
  blue: "#58a6ff",
  magenta: "#bc8cff",
  cyan: "#39c5cf",
  white: "#b1bac4",
  brightBlack: "#6e7681",
  brightRed: "#ffa198",
  brightGreen: "#56d364",
  brightYellow: "#e3b341",
  brightBlue: "#79c0ff",
  brightMagenta: "#d2a8ff",
  brightCyan: "#56d4dd",
  brightWhite: "#f0f6fc",
};

/** Light xterm theme: background = --af-bg-term (light), foreground = --af-text
 *  (light), selection = --af-accent-tint (light). The ANSI palette uses GitHub-light
 *  hues — darker and more saturated than the dark palette so colored agent output
 *  stays readable on a near-white terminal. */
export const LIGHT_XTERM: ITheme = {
  background: "#fdfefe",
  foreground: "#17202e",
  cursor: "#17202e",
  cursorAccent: "#fdfefe",
  selectionBackground: "rgba(47, 95, 216, 0.16)",
  black: "#24292f",
  red: "#cf222e",
  green: "#1a7f37",
  yellow: "#9a6700",
  blue: "#0969da",
  magenta: "#8250df",
  cyan: "#1b7c83",
  white: "#6e7781",
  brightBlack: "#57606a",
  brightRed: "#a40e26",
  brightGreen: "#116329",
  brightYellow: "#7d4e00",
  brightBlue: "#0550ae",
  brightMagenta: "#6639ba",
  brightCyan: "#3192aa",
  brightWhite: "#8c959f",
};

/** The xterm ITheme for a resolved mode. */
export function xtermTheme(mode: ThemeMode): ITheme {
  return mode === "dark" ? DARK_XTERM : LIGHT_XTERM;
}

/** The xterm ITheme matching whatever the DOM is currently rendering, so a freshly
 *  constructed terminal (terminal.ts) is born in the active theme without the caller
 *  needing to know the mode. */
export function currentXtermTheme(): ITheme {
  return xtermTheme(currentMode());
}
