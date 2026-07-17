---
name: gate-pr
description: Review and gate Captain Claude pull requests through CI, Greptile, play-test, and squash merge
user_invocable: true
---

# Gate Pull Request

Review a PR authored by `sachiniyer` or `app/detail-app`, verify it is truly
ready, and merge it only after the required gates pass. Detail dead-code PRs
from `app/detail-app` may auto-merge once the gates are clean.

## Steps

1. **Fetch the PR state** — inspect the base branch, author, merge state, and
   check rollup:
   ```bash
   gh pr view <n> --json number,title,url,author,baseRefName,headRefName,mergeStateStatus,statusCheckRollup
   gh pr checks <n> --watch
   ```

   - Gate only PRs authored by `sachiniyer` or `app/detail-app`.
   - Require `baseRefName` to be `master`.
   - Require `mergeStateStatus` to be mergeable or clean enough to merge.
   - Inspect `statusCheckRollup` for failing and pending checks.
   - CodeQL can report mid-run non-success states; only a completed
     conclusion is real. Do not fail a PR on an in-progress CodeQL state.

2. **Require green CI** — do not merge while any required signal is red or
   pending:
   - No `FAILURE` conclusions.
   - Zero pending checks.
   - No missing required check that is expected to report.

3. **Gate Greptile** — require one of these outcomes:
   - Greptile Confidence `5/5`.
   - Greptile Confidence `4/5` with zero unresolved inline findings.

   If a Greptile finding is valid, route it back to the authoring session and
   stop. Do not merge a PR with a valid unresolved finding.

4. **Verify branch shape** — catch stacked or tangled branches before merge:
   ```bash
   git fetch origin master
   gh pr checkout <n>
   git diff --stat origin/master
   git log --oneline origin/master..HEAD
   ```

   For decomposition PRs, `git diff --stat origin/master` must show exactly
   the one source file being decomposed, its split files, and any expected
   `scripts/file-length-allowlist.txt` or docs changes. If unrelated files
   appear, the PR is stacked or stale; send it back instead of merging.

5. **Play-test TUI-visible changes** — for `VISIBLE-TUI`, pane-focus, attach,
   or similar interaction changes, play-test before merge:
   ```bash
   git fetch origin pull/<n>/head:gate-pr-<n>
   git worktree add ../gate-pr-<n> gate-pr-<n>
   make -C ../gate-pr-<n> tui-driver-selftest
   ```

   Require the self-test to report all steps green (the count grows as the
   suite is extended — match the `N/N` in its final `SELF-TEST PASSED` line,
   not a hard-coded number). If it fails, send the failure back to the
   authoring session.

6. **Merge only after all gates pass**:
   ```bash
   gh pr merge <n> --squash
   ```

   Do not use another merge strategy unless the maintainer explicitly asks.
