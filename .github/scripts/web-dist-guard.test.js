"use strict";

// The guard in front of the write-capable web/dist commit job (#6447): an
// untrusted tree must be refused unless it is plain, small and well-named.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const GUARD = path.join(__dirname, "web-dist-guard.sh");

function tree(setup) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "wdg-"));
  const dir = path.join(root, "dist");
  fs.mkdirSync(path.join(dir, "lazy"), { recursive: true });
  fs.writeFileSync(path.join(dir, "index.html"), "<html></html>");
  fs.writeFileSync(path.join(dir, "af-web.js"), "console.log(1)");
  fs.writeFileSync(path.join(dir, "lazy", "docs-editor.js"), "1");
  if (setup) setup(dir, root);
  return dir;
}
const run = (dir) => spawnSync("bash", [GUARD, dir], { encoding: "utf8" });

test("a plain bundle passes", () => {
  const r = run(tree());
  assert.equal(r.status, 0, r.stderr);
});

test("a symlink is refused", () => {
  const r = run(tree((d) => fs.symlinkSync("/etc/passwd", path.join(d, "x.js"))));
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /non-regular/);
});

test("a symlinked root is refused", () => {
  const real = tree();
  const link = real + "-link";
  fs.symlinkSync(real, link);
  assert.notEqual(run(link).status, 0);
});

test("a fifo is refused", () => {
  const r = run(tree((d) => spawnSync("mkfifo", [path.join(d, "p")])));
  assert.notEqual(r.status, 0);
});

test("dot-prefixed, dash-prefixed and odd names are refused", () => {
  for (const name of [".gitattributes", "-rf", "a b.js", "a;b.js", "a\nb.js"]) {
    const r = run(tree((d) => fs.writeFileSync(path.join(d, name), "x")));
    assert.notEqual(r.status, 0, JSON.stringify(name));
    assert.match(r.stderr, /unsafe name/, JSON.stringify(name));
  }
});

test("a nested .git directory is refused", () => {
  const r = run(tree((d) => {
    fs.mkdirSync(path.join(d, ".git"));
    fs.writeFileSync(path.join(d, ".git", "config"), "x");
  }));
  assert.notEqual(r.status, 0);
});

test("an oversize file is refused", () => {
  const r = run(tree((d) => fs.writeFileSync(path.join(d, "big.js"), Buffer.alloc(9 * 1024 * 1024))));
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /bytes/);
});

test("too many files are refused", () => {
  const r = run(tree((d) => {
    for (let i = 0; i < 201; i++) fs.writeFileSync(path.join(d, `f${i}.js`), "x");
  }));
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /files exceeds/);
});

test("a tree without index.html is refused", () => {
  const r = run(tree((d) => fs.rmSync(path.join(d, "index.html"))));
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /index.html/);
});

test("a hard-linked file is refused", () => {
  const r = run(tree((d, root) => {
    fs.writeFileSync(path.join(root, "outside"), "x");
    fs.linkSync(path.join(root, "outside"), path.join(d, "linked.js"));
  }));
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /hard-linked/);
});

test("Windows-hostile names are refused", () => {
  for (const name of ["CON", "nul.js", "Lpt1.txt", "trailing.", "lazy/aux.js"]) {
    const r = run(tree((d) => {
      fs.mkdirSync(path.dirname(path.join(d, name)), { recursive: true });
      fs.writeFileSync(path.join(d, name), "x");
    }));
    assert.notEqual(r.status, 0, name);
    assert.match(r.stderr, /Windows-hostile/, name);
  }
});

test("ordinary look-alikes pass", () => {
  for (const name of ["console.js", "icons/auxiliary.svg", "af-web.css", "a.b.c"]) {
    const r = run(tree((d) => {
      fs.mkdirSync(path.dirname(path.join(d, name)), { recursive: true });
      fs.writeFileSync(path.join(d, name), "x");
    }));
    assert.equal(r.status, 0, name + " " + r.stderr);
  }
});
