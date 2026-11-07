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
// copied here, and the service worker is copied verbatim, so the same source always
// yields the same dist.
import { build } from "esbuild";
import { createHash } from "node:crypto";
import { copyFile, mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
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

// The Docs tab (docs_pane.ts and the CodeMirror editor) is its own build, loaded
// by docs_tab.ts with a dynamic import the first time a Docs tab opens — and so
// is the code-fence grammar chunk (#6731, codeblocks_chunk.ts), fetched by the
// Docs bundle only when a previewed doc holds a fenced block with a language.
// They are ONE build with splitting on, so the CodeMirror core both need
// (@codemirror/language pulls in view/state) lands in the shared chunk-*.js
// instead of being duplicated into codeblocks.js. af-web.js is untouched either
// way — nothing it shares is promoted into a startup fetch. Minified because it
// is almost all vendored CodeMirror, which nobody reads in the committed diff.
await build({
  entryPoints: { "docs-editor": "src/docs_pane.ts", codeblocks: "src/codeblocks_chunk.ts" },
  bundle: true,
  format: "esm",
  platform: "browser",
  target: "es2022",
  outdir: "dist/lazy",
  splitting: true,
  chunkNames: "chunk-[hash]",
  sourcemap: false,
  minify: true,
  legalComments: "none",
  logLevel: "info",
});

// The Mermaid library ships as the render frame's own DOCUMENT,
// dist/lazy/mermaid-frame.html (#6642, #6669): the Docs pane navigates a
// sandboxed iframe to it when a doc being previewed holds a ```mermaid fence.
// It is a real served page rather than srcdoc — srcdoc inherits the page's
// CSP, and in WebKit 'self' matches nothing in the frame's opaque origin, so
// every load is denied — and the whole library plus its postMessage bootstrap
// is INLINE, so the frame issues zero fetches of its own: no credential-less
// request for an authenticating proxy to refuse, and no CSP URL-source an
// IPv6 or proxied UI origin would have to spell. The daemon serves the file
// with a per-process script-src nonce stamped over __AF_MMD_FRAME_NONCE__
// (daemon/webserve.go); the script element is the only code in the document
// that can run.
const frameBuild = await build({
  entryPoints: { "mermaid-frame": "src/mermaid_chunk.ts" },
  bundle: true,
  format: "esm",
  platform: "browser",
  target: "es2022",
  outdir: "dist/lazy",
  minify: true,
  legalComments: "none",
  sourcemap: false,
  logLevel: "info",
  write: false,
});
const frameJs = frameBuild.outputFiles.filter((f) => f.path.endsWith(".js"));
if (frameJs.length !== 1) {
  throw new Error(
    `mermaid frame bundle must be exactly one file — got ${frameJs.length}: ` +
      frameBuild.outputFiles.map((f) => f.path).join(", "),
  );
}
const frameCode = frameJs[0].text;
// Inlining is only safe with the two escapes below: `</script` in the module
// text would end the element early (the HTML parser owns termination), and
// `<!--` opens script-data-escaped mode. A backslash is a no-op inside
// strings, templates and regexes alike, so escaping never changes the code.
const frameInline = frameCode.replaceAll("<!--", "<\\!--").replaceAll("</script", "<\\/script");
await writeFile(
  "dist/lazy/mermaid-frame.html",
  `<!doctype html><meta http-equiv="x-dns-prefetch-control" content="off">` +
    `<meta charset="utf-8">` +
    `<script type="module" nonce="__AF_MMD_FRAME_NONCE__">${frameInline}</script>`,
);

// The code-fence grammars (#6731) ship as dist/lazy/codeblocks.js, built above
// alongside the Docs bundle. Naming stays `codeblocks*` so the perf harness
// budgets it under its own keys, like mermaid's.

// Stamp the web client's X-AF-Client-Version value into the bundle.
//
// af() (src/api.ts) sends that header on every /v1/ request so the daemon decodes the
// body with its tolerant (additive-field) decoder rather than the strict
// DisallowUnknownFields path reserved for hand-authored curl (daemon/httpserver.go).
// The daemon keys only on the header's PRESENCE — the value is diagnostics — but a
// content hash is the honest "which web build is this" answer, and it changes exactly
// when the bundle the browser runs changes. Unlike the service-worker cache name,
// which web/embed.go stamps at SERVE time from the embedded bytes (so two branches
// that both change the shell never conflict on a committed hash), this is stamped at
// BUILD time and committed inside dist/af-web.js. That does not reintroduce the
// #4116 conflict: dist/af-web.js is already a committed generated file that changes
// wholesale on every web edit, so a single line inside it neither adds a new conflict
// surface nor a new reproducibility hazard — a rebuild reproduces the bytes (esbuild
// is deterministic, the placeholder bundle it hashes is stable, so the hash is too).
//
// The placeholder stays in the source literal (src/api.ts CLIENT_VERSION) so the
// header is non-empty everywhere the build has NOT run — under tsx (the unit tests),
// where no stamp is applied — and the daemon's presence check still accepts it, the
// same graceful fallback the Go apiclient gets from its "unknown" sentinel.
const CLIENT_VERSION_PLACEHOLDER = "__AF_CLIENT_VERSION__";
const bundlePath = "dist/af-web.js";
const bundle = await readFile(bundlePath, "utf8");
if (!bundle.includes(CLIENT_VERSION_PLACEHOLDER)) {
  // The stamp is what opts the browser into the daemon's tolerant decode, and the
  // placeholder is where af() reads the value from. If src/api.ts ever stops carrying
  // it, fail the build rather than ship a bundle the daemon would strict-decode.
  throw new Error(`build: ${bundlePath} has no ${CLIENT_VERSION_PLACEHOLDER} placeholder for af() to send as X-AF-Client-Version`);
}
// Hash the bundle WITH the placeholder still in place, then substitute. The hash is
// over the pre-stamp bytes so it is a pure function of src/ and is reproduced by every
// rebuild (the bundle esbuild emits is byte-identical each time); a build that hashed
// the already-stamped bytes would be circular and self-inconsistent.
const clientVersion = createHash("sha256").update(bundle).digest("hex").slice(0, 12);
await writeFile(bundlePath, bundle.split(CLIENT_VERSION_PLACEHOLDER).join(clientVersion));

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
// It is copied VERBATIM, placeholder and all. Its cache name is a hash of the shell it
// serves, and web/embed.go fills that in when the daemon serves the file, from the
// exact bytes the binary embeds (#4116). This build used to write the hash into
// dist/sw.js instead. The correct value for a merge of two shell changes is a hash of
// the merged shell, which neither branch had committed, so every pair of open web PRs
// conflicted on that one line. Taking either side mechanically would have committed a
// stale name, which the committed-bundle check rejects. A committed line that only
// changes when src/sw.js does cannot fall into that trap.
const sw = await readFile("src/sw.js", "utf8");
if (!sw.includes("__AF_SHELL_VERSION__")) {
  // The stamp is what busts a stale cache on deploy, and web/embed.go can only apply
  // it where this placeholder stands. If it is ever renamed away, fail the build
  // rather than ship a worker pinned to one cache name forever.
  throw new Error("build: src/sw.js has no __AF_SHELL_VERSION__ placeholder for the daemon to stamp");
}
await writeFile("dist/sw.js", sw);
