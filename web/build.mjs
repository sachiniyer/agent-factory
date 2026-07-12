// esbuild build for the Agent Factory web client (#1592 Phase 5).
//
// esbuild is a single fast bundler (the locked toolchain choice, design §3.1). It
// bundles src/index.ts into a self-contained ESM file under dist/. The built dist/
// is COMMITTED (locked decision) so `go build ./...` and the Go test suite never
// need Node — the JS toolchain is gated entirely behind `make web-*`. Later PRs add
// the SPA (xterm.js, sidebar, modals) and go:embed dist/ into the daemon; PR1 ships
// only the wire-frame codec so the bundle is tiny.
import { build } from "esbuild";

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
