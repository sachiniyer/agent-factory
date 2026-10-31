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
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
