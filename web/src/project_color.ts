// Per-project display color: the frame around the session pane and the browser
// tab's favicon take the selected project's color, so a project's tab or window is
// easy to find among many. The color itself lives in the daemon's project registry
// (`af projects color`, SetProjectColor); this module only paints it.

import { h } from "./dom.js";
import type { RegisteredProject } from "./api.js";

/** Preset swatches offered in the project menu. Any #rrggbb is accepted. */
export const PROJECT_COLOR_PALETTE: readonly string[] = [
  "#e11d48",
  "#ea580c",
  "#ca8a04",
  "#16a34a",
  "#0d9488",
  "#2563eb",
  "#7c3aed",
  "#db2777",
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

/** White on a dark tile, near-black on a light one — whichever reads better. */
export function markInk(color: string): string {
  const channel = (i: number): number => {
    const v = parseInt(color.slice(i, i + 2), 16) / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  const luminance = 0.2126 * channel(1) + 0.7152 * channel(3) + 0.0722 * channel(5);
  // WCAG contrast of each candidate ink against the tile; #1f1f1f's own
  // luminance is ~0.014, white's 1.0. Mid-light colors can favour the dark ink
  // where a fixed luminance threshold would pick white (#ca8a04: ~5.6 vs ~2.9).
  const darkInkContrast = (luminance + 0.05) / (0.014 + 0.05);
  const whiteInkContrast = 1.05 / (luminance + 0.05);
  return darkInkContrast >= whiteInkContrast ? "#1f1f1f" : "#ffffff";
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

let appliedFavicon: string | null = null;

/** Points every favicon link at the project-colored mark, or back at the static
 *  icons when color is null. Writes only on a change. */
export function applyProjectFavicon(color: string | null): void {
  if (appliedFavicon === color) return;
  appliedFavicon = color;
  for (const link of document.querySelectorAll<HTMLLinkElement>('link[rel="icon"]')) {
    if (link.dataset.afDefaultHref === undefined) link.dataset.afDefaultHref = link.getAttribute("href") ?? "";
    let href: string | null = null;
    if (color !== null) {
      if (link.type === "image/svg+xml") {
        href = projectFaviconDataUrl(color);
      } else {
        href = projectFaviconPng(color, Number.parseInt(link.sizes.value, 10) || 32);
      }
    }
    link.setAttribute("href", href ?? link.dataset.afDefaultHref);
  }
}

/** Paints the session-pane frame with color, or removes it. */
export function applyProjectFrame(main: HTMLElement, color: string | null): void {
  if (color === null) {
    if (main.dataset.projectColor === undefined) return;
    delete main.dataset.projectColor;
    main.style.removeProperty("--af-project-color");
    return;
  }
  if (main.dataset.projectColor === color) return;
  main.dataset.projectColor = color;
  main.style.setProperty("--af-project-color", color);
}

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
  const clear = h("button", { type: "button", class: "af-ghost af-project-color-clear" }, "No color");
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
