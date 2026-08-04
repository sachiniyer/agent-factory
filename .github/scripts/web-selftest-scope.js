// Decides whether a change needs the Playwright web-driver selftest (#2762).
//
// The suite is a REQUIRED gate — pr.yml's Build job lists it in `needs`, which is
// what gives a red run teeth (docs/web-selftest.md, and the header on
// .github/workflows/web-selftest.yml). But it is also the most expensive thing in
// CI: a Go+Node+Chromium image, then a real daemon and a real browser. Running it
// on every PR would tax every docs typo with that; running it on none of them is
// the hole #2762 recorded. So it runs on exactly the changes that can break it.
//
// The path list below is the ONE source of truth for that decision. The `paths:`
// filter on web-selftest.yml's push trigger repeats it as literal YAML — GitHub
// gives no way to compute a trigger filter — and web-selftest-scope.test.js reads
// that file and fails if the two disagree, so the copy cannot drift silently.
//
// The workflow chooses to RUN whenever it cannot compute a diff (an unexpected
// event, a shallow clone with no base parent). This helper is only consulted with
// a real file list; "I don't know" never reaches it, because the safe answer to
// that question is to run the suite, not to ask a predicate about an empty list.

// Paths whose change can break the web selftest.
//
//   * web/**            the SPA, and the committed bundle the harness serves.
//   * daemon/**         the harness drives a REAL daemon, so its HTTP/WS surface
//     agentproto/**     and the wire types are as able to break the suite as web/
//     apiproto/**       is. #1932 (a stale header) and #1894 (a detach that
//                       dropped scroll) were both found exactly this way.
//   * the harness itself, and the CI wiring that decides whether it gates — a
//     change to this file or to pr.yml's Build `needs` must re-prove the gate on
//     the PR that makes it, not on some later one.
const SELFTEST_PATHS = [
  "web/**",
  "daemon/**",
  "agentproto/**",
  "apiproto/**",
  "scripts/testbox.sh",
  "scripts/container/Dockerfile.web-selftest",
  "scripts/container/web-selftest-entry.sh",
  ".github/workflows/web-selftest.yml",
  ".github/workflows/pr.yml",
  ".github/scripts/web-selftest-scope.js",
];

// A deliberately tiny subset of GitHub's `paths:` globbing: a trailing `/**`
// matches everything under a directory, anything else is an exact file path.
// Every pattern above is one of those two shapes and the test asserts it stays
// that way — a `*.ts`-style pattern would be silently mismatched here rather than
// half-supported, which is how a filter starts skipping the suite it exists to
// schedule.
function matchesPattern(pattern, path) {
  if (pattern.endsWith("/**")) {
    return path.startsWith(pattern.slice(0, -2));
  }
  return path === pattern;
}

/**
 * @param {string[]} changedPaths repo-relative paths changed by the PR/push.
 * @returns {{run: boolean, matched: string[]}} whether to run, and the paths that
 *   decided it (capped by the caller for logging).
 */
function scopeWebSelftest(changedPaths) {
  const matched = changedPaths.filter((path) =>
    SELFTEST_PATHS.some((pattern) => matchesPattern(pattern, path)),
  );
  return { run: matched.length > 0, matched };
}

// CLI: `node web-selftest-scope.js <file-of-changed-paths>` prints `run=true` or
// `run=false` on stdout for `>> "$GITHUB_OUTPUT"`, and the reasoning on stderr so
// the job log says why it did what it did. A missing/unreadable file is not an
// empty diff — it is a broken diff, so it prints run=true.
if (require.main === module) {
  const fs = require("node:fs");
  const file = process.argv[2];
  let changed;
  try {
    changed = fs.readFileSync(file, "utf8").split("\n");
  } catch (error) {
    process.stderr.write(`web-selftest-scope: cannot read ${file} (${error.message}) — running the suite\n`);
    process.stdout.write("run=true\n");
    process.exit(0);
  }
  const paths = changed.map((line) => line.trim()).filter((line) => line.length > 0);
  const { run, matched } = scopeWebSelftest(paths);
  if (run) {
    process.stderr.write(
      `web-selftest-scope: ${matched.length} of ${paths.length} changed path(s) are in scope, e.g.\n` +
        matched
          .slice(0, 10)
          .map((path) => `  ${path}\n`)
          .join(""),
    );
  } else {
    process.stderr.write(
      `web-selftest-scope: none of ${paths.length} changed path(s) can reach the web selftest — skipping it.\n`,
    );
  }
  process.stdout.write(`run=${run}\n`);
}

module.exports = { SELFTEST_PATHS, scopeWebSelftest, matchesPattern };
