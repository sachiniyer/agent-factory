# Auto Gate enforcement

Auto Gate publishes two kinds of check run:

- `Auto Gate decision / PR #N / SHA` is the composite decision for one exact
  `(pull request, head commit)` pair. Its dynamic name makes the evidence
  unambiguous, but GitHub rulesets cannot require every possible dynamic name.
- `Auto Gate decision` is the fixed-name, commit-scoped aggregate owned by the
  built-in GitHub Actions app. The master ruleset requires this check from that
  app. It passes only when every open pull request to `master` at the commit has
  a passing composite decision.

## Play-test evidence

A PR touching production files under `app/`, `ui/`, or `session/tmux/`
requires the `play-tested` label and a comment from an account in the gate's
`ALLOWED_AUTHORS` set. After testing, start the comment with this exact line,
using the full 40-character SHA of the commit actually tested:

```text
Play-tested commit: <full tested commit SHA>
```

Put the command, result, and signature on subsequent lines. The latest matching
comment is the attestation; the label alone no longer satisfies the gate.
Existing labeled PRs need a comment identifying their actual tested commit.
Do not substitute the current head unless that is the code you exercised.

An attestation for the current head passes directly. For an older commit, the
gate compares complete Git tree snapshots of the tested commit and current
head, restricted to the same gated paths and excluding `_test.go` files.
Evidence survives a master merge or rebase when those files are unchanged.
Content, path, or file-mode changes require another play-test and a new comment;
merge shape alone cannot exempt a conflict resolution. Missing or truncated
trees block verification. Removing the label also blocks the automatic gate.
The existing manual-path advisory policy for the TUI requirement is unchanged.

This uses snapshot equality rather than the compare API's merge-base diff,
which can omit differences between rebased heads and truncate its file list.
The decision names both the tested SHA and the head it covers.

## Shared heads

Two pull requests can point at the same commit. Their composite decisions remain
independent, but their fixed aggregate is necessarily shared because GitHub check
runs belong to commits. If PR B has an unresolved finding, the aggregate blocks
PR A too. The check summary names every blocking PR and its current reason.

You do not have to merge B to unblock A. Decouple the commit by doing one of the
following, then run the **Auto Gate** workflow manually with the remaining PR
number:

- push either branch to a distinct commit (an empty commit is sufficient when
  no content change is wanted);
- close the other pull request; or
- retarget the other pull request away from `master`.

The manual workflow uses the base repository's built-in Actions token. It does
not require a PAT, custom GitHub App, or ruleset bypass.

Auto Gate never auto-merges fork heads. For a non-allowlisted author, however,
the required decision uses the manual-only path and lists any unmet
automatic-merge requirements in its summary, restoring the normal
maintainer-review path for external contributions once its answerable blockers
are cleared.

**Live Codex findings are the exception, and they block that pass** (#3558). A
finding is a claim about the code; it is no less true because of who opened the
PR, so an unanswered inline finding — or one marked `RESOLVED` with no commit
pushed after it — makes the decision fail rather than pass, and the summary
names it above the advisory list, with the recovery that actually clears **that**
blocker: an unanswered finding takes a threaded `RESOLVED`, `ACCEPTED` or
`[gate-ack]` reply, while one already marked `RESOLVED` needs a commit pushed
after it (or `ACCEPTED` / `[gate-ack]` to withdraw the claim) — a second
`RESOLVED` cannot clear that one.

**An absent or stale verdict blocks that pass too** (#4091). The test is not "is
it a finding" but "can a maintainer answer it per item, without the author
iterating": post `@codex review` on the current head, and the blocker clears when
Codex returns a fresh covering verdict. The missing play-tested label remains an
advisory note as a separate policy choice; #4091 changes only the review-verdict
requirement.

**An unreviewed reviewer-unavailable degradation also blocks that pass** (#3825),
but it has a different exit because another review request cannot produce a
verdict. It takes the approval marker below. The absent-verdict blocker is
suppressed whenever the latest exact-head Codex artifact proves the reviewer is
unavailable, even if another unmet requirement temporarily prevents degradation
from activating. The approval blocker replaces it immediately on the manual path,
so an independent advisory cannot make the required decision green with neither
a verdict nor an approval. It can appear beside a finding blocker; each item
keeps its own maintainer-only exit.

A degraded pass plus a **maintainer approval bound to the head** rides the
ordinary update-and-merge loop instead (#3790): the review requirement is
satisfied by the maintainer rather than skipped, so what is left is the mechanical
part the gate already performs for every other passing PR. The approval is an
APPROVED review from an allowed author, or a comment from one whose first line is
exactly `## Review — approve` — the maintainer account cannot approve its own PR.
That is the ENTIRE first line, exactly, not a prefix: a qualifier on the heading
(`## Review — approve, one fix owed before landing`) withholds the approval on
purpose, so a review that owes a fix cannot land on its own heading.
It is bound by `headCurrentSince` like a Codex artifact, so a push after the
sign-off returns the PR to the manual pass. It waives the review requirement and
nothing else.

An update-branch is not such a push (#3803). When the head is a merge with exactly
two parents whose second is contained in the base branch, the approval and every
Codex artifact bind to the merge's FIRST parent — the content head — because the
reviewed change did not move. Otherwise the gate's own update-branch would void
the approval it had just acted on, which is the livelock #3799 hit minutes after
#3796 landed. The decision names both heads.

**The runs that update-branch triggers arrive parked, and the gate approves them
(#3807).** The merge commit `PUT update-branch` writes is authored by the workflow
token, so every `pull_request` run it triggers is attributed to
`github-actions[bot]` and GitHub holds it in `action_required` behind "Approve and
run". Measured on #3799 (`e5ab353f`) and #3802 (`09ffb01f`): PR Validation, Docs
and Dependency review were all parked, the required checks never reported, and the
gate read them as "missing" for over half an hour until a maintainer pressed the
button by hand.

This is the token's actor, not the fork policy. The repository's fork-PR setting
is `approval_policy: first_time_contributors`, which applies to fork
contributors; these are same-repo branches, so narrowing that policy would not
help. After a successful update the gate therefore re-reads the PR — the endpoint
returns a status message, not the sha it created — and approves EVERY parked
`pull_request` run on the new head. Every one, not the workflow it happens to
require: it was never only PR Validation. A queued or running job is left alone.

Because that loop brings a behind head up to date itself, the ruleset's strict
required-status-checks policy can stay on: a hand merge no longer has to win a
race against the fleet's merge rate.

**A base that moves between the compare and the merge waits; it does not red the
run (#3808).** `PUT /pulls/N/merge` answers 405 `Base branch was modified` when
another merge lands in the window the up-to-date compare cannot close. Nobody won
that head and nothing merged, so it is not a concession — the gate reports it as
its own refusal, invalidates the aggregate as it already did, and the next
evaluation brings the head up to date. Conceding instead would exit success
having merged nothing. Any other unclassified refusal still fails loudly.

**A ruleset refusal with no proven winner preserves PASS (#3902).** GitHub can
answer 405 `Repository rule violations found` immediately after the aggregate
PASS write, before the ruleset observes that success. Writing a new failing
WAITING generation in response created a permanent loop on #3893: every run
published PASS, got 405, and erased its own PASS.

Before merging, the gate re-reads the exact aggregate check it published and
requires `conclusion=success`. It polls at most four times with delays of 250,
500 and 1000 milliseconds. If success is not observable yet, it reports a wait
and leaves the published PASS alone. It also checks the latest aggregate
generation before merging, so a newer owner still prevents the merge (#3829).
These observations narrow the propagation window; they cannot guarantee the
ruleset's internal view is already current.

After a 405 rule violation, the gate checks for a competing winner. A proven
winner keeps the existing concession behavior (#3324). With no winner, it leaves
the aggregate PASS, waits one second, and retries the merge once, re-running the
full merge preflight first. If the second attempt gets the same refusal, the
run reports the wait and leaves PASS green for propagation and the next run.
If that preflight no longer passes, the aggregate is invalidated as before.
An unreadable ownership check is not proof of no winner: it stays loud and does
not overwrite an unknown owner. A successful merge still invalidates the old
shared-head authorization because master has advanced.

The evidence for "another transaction owns this head" is the newest published
generation of the aggregate check, ordered by the timestamps a check run
actually carries. A check run has **no `created_at`** — the resource exposes
`started_at` and `completed_at` — so a generation comparison written against
`created_at` silently compares nothing (#3827).

The hand gate in `.claude/skills/gate-pr.md` runs precisely where this one
cannot — on a PR that changes `auto-gate.js`, since Auto Gate runs master's copy
of the helper, and during a Codex outage. For finding artifacts that bind to no
head, that gate now CALLS `unansweredFindingArtifacts` from this script rather
than restating the rule in jq: the restatement had drifted, and the shape that
merged #3656 passed the hand gate for months after this script started blocking
it (#3773). A test runs the skill's recipe and fails if the two disagree.

A push never clears an inline finding by itself, and where the thread currently
points is no part of the test. Moving the code a thread was anchored to only
marks that thread outdated, which a rebase or a fix to the neighbouring line does
as readily as the fix itself — so an outdated thread blocks exactly like any
other until it is answered in-thread (#3689).

When the latest Codex response against the head is reviewer-unavailable, no
verdict arrived, so the gate degrades to maintainer review rather than wait
indefinitely. That degradation is a block, not a pass (#3819): the decision
stays red with the unmet item

> awaiting maintainer review — post `## Review — approve` on this head

and a maintainer approval bound to the head clears it. On an allowlisted author's
PR the gate then merges it itself; on any other author's the PR is a manual merge
regardless, so the approval restores the manual pass and the maintainer lands it
by hand. Either way the marker is what clears it, and either way the decision is
red until someone posts one — including on external PRs, whose conclusion is
computed from the blocker list rather than the unmet list (#3825). It used to
publish a green manual-only pass instead, which made the PR mergeable while
leaving the review to a convention — #3760 landed that way with no review of any
kind, and #3824 closed that everywhere except the external path, which kept
publishing the same green pass until #3825.

The known responses keep distinct causes: the usage-limit family is
`usage-limit`, and `Codex Review: Something went wrong. Try again later by
commenting "@codex review". Unknown error` is `failure` (#3951). The production
response “To use Codex here, [create an environment for this
repo](https://chatgpt.com/codex/cloud/settings/environments).” exposed why
enumerating vendor stems is not sufficient (#3985). Classification is now
structural: a top-level pull-review comment is a finding surface and never
availability evidence (`pull_request_review_id` set with no `in_reply_to_id`,
#3989). After also preserving a review body (`CODEX_REVIEW_RE` plus
`REVIEWED_COMMIT_RE`), an automatic-review body (the Codex Review heading with a
clean result, suggestions, or findings), a finding-shaped inline reply, and a
parseable summary or verdict, every other non-empty Codex-authored artifact is
reviewer-unavailable with kind `unrecognised`. Its first body line is retained as the cause so the
gate summary names what it saw. Silence remains ordinary missing-review state;
the absence of an artifact is not an unrecognised artifact. All unavailable
responses use the same strictly-after-`headCurrentSince` timing rule and
maintainer-review requirement.

The shared outage record treats `usage-limit` and `failure` as strong enough to
open an episode. An `unrecognised` response is weaker evidence of a
repository-wide outage, so it extends an open episode but never opens one alone;
within an open episode it remains strong evidence that no review happened on
that PR. The record preserves all three causes. When a notice adopts the
repository episode's earlier start, it uses specific wording only if that
episode has the same single cause as the local answer. Mixed or differing causes
use “Codex unavailable since” and list the recorded causes in onset order.
Without a usable record, the local answer supplies both the start and the cause.
A real review quoting an unavailable message remains a review.

`<!-- codex-pull-request-review-summary -->` identifies the maintained
“Codex Review Summary” activity comment. Its edit time is excluded from
latest-response selection: an edit cannot supersede an earlier head-current
unavailable answer. A Completed row is **never a verdict by itself** (#4052).
It may supply a completion timestamp only when a Codex-authored artifact for
the same commit corroborates it: a `Reviewed commit:` prose verdict, a submitted
pull-request review (`/pulls/N/reviews`, matching `commit_id`), or at least one
top-level inline review comment (`/pulls/N/comments`, matching `commit_id`).
The row and corroborating artifact must both post-date the head's push anchor
(`headCurrentSince`). An inline comment uses its creation time, so editing it
cannot refresh old evidence. A submitted review without the prose footer must
carry a known automatic-review body: the Codex review heading with a clean
result, automated-suggestions wrapper, or findings. Empty wrappers and arbitrary
submitted bodies do not count. Every reviewer-unavailable classification,
including `unrecognised` (such as the environment-missing response), is rejected
as corroboration and retains its outage meaning. Replies do not corroborate a
review, and body links alone do not corroborate one.
For each row, the existing push floor is advanced to the latest preceding
unavailable Codex response for that commit. The corroborating artifact must be
strictly newer than this combined floor: an earlier successful review cannot
certify a later bare Completed row after a failed retry on the same head.
Unavailable issue comments bind by time; reviews and replies naming another
commit do not reset the floor. Response creation/submission times are used so
later edits cannot hide an earlier failed attempt.

This preserves #3606: an automatic review with real artifacts counts even when
it omits the prose footer. The gate summary identifies the corroborating review,
prose verdict, or inline comment count. A bare row instead reports “Codex has not
reviewed head”; a usage-limit notice still requires maintainer approval through
the degraded path.

Running rows, malformed rows, and uncorroborated Completed rows are excluded
from availability ordering as well as verdict selection. A maintained summary
is status, never an unrecognised outage response. The repository outage record
uses the same corroboration rule for current and superseded commits, with
commit dates, PR creation, force-push history and recorded head announcements
supplying historical freshness floors. Only merge accounting requires the merged head specifically.
Recovery uses the row's own time, never the summary edit time, and cannot be
earlier than the corroborating artifact. A later artifact therefore cannot
backdate a recovery or erase an earlier degraded merge.

Reviewer-unavailable evidence includes Codex inline review replies
(`in_reply_to_id` set), including replies carried by an empty `COMMENTED` review
(#3900). The reply's
`commit_id` must match the head, and its `created_at` must be strictly later than
`headCurrentSince`; an edit cannot refresh an old answer. The latest artifact
across issue comments, reviews and eligible replies wins (the reply wins a tie
with its empty enclosing review), and a later real verdict supersedes the
unavailable response. The decision names the inline comment id. Replies do not
supply verdicts or enter the body-finding artifact list, and finding-shaped text
is not accepted as an inline outage answer. The existing finding predicates are
unchanged.

What counts as that observation is one rule, stated once as `CODEX_LIMIT_RULE` in
`.github/scripts/auto-gate.js` and quoted here verbatim because a test requires
every statement of it to agree (#3744):

> A Codex usage-limit message counts as a review outage unless its
> scope clause names a job this repository has OBSERVED naming something other
> than review. An unobserved phrasing counts, because a false block during a real
> outage has no exit while a false degrade leaves a maintainer-review exit a
> human can take.

Auto Gate disables any pending GitHub-native auto-merge
request before publishing that pass. Review and review-comment workflows can be
read-only for fork pull requests; if one cannot update the decision, run Auto
Gate manually by PR number from the base repository. The `pull_request_target`
lifecycle trigger only executes the default-branch helper; it never checks out
or executes pull-request code.

## Ruleset

The `master` ruleset requires the fixed `Auto Gate decision` check from the
built-in GitHub Actions app (app ID 15368). It must not grant repository roles a
bypass: a maintainer's direct merge is supposed to meet the same required check
as the workflow's merge. The stable-release deploy key may retain its narrow
bypass because that non-session path updates the release commit directly.

## Queued-only deduplication

Concurrency belongs to evaluation jobs, never the workflow. Every run follows
this dependency graph:

```text
auto-gate (resolve event heads, ungrouped)
  -> invalidate-gate (ungrouped, including retries)
    -> apply-gate (one reusable-workflow call per invalidated head)
      -> aggregate transaction (head-serialized evaluation/report/merge)
```

Neither the resolver nor invalidation has a concurrency group or waits on a
grouped job. Every event can therefore invalidate while an older transaction
is running; dedupe cannot discard an event before its invalidation attempt.
Only successfully invalidated heads enter evaluation. This preserves generation
ownership checks immediately before PASS and merge, including write retries.
Runner availability and API failures still apply; concurrency adds no wait here.

The calling evaluation job holds `auto-gate-target-<target>-head-<head SHA>`
for the entire reusable aggregate transaction. The target is the issue or PR
number, dispatch PR number, workflow-run head SHA, check-suite head SHA, or
status SHA (in that order), falling back to the unique run ID. With
`cancel-in-progress: false`, newer pending evaluation jobs replace older pending
jobs for that target/head; active transactions are never cancelled. Every
comment event remains subscribed, including marker replies. The surviving job
re-reads current PR, review, and check state.

`resolveAggregateHeads` consumes a synchronize payload's `before` in the
ungrouped resolver, and invalidation covers both current and previous heads.
The matrix creates a separate evaluation call for each head. Its head suffix
prevents a later same-PR comment about the current head from replacing the
previous-head refresh; that refresh still reevaluates other PRs sharing the
previous commit. This also preserves a head resolved before a subsequent push.

The reusable workflow keeps the existing `auto-gate-aggregate-<head SHA>` job
lane with `cancel-in-progress: false`. Different PR or SHA targets sharing a
commit still serialize there. The target and aggregate prefixes are distinct,
so the caller never waits on a lock it already holds.

## Event and merge ordering

Each subscribed run first creates a new non-green aggregate generation
without waiting for the head's serialized lane. It then refreshes every
associated PR/head decision, republishes its own generation, and considers a
merge inside that lane. If a newer event invalidates the head while the older
transaction is running, the older generation refuses to publish PASS; updating
an older check run also cannot supersede the newer check-run generation. A head
synchronization reevaluates both the new head and the previous head because the
set of associated PRs changed for both commits. After a successful merge, the
same transaction explicitly makes the old-head aggregate non-green; it does not
depend on a `closed` event that GitHub may suppress for token-authenticated
writes.

GitHub suppresses `check_suite` recursion for suites created by Actions. The
required `Lint` and `Build` jobs both belong to **PR Validation**, so Auto Gate
also subscribes to that workflow's terminal `workflow_run` event. This ensures
their completed state is reevaluated without subscribing Auto Gate to itself.

GitHub also suppresses `push` workflows when Auto Gate merges with its
`GITHUB_TOKEN`. After a merge, the gate therefore dispatches the five
master-verification workflows named by `MASTER_PUSH_WORKFLOWS`: Build, Docs,
Lint, TUI driver selftest, and Web selftest. A dispatch ignores each workflow's
path filter, so all five run after every Auto Gate merge. The literal list is
kept honest by the required auto-gate helper test, which scans workflow triggers
and requires every listed workflow to accept `workflow_dispatch`.

The gate runs master's pre-merge helper. A merge that first adds or renames one
of those workflows cannot re-dispatch that new entry for its own landing commit;
the post-merge warning names the gap, the maintainer dispatches that first run,
and every later merge uses the updated list automatically.

Repository-ruleset changes and mergeability changes caused only by `master`
advancing have no GitHub event here. Use the same manual PR-number dispatch to
refresh that observational state. The destructive merge path still reevaluates
the target PR, every other associated PR, and the association set immediately
before its write.

## Head-branch pruning

`delete_branch_on_merge` does not fire for a `GITHUB_TOKEN` merge, so the gate
deletes the head branch itself (#3603). Three conditions have to hold, and each
one no-ops rather than failing — the merge has already landed, and no pruning
problem is worth reporting it as a failure:

- **(a)** the head repository is this repository — a fork's branch is not ours,
  and the token could not delete it anyway;
- **(c)** no open PR is based on the branch, and no open PR still has it as a
  head — deleting it would close the first and leave the second headless;
- **(b)**, last and immediately before the delete, the branch still points at the
  commit that was merged. A lane that pushed after the merge keeps its branch:
  that work is not in `master`, and the pushed ref may be its only copy. Neither
  the REST ref API nor GraphQL's `deleteRef` accepts an expected OID, so ordering
  is the only lever there is.

Those conditions are correct when the merge asks them and nothing revisits the
answer, so a branch kept for a reason that later disappears leaks forever
(#3852): #3847 was the base of the open #3849, and #3849 was retargeted onto
`master` 89 seconds after #3847 merged.

The resolver job therefore runs a **sweep** on every gate run, including the runs
where nothing merges. It enumerates every branch on the repository in one
GraphQL query — name, tip, ruleset rules, and the pull requests whose head it is
— and treats a branch as a candidate only when a merged PR of this repository has
it as a head and its tip still equals that merge's head. The default branch, a
protected branch, and a branch whose rule list could not be read in full are
never candidates. Each candidate then goes through the same helper the merge path
uses, so the three conditions have one home and get fresh reads. The pass is
capped per run, so a backlog drains over several runs rather than making one run
slow, and its one-line summary is the `branch_sweep` output of the resolver job.

Deleting a ref is the gate writing to the repository on its own, so the sweep is
gated on `AUTO_GATE_ENABLED` like the merge itself. With that switch off nothing
merges, so nothing new leaks either.

## Reviewer outage record

Master Health Watch (task `4ab7ba4f`, hourly) owns one comment on #3932,
identified by `<!-- codex-reviewer-outage:v1 … -->`. Each run executes:

```sh
node .github/scripts/codex-outage.js sachiniyer/agent-factory
```

The helper creates that comment once, then updates it in place. It preserves
completed outage episodes, closes each at the first real `Reviewed commit:`
verdict, and names the start, latest notice, elapsed hours, and merged PRs.
A later outage adds another episode to the same record. No issue is closed and
no new issue or per-PR outage comment is opened. Run with a final `--dry-run`
argument to inspect the proposed body without writing it.

The sweep paginates PRs in updated order back to the #3932 evidence window
(2026-09-05 UTC) on bootstrap, including open and closed PRs. Later sweeps use a
named 24-hour recompute window: a completed episode's boundaries and outage
evidence are frozen only when
its recovery is older than `now - 24h`. The sweep scans from 1 ms after the
latest frozen recovery, or from the bootstrap boundary when none is frozen, and
re-aggregates everything after that boundary with the current classifier. This
lets corrected classification discover earlier evidence and wholly missed
recent episodes without re-aggregating frozen history. With no episode inside
the window, the latest frozen recovery supplies the same scan start as before.
Older history therefore cannot disappear and does not require rescanning all
past PRs. An episode starts at the first observed known limit or transient
failure, not the preceding verdict. An unrecognised response extends an episode
already open but does not open one by itself. The sweep reads **both**
`/pulls/N/comments` and
`/issues/N/comments` unfiltered, plus review bodies. The shared classifier
structurally removes top-level pull-review comments from that feed before body
classification; those artifacts are finding surfaces, while replies retain the
finding-shaped body guard. It reconstructs degraded merges using #3932's method:
a reviewer-unavailable response whose artifact timestamp falls inside the
episode and before merge, plus no real verdict covering the actual merged head
before merge. Each degraded merge is attributed once, to the episode holding
the latest qualifying notice at or before that merge, even when the merge lands
after recovery. Scanned merged PRs have their attribution refreshed across both
rebuilt and frozen episodes, so merges after the 24-hour boundary are counted
too; unscanned PRs retain their recorded counts. This is historical coverage
accounting, not a second
implementation of the merge gate; the count is labelled with its method in the
record. An unrecognised artifact before the episode is not evidence. Late
reviews cannot undo a degraded merge. The shared `codexEvidence` export from
`auto-gate.js` supplies structural classification, quotation/finding exclusions,
and verdict parsing. Finding predicates and the hand gate's jq are unchanged.

On a degraded evaluation, Auto Gate reads this record once and writes the
outage duration to the job summary. It labels the watch's observation time;
between sweeps, or if the record cannot be read, it falls back to the duration
observed on the PR and says so. The record never authorizes a merge, and an
unreadable record never changes a gate decision. Per-head unavailable evidence
and maintainer approval are still required exactly as before.

The live task prompt must run the helper before its no-findings early exit.
Until this helper lands on master, it skips that command when the file is absent.
The watch is the only writer; do not run overlapping write sweeps. A failed API
read aborts before updating the record, preserving the previous successful sweep.
