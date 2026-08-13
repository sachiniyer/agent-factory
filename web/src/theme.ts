// Browser theme choice + daemon palette projection (#3220 deliverable 1).
//
// The browser owns only Auto/Light/Dark. The daemon owns the semantic palette;
// this module validates its nineteen source slots, derives renderer tokens and
// the xterm ANSI palette, and installs the active mode as CSS custom properties.

import type { ITheme } from "@xterm/xterm";
import type { DaemonTheme } from "./types.js";

export type ThemeChoice = "auto" | "light" | "dark";
export type ThemeMode = "light" | "dark";
export const THEME_CHOICES: readonly ThemeChoice[] = ["auto", "light", "dark"];

const STORAGE_KEY = "af-theme";
const HEX = /^#[0-9A-Fa-f]{6}$/;
const BLACK = "#000000";
const WHITE = "#FFFFFF";

/** The built-in readable floor and the palette new daemons return by default. */
export const NORD_THEME: DaemonTheme = {
  name: "nord",
  foreground: "#D8DEE9",
  foreground_strong: "#ECEFF4",
  foreground_muted: "#C3CBD6",
  foreground_dim: "#A7B0BE",
  background: "#2E3440",
  background_subtle: "#3B4252",
  background_panel: "#434C5E",
  accent: "#88C0D0",
  success: "#A3BE8C",
  warning: "#EBCB8B",
  error: "#CC8A91",
  info: "#81A1C1",
  purple: "#B590AF",
  selection_background: "#4C566A",
  selection_foreground: "#ECEFF4",
  pane_border_default: "#4C566A",
  pane_border_selected: "#88C0D0",
  pane_border_interactive: "#A3BE8C",
  pane_border_preview: "#B48EAD",
};

type RGB = readonly [number, number, number];

function rgb(hex: string): RGB | null {
  if (!HEX.test(hex)) return null;
  return [
    Number.parseInt(hex.slice(1, 3), 16),
    Number.parseInt(hex.slice(3, 5), 16),
    Number.parseInt(hex.slice(5, 7), 16),
  ];
}

function hex([r, g, b]: RGB): string {
  return `#${[r, g, b].map((v) => Math.round(v).toString(16).padStart(2, "0")).join("")}`.toUpperCase();
}

function mix(a: string, b: string, amount: number): string {
  const aa = rgb(a);
  const bb = rgb(b);
  if (!aa || !bb) return NORD_THEME.foreground;
  return hex(aa.map((v, i) => v + (bb[i]! - v) * amount) as unknown as RGB);
}

function rgbToHSL(color: string): readonly [number, number, number] {
  const value = rgb(color) ?? rgb(NORD_THEME.accent)!;
  const [r, g, b] = value.map((part) => part / 255);
  const max = Math.max(r!, g!, b!);
  const min = Math.min(r!, g!, b!);
  const lightness = (max + min) / 2;
  if (max === min) return [0, 0, lightness];
  const delta = max - min;
  const saturation = lightness > 0.5 ? delta / (2 - max - min) : delta / (max + min);
  let hue = 0;
  if (max === r) hue = (g! - b!) / delta + (g! < b! ? 6 : 0);
  else if (max === g) hue = (b! - r!) / delta + 2;
  else hue = (r! - g!) / delta + 4;
  return [hue / 6, saturation, lightness];
}

function hslToHex(hue: number, saturation: number, lightness: number): string {
  if (saturation === 0) return hex([255 * lightness, 255 * lightness, 255 * lightness]);
  const q = lightness < 0.5 ? lightness * (1 + saturation) : lightness + saturation - lightness * saturation;
  const p = 2 * lightness - q;
  const channel = (offset: number): number => {
    let value = hue + offset;
    if (value < 0) value += 1;
    if (value > 1) value -= 1;
    if (value < 1 / 6) return p + (q - p) * 6 * value;
    if (value < 1 / 2) return q;
    if (value < 2 / 3) return p + (q - p) * (2 / 3 - value) * 6;
    return p;
  };
  return hex([255 * channel(1 / 3), 255 * channel(0), 255 * channel(-1 / 3)]);
}

function rgba(color: string, alpha: number): string {
  const value = rgb(color) ?? rgb(NORD_THEME.background)!;
  return `rgba(${value[0]}, ${value[1]}, ${value[2]}, ${alpha})`;
}

function luminance(color: string): number {
  const value = rgb(color);
  if (!value) return 0;
  const linear = value.map((part) => {
    const channel = part / 255;
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
}

/** WCAG relative-luminance contrast ratio, exported so tests measure the floor. */
export function contrastRatio(a: string, b: string): number {
  const first = luminance(a);
  const second = luminance(b);
  return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
}

function passes(color: string, backgrounds: readonly string[], minimum: number): boolean {
  return HEX.test(color) && backgrounds.every((background) => contrastRatio(color, background) >= minimum);
}

function shiftToContrast(color: string, backgrounds: readonly string[], minimum: number, toward: string): string {
  if (passes(color, backgrounds, minimum)) return color.toUpperCase();
  for (let step = 1; step <= 100; step++) {
    const candidate = mix(color, toward, step / 100);
    if (passes(candidate, backgrounds, minimum)) return candidate;
  }
  return color.toUpperCase();
}

// Adjust light-mode semantics along their own HSL lightness axis before using
// a neutral floor. Trying both directions preserves the configured hue even
// when a custom "light" surface system is darker than its semantic colours.
function adjustLightnessToContrast(
  color: string,
  backgrounds: readonly string[],
  minimum: number,
): string | null {
  if (passes(color, backgrounds, minimum)) return color.toUpperCase();
  const [hue, saturation, lightness] = rgbToHSL(color);
  for (let step = 1; step <= 100; step++) {
    const amount = step / 100;
    const darker = hslToHex(hue, saturation, lightness * (1 - amount));
    if (passes(darker, backgrounds, minimum)) return darker;
    const lighter = hslToHex(hue, saturation, lightness + (1 - lightness) * amount);
    if (passes(lighter, backgrounds, minimum)) return lighter;
  }
  return null;
}

function readable(
  candidate: string,
  backgrounds: readonly string[],
  minimum: number,
  fallback: string,
  toward: string,
): string {
  if (passes(candidate, backgrounds, minimum)) return candidate.toUpperCase();
  const safeFallback = shiftToContrast(fallback, backgrounds, minimum, toward);
  if (passes(safeFallback, backgrounds, minimum)) return safeFallback;
  for (const endpoint of [BLACK, WHITE]) {
    const shifted = shiftToContrast(fallback, backgrounds, minimum, endpoint);
    if (passes(shifted, backgrounds, minimum)) return shifted;
  }
  const minimumContrast = (color: string): number =>
    Math.min(...backgrounds.map((background) => contrastRatio(color, background)));
  return minimumContrast(BLACK) >= minimumContrast(WHITE) ? BLACK : WHITE;
}

const themeKeys = Object.keys(NORD_THEME).filter((key) => key !== "name") as (keyof Omit<DaemonTheme, "name">)[];

function normalizeTheme(value: Partial<DaemonTheme> | null | undefined): DaemonTheme {
  const out = { ...NORD_THEME, name: typeof value?.name === "string" ? value.name : undefined };
  for (const key of themeKeys) {
    const candidate = value?.[key];
    out[key] = typeof candidate === "string" && HEX.test(candidate) ? candidate.toUpperCase() : NORD_THEME[key];
  }
  return out;
}

export interface DerivedTheme {
  tokens: Record<string, string>;
  xterm: ITheme;
}

export interface LatestRequestGate {
  begin(): LatestRequest;
  invalidate(): void;
}

export interface LatestRequest {
  isCurrent(): boolean;
}

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

export interface PaletteFetchFailurePlan {
  reset: boolean;
  retry: boolean;
  reauthenticate: boolean;
}

/** Keeps a last-good daemon palette across transient additive-read failures. */
export function paletteFetchFailurePlan(status: number, hasLoadedPalette: boolean): PaletteFetchFailurePlan {
  const unsupported = status === 404 || status === 405 || status === 501;
  const rejectedCredential = status === 401 || status === 403;
  return {
    reset: unsupported || !hasLoadedPalette,
    retry: !unsupported && !rejectedCredential,
    reauthenticate: rejectedCredential,
  };
}

/** Fences asynchronous palette reads that share the same login token. */
export function createLatestRequestGate(): LatestRequestGate {
  let generation = 0;
  return {
    begin() {
      const requestGeneration = ++generation;
      return { isCurrent: () => requestGeneration === generation };
    },
    invalidate() {
      generation += 1;
    },
  };
}

function semantic(
  candidate: string,
  fallback: string,
  surfaces: readonly string[],
  minimum: number,
  toward: string,
): string {
  return readable(candidate, surfaces, minimum, fallback, toward);
}

/** Maps one daemon palette into every color-bearing web token for one mode. */
export function deriveTheme(input: Partial<DaemonTheme>, mode: ThemeMode): DerivedTheme {
  const source = normalizeTheme(input);
  const dark = mode === "dark";

  let canvas = dark ? source.background : source.foreground;
  let surface = dark ? source.background_subtle : mix(source.foreground, source.foreground_strong, 0.55);
  let raised = dark ? source.background_panel : source.foreground_strong;
  let inset = dark
    ? mix(source.background, source.background_subtle, 0.22)
    : mix(source.foreground, source.background, 0.06);
  let surfaces = [canvas, surface, inset, raised];
  const resetSurfaceSystem = (): void => {
    canvas = dark ? NORD_THEME.background : NORD_THEME.foreground;
    surface = dark
      ? NORD_THEME.background_subtle
      : mix(NORD_THEME.foreground, NORD_THEME.foreground_strong, 0.55);
    raised = dark ? NORD_THEME.background_panel : NORD_THEME.foreground_strong;
    inset = dark
      ? mix(NORD_THEME.background, NORD_THEME.background_subtle, 0.22)
      : mix(NORD_THEME.foreground, NORD_THEME.background, 0.06);
    surfaces = [canvas, surface, inset, raised];
  };

  // A custom table can make its three elevations mutually incompatible with
  // one shared text colour (for example black, white, black). In that case the
  // safe unit is the surface SYSTEM, not an arbitrary single slot: retain the
  // user's chromatic roles but restore Nord's coherent elevation floor.
  const sourceSurfaceText = dark ? source.foreground : source.background;
  const fallbackSurfaceText = dark ? NORD_THEME.foreground : NORD_THEME.background;
  if (!passes(sourceSurfaceText, surfaces, 4.5) && !passes(fallbackSurfaceText, surfaces, 4.5)) {
    resetSurfaceSystem();
  }
  const toward = dark ? source.foreground_strong : source.background;
  const subtleAlpha = dark ? 0.12 : 0.09;
  const tintAlpha = dark ? 0.2 : 0.16;
  const provisionalText = readable(
    dark ? source.foreground : source.background,
    surfaces,
    4.5,
    fallbackSurfaceText,
    toward,
  );
  const provisionalSemantic = (candidate: string, fallback: string): string => {
    if (dark) return semantic(candidate, fallback, surfaces, 4.5, toward);
    if (passes(candidate, surfaces, 4.5)) return candidate.toUpperCase();
    return (
      adjustLightnessToContrast(fallback, surfaces, 4.5) ??
      readable(fallback, surfaces, 4.5, provisionalText, provisionalText)
    );
  };
  const hasSharedFillText = (color: string, alphas: readonly number[]): boolean => {
    const fills = surfaces.flatMap((background) => alphas.map((alpha) => mix(background, color, alpha)));
    // WCAG contrast depends only on relative luminance. If neither luminance
    // endpoint can cover every fill, no intermediate foreground can do so.
    return passes(BLACK, fills, 4.5) || passes(WHITE, fills, 4.5);
  };
  const provisionalAccent = provisionalSemantic(source.accent, NORD_THEME.accent);
  const provisionalDanger = provisionalSemantic(source.error, NORD_THEME.error);
  if (
    !hasSharedFillText(provisionalAccent, [subtleAlpha, tintAlpha]) ||
    !hasSharedFillText(provisionalDanger, [subtleAlpha])
  ) {
    resetSurfaceSystem();
  }
  const text = readable(
    dark ? source.foreground : source.background,
    surfaces,
    4.5,
    dark ? NORD_THEME.foreground : NORD_THEME.background,
    toward,
  );
  const text2 = readable(
    dark ? source.foreground_muted : source.background_subtle,
    surfaces,
    4.5,
    dark ? NORD_THEME.foreground_muted : NORD_THEME.background_subtle,
    toward,
  );
  const text3 = readable(
    dark ? source.foreground_dim : source.background_panel,
    surfaces,
    4.5,
    dark ? NORD_THEME.foreground_dim : NORD_THEME.background_panel,
    toward,
  );

  const modeSemantic = (candidate: string, fallback: string, minimum: number): string => {
    if (dark) return semantic(candidate, fallback, surfaces, minimum, toward);
    if (passes(candidate, surfaces, minimum)) return candidate.toUpperCase();
    const adjusted = adjustLightnessToContrast(fallback, surfaces, minimum);
    if (adjusted && passes(adjusted, surfaces, minimum)) return adjusted;
    return readable(fallback, surfaces, minimum, text, text);
  };

  const accent = modeSemantic(source.accent, NORD_THEME.accent, 4.5);
  const danger = modeSemantic(source.error, NORD_THEME.error, 4.5);
  const ready = modeSemantic(source.success, NORD_THEME.success, 3);
  const statusNeedsYou = modeSemantic(source.success, NORD_THEME.success, 4.5);
  const lost = modeSemantic(source.warning, NORD_THEME.warning, 3);
  const limit = modeSemantic(source.error, NORD_THEME.error, 3);
  const dead = semantic(text2, NORD_THEME.foreground_muted, surfaces, 3, toward);
  const termColor = (candidate: string, fallback: string): string =>
    dark
      ? semantic(candidate, fallback, [canvas], 4.5, toward)
      : passes(candidate, [canvas], 4.5)
        ? candidate.toUpperCase()
        : (adjustLightnessToContrast(fallback, [canvas], 4.5) ?? readable(fallback, [canvas], 4.5, text, text));
  const termGreen = termColor(source.success, NORD_THEME.success);
  const termAmber = termColor(source.warning, NORD_THEME.warning);
  const termBlue = termColor(source.info, NORD_THEME.info);
  const border = semantic(source.pane_border_default, NORD_THEME.pane_border_default, surfaces, 3, toward);
  const borderSelected = modeSemantic(source.pane_border_selected, NORD_THEME.pane_border_selected, 3);
  const borderInteractive = modeSemantic(source.pane_border_interactive, NORD_THEME.pane_border_interactive, 3);
  const borderPreview = modeSemantic(source.pane_border_preview, NORD_THEME.pane_border_preview, 3);

  const onAccentCandidates = [source.selection_foreground, source.background, text, BLACK, WHITE];
  const onAccent = onAccentCandidates.find((candidate) => passes(candidate, [accent], 4.5)) ?? text;
  const hoverToward = luminance(onAccent) > luminance(accent) ? BLACK : WHITE;
  const hoverCandidate = mix(accent, hoverToward, 0.12);
  const accentHover = passes(onAccent, [hoverCandidate], 4.5) ? hoverCandidate : accent;

  const semanticFillText = (candidate: string, fillSurfaces: readonly string[]): string => {
    const adjusted = adjustLightnessToContrast(candidate, fillSurfaces, 4.5);
    return adjusted ?? readable(candidate, fillSurfaces, 4.5, text, toward);
  };
  const accentFillSurfaces = surfaces.flatMap((background) => [
    mix(background, accent, subtleAlpha),
    mix(background, accent, tintAlpha),
  ]);
  const accentText = semanticFillText(accent, accentFillSurfaces);
  const selectedText = semanticFillText(text, accentFillSurfaces);
  const selectedTextMuted = semanticFillText(text2, accentFillSurfaces);
  const selectedStatusNeedsYou = semanticFillText(statusNeedsYou, accentFillSurfaces);
  const selectedStatusWorking = semanticFillText(text2, accentFillSurfaces);
  const selectedStatusWaiting = semanticFillText(danger, accentFillSurfaces);
  const selectedStatusBroken = semanticFillText(danger, accentFillSurfaces);
  const selectedStatusInactive = semanticFillText(text2, accentFillSurfaces);
  const dangerFillSurfaces = surfaces.map((background) => mix(background, danger, subtleAlpha));
  const dangerText = semanticFillText(danger, dangerFillSurfaces);
  // Elevation effects must follow the surface system that survived validation.
  // Dark shadows use the accepted canvas; light shadows use its accepted dark text
  // endpoint. Reading source.background here would reintroduce a rejected custom
  // surface after the coherent-system fallback above.
  const effectBase = dark ? canvas : text;

  const selectionAlpha = dark ? 0.72 : 0.45;
  const selectionSurface = mix(canvas, source.selection_background, selectionAlpha);
  const selectionForeground = readable(
    source.selection_foreground,
    [selectionSurface],
    4.5,
    NORD_THEME.selection_foreground,
    text,
  );

  const tokens: Record<string, string> = {
    "--af-bg-canvas": canvas,
    "--af-bg-surface": surface,
    "--af-bg-inset": inset,
    "--af-bg-raised": raised,
    "--af-bg-term": canvas,
    "--af-border": border,
    "--af-border-subtle": mix(surface, border, 0.35),
    "--af-border-strong": border,
    "--af-border-selected": borderSelected,
    "--af-border-interactive": borderInteractive,
    "--af-border-preview": borderPreview,
    "--af-text": text,
    "--af-text-2": text2,
    "--af-text-3": text3,
    "--af-accent": accent,
    "--af-accent-text": accentText,
    "--af-accent-hover": accentHover,
    "--af-accent-subtle": rgba(accent, subtleAlpha),
    "--af-accent-tint": rgba(accent, tintAlpha),
    "--af-on-accent": onAccent,
    "--af-danger": danger,
    "--af-danger-text": dangerText,
    "--af-danger-subtle": rgba(danger, subtleAlpha),
    "--af-focus-ring": accent,
    "--af-text-muted": text2,
    "--af-status-needs-you": statusNeedsYou,
    "--af-status-working": text2,
    "--af-status-waiting": danger,
    "--af-status-broken": danger,
    "--af-status-inactive": text2,
    "--af-selected-text": selectedText,
    "--af-selected-text-muted": selectedTextMuted,
    "--af-selected-status-needs-you": selectedStatusNeedsYou,
    "--af-selected-status-working": selectedStatusWorking,
    "--af-selected-status-waiting": selectedStatusWaiting,
    "--af-selected-status-broken": selectedStatusBroken,
    "--af-selected-status-inactive": selectedStatusInactive,
    "--af-dot-ready": ready,
    "--af-dot-lost": lost,
    "--af-dot-dead": dead,
    "--af-dot-archived": dead,
    "--af-dot-limit": limit,
    "--af-term-green": termGreen,
    "--af-term-amber": termAmber,
    "--af-term-blue": termBlue,
    "--af-term-dim": text3,
    "--af-shadow-1": `0 1px 2px ${rgba(effectBase, dark ? 0.4 : 0.08)}`,
    "--af-shadow-2": `0 4px 10px ${rgba(effectBase, dark ? 0.45 : 0.12)}`,
    "--af-shadow-overlay": `0 16px 48px ${rgba(effectBase, dark ? 0.6 : 0.22)}`,
    "--af-backdrop": rgba(effectBase, dark ? 0.66 : 0.42),
  };

  const ansiDistinct = (color: string, avoid: readonly string[]): string => {
    const normalized = color.toUpperCase();
    const rejected = new Set(avoid.map((value) => value.toUpperCase()));
    if (!rejected.has(normalized) && passes(normalized, [canvas], 4.5)) return normalized;

    // A canvas near the WCAG midpoint can leave only a narrow readable band at
    // one endpoint. Choose the candidate with the largest minimum RGB distance
    // from every occupied role, so that narrow band is used rather than taking
    // the first one-codepoint difference and calling it distinct.
    let best = normalized;
    let bestDistance = -1;
    for (const endpoint of dark ? [WHITE, BLACK] : [BLACK, WHITE]) {
      for (let step = 1; step <= 255; step++) {
        const candidate = mix(normalized, endpoint, step / 255);
        if (rejected.has(candidate) || !passes(candidate, [canvas], 4.5)) continue;
        const value = rgb(candidate)!;
        const distance = Math.min(
          ...avoid.map((occupied) => {
            const other = rgb(occupied)!;
            return value.reduce((sum, channel, index) => sum + (channel - other[index]!) ** 2, 0);
          }),
        );
        if (distance > bestDistance) {
          best = candidate;
          bestDistance = distance;
        }
      }
    }
    return best;
  };
  const bright = (color: string): string => {
    for (const endpoint of dark ? [WHITE, BLACK] : [BLACK, WHITE]) {
      for (let step = 18; step <= 100; step++) {
        const candidate = mix(color, endpoint, step / 100);
        if (candidate !== color.toUpperCase() && passes(candidate, [canvas], 4.5)) return candidate;
      }
    }
    return color.toUpperCase();
  };
  // ANSI names describe terminal roles, not permission to disappear into the
  // canvas. Use the already-accepted primary/tertiary text endpoints, swapping
  // their intensity by mode so black and white both remain AA on the terminal.
  const ansiBlack = dark ? text3 : text;
  const ansiWhite = ansiDistinct(dark ? text : text3, [ansiBlack]);
  const brightBlack = ansiDistinct(ansiBlack, [ansiBlack, ansiWhite]);
  const brightWhite = ansiDistinct(ansiWhite, [ansiWhite, ansiBlack, brightBlack]);
  const xterm: ITheme = {
    background: tokens["--af-bg-term"],
    foreground: text,
    cursor: text,
    cursorAccent: canvas,
    selectionBackground: rgba(source.selection_background, selectionAlpha),
    selectionForeground,
    black: ansiBlack,
    red: danger,
    green: termGreen,
    yellow: termAmber,
    blue: termBlue,
    magenta: termColor(source.purple, NORD_THEME.purple),
    cyan: accent,
    white: ansiWhite,
    brightBlack,
    brightRed: bright(danger),
    brightGreen: bright(termGreen),
    brightYellow: bright(termAmber),
    brightBlue: bright(termBlue),
    brightMagenta: bright(termColor(source.purple, NORD_THEME.purple)),
    brightCyan: bright(accent),
    brightWhite,
  };
  return { tokens, xterm };
}

let activeThemes = {
  light: deriveTheme(NORD_THEME, "light"),
  dark: deriveTheme(NORD_THEME, "dark"),
};

function isChoice(value: unknown): value is ThemeChoice {
  return value === "auto" || value === "light" || value === "dark";
}

export function readThemeChoice(): ThemeChoice {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (isChoice(raw)) return raw;
  } catch {
    // A blocked store keeps the session choice but cannot persist it.
  }
  return "auto";
}

export function persistThemeChoice(choice: ThemeChoice): void {
  try {
    localStorage.setItem(STORAGE_KEY, choice);
  } catch {
    // Best effort.
  }
}

function prefersDark(): boolean {
  try {
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  } catch {
    return false;
  }
}

export function currentMode(): ThemeMode {
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "light" || attr === "dark") return attr;
  return prefersDark() ? "dark" : "light";
}

function applyCurrentMode(): void {
  const root = document.documentElement;
  for (const [name, value] of Object.entries(activeThemes[currentMode()].tokens)) root.style.setProperty(name, value);
}

export function themeColorMetaContents(choice: ThemeChoice): { light: string; dark: string } {
  const light = activeThemes.light.tokens["--af-bg-surface"]!;
  const dark = activeThemes.dark.tokens["--af-bg-surface"]!;
  if (choice === "auto") return { light, dark };
  const forced = choice === "dark" ? dark : light;
  return { light: forced, dark: forced };
}

function syncThemeColorMeta(choice: ThemeChoice): void {
  const colors = themeColorMetaContents(choice);
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    meta.setAttribute("content", (meta.getAttribute("media") ?? "").includes("dark") ? colors.dark : colors.light);
  }
}

export function refreshThemeMode(): void {
  applyCurrentMode();
  syncThemeColorMeta(document.documentElement.hasAttribute("data-theme") ? currentMode() : "auto");
}

/** Installs a newly fetched daemon palette without changing the local mode choice. */
export function applyDaemonTheme(theme: Partial<DaemonTheme>): void {
  activeThemes = { light: deriveTheme(theme, "light"), dark: deriveTheme(theme, "dark") };
  refreshThemeMode();
}

export function resetDaemonTheme(): void {
  applyDaemonTheme(NORD_THEME);
}

export function stampTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === "auto") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
  applyCurrentMode();
  syncThemeColorMeta(choice);
}

export function bootStampTheme(): ThemeChoice {
  const choice = readThemeChoice();
  stampTheme(choice);
  return choice;
}

export function xtermTheme(mode: ThemeMode): ITheme {
  return activeThemes[mode].xterm;
}

export function currentXtermTheme(): ITheme {
  return xtermTheme(currentMode());
}
