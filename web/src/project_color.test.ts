import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import type { RegisteredProject } from "./api.js";
import {
  FAVICON_MARK_PATH,
  PROJECT_COLOR_PALETTE,
  markInk,
  projectColor,
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
