// esbuild build for the Agent Factory web client (#1592 Phase 5).
//
// esbuild is a single fast bundler (the locked toolchain choice, design §3.1). It
// bundles src/index.ts into a self-contained ESM file under dist/, and — because
// index.ts imports styles.css — extracts the CSS into a sibling dist/af-web.css.
// The static index.html shell is copied verbatim into dist/ so the whole served
// tree lives under one committed directory the daemon go:embeds.
//
// The built dist/ is COMMITTED (locked decision) so `go build ./...` and the Go
// test suite never need Node — the JS toolchain is gated entirely behind
// `make web-*`. The build is deterministic: a rebuild reproduces the committed
// bytes (the reproducibility gate). Everything added for the PWA keeps that
// property — the icons are rasterised OUT of band by gen-icons.mjs and merely
// copied here, and the worker's cache name is a hash of bytes this build just
// produced, so the same source always yields the same dist.
import { build } from "esbuild";
import { copyFile, mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { join } from "node:path";

// Rebuild dist/ from scratch. The committed dist/ is ENTIRELY generated, so wiping it
// first is what makes the output a pure function of src/: a renamed or deleted source
// — an icon especially, since the icon set is discovered by readdir rather than a
// fixed list — would otherwise leave its stale output orphaned in dist/, embedded and
// served forever, and the committed bytes would depend on what the previous checkout
// left behind instead of on src/ alone. That is exactly the reproducibility contract
// this build exists to hold (a rebuild reproduces the committed bytes), and copying
// over a dirty dist/ silently breaks it.
await rm("dist", { recursive: true, force: true });
await mkdir("dist", { recursive: true });

await build({
  entryPoints: ["src/index.ts"],
  bundle: true,
  format: "esm",
  platform: "browser",
  target: "es2022",
  outfile: "dist/af-web.js",
  sourcemap: false,
  minify: false,
  logLevel: "info",
});

// The HTML shell is not an esbuild input (it references the bundle by URL), so
// copy it into the embed root alongside the built JS/CSS.
await copyFile("src/index.html", "dist/index.html");

// The manifest is served from the root so its "/" start_url and scope resolve
// against the origin. The daemon needs no route for it (nor for the icons or the
// worker): serveSPA serves any file that exists in the embedded dist/ by name. It
// DOES need a MIME type registration, because Go's table has no .webmanifest and the
// responses carry nosniff — see daemon/webserve.go.
await copyFile("src/manifest.webmanifest", "dist/manifest.webmanifest");

// The icon set: the three SVG sources plus the PNGs gen-icons.mjs rendered from
// them. Copied wholesale (the whole src/icons dir) rather than named individually, so
// adding a size is a gen-icons.mjs edit and nothing else — icons.test.ts is what pins
// the set that index.html and the manifest actually reference. This copies INTO the
// freshly-wiped dist/ above, so a size that was renamed or dropped from src/ does not
// linger here from a prior build.
await mkdir("dist/icons", { recursive: true });
for (const name of await readdir("src/icons")) {
  await copyFile(join("src/icons", name), join("dist/icons", name));
}

// The service worker is copied, NOT bundled: it must be served from the scope root
// to control "/", and it is plain JS with no imports.
//
// Its cache name is stamped here with a hash of the shell it will serve. A CONTENT
// hash, specifically — the obvious alternative, main.go's version, is wrong: CI bumps
// that version without rebuilding web/dist, so the committed worker would name a
// cache for a build it was never part of. Hashing the actual output means the name
// changes exactly when the bytes do, which is the only thing the cache cares about,
// and keeps the build reproducible (same source in, same hash out).
const shellBytes = await Promise.all(
  ["dist/af-web.js", "dist/af-web.css", "dist/index.html"].map((f) => readFile(f)),
);
const shellHash = createHash("sha256").update(Buffer.concat(shellBytes)).digest("hex").slice(0, 12);
const sw = await readFile("src/sw.js", "utf8");
if (!sw.includes("__AF_SHELL_VERSION__")) {
  // The stamp is what busts a stale cache on deploy. If the placeholder is ever
  // renamed away, fail the build rather than ship a worker pinned to the literal
  // string "__AF_SHELL_VERSION__" forever.
  throw new Error("build: src/sw.js has no __AF_SHELL_VERSION__ placeholder to stamp");
}
await writeFile("dist/sw.js", sw.replace("__AF_SHELL_VERSION__", shellHash));
console.log(`build: stamped sw.js cache af-shell-${shellHash}`);
