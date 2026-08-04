const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const { SELFTEST_PATHS, scopeWebSelftest, matchesPattern } = require("./web-selftest-scope.js");

const WORKFLOWS = path.join(__dirname, "..", "workflows");
const readWorkflow = (name) => fs.readFileSync(path.join(WORKFLOWS, name), "utf8");

// ── the predicate ─────────────────────────────────────────────────────────────

test("a change under a watched directory runs the suite", () => {
  assert.equal(scopeWebSelftest(["web/src/ui.ts"]).run, true);
  assert.equal(scopeWebSelftest(["daemon/server.go"]).run, true);
  assert.equal(scopeWebSelftest(["agentproto/frame.go"]).run, true);
  assert.equal(scopeWebSelftest(["apiproto/envelope.go"]).run, true);
  // The committed bundle counts: it is what the harness actually serves.
  assert.equal(scopeWebSelftest(["web/dist/af-web.js"]).run, true);
});

test("a change that cannot reach the web client skips the suite", () => {
  const { run, matched } = scopeWebSelftest([
    "docs/web-selftest.md",
    "README.md",
    "CLAUDE.md",
    "plugins/claude/agent-factory/skills/agent-factory/SKILL.md",
    ".github/workflows/stale.yml",
  ]);
  assert.equal(run, false);
  assert.deepEqual(matched, []);
});

test("ANY Go source runs the suite — the harness builds and runs the whole binary", () => {
  // This replaced an enumerated package list. The old list called
  // session/git/worktree.go out of scope, which was wrong: the entry script
  // seeds through `$BIN sessions create` and the spec round-trips those flows.
  // Rather than add the packages review found (twice), the predicate now asks a
  // question that is decidable: is this Go the binary is built from?
  for (const path of [
    "main.go", // root package — `**/*.go` must match at depth 0 too
    "session/git/worktree.go",
    "task/runner.go",
    "api/sessions.go",
    "commands/configcmd.go",
    "apiclient/client.go",
    "ui/overlay.go",
    "internal/proctree/proctree_darwin.go",
  ]) {
    assert.equal(scopeWebSelftest([path]).run, true, `${path} is compiled into af and must be watched`);
  }
  assert.equal(scopeWebSelftest(["go.mod"]).run, true);
  assert.equal(scopeWebSelftest(["go.sum"]).run, true);
});

test("one in-scope path among many out-of-scope ones still runs the suite", () => {
  const { run, matched } = scopeWebSelftest(["README.md", "CHANGELOG.md", "daemon/tasks.go"]);
  assert.equal(run, true);
  assert.deepEqual(matched, ["daemon/tasks.go"]);
});

test("a rename OUT of a watched path is still seen (the --no-renames contract)", () => {
  // `git diff --name-only` reports a pure rename as the NEW path only, so the
  // scope job passes --no-renames and git emits the delete AND the add. This
  // pins the helper's half of that contract: given both paths, the watched
  // source still matches even though the destination does not.
  const { run, matched } = scopeWebSelftest([
    "scripts/container/backup-entry.sh", // the rename destination — not watched
    "scripts/container/web-selftest-entry.sh", // the deleted source — watched
  ]);
  assert.equal(run, true);
  assert.deepEqual(matched, ["scripts/container/web-selftest-entry.sh"]);
});

test("an exact-file pattern does not match a longer sibling name", () => {
  // scripts/testbox.sh is watched; scripts/testbox-selftest.sh is a DIFFERENT
  // file and must not match it. A prefix-matching bug here would run the
  // expensive suite on unrelated harness edits — the cheap direction, but it
  // would also mean the pattern semantics are not what the workflow's `paths:`
  // filter does, and then the two schedules disagree.
  assert.equal(matchesPattern("scripts/testbox.sh", "scripts/testbox.sh"), true);
  assert.equal(matchesPattern("scripts/testbox.sh", "scripts/testbox-selftest.sh"), false);
  assert.equal(scopeWebSelftest(["scripts/testbox-selftest.sh"]).run, false);
  assert.equal(scopeWebSelftest(["scripts/testbox.sh"]).run, true);
});

test("a directory pattern does not match a same-prefixed sibling directory", () => {
  assert.equal(matchesPattern("web/**", "web/src/ui.ts"), true);
  assert.equal(matchesPattern("web/**", "webhooks/handler.go"), false);
});

test("an empty change list skips — the workflow never asks about an unknown diff", () => {
  // Documented contract, not an accident: pr.yml resolves "I could not compute
  // the diff" to run=true WITHOUT consulting this helper, so an empty list here
  // always means a genuinely empty diff.
  assert.equal(scopeWebSelftest([]).run, false);
});

test("the whole harness invocation chain is watched, not just its middle", () => {
  // CI runs `make web-selftest-container`, so every link between that command and
  // the browser can break the suite. Missing a link means a PR that edits only
  // that link merges without running the suite it just changed — the same "reads
  // as coverage" failure this whole PR exists to remove (Codex review on #2762
  // caught Makefile and copy-src.sh absent from the first cut).
  for (const link of [
    "Makefile", // defines the web-selftest-container target CI invokes
    "scripts/testbox.sh", // the target's implementation
    "scripts/container/Dockerfile.web-selftest", // the image it builds
    "scripts/container/web-selftest-entry.sh", // what runs inside
    "scripts/container/copy-src.sh", // sourced by the entry to stage /src -> /work
  ]) {
    assert.equal(scopeWebSelftest([link]).run, true, `${link} is part of the harness and must be watched`);
  }
});

test("the CI wiring watches itself, so a change to the gate re-proves the gate", () => {
  assert.equal(scopeWebSelftest([".github/workflows/pr.yml"]).run, true);
  assert.equal(scopeWebSelftest([".github/scripts/web-selftest-scope.js"]).run, true);
  assert.equal(scopeWebSelftest([".github/workflows/web-selftest.yml"]).run, true);
});

test("every pattern is a shape this matcher actually implements", () => {
  for (const pattern of SELFTEST_PATHS) {
    if (pattern.startsWith("**/*.")) {
      assert.equal(pattern.slice(5).includes("*"), false, `${pattern}: only a plain **/*.ext is supported`);
      continue;
    }
    if (pattern.endsWith("/**")) {
      assert.equal(pattern.slice(0, -3).includes("*"), false, `${pattern}: only a TRAILING /** is supported`);
      continue;
    }
    assert.equal(
      pattern.includes("*"),
      false,
      `${pattern}: matchesPattern implements exact paths, a trailing /** and a leading **/*.ext only. ` +
        "Teach it the new shape (and teach the workflow's paths: filter the same one) before adding this.",
    );
  }
});

test("**/*.ext matches by extension, not by a same-prefixed name", () => {
  assert.equal(matchesPattern("**/*.go", "main.go"), true);
  assert.equal(matchesPattern("**/*.go", "a/b/c.go"), true);
  assert.equal(matchesPattern("**/*.go", "docs/going-further.md"), false);
  assert.equal(matchesPattern("**/*.go", "web/src/ui.ts"), false);
});

// ── the copy of the list that lives in the workflow trigger ───────────────────

/** The `paths:` list under a workflow trigger, in file order. */
function triggerPaths(yaml) {
  const lines = yaml.split("\n");
  const start = lines.findIndex((line) => /^\s+paths:\s*$/.test(line));
  assert.notEqual(start, -1, "no `paths:` trigger filter found");
  const indent = lines[start].match(/^\s*/)[0].length;
  const found = [];
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.trim() === "") continue;
    if (line.match(/^\s*/)[0].length <= indent) break;
    if (line.trim().startsWith("#")) continue;
    const item = line.trim().match(/^- '(.+)'$/);
    if (!item) break;
    found.push(item[1]);
  }
  return found;
}

test("web-selftest.yml's push filter matches the helper's path list exactly", () => {
  // The whole point of this file. A trigger `paths:` cannot be computed, so that
  // list is a literal copy of SELFTEST_PATHS — and a copy nobody checks is a copy
  // that is wrong. Drift in either direction is a hole: the PR gate and the master
  // signal would then watch different things, and the one that stopped watching
  // would keep reporting green.
  assert.deepEqual(
    triggerPaths(readWorkflow("web-selftest.yml")),
    SELFTEST_PATHS,
    "web-selftest.yml's `paths:` and SELFTEST_PATHS have drifted — update both.",
  );
});

// ── the wiring that gives a red run teeth (#2762) ─────────────────────────────

/** Top-level job ids of a workflow, in file order. */
function jobIds(yaml) {
  const lines = yaml.split("\n");
  const start = lines.findIndex((line) => /^jobs:\s*$/.test(line));
  assert.notEqual(start, -1, "no `jobs:` block found");
  return lines
    .slice(start + 1)
    .map((line) => line.match(/^ {2}([A-Za-z0-9_-]+):\s*$/))
    .filter(Boolean)
    .map((match) => match[1]);
}

/** One job's inline `needs: [a, b]` list, or null. */
function jobNeeds(yaml, jobId) {
  let inJob = false;
  for (const line of yaml.split("\n")) {
    const job = line.match(/^ {2}([A-Za-z0-9_-]+):\s*$/);
    if (job) {
      inJob = job[1] === jobId;
      continue;
    }
    if (!inJob) continue;
    const needs = line.match(/^ {4}needs:\s*\[(.*)\]\s*$/);
    if (needs) {
      return needs[1]
        .split(",")
        .map((entry) => entry.trim())
        .filter(Boolean);
    }
  }
  return null;
}

test("every job in pr.yml is listed in Build's needs", () => {
  // "Lint" and "Build" are this repo's only required checks (ruleset 14851503), so
  // a job that Build does not depend on gates NOTHING — it renders as a check,
  // reads as coverage, and can be red on a PR that merges. That is #2762 (the web
  // selftest) and #2623 (a docs check switched off) in one sentence.
  //
  // So: adding a job to pr.yml without adding it to `needs` fails Lint. If a job is
  // ever meant to be advisory, that is a deliberate decision — delete it from this
  // assertion with a comment saying why, rather than letting it be an omission
  // nobody notices.
  const yaml = readWorkflow("pr.yml");
  const needs = jobNeeds(yaml, "build");
  assert.notEqual(needs, null, "pr.yml's build job has no inline `needs: [...]` list");
  for (const id of jobIds(yaml)) {
    if (id === "build") continue;
    assert.ok(needs.includes(id), `pr.yml job "${id}" is not in Build's needs, so it gates nothing`);
  }
});

test("Build runs even when a need fails, so the required check goes red not skipped", () => {
  // GitHub skips a job whose `needs` failed, and a SKIPPED required check does not
  // fail — the merge button stays live. `if: always()` plus the gate step is what
  // converts a failed need into a red "Build". Without it, every `needs` entry
  // above is decorative.
  const yaml = readWorkflow("pr.yml");
  const build = yaml.slice(yaml.indexOf("\n  build:\n"));
  assert.match(build, /^ {4}if: always\(\)$/m, "pr.yml's build job must keep `if: always()`");
  assert.match(
    build,
    /^ {8}if: contains\(needs\.\*\.result, 'failure'\) \|\| contains\(needs\.\*\.result, 'cancelled'\)$/m,
    "pr.yml's build gate step must fail on a failed or cancelled need",
  );
});

test("pr.yml calls the web selftest workflow, and it is scoped rather than unconditional", () => {
  const yaml = readWorkflow("pr.yml");
  assert.match(
    yaml,
    /^ {4}uses: \.\/\.github\/workflows\/web-selftest\.yml$/m,
    "pr.yml must call web-selftest.yml as a reusable workflow — that is the only way Build can `needs` it",
  );
  assert.deepEqual(jobNeeds(yaml, "web-selftest"), ["web-selftest-scope"]);
  assert.match(yaml, /^ {4}if: needs\.web-selftest-scope\.outputs\.run == 'true'$/m);
});

test("web-selftest.yml is callable AND runs on master", () => {
  const yaml = readWorkflow("web-selftest.yml");
  // Callable: the PR gate. Without this pr.yml cannot call it and nothing gates.
  assert.match(yaml, /^ {2}workflow_call:$/m, "web-selftest.yml must stay callable — pr.yml's gate depends on it");
  // Master: the signal run (#2762). Without this a regression that lands stays
  // invisible until some later PR happens to touch these paths.
  assert.match(yaml, /^ {2}push:\n {4}branches: \[master\]$/m, "web-selftest.yml must keep its master push trigger");
  // A second `pull_request:` trigger would double every PR run — once standalone,
  // once through pr.yml — and the standalone one would gate nothing.
  assert.doesNotMatch(yaml, /^ {2}pull_request:$/m, "the PR run goes through pr.yml now, not a pull_request trigger");
});
