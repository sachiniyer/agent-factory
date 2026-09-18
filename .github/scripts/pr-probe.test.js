// The probe-mode wiring in pr.yml (#4563, docs/dev/probe-runs.md).
//
// A probe is a workflow_dispatch of PR Validation that runs only the Linux Test
// job. It can land on a PR's head, and every job in the file reports a check run
// there, skipped ones included. auto-gate.js reads the required "Build" and
// "Lint" by exact name and takes the newest, and the ruleset counts a skipped
// check as passing. So a probe that reported under one of those names would
// either pass a PR its real run failed or block one its real run passed. These
// tests pin the three things that prevent that: what a probe skips, what its
// jobs are called, and how its inputs reach a shell. They also pin the probe
// step's own behavior, by running it against a stub `go`.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");
const test = require("node:test");

const PR_YML = fs.readFileSync(path.join(__dirname, "..", "workflows", "pr.yml"), "utf8");

/** Job id -> { name, if, needs, body } for pr.yml's top-level jobs. */
function jobs(yaml) {
  const lines = yaml.split("\n");
  const start = lines.findIndex((line) => /^jobs:\s*$/.test(line));
  assert.notEqual(start, -1, "no `jobs:` block found");
  const found = new Map();
  let current = null;
  for (const line of lines.slice(start + 1)) {
    const header = line.match(/^ {2}([A-Za-z0-9_-]+):\s*$/);
    if (header) {
      current = { name: null, if: null, needs: [], body: [] };
      found.set(header[1], current);
      continue;
    }
    if (!current) continue;
    current.body.push(line);
    const key = line.match(/^ {4}(name|if|needs):\s*(.*)$/);
    if (!key) continue;
    if (key[1] === "needs") {
      const list = key[2].match(/^\[(.*)\]$/);
      assert.ok(list, `only inline needs lists are parsed here: ${line}`);
      current.needs = list[1].split(",").map((entry) => entry.trim()).filter(Boolean);
    } else {
      current[key[1]] = key[2];
    }
  }
  return found;
}

const PROBE_SKIP = "${{ !inputs.probe }}";
const BUILD_IF = "always() && !inputs.probe";
const PROBE_NAME = /^\$\{\{ inputs\.probe && '([^']+)' \|\| '([^']+)' \}\}$/;

/** The name a job reports on a PR run (probe unset). */
function prName(job) {
  const match = job.name.match(PROBE_NAME);
  return match ? match[2] : job.name;
}

test("a normal PR reports exactly the check names it reported before probe mode", () => {
  // The one acceptance line that cannot be seen from a probe run: a PR's own
  // check set must not move. Build and Lint are the required contexts.
  assert.deepEqual(
    [...jobs(PR_YML).values()].map(prName),
    [
      "Lint",
      "Test",
      "Registry-free image build",
      "Test (systemd, ${{ matrix.os }})",
      "Web",
      "Web and performance scope",
      "Web selftest",
      "Performance and visual baselines",
      "Test (macOS)",
      "Build",
    ],
  );
});

test("a probe runs the Linux Test job and nothing else", () => {
  const all = jobs(PR_YML);
  const skipped = new Set();
  // Resolve in dependency order: a job with no status function in its `if:`
  // is skipped whenever one of its needs is.
  let changed = true;
  while (changed) {
    changed = false;
    for (const [id, job] of all) {
      if (skipped.has(id)) continue;
      const bySelf = job.if === PROBE_SKIP || (id === "build" && job.if === BUILD_IF);
      const statusFunction = /\b(always|failure|cancelled|success)\(\)/.test(job.if || "");
      const byNeed = !statusFunction && job.needs.some((need) => skipped.has(need));
      if (bySelf || byNeed) {
        skipped.add(id);
        changed = true;
      }
    }
  }
  const running = [...all.keys()].filter((id) => !skipped.has(id));
  assert.deepEqual(running, ["test"], "a job was added to pr.yml without the probe skip — add `if: ${{ !inputs.probe }}`");
  assert.equal(all.get("test").if, null, "the test job is the probe; it must not be conditional");
});

test("no job reports a probe row under a name a PR run uses", () => {
  const all = jobs(PR_YML);
  const prNames = new Set([...all.values()].map(prName));
  for (const [id, job] of all) {
    if (/needs\./.test(job.if || "")) {
      // These skip on ordinary PRs, where GitHub shows an unevaluated name raw,
      // so they must keep a plain one. No gate reads them.
      assert.doesNotMatch(job.name, /\$\{\{/, `${id} skips on ordinary PRs, so its name must be plain`);
      assert.ok(!["Build", "Lint"].includes(job.name), `${id} must not carry a required check's name`);
      continue;
    }
    if (/\$\{\{ matrix\./.test(job.name)) {
      // A skipped matrix job never expands, so its row keeps the raw text.
      assert.equal(job.if, PROBE_SKIP, `${id}: an unexpanded matrix name is only safe on a job a probe skips`);
      continue;
    }
    const match = job.name.match(PROBE_NAME);
    assert.ok(match, `${id} runs on every PR, so its name must be \${{ inputs.probe && '…' || '…' }}, got ${job.name}`);
    assert.ok(!prNames.has(match[1]), `${id}'s probe name "${match[1]}" is a name a PR run reports`);
  }
});

test("dispatch inputs reach pr.yml only through names, conditions, the group and env", () => {
  // Never `${{ inputs.* }}` spliced into a script: that is shell injection from
  // anyone who can dispatch. Each allowed shape is an expression GitHub
  // evaluates itself, or an env entry the shell reads as data.
  const allowed = [
    /^ {4}name: \$\{\{ inputs\.probe && '[^']+' \|\| '[^']+' \}\}$/,
    /^ {4}if: \$\{\{ !inputs\.probe \}\}$/,
    /^ {4}if: always\(\) && !inputs\.probe$/,
    /^ {8}if: \$\{\{ !?inputs\.probe \}\}$/,
    /^ {2}group: \$\{\{ inputs\.probe && 'probe-' \|\| 'pr-' \}\}\$\{\{ github\.event\.pull_request\.number \|\| github\.ref \}\}$/,
    /^ {10}PROBE_(PACKAGES|RUN): \$\{\{ inputs\.(packages|run) \}\}$/,
  ];
  const uses = PR_YML.split("\n").filter((line) => /inputs\./.test(line) && !line.trim().startsWith("#"));
  assert.ok(uses.length > 0, "found no inputs use at all; this test is reading the wrong file");
  for (const line of uses) {
    assert.ok(allowed.some((shape) => shape.test(line)), `inputs used in an unreviewed shape: ${line.trim()}`);
  }
  assert.doesNotMatch(PR_YML, /github\.event\.inputs/, "use the typed inputs context, not github.event.inputs");
});

test("a probe has its own concurrency group", () => {
  // Sharing pr-refs/heads/<branch> with Auto Gate's recovery dispatch on the same
  // branch would let each cancel the other, and a cancelled need turns Build red.
  // Every other event keeps the pr-… group it always had.
  assert.match(
    PR_YML,
    /^concurrency:\n(?: {2}#.*\n)* {2}group: \$\{\{ inputs\.probe && 'probe-' \|\| 'pr-' \}\}\$\{\{ github\.event\.pull_request\.number \|\| github\.ref \}\}\n {2}cancel-in-progress: true$/m,
  );
});

test("a dispatch without inputs is still the full run Auto Gate's recovery expects", () => {
  // auto-gate.js ensureValidationRun dispatches pr.yml with no inputs (#3807).
  assert.match(PR_YML, /^ {6}probe:\n(?: {8}.*\n)*? {8}type: boolean\n {8}default: false$/m);
});

/** A step's run command within pr.yml's test job, dedented. */
function stepRun(stepName) {
  const lines = PR_YML.split("\n");
  // Scoped to the test job: test-macos has a step named "Run tests" too.
  const job = lines.indexOf("  test:");
  assert.notEqual(job, -1, "no test job");
  const at = lines.findIndex((line, i) => i > job && line === `      - name: ${stepName}`);
  assert.notEqual(at, -1, `no step named ${stepName} in the test job`);
  assert.ok(!lines.slice(job + 1, at).some((line) => /^ {2}[A-Za-z0-9_-]+:\s*$/.test(line)), `${stepName} is not in the test job`);
  const single = [];
  for (let i = at + 1; i < lines.length && !/^ {6}- /.test(lines[i]) && lines[i].trim() !== ""; i += 1) {
    single.push(lines[i]);
  }
  const inline = single.find((line) => /^ {8}run: (?!\|)/.test(line));
  if (inline) return inline.replace(/^ {8}run: /, "");
  const block = [];
  let started = false;
  for (let i = at + 1; i < lines.length; i += 1) {
    if (!started) {
      if (/^ {8}run: \|$/.test(lines[i])) started = true;
      continue;
    }
    if (lines[i].trim() !== "" && !/^ {10}/.test(lines[i])) break;
    block.push(lines[i].slice(10));
  }
  assert.ok(started, `step ${stepName} has no run block`);
  return block.join("\n");
}

test("a probe runs go test with the same flags as the real Linux run", () => {
  // A probe that used other flags (no -race, another -timeout) could go green
  // or red where the real run would not. #4556 is moving this budget; whichever
  // of the two lands second must move both.
  const real = stepRun("Run tests").match(/^go test (.*) \.\/\.\.\.$/);
  assert.ok(real, "Run tests no longer reads `go test <flags> ./...`");
  const probeScript = stepRun("Run probe tests");
  const probe = probeScript.match(/^go test (.*) "\$\{run_args\[@\]\}" "\$\{packages\[@\]\}" /m);
  assert.ok(probe, "the probe step no longer reads `go test <flags> \"${run_args[@]}\" \"${packages[@]}\"`");
  assert.equal(probe[1], real[1]);
  assert.doesNotMatch(probeScript, /\$\{\{/, "the probe script must read its inputs from env only");
});

// ── the probe step itself, run against a stub go ─────────────────────────────

function runProbe({ packages = "", run = "", goOutput = "", goExit = 0 }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "af-pr-probe-"));
  try {
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(dir, "out.txt"), goOutput);
    fs.writeFileSync(
      path.join(bin, "go"),
      `#!/bin/bash\nprintf '%s\\n' "$@" > "${dir}/args.txt"\ncat "${dir}/out.txt"\nexit ${goExit}\n`,
      { mode: 0o755 },
    );
    const result = spawnSync("bash", ["-e", "-c", stepRun("Run probe tests")], {
      encoding: "utf8",
      env: {
        PATH: `${bin}:${process.env.PATH ?? "/usr/bin:/bin"}`,
        PROBE_PACKAGES: packages,
        PROBE_RUN: run,
        RUNNER_TEMP: dir,
        GITHUB_STEP_SUMMARY: path.join(dir, "summary.md"),
      },
    });
    const argsFile = path.join(dir, "args.txt");
    const args = fs.existsSync(argsFile) ? fs.readFileSync(argsFile, "utf8").trimEnd().split("\n") : null;
    const summaryFile = path.join(dir, "summary.md");
    const summary = fs.existsSync(summaryFile) ? fs.readFileSync(summaryFile, "utf8") : "";
    return { status: result.status, stdout: result.stdout, args, summary };
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

const PASSED = "=== RUN   TestDefaultConfig\n--- PASS: TestDefaultConfig (0.00s)\nok  \tx/config\t0.01s\n";

test("probe inputs become go test arguments, and nothing else", () => {
  const flags = stepRun("Run tests").match(/^go test (.*) \.\/\.\.\.$/)[1].split(" ");
  let got = runProbe({ goOutput: PASSED });
  assert.equal(got.status, 0);
  assert.deepEqual(got.args, ["test", ...flags, "./..."], "no packages means the whole module");

  got = runProbe({ packages: " ./config/  ./session/... ", run: "^TestA$|-x y", goOutput: PASSED });
  assert.equal(got.status, 0);
  assert.deepEqual(got.args, ["test", ...flags, "-run", "^TestA$|-x y", "./config/", "./session/..."]);
});

test("a probe refuses a package that is not a ./ path inside the module", () => {
  for (const packages of ["-toolexec=/bin/sh", "config", "./../etc", "./a/../../x", "./config/ $(id)", "./conf*"]) {
    const got = runProbe({ packages, goOutput: PASSED });
    assert.equal(got.status, 1, packages);
    assert.equal(got.args, null, `${packages}: go must not be invoked`);
    assert.match(got.stdout, /^::error::each package must be a \.\/-relative pattern/m, packages);
  }
});

test("a probe refuses an input with a line break", () => {
  for (const input of [{ run: "^TestA$\n::add-mask::x" }, { packages: "./config/\r" }]) {
    const got = runProbe({ ...input, goOutput: PASSED });
    assert.equal(got.status, 1, JSON.stringify(input));
    assert.equal(got.args, null);
    assert.match(got.stdout, /^::error::packages and run must each be one line/m);
  }
});

test("a green probe that ran no test is refused, because go test exits 0 on it", () => {
  const got = runProbe({ run: "^TestNope$", goOutput: "testing: warning: no tests to run\nok  \tx\t0.01s [no tests to run]\n" });
  assert.equal(got.status, 1);
  assert.match(got.stdout, /^::error::the probe ran no tests/m);
});

test("a red probe keeps go test's exit and names the failing assertion lines only", () => {
  const got = runProbe({
    goExit: 1,
    goOutput: [
      "=== RUN   TestA",
      "=== PAUSE TestA",
      "=== RUN   TestB",
      "    b_test.go:5: a log line from a test that passes",
      "--- PASS: TestB (0.00s)",
      "=== CONT  TestA",
      "    a_test.go:28: Should NOT be empty",
      "--- FAIL: TestA (0.00s)",
      "=== RUN   TestC",
      "--- SKIP: TestC (0.00s)",
      "FAIL",
      "",
    ].join("\n"),
  });
  assert.equal(got.status, 1);
  assert.match(got.summary, /a_test\.go:28: Should NOT be empty\n--- FAIL: TestA/);
  assert.doesNotMatch(got.summary, /b_test\.go/, "a passing test's log line is not a failure");
  assert.match(got.stdout, /^::warning::1 test\(s\) skipped/m);
});
