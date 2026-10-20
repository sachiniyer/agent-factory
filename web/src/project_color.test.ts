import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import type { RegisteredProject } from "./api.js";
import {
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

test("the PNG favicon renders without roundRect and resets to the static icon", () => {
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
    applyProjectFavicon(null);
    assert.equal(attrs.get("href"), "/icons/favicon-32.png");
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
