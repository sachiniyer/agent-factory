// Rasterises the icon SVGs into the committed PNG set (`npm run icons`).
//
// THIS IS NOT PART OF `npm run build`, and that is deliberate. The PNGs are
// committed next to their sources and build.mjs merely copies them, which keeps two
// properties we would otherwise lose:
//
//   - `make web-build` stays deterministic and Chromium-free. Rasterising during
//     the build would make the committed dist/ depend on which Playwright browser
//     revision the builder happened to have, so two people building the same commit
//     would produce different bytes.
//   - The build keeps working on a machine with no Playwright browsers installed.
//     Only the person who EDITS the mark needs Chromium, and they need it once.
//
// The cost is that the PNGs can fall behind an edited SVG, so icons.test.ts pins
// every PNG's dimensions and re-asserts the sources agree. Re-run this script after
// touching any icon SVG; the diff it produces is the regenerated set.
//
// Chromium is the rasteriser because Playwright already ships it for the selftest —
// no new dependency, and it renders the SVG with the same engine that will render
// the favicon in the browser, so what we commit is what a user sees.
import { chromium } from "@playwright/test";
import { readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const iconsDir = join(here, "src", "icons");

// Each PNG names its source SVG, so the variant rationale lives in the SVG headers
// rather than being re-explained here. Sizes are the union of what the HTML shell
// links and what the manifest declares — icons.test.ts asserts this list and those
// two consumers stay in agreement.
const TARGETS = [
  { src: "icon.svg", out: "favicon-16.png", size: 16 },
  { src: "icon.svg", out: "favicon-32.png", size: 32 },
  { src: "icon.svg", out: "icon-192.png", size: 192 },
  { src: "icon.svg", out: "icon-512.png", size: 512 },
  { src: "icon-fullbleed.svg", out: "apple-touch-icon-180.png", size: 180 },
  { src: "icon-maskable.svg", out: "icon-maskable-512.png", size: 512 },
];

const browser = await chromium.launch();
try {
  for (const { src, out, size } of TARGETS) {
    const svg = await readFile(join(iconsDir, src), "utf8");
    // deviceScaleFactor 1 so a `size` viewport is exactly `size` device pixels —
    // the default would silently emit a 2x image on a HiDPI-configured run.
    const page = await browser.newPage({ viewport: { width: size, height: size }, deviceScaleFactor: 1 });
    // The SVG rides in as a data: URL inside an <img> sized to the viewport, rather
    // than being navigated to directly: navigating renders the SVG at its own
    // intrinsic size against a white page, which would bake a white fringe into the
    // rounded tile's corners. An <img> scales the viewBox to exactly the box we ask
    // for, and omitBackground keeps whatever the SVG itself does not paint
    // transparent (the corners outside icon.svg's rx).
    const dataURL = `data:image/svg+xml;base64,${Buffer.from(svg, "utf8").toString("base64")}`;
    await page.setContent(
      `<!doctype html><style>html,body{margin:0;padding:0}img{display:block;width:${size}px;height:${size}px}</style>` +
        `<img src="${dataURL}">`,
    );
    await page.locator("img").waitFor({ state: "visible" });
    const png = await page.screenshot({ omitBackground: true });
    await writeFile(join(iconsDir, out), png);
    await page.close();
    console.log(`icons: ${src} -> ${out} (${size}x${size}, ${png.length} bytes)`);
  }
} finally {
  await browser.close();
}
