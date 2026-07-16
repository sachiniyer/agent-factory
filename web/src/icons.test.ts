// Unit coverage for the icon set (feat: PWA).
//
// The PNGs are rendered out of band by `npm run icons` and committed, which buys a
// Chromium-free deterministic build but costs the guarantee that they match their
// sources — nothing in `npm run build` would notice an SVG edited without a regen, or
// a manifest entry pointing at a size that was never rendered. These tests are that
// guarantee: they pin the set against its two real consumers (index.html and the
// manifest) and pin each file's actual pixels against the size it claims.
//
// What is deliberately NOT here: how the mark LOOKS. That is a judgement call made by
// rendering it at 16px and looking (see icon.svg's header), and no assertion is going
// to catch a mark that reads as an arrow instead of a factory.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const read = (rel: string): Buffer => readFileSync(fileURLToPath(new URL(rel, import.meta.url)));
const readText = (rel: string): string => read(rel).toString("utf8");

/** The three icon SVG sources. All must carry the same mark. */
const SVG_SOURCES = ["./icons/icon.svg", "./icons/icon-fullbleed.svg", "./icons/icon-maskable.svg"];

/** Pulls every `d="…"` out of an SVG. */
function pathData(svg: string): string[] {
  return [...svg.matchAll(/\sd="([^"]+)"/g)].map((m) => m[1]);
}

/**
 * The pixel dimensions in a PNG's IHDR.
 *
 * A PNG is an 8-byte signature, then a chunk of 4-byte length + 4-byte type, then
 * the chunk body. IHDR is required to come first, and its first 8 bytes are
 * width/height as big-endian uint32 — so they sit at a fixed 16 and 20.
 */
function pngSize(buf: Buffer): { width: number; height: number } {
  assert.equal(buf.subarray(1, 4).toString("ascii"), "PNG", "not a PNG");
  assert.equal(buf.subarray(12, 16).toString("ascii"), "IHDR", "PNG does not start with IHDR");
  return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20) };
}

test("the three icon SVGs carry byte-identical mark geometry", () => {
  // The variants differ ONLY in their field (rounded / full-bleed / masked-and-shrunk).
  // The mark itself is duplicated across three files rather than shared, so this is
  // the thing standing between us and a silent divergence where the favicon and the
  // Android icon are different factories.
  const marks = SVG_SOURCES.map((f) => {
    const paths = pathData(readText(f));
    assert.equal(paths.length, 1, `${f} should carry exactly one path (the mark)`);
    return paths[0];
  });
  assert.equal(marks[1], marks[0], "icon-fullbleed.svg's mark has drifted from icon.svg");
  assert.equal(marks[2], marks[0], "icon-maskable.svg's mark has drifted from icon.svg");
});

test("the mark's coordinates are all whole 16px-grid units, so the favicon stays crisp", () => {
  // icon.svg's header explains why: every edge must land on a pixel boundary at 16px,
  // which on a 512 viewBox means every coordinate is a multiple of 32. This was
  // measured — a half-unit inset rendered visibly blurry at 16px while its neighbours
  // stayed sharp. The rule is invisible at 512, so it is exactly the kind of thing a
  // later "just nudge it" edit breaks without noticing.
  const [mark] = pathData(readText("./icons/icon.svg"));
  const coords = mark.match(/\d+(\.\d+)?/g)?.map(Number) ?? [];
  assert.ok(coords.length > 0, "no coordinates parsed out of the mark");
  const offGrid = coords.filter((n) => n % 32 !== 0);
  assert.deepEqual(offGrid, [], `mark coordinates must be multiples of 32 (16px grid); off-grid: ${offGrid}`);
});

test("the maskable variant keeps the mark inside the 80% safe zone", () => {
  // A maskable icon may be cropped to the centred circle of 80% diameter. The mark
  // spans 96..416, so its corners sit √(160² + 160²) ≈ 226 from centre — OUTSIDE the
  // 204.8 safe radius at full size. icon-maskable.svg scales it to survive that, and
  // this asserts the scale is actually present and actually sufficient, because the
  // failure it prevents (Android quietly clipping the mark's corners) is invisible
  // from a desktop browser.
  const svg = readText("./icons/icon-maskable.svg");
  const scale = Number(svg.match(/scale\(([\d.]+)\)/)?.[1]);
  assert.ok(Number.isFinite(scale), "icon-maskable.svg must scale the mark");
  const halfSpan = (416 - 96) / 2; // the mark is centred on 256, so this is its half-extent
  const cornerRadius = Math.hypot(halfSpan, halfSpan) * scale;
  assert.ok(cornerRadius <= 204.8, `scaled mark corner at ${cornerRadius.toFixed(1)}px exceeds the 204.8 safe radius`);
});

test("every icon index.html links actually exists, at the size it claims", () => {
  const html = readText("./index.html");
  const links = [...html.matchAll(/<link[^>]+rel="(icon|apple-touch-icon)"[^>]*>/g)].map((m) => m[0]);
  assert.ok(links.length >= 4, `expected the SVG + 2 PNG fallbacks + apple-touch, found ${links.length}`);

  for (const link of links) {
    const href = link.match(/href="([^"]+)"/)?.[1];
    assert.ok(href, `link has no href: ${link}`);
    // Everything is served from the dist root, so /icons/x maps to src/icons/x.
    const file = `./icons/${href.split("/").pop()}`;
    const bytes = read(file); // throws if index.html points at an icon that isn't there
    const sizes = link.match(/sizes="(\d+)x(\d+)"/);
    if (!sizes || !href.endsWith(".png")) {
      continue; // the SVG declares no pixel size, and has none to check
    }
    const want = Number(sizes[1]);
    assert.deepEqual(pngSize(bytes), { width: want, height: want }, `${file} is not ${want}x${want} as index.html claims`);
  }
});

test("every manifest icon exists, at its declared size, and the set covers any + maskable", () => {
  const manifest = JSON.parse(readText("./manifest.webmanifest")) as {
    icons: { src: string; sizes: string; type: string; purpose: string }[];
  };
  for (const icon of manifest.icons) {
    const bytes = read(`./icons/${icon.src.split("/").pop()}`); // throws on a missing file
    if (icon.type !== "image/png") {
      continue;
    }
    const want = Number(icon.sizes.split("x")[0]);
    assert.deepEqual(
      pngSize(bytes),
      { width: want, height: want },
      `${icon.src} is not ${icon.sizes} as the manifest claims`,
    );
  }
  // Chrome wants a 192 and a 512 for install; a maskable is what stops Android from
  // cropping the tile's corners off. Losing any of the three degrades silently.
  const has = (p: string, s: string) => manifest.icons.some((i) => i.purpose === p && i.sizes === s);
  assert.ok(has("any", "192x192"), "manifest is missing the 192x192 any icon");
  assert.ok(has("any", "512x512"), "manifest is missing the 512x512 any icon");
  assert.ok(has("maskable", "512x512"), "manifest is missing the 512x512 MASKABLE icon");
});
