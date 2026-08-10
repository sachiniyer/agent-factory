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

## Ruleset

The `master` ruleset requires the fixed `Auto Gate decision` check from the
built-in GitHub Actions app (app ID 15368). It must not grant repository roles a
bypass: a maintainer's direct merge is supposed to meet the same required check
as the workflow's merge. The stable-release deploy key may retain its narrow
bypass because that non-session path updates the release commit directly.

## Event and merge ordering

Each subscribed input event makes the affected aggregate non-green before
refreshing the PR/head decisions. After all decisions settle, Auto Gate
republishes the aggregate and attempts a merge only after a fresh aggregate
evaluation. A head synchronization reevaluates both the new head and the
previous head because the set of associated PRs changed for both commits.

Repository-ruleset changes and mergeability changes caused only by `master`
advancing have no GitHub event here. Use the same manual PR-number dispatch to
refresh that observational state. The destructive merge path still reevaluates
the target PR immediately before its write.
