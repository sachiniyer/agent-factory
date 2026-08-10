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

Auto Gate does not pass fork heads. GitHub deliberately makes review and review
comment workflows read-only for fork pull requests, so those events cannot
reliably invalidate a previously green aggregate. Move an authorized change to
a branch in this repository before asking Auto Gate to merge it. The
`pull_request_target` lifecycle trigger only executes the default-branch helper;
it never checks out or executes pull-request code.

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
