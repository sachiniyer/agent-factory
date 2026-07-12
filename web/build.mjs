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
// bytes (the reproducibility gate).
import { build } from "esbuild";
import { copyFile, mkdir } from "node:fs/promises";

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
