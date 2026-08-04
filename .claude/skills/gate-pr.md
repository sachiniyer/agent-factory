---
name: gate-pr
description: Review and gate Captain Claude pull requests through CI, the Codex review, play-test, and squash merge
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

3. **Gate the Codex review** — the AI reviewer on this repo is
   `chatgpt-codex-connector[bot]`. `.github/scripts/auto-gate.js` already
   enforces the conditions below on every PR; this step is the same gate run by
   hand, so a manual merge cannot be looser than the automated one.

   ```bash
   head="$(gh pr view <n> --json headRefOid -q .headRefOid)"
   echo "head=$head committed=$(gh api repos/sachiniyer/agent-factory/commits/$head --jq .commit.committer.date)"

   # a) every Codex verdict, NEWEST FIRST. The body carries "Reviewed commit:
   #    `abc1234…`", an abbreviation that must prefix $head. Codex posts a
   #    verdict as a review on some PRs and as an issue comment on others, and
   #    can post several for one head — auto-gate.js merges both kinds into one
   #    list and judges only the newest matching artifact, so read them in that
   #    order rather than as two separate streams.
   { gh api --paginate repos/sachiniyer/agent-factory/pulls/<n>/reviews
     gh api --paginate repos/sachiniyer/agent-factory/issues/<n>/comments
   } | jq -s -r 'add
     | map(select(.user.login == "chatgpt-codex-connector[bot]"))
     | sort_by(.updated_at // .submitted_at // .created_at) | reverse
     | .[] | "=== \(.updated_at // .submitted_at // .created_at)\n\(.body)"'

   # b) live inline findings, minus the ones an allowed reply already cleared —
   #    the same subtraction auto-gate.js makes, so this can never block a PR
   #    the workflow would merge. A null .line is stale, already fixed.
   gh api --paginate repos/sachiniyer/agent-factory/pulls/<n>/comments | jq -s -r 'add
     | ([ .[] | select(.in_reply_to_id
                       and (.user.login | IN("sachiniyer", "app-detail-app", "app-detail-app[bot]"))
                       and ((.body | test("\\b(?:RESOLVED|ACCEPTED)\\b")) or (.body | contains("[gate-ack]"))))
              | .in_reply_to_id ] | unique) as $cleared
     | .[] | select(.user.login == "chatgpt-codex-connector[bot]"
                    and .line and (.in_reply_to_id | not)
                    and (.id | IN($cleared[]) | not))
     | "\(.id) \(.path):\(.line)\n\(.body)"'
   ```

   `--paginate` is not optional. A page is 30 comments, and `gh api` fetches one
   page without it — so a finding on page 2 reads as "no findings", which is the
   single wrong answer this step must never produce. `auto-gate.js` paginates
   these same lists (`github.paginate`, `per_page: 100`).

   Every filter above mirrors `auto-gate.js` exactly, in both directions: an
   author is an exact login (a `sachiniyer-…` account is not `sachiniyer`), and
   a marker is a whole word (`UNRESOLVED — sent back` contains `RESOLVED` and
   must not clear anything). Looser than the workflow lets a live finding
   through; stricter blocks a PR the workflow would merge. Neither is acceptable.

   Require all three:

   - **A verdict that names this head.** Its `Reviewed commit:` must prefix
     `headRefOid`, and its timestamp must be newer than the head commit's — the
     `committed=` value printed above, which is why it is printed. An older
     verdict read different bytes. Judge the **newest** matching artifact; an
     older one it superseded is not the gate. Silence is not a pass: an
     unreviewed PR and a clean one are byte-identical through the findings API,
     so (b) means nothing until (a) holds. If nothing has posted, comment
     `@codex review this PR` and wait — hours is normal. A `reached your Codex
     usage limits for code reviews` reply is *not* a verdict.
   - **No `P0`–`P3` in that verdict body.** Findings usually live inline, but
     Codex does sometimes file one in the body with zero inline comments.
   - **Zero unresolved live inline findings.** These are independent of the
     verdict text — a PR can carry a "didn't find any major issues" verdict for
     the exact head *and* live P1s from an earlier pass on the same head. The
     inline query overrides the verdict, never the other way round.

   Clear a finding by replying **in-thread**, from `sachiniyer` or
   `app-detail-app`, with `RESOLVED`, `ACCEPTED`, or `[gate-ack]` — all three
   clear it (`hasResolutionMarker`) — plus what changed or why the finding is
   wrong. A top-level PR comment does not clear it; the reply has to hang off
   that comment id:

   ```bash
   gh api repos/sachiniyer/agent-factory/pulls/<n>/comments/<comment-id>/replies \
     -f body='RESOLVED — <what changed, or why this is wrong>'
   ```

   The PR number belongs in that path. `pulls/comments/<id>/replies` — the shape
   the review-comment objects themselves are fetched from — is not a route, and
   answers `404 Not Found` while looking like a posted reply if you do not read
   the response.

   If a finding is valid, route it back to the authoring session and stop. Do
   not merge a PR with a valid unresolved finding.

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

   **Never `--auto`.** `gh pr merge --auto` is GitHub-native auto-merge: it
   merges as soon as the *branch ruleset's* required checks pass, which on
   `master` is `Lint` and `Build` only. It does not consult
   `.github/scripts/auto-gate.js`, so arming it routes around step 3 entirely —
   and it fires while the Codex review is still minutes-to-hours away, so the
   finding count it merges on is zero because nothing has looked yet. Either
   merge by hand once every gate above holds, or leave the PR alone and let the
   Auto Gate workflow merge it.
