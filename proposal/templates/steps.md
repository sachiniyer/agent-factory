# {{title}}: steps

How this change lands: the PR stack, and the steps to roll it out safely.

## PR stack

<!--
One row per PR, in merge order. Order the stack by the deployment timeline,
not by convenience: each PR must be safe to deploy on its own, with the PRs
above it deployed and the PRs below it not yet merged.

- Ship readers before writers: code that understands a new field, format,
  route, or schema goes out before code that produces it.
- Ship additive changes before removals: add, migrate, then delete.
- Keep old and new versions compatible while both can run at once (rolling
  deploys, clients on an older release, data written by the old code).
- Gate new behavior behind a flag or config when a PR must merge before it is
  safe to turn on.
-->

| # | PR | Depends on | Deploys to | Safe because |
|---|----|------------|------------|--------------|
| 1 |    |            |            |              |

## Rollout steps

<!--
The ordered steps from first merge to done: merges, deploys, migrations,
flag flips, backfills, and cleanup. For each step, say what must be true
before it starts (a deploy finished, a migration ran, a metric is healthy),
and how long to wait before the next step.
-->

1.

## Verification

<!-- How to confirm each step worked before moving on: checks, metrics, logs. -->

## Rollback

<!--
For each step, how to undo it and whether it can be undone at all. Mark the
point of no return (for example, a destructive migration or a removed field)
and what must be verified before passing it.
-->
