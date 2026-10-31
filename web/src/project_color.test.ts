import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import type { RegisteredProject } from "./api.js";
import {
  DEFAULT_FAVICON_COLOR,
  FAVICON_MARK_PATH,
  PROJECT_COLOR_PALETTE,
  applyProjectFavicon,
  markInk,
  projectAccent,
  projectColor,
  projectColorRow,
  projectFaviconDataUrl,
  projectFaviconSvg,
} from "./project_color.js";

const record = (root: string, color?: string): RegisteredProject => ({
  id: `prj_${"0".repeat(32)}`,
  checkout_id: `chk_${"0".repeat(32)}`,
  root,
  relative_root: ".",
  path_exists: true,
  ...(color === undefined ? {} : { color }),
});

test("the favicon mark matches icons/icon.svg", () => {
  const svg = readFileSync(fileURLToPath(new URL("./icons/icon.svg", import.meta.url)), "utf8");
  assert.ok(svg.includes(`d="${FAVICON_MARK_PATH}"`));
  assert.ok(svg.includes(`fill="${DEFAULT_FAVICON_COLOR}"`));
  assert.equal(markInk(DEFAULT_FAVICON_COLOR), "#ffffff");
});

test("projectColor reads the selected project's valid color only", () => {
  const projects = [record("/a", "#E11D48"), record("/b"), record("/c", "red")];
  assert.equal(projectColor(projects, "/a"), "#e11d48");
  assert.equal(projectColor(projects, "/b"), null);
  assert.equal(projectColor(projects, "/c"), null);
  assert.equal(projectColor(projects, "/missing"), null);
  assert.equal(projectColor(projects, null), null);
});

test("markInk keeps the mark legible on light and dark tiles", () => {
  assert.equal(markInk("#2f5fd8"), "#ffffff");
  assert.equal(markInk("#000000"), "#ffffff");
  assert.equal(markInk("#ffffff"), "#1f1f1f");
  assert.equal(markInk("#fde047"), "#1f1f1f");
  // Mid-light tiles can favour the dark ink where a fixed luminance threshold
  // picks white: on #ca8a04 dark reads ~5.6:1 against white's ~2.9:1.
  assert.equal(markInk("#ca8a04"), "#1f1f1f");
});

test("the favicon SVG paints the tile in the project color", () => {
  const svg = projectFaviconSvg("#16a34a");
  assert.ok(svg.includes('<rect width="512" height="512" rx="112" fill="#16a34a"/>'));
  assert.ok(svg.includes(`<path d="${FAVICON_MARK_PATH}"`));
  assert.ok(projectFaviconDataUrl("#16a34a").startsWith("data:image/svg+xml,"));
});

test("every palette color is a lowercase #rrggbb", () => {
  for (const c of PROJECT_COLOR_PALETTE) assert.match(c, /^#[0-9a-f]{6}$/);
});

test("every color control carries a project-focus key so a menu rebuild keeps focus", () => {
  class El {
    dataset: Record<string, string> = {};
    children: El[] = [];
    style = { setProperty: () => {} };
    setAttribute() {}
    addEventListener() {}
    append(child: El | string) {
      if (typeof child !== "string") this.children.push(child);
    }
  }
  const g = globalThis as Record<string, unknown>;
  const saved = g.document;
  g.document = { createElement: () => new El() };
  try {
    const row = projectColorRow("#16a34a", "todo-cli", () => {}) as unknown as El;
    const keys = row.children.map((c) => c.dataset.projectFocus).filter((k) => k !== undefined);
    assert.deepEqual(keys, [...PROJECT_COLOR_PALETTE.map((c) => `color:${c}`), "color:custom", "color:clear"]);
  } finally {
    g.document = saved;
  }
});

test("the PNG favicon renders without roundRect and redraws the default mark on reset", () => {
  const calls: string[] = [];
  const ctx = {
    fillStyle: "",
    scale: () => {},
    beginPath: () => {},
    moveTo: () => calls.push("moveTo"),
    arcTo: () => calls.push("arcTo"),
    closePath: () => {},
    fill: () => {},
  };
  const canvas = { width: 0, height: 0, getContext: () => ctx, toDataURL: () => "data:image/png;base64,TILE" };
  const attrs = new Map<string, string>([["href", "/icons/favicon-32.png"]]);
  const link = {
    type: "image/png",
    sizes: { value: "32" },
    dataset: {} as Record<string, string>,
    getAttribute: (k: string) => attrs.get(k) ?? null,
    setAttribute: (k: string, v: string) => void attrs.set(k, v),
  };
  const g = globalThis as Record<string, unknown>;
  const saved = { document: g.document, Path2D: g.Path2D };
  g.document = { createElement: () => canvas, querySelectorAll: () => [link] };
  g.Path2D = class {};
  try {
    applyProjectFavicon("#16a34a");
    assert.equal(attrs.get("href"), "data:image/png;base64,TILE");
    assert.ok(calls.includes("arcTo"));
    const drawn = calls.length;
    attrs.set("href", "/icons/favicon-32.png");
    applyProjectFavicon(null);
    assert.equal(attrs.get("href"), "data:image/png;base64,TILE");
    assert.ok(calls.length > drawn);
  } finally {
    g.document = saved.document;
    g.Path2D = saved.Path2D;
  }
});

test("projectAccent keeps 4.5:1 contrast against each theme surface", () => {
  assert.equal(projectAccent("#ff79c6", "#1f1f1f"), "#ff79c6");
  const light = projectAccent("#f1fa8c", "#f8f9fc");
  assert.notEqual(light, "#f1fa8c");
  assert.match(light, /^#[0-9a-f]{6}$/);
  assert.equal(markInk(light), "#ffffff");
  assert.equal(markInk("#bd93f9"), "#1f1f1f");
});

function contrastRatio(a: string, b: string): number {
  const lum = (c: string): number => {
    const [r, g, bl] = [1, 3, 5].map((i) => {
      const x = parseInt(c.slice(i, i + 2), 16) / 255;
      return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * r + 0.7152 * g + 0.0722 * bl;
  };
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

test("markInk reaches 4.5:1 on every color, including the mid-luminance band", () => {
  for (const color of ["#6c8818", "#777777", "#808080", "#2f5fd8", "#f1fa8c", "#000000", "#ffffff"]) {
    assert.ok(contrastRatio(markInk(color), color) >= 4.5, `${color} → ${markInk(color)}`);
  }
});

test("projectAccent reaches 4.5:1 against every surface it is given, after rounding", () => {
  const accent = projectAccent("#2a695e", "#1f1f1f", "#2b2b2b");
  assert.ok(contrastRatio(accent, "#1f1f1f") >= 4.5, accent);
  assert.ok(contrastRatio(accent, "#2b2b2b") >= 4.5, accent);
  const light = projectAccent("#f1fa8c", "#f8f9fc", "#eceff4");
  assert.ok(contrastRatio(light, "#f8f9fc") >= 4.5, light);
  assert.ok(contrastRatio(light, "#eceff4") >= 4.5, light);
});

test("app bar focus rings take the project ink", () => {
  const css = readFileSync(fileURLToPath(new URL("./styles.css", import.meta.url)), "utf8");
  assert.match(css, /\.af-app\[data-project-color\] \.af-appbar :is\(button, summary, a\):focus-visible \{\s*outline-color: var\(--af-project-ink\);/);
  assert.match(css, /\.af-app\[data-project-color\] \.af-appbar-tools :is\(button, summary, a\):focus-visible \{\s*outline-color: var\(--af-accent\);/);
});

test("the phone tools popover restores theme ink over the colored app bar's ink", () => {
  const css = readFileSync(fileURLToPath(new URL("./styles.css", import.meta.url)), "utf8");
  assert.match(
    css,
    /@media \(max-width: 768px\) \{\s*\.af-app\[data-project-color\] \.af-appbar-tools \{\s*color: var\(--af-ink\);/,
  );
});

test("body-mounted controls take the project accent too", () => {
  const css = readFileSync(fileURLToPath(new URL("./styles.css", import.meta.url)), "utf8");
  assert.match(css, /body\[data-project-color\] \[data-af-theme="light"\] \{\s*--af-accent: var\(--af-project-accent-light\);/);
  assert.match(css, /body\[data-project-color\] \[data-af-theme="dark"\] \{\s*--af-accent: var\(--af-project-accent-dark\);/);
});
