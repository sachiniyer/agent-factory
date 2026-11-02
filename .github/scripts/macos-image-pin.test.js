// The arm64 macOS legs' image pin (#6631).
//
// GitHub's `macos-latest` label is migrating to macOS 26, and while it migrates
// its arm64 capacity is starved: three `macOS end-to-end` master runs in one
// day had their arm64 leg sit queued ~15m and then get platform-cancelled
// having run zero steps — and a cancelled leg fails the run despite
// `continue-on-error`. The arm64 legs pin `macos-15`, the image the label
// resolves to today, until the migration settles. Once it does, move them back
// to `macos-latest` (or `macos-26`) and retire this test with the pin.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const WORKFLOWS = path.join(__dirname, "..", "workflows");
const read = (name) => fs.readFileSync(path.join(WORKFLOWS, name), "utf8");

const PIN = "macos-15";

test("no workflow selects the floating macos-latest label", () => {
  for (const file of fs.readdirSync(WORKFLOWS).filter((name) => /\.ya?ml$/.test(name))) {
    const hits = read(file)
      .split("\n")
      .map((line, i) => `${file}:${i + 1}: ${line}`)
      .filter((line) => /macos-latest/.test(line));
    assert.deepEqual(hits, [], `pin the arm64 leg to ${PIN} instead — see this file's header`);
  }
});

test("pr.yml's darwin test job runs on the pinned arm64 image", () => {
  const pr = read("pr.yml");
  const start = pr.indexOf("\n  test-macos:\n");
  assert.notEqual(start, -1, "no test-macos job in pr.yml");
  // The job block runs to the next key (or comment) at two-space indent; its
  // own lines are indented deeper.
  const rest = pr.slice(start + 1);
  const next = rest.match(/\n {2}\S/);
  const block = next ? rest.slice(0, next.index) : rest;
  assert.match(block, /^ {4}name: Test \(macOS\)$/m, "its check name is a contract — macos-verdict.js reads it exactly");
  assert.match(block, /^ {4}runs-on: macos-15$/m, "the arm64 leg must pin macos-15, not the floating label");
});

test("mac-e2e.yml's arm64 journey leg runs on the pinned image", () => {
  const e2e = read("mac-e2e.yml");
  const matrix = e2e.slice(e2e.indexOf("\n      matrix:"), e2e.indexOf("\n    steps:"));
  const os = matrix.match(/^ {8}os: \[(.*)\]$/m);
  assert.ok(os, "no inline `os:` matrix list in mac-e2e.yml");
  const legs = os[1].split(",").map((entry) => entry.trim());
  assert.ok(legs.includes("macos-15"), `the arm64 leg must pin macos-15: ${os[1]}`);
  assert.ok(legs.includes("macos-15-intel"), `the Intel leg is macos-15-intel: ${os[1]}`);
});
