// Coverage for the build's reproducibility wipe (feat: PWA follow-up).
//
// build.mjs wipes dist/ before regenerating it, so the committed output is a pure
// function of src/ and never carries an orphan from a renamed or deleted source. The
// icon set is the file that makes this matter — it is discovered by readdir, so a
// renamed icon would otherwise linger in dist/ and stay embedded and served. This runs
// the real build against a planted stale file and asserts it is gone.
//
// The build is deterministic, so regenerating dist/ here just reproduces the committed
// bytes — the test leaves the tree clean apart from removing the stale file it planted.

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// build.mjs uses paths relative to the web/ root (its own directory), so run it there.
const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));

// #4116: the worker's cache name is stamped by the daemon when it serves the file
// (web/embed.go), so the committed copy must be the source, placeholder and all. A
// build that wrote the stamp back in would reintroduce a committed line whose right
// value, after any merge of two shell changes, is one neither branch has.
test("the build copies the service worker verbatim, leaving its cache name for the daemon to stamp", () => {
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const source = readFileSync(join(webRoot, "src", "sw.js"), "utf8");
  const built = readFileSync(join(webRoot, "dist", "sw.js"), "utf8");
  assert.ok(source.includes("__AF_SHELL_VERSION__"), "src/sw.js must name its cache with the placeholder");
  assert.ok(built === source, "dist/sw.js must be byte-identical to src/sw.js");
});

test("the build wipes stale dist artifacts so the embedded set is a pure function of src", () => {
  const stale = join(webRoot, "dist", "icons", "__stale_regression__.png");
  writeFileSync(stale, "stale");
  try {
    execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
    assert.ok(!existsSync(stale), "build must clear a stale file left in dist/icons, not carry it forward");
    // And it is a wipe-then-regenerate, not a wipe: the real set is back afterwards.
    assert.ok(existsSync(join(webRoot, "dist", "icons", "icon.svg")), "build must regenerate the icon set");
    assert.ok(existsSync(join(webRoot, "dist", "af-web.js")), "build must regenerate the bundle");
    assert.ok(existsSync(join(webRoot, "dist", "sw.js")), "build must regenerate the worker");
  } finally {
    // Belt and suspenders: if the build failed to wipe (the bug this guards), don't
    // leave the planted file behind to pollute the committed tree.
    rmSync(stale, { force: true });
  }
});

// The Docs tab's CodeMirror bundle loads on demand (docs_tab.ts). The shell must not
// carry it, and must not import anything at startup: the service worker caches only
// the shell files, so a static import of another file breaks the offline shell.
// #6731: the Docs build shares its CodeMirror core with the code-fence grammars
// through esbuild splitting, so CodeMirror itself may live in the chunk-*.js both
// entries import — the docs set is docs-editor.js plus those chunks.
test("the Docs editor is a separate lazy bundle that the shell does not import", () => {
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const shell = readFileSync(join(webRoot, "dist", "af-web.js"), "utf8");
  const lazy = readFileSync(join(webRoot, "dist", "lazy", "docs-editor.js"), "utf8");
  const docsSet =
    lazy +
    readdirSync(join(webRoot, "dist", "lazy"))
      .filter((f) => f.startsWith("chunk-") && f.endsWith(".js"))
      .map((f) => readFileSync(join(webRoot, "dist", "lazy", f), "utf8"))
      .join("");
  assert.ok(shell.includes('"./lazy/docs-editor.js"'), "the shell must load the Docs bundle by its path");
  assert.doesNotMatch(shell, /^\s*import\s[^(]/m, "the shell must have no static imports");
  assert.ok(!shell.includes("cm-scroller"), "CodeMirror must not be in the shell");
  assert.ok(docsSet.includes("cm-scroller"), "CodeMirror must be in the Docs lazy set");
  assert.match(lazy, /export\s*\{[^}]*DocsPane/, "the Docs bundle must export DocsPane");
});

// #6642: a ```mermaid fence renders through mermaid 12, which is far too big for
// either the shell or the Docs bundle — so it ships as its own lazy set,
// dist/lazy/mermaid.js plus the mermaid-* chunk files esbuild splits its dynamic
// imports into, fetched by the Docs bundle only when a doc holds a mermaid fence.
test("the Mermaid renderer is its own lazy chunk set, outside the shell and the Docs bundle", () => {
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const shell = readFileSync(join(webRoot, "dist", "af-web.js"), "utf8");
  const docs = readFileSync(join(webRoot, "dist", "lazy", "docs-editor.js"), "utf8");
  const entry = join(webRoot, "dist", "lazy", "mermaid.js");
  assert.ok(existsSync(entry), "the Mermaid entry chunk must be built into dist/lazy");
  assert.ok(!shell.includes("mermaid"), "the shell must carry no Mermaid");
  assert.ok(docs.includes('"./mermaid.js"'), "the Docs bundle must name the chunk it lazy-loads");
  assert.ok(!docs.includes("dagre"), "the Docs bundle must not bundle the Mermaid library");
  assert.match(readFileSync(entry, "utf8"), /export\s*\{[^}]*renderMermaidDiagram/, "the entry must export the renderer");
  const set = readdirSync(join(webRoot, "dist", "lazy"))
    .filter((f) => f.startsWith("mermaid") && f.endsWith(".js"))
    .map((f) => readFileSync(join(webRoot, "dist", "lazy", f), "utf8"))
    .join("");
  assert.ok(set.includes("dagre"), "the Mermaid library must live in the lazy chunk set");
});

// #6731: the syntax grammars for ``` fences render through dist/lazy/codeblocks.js,
// fetched by the Docs bundle by URL only when a previewed doc's fence names a
// language — never the shell, never eagerly. It shares the Docs build's esbuild
// splitting so the CodeMirror core both need is not duplicated across the two.
test("the code-fence grammars are their own lazy chunk, outside the shell and named by the Docs bundle", () => {
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const shell = readFileSync(join(webRoot, "dist", "af-web.js"), "utf8");
  const docs = readFileSync(join(webRoot, "dist", "lazy", "docs-editor.js"), "utf8");
  const entry = join(webRoot, "dist", "lazy", "codeblocks.js");
  assert.ok(existsSync(entry), "the codeblocks entry chunk must be built into dist/lazy");
  assert.ok(docs.includes("./codeblocks.js"), "the Docs bundle must name the chunk it lazy-loads");
  assert.ok(!shell.includes("codeblocks"), "the shell must carry no codeblock grammar chunk");
  assert.match(readFileSync(entry, "utf8"), /export\s*\{[^}]*highlightRanges/, "the entry must export the highlighter");
});

// af() (src/api.ts) sends X-AF-Client-Version on every /v1/ request so the daemon
// decodes the body with its tolerant (additive-field) decoder instead of the strict
// DisallowUnknownFields path reserved for hand-authored curl (daemon/httpserver.go).
// The header's PRESENCE is the discriminator; the value is a content hash build.mjs
// stamps into the bundle at build time. This pins both halves of that contract on the
// built artifact the daemon embeds.
test("the build stamps af()'s X-AF-Client-Version into the bundle and is reproducible", () => {
  const apiSource = readFileSync(join(webRoot, "src", "api.ts"), "utf8");
  assert.ok(
    apiSource.includes("__AF_CLIENT_VERSION__"),
    "src/api.ts must carry the __AF_CLIENT_VERSION__ placeholder af() sends pre-stamp (and the build replaces)",
  );

  // Two consecutive builds must reproduce dist/af-web.js byte-for-byte: esbuild is
  // deterministic and the stamp is sha256 of the placeholder bundle (stable per
  // source), so a rebuild reproduces the committed bytes — the property the
  // `make web-build && git status --porcelain -- web/dist` gate relies on.
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const first = readFileSync(join(webRoot, "dist", "af-web.js"), "utf8");
  execFileSync("node", ["build.mjs"], { cwd: webRoot, stdio: "pipe" });
  const second = readFileSync(join(webRoot, "dist", "af-web.js"), "utf8");

  assert.equal(first, second, "a rebuild must reproduce dist/af-web.js byte-for-byte (deterministic stamp)");
  assert.ok(
    !first.includes("__AF_CLIENT_VERSION__"),
    "build.mjs must replace the placeholder in dist/af-web.js — the browser must never send the literal sentinel",
  );
  assert.ok(
    first.includes("X-AF-Client-Version"),
    "the bundle must carry the header name so af() opts the browser into the daemon's tolerant decode",
  );
});
