// The cascade guard for the web-driver selftest (#2813/#2816).
//
// THE BUG CLASS, stated once. The selftest runs sequentially in one worker against
// one shared `page`. Playwright discards that worker after ANY failure and re-runs
// `beforeAll`, which logs in again and leaves the app at a KNOWN baseline. So a test
// that depends only on that baseline is fine after a failure — it is rebuilt for it.
//
// What is NOT rebuilt is everything a *previous test* did on top of the baseline:
// which session is attached, which row is selected, which top-level view is active.
// A test that inherits one of those does not merely depend on its predecessor
// passing — it FAILS when its predecessor fails. One flake becomes two, the second
// failure describes a behavior that was never exercised, and whoever reads the run
// is told the keyboard model is broken when only the setup was missing. That is
// exactly what happened when `#2681 mouse-mode` flaked and took `#1694 keyboard`
// down with it, on three unrelated branches in one afternoon.
//
// The spec's own header already states the rule ("a new test belongs at the top
// level unless it consumes state another test produced"). Nothing enforced it, so
// two tests drifted across it. This is the enforcement.
//
// WHAT IT CHECKS: a top-level test that drives the SHARED page must establish
// attachment/selection/view itself before it reads or drives any of them. Tests that
// create their OWN context (`async ({ browser })`) are exempt — they start from a
// page nobody else has touched.
//
// Deliberately narrow. It does NOT flag a test for asserting on the rail as loaded:
// `beforeAll` guarantees that, and a broader rule flagged 28 of 92 tests, nearly all
// of them correct. It flags only the state a worker restart cannot restore.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const SPEC = join(webRoot, "selftest", "web-driver.spec.ts");

/** State a worker restart does NOT restore, so reading or driving it without first
 *  establishing it is an inherited precondition. */
const INHERITED_STATE = [
  /page\.keyboard\./, // which pane owns the keyboard depends on what is attached
  /af-kb-terminal/,
  /af-kb-rail/,
  /af-row-selected/,
  /af-viewtab-active/,
];

/** Calls that ESTABLISH that state. A viewtab must be CLICKED, not merely asserted
 *  about — matching an assertion here is how an earlier draft of this rule let the
 *  `[ / ]` view-cycle test through. */
const ESTABLISHERS = [
  /attachToSettledRow\(/,
  /ensureRailOnSessions\(/,
  /reprobeDeadTab\(/,
  /row\(page, *[A-Za-z_.]+\)\.click\(\)/,
  /af-viewtab\[data-view[^\n]*\)\.click\(/,
];

interface SpecTest {
  line: number;
  name: string;
  body: string[];
  ownsItsPage: boolean;
}

function parseTests(source: string): SpecTest[] {
  const lines = source.split("\n");
  const starts: number[] = [];
  lines.forEach((l, i) => {
    if (/^test\(/.test(l)) {
      starts.push(i);
    }
  });
  starts.push(lines.length);
  const out: SpecTest[] = [];
  for (let k = 0; k < starts.length - 1; k++) {
    const body = lines.slice(starts[k], starts[k + 1]);
    const named = /^test\("(.+?)"/.exec(body[0]);
    if (!named) {
      continue;
    }
    // The fixture arg is routinely wrapped across lines (`async ({\n  browser,\n})`),
    // so match over the joined signature rather than a single line.
    const signature = body.slice(0, 4).join("\n");
    out.push({
      line: starts[k] + 1,
      name: named[1],
      body,
      ownsItsPage: /async *\(\{[^}]*browser/s.test(signature),
    });
  }
  return out;
}

function firstMatch(body: string[], patterns: RegExp[]): number | null {
  for (let i = 0; i < body.length; i++) {
    if (patterns.some((p) => p.test(body[i]))) {
      return i;
    }
  }
  return null;
}

test("no shared-page selftest inherits attachment/selection/view from its predecessor (#2816)", () => {
  const source = readFileSync(SPEC, "utf8");
  const tests = parseTests(source);
  const shared = tests.filter((t) => !t.ownsItsPage);

  // Anti-vacuous, three ways. A spec restructure that breaks this parser must FAIL
  // here rather than silently reporting a clean suite — the whole failure mode this
  // guard exists to prevent is an absent signal read as a passing one.
  assert.ok(
    tests.length >= 80,
    `parsed only ${tests.length} tests from ${SPEC}: the \`^test(\` parse is blind, so this check is not looking at the suite. Fix the parser, do not lower this floor.`,
  );
  assert.ok(
    shared.length >= 50,
    `parsed only ${shared.length} shared-page tests: the browser-fixture detection is over-matching, so nearly everything is being exempted.`,
  );
  for (const pattern of [...INHERITED_STATE, ...ESTABLISHERS]) {
    assert.ok(
      pattern.test(source),
      `pattern ${pattern} matches nothing in the spec any more. It was renamed or removed, so this guard is now partly blind — update the pattern rather than deleting it.`,
    );
  }

  const offenders = shared
    .map((t) => ({ t, inherits: firstMatch(t.body, INHERITED_STATE), establishes: firstMatch(t.body, ESTABLISHERS) }))
    .filter(({ inherits, establishes }) => inherits !== null && (establishes === null || establishes > inherits))
    .map(({ t }) => `  web-driver.spec.ts:${t.line}  ${t.name}`);

  assert.deepEqual(
    offenders,
    [],
    `These selftests read or drive attachment/selection/view before establishing it, so they FAIL when the ` +
      `preceding test fails (the worker restarts with a fresh page):\n\n${offenders.join("\n")}\n\n` +
      `Establish the precondition instead of inheriting it — attachToSettledRow(page, TITLE) to attach, ` +
      `ensureRailOnSessions(page) for rail mode on the sessions view. Both are idempotent and both wait out ` +
      `the transient Lost window. Do not "fix" this by moving the test into a serial describe: that converts ` +
      `the cascade from a failure into a SKIP, which still loses the coverage.`,
  );
});
