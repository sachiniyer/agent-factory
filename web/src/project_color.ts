// Per-project display color: the selected project's color replaces the theme accent,
// fills the app bar, and paints the browser tab's favicon, so a project's tab or
// window is easy to find among many. The color itself lives in the daemon's project registry
// (`af projects color`, SetProjectColor); this module only paints it.

import { h } from "./dom.js";
import type { RegisteredProject } from "./api.js";

/** Preset swatches offered in the project menu. Any #rrggbb is accepted. */
export const PROJECT_COLOR_PALETTE: readonly string[] = [
  "#ff5555",
  "#ffb86c",
  "#f1fa8c",
  "#50fa7b",
  "#8be9fd",
  "#bd93f9",
  "#ff79c6",
];

/** The app mark, byte-identical to icons/icon.svg (project_color.test.ts pins it). */
export const FAVICON_MARK_PATH =
  "M96 416 L96 96 L160 96 L160 288 L224 288 L224 192 L320 288 L320 192 L416 288 L416 416 Z";

const HEX = /^#[0-9a-f]{6}$/;

/** The color of the project at root, or null when it has none (or is unregistered). */
export function projectColor(projects: readonly RegisteredProject[], root: string | null): string | null {
  if (root === null) return null;
  const color = projects.find((p) => p.root === root)?.color?.toLowerCase() ?? "";
  return HEX.test(color) ? color : null;
}

type Rgb = readonly [number, number, number];

function rgb(color: string): Rgb {
  return [1, 3, 5].map((i) => parseInt(color.slice(i, i + 2), 16)) as unknown as Rgb;
}

function hex(c: Rgb): string {
  return `#${c.map((v) => Math.round(v).toString(16).padStart(2, "0")).join("")}`;
}

function luminance(c: Rgb): number {
  const [r, g, b] = c.map((v) => {
    const x = v / 255;
    return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: Rgb, b: Rgb): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const INK_DARK = "#1f1f1f";
const INK_LIGHT = "#ffffff";

/** Whichever of near-black or white has more contrast on color. In the
 *  mid-luminance band where neither reaches 4.5:1, true black or white does. */
export function markInk(color: string): string {
  const c = rgb(color);
  const dark = contrast(c, rgb(INK_DARK));
  const light = contrast(c, rgb(INK_LIGHT));
  if (Math.max(dark, light) >= 4.5) return dark >= light ? INK_DARK : INK_LIGHT;
  return contrast(c, [0, 0, 0]) >= light ? "#000000" : INK_LIGHT;
}

/** color, moved toward black or white until it reaches 4.5:1 against every surface
 *  it can sit on, so it stays legible as accent text there and as a fill behind
 *  surface text. The surfaces share one theme, so the first sets the direction. */
export function projectAccent(color: string, ...surfaces: [string, ...string[]]): string {
  const c = rgb(color);
  const s = surfaces.map(rgb);
  const target: Rgb = luminance(s[0]) > 0.5 ? [0, 0, 0] : [255, 255, 255];
  for (let step = 0; step <= 20; step++) {
    const t = step / 20;
    const candidate = hex(c.map((v, i) => v + (target[i] - v) * t) as unknown as Rgb);
    if (s.every((surface) => contrast(rgb(candidate), surface) >= 4.5)) return candidate;
  }
  return hex(target);
}

/** The favicon SVG with its tile painted in color. */
export function projectFaviconSvg(color: string): string {
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512">` +
    `<rect width="512" height="512" rx="112" fill="${color}"/>` +
    `<path d="${FAVICON_MARK_PATH}" fill="${markInk(color)}"/></svg>`
  );
}

export function projectFaviconDataUrl(color: string): string {
  return `data:image/svg+xml,${encodeURIComponent(projectFaviconSvg(color))}`;
}

/** Renders the same tile to a PNG for browsers (Safari) that read only the PNG
 *  favicons. Null where no canvas is available. */
function projectFaviconPng(color: string, size: number): string | null {
  const canvas = document.createElement("canvas");
  canvas.width = size;
  canvas.height = size;
  const ctx = canvas.getContext("2d");
  if (!ctx || typeof Path2D === "undefined") return null;
  ctx.scale(size / 512, size / 512);
  ctx.fillStyle = color;
  // arcTo, not roundRect, which older Safari lacks.
  ctx.beginPath();
  ctx.moveTo(112, 0);
  ctx.arcTo(512, 0, 512, 512, 112);
  ctx.arcTo(512, 512, 0, 512, 112);
  ctx.arcTo(0, 512, 0, 0, 112);
  ctx.arcTo(0, 0, 512, 0, 112);
  ctx.closePath();
  ctx.fill();
  ctx.fillStyle = markInk(color);
  ctx.fill(new Path2D(FAVICON_MARK_PATH));
  return canvas.toDataURL("image/png");
}

/** The tile color of icons/icon.svg (project_color.test.ts pins it). */
export const DEFAULT_FAVICON_COLOR = "#2f5fd8";

let appliedFavicon: string | null = null;

/** Points every favicon link at the project-colored mark, or back at the default
 *  mark when color is null. Writes only on a change. */
export function applyProjectFavicon(color: string | null): void {
  if (appliedFavicon === color) return;
  appliedFavicon = color;
  // Chrome keeps showing the last data: favicon when a link's href returns to a
  // URL it already loaded, so the default is redrawn as a data: URL too.
  const tile = color ?? DEFAULT_FAVICON_COLOR;
  for (const link of document.querySelectorAll<HTMLLinkElement>('link[rel="icon"]')) {
    if (link.dataset.afDefaultHref === undefined) link.dataset.afDefaultHref = link.getAttribute("href") ?? "";
    const href =
      link.type === "image/svg+xml"
        ? projectFaviconDataUrl(tile)
        : projectFaviconPng(tile, Number.parseInt(link.sizes.value, 10) || 32);
    link.setAttribute("href", href ?? link.dataset.afDefaultHref);
  }
}

/** Sets the project color on the app root, where CSS turns it into the accent for
 *  both themes and the app bar fill, or removes it. surfaces are the light and dark
 *  theme surfaces the accent must stay legible against. */
export function applyProjectColor(
  root: HTMLElement,
  color: string | null,
  surfaces: () => { light: readonly string[]; dark: readonly string[] },
): void {
  if (color === null) {
    if (root.dataset.projectColor === undefined) return;
    delete root.dataset.projectColor;
    for (const name of PROJECT_VARS) root.style.removeProperty(name);
    return;
  }
  if (root.dataset.projectColor === color) return;
  const { light, dark } = surfaces();
  root.dataset.projectColor = color;
  root.style.setProperty("--af-project-color", color);
  root.style.setProperty("--af-project-ink", markInk(color));
  root.style.setProperty("--af-project-accent-light", accentOn(color, light));
  root.style.setProperty("--af-project-accent-dark", accentOn(color, dark));
}

function accentOn(color: string, surfaces: readonly string[]): string {
  const [first, ...rest] = surfaces.filter((s) => HEX.test(s));
  return first === undefined ? color : projectAccent(color, first, ...rest);
}

const PROJECT_VARS = ["--af-project-color", "--af-project-ink", "--af-project-accent-light", "--af-project-accent-dark"];

/** The project menu's color row: the preset swatches, a custom picker, and a clear
 *  button. Each choice reports through onPick; "" clears. */
export function projectColorRow(current: string | null, label: string, onPick: (color: string) => void): HTMLElement {
  const pick = (color: string) => (e: Event): void => {
    e.stopPropagation();
    onPick(color);
  };
  const swatches = PROJECT_COLOR_PALETTE.map((color) => {
    const swatch = h("button", { type: "button", class: "af-project-swatch" });
    swatch.style.setProperty("--af-swatch", color);
    swatch.setAttribute("aria-label", `Color ${label} ${color}`);
    swatch.setAttribute("title", color);
    swatch.setAttribute("aria-pressed", current === color ? "true" : "false");
    swatch.dataset.projectFocus = `color:${color}`;
    swatch.addEventListener("click", pick(color));
    return swatch;
  });
  const input = h("input", { type: "color", class: "af-project-color-input", tabIndex: -1 });
  input.value = current ?? PROJECT_COLOR_PALETTE[5];
  input.setAttribute("aria-hidden", "true");
  input.addEventListener("click", (e) => e.stopPropagation());
  input.addEventListener("change", () => onPick(input.value));
  const custom = h("button", { type: "button", class: "af-project-swatch af-project-swatch-custom" }, "+");
  custom.setAttribute("aria-label", `Custom color for ${label}`);
  custom.setAttribute("title", "Custom color");
  custom.dataset.projectFocus = "color:custom";
  custom.addEventListener("click", (e) => {
    e.stopPropagation();
    try {
      input.showPicker();
    } catch {
      input.click();
    }
  });
  const clear = h("button", { type: "button", class: "af-ghost af-project-color-clear" }, "Default");
  clear.setAttribute("aria-label", `Default color for ${label}`);
  clear.disabled = current === null;
  clear.dataset.projectFocus = "color:clear";
  clear.addEventListener("click", pick(""));
  const row = h(
    "div",
    { class: "af-project-color" },
    h("span", { class: "af-project-color-label" }, "Color"),
    ...swatches,
    custom,
    input,
    clear,
  );
  row.setAttribute("role", "group");
  row.setAttribute("aria-label", `Color for ${label}`);
  return row;
}
