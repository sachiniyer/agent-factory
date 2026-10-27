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
