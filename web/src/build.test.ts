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
import { existsSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// build.mjs uses paths relative to the web/ root (its own directory), so run it there.
const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));

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
