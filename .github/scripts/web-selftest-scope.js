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
// **Go source is watched wholesale**, and that is a deliberate correction. The
// first cut enumerated the packages thought to matter (daemon, agentproto,
// apiproto) and was wrong twice in review: it missed the harness invocation chain,
// then missed `session/**` and `task/**` — which the entry script drives directly
// via `$BIN sessions create|list|get|archive|restore|kill|tab-create|tab-delete`
// and `$BIN tasks add`, with the spec round-tripping those flows in the browser.
//
// The second miss is the tell. The harness BUILDS AND RUNS THE WHOLE `af` BINARY,
// so the honest predicate is not "which packages did I remember?" — a question
// nobody answers correctly twice running — but "does this change any Go the binary
// is built from?", which is decidable. Enumerating packages made the filter's
// blind spots a function of my memory, and a gate whose blind spots nobody can
// enumerate is the #2762 failure one level in.
//
// The cost that argued for a narrow list is gone: measured cold on a runner the
// suite is ~4 minutes, and it runs in parallel with `Test` and `Test (macOS)`,
// which are longer — so a Go-touching PR pays approximately nothing in wall clock.
// Docs-, plugin- and prose-only PRs still skip it, which is what the filter is
// actually for.
//
//   * **/*.go, go.mod, go.sum   everything the binary is built from.
//   * web/**                    the SPA, and the committed bundle the harness serves.
//   * daemon/**                 kept alongside `**/*.go` on purpose: these also
//     agentproto/**             carry non-Go files (testdata, fixtures) that the
//     apiproto/**               suite can depend on. #1932 (a stale header) and
//                               #1894 (a detach that dropped scroll) were both
//                               found exactly this way.
//   * the harness itself — the WHOLE invocation chain, not just its middle. CI
//     runs `make web-selftest-container`, so the closure is Makefile ->
//     scripts/testbox.sh -> Dockerfile.web-selftest + web-selftest-entry.sh ->
//     copy-src.sh (the entry sources it to stage /src into /work). Miss a link and
//     a PR editing only that link merges without running the suite it just
//     changed, which is the same "reads as coverage" failure as #2762 itself.
//   * the CI wiring that decides whether it gates — a change to this file or to
//     pr.yml's Build `needs` must re-prove the gate on the PR that makes it, not
//     on some later one.
const SELFTEST_PATHS = [
  "**/*.go",
  "go.mod",
  "go.sum",
  "web/**",
  "daemon/**",
  "agentproto/**",
  "apiproto/**",
  "Makefile",
  "scripts/testbox.sh",
  "scripts/container/Dockerfile.web-selftest",
  "scripts/container/web-selftest-entry.sh",
  "scripts/container/copy-src.sh",
  ".github/workflows/web-selftest.yml",
  ".github/workflows/pr.yml",
  ".github/scripts/web-selftest-scope.js",
];

// A deliberately tiny subset of GitHub's `paths:` globbing, matching what the
// trigger filter does for the three shapes actually used:
//
//   `**/*.ext`  any file with that extension, at any depth INCLUDING the root
//               (GitHub's `**` matches zero or more directories, so `**/*.go`
//               covers `main.go` as well as `daemon/x.go`)
//   `dir/**`    everything under a directory
//   anything else is an exact file path
//
// The test asserts every pattern stays one of those three — a shape this does not
// implement would be silently mismatched rather than half-supported, which is how
// a filter starts skipping the suite it exists to schedule.
function matchesPattern(pattern, path) {
  if (pattern.startsWith("**/*.")) {
    // slice(4) keeps the dot: "**/*.go" -> ".go", so "cargo.go" matches on the
    // extension but "going.md" cannot match on a bare suffix.
    return path.endsWith(pattern.slice(4));
  }
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
