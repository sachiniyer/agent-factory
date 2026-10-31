"use strict";

// The PR-side rule for web/dist (#6447), including the shapes a text match of
// `git diff --name-only` missed: non-ASCII names, renames out of web/dist, deletes.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const RULES = path.join(__dirname, "web-dist-pr-rules.sh");

function git(cwd, ...args) {
  const r = spawnSync("git", ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "core.hooksPath=/dev/null", ...args], { cwd, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return r.stdout;
}

// A repo whose base commit has web/dist/x.js, web/dist/index.html, web/src/a.ts;
// `change` edits the tree and the result is committed as the PR head.
function pr(change) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "wdr-"));
  git(dir, "init", "-q", "-b", "master");
  for (const [f, c] of [["web/dist/x.js", "x"], ["web/dist/index.html", "i"], ["web/src/a.ts", "a"], ["README.md", "r"]]) {
    fs.mkdirSync(path.dirname(path.join(dir, f)), { recursive: true });
    fs.writeFileSync(path.join(dir, f), c);
  }
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "base");
  const base = git(dir, "rev-parse", "HEAD").trim();
  change(dir);
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "pr", "--allow-empty");
  return { dir, base };
}
const run = ({ dir, base }, mode) => spawnSync("bash", [RULES, base, mode], { cwd: dir, encoding: "utf8" });
const write = (dir, f, c = "n") => {
  fs.mkdirSync(path.dirname(path.join(dir, f)), { recursive: true });
  fs.writeFileSync(path.join(dir, f), c);
};

test("normal PR: source-only change passes", () => {
  assert.equal(run(pr((d) => write(d, "web/src/a.ts", "b")), "normal").status, 0);
});

test("normal PR: every way of touching web/dist fails", () => {
  const shapes = {
    modify: (d) => write(d, "web/dist/x.js", "y"),
    add: (d) => write(d, "web/dist/new.js"),
    "add non-ASCII name": (d) => write(d, "web/dist/é.html"),
    "add emoji name": (d) => write(d, "web/dist/lazy/💥.js"),
    delete: (d) => fs.rmSync(path.join(d, "web/dist/x.js")),
    "rename out of web/dist": (d) => {
      fs.mkdirSync(path.join(d, "web/src"), { recursive: true });
      fs.renameSync(path.join(d, "web/dist/x.js"), path.join(d, "web/src/x.js"));
    },
    "rename into web/dist": (d) => {
      fs.renameSync(path.join(d, "README.md"), path.join(d, "web/dist/README.md"));
    },
    "rename within web/dist": (d) => fs.renameSync(path.join(d, "web/dist/x.js"), path.join(d, "web/dist/z.js")),
  };
  for (const [name, change] of Object.entries(shapes)) {
    const r = run(pr(change), "normal");
    assert.notEqual(r.status, 0, name);
    assert.match(r.stdout, /changes web\/dist/, name);
  }
});

test("sync PR: a dist-only change passes, including non-ASCII names and deletes", () => {
  const shapes = [
    (d) => write(d, "web/dist/x.js", "y"),
    (d) => write(d, "web/dist/é.html"),
    (d) => fs.rmSync(path.join(d, "web/dist/x.js")),
    (d) => fs.renameSync(path.join(d, "web/dist/x.js"), path.join(d, "web/dist/z.js")),
  ];
  for (const change of shapes) assert.equal(run(pr(change), "sync").status, 0);
});

test("sync PR: anything outside web/dist fails", () => {
  const shapes = {
    "other file": (d) => write(d, "README.md", "changed"),
    "non-ASCII name outside": (d) => write(d, "docs/é.md"),
    "workflow file": (d) => write(d, ".github/workflows/x.yml"),
    "dist change plus source": (d) => { write(d, "web/dist/x.js", "y"); write(d, "web/src/a.ts", "b"); },
    "rename out of web/dist": (d) => {
      fs.mkdirSync(path.join(d, "web/src"), { recursive: true });
      fs.renameSync(path.join(d, "web/dist/x.js"), path.join(d, "web/src/x.js"));
    },
    "delete outside": (d) => fs.rmSync(path.join(d, "README.md")),
  };
  for (const [name, change] of Object.entries(shapes)) {
    const r = run(pr(change), "sync");
    assert.notEqual(r.status, 0, name);
    assert.match(r.stdout, /may change only web\/dist/, name);
  }
});

test("an empty PR passes both modes", () => {
  assert.equal(run(pr(() => {}), "normal").status, 0);
  assert.equal(run(pr(() => {}), "sync").status, 0);
});

test("an unknown mode is refused", () => {
  assert.equal(run(pr(() => {}), "bogus").status, 2);
});
