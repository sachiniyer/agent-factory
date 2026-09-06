// Browser appearance selects one of the two generated product palettes.
import type { ITheme } from "@xterm/xterm";
import type { LatestRequest } from "./refetch.js";
import { TERMINAL_ANSI } from "./terminal_ansi.js";
export type ThemeChoice = "light" | "dark" | "system";
export type ThemeMode = "light" | "dark";
export const THEME_CHOICES: readonly ThemeChoice[] = ["light", "dark", "system"];
const STORAGE_KEY = "af-theme";

/** Empty string is the authorized tokenless sentinel; only null is disconnected. */
export function hasConnectedToken(token: string | null): token is string {
  return token !== null;
}

/** An async login may commit only while both its generation and credential remain installed. */
export function connectionAttemptMayCommit(
  request: LatestRequest,
  installedToken: string | null,
  candidate: string,
): boolean {
  return request.isCurrent() && installedToken === candidate;
}

export function normalizeThemeChoice(value: unknown): ThemeChoice {
  return value === "light" || value === "dark" ? value : "system";
}
export function readThemeChoice(): ThemeChoice {
  try { return normalizeThemeChoice(localStorage.getItem(STORAGE_KEY)); }
  catch { return "system"; }
}
export function persistThemeChoice(choice: ThemeChoice): void {
  try { localStorage.setItem(STORAGE_KEY, choice); } catch { /* best effort */ }
}
export function currentMode(): ThemeMode {
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "light" || attr === "dark") return attr;
  try { return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"; }
  catch { return "dark"; }
}
function surface(mode: ThemeMode): string {
  const probe = document.createElement("span");
  probe.dataset.afTheme = mode;
  probe.hidden = true;
  document.documentElement.append(probe);
  const color = getComputedStyle(probe).getPropertyValue("--af-surface").trim();
  probe.remove();
  return color;
}
export function themeColorMetaContents(choice: ThemeChoice): { light: string; dark: string } {
  return { light: surface(choice === "system" ? "light" : choice), dark: surface(choice === "system" ? "dark" : choice) };
}
export function refreshThemeMode(): void {
  const mode = currentMode();
  document.documentElement.dataset.afTheme = mode;
  for (const chrome of document.querySelectorAll<HTMLElement>("[data-af-theme]")) chrome.dataset.afTheme = mode;
  const colors = themeColorMetaContents(document.documentElement.hasAttribute("data-theme") ? mode : "system");
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    meta.setAttribute("content", (meta.getAttribute("media") ?? "").includes("dark") ? colors.dark : colors.light);
  }
}
export function stampTheme(choice: ThemeChoice): void {
  if (choice === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", choice);
  refreshThemeMode();
}
export function bootStampTheme(): ThemeChoice {
  const choice = readThemeChoice();
  stampTheme(choice);
  return choice;
}
export function xtermTheme(mode: ThemeMode): ITheme { return TERMINAL_ANSI[mode]; }
export function currentXtermTheme(): ITheme { return xtermTheme(currentMode()); }
