# Auto Gate enforcement

Auto Gate publishes two kinds of check run:

- `Auto Gate decision / PR #N / SHA` is the composite decision for one exact
  `(pull request, head commit)` pair. Its dynamic name makes the evidence
  unambiguous, but GitHub rulesets cannot require every possible dynamic name.
- `Auto Gate decision` is the fixed-name, commit-scoped aggregate owned by the
  built-in GitHub Actions app. The master ruleset requires this check from that
  app. It passes only when every open pull request to `master` at the commit has
  a passing composite decision.

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
the required decision passes as manual-only and lists any unmet automatic-merge
requirements in its summary, restoring the normal maintainer-review path for
external contributions.

**Live Codex findings are the exception, and they block that pass** (#3558). A
finding is a claim about the code; it is no less true because of who opened the
PR, so an unanswered inline finding — or one marked `RESOLVED` with no commit
pushed after it — makes the decision fail rather than pass, and the summary
names it above the advisory list, with the recovery that actually clears **that**
blocker: an unanswered finding takes a threaded `RESOLVED`, `ACCEPTED` or
`[gate-ack]` reply, while one already marked `RESOLVED` needs a commit pushed
after it (or `ACCEPTED` / `[gate-ack]` to withdraw the claim) — a second
`RESOLVED` cannot clear that one.

**An unreviewed usage-limit degradation blocks that pass too** (#3825). The test
is not "is it a finding" but "can a maintainer answer it per item, without the
author iterating" — a finding takes a threaded reply, and a degradation takes the
approval marker below, so both leave an exit. A missing play-tested label or a
merely absent verdict has no such answer, so those stay advisory notes, and
promoting them would make every external PR unmergeable. The two blockers never
appear together: the degradation fires only when nothing else is unmet, and a live
finding is something else unmet.

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

**A required check that changes in that same window waits too, when nothing can
be proven to have won (#3827).** `PUT /pulls/N/merge` answers 405 `Repository
rule violations found` when the fixed aggregate is not green at the instant of
the write — and it is invalidated outside the head's serialized lane on purpose,
so a second evaluation of one head can flip it mid-transaction. With a winner
proven that is still a concession (#3324); with none it is the gate's own
refusal, and the next evaluation re-checks. An unreadable ownership read is not
"no winner" — that stays loud, because the waiting path writes and a blind write
would supersede whichever transaction owns the head.

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

When the Codex reviewer is observed to be usage-limited against the head no
verdict can arrive, so the gate degrades to maintainer review rather than wait
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

## Event and merge ordering

Each subscribed input event first creates a new non-green aggregate generation
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

Repository-ruleset changes and mergeability changes caused only by `master`
advancing have no GitHub event here. Use the same manual PR-number dispatch to
refresh that observational state. The destructive merge path still reevaluates
the target PR, every other associated PR, and the association set immediately
before its write.
